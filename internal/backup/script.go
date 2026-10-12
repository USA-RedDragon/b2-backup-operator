package backup

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// The body runs in a group piped through ts, rather than behind
// exec > >(ts ...): busybox ash kills the process substitution when the shell
// exits, and a fast failure lost every line of its output. pipefail makes the
// script's status the body's, and inside the body a failed dump piped into
// restic fails the run instead of being saved.
const (
	preamble  = "set -o pipefail\n{\nset -euo pipefail"
	postamble = "restic cache --cleanup\n} 2>&1 | ts '%H:%M:%S'"
)

// Render builds the shell script the backup container runs.
func Render(sb *b2backupv1.ScheduledBackup) (string, error) {
	spec := &sb.Spec
	var b strings.Builder
	b.WriteString(preamble)
	b.WriteString("\n")
	b.WriteString(UnlockLine)
	b.WriteString("\n")

	if spec.Script != "" {
		b.WriteString(strings.TrimRight(spec.Script, "\n"))
		b.WriteString("\n")
		b.WriteString(postamble)
		b.WriteString("\n")
		return b.String(), nil
	}

	for _, src := range spec.Sources {
		line, err := renderSource(src, sb.EffectiveWorkingDir(), retryLockFlag(spec))
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteString("\n")
		b.WriteString(SummaryLine(src.EffectiveTag()))
		b.WriteString("\n")
	}

	if forget := renderForget(spec.Retention, SourceTags(spec.Sources), retryLockFlag(spec)); forget != "" {
		b.WriteString(UnlockLine)
		b.WriteString("\n")
		b.WriteString(forget)
		b.WriteString("\n")
	}

	b.WriteString(postamble)
	b.WriteString("\n")
	return b.String(), nil
}

// UnlockLine removes the locks restic considers stale: older than 30 minutes,
// or held by a process that no longer runs on this host. A live restic
// refreshes its lock every 5 minutes, so this never removes one in use. A
// restic that died without unlocking leaves its lock behind, and restic
// refuses an exclusive lock, which forget --prune takes, while any other lock
// exists, stale or not. Backups run it first, since a leaked exclusive lock
// stops restic backup too, and again before forget. A failed unlock does not
// fail the run: it only means a stale lock may still be in the way.
const UnlockLine = "restic unlock || true"

func retryLockFlag(spec *b2backupv1.ScheduledBackupSpec) string {
	if spec.RetryLock == nil || spec.RetryLock.Duration <= 0 {
		return ""
	}
	return " --retry-lock=" + spec.RetryLock.Duration.String()
}

// SourceTags returns the distinct tags of sources, in order.
func SourceTags(sources []b2backupv1.BackupSource) []string {
	var tags []string
	for _, src := range sources {
		if tag := src.EffectiveTag(); !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return tags
}

func renderSource(src b2backupv1.BackupSource, workingDir, retryLock string) (string, error) {
	tag := src.EffectiveTag()

	switch src.Type {
	case b2backupv1.SourceTypeFiles:
		paths := src.ResolvedPaths(workingDir)
		quoted := make([]string, 0, len(paths))
		for _, p := range paths {
			quoted = append(quoted, shellQuoteIfNeeded(p))
		}
		line := fmt.Sprintf("restic backup%s --tag=%s %s", retryLock, tag, strings.Join(quoted, " "))
		if len(src.Exclude) == 0 {
			return line, nil
		}
		parts := make([]string, 0, len(src.Exclude)+1)
		parts = append(parts, line)
		for _, ex := range src.Exclude {
			parts = append(parts, "  --exclude="+shellQuote(ex))
		}
		return strings.Join(parts, " \\\n"), nil

	case b2backupv1.SourceTypeCNPG, b2backupv1.SourceTypeMariaDB:

		var sb strings.Builder

		fmt.Fprintf(&sb, "restic backup%s --tag=%s --stdin-from-command -- dumpdb %s --secret-mount=%s",
			retryLock, tag, src.Type, b2backupv1.DBSecretMountPath)
		if src.Database != "" {
			sb.WriteString(" --database=" + shellQuoteIfNeeded(src.Database))
		}
		if len(src.ExtraArgs) > 0 {
			sb.WriteString(" --")
			for _, a := range src.ExtraArgs {
				sb.WriteString(" " + shellQuoteIfNeeded(a))
			}
		}
		return sb.String(), nil

	default:
		return "", fmt.Errorf("unknown source type %q", src.Type)
	}
}

// ContainerName is the backup container's name in every backup pod.
const ContainerName = containerName

// SummaryCommand starts the line that records restic's summary of the
// snapshot a source just saved. The line appends the tag, a space and the
// snapshot's summary object to the container's termination message, less the
// directory and blob counts and the fractions of a second, which keeps a line
// near 220 bytes: the kubelet keeps 4096 bytes of the message, and the
// operator skips a line it cut short. restic stores the summary in the
// snapshot itself, so nothing parses its human-readable output. The line runs
// restic without a lock and cannot fail the backup.
const SummaryCommand = "restic snapshots --json --no-lock --group-by= --tag="

// SummaryLine renders the summary line for the source tagged tag. An empty
// --group-by keeps the image's wrapper from grouping the JSON. Tags are
// [A-Za-z0-9_.-], which need no quoting in the shell or in sed.
func SummaryLine(tag string) string {
	return SummaryCommand + tag + ` --host="$RESTIC_HOST" latest 2>/dev/null | ` +
		`sed -nE '/"summary":/{s/.*"summary":(\{[^}]*\}).*/` + tag + ` \1/;` +
		`s/"(dirs_[a-z]+|data_blobs|tree_blobs)":[0-9]+,//g;s/[.][0-9]+([Z+-])/\1/g;p;}' 2>/dev/null >>` +
		corev1.TerminationMessagePathDefault + " || true"
}

// IsSummaryLine reports whether a script line is one SummaryLine rendered.
func IsSummaryLine(line string) bool {
	line = strings.TrimSpace(line)
	tag, ok := strings.CutPrefix(line, SummaryCommand)
	if !ok {
		return false
	}
	tag, _, _ = strings.Cut(tag, " ")
	return line == SummaryLine(tag)
}

// ForgetHostFlag scopes restic forget to the host this backup's snapshots carry.
// restic already defaults --host to $RESTIC_HOST; the flag makes it explicit.
const ForgetHostFlag = `--host="$RESTIC_HOST"`

// ForgetGroupFlag keeps each tag a retention group of its own whatever
// RESTIC_GROUP_BY says: grouped by host alone, a backup's sources would share one
// group, and the policy would keep one snapshot per period across all of them.
const ForgetGroupFlag = "--group-by=tags"

func renderForget(r *b2backupv1.Retention, tags []string, retryLock string) string {
	if r == nil {
		return ""
	}
	flags := make([]string, 0, 6)
	for _, f := range []struct {
		name  string
		value *int32
	}{
		{"last", r.Last},
		{"hourly", r.Hourly},
		{"daily", r.Daily},
		{"weekly", r.Weekly},
		{"monthly", r.Monthly},
		{"yearly", r.Yearly},
	} {
		if f.value != nil {
			flags = append(flags, fmt.Sprintf("--keep-%s=%d", f.name, *f.value))
		}
	}
	if len(flags) == 0 {
		return ""
	}
	scope := []string{ForgetHostFlag, ForgetGroupFlag}
	for _, tag := range tags {
		scope = append(scope, "--tag="+tag)
	}
	return "restic forget --prune" + retryLock + " " + strings.Join(append(scope, flags...), " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeBare = regexp.MustCompile(`^[A-Za-z0-9_.:@%+,/=-]+$`)

// IsBareWord reports whether s needs no quoting in a shell: the shell reads it
// as the one word it is, unchanged.
func IsBareWord(s string) bool { return s != "" && safeBare.MatchString(s) }

func shellQuoteIfNeeded(s string) string {
	if s != "" && safeBare.MatchString(s) {
		return s
	}
	return shellQuote(s)
}
