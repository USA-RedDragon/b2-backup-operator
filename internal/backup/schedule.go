package backup

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

var cronFieldChars = [5]*regexp.Regexp{
	regexp.MustCompile(`^[0-9*/,?-]+$`),
	regexp.MustCompile(`^[0-9*/,?-]+$`),
	regexp.MustCompile(`^[0-9*/,?-]+$`),
	regexp.MustCompile(`^(?i:[0-9*/,?-]|jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)+$`),
	regexp.MustCompile(`^(?i:[0-9*/,?-]|sun|mon|tue|wed|thu|fri|sat)+$`),
}

// ResolveSchedule is ResolveScheduleIn for UTC, which has no daylight saving.
func ResolveSchedule(schedule, key string) (string, error) {
	return ResolveScheduleIn(schedule, key, "UTC")
}

// ResolveScheduleIn expands a cron shorthand such as @daily or @every 6h into a 5-field
// cron expression, jittered by key so backups do not all fire at once. Plain cron passes
// through. A jittered time of day that daylight saving skips or repeats in zone, such as
// 01:xx or 02:xx in America/Chicago, is moved to another hour at the same minute; every
// other schedule is the same in every zone. An empty zone counts as UTC.
func ResolveScheduleIn(schedule, key, zone string) (string, error) {
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
	phase := (h / 60) % 24
	unsafe, err := transitionMinutes(zone)
	if err != nil {
		return "", err
	}
	hour := phase
	if unsafe[hour*60+minute] {
		var safe []int
		for i := range 24 {
			if !unsafe[i*60+minute] {
				safe = append(safe, i)
			}
		}
		hour = safe[jitter(key+"\x00dst")%len(safe)]
	}

	if d, ok := everyDuration(schedule); ok {
		return resolveEvery(d, minute, phase, hour)
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
func resolveEvery(d string, minute, phase, hour int) (string, error) {
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
		return fmt.Sprintf("%d %d-23/%d * * *", minute, phase%hours, hours), nil

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

var transitionCache sync.Map

// transitionMinutes returns the local minutes of the day, counted from
// midnight, that a daylight saving transition of zone skips or repeats in any
// year from 2020 to 2050. A stepped schedule shorter than a day cannot avoid
// them, so only a schedule that fires at one time of the day is moved.
func transitionMinutes(zone string) (map[int]bool, error) {
	if zone == "" {
		zone = "UTC"
	}
	if minutes, ok := transitionCache.Load(zone); ok {
		return minutes.(map[int]bool), nil
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("time zone %q: %w", zone, err)
	}
	minutes := map[int]bool{}
	end := time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC)
	for at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC); at.Before(end); {
		_, next := at.In(loc).ZoneBounds()
		if next.IsZero() || !next.Before(end) {
			break
		}
		if !next.After(at) {
			at = at.Add(time.Second)
			continue
		}
		_, before := next.Add(-time.Second).In(loc).Zone()
		_, after := next.In(loc).Zone()
		lo, hi := next.Unix()+int64(before), next.Unix()+int64(after)
		if lo > hi {
			lo, hi = hi, lo
		}
		for wall := lo - lo%60; wall < hi; wall += 60 {
			t := time.Unix(wall, 0).UTC()
			minutes[t.Hour()*60+t.Minute()] = true
		}
		at = next
	}
	transitionCache.Store(zone, minutes)
	return minutes, nil
}
