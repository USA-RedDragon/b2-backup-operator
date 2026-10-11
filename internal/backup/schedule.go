package backup

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"
)

var cronFieldChars = [5]*regexp.Regexp{
	regexp.MustCompile(`^[0-9*/,?-]+$`),
	regexp.MustCompile(`^[0-9*/,?-]+$`),
	regexp.MustCompile(`^[0-9*/,?-]+$`),
	regexp.MustCompile(`^(?i:[0-9*/,?-]|jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)+$`),
	regexp.MustCompile(`^(?i:[0-9*/,?-]|sun|mon|tue|wed|thu|fri|sat)+$`),
}

// ResolveSchedule expands a cron shorthand such as @daily or @every 6h into a 5-field cron
// expression, jittered by key so backups do not all fire at once. Plain cron passes through.
func ResolveSchedule(schedule, key string) (string, error) {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return "", fmt.Errorf("schedule is empty")
	}

	if !strings.HasPrefix(schedule, "@") {
		fields := strings.Fields(schedule)
		if len(fields) != 5 {
			return "", fmt.Errorf("cron expression %q has %d fields, want 5", schedule, len(fields))
		}
		for i, f := range fields {
			if !cronFieldChars[i].MatchString(f) {
				return "", fmt.Errorf("field %d of cron expression %q is not a cron field: %q",
					i+1, schedule, f)
			}
		}
		return schedule, nil
	}

	h := jitter(key)
	minute := h % 60
	hour := (h / 60) % 24

	if d, ok := everyDuration(schedule); ok {
		return resolveEvery(d, minute, hour)
	}

	switch schedule {
	case "@hourly":
		return fmt.Sprintf("%d * * * *", minute), nil
	case "@daily", "@midnight":
		return fmt.Sprintf("%d %d * * *", minute, hour), nil
	case "@weekly":
		return fmt.Sprintf("%d %d * * %d", minute, hour, (h/1440)%7), nil
	case "@monthly":

		return fmt.Sprintf("%d %d %d * *", minute, hour, (h/1440)%28+1), nil
	case "@yearly", "@annually":
		return fmt.Sprintf("%d %d %d %d *", minute, hour, (h/1440)%28+1, (h/40320)%12+1), nil
	default:
		return "", fmt.Errorf("unknown schedule shorthand %q", schedule)
	}
}

func everyDuration(schedule string) (string, bool) {
	rest, ok := strings.CutPrefix(schedule, "@every")
	if !ok {
		return "", false
	}
	d := strings.TrimLeft(rest, " \t")
	if d == rest || d == "" {
		return "", false
	}
	return d, true
}

// resolveEvery expands "@every <duration>" into a stepped cron expression.
//
// Only durations dividing evenly into an hour or a day are accepted, because
// cron steps restart each period: "@every 7h" would fire at 00:00, 07:00, 14:00,
// 21:00, then again three hours later.
//
// The steps are phase-shifted ("M-59/15", not "*/15") so a stepped schedule is
// jittered like every other shorthand. A plain step always starts at zero, which
// is the top-of-the-hour stampede jitter exists to avoid.
func resolveEvery(d string, minute, hour int) (string, error) {
	dur, err := time.ParseDuration(d)
	if err != nil {
		return "", fmt.Errorf("parsing @every duration %q: %w", d, err)
	}
	switch {
	case dur <= 0:
		return "", fmt.Errorf("@every duration %q must be positive", d)

	case dur < time.Hour:
		mins := int(dur.Minutes())
		if mins == 0 || dur%time.Minute != 0 {
			return "", fmt.Errorf("@every duration %q must be a whole number of minutes", d)
		}
		if 60%mins != 0 {
			return "", fmt.Errorf("@every duration %q must divide evenly into an hour", d)
		}
		return fmt.Sprintf("%d-59/%d * * * *", minute%mins, mins), nil

	case dur == time.Hour:
		return fmt.Sprintf("%d * * * *", minute), nil

	case dur < 24*time.Hour:
		hours := int(dur.Hours())
		if dur%time.Hour != 0 {
			return "", fmt.Errorf("@every duration %q must be a whole number of hours", d)
		}
		if 24%hours != 0 {
			return "", fmt.Errorf("@every duration %q must divide evenly into a day", d)
		}
		return fmt.Sprintf("%d %d-23/%d * * *", minute, hour%hours, hours), nil

	case dur == 24*time.Hour:

		return fmt.Sprintf("%d %d * * *", minute, hour), nil

	default:
		return "", fmt.Errorf("@every duration %q is longer than a day; use an explicit cron expression", d)
	}
}

func jitter(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % (1 << 24))
}
