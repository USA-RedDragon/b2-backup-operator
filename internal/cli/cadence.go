package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const cadenceWindowDays = 364

type cadence struct {
	fires int
	gaps  []int
}

func (c cadence) equal(other cadence) bool {
	return c.fires == other.fires && slices.Equal(c.gaps, other.gaps)
}

func (c cadence) String() string {
	if c.fires == 0 {
		return "never"
	}
	if len(c.gaps) == 0 {
		return fmt.Sprintf("%d times per %d days", c.fires, cadenceWindowDays)
	}
	if c.gaps[0] == c.gaps[len(c.gaps)-1] {
		return fmt.Sprintf("every %s", time.Duration(c.gaps[0])*time.Minute)
	}
	return fmt.Sprintf("%d times per %d days, %s to %s apart", c.fires, cadenceWindowDays,
		time.Duration(c.gaps[0])*time.Minute, time.Duration(c.gaps[len(c.gaps)-1])*time.Minute)
}

type cronField struct {
	minValue, maxValue int
	names              map[string]int
	wrap               bool
}

var (
	monthNames = map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
	dayNames = map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}

	cronFields = [5]cronField{
		{minValue: 0, maxValue: 59},
		{minValue: 0, maxValue: 23},
		{minValue: 1, maxValue: 31},
		{minValue: 1, maxValue: 12, names: monthNames},
		{minValue: 0, maxValue: 7, names: dayNames, wrap: true},
	}
)

// cadenceOf expands a five-field cron expression over cadenceWindowDays and
// records how often it fires and how far apart.
//
// When both day-of-month and day-of-week are restricted, real cron ORs them
// where this ANDs. Migration only ever converts expressions whose day, month and
// weekday fields are all "*", and a schedule it leaves pinned is compared against
// itself, so the difference cannot arise here. It would in any other caller.
func cadenceOf(expr string) (cadence, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return cadence{}, fmt.Errorf("cron expression %q has %d fields, want 5", expr, len(fields))
	}

	matchers := make([]func(int) bool, 5)
	for i, f := range fields {
		m, err := fieldMatcher(f, cronFields[i])
		if err != nil {
			return cadence{}, fmt.Errorf("field %d of %q: %w", i+1, expr, err)
		}
		matchers[i] = m
	}

	start := time.Date(2026, time.January, 5, 0, 0, 0, 0, time.UTC)
	const window = cadenceWindowDays * 24 * 60

	var fired []int
	for m := range window {
		t := start.Add(time.Duration(m) * time.Minute)
		if matchers[0](t.Minute()) && matchers[1](t.Hour()) && matchers[2](t.Day()) &&
			matchers[3](int(t.Month())) && matchers[4](int(t.Weekday())) {
			fired = append(fired, m)
		}
	}

	gaps := make([]int, 0, len(fired))
	for i := 1; i < len(fired); i++ {
		gaps = append(gaps, fired[i]-fired[i-1])
	}
	slices.Sort(gaps)
	return cadence{fires: len(fired), gaps: gaps}, nil
}

func fieldMatcher(field string, f cronField) (func(int) bool, error) {
	allowed := map[int]bool{}

	for part := range strings.SplitSeq(field, ",") {
		step := 1
		stepped := false
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
			stepped = true
			part = base
		}

		lo, hi := f.minValue, f.maxValue
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			var ok bool
			if lo, ok = cronValue(a, f.names); !ok {
				return nil, fmt.Errorf("bad range start %q", a)
			}
			if hi, ok = cronValue(b, f.names); !ok {
				return nil, fmt.Errorf("bad range end %q", b)
			}
		default:
			n, ok := cronValue(part, f.names)
			if !ok {
				return nil, fmt.Errorf("bad value %q", part)
			}
			lo, hi = n, n
			if stepped {
				hi = f.maxValue
			}
		}

		if lo < f.minValue || hi > f.maxValue || lo > hi {
			return nil, fmt.Errorf("%q is outside %d-%d", part, f.minValue, f.maxValue)
		}
		for v := lo; v <= hi; v += step {
			allowed[v] = true
		}
	}

	if f.wrap && allowed[f.maxValue] {
		allowed[f.minValue] = true
	}

	return func(v int) bool { return allowed[v] }, nil
}

func cronValue(s string, names map[string]int) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	n, ok := names[strings.ToLower(s)]
	return n, ok
}
