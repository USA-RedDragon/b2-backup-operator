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
	"regexp"
	"slices"
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
		Use:     "parity <generated.yaml> <helmrelease.yaml | resources/restic directory | cronjob.yaml>",
		Short:   "Compare a generated backup against the HelmRelease it replaces",
		GroupID: GroupDevelopment,
		Long: `Check that migrating an app does not change what gets backed up.

Compares the CronJob the operator would build for the generated ScheduledBackup
with the CronJob the original runs: the script, schedule, time zone, image,
mounts, environment, envFrom, database Secret, resources, security contexts,
scheduling constraints, labels, other containers and controllers, and the rest
of the pod spec. With --repository or --from-secret it also compares the
Repository's bucket and path with the HelmRelease's RESTIC_REPOSITORY. Exits
non-zero on any difference, so it can gate a migration.

The original is rendered from the HelmRelease's values the way app-template
does, independently of migrate, or read from a manifest holding the CronJob
itself, such as helm template output. The generated side goes through the
operator's own validation, so a backup the operator would refuse is an error.

A database dump that moves from backup-cnpg.sh to dumpdb cnpg is reported as a
difference of its own, since the dump's format and file name change with it.

Only --from-secret runs anything, sops.`,
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

var (
	ignoredFlags = regexp.MustCompile(` --(retry-lock|secret-mount)=\S+`)

	quotedExclude = regexp.MustCompile(`--exclude=(?:'([^']*)'|"([^"]*)")`)

	cnpgDump = regexp.MustCompile(`-- (?:dumpdb cnpg|backup-cnpg[.]sh)((?: --(?:database|dbname)=\S+)?)$`)

	dumpDatabase = regexp.MustCompile(`--(?:database|dbname)=`)
)

const cnpgDumpToken = "-- <cnpg dump>"

var forgetScope = regexp.MustCompile(` --(?:tag=(\S+)|host="\$RESTIC_HOST"|group-by=tags)`)

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

const (
	optErrexit  = "errexit"
	optNounset  = "nounset"
	optPipefail = "pipefail"
)

// shellCommand is one command a script runs, with the shell options on when
// it runs.
type shellCommand struct {
	text string
	on   map[string]bool
}

// shellCommands reads a script's commands in order, one per line or per ;,
// tracking what its set commands turn on and off, and leaving out the
// preamble and postamble around a backup script.
func shellCommands(script string) []shellCommand {
	on := map[string]bool{}
	var out []shellCommand
	for _, line := range joinContinuations(script) {
		for _, cmd := range splitCommands(line) {
			words := strings.Fields(cmd)
			switch {
			case len(words) == 0:
			case words[0] == "set":
				applySet(on, words[1:])
			case cmd == "{" || cmd == "restic cache --cleanup" || groupClosing.MatchString(cmd) ||
				slices.Contains(preambleLines, cmd):
			default:
				out = append(out, shellCommand{text: cmd, on: maps.Clone(on)})
			}
		}
	}
	return out
}

func applySet(on map[string]bool, words []string) {
	letters := map[rune]string{'e': optErrexit, 'u': optNounset}
	for i := 0; i < len(words); i++ {
		w := words[i]
		if len(w) < 2 || (w[0] != '-' && w[0] != '+') {
			continue
		}
		enable := w[0] == '-'
		for _, r := range w[1:] {
			switch {
			case r == 'o' && i+1 < len(words):
				i++
				on[words[i]] = enable
			case letters[r] != "":
				on[letters[r]] = enable
			}
		}
	}
}

// splitCommands splits a line at each ; outside quotes.
func splitCommands(line string) []string {
	var out []string
	var quote rune
	start := 0
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ';' && !strings.HasPrefix(line[i:], ";;"):
			out = append(out, strings.TrimSpace(line[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(line[start:]))
}

var pipeline = regexp.MustCompile(`(^|[^|])\|([^|]|$)`)

// shellOptionsAdded lists the shell options the generated script runs under
// that fewer of the original's commands did, counting only commands an option
// could change: errexit for one followed by another (in a && or || list only
// the last command counts, as bash ignores errexit for the others), nounset
// for one naming a variable, pipefail for one with a pipe.
func shellOptionsAdded(original, generated string) []string {
	off := func(script string) map[string]int {
		cmds := shellCommands(script)
		counts := map[string]int{}
		for i, c := range cmds {
			matters := map[string]bool{
				optErrexit:  i < len(cmds)-1,
				optNounset:  strings.Contains(c.text, "$"),
				optPipefail: pipeline.MatchString(c.text),
			}
			for name, m := range matters {
				if m && !c.on[name] {
					counts[name]++
				}
			}
		}
		return counts
	}
	was, now := off(original), off(generated)
	var out []string
	for _, name := range []string{optErrexit, optNounset, optPipefail} {
		if now[name] < was[name] {
			out = append(out, name)
		}
	}
	return out
}

// unquoteExclude reads the quotes around an --exclude pattern as the shell
// does: a pattern that is one bare word compares without them, one a double
// quote would not expand compares single-quoted, and anything else keeps its
// quotes, so a space or an expansion is never mistaken for unquoted text.
func unquoteExclude(m string) string {
	inner := quotedExclude.FindStringSubmatch(m)
	pattern := inner[1] + inner[2]
	switch {
	case backup.IsBareWord(pattern):
		return "--exclude=" + pattern
	case inner[1] == "" && !strings.ContainsAny(pattern, "$`\\'"):
		return "--exclude='" + pattern + "'"
	}
	return m
}

// groupByFlag is a forget's grouping, which parity compares on its own.
var groupByFlag = regexp.MustCompile(` --group-by[= ](\S+)`)

// normalizeScript drops the preamble and postamble around a backup script,
// both the operator's and the one hand-written scripts carried, and what the
// operator adds to each line, so two scripts compare equal when they run the
// same commands.
func normalizeScript(s string) (string, []string) {
	var lines, dumps []string
	for _, line := range joinContinuations(scriptBody(s)) {
		line = ignoredFlags.ReplaceAllString(line, "")
		line = quotedExclude.ReplaceAllStringFunc(line, unquoteExclude)
		if strings.HasPrefix(strings.TrimSpace(line), "restic forget ") {
			line = groupByFlag.ReplaceAllString(line, "")
		}
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
