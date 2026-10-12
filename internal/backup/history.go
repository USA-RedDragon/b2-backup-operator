package backup

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// LastSuccess is when the backup last succeeded: status.lastSuccessfulTime or
// the newest succeeded run in its history, whichever is later, since an older
// operator took lastSuccessfulTime from the CronJob alone and left out
// triggered runs.
func LastSuccess(sb *b2backupv1.ScheduledBackup) *metav1.Time {
	latest := sb.Status.LastSuccessfulTime
	for i := range sb.Status.History {
		run := &sb.Status.History[i]
		if run.Result != b2backupv1.BackupRunSucceeded {
			continue
		}
		at := run.CompletionTime
		if at == nil {
			at = run.StartTime
		}
		if at != nil && (latest == nil || at.After(latest.Time)) {
			latest = at
		}
	}
	return latest
}

// maxRunSources is the most sources a ScheduledBackup can have, and so the
// most summaries one run can leave.
const maxRunSources = 16

var summaryTag = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// resticSummary is the summary restic stores in a snapshot, as
// restic snapshots --json prints it.
type resticSummary struct {
	BackupStart     *time.Time `json:"backup_start"`
	BackupEnd       *time.Time `json:"backup_end"`
	FilesNew        *int64     `json:"files_new"`
	FilesChanged    *int64     `json:"files_changed"`
	FilesUnmodified *int64     `json:"files_unmodified"`
	DataAdded       *int64     `json:"data_added"`
	DataAddedPacked *int64     `json:"data_added_packed"`
	FilesProcessed  *int64     `json:"total_files_processed"`
	BytesProcessed  *int64     `json:"total_bytes_processed"`
}

// ParseRunSummary reads the summaries SummaryLine leaves in a backup
// container's termination message. It skips any line it cannot read in full,
// such as the last one when the kubelet cut the message short, and returns
// nil when no line is readable.
func ParseRunSummary(message string) []b2backupv1.BackupRunSource {
	var out []b2backupv1.BackupRunSource
	seen := map[string]bool{}
	for line := range strings.SplitSeq(message, "\n") {
		tag, raw, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !summaryTag.MatchString(tag) || seen[tag] {
			continue
		}
		var s resticSummary
		dec := json.NewDecoder(strings.NewReader(raw))
		if err := dec.Decode(&s); err != nil || dec.More() {
			continue
		}
		counts := []*int64{s.FilesNew, s.FilesChanged, s.FilesUnmodified, s.DataAdded, s.DataAddedPacked,
			s.FilesProcessed, s.BytesProcessed}
		if slices.ContainsFunc(counts, func(n *int64) bool { return n == nil || *n < 0 }) {
			continue
		}
		src := b2backupv1.BackupRunSource{
			Tag:             tag,
			FilesNew:        *s.FilesNew,
			FilesChanged:    *s.FilesChanged,
			FilesUnmodified: *s.FilesUnmodified,
			ProcessedFiles:  *s.FilesProcessed,
			ProcessedBytes:  *s.BytesProcessed,
			AddedBytes:      *s.DataAdded,
			StoredBytes:     *s.DataAddedPacked,
		}
		if s.BackupStart != nil && !s.BackupStart.IsZero() {
			src.StartTime = &metav1.Time{Time: *s.BackupStart}
		}
		if s.BackupEnd != nil && !s.BackupEnd.IsZero() {
			src.CompletionTime = &metav1.Time{Time: *s.BackupEnd}
		}
		seen[tag] = true
		out = append(out, src)
		if len(out) == maxRunSources {
			break
		}
	}
	return out
}

// RunSize is what restic recorded for a run, over all its sources.
type RunSize struct {
	// Added is the new data added to the repository, before compression.
	Added int64
	// Stored is what the added data took in the repository.
	Stored int64
	// Processed is the size of the files the run's snapshots hold.
	Processed int64
}

// SizeOf totals a run's sources, and reports false when it recorded none.
func SizeOf(run *b2backupv1.BackupRun) (RunSize, bool) {
	var size RunSize
	for _, src := range run.Sources {
		size.Added += src.AddedBytes
		size.Stored += src.StoredBytes
		size.Processed += src.ProcessedBytes
	}
	return size, len(run.Sources) > 0
}

// LastRunSize is the size of the newest succeeded run, and false when that
// run recorded none, so an older run's figure is never shown as the latest.
func LastRunSize(sb *b2backupv1.ScheduledBackup) (RunSize, bool) {
	var newest *b2backupv1.BackupRun
	for i := range sb.Status.History {
		run := &sb.Status.History[i]
		if run.Result != b2backupv1.BackupRunSucceeded {
			continue
		}
		if newest == nil || runTime(run).After(runTime(newest).Time) {
			newest = run
		}
	}
	if newest == nil {
		return RunSize{}, false
	}
	return SizeOf(newest)
}

func runTime(run *b2backupv1.BackupRun) *metav1.Time {
	if run.CompletionTime != nil {
		return run.CompletionTime
	}
	if run.StartTime != nil {
		return run.StartTime
	}
	return &metav1.Time{}
}

// FormatBytes renders a size the way restic does: 512 B, 1.5 KiB, 2.0 GiB.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
