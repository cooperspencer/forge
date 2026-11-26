package ci

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression:
//
//	minute hour day-of-month month day-of-week
//
// Fields accept *, lists (1,15), ranges (1-5), steps (*/15, 0-30/10) and
// month/weekday names (jan, mon). Sunday is 0 or 7. As in classic cron, when
// both day-of-month and day-of-week are restricted, either may match.
// Shortcuts: @hourly, @daily (@midnight), @weekly, @monthly, @yearly (@annually).
type Cron struct {
	min, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar           bool
}

var cronShortcuts = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func ParseCron(expr string) (*Cron, error) {
	s := strings.ToLower(strings.TrimSpace(expr))
	if v, ok := cronShortcuts[s]; ok {
		s = v
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day month weekday)", expr)
	}
	var c Cron
	var err error
	if c.min, err = cronField(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	if c.hour, err = cronField(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	if c.dom, err = cronField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q: day of month: %w", expr, err)
	}
	if c.month, err = cronField(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	if c.dow, err = cronField(f[4], 0, 7, dowNames); err != nil {
		return nil, fmt.Errorf("cron %q: day of week: %w", expr, err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*", f[4] == "*"
	return &c, nil
}

func cronField(s string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, step := part, 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			rng, step = part[:i], n
		}
		a, b := lo, hi
		if rng != "*" {
			var err error
			if i := strings.IndexByte(rng, '-'); i > 0 {
				if a, err = cronValue(rng[:i], names); err == nil {
					b, err = cronValue(rng[i+1:], names)
				}
			} else {
				a, err = cronValue(rng, names)
				b = a
				if step > 1 { // "5/15" means 5-max/15
					b = hi
				}
			}
			if err != nil {
				return 0, err
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("%q out of range %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func cronValue(s string, names map[string]int) (int, error) {
	if v, ok := names[s]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return n, nil
}

// Matches reports whether t (to the minute, in t's location) is a firing time.
func (c *Cron) Matches(t time.Time) bool {
	if c.min&(1<<t.Minute()) == 0 || c.hour&(1<<t.Hour()) == 0 || c.month&(1<<int(t.Month())) == 0 {
		return false
	}
	dom := c.dom&(1<<t.Day()) != 0
	dow := c.dow&(1<<int(t.Weekday())) != 0
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow
}
