package backup

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func TestLastRunSizeIsTheNewestSucceededRunOnly(t *testing.T) {
	at := func(h int) *metav1.Time { return &metav1.Time{Time: time.Date(2026, 10, 11, h, 0, 0, 0, time.UTC)} }
	src := func(added int64) []b2backupv1.BackupRunSource {
		return []b2backupv1.BackupRunSource{{Tag: "files", AddedBytes: added, StoredBytes: added / 2,
			ProcessedBytes: 10 * added}, {Tag: "db", AddedBytes: 1, StoredBytes: 1, ProcessedBytes: 1}}
	}
	sb := &b2backupv1.ScheduledBackup{Status: b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{
		{JobName: "failed", Result: b2backupv1.BackupRunFailed, StartTime: at(5), Sources: src(999)},
		{JobName: "older", Result: b2backupv1.BackupRunSucceeded, StartTime: at(1), CompletionTime: at(2),
			Sources: src(100)},
		{JobName: "newer", Result: b2backupv1.BackupRunSucceeded, StartTime: at(3), CompletionTime: at(4),
			Sources: src(200)},
	}}}
	got, ok := LastRunSize(sb)
	if want := (RunSize{Added: 201, Stored: 101, Processed: 2001}); !ok || got != want {
		t.Errorf("LastRunSize = %+v, %v; want %+v from the newest succeeded run", got, ok, want)
	}

	sb.Status.History = append(sb.Status.History, b2backupv1.BackupRun{JobName: "newest",
		Result: b2backupv1.BackupRunSucceeded, StartTime: at(6), CompletionTime: at(7)})
	if got, ok := LastRunSize(sb); ok {
		t.Errorf("LastRunSize = %+v; want none when the newest succeeded run recorded no size", got)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 12_582: "12.3 KiB",
		2_040_109_465: "1.9 GiB", 1 << 62: "4.0 EiB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestParseRunSummaryReadsTheZoneThePodRunsIn(t *testing.T) {
	got := ParseRunSummary(`db {"backup_start":"2026-10-11T23:43:23-05:00","backup_end":"2026-10-11T23:43:24-05:00",` +
		`"files_new":1,"files_changed":0,"files_unmodified":0,"data_added":54105,"data_added_packed":1526,` +
		`"total_files_processed":1,"total_bytes_processed":53802}`)
	if len(got) != 1 || got[0].Tag != "db" || got[0].StoredBytes != 1526 || got[0].StartTime == nil ||
		!got[0].StartTime.Equal(&metav1.Time{Time: time.Date(2026, 10, 12, 4, 43, 23, 0, time.UTC)}) {
		t.Errorf("ParseRunSummary = %+v", got)
	}
}
