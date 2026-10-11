package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
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
		script, err := backup.Render(&sb.Spec)
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
	if _, err := backup.Render(&sb.Spec); err != nil {
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
	cmd := &cobra.Command{
		Use:     "parity <generated.yaml> <helmrelease.yaml | resources/restic directory>",
		Short:   "Compare a generated backup against the HelmRelease it replaces",
		GroupID: GroupDevelopment,
		Long: `Check that migrating an app does not change what gets backed up.

Compares the rendered script, the resolved schedule, the time zone, the claim
mounted and the database Secret against the hand-written HelmRelease. Exits
non-zero on any difference, so it can gate a migration.

A database dump that moves from backup-cnpg.sh to dumpdb cnpg is reported as a
difference of its own, since the dump's format and file name change with it.

Both sides are parsed in-process; nothing external is needed on PATH.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runParity(f.Streams.Out, args[0], args[1])
		},
	}
	return cmd
}

type plan struct {
	script   string
	schedule string
	timeZone string
	volume   string
	dbSecret string
}

func runParity(out io.Writer, generatedPath, helmReleasePath string) error {
	rendered, err := renderedPlan(generatedPath)
	if err != nil {
		return fmt.Errorf("rendering the generated resource: %w", err)
	}
	original, err := originalPlan(helmReleasePath)
	if err != nil {
		return fmt.Errorf("reading the original backup: %w", err)
	}

	p := newPrinter(out)
	var differences, notes []string

	renderedScript, renderedDumps := normalizeScript(rendered.script)
	originalScript, originalDumps := normalizeScript(original.script)
	scriptDiffers := renderedScript != originalScript
	if scriptDiffers {
		differences = append(differences, "SCRIPT DIFFERS")
	} else if dumps := dumpChanges(originalDumps, renderedDumps); len(dumps) > 0 {
		differences = append(differences, dumps...)
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
			"RESCHEDULED: %q -> %q (%s either way; the operator jittered it)",
			original.schedule, rendered.schedule, renderedCadence))
	}

	if rendered.timeZone != original.timeZone {
		differences = append(differences, fmt.Sprintf(
			"TIMEZONE DIFFERS: original %q, generated %q", original.timeZone, rendered.timeZone))
	}
	if rendered.volume != original.volume {
		differences = append(differences, fmt.Sprintf(
			"VOLUME DIFFERS: original %s, generated %s", orNone(original.volume), orNone(rendered.volume)))
	}
	if rendered.dbSecret != original.dbSecret {
		differences = append(differences, fmt.Sprintf(
			"DATABASE SECRET DIFFERS: original %s, generated %s",
			orNone(original.dbSecret), orNone(rendered.dbSecret)))
	}

	for _, n := range notes {
		p.println(n)
	}
	if len(differences) == 0 {
		if len(notes) == 0 {
			p.println("IDENTICAL")
		} else {
			p.println("EQUIVALENT")
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

func renderedPlan(path string) (plan, error) {
	sb, err := readScheduledBackup(path)
	if err != nil {
		return plan{}, err
	}
	script, err := backup.Render(&sb.Spec)
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
	out := plan{script: script, schedule: schedule, timeZone: tz}
	if v := sb.Spec.Volume; v != nil {
		out.volume = describeVolume(v.ExistingClaim, v.EffectiveMountPath(), v.ReadOnly)
	}
	if db := sb.Spec.Database; db != nil {
		out.dbSecret = db.EffectiveSecretName()
	}
	return out, nil
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
	out := plan{script: script, schedule: controller.CronJob.Schedule, timeZone: tz}

	var claims, secretNames []string
	for _, v := range hr.volumes() {
		switch {
		case v.kind == volumeClaim && v.name != "" && v.key != "cache":
			claims = append(claims, describeVolume(v.name, v.path, v.readOnly))
		case v.kind == volumeSecret:
			secretNames = append(secretNames, v.name)
		}
	}
	out.volume = strings.Join(claims, " and ")
	if usesDatabase(script) {
		out.dbSecret = strings.Join(secretNames, " and ")
	}
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
