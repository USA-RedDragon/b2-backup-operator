package backup

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

const testKey = "myapp-prod/restic"

func TestResolveScheduleVerbatim(t *testing.T) {
	for _, expr := range []string{
		testSchedule, "36 * * * *", "58 * * * *",
		"27 */6 * * *", "29 */6 * * *",
		"0 1 * * *", "3 0 * * *", "20 0 * * *", "30 0 * * *",
	} {
		got, err := ResolveSchedule(expr, "any/key")
		if err != nil {
			t.Fatalf("ResolveSchedule(%q) error = %v", expr, err)
		}
		if got != expr {
			t.Errorf("ResolveSchedule(%q) = %q, want it unchanged", expr, got)
		}
	}
}

func TestResolveScheduleRejectsMalformedCron(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "  "} {
		if _, err := ResolveSchedule(expr, "k"); err == nil {
			t.Errorf("ResolveSchedule(%q) expected an error", expr)
		}
	}
}

func TestResolveScheduleRejectsNonCronWords(t *testing.T) {
	for _, expr := range []string{
		"every day at midnight ok",
		"0 0 * * monday",
		"0 0 1 * $",
		"0 0 * * sunday",
		"nightly at half past two",
	} {
		if _, err := ResolveSchedule(expr, "k"); err == nil {
			t.Errorf("ResolveSchedule(%q) was passed through to the API server", expr)
		}
	}

	for _, expr := range []string{"0 0 * * MON-FRI", "0 0 1 JAN *", "0 0 ? * SAT", "*/5 0-12 * * *"} {
		if _, err := ResolveSchedule(expr, "k"); err != nil {
			t.Errorf("ResolveSchedule(%q) error = %v, want it passed through", expr, err)
		}
	}
}

func TestResolveScheduleShorthands(t *testing.T) {
	tests := []struct {
		schedule string
		pattern  string
	}{
		{"@hourly", `^\d+ \* \* \* \*$`},
		{"@daily", `^\d+ \d+ \* \* \*$`},
		{"@weekly", `^\d+ \d+ \* \* \d$`},
		{"@monthly", `^\d+ \d+ \d+ \* \*$`},
		{"@every 6h", `^\d+ \d-23/6 \* \* \*$`},
		{"@every 15m", `^\d+-59/15 \* \* \* \*$`},
		{"@every 1h", `^\d+ \* \* \* \*$`},
	}
	for _, tt := range tests {
		got, err := ResolveSchedule(tt.schedule, "myapp-prod/restic")
		if err != nil {
			t.Fatalf("ResolveSchedule(%q) error = %v", tt.schedule, err)
		}
		if !regexp.MustCompile(tt.pattern).MatchString(got) {
			t.Errorf("ResolveSchedule(%q) = %q, want match %q", tt.schedule, got, tt.pattern)
		}
	}
}

func TestEveryAcceptsAnyWhitespaceSeparator(t *testing.T) {
	want, err := ResolveSchedule("@every 6h", testKey)
	if err != nil {
		t.Fatalf("ResolveSchedule error = %v", err)
	}
	for _, expr := range []string{"@every\t6h", "@every  6h", "@every \t 6h"} {
		got, err := ResolveSchedule(expr, testKey)
		if err != nil {
			t.Errorf("ResolveSchedule(%q) error = %v", expr, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveSchedule(%q) = %q, want %q", expr, got, want)
		}
	}

	for _, expr := range []string{"@every6h", "@every", "@everything"} {
		if _, err := ResolveSchedule(expr, testKey); err == nil {
			t.Errorf("ResolveSchedule(%q) expected an error", expr)
		}
	}
}

func TestResolveScheduleRejectsUnevenSteps(t *testing.T) {
	for _, expr := range []string{"@every 7h", "@every 25m", "@every 90s", "@every 0h", "@every 48h", "@nonsense"} {
		if _, err := ResolveSchedule(expr, "k"); err == nil {
			t.Errorf("ResolveSchedule(%q) expected an error", expr)
		}
	}
}

func TestJitterIsStableAndDistributed(t *testing.T) {
	first, err := ResolveSchedule("@hourly", testKey)
	if err != nil {
		t.Fatalf("ResolveSchedule error = %v", err)
	}
	for range 100 {
		again, err := ResolveSchedule("@hourly", testKey)
		if err != nil {
			t.Fatalf("ResolveSchedule error = %v", err)
		}
		if again != first {
			t.Fatalf("jitter is not stable: got %q then %q", first, again)
		}
	}

	names := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
		"india", "juliett", "kilo", "lima", "mike", "november", "oscar", "papa",
		"quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey",
		"xray", "yankee", "zulu",
	}
	namespaces := make([]string, 0, len(names)+5)
	for _, n := range names {
		namespaces = append(namespaces, n+"-prod")
	}
	for _, n := range names[:5] {
		namespaces = append(namespaces, n+"-staging")
	}

	minutes := map[string]int{}
	for _, ns := range namespaces {
		got, err := ResolveSchedule("@hourly", ns+"/restic")
		if err != nil {
			t.Fatalf("ResolveSchedule error = %v", err)
		}
		minutes[strings.Fields(got)[0]]++
	}

	for minute, n := range minutes {
		if n > 3 {
			t.Errorf("minute %s has %d backups, want at most 3", minute, n)
		}
	}
	if len(minutes) < 20 {
		t.Errorf("%d namespaces landed on only %d distinct minutes, want at least 20",
			len(namespaces), len(minutes))
	}
}

func TestEveryDayMatchesDaily(t *testing.T) {
	for _, key := range []string{testKey, "other-prod/restic", "third-staging/restic"} {
		daily, err := ResolveSchedule("@daily", key)
		if err != nil {
			t.Fatalf("ResolveSchedule(@daily, %q) error = %v", key, err)
		}
		every, err := ResolveSchedule("@every 24h", key)
		if err != nil {
			t.Fatalf("ResolveSchedule(@every 24h, %q) error = %v", key, err)
		}
		if daily != every {
			t.Errorf("%q: @daily = %q but @every 24h = %q", key, daily, every)
		}
	}
}

func TestEveryStepsArePhaseShifted(t *testing.T) {
	keys := []string{
		"alpha-prod/restic", "bravo-prod/restic", "charlie-prod/restic",
		"delta-prod/restic", "echo-prod/restic", "foxtrot-prod/restic",
		"golf-prod/restic", "hotel-prod/restic", "india-prod/restic",
	}
	for _, schedule := range []string{"@every 15m", "@every 6h"} {
		seen := map[string]bool{}
		for _, key := range keys {
			got, err := ResolveSchedule(schedule, key)
			if err != nil {
				t.Fatalf("ResolveSchedule(%q, %q) error = %v", schedule, key, err)
			}
			seen[got] = true
		}
		if len(seen) < 3 {
			t.Errorf("%s produced only %d distinct schedules across %d keys, want spread",
				schedule, len(seen), len(keys))
		}
	}
}

func TestEveryOffsetStaysWithinTheStep(t *testing.T) {
	for _, key := range []string{"a/x", "b/y", "c/z", testKey, "zulu-staging/restic"} {
		got, err := ResolveSchedule("@every 15m", key)
		if err != nil {
			t.Fatalf("ResolveSchedule error = %v", err)
		}
		var offset, step int
		if _, err := fmt.Sscanf(strings.Fields(got)[0], "%d-59/%d", &offset, &step); err != nil {
			t.Fatalf("unexpected minute field %q: %v", got, err)
		}
		if offset >= step {
			t.Errorf("%q: offset %d is outside step %d, so it would fire less often than asked",
				got, offset, step)
		}
	}

	for _, key := range []string{"a/x", "b/y", "c/z", testKey} {
		got, err := ResolveSchedule("@every 6h", key)
		if err != nil {
			t.Fatalf("ResolveSchedule error = %v", err)
		}
		var offset, step int
		if _, err := fmt.Sscanf(strings.Fields(got)[1], "%d-23/%d", &offset, &step); err != nil {
			t.Fatalf("unexpected hour field %q: %v", got, err)
		}
		if offset >= step {
			t.Errorf("%q: hour offset %d is outside step %d", got, offset, step)
		}
	}
}
