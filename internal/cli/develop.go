package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	pathpkg "path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const defaultTimeZone = "America/Chicago"

// Errors returned by the develop command.
var (
	ErrNoScheduledBackup = errors.New("no ScheduledBackup found")
	ErrParityDiffers     = errors.New("the generated backup differs from the original")
)

func newRenderCommand(f *Factory) *cobra.Command {
	var asCronJob bool

	cmd := &cobra.Command{
		Use:     "render <file.yaml|name>",
		Short:   "Print the script a backup runs",
		GroupID: GroupDevelopment,
		Long: `Print the shell script a ScheduledBackup renders to.

Takes either a local manifest or the name of one in the cluster, so it answers
both "what would this change do" and "what is this actually running".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := loadScheduledBackup(cmd.Context(), f, args[0])
			if err != nil {
				return err
			}
			return renderBackup(f.Streams.Out, sb, asCronJob)
		},
	}

	cmd.Flags().BoolVar(&asCronJob, "cronjob", false, "Print the whole CronJob instead of the script")
	return cmd
}

func loadScheduledBackup(
	ctx context.Context, f *Factory, arg string,
) (*b2backupv1.ScheduledBackup, error) {
	if _, err := os.Stat(arg); err == nil {
		return readScheduledBackup(arg)
	}

	c, err := f.Client()
	if err != nil {
		return nil, err
	}
	ns, err := f.Namespace()
	if err != nil {
		return nil, err
	}
	target, err := Resolve(ctx, c, ns, arg)
	if err != nil {
		return nil, err
	}
	if target.Kind != TargetScheduledBackup {
		return nil, fmt.Errorf("render needs a ScheduledBackup, not a %s", target.Kind)
	}
	return target.ScheduledBackup, nil
}

func readScheduledBackup(path string) (*b2backupv1.ScheduledBackup, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}

		var sb b2backupv1.ScheduledBackup
		if err := yaml.Unmarshal(doc, &sb); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if sb.Kind != "ScheduledBackup" {
			continue
		}
		return &sb, nil
	}
	return nil, fmt.Errorf("%w in %s", ErrNoScheduledBackup, path)
}

func renderBackup(out io.Writer, sb *b2backupv1.ScheduledBackup, asCronJob bool) error {
	p := newPrinter(out)

	if !asCronJob {
		script, err := backup.Render(sb)
		if err != nil {
			return err
		}
		p.println(script)
		return p.Err()
	}

	repo := &b2backupv1.Repository{
		Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name,
	}
	cj, err := backup.BuildCronJob(sb, repo, backup.DefaultConfig())
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cj)
	if err != nil {
		return err
	}
	p.print(string(data))
	return p.Err()
}

func newValidateCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "validate <file.yaml>",
		Short:   "Check a manifest without a cluster",
		GroupID: GroupDevelopment,
		Long: `Check that a ScheduledBackup manifest renders.

This catches what the operator would reject at reconcile time -- an
unresolvable schedule, a source it cannot render, a missing database field --
without needing a cluster. It does not run the CRD's CEL rules, which the API
server owns.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := readScheduledBackup(args[0])
			if err != nil {
				return err
			}
			return validateBackup(f.Streams.Out, sb)
		},
	}
	return cmd
}

func validateBackup(out io.Writer, sb *b2backupv1.ScheduledBackup) error {
	p := newPrinter(out)

	schedule, err := backup.ResolveSchedule(sb.Spec.Schedule, sb.Namespace+"/"+sb.Name)
	if err != nil {
		return fmt.Errorf("spec.schedule: %w", err)
	}
	if _, err := backup.Render(sb); err != nil {
		return fmt.Errorf("rendering the script: %w", err)
	}
	repo := &b2backupv1.Repository{Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name}
	if _, err := backup.BuildCronJob(sb, repo, backup.DefaultConfig()); err != nil {
		return fmt.Errorf("building the CronJob: %w", err)
	}

	p.printf("scheduledbackup/%s is valid\n", sb.Name)
	p.printf("  schedule  %s\n", schedule)
	p.printf("  sources   %d\n", len(sb.Spec.Sources))
	return p.Err()
}

func newParityCommand(f *Factory) *cobra.Command {
	var opts migrateOptions

	cmd := &cobra.Command{
		Use:     "parity <generated.yaml> <helmrelease.yaml | resources/restic directory>",
		Short:   "Compare a generated backup against the HelmRelease it replaces",
		GroupID: GroupDevelopment,
		Long: `Check that migrating an app does not change what gets backed up.

Compares the rendered script, the resolved schedule, the time zone, the claims
and scratch space mounted, the working directory, the snapshot host, the extra
environment and the database Secret against the hand-written HelmRelease. With
--repository or --from-secret it also compares the Repository's bucket and path
with the HelmRelease's RESTIC_REPOSITORY. Exits non-zero on any difference, so
it can gate a migration.

A database dump that moves from backup-cnpg.sh to dumpdb cnpg is reported as a
difference of its own, since the dump's format and file name change with it.

Both sides are parsed in-process; only --from-secret runs anything, sops.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runParity(f.Streams.Out, args[0], args[1], opts)
		},
	}
	cmd.Flags().StringVar(&opts.repository, "repository", "",
		"The original repository, as the s3: URL in RESTIC_REPOSITORY")
	cmd.Flags().StringVar(&opts.fromSecret, "from-secret", "",
		"A SOPS-encrypted Secret to read the original RESTIC_REPOSITORY from")
	cmd.MarkFlagsMutuallyExclusive("repository", "from-secret")
	return cmd
}

type plan struct {
	script     string
	schedule   string
	timeZone   string
	volumes    string
	dbSecret   string
	workingDir string
	host       string
	env        map[string]string
	repository string
	tags       []string
	namespace  string
}

func runParity(out io.Writer, generatedPath, helmReleasePath string, opts migrateOptions) error {
	rendered, err := renderedPlan(generatedPath)
	if err != nil {
		return fmt.Errorf("rendering the generated resource: %w", err)
	}
	original, err := originalPlan(helmReleasePath)
	if err != nil {
		return fmt.Errorf("reading the original backup: %w", err)
	}
	repositoryURL, err := opts.repositoryURL(original.namespace, false)
	if err != nil {
		return err
	}
	if repositoryURL != "" {
		loc, err := parseS3Repository(repositoryURL)
		if err != nil {
			return err
		}
		original.repository = describeLocation(loc.Bucket, loc.Path)
	}

	p := newPrinter(out)
	var differences, notes []string

	renderedScript, renderedDumps, scoped := normalizeRendered(rendered.script, rendered.tags)
	originalScript, originalDumps := normalizeScript(original.script)
	scriptDiffers := renderedScript != originalScript
	if scriptDiffers {
		differences = append(differences, "SCRIPT DIFFERS")
	} else if dumps := dumpChanges(originalDumps, renderedDumps); len(dumps) > 0 {
		differences = append(differences, dumps...)
	}
	if scoped != "" && !scriptDiffers {
		notes = append(notes, scoped)
	}

	renderedCadence, err := cadenceOf(rendered.schedule)
	if err != nil {
		return fmt.Errorf("reading the generated schedule: %w", err)
	}
	originalCadence, err := cadenceOf(original.schedule)
	if err != nil {
		return fmt.Errorf("reading the original schedule: %w", err)
	}
	switch {
	case !renderedCadence.equal(originalCadence):
		differences = append(differences, fmt.Sprintf(
			"CADENCE DIFFERS: original %q runs %s, generated %q runs %s",
			original.schedule, originalCadence, rendered.schedule, renderedCadence))
	case rendered.schedule != original.schedule:
		notes = append(notes, fmt.Sprintf(
			"RESCHEDULED: %s -> %s (%s either way; the operator jittered it)",
			describeSchedule(original.schedule, original.timeZone),
			describeSchedule(rendered.schedule, rendered.timeZone), renderedCadence))
	}

	if _, both := rendered.env[cnpgSecretPathEnv]; !both {
		delete(original.env, cnpgSecretPathEnv)
	} else if _, both = original.env[cnpgSecretPathEnv]; !both {
		delete(rendered.env, cnpgSecretPathEnv)
	}
	for _, c := range []struct{ label, original, rendered string }{
		{"TIMEZONE DIFFERS", original.timeZone, rendered.timeZone},
		{volumeDiffers, original.volumes, rendered.volumes},
		{"DATABASE SECRET DIFFERS", original.dbSecret, rendered.dbSecret},
		{"WORKING DIR DIFFERS", original.workingDir, rendered.workingDir},
		{"RESTIC HOST DIFFERS", original.host, rendered.host},
		{"ENV DIFFERS", describeEnv(original.env), describeEnv(rendered.env)},
	} {
		if c.original != c.rendered {
			differences = append(differences, fmt.Sprintf("%s: original %s, generated %s",
				c.label, orNone(c.original), orNone(c.rendered)))
		}
	}
	if original.repository != "" && original.repository != rendered.repository {
		differences = append(differences, fmt.Sprintf("REPOSITORY DIFFERS: original %s, generated %s",
			original.repository, orNone(rendered.repository)))
	}

	for _, n := range notes {
		p.println(n)
	}
	if len(differences) == 0 {
		if len(notes) == 0 {
			p.println("IDENTICAL")
		} else {
			p.println(verdictEquivalent)
		}
		return p.Err()
	}

	for _, d := range differences {
		p.println(d)
	}
	if scriptDiffers {
		p.println("--- original")
		p.println(original.script)
		p.println("--- rendered")
		p.println(rendered.script)
	}
	if err := p.Err(); err != nil {
		return err
	}
	return ErrParityDiffers
}

func describeLocation(bucket, path string) string {
	if path == "" {
		return bucket + " (bucket root)"
	}
	return bucket + "/" + path
}

// describeSchedule names a cron expression and, for the daily and hourly
// shapes migration produces, when it fires in its time zone.
func describeSchedule(expr, tz string) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 || fields[2] != "*" || fields[3] != "*" || fields[4] != "*" || !isNumeric(fields[0]) {
		return fmt.Sprintf("%q", expr)
	}
	minute, _ := strconv.Atoi(fields[0])
	switch {
	case isNumeric(fields[1]):
		hour, _ := strconv.Atoi(fields[1])
		return fmt.Sprintf("%q (daily at %02d:%02d %s)", expr, hour, minute, tz)
	case fields[1] == "*":
		return fmt.Sprintf("%q (hourly at :%02d)", expr, minute)
	}
	return fmt.Sprintf("%q", expr)
}

func renderedPlan(path string) (plan, error) {
	sb, err := readScheduledBackup(path)
	if err != nil {
		return plan{}, err
	}
	if err := backup.ValidateMounts(sb); err != nil {
		return plan{}, err
	}
	script, err := backup.Render(sb)
	if err != nil {
		return plan{}, err
	}

	schedule, err := backup.ResolveSchedule(sb.Spec.Schedule, sb.Namespace+"/"+sb.Name)
	if err != nil {
		return plan{}, err
	}
	tz := sb.Spec.TimeZone
	if tz == "" {
		tz = defaultTimeZone
	}
	out := plan{
		script: script, schedule: schedule, timeZone: tz,
		workingDir: sb.EffectiveWorkingDir(),
		host:       sb.EffectiveResticHost(),
		tags:       backup.SourceTags(sb.Spec.Sources),
	}
	var volumes []string
	for i := range sb.Spec.Volumes {
		v := &sb.Spec.Volumes[i]
		volumes = append(volumes, describeVolume(v.ExistingClaim, v.EffectiveMountPath(), v.ReadOnly))
	}
	if sc := sb.Spec.Scratch; sc != nil {
		volumes = append(volumes, "scratch space at "+pathpkg.Clean(sc.MountPath))
	}
	out.volumes = strings.Join(slices.Sorted(slices.Values(volumes)), " and ")

	env := map[string]string{"TZ": tz}
	maps.Copy(env, sb.Spec.Env)
	secretMounts := map[string]bool{}
	if db := sb.Spec.Database; db != nil {
		out.dbSecret = db.EffectiveSecretName()
		secretMounts[b2backupv1.DBSecretMountPath] = true
	}
	normalizeCNPGSecretPath(env, script, secretMounts)
	out.env = env

	if repo, err := readRepository(path, sb.Namespace, sb.Spec.RepositoryRef.Name); err == nil && repo != nil {
		out.repository = describeLocation(repo.Spec.Bucket, repo.EffectivePath())
	}
	return out, nil
}

const databaseMountToken = "<database Secret mount>"

const (
	envResticHost     = "RESTIC_HOST"
	volumeDiffers     = "VOLUME DIFFERS"
	verdictEquivalent = "EQUIVALENT"
)

const backupCNPGDefaultMount = "/postgresql-app"

// normalizeCNPGSecretPath leaves CNPG_SECRET_PATH in env only when the script
// runs backup-cnpg.sh, the one thing that reads it, and then as the directory it
// effectively names, or as databaseMountToken when that is where the database
// Secret is mounted. runParity compares it only when both sides run the script;
// a switch to dumpdb is reported as a dump change of its own.
func normalizeCNPGSecretPath(env map[string]string, script string, secretMounts map[string]bool) {
	if !strings.Contains(script, backupCNPGScript) {
		delete(env, cnpgSecretPathEnv)
		return
	}
	value, ok := env[cnpgSecretPathEnv]
	if !ok {
		value = backupCNPGDefaultMount
	}
	if secretMounts[pathpkg.Clean(value)] {
		value = databaseMountToken
	}
	env[cnpgSecretPathEnv] = value
}

func describeEnv(env map[string]string) string {
	parts := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		parts = append(parts, k+"="+env[k])
	}
	return strings.Join(parts, " ")
}

func readRepository(path, namespace, name string) (*b2backupv1.Repository, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var repo b2backupv1.Repository
		if yaml.Unmarshal(doc, &repo) != nil || repo.Kind != "Repository" {
			continue
		}
		if repo.Name == name && (repo.Namespace == "" || repo.Namespace == namespace) {
			return &repo, nil
		}
	}
}

func originalPlan(arg string) (plan, error) {
	path, err := resolveHelmRelease(arg)
	if err != nil {
		return plan{}, err
	}
	hr, err := readHelmRelease(path)
	if err != nil {
		return plan{}, err
	}
	controller, container, err := hr.backupController()
	if err != nil {
		return plan{}, fmt.Errorf("%s: %w", path, err)
	}
	script, err := container.script()
	if err != nil {
		return plan{}, fmt.Errorf("%s: %w", path, err)
	}

	tz := controller.CronJob.TimeZone
	if tz == "" {
		tz = defaultTimeZone
	}
	out := plan{script: script, schedule: controller.CronJob.Schedule, timeZone: tz, namespace: hr.Metadata.Namespace}

	workingDir := container.WorkingDir
	if workingDir == "" {
		workingDir = "/"
	}
	out.workingDir = pathpkg.Clean(workingDir)

	var volumes, secretNames []string
	secretMounts := map[string]bool{}
	for _, v := range hr.volumes() {
		switch {
		case v.kind == volumeClaim && v.name != "" && v.key != "cache":
			volumes = append(volumes, describeVolume(v.name, v.path, v.readOnly))
		case v.kind == volumeEmptyDir:
			volumes = append(volumes, "scratch space at "+pathpkg.Clean(v.path))
		case v.kind == volumeSecret:
			secretNames = append(secretNames, v.name)
			secretMounts[pathpkg.Clean(v.path)] = true
		}
	}
	out.volumes = strings.Join(slices.Sorted(slices.Values(volumes)), " and ")
	if usesDatabase(script) {
		out.dbSecret = strings.Join(secretNames, " and ")
	}

	host, err := container.resticHost(hr.Metadata.Namespace)
	if err != nil {
		return plan{}, err
	}
	out.host = host

	env := map[string]string{}
	for _, name := range sortedKeys(container.Env) {
		if name == envResticHost {
			continue
		}
		value := container.env(name)
		if value == "" {
			value = fmt.Sprintf("<%v>", container.Env[name])
		}
		env[name] = value
	}
	normalizeCNPGSecretPath(env, script, secretMounts)
	if _, ok := env["TZ"]; !ok {
		env["TZ"] = ""
	}
	out.env = env
	return out, nil
}

func usesDatabase(script string) bool {
	return strings.Contains(script, "dumpdb ") || strings.Contains(script, "backup-cnpg.sh") ||
		strings.Contains(script, "backup-mariadb.sh")
}

func describeVolume(claim, path string, readOnly bool) string {
	s := claim + " at " + path
	if readOnly {
		s += " (read-only)"
	}
	return s
}

var (
	ignoredFlags = regexp.MustCompile(` --(retry-lock|secret-mount)=\S+`)

	quotedExclude = regexp.MustCompile(`--exclude=(?:'([^']*)'|"([^"]*)")`)

	cnpgDump = regexp.MustCompile(`-- (?:dumpdb cnpg|backup-cnpg[.]sh)((?: --(?:database|dbname)=\S+)?)$`)

	dumpDatabase = regexp.MustCompile(`--(?:database|dbname)=`)
)

const cnpgDumpToken = "-- <cnpg dump>"

var forgetScope = regexp.MustCompile(` --(?:tag=(\S+)|host="\$RESTIC_HOST")`)

// normalizeRendered normalizes a rendered script like normalizeScript, and also
// takes out of restic forget the --host and --tag scoping the operator adds when
// it names exactly this backup's own tags. restic already defaults --host to
// $RESTIC_HOST, so only the tags change what forget considers, and the returned
// note says so.
func normalizeRendered(s string, tags []string) (string, []string, string) {
	var lines []string
	note := ""
	for line := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "restic forget ") {
			var found []string
			stripped := forgetScope.ReplaceAllStringFunc(line, func(m string) string {
				if tag := forgetScope.FindStringSubmatch(m)[1]; tag != "" {
					found = append(found, tag)
				}
				return ""
			})
			if slices.Equal(found, tags) {
				line = stripped
				if len(tags) > 0 {
					note = fmt.Sprintf("FORGET SCOPED: forget now only expires snapshots tagged %s on this host; "+
						"snapshots under any other tag are no longer expired by this backup", strings.Join(tags, ", "))
				}
			}
		}
		lines = append(lines, line)
	}
	out, dumps := normalizeScript(strings.Join(lines, "\n"))
	return out, dumps, note
}

func normalizeScript(s string) (string, []string) {
	var lines, dumps []string
	for _, line := range joinContinuations(s) {
		line = ignoredFlags.ReplaceAllString(line, "")
		line = quotedExclude.ReplaceAllString(line, "--exclude=$1$2")
		if m := cnpgDump.FindStringSubmatchIndex(line); m != nil {
			command := "dumpdb cnpg"
			if strings.Contains(line[m[0]:m[2]], "backup-cnpg.sh") {
				command = "backup-cnpg.sh"
			}
			dumps = append(dumps, command)
			database := dumpDatabase.ReplaceAllString(line[m[2]:m[3]], "--database=")
			line = line[:m[0]] + cnpgDumpToken + database
		}

		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return strings.Join(lines, "\n"), dumps
}

func dumpChanges(original, rendered []string) []string {
	var out []string
	for i := range min(len(original), len(rendered)) {
		if original[i] == rendered[i] {
			continue
		}
		change := fmt.Sprintf("DUMP FORMAT CHANGES: database dump %d moves from %s to %s", i+1, original[i], rendered[i])
		if original[i] == "backup-cnpg.sh" {
			change += ". backup-cnpg.sh writes plain SQL stored as <host>; dumpdb cnpg writes pg_dump's " +
				"custom format stored as <host>.dmp, which b2bk restore --to-database reads. " +
				"Earlier snapshots keep the old format"
		}
		out = append(out, change)
	}
	return out
}
