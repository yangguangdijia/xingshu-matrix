// Package backup holds pure helpers for scheduled platform backups:
// cron parsing, slot keys and retention pruning.
package backup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed standard 5-field cron expression.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bitsets
	domStar, dowStar              bool
}

type fieldSpec struct {
	name     string
	min, max int
}

var specs = [5]fieldSpec{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7},
}

// ParseCron parses "min hour dom month dow". Supports *, lists, ranges and
// steps. Day-of-week accepts 0-7 (0 and 7 are Sunday). When both dom and dow
// are restricted, a time matches if either matches (cron semantics).
func ParseCron(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron: expected 5 fields, got %d in %q", len(fields), expr)
	}
	var bits [5]uint64
	for i, f := range fields {
		b, err := parseField(f, specs[i])
		if err != nil {
			return Schedule{}, err
		}
		bits[i] = b
	}
	dow := bits[4]
	if dow&(1<<7) != 0 {
		dow = (dow | 1) &^ (1 << 7)
	}
	return Schedule{
		minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: dow,
		domStar: strings.HasPrefix(fields[2], "*"),
		dowStar: strings.HasPrefix(fields[4], "*"),
	}, nil
}

func parseField(f string, s fieldSpec) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(f, ",") {
		if part == "" {
			return 0, fmt.Errorf("cron: empty item in %s field %q", s.name, f)
		}
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("cron: bad step %q in %s field", stepStr, s.name)
			}
			step = n
		}
		var lo, hi int
		switch {
		case rng == "*":
			lo, hi = s.min, s.max
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err1, err2 error
			lo, err1 = strconv.Atoi(a)
			hi, err2 = strconv.Atoi(b)
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("cron: bad range %q in %s field", rng, s.name)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, fmt.Errorf("cron: bad value %q in %s field", rng, s.name)
			}
			lo, hi = n, n
			if hasStep {
				hi = s.max
			}
		}
		if lo < s.min || hi > s.max || lo > hi {
			return 0, fmt.Errorf("cron: %q out of range %d-%d in %s field", part, s.min, s.max, s.name)
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func (s Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next returns the first matching minute strictly after `after`, in after's
// location. It returns the zero time if nothing matches within 5 years
// (e.g. "0 0 30 2 *").
func (s Schedule) Next(after time.Time) time.Time {
	loc := after.Location()
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Truncate(time.Minute).Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// SlotKey identifies a schedule slot: UTC, minute precision,
// e.g. "2026-11-14T02:00Z".
func SlotKey(t time.Time) string {
	return t.UTC().Truncate(time.Minute).Format("2006-01-02T15:04Z")
}
