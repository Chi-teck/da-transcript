package app

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	loc := time.FixedZone("+05", 5*3600)
	now := time.Date(2026, 8, 18, 15, 4, 5, 0, loc)
	ms := func(t time.Time) int64 { return t.UnixMilli() }

	cases := []struct {
		spec string
		end  bool
		want int64
	}{
		{"2h", false, ms(now.Add(-2 * time.Hour))},
		{"30m", false, ms(now.Add(-30 * time.Minute))},
		{"1.5h", false, ms(now.Add(-90 * time.Minute))},
		{"3d", false, ms(now.Add(-72 * time.Hour))},
		{"1w", false, ms(now.Add(-7 * 24 * time.Hour))},
		{"45s", false, ms(now.Add(-45 * time.Second))},
		{"2H", false, ms(now.Add(-2 * time.Hour))},
		{" 2h ", false, ms(now.Add(-2 * time.Hour))},
		// The furthest back a relative time reaches, just inside a Duration.
		{"106751d", false, ms(now.AddDate(0, 0, -106751))},
		{"2562047h", false, ms(now.Add(-2562047 * time.Hour))},
		{"today", false, ms(time.Date(2026, 8, 18, 0, 0, 0, 0, loc))},
		{"today", true, ms(time.Date(2026, 8, 19, 0, 0, 0, 0, loc))},
		{"yesterday", false, ms(time.Date(2026, 8, 17, 0, 0, 0, 0, loc))},
		{"yesterday", true, ms(time.Date(2026, 8, 18, 0, 0, 0, 0, loc))},
		{"2026-08-18", false, ms(time.Date(2026, 8, 18, 0, 0, 0, 0, loc))},
		{"20260818", false, ms(time.Date(2026, 8, 18, 0, 0, 0, 0, loc))},
		// A bare date as --until means the end of that day, in either form; a
		// time of day, however short, is taken as given.
		{"2026-08-18", true, ms(time.Date(2026, 8, 19, 0, 0, 0, 0, loc))},
		{"20260818", true, ms(time.Date(2026, 8, 19, 0, 0, 0, 0, loc))},
		{"20260818T12", true, ms(time.Date(2026, 8, 18, 12, 0, 0, 0, loc))},
		{"2026-08-18T12:30", false, ms(time.Date(2026, 8, 18, 12, 30, 0, 0, loc))},
		{"2026-08-18 12:30:05", false, ms(time.Date(2026, 8, 18, 12, 30, 5, 0, loc))},
		{"2026-08-18T12:30:05.25", false, ms(time.Date(2026, 8, 18, 12, 30, 5, 250e6, loc))},
		{"2026-08-18T12:30:05.123456789", false, ms(time.Date(2026, 8, 18, 12, 30, 5, 123456000, loc))},
		{"2026-08-18T12:30:05+02:00", false, ms(time.Date(2026, 8, 18, 12, 30, 5, 0, time.FixedZone("", 2*3600)))},
		{"2026-08-18T12:30:05Z", false, ms(time.Date(2026, 8, 18, 12, 30, 5, 0, time.UTC))},
		{"2026-08-18T12:30:05-0330", false, ms(time.Date(2026, 8, 18, 12, 30, 5, 0, time.FixedZone("", -(3*3600+30*60))))},
		{"2026-08-18T1230", false, ms(time.Date(2026, 8, 18, 12, 30, 0, 0, loc))},
	}
	for _, c := range cases {
		got, err := parseWhen(c.spec, c.end, now)
		if err != nil {
			t.Errorf("parseWhen(%q, end=%v): %v", c.spec, c.end, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseWhen(%q, end=%v) = %d (%s), want %d (%s)",
				c.spec, c.end, got, time.UnixMilli(got).In(loc), c.want, time.UnixMilli(c.want).In(loc))
		}
	}
}

// Across a DST change, s/m/h stay elapsed time and d/w stay calendar days —
// and no relative spec ever lands after now.
func TestParseWhenAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(mo time.Month, d, h, mi int) time.Time { return time.Date(2026, mo, d, h, mi, 0, 0, time.UTC) }
	// 03:10 CEST, an hour after the clocks sprang from 02:00 to 03:00.
	spring := utc(3, 29, 1, 10).In(loc)
	// 02:30 CET, the second pass through 02:00–03:00 after falling back.
	fall := utc(10, 25, 1, 30).In(loc)

	for _, c := range []struct {
		now  time.Time
		spec string
		want time.Time
	}{
		{spring, "30m", spring.Add(-30 * time.Minute)}, // not 03:40, in the future
		{spring, "2h", spring.Add(-2 * time.Hour)},     // not 01:10 CET, one hour back
		{spring, "24h", time.Date(2026, 3, 28, 2, 10, 0, 0, loc)},
		{spring, "1d", time.Date(2026, 3, 28, 3, 10, 0, 0, loc)},
		{spring, "1.5d", time.Date(2026, 3, 27, 15, 10, 0, 0, loc)},
		{spring, "0.01d", spring.Add(-864 * time.Second)},
		{spring, "1w", time.Date(2026, 3, 22, 3, 10, 0, 0, loc)},
		{fall, "45s", fall.Add(-45 * time.Second)},
		{fall, "1h", utc(10, 25, 0, 30)}, // 02:30 CEST, not 01:30 CEST two hours back
		{fall, "24h", time.Date(2026, 10, 24, 3, 30, 0, 0, loc)},
		{fall, "1d", time.Date(2026, 10, 24, 2, 30, 0, 0, loc)},
		{fall, "1w", time.Date(2026, 10, 18, 2, 30, 0, 0, loc)},
	} {
		got, err := parseWhen(c.spec, false, c.now)
		if err != nil {
			t.Errorf("parseWhen(%q) at %s: %v", c.spec, c.now, err)
			continue
		}
		if got != c.want.UnixMilli() {
			t.Errorf("parseWhen(%q) at %s = %s, want %s",
				c.spec, c.now, time.UnixMilli(got).In(loc), c.want.In(loc))
		}
	}
}

func TestParseWhenErrors(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 4, 5, 0, time.UTC)
	for _, spec := range []string{
		"nope", "2026-13-01", "2026-02-30", "2026-0818", "5x", "2026-08-18T25:00",
		"2026-08-18T12:61", "", "h", "2026-08-18X12:30", "-5h", "NaNh", "Infd", "1e400h",
	} {
		_, err := parseWhen(spec, false, now)
		if err == nil {
			t.Errorf("parseWhen(%q) unexpectedly parsed", spec)
			continue
		}
		want := "cannot parse time " + strconv.Quote(spec) + " — use 2h, 30m, 3d, today, yesterday or an ISO timestamp"
		if err.Error() != want {
			t.Errorf("parseWhen(%q) error = %q, want %q", spec, err.Error(), want)
		}
	}
}

// A relative time past what a Duration holds is refused, not wrapped round
// into the future or back to now.
func TestParseWhenOutOfRange(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 4, 5, 0, time.UTC)
	for _, spec := range []string{
		"106752d", "110000d", "15251w", "2562048h", "153722868m", "9223372037s",
		"99999999999999999999h",
		strings.Repeat("9", 400) + "d", // too long for a float: ParseFloat says Inf
	} {
		_, err := parseWhen(spec, false, now)
		if err == nil {
			t.Errorf("parseWhen(%q) unexpectedly parsed", spec)
			continue
		}
		want := "cannot parse time " + strconv.Quote(spec) + " — a relative time reaches back 292 years at most"
		if err.Error() != want {
			t.Errorf("parseWhen(%q) error = %q, want %q", spec, err.Error(), want)
		}
	}
}

// spanText picks one unit and never spends more than four cells on it — the
// column is budgeted for a magnitude, not for a duration.
func TestSpanText(t *testing.T) {
	sec := func(f float64) int64 { return int64(f * 1000) }
	for _, c := range []struct {
		ms   int64
		want string
	}{
		{-1000, "0s"},
		{0, "0s"},
		{sec(3), "3s"},
		{sec(59.9), "59s"},
		{sec(60), "1m"},
		{sec(110), "2m"},
		{sec(445), "7m"},
		{sec(59.4 * 60), "59m"},
		{sec(59.9 * 60), "1.0h"}, // never "60m": the unit hands over at half a unit
		{sec(3600), "1.0h"},
		{sec(6707), "1.9h"},
		{sec(9.96 * 3600), "10h"}, // 9.95 is the cut-off: "10.0h" would be five cells
		{sec(23.4 * 3600), "23h"},
		{sec(23.99 * 3600), "1.0d"}, // never "24h", for the same reason
		{sec(86400), "1.0d"},
		{sec(160037), "1.9d"},
		{sec(9358645), "108d"},
	} {
		got := spanText(c.ms)
		if got != c.want {
			t.Errorf("spanText(%d) = %q, want %q", c.ms, got, c.want)
		}
		if cells(got) > 4 {
			t.Errorf("spanText(%d) = %q, %d cells", c.ms, got, cells(got))
		}
	}
}
