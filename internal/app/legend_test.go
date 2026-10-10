package app

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// The legend's `where` column is relative to --root when a session sits under
// it, and stays absolute when it does not.
func TestLegendWhereRelativeToRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	d := newTestDB(t)
	d.session("a", "", "At the root", root, "2026-08-17T10:00:00Z")
	d.session("b", "", "Below it", sub, "2026-08-17T10:00:01Z")
	d.userText("a", "2026-08-17T10:00:00Z", "hello")
	d.userText("b", "2026-08-17T10:00:01Z", "hello")
	t.Setenv("COLUMNS", "")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--db", d.path, "--all", "--root", root, "--list"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	// Read the `where` cell out of each row rather than matching the padding
	// around it, so the assertion is about the path and not the furniture. The
	// top rule, the heads and their rule come first; the foot rule and the
	// footer last.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines[3 : len(lines)-2] {
		cols := strings.Split(strings.Trim(line, "│"), "│")
		if len(cols) != 7 {
			t.Fatalf("row %q has %d columns, want 7", line, len(cols))
		}
		if got := strings.TrimSpace(cols[1]); got != "." && got != "sub" {
			t.Errorf("where column is %q, want it relative to the root, in:\n%s", got, out)
		}
	}
	if strings.Contains(out, sub) {
		t.Errorf("path under the root stayed absolute:\n%s", out)
	}
}

// The --list table ends on a foot rule and a line counting what is above it,
// back to the earliest row's date. The transcript's key is boxed the same way
// but gets no foot line: the summary over it already says what that would.
func TestLegendFooter(t *testing.T) {
	d := zooDB(t)
	t.Setenv("COLUMNS", "")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--db", d.path, "--everywhere", "--all", "--list"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if foot := lines[len(lines)-2]; !strings.HasPrefix(foot, "└") || !strings.HasSuffix(foot, "┘") {
		t.Errorf("foot rule = %q, want the box closed off", foot)
	}
	// Indented to where the first column's text starts, not to the margin.
	if got, want := lines[len(lines)-1], "  4 sessions since 2026-08-17"; got != want {
		t.Errorf("footer = %q, want %q", got, want)
	}

	stdout.Reset()
	if err := run([]string{"--db", d.path, "--everywhere", "--all"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "sessions since") {
		t.Error("the transcript's key grew a footer")
	}
	// The summary, a blank line, then the box.
	if key := strings.Split(stdout.String(), "\n")[2]; !strings.HasPrefix(key, "┌") {
		t.Errorf("key opens on %q, want the box", key)
	}
}

// tokenText keeps the magnitude and drops the digits nobody reads, in four
// cells or fewer.
func TestTokenText(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{1020, "1.0k"},
		{8242, "8.2k"},
		{9949, "9.9k"},
		{9950, "10k"}, // 9.95 is the cut-off: "10.0k" would be five cells
		{23499, "23k"},
		{104600, "105k"},
		{589674, "590k"},
		{999499, "999k"},
		{999600, "1.0M"},
		{1765712, "1.8M"},
		{3360202, "3.4M"},
		{9949999, "9.9M"},
		{9950000, "10M"},
	} {
		got := tokenText(c.n)
		if got != c.want {
			t.Errorf("tokenText(%d) = %q, want %q", c.n, got, c.want)
		}
		if cells(got) > 4 {
			t.Errorf("tokenText(%d) = %q, %d cells", c.n, got, cells(got))
		}
	}
}

// A session the store never accounted for gets a blank cell, not a fabricated
// zero: the two are different facts, and both are in the fixture.
func TestLegendBlankAndZero(t *testing.T) {
	unknown := &session{}
	if got := spanOf(unknown); got != "" {
		t.Errorf("span of an untouched session = %q, want blank", got)
	}
	if got := tokensOf(unknown); got != "" {
		t.Errorf("tokens of an unaccounted session = %q, want blank", got)
	}
	spent := &session{}
	spent.timeCreated, spent.timeUpdated, spent.hasTokens = 1000, 1000, true
	if got := spanOf(spent); got != "0s" {
		t.Errorf("span of a session touched at once = %q, want %q", got, "0s")
	}
	if got := tokensOf(spent); got != "0" {
		t.Errorf("tokens of a session that spent nothing = %q, want %q", got, "0")
	}
}

// The token total is the four figures of every item that reported usage —
// model steps, compactions, evaluator calls — and nothing else: not the
// reasoning inside the output, not a usage on an item this version cannot
// read, and not a subagent's, which stays on its own row. The span runs to the
// latest item.
func TestTally(t *testing.T) {
	d := newTestDB(t)
	d.session("a", "", "", "/p", "2026-10-08T09:00:00+05:00")
	d.session("zero", "", "", "/p", "2026-10-08T09:00:01+05:00")
	d.session("none", "", "", "/p", "2026-10-08T09:00:02+05:00")
	d.session("kid", "a", "", "/p", "2026-10-08T09:02:30+05:00")

	d.item("a", "message", `{"role":"system","content":"task"}`)
	d.userText("a", "2026-10-08T09:01:00+05:00", "go")
	d.item("a", "message", `{"role":"assistant","finish_reason":"tool_calls","created_at":"2026-10-08T09:02:00+05:00",
	  "usage":{"input_tokens":100,"output_tokens":20,"cached_input_tokens":1000,"cached_write_tokens":5}}`)
	d.item("a", "message", `{"role":"tool","tool_call_id":"c1","content":"ok","created_at":"2026-10-08T09:03:00+05:00"}`)
	d.item("a", "message", `{"role":"assistant","finish_reason":"stop","created_at":"2026-10-08T09:04:00+05:00",
	  "usage":{"input_tokens":50,"output_tokens":10,"cached_input_tokens":0,"cached_write_tokens":0,"reasoning_tokens":7}}`)
	d.item("a", "message", `{"role":"assistant","content":"cut off"}`)
	d.summary("a", `{"input_tokens":400,"output_tokens":100,"cached_input_tokens":0,"cached_write_tokens":0}`)
	d.item("a", "evaluation", `{"id":"e1","evaluator":"judge","created_at":"2026-10-08T09:05:00.5+05:00",
	  "usage":{"input_tokens":30,"output_tokens":3}}`)
	d.item("a", "mystery", `{"usage":{"input_tokens":9999}}`)
	d.subsession("a", "kid")
	d.item("kid", "message", `{"role":"assistant","content":"ok","finish_reason":"stop",
	  "created_at":"2026-10-08T09:02:40+05:00","usage":{"input_tokens":7,"output_tokens":2}}`)

	d.item("zero", "message", `{"role":"assistant","content":"ok","finish_reason":"stop",
	  "created_at":"2026-10-08T09:00:01+05:00",
	  "usage":{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0,"cached_write_tokens":0}}`)
	d.userText("none", "2026-10-08T09:00:02+05:00", "hi")

	tr, err := build(context.Background(), d.open(t), &options{all: true, stderr: io.Discard, width: 100}, nil, seam{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*session{}
	for _, s := range tr.used {
		by[s.id] = s
	}
	a := by["a"]
	if want := int64(100 + 20 + 1000 + 5 + 50 + 10 + 400 + 100 + 30 + 3); !a.hasTokens || a.tokens != want {
		t.Errorf("a: tokens = %d (has %v), want %d", a.tokens, a.hasTokens, want)
	}
	if want := ms(t, "2026-10-08T09:05:00.5+05:00"); a.timeUpdated != want {
		t.Errorf("a: timeUpdated = %d, want the latest item's %d", a.timeUpdated, want)
	}
	if k := by["kid"]; !k.hasTokens || k.tokens != 7+2 {
		t.Errorf("kid: tokens = %d (has %v), want its own %d", k.tokens, k.hasTokens, 7+2)
	}
	if z := by["zero"]; !z.hasTokens || z.tokens != 0 || tokensOf(z) != "0" {
		t.Errorf("zero: tokens = %d (has %v), want a reported 0", z.tokens, z.hasTokens)
	}
	if n := by["none"]; n.hasTokens || tokensOf(n) != "" {
		t.Errorf("none: tokens = %d (has %v), want nothing reported", n.tokens, n.hasTokens)
	}
}

// A session is listed when a block of its is drawn in the window, an item with
// no time of its own counting at the time of the one before it. Its figures
// describe the whole session whatever the window.
func TestListWindow(t *testing.T) {
	d := newTestDB(t)
	d.session("aaaaaa", "", "Before", "/w", "2026-09-01T09:00:00Z")
	d.session("bbbbbb", "", "Across", "/w", "2026-09-01T10:00:00Z")
	// A's last row is a compaction with no time of its own: it carries A's
	// last time, not "now", so A stays out of a window that starts later.
	d.userText("aaaaaa", "2026-09-01T09:01:00Z", "early")
	d.summary("aaaaaa", `{"input_tokens":1,"output_tokens":1}`)
	d.userText("bbbbbb", "2026-09-01T10:01:00Z", "early")
	d.item("bbbbbb", "message", `{"role":"assistant","finish_reason":"stop","created_at":"2026-09-02T10:00:00Z",
	  "usage":{"input_tokens":1500,"output_tokens":300}}`)
	d.userText("bbbbbb", "2026-09-03T10:00:00Z", "late")
	t.Setenv("COLUMNS", "")
	list := func(args ...string) (rows map[string][]string, stderr string) {
		t.Helper()
		var so, se bytes.Buffer
		if err := run(append([]string{"--db", d.path, "--everywhere", "--list"}, args...), &so, &se); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
		rows = map[string][]string{}
		lines := strings.Split(strings.TrimSpace(so.String()), "\n")
		if len(lines) < 5 {
			return rows, se.String()
		}
		for _, line := range lines[3 : len(lines)-2] {
			cols := strings.Split(strings.Trim(line, "│"), "│")
			for i := range cols {
				cols[i] = strings.TrimSpace(cols[i])
			}
			rows[cols[0]] = cols
		}
		return rows, se.String()
	}

	since, _ := list("--since", "2026-09-02T00:00:00Z")
	if len(since) != 1 || since["bbbbbb"] == nil {
		t.Fatalf("--since lists %v, want only bbbbbb", since)
	}
	all, _ := list("--all")
	if len(all) != 2 || all["aaaaaa"] == nil || all["bbbbbb"] == nil {
		t.Fatalf("--all lists %v, want both", all)
	}
	// Span, Messages and Tokens: the same under either window.
	for _, i := range []int{3, 4, 5} {
		if since["bbbbbb"][i] != all["bbbbbb"][i] {
			t.Errorf("column %d = %q under --since, %q under --all", i, since["bbbbbb"][i], all["bbbbbb"][i])
		}
	}
	if got := strings.Join(all["bbbbbb"][3:6], " "); got != "2.0d 3 1.8k" {
		t.Errorf("bbbbbb figures = %q, want %q", got, "2.0d 3 1.8k")
	}

	// The window is half-open, as oc's: an item on --since is in it, and one
	// on --until is not.
	if edge, _ := list("--since", "2026-09-03T10:00:00Z"); len(edge) != 1 || edge["bbbbbb"] == nil {
		t.Errorf("--since on bbbbbb's last item lists %v, want only bbbbbb", edge)
	}
	if edge, _ := list("--since", "2026-09-01T00:00:00Z", "--until", "2026-09-01T09:01:00Z"); len(edge) != 0 {
		t.Errorf("--until on aaaaaa's only timed item lists %v, want nothing", edge)
	}

	none, stderr := list("--since", "2026-01-01", "--until", "2026-08-01")
	if len(none) != 0 || stderr != "Nothing from 2 session(s) in this window — try --all.\n" {
		t.Errorf("--until before everything lists %v, stderr %q", none, stderr)
	}
}
