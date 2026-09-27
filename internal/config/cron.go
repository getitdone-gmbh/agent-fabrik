package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed 5-field cron expression (minute hour dom month dow).
type Cron struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

var cronAliases = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

// ParseCron parses expressions like "*/15 8-18 * * 1-5" or "@daily".
func ParseCron(expr string) (*Cron, error) {
	if a, ok := cronAliases[strings.TrimSpace(expr)]; ok {
		expr = a
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: expected 5 fields", expr)
	}
	c := &Cron{}
	var err error
	if c.minute, err = cronField(f[0], 0, 59); err != nil {
		return nil, err
	}
	if c.hour, err = cronField(f[1], 0, 23); err != nil {
		return nil, err
	}
	if c.dom, err = cronField(f[2], 1, 31); err != nil {
		return nil, err
	}
	if c.month, err = cronField(f[3], 1, 12); err != nil {
		return nil, err
	}
	if c.dow, err = cronField(f[4], 0, 7); err != nil {
		return nil, err
	}
	if has(c.dow, 7) { // 7 is Sunday too
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*", f[4] == "*"
	return c, nil
}

func cronField(s string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("cron: bad step in %q", part)
			}
			step, part = n, part[:i]
		}
		start, end := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			ab := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(ab[0])
			b, err2 := strconv.Atoi(ab[1])
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("cron: bad range %q", part)
			}
			start, end = a, b
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return 0, fmt.Errorf("cron: bad value %q", part)
			}
			start, end = n, n
			if step > 1 {
				end = hi
			}
		}
		if start < lo || end > hi || start > end {
			return 0, fmt.Errorf("cron: %q out of range %d-%d", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }

// Next returns the first matching time strictly after t.
func (c *Cron) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !has(c.month, int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !has(c.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !has(c.minute, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

func (c *Cron) dayMatches(t time.Time) bool {
	dom, dow := has(c.dom, t.Day()), has(c.dow, int(t.Weekday()))
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	default: // classic cron: either matches
		return dom || dow
	}
}

// MinGap returns the shortest interval between two consecutive firings,
// sampled over two weeks.
func (c *Cron) MinGap() time.Duration {
	t := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	end := t.AddDate(0, 0, 14)
	prev := c.Next(t)
	min := time.Duration(1<<63 - 1)
	for i := 0; i < 5000 && !prev.IsZero() && prev.Before(end); i++ {
		n := c.Next(prev)
		if n.IsZero() {
			break
		}
		if g := n.Sub(prev); g < min {
			min = g
		}
		prev = n
	}
	return min
}
