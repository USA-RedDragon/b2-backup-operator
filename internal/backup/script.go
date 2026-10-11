package backup

import (
	"fmt"
	"regexp"
	"strings"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	preamble  = "exec > >(ts '%H:%M:%S') 2>&1\nset -eu"
	postamble = "restic cache --cleanup"
)

// Render builds the shell script the backup container runs.
func Render(spec *b2backupv1.ScheduledBackupSpec) (string, error) {
	var b strings.Builder
	b.WriteString(preamble)
	b.WriteString("\n")

	if spec.Script != "" {
		b.WriteString(strings.TrimRight(spec.Script, "\n"))
		b.WriteString("\n")
		b.WriteString(postamble)
		b.WriteString("\n")
		return b.String(), nil
	}

	for _, src := range spec.Sources {
		line, err := renderSource(src, retryLockFlag(spec))
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	if forget := renderForget(spec.Retention, retryLockFlag(spec)); forget != "" {
		b.WriteString(forget)
		b.WriteString("\n")
	}

	b.WriteString(postamble)
	b.WriteString("\n")
	return b.String(), nil
}

func retryLockFlag(spec *b2backupv1.ScheduledBackupSpec) string {
	if spec.RetryLock == nil || spec.RetryLock.Duration <= 0 {
		return ""
	}
	return " --retry-lock=" + spec.RetryLock.Duration.String()
}

func renderSource(src b2backupv1.BackupSource, retryLock string) (string, error) {
	tag := src.EffectiveTag()

	switch src.Type {
	case b2backupv1.SourceTypeFiles:

		line := fmt.Sprintf("restic backup%s --tag=%s %s",
			retryLock, tag, shellQuoteIfNeeded(src.EffectivePath()))
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

func renderForget(r *b2backupv1.Retention, retryLock string) string {
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
	return "restic forget --prune" + retryLock + " " + strings.Join(flags, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeBare = regexp.MustCompile(`^[A-Za-z0-9_.:@%+,/=-]+$`)

func shellQuoteIfNeeded(s string) string {
	if s != "" && safeBare.MatchString(s) {
		return s
	}
	return shellQuote(s)
}
