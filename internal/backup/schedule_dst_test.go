package backup_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	dstKeys = 2000
	daily   = "@daily"
	chicago = "America/Chicago"
)

func clock(t *testing.T, schedule string) (hour, minute int) {
	t.Helper()
	fields := strings.Fields(schedule)
	m, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("minute of %q: %v", schedule, err)
	}
	h, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("hour of %q: %v", schedule, err)
	}
	return h, m
}

func transitionDays(loc *time.Location) []time.Time {
	var days []time.Time
	day := time.Date(2026, 1, 1, 12, 0, 0, 0, loc)
	_, prev := day.Zone()
	for ; day.Year() <= 2036; day = day.AddDate(0, 0, 1) {
		if _, off := day.Zone(); off != prev {
			days = append(days, day.AddDate(0, 0, -1), day)
			prev = off
		}
	}
	return days
}

func ambiguous(loc *time.Location, days []time.Time, hour, minute int) bool {
	for _, day := range days {
		at := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
		if at.Hour() != hour || at.Minute() != minute {
			return true
		}
		for _, other := range []time.Time{at.Add(time.Hour), at.Add(-time.Hour), at.Add(30 * time.Minute), at.Add(-30 * time.Minute)} {
			if other.Hour() == hour && other.Minute() == minute {
				return true
			}
		}
	}
	return false
}

func TestJitterAvoidsDaylightSavingTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		zone      string
		shorthand string
		wantMoved bool
	}{
		{chicago, daily, true},
		{chicago, "@weekly", true},
		{chicago, "@monthly", true},
		{chicago, "@yearly", true},
		{chicago, "@every 24h", true},
		{"Europe/London", daily, true},
		{"Australia/Lord_Howe", daily, true},
		{"UTC", daily, false},
		{"", daily, false},
	}
	for _, tt := range tests {
		t.Run(tt.zone+"/"+tt.shorthand, func(t *testing.T) {
			t.Parallel()
			loc, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Fatal(err)
			}
			days := transitionDays(loc)
			seen := map[int]bool{}
			moved := 0
			bad := map[[2]int]bool{}
			for i := range dstKeys {
				key := fmt.Sprintf("ns-%d/restic", i)
				got, err := backup.ResolveScheduleIn(tt.shorthand, key, tt.zone)
				if err != nil {
					t.Fatal(err)
				}
				plain, err := backup.ResolveSchedule(tt.shorthand, key)
				if err != nil {
					t.Fatal(err)
				}
				h, m := clock(t, got)
				ph, pm := clock(t, plain)
				seen[h] = true
				if pm != m || strings.Join(strings.Fields(got)[2:], " ") != strings.Join(strings.Fields(plain)[2:], " ") {
					t.Fatalf("%s: %q differs from %q in more than the hour", key, got, plain)
				}
				known, checked := bad[[2]int{h, m}]
				if !checked {
					known = ambiguous(loc, days, h, m)
					bad[[2]int{h, m}] = known
				}
				if known {
					t.Fatalf("%s in %s resolved to %q, a local time daylight saving skips or repeats", key, tt.zone, got)
				}
				if h != ph {
					moved++
					if !ambiguous(loc, days, ph, pm) {
						t.Fatalf("%s moved from %q to %q although %q is safe", key, plain, got, plain)
					}
				}
			}
			if (moved > 0) != tt.wantMoved {
				t.Errorf("%d of %d schedules moved, want moved = %t", moved, dstKeys, tt.wantMoved)
			}
			if tt.wantMoved && len(seen) < 20 {
				t.Errorf("only %d distinct hours used", len(seen))
			}
		})
	}
}

func TestStepsShorterThanADayKeepTheirPhase(t *testing.T) {
	t.Parallel()
	for _, shorthand := range []string{"@every 6h", "@every 2h", "@hourly", "@every 15m"} {
		t.Run(shorthand, func(t *testing.T) {
			t.Parallel()
			for i := range 200 {
				key := fmt.Sprintf("ns-%d/restic", i)
				got, err := backup.ResolveScheduleIn(shorthand, key, chicago)
				if err != nil {
					t.Fatal(err)
				}
				plain, err := backup.ResolveSchedule(shorthand, key)
				if err != nil {
					t.Fatal(err)
				}
				if got != plain {
					t.Errorf("%s: %q in America/Chicago, %q in UTC", key, got, plain)
				}
			}
		})
	}
}

func TestAnUnknownTimeZoneIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := backup.ResolveScheduleIn(daily, "ns/restic", "Mars/Olympus_Mons"); err == nil {
		t.Error("an unknown time zone was accepted")
	}
}

func TestTheCronJobScheduleAvoidsDaylightSavingTransitions(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation(chicago)
	if err != nil {
		t.Fatal(err)
	}
	days := transitionDays(loc)
	tests := []struct {
		name  string
		hour  int
		zone  string
		moves bool
	}{
		{name: "02:xx in America/Chicago", hour: 2, zone: chicago, moves: true},
		{name: "01:xx in America/Chicago", hour: 1, zone: chicago, moves: true},
		{name: "02:xx in UTC", hour: 2, zone: "UTC"},
		{name: "05:xx in America/Chicago", hour: 5, zone: chicago},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var sb *b2backupv1.ScheduledBackup
			var plain string
			for i := 0; sb == nil; i++ {
				name := fmt.Sprintf("app-%d", i)
				s, err := backup.ResolveSchedule(daily, "prod/"+name)
				if err != nil {
					t.Fatal(err)
				}
				if h, _ := clock(t, s); h == tt.hour {
					plain = s
					sb = &b2backupv1.ScheduledBackup{Namespace: "prod", Name: name, Spec: b2backupv1.ScheduledBackupSpec{
						RepositoryRef: corev1.LocalObjectReference{Name: "store"}, Schedule: daily, TimeZone: tt.zone,
						Sources: []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
					}}
				}
			}
			cj, err := backup.BuildCronJob(sb, &b2backupv1.Repository{Namespace: "prod", Name: "store"}, backup.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			h, m := clock(t, cj.Spec.Schedule)
			if tt.moves && ambiguous(loc, days, h, m) {
				t.Errorf("schedule %q in %s fires at a local time daylight saving skips or repeats", cj.Spec.Schedule, tt.zone)
			}
			if !tt.moves && cj.Spec.Schedule != plain {
				t.Errorf("schedule %q moved from %q", cj.Spec.Schedule, plain)
			}
		})
	}
}
