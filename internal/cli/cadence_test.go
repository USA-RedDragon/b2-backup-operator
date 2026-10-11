package cli

import (
	"fmt"
	"testing"
)

const (
	cronHourly     = "36 * * * *"
	cronDaily      = "20 0 * * *"
	cronSixHourly  = "27 */6 * * *"
	cronEvery15Min = "*/15 * * * *"
	cronMonthly    = "0 3 1 * *"
)

func TestCadenceSurvivesJittering(t *testing.T) {
	tests := []struct {
		name     string
		original string
		jittered string
	}{
		{"hourly", cronHourly, "7 * * * *"},
		{"hourly at the top of the hour", "0 * * * *", "59 * * * *"},
		{"daily", cronDaily, "33 20 * * *"},
		{"daily across midnight", "0 1 * * *", "58 23 * * *"},
		{"every six hours", cronSixHourly, "14 3-23/6 * * *"},
		{"every fifteen minutes", cronEvery15Min, "7-59/15 * * * *"},
		{"weekly", "0 3 * * 0", "41 17 * * 4"},
		{"weekly on a different day", "0 3 * * 1", "41 17 * * 3"},
		{"weekly on saturday", "0 3 * * 6", "41 17 * * 3"},
		{"monthly", cronMonthly, "41 17 28 * *"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original, err := cadenceOf(tt.original)
			if err != nil {
				t.Fatalf("cadenceOf(%q) error = %v", tt.original, err)
			}
			jittered, err := cadenceOf(tt.jittered)
			if err != nil {
				t.Fatalf("cadenceOf(%q) error = %v", tt.jittered, err)
			}
			if !original.equal(jittered) {
				t.Errorf("%q runs %s but %q runs %s; jitter must not change how often a backup runs",
					tt.original, original, tt.jittered, jittered)
			}
		})
	}
}

func TestCadenceCatchesRealChanges(t *testing.T) {
	tests := []struct {
		name string
		a, b string
	}{
		{"hourly became daily", cronHourly, "36 3 * * *"},
		{"daily became weekly", cronDaily, "20 0 * * 1"},
		{"six hourly became twelve hourly", cronSixHourly, "27 */12 * * *"},
		{"quarter hourly became half hourly", cronEvery15Min, "*/30 * * * *"},
		{"hourly became twice hourly", cronHourly, "6,36 * * * *"},
		{"monthly became yearly", cronMonthly, "0 3 10 1 *"},
		{"monthly became quarterly", cronMonthly, "0 3 1 1,4,7,10 *"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := cadenceOf(tt.a)
			if err != nil {
				t.Fatal(err)
			}
			b, err := cadenceOf(tt.b)
			if err != nil {
				t.Fatal(err)
			}
			if a.equal(b) {
				t.Errorf("%q (%s) and %q (%s) were treated as the same cadence", tt.a, a, tt.b, b)
			}
		})
	}
}

func TestCadenceReportsUnevenSteps(t *testing.T) {
	c, err := cadenceOf("0 */7 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.gaps) == 0 || c.gaps[0] == c.gaps[len(c.gaps)-1] {
		t.Errorf("%s: expected an uneven spacing for a 7 hour step", c)
	}
}

func TestCadenceCounts(t *testing.T) {
	tests := []struct {
		expr  string
		fires int
	}{
		{cronHourly, cadenceWindowDays * 24},
		{cronDaily, cadenceWindowDays},
		{cronSixHourly, cadenceWindowDays * 4},
		{cronEvery15Min, cadenceWindowDays * 24 * 4},
		{cronMonthly, 12},
		{"0 3 10 1 *", 1},
	}
	for _, tt := range tests {
		c, err := cadenceOf(tt.expr)
		if err != nil {
			t.Fatalf("cadenceOf(%q) error = %v", tt.expr, err)
		}
		if c.fires != tt.fires {
			t.Errorf("cadenceOf(%q).fires = %d, want %d", tt.expr, c.fires, tt.fires)
		}
	}
}

func TestCadenceRejectsMalformed(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "99 * * * *", "x * * * *", "0 */0 * * *"} {
		if _, err := cadenceOf(expr); err == nil {
			t.Errorf("cadenceOf(%q) expected an error", expr)
		}
	}
}

func TestFieldMatcherAcceptsRealCronSyntax(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		sameAs   string
		notEmpty bool
	}{
		{name: "sunday as seven", expr: "0 0 * * 7", sameAs: "0 0 * * 0"},
		{name: "sunday as a name", expr: "0 0 * * SUN", sameAs: "0 0 * * 0"},
		{name: "weekday range by name", expr: "0 0 * * MON-FRI", sameAs: "0 0 * * 1-5"},
		{name: "seven in a list", expr: "0 0 * * 3,7", sameAs: "0 0 * * 0,3"},
		{name: "month by name", expr: "0 0 1 JAN *", sameAs: "0 0 1 1 *"},
		{name: "month range by name", expr: "0 0 1 JAN-mar *", sameAs: "0 0 1 1-3 *"},
		{name: "stepped from a start", expr: "5/15 * * * *", sameAs: "5,20,35,50 * * * *"},
		{name: "stepped hour from a start", expr: "0 2/6 * * *", sameAs: "0 2,8,14,20 * * *"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cadenceOf(tt.expr)
			if err != nil {
				t.Fatalf("cadenceOf(%q) error = %v", tt.expr, err)
			}
			want, err := cadenceOf(tt.sameAs)
			if err != nil {
				t.Fatal(err)
			}
			if got.fires == 0 {
				t.Fatalf("cadenceOf(%q) never fires", tt.expr)
			}
			if !got.equal(want) {
				t.Errorf("cadenceOf(%q) = %s, want the same as %q (%s)", tt.expr, got, tt.sameAs, want)
			}
		})
	}
}

func TestCadenceWindowIsWholeWeeks(t *testing.T) {
	if cadenceWindowDays%7 != 0 {
		t.Fatalf("cadenceWindowDays = %d, which is not a whole number of weeks; "+
			"weekdays would fire an uneven number of times", cadenceWindowDays)
	}
	first, err := cadenceOf("0 3 * * 0")
	if err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 6; day++ {
		c, err := cadenceOf(fmt.Sprintf("0 3 * * %d", day))
		if err != nil {
			t.Fatal(err)
		}
		if c.fires != first.fires {
			t.Errorf("weekday %d fires %d times but Sunday fires %d", day, c.fires, first.fires)
		}
	}
}

func TestCadenceDistinguishesSingleFirings(t *testing.T) {
	monthly, err := cadenceOf(cronMonthly)
	if err != nil {
		t.Fatal(err)
	}
	yearly, err := cadenceOf("0 3 10 1 *")
	if err != nil {
		t.Fatal(err)
	}
	if monthly.equal(yearly) {
		t.Errorf("a monthly schedule (%s) compared equal to a yearly one (%s)", monthly, yearly)
	}
}

func TestCadenceSeesEveryDayOfTheMonth(t *testing.T) {
	for day := 1; day <= 28; day++ {
		expr := fmt.Sprintf("0 3 %d * *", day)
		c, err := cadenceOf(expr)
		if err != nil {
			t.Fatal(err)
		}
		if c.fires == 0 {
			t.Errorf("cadenceOf(%q) never fires, so parity would compare nothing", expr)
		}
	}
}
