package backup

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// RestoreHold says why a restore holds the backup's schedule, or "" when none
// does: one is running, or one failed and may have left a target half-written.
// A held backup's CronJob is suspended whatever spec.suspend says, and no
// triggered backup starts, so a partial restore is never saved.
func RestoreHold(sb *b2backupv1.ScheduledBackup) string {
	if holder := sb.Annotations[b2backupv1.AnnotationRestoreLock]; holder != "" {
		return "a restore is running (" + holder + ")"
	}
	if partial := sb.Annotations[b2backupv1.AnnotationRestorePartial]; partial != "" {
		return "a failed restore may have left " + partial + " half-written; check it, then run b2bk resume sb/" + sb.Name
	}
	return ""
}

// PartialTargets reads AnnotationRestorePartial: pvc/<claim>:<path> and
// db/<engine>/<secret>/<host>/<database> entries.
func PartialTargets(sb *b2backupv1.ScheduledBackup) []string {
	raw := sb.Annotations[b2backupv1.AnnotationRestorePartial]
	if raw == "" {
		return nil
	}
	var targets []string
	if err := json.Unmarshal([]byte(raw), &targets); err != nil || len(targets) == 0 {
		return []string{raw}
	}
	return targets
}

// SharedRestoreHold says why a restore through another ScheduledBackup in the
// namespace holds this one, or "": one is running through a ScheduledBackup that
// shares a claim or database with it, or one left a claim it mounts or a
// database it dumps half-written. It covers a ScheduledBackup that came to
// share the target after the restore took its hold.
func SharedRestoreHold(sb *b2backupv1.ScheduledBackup, backups []b2backupv1.ScheduledBackup) string {
	for i := range backups {
		other := &backups[i]
		if other.Name == sb.Name {
			continue
		}
		if holder := other.Annotations[b2backupv1.AnnotationRestoreLock]; holder != "" && sharesTarget(sb, other) {
			return fmt.Sprintf("a restore through scheduledbackup/%s, which shares its claims or database, is "+
				"running (%s)", other.Name, holder)
		}
		for _, entry := range PartialTargets(other) {
			if coversEntry(sb, entry) {
				return fmt.Sprintf("a failed restore through scheduledbackup/%s may have left %s half-written; "+
					"check it, then run b2bk resume sb/%s", other.Name, entry, other.Name)
			}
		}
	}
	return ""
}

func sharesTarget(sb, other *b2backupv1.ScheduledBackup) bool {
	for _, v := range other.Spec.Volumes {
		if slices.ContainsFunc(sb.Spec.Volumes, func(w b2backupv1.VolumeSpec) bool {
			return w.ExistingClaim == v.ExistingClaim
		}) {
			return true
		}
	}
	return sb.Spec.Database != nil && other.Spec.Database != nil &&
		sb.Spec.Database.Engine == other.Spec.Database.Engine &&
		sb.Spec.Database.EffectiveSecretName() == other.Spec.Database.EffectiveSecretName()
}

func coversEntry(sb *b2backupv1.ScheduledBackup, entry string) bool {
	if rest, ok := strings.CutPrefix(entry, "pvc/"); ok {
		claim, _, _ := strings.Cut(rest, ":")
		return slices.ContainsFunc(sb.Spec.Volumes, func(v b2backupv1.VolumeSpec) bool {
			return v.ExistingClaim == claim
		})
	}
	if rest, ok := strings.CutPrefix(entry, "db/"); ok && sb.Spec.Database != nil {
		parts := strings.SplitN(rest, "/", 3)
		return len(parts) >= 2 && parts[0] == string(sb.Spec.Database.Engine) &&
			parts[1] == sb.Spec.Database.EffectiveSecretName()
	}
	return !strings.HasPrefix(entry, "pvc/") && !strings.HasPrefix(entry, "db/")
}
