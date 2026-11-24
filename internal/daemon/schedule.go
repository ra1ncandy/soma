package daemon

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var shorthandEventRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`)

// Schedule represents either a fixed Go/day duration interval ("15m", "24h", "7d")
// or a standard 5-field cron expression ("0 9 * * 1-5", "*/15 * * * *").
type Schedule struct {
	Raw      string
	IsCron   bool
	Interval time.Duration

	minutes [60]bool
	hours   [24]bool
	dom     [32]bool // 1..31
	months  [13]bool // 1..12
	dow     [7]bool  // 0..6 (0 = Sunday)
}

// ParseSchedule parses either a duration ("15m", "1h", "7d") or a 5-field cron expression.
func ParseSchedule(expr string) (Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Schedule{}, errors.New("schedule expression is required (e.g. '15m', '1h', or '0 9 * * *')")
	}
	fields := strings.Fields(expr)
	if len(fields) == 1 {
		dur, err := ParseDurationExtended(expr)
		if err != nil || dur <= 0 {
			return Schedule{}, fmt.Errorf("invalid schedule %q: expected positive duration (e.g. 15m, 1h, 24h, 7d) or 5-field cron (e.g. '0 9 * * *')", expr)
		}
		return Schedule{Raw: expr, Interval: dur}, nil
	}
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("invalid cron expression %q: expected 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}

	var s Schedule
	s.Raw = expr
	s.IsCron = true

	minSet, err := parseCronField(fields[0], 0, 59, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron minute %q: %w", fields[0], err)
	}
	copy(s.minutes[:], minSet)

	hrSet, err := parseCronField(fields[1], 0, 23, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron hour %q: %w", fields[1], err)
	}
	copy(s.hours[:], hrSet)

	domSet, err := parseCronField(fields[2], 1, 31, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron day-of-month %q: %w", fields[2], err)
	}
	copy(s.dom[:], domSet)

	monSet, err := parseCronField(fields[3], 1, 12, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron month %q: %w", fields[3], err)
	}
	copy(s.months[:], monSet)

	dowSet, err := parseCronField(fields[4], 0, 7, true)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron day-of-week %q: %w", fields[4], err)
	}
	copy(s.dow[:], dowSet[:7])

	return s, nil
}

func parseCronField(raw string, minVal, maxVal int, wrapSunday bool) ([]bool, error) {
	out := make([]bool, maxVal+1)
	parts := strings.Split(raw, ",")
	matched := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("empty list item")
		}
		step := 1
		rangePart := part
		if slashIdx := strings.IndexByte(part, '/'); slashIdx >= 0 {
			rangePart = part[:slashIdx]
			stepVal, err := strconv.Atoi(part[slashIdx+1:])
			if err != nil || stepVal <= 0 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			step = stepVal
		}
		var lo, hi int
		switch {
		case rangePart == "*":
			lo, hi = minVal, maxVal
			if wrapSunday && hi == 7 {
				hi = 6
			}
		case strings.Contains(rangePart, "-"):
			bounds := strings.SplitN(rangePart, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil || a < minVal || b > maxVal || a > b {
				return nil, fmt.Errorf("range %q out of bounds [%d..%d]", rangePart, minVal, maxVal)
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil || v < minVal || v > maxVal {
				return nil, fmt.Errorf("value %q out of bounds [%d..%d]", rangePart, minVal, maxVal)
			}
			lo = v
			if strings.Contains(part, "/") {
				hi = maxVal
				if wrapSunday && hi == 7 {
					hi = 6
				}
			} else {
				hi = v
			}
		}
		for v := lo; v <= hi; v += step {
			idx := v
			if wrapSunday && idx == 7 {
				idx = 0
			}
			out[idx] = true
			matched = true
		}
	}
	if !matched {
		return nil, errors.New("field matches no values")
	}
	return out, nil
}

// Next returns the next firing timestamp strictly after now.
func (s Schedule) Next(now time.Time) time.Time {
	if !s.IsCron {
		d := s.Interval
		if d <= 0 {
			d = time.Minute
		}
		return now.Add(d)
	}
	// Advance to the start of the next whole minute.
	cand := now.Truncate(time.Minute).Add(time.Minute)
	// Search up to 4 years (~2.1M minutes) to cover leap-day schedules cleanly.
	limit := cand.Add(4 * 366 * 24 * time.Hour)
	for !cand.After(limit) {
		if !s.months[int(cand.Month())] {
			cand = time.Date(cand.Year(), cand.Month()+1, 1, 0, 0, 0, 0, cand.Location())
			continue
		}
		if !s.dom[cand.Day()] || !s.dow[int(cand.Weekday())] {
			cand = time.Date(cand.Year(), cand.Month(), cand.Day()+1, 0, 0, 0, 0, cand.Location())
			continue
		}
		if !s.hours[cand.Hour()] {
			cand = time.Date(cand.Year(), cand.Month(), cand.Day(), cand.Hour()+1, 0, 0, 0, cand.Location())
			continue
		}
		if !s.minutes[cand.Minute()] {
			cand = cand.Add(time.Minute)
			continue
		}
		return cand
	}
	return now.Add(24 * time.Hour)
}

// NextDelay returns the duration from now until the next scheduled firing.
func (s Schedule) NextDelay(now time.Time) time.Duration {
	if !s.IsCron {
		if s.Interval <= 0 {
			return time.Minute
		}
		return s.Interval
	}
	d := s.Next(now).Sub(now)
	if d <= 0 {
		return time.Second
	}
	return d
}

// ValidateFilter validates webhook event filter syntax before saving triggers configuration.
func ValidateFilter(filter string) error {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return nil
	}
	clauses := strings.Split(filter, "&&")
	for _, raw := range clauses {
		clause := strings.TrimSpace(raw)
		if clause == "" {
			return fmt.Errorf("invalid filter %q: empty clause around '&&'", filter)
		}
		var op string
		if strings.Contains(clause, "!=") {
			op = "!="
		} else if strings.Contains(clause, "==") {
			op = "=="
		}
		if op != "" {
			parts := strings.SplitN(clause, op, 2)
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if key == "" || val == "" {
				return fmt.Errorf("invalid filter clause %q: expected key %s \"value\"", clause, op)
			}
			continue
		}
		if shorthandEventRe.MatchString(clause) {
			continue
		}
		return fmt.Errorf("invalid filter clause %q: use key == \"value\", key != \"value\", or an event name like pull_request.opened", clause)
	}
	return nil
}
