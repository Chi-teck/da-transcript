package app

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var relativeSpec = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*([smhdw])$`)

// parseWhen turns --since/--until into epoch milliseconds: relative offsets,
// today/yesterday, or an ISO timestamp, all in the zone `now` carries. `end` marks an --until value, where a bare date means
// the end of that day rather than its midnight.
//
// Seconds, minutes and hours are elapsed time: `30m` is half an hour even
// across a DST change, where `24h` ago is an hour off yesterday's clock. Days
// and weeks are calendar arithmetic: `1d` ago is the same clock reading
// yesterday, and a fraction of a day left over is elapsed time on top.
func parseWhen(spec string, end bool, now time.Time) (int64, error) {
	spec = strings.TrimSpace(spec)
	loc := now.Location()

	if m := relativeSpec.FindStringSubmatch(spec); m != nil {
		n, _ := strconv.ParseFloat(m[1], 64)
		unit := map[string]time.Duration{
			"s": time.Second, "m": time.Minute, "h": time.Hour,
			"d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
		}[strings.ToLower(m[2])]
		// Past 292 years — or Inf, from a digit run too long for a float — the
		// offset is no Duration at all, and the conversion would wrap. The test
		// is negated because NaN fails every comparison.
		ns := n * float64(unit)
		if !(ns < math.MaxInt64) {
			return 0, fmt.Errorf("cannot parse time %s — a relative time reaches back 292 years at most", strconv.Quote(spec))
		}
		// Microsecond precision, like timedelta(seconds=n).
		delta := time.Duration(ns/float64(time.Microsecond)) * time.Microsecond
		day := 24 * time.Hour
		if unit < day {
			return now.Add(-delta).UnixMilli(), nil
		}
		return now.AddDate(0, 0, -int(delta/day)).Add(-(delta % day)).UnixMilli(), nil
	}

	y, mo, d := now.Date()
	switch spec {
	case "today":
		if end {
			return time.Date(y, mo, d+1, 0, 0, 0, 0, loc).UnixMilli(), nil
		}
		return time.Date(y, mo, d, 0, 0, 0, 0, loc).UnixMilli(), nil
	case "yesterday":
		if end {
			return time.Date(y, mo, d, 0, 0, 0, 0, loc).UnixMilli(), nil
		}
		return time.Date(y, mo, d-1, 0, 0, 0, 0, loc).UnixMilli(), nil
	}

	t, dateOnly, ok := parseISO(spec, loc)
	if !ok {
		return 0, fmt.Errorf("cannot parse time %s — use 2h, 30m, 3d, today, yesterday or an ISO timestamp", strconv.Quote(spec))
	}
	// A bare date as --until means the end of that day, not its midnight.
	if end && dateOnly {
		t = t.AddDate(0, 0, 1)
	}
	return t.UnixMilli(), nil
}

// parseISO accepts what datetime.fromisoformat does in practice: a date, with
// or without a time separated by T or a space, optional seconds, optional
// fraction, optional offset (Z, ±HH:MM, ±HHMM, ±HH). A value without an offset
// is local time. dateOnly reports that no time of day was given.
func parseISO(spec string, loc *time.Location) (t time.Time, dateOnly, ok bool) {
	m := isoSpec.FindStringSubmatch(spec)
	if m == nil {
		return time.Time{}, false, false
	}
	y, mo, d := atoi(m[1]), atoi(m[2]), atoi(m[3])
	if m[4] != "" {
		y, mo, d = atoi(m[4]), atoi(m[5]), atoi(m[6])
	}
	h, mi, s := atoi(m[7]), atoi(m[8]), atoi(m[9])
	if mo < 1 || mo > 12 || d < 1 || h > 23 || mi > 59 || s > 59 {
		return time.Time{}, false, false
	}
	ns := 0
	if m[10] != "" {
		// Fractions run to microseconds at most, as fromisoformat keeps them.
		ns = atoi((m[10] + "000000")[:6]) * 1000
	}
	zone := loc
	switch off := m[11]; off {
	case "":
	case "Z", "z":
		zone = time.UTC
	default:
		sign := 1
		if off[0] == '-' {
			sign = -1
		}
		digits := strings.ReplaceAll(off[1:], ":", "")
		oh, om := atoi(digits[:2]), 0
		if len(digits) == 4 {
			om = atoi(digits[2:])
		}
		if oh > 23 || om > 59 {
			return time.Time{}, false, false
		}
		zone = time.FixedZone("", sign*(oh*3600+om*60))
	}
	t = time.Date(y, time.Month(mo), d, h, mi, s, ns, zone)
	return t, m[7] == "", t.Day() == d // a normalised day means the date was not real
}

// A date in extended or basic form (never mixed), an optional time (T or
// space separated), an optional fraction, an optional offset.
var isoSpec = regexp.MustCompile(
	`^(?:(\d{4})-(\d{2})-(\d{2})|(\d{4})(\d{2})(\d{2}))` +
		`(?:[Tt ](\d{2})(?::?(\d{2})(?::?(\d{2})(?:[.,](\d+))?)?)?` +
		`(Z|z|[+-]\d{2}(?::?\d{2})?)?)?$`)

// atoi reads a run of digits already validated by the pattern; "" is 0.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// stamp formats a millisecond timestamp as a full local date-time. It heads
// every message, so a line lifted out of the transcript still says when it was.
func stamp(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

// shortStamp is the session table's timestamp: stamp() with the seconds
// dropped. The date is spelled out in full — a session table is read months
// after the fact as often as minutes, and a bare 01-02 leaves the reader
// guessing at the year. Space rather than T between the halves, to match the
// stamp heading every message.
func shortStamp(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// dayStamp is the date on its own, for the places a clock time would be noise:
// the span a table covers is a matter of days, and the rows themselves already
// say what happened at what minute.
func dayStamp(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02")
}

// spanText is how long something ran, in one unit and four cells: a table
// column has room for a magnitude, not for a duration spelled out, and the
// rows already carry the minute each session started at.
//
// The decimal survives only where the leading digit alone would throw the
// magnitude away — 1.9h says something 1h does not, while 23h and 108d say it
// on their own. Its cut-offs are 9.95 and not 10 because "%.1f" of 9.96 is
// "10.0", five cells where every other reading is four.
//
// A unit hands over half a unit before it runs out, not at the boundary: the
// rounding is what decides the reading, so 59.7 minutes belongs to the hour it
// prints as and not to the minutes it came from. Rounding first and choosing
// the unit after is how a duration column comes to say "60m" or "24h", both of
// which name a unit the column has and did not use.
func spanText(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d.Minutes() < 59.5:
		return fmt.Sprintf("%.0fm", d.Minutes())
	case d.Hours() < 9.95:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d.Hours() < 23.5:
		return fmt.Sprintf("%.0fh", d.Hours())
	case d.Hours()/24 < 9.95:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	default:
		return fmt.Sprintf("%.0fd", d.Hours()/24)
	}
}
