package backup

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"k8s.io/utils/ptr"
)

func hourlyRetention() *b2backupv1.Retention    { return retention(168) }
func sixHourlyRetention() *b2backupv1.Retention { return retention(28) }
func dailyRetention() *b2backupv1.Retention     { return retention(0) }

func retention(hourly int32) *b2backupv1.Retention {
	r := &b2backupv1.Retention{
		Daily:   ptr.To[int32](90),
		Monthly: ptr.To[int32](24),
		Yearly:  ptr.To[int32](10),
	}
	if hourly > 0 {
		r.Hourly = ptr.To(hourly)
	}
	return r
}

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		spec b2backupv1.ScheduledBackupSpec
		want string
	}{
		{
			name: "cnpg and files with excludes",
			spec: b2backupv1.ScheduledBackupSpec{
				Sources: []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeCNPG},
					{
						Type:    b2backupv1.SourceTypeFiles,
						Paths:   []string{appDir},
						Exclude: []string{"**/temp*", "app/export-logs"},
					},
				},
				Retention: hourlyRetention(),
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/b2backup/database
@summary db
restic backup --retry-lock=5m0s --tag=files /data/app \
  --exclude='**/temp*' \
  --exclude='app/export-logs'
@summary files
restic unlock || true
restic forget --prune --retry-lock=5m0s --host="$RESTIC_HOST" --group-by=tags --tag=db --tag=files --keep-hourly=168 --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "extra args are passed as single words",
			spec: b2backupv1.ScheduledBackupSpec{
				Sources: []b2backupv1.BackupSource{{
					Type: b2backupv1.SourceTypeMariaDB,
					ExtraArgs: []string{
						"--single-transaction",
						"--where=id > 5",
						"--ignore-table=app.*",
						"--comment=it's fine",
					},
				}},
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb mariadb --secret-mount=/var/run/secrets/b2backup/database -- --single-transaction '--where=id > 5' '--ignore-table=app.*' '--comment=it'\''s fine'
@summary db
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "mariadb with extra args",
			spec: b2backupv1.ScheduledBackupSpec{
				Sources: []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeMariaDB, ExtraArgs: []string{"--skip-ssl"}},
					{Type: b2backupv1.SourceTypeFiles, Exclude: []string{"dumps"}},
				},
				Retention: dailyRetention(),
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb mariadb --secret-mount=/var/run/secrets/b2backup/database -- --skip-ssl
@summary db
restic backup --retry-lock=5m0s --tag=files /data \
  --exclude='dumps'
@summary files
restic unlock || true
restic forget --prune --retry-lock=5m0s --host="$RESTIC_HOST" --group-by=tags --tag=db --tag=files --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "named database with custom tag",
			spec: b2backupv1.ScheduledBackupSpec{
				Sources: []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeCNPG},
					{Type: b2backupv1.SourceTypeCNPG, Tag: "db-external", Database: "reporting"},
				},
				Retention: dailyRetention(),
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/b2backup/database
@summary db
restic backup --retry-lock=5m0s --tag=db-external --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/b2backup/database --database=reporting
@summary db-external
restic unlock || true
restic forget --prune --retry-lock=5m0s --host="$RESTIC_HOST" --group-by=tags --tag=db --tag=db-external --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "files only without excludes",
			spec: b2backupv1.ScheduledBackupSpec{
				Sources:   []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}},
				Retention: hourlyRetention(),
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=files /data
@summary files
restic unlock || true
restic forget --prune --retry-lock=5m0s --host="$RESTIC_HOST" --group-by=tags --tag=files --keep-hourly=168 --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "six hourly retention tier",
			spec: b2backupv1.ScheduledBackupSpec{
				Sources:   []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
				Retention: sixHourlyRetention(),
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/b2backup/database
@summary db
restic unlock || true
restic forget --prune --retry-lock=5m0s --host="$RESTIC_HOST" --group-by=tags --tag=db --keep-hourly=28 --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "several paths on several volumes in one snapshot",
			spec: b2backupv1.ScheduledBackupSpec{
				Volumes: []b2backupv1.VolumeSpec{
					{ExistingClaim: "config", MountPath: dataMount}, {ExistingClaim: "blobs"},
				},
				Sources: []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeFiles, Paths: []string{dataMount, "/blobs"}},
					{Type: b2backupv1.SourceTypeFiles, Tag: "server", Paths: []string{"sub/../server"}},
				},
				Retention: dailyRetention(),
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=files /data /blobs
@summary files
restic backup --retry-lock=5m0s --tag=server /data/server
@summary server
restic unlock || true
restic forget --prune --retry-lock=5m0s --host="$RESTIC_HOST" --group-by=tags --tag=files --tag=server --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "raw script keeps preamble and postamble",
			spec: b2backupv1.ScheduledBackupSpec{
				Script: "restic backup --retry-lock=5m0s --tag=custom /data\n",
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=custom /data
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
		{
			name: "no retention omits forget entirely",
			spec: b2backupv1.ScheduledBackupSpec{
				Sources: []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
			},
			want: `set -o pipefail
{
set -euo pipefail
restic unlock || true
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/b2backup/database
@summary db
restic cache --cleanup
} 2>&1 | ts '%H:%M:%S'
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.spec.RetryLock == nil {
				tt.spec.RetryLock = &metav1.Duration{Duration: 5 * time.Minute}
			}
			if tt.spec.Volumes == nil {
				tt.spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: dataClaim}}
			}
			got, err := Render(&b2backupv1.ScheduledBackup{Spec: tt.spec})
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			want := summaryToken.ReplaceAllStringFunc(tt.want, func(m string) string {
				return SummaryLine(strings.TrimPrefix(m, "@summary "))
			})
			if got != want {
				t.Errorf("Render() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

var summaryToken = regexp.MustCompile(`@summary \S+`)

func TestSummaryLine(t *testing.T) {
	want := `restic snapshots --json --no-lock --group-by= --tag=db-2 --host="$RESTIC_HOST" latest 2>/dev/null | ` +
		`sed -nE '/"summary":/{s/.*"summary":(\{[^}]*\}).*/db-2 \1/;s/"(dirs_[a-z]+|data_blobs|tree_blobs)":[0-9]+,//g;` +
		`s/[.][0-9]+([Z+-])/\1/g;p;}' ` +
		`2>/dev/null >>/dev/termination-log || true`
	if got := SummaryLine("db-2"); got != want {
		t.Errorf("SummaryLine() =\n%s\nwant\n%s", got, want)
	}
	if !IsSummaryLine("  "+want) || IsSummaryLine(strings.Replace(want, "|| true", "", 1)) ||
		IsSummaryLine("restic snapshots --json") {
		t.Error("IsSummaryLine does not match exactly the lines SummaryLine renders")
	}
}

func TestScriptBackupRecordsNoSummary(t *testing.T) {
	got, err := Render(withSpec(b2backupv1.ScheduledBackupSpec{Script: "restic backup --tag=custom /data\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, SummaryCommand) || strings.Contains(got, "termination-log") {
		t.Errorf("a script backup got a summary line:\n%s", got)
	}
}

func TestRenderUnknownSourceType(t *testing.T) {
	_, err := Render(withSpec(b2backupv1.ScheduledBackupSpec{
		Sources: []b2backupv1.BackupSource{{Type: "nope"}},
	}))
	if err == nil {
		t.Fatal("Render() expected an error for an unknown source type")
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	got, err := Render(withSpec(b2backupv1.ScheduledBackupSpec{
		Sources: []b2backupv1.BackupSource{
			{Type: b2backupv1.SourceTypeFiles, Exclude: []string{`it's; rm -rf /`}},
		},
	}))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	want := `  --exclude='it'\''s; rm -rf /'`
	if !strings.Contains(got, want) {
		t.Errorf("Render() did not escape the quote\n--- got ---\n%s\n--- want line ---\n%s", got, want)
	}
}

func TestRetryLockIsAppliedToLockingCommands(t *testing.T) {
	spec := b2backupv1.ScheduledBackupSpec{
		Sources: []b2backupv1.BackupSource{
			{Type: b2backupv1.SourceTypeCNPG},
			{Type: b2backupv1.SourceTypeFiles},
		},
		Retention: hourlyRetention(),
		RetryLock: &metav1.Duration{Duration: 90 * time.Second},
	}
	got, err := Render(&b2backupv1.ScheduledBackup{Spec: spec})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, want := range []string{
		"restic backup --retry-lock=1m30s --tag=db",
		"restic backup --retry-lock=1m30s --tag=files",
		"restic forget --prune --retry-lock=1m30s --host=\"$RESTIC_HOST\" --group-by=tags --tag=db --tag=files --keep-hourly=168",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestRetryLockCanBeDisabled(t *testing.T) {
	for _, d := range []*metav1.Duration{nil, {Duration: 0}} {
		spec := b2backupv1.ScheduledBackupSpec{
			Sources:   []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
			Retention: hourlyRetention(),
			RetryLock: d,
		}
		got, err := Render(&b2backupv1.ScheduledBackup{Spec: spec})
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if strings.Contains(got, "--retry-lock") {
			t.Errorf("Render() emitted --retry-lock for %v:\n%s", d, got)
		}
	}
}

func TestOrdinaryPathsRenderUnquoted(t *testing.T) {
	got, err := Render(withSpec(b2backupv1.ScheduledBackupSpec{
		Sources: []b2backupv1.BackupSource{
			{Type: b2backupv1.SourceTypeFiles, Paths: []string{"app/storage"}},
		},
	}))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(got, "--tag=files /data/app/storage\n") {
		t.Errorf("Render() quoted an ordinary path:\n%s", got)
	}
}

func TestPathWithSpaceIsQuoted(t *testing.T) {
	got, err := Render(withSpec(b2backupv1.ScheduledBackupSpec{
		Sources: []b2backupv1.BackupSource{
			{Type: b2backupv1.SourceTypeFiles, Paths: []string{"app/user uploads"}},
		},
	}))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(got, `--tag=files '/data/app/user uploads'`) {
		t.Errorf("Render() did not quote a path with a space:\n%s", got)
	}
}

func withSpec(spec b2backupv1.ScheduledBackupSpec) *b2backupv1.ScheduledBackup {
	spec.Volumes = append(spec.Volumes, b2backupv1.VolumeSpec{ExistingClaim: dataClaim})
	return &b2backupv1.ScheduledBackup{Spec: spec}
}

func TestRenderRemovesStaleLocksBeforeLocking(t *testing.T) {
	for name, spec := range map[string]b2backupv1.ScheduledBackupSpec{
		"sources": {
			Sources:   []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}, {Type: b2backupv1.SourceTypeFiles}},
			Retention: hourlyRetention(),
		},
		"script": {Script: "restic backup --tag=custom /data\nrestic forget --prune --keep-last=3"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Render(&b2backupv1.ScheduledBackup{Spec: spec})
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			lines := strings.Split(got, "\n")
			first := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "restic ") })
			if first < 0 || lines[first] != UnlockLine {
				t.Errorf("the first restic command is not %q:\n%s", UnlockLine, got)
			}
			if spec.Script != "" {
				return
			}
			forget := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "restic forget ") })
			if forget < 1 || lines[forget-1] != UnlockLine {
				t.Errorf("forget does not follow %q:\n%s", UnlockLine, got)
			}
		})
	}
}
