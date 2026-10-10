package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// clock is the store's stamp for sec seconds past noon on a fixed day.
func clock(sec int) string {
	return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
}

// lastID is the id of the row the fixture wrote last.
func (d *testDB) lastID() int64 {
	d.t.Helper()
	var id int64
	if err := d.db.QueryRow("select max(id) from session_items").Scan(&id); err != nil {
		d.t.Fatal(err)
	}
	return id
}

// callJSON is one tool_calls entry; its arguments are the JSON string the
// store keeps them as.
func callJSON(id, name, args string) string {
	return `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + jsonStr(args) + `}}`
}

// userJSON is a prompt's message_json.
func userJSON(at, text string) string {
	return `{"role":"user","content":` + jsonStr(text) + `,"created_at":"` + at + `"}`
}

// stepJSON is a step as docker-agent writes it once the step ends: its text
// and calls, with created_at and the finish reason.
func stepJSON(at, text string, calls ...string) string {
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	return `{"role":"assistant","content":` + jsonStr(text) + `,"created_at":"` + at + `","model":"zai/glm-5.3",` +
		`"finish_reason":"` + finish + `","tool_calls":[` + strings.Join(calls, ",") + `]}`
}

// streamingJSON is a step as docker-agent leaves it while it streams: the text
// so far, with neither created_at nor finish_reason.
func streamingJSON(text string) string {
	return `{"role":"assistant","content":` + jsonStr(text) + `}`
}

// step appends a step that has ended.
func (d *testDB) step(sid, at, text string, calls ...string) {
	d.t.Helper()
	d.message(sid, "root", stepJSON(at, text, calls...))
}

// streaming appends a step at its first chunk, the row docker-agent then
// rewrites in place until the step ends, and returns the row's id.
func (d *testDB) streaming(sid, text string) int64 {
	d.t.Helper()
	d.message(sid, "root", streamingJSON(text))
	return d.lastID()
}

// rewrite replaces a row's message_json in place, as docker-agent does while a
// step streams and when it ends.
func (d *testDB) rewrite(id int64, data string) {
	d.t.Helper()
	if _, err := d.db.Exec("update session_items set message_json = ? where id = ?", data, id); err != nil {
		d.t.Fatal(err)
	}
}

// toolResult appends the result of a call.
func (d *testDB) toolResult(sid, call, at, content string) {
	d.t.Helper()
	d.message(sid, "root", `{"role":"tool","tool_call_id":"`+call+`","content":`+jsonStr(content)+`,"created_at":"`+at+`"}`)
}

// whole writes a session the way docker-agent writes a subagent when it ends,
// or a fork: its sessions row and all its items in one transaction, and for a
// subagent its parent's subsession row with them. items are message_json
// values; a parent of "" makes a top-level session.
func (d *testDB) whole(parent, id, created string, items ...string) {
	d.t.Helper()
	tx, err := d.db.Begin()
	if err != nil {
		d.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		d.t.Helper()
		if _, err := tx.Exec(query, args...); err != nil {
			d.t.Fatal(err)
		}
	}
	var p any
	if parent != "" {
		p = parent
	}
	exec("insert into sessions (id, parent_id, title, working_dir, created_at) values (?,?,?,?,?)",
		id, p, "", "/p", created)
	for i, m := range items {
		exec("insert into session_items (session_id, position, item_type, agent_name, message_json) values (?,?,?,?,?)",
			id, i, "message", "helper", m)
	}
	if parent != "" {
		exec(`insert into session_items (session_id, position, item_type, subsession_id)
		   values (?, (select coalesce(max(position), -1) + 1 from session_items where session_id = ?), 'subsession', ?)`,
			parent, parent, id)
	}
	if err := tx.Commit(); err != nil {
		d.t.Fatal(err)
	}
}

// roundOpts parses the flags a build() test needs, with warnings collected
// where a failure can print them.
func roundOpts(t *testing.T, d *testDB, extra ...string) *options {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts, err := parseArgs(append([]string{"--db", d.path, "--everywhere", "--all"}, extra...),
		&stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	opts.stderr = &stderr
	return opts
}

// tailer runs build() the way follow does — the opening transcript at the
// first call, a round at each one after — and keeps every line in one
// stream, as the reader sees it.
type tailer struct {
	t      *testing.T
	st     *store
	opts   *options
	cur    *cursor
	carry  seam
	stream []string
}

func (f *tailer) round() *transcript {
	f.t.Helper()
	got, err := build(context.Background(), f.st, f.opts, f.cur, f.carry)
	if err != nil {
		f.t.Fatal(err)
	}
	if got.cur != nil {
		f.cur = got.cur
	}
	f.carry = got.seam
	f.stream = append(f.stream, got.lines...)
	return got
}

// onePass is the transcript a run without --follow prints over the fixture
// as it stands.
func onePass(t *testing.T, d *testDB, opts *options) []string {
	t.Helper()
	got, err := build(context.Background(), d.open(t), opts, nil, seam{})
	if err != nil {
		t.Fatal(err)
	}
	return got.lines
}

// headLine is a turn's head: what a block is drawn under.
var headLine = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} • `)

// heads is the turn heads among lines, sorted, so two streams drawing the same
// blocks in different orders have the same heads.
func heads(lines []string) []string {
	var out []string
	for _, line := range lines {
		if headLine.MatchString(line) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	slices.Sort(out)
	return out
}

// TestRoundSeamSpacing pins the spacing across a --follow poll boundary to what
// one pass would have produced: two blank rows between turns, and two before a
// session banner. A round cannot take back rows already written, so each ends
// on the single row every seam has in common and the next opens with whatever
// more that seam needed.
func TestRoundSeamSpacing(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.session("ses_b", "", "B", "/p", clock(0))
	d.userText("ses_a", clock(0), "one")
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d)}

	// Three rounds, each seeing exactly one item: the opening pass, then a
	// turn in the same session, then a turn in another one.
	f.round()
	d.userText("ses_a", clock(1), "two")
	f.round()
	d.userText("ses_b", clock(2), "three")
	f.round()
	// Back to the first session a round later. Which flag its banner takes is
	// the other thing the seam carries: nothing in this round has seen ses_a
	// before, and the reader has.
	d.userText("ses_a", clock(3), "four")
	f.round()
	stream := f.stream

	gap := func(needle string) int {
		t.Helper()
		for i, line := range stream {
			if !strings.Contains(line, needle) {
				continue
			}
			n := 0
			for j := i - 1; j >= 0 && stream[j] == ""; j-- {
				n++
			}
			return n
		}
		t.Fatalf("%q not in stream:\n%s", needle, strings.Join(stream, "\n"))
		return 0
	}
	if got := gap("12:00:01 • user"); got != 2 {
		t.Errorf("blank rows before a turn across the seam = %d, want 2", got)
	}
	if got := gap(tagOpen + "ses_b" + tagClose + " B"); got != 2 {
		t.Errorf("blank rows before a session banner across the seam = %d, want 2", got)
	}
	banner := func(flag, id string) bool {
		t.Helper()
		for _, line := range stream {
			if strings.Contains(line, flag+" "+tagOpen+id+tagClose) {
				return true
			}
		}
		return false
	}
	if !banner(newFlag, "ses_b") {
		t.Errorf("first banner for ses_b is not flagged as a beginning:\n%s", strings.Join(stream, "\n"))
	}
	if !banner(backFlag, "ses_a") {
		t.Errorf("ses_a is not flagged as a return across the seam:\n%s", strings.Join(stream, "\n"))
	}
	if got := stream[len(stream)-1]; got != "" {
		t.Errorf("stream ends on %q, want a blank row", got)
	}
	if got := stream[len(stream)-2]; got == "" {
		t.Error("stream ends on two blank rows, want one")
	}
}

// TestFollowExactlyOnce guards --follow against duplicates and losses: a
// writer adds rows while the tail polls — prompts in two sessions sharing
// seconds, a step rewritten in place as it streams, a step whose result lands
// later, a subagent landing whole — and every one must be emitted exactly
// once, and none half-written. The lanes are what make that hold.
func TestFollowExactlyOnce(t *testing.T) {
	t.Setenv("COLUMNS", "")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.session("ses_b", "", "B", "/p", clock(0))
	// Two rows exist before the tail starts; the opening transcript takes them.
	d.userText("ses_a", clock(0), "pre-0")
	d.userText("ses_b", clock(0), "pre-1")

	var stdout, stderr bytes.Buffer
	opts, err := parseArgs([]string{"--db", d.path, "--everywhere", "--all", "--follow", "--interval", "0.02"},
		&stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	opts.stderr = &stderr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := d.open(t)

	opening, err := build(ctx, st, opts, nil, seam{})
	if err != nil {
		t.Fatal(err)
	}
	if opening.cur == nil || opening.cur.lastID != d.lastID() {
		t.Fatalf("opening cursor = %+v", opening.cur)
	}

	w := bufio.NewWriter(&stdout)
	followErr := make(chan error, 1)
	go func() { followErr <- follow(ctx, st, opts, w, opening.cur, opening.seam) }()

	// The writer runs on the test's own goroutine, so a fixture helper may
	// fail the test. It paces itself against the poller, so rows land in
	// bursts that straddle poll boundaries, four prompts to a second.
	n := 0
	pause := func() { time.Sleep(3 * time.Millisecond) }
	now := func() string { return clock(1 + n/4) }
	prompt := func(sid string) {
		d.userText(sid, now(), fmt.Sprintf("row-%03d", n))
		n++
		pause()
	}
	for range 5 {
		prompt("ses_a")
		prompt("ses_b")
	}
	// A step streaming in b, rewritten twice and then ended, while a goes on.
	live := d.streaming("ses_b", "partial-one")
	pause()
	prompt("ses_a")
	d.rewrite(live, streamingJSON("partial-one partial-two"))
	pause()
	prompt("ses_a")
	d.rewrite(live, stepJSON(now(), "final-answer"))
	pause()
	// A step in a whose result lands a few rows later, while b goes on.
	d.step("ses_a", now(), "calling", callJSON("call_x", "shell", `{"cmd":"date"}`))
	pause()
	prompt("ses_b")
	prompt("ses_b")
	d.toolResult("ses_a", "call_x", now(), "Tue")
	pause()
	// A background subagent of a's, landing whole with its marker, its run
	// stamped back to when it started.
	d.whole("ses_a", "ses_kid", clock(2), userJSON(clock(2), "kid-task"), stepJSON(clock(3), "kid-answer"))
	pause()
	for range 5 {
		prompt("ses_a")
		prompt("ses_b")
	}

	time.Sleep(150 * time.Millisecond) // a few more polls to drain the tail
	cancel()
	if err := <-followErr; err != nil {
		t.Fatal(err)
	}

	out := stdout.String()
	for i := range n {
		want := fmt.Sprintf("row-%03d", i)
		if got := strings.Count(out, want); got != 1 {
			t.Errorf("%s emitted %d times, want exactly once", want, got)
		}
	}
	for _, once := range []string{"final-answer", "calling", "kid-task", "kid-answer"} {
		if got := strings.Count(out, once); got != 1 {
			t.Errorf("%q emitted %d times, want exactly once", once, got)
		}
	}
	// Nothing half-written — neither a streaming row's text nor a call
	// without its result — and the opening rows not again.
	for _, never := range []string{"partial", "no result", "pre-0", "pre-1"} {
		if got := strings.Count(out, never); got != 0 {
			t.Errorf("%q emitted %d times by the tail", never, got)
		}
	}
	// The opening and the tail draw every block one pass draws, once each.
	stream := slices.Concat(opening.lines, strings.Split(out, "\n"))
	if got, want := heads(stream), heads(onePass(t, d, roundOpts(t, d))); !slices.Equal(got, want) {
		t.Errorf("heads drawn:\n%s\n\nnot one pass's:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A banner is drawn at a change of session, never again for the session
	// already under way — across a poll included.
	banner := regexp.MustCompile(`^ \S ` + tagOpen + `([^` + tagClose + `]+)` + tagClose)
	prev := ""
	for _, line := range stream {
		if m := banner.FindStringSubmatch(line); m != nil {
			if m[1] == prev {
				t.Errorf("banner for %s repeated", m[1])
			}
			prev = m[1]
		}
	}
}

// TestFollowHoldsUnsettledStep is the regression the hold exists for: a poll
// landing inside a step would print whatever had arrived and move the lane
// past it, so the rest, rewritten in place, would never be read. The step
// must appear once, whole, in the round after it is complete — written out,
// and with a result for every call — and the rounds together must be what one
// pass over the finished fixture prints.
func TestFollowHoldsUnsettledStep(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	// The session's first item, so the opening has nothing else to print.
	live := d.streaming("ses_a", "weighing the")
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}

	first := f.round()
	if len(first.held) != 1 || first.held[0].itemID != live || first.held[0].what != "the step" {
		t.Fatalf("held = %+v, want the step being written", first.held)
	}
	if l := first.cur.lanes["ses_a"]; l.next != 0 || !l.held {
		t.Errorf("lane = %+v, want it left on the step, held", l)
	}
	if len(first.lines) != 0 {
		t.Errorf("the step being written was emitted:\n%s", strings.Join(first.lines, "\n"))
	}
	// The reader is told which of the two reasons an empty opening has.
	if want := "Nothing settled yet — waiting on the step in ses_a."; first.empty != want {
		t.Errorf("empty = %q, want %q", first.empty, want)
	}

	d.rewrite(live, stepJSON(clock(5), "the answer"))
	if second := f.round(); len(second.held) != 0 || !strings.Contains(strings.Join(second.lines, "\n"), "the answer") {
		t.Errorf("held = %+v, lines:\n%s\nwant the step emitted whole", second.held, strings.Join(second.lines, "\n"))
	}

	// A step that has ended but is still waiting on one of its calls is held
	// too, and the call is named.
	d.userText("ses_a", clock(10), "run two things")
	d.step("ses_a", clock(11), "", callJSON("c1", "shell", `{"cmd":"date"}`),
		callJSON("c2", "transfer_task", `{"agent":"helper","task":"look it up"}`))
	d.toolResult("ses_a", "c1", clock(12), "Tue")
	third := f.round()
	if len(third.held) != 1 || third.held[0].what != "transfer_task" || third.held[0].tag != "ses_a" {
		t.Fatalf("held = %+v, want the step waiting on transfer_task", third.held)
	}
	if out := strings.Join(third.lines, "\n"); !strings.Contains(out, "run two things") || strings.Contains(out, "shell") {
		t.Errorf("want the prompt emitted and the step not:\n%s", out)
	}
	d.toolResult("ses_a", "c2", clock(40), "found it")
	if fourth := f.round(); len(fourth.held) != 0 {
		t.Errorf("held = %+v after the last result, want none", fourth.held)
	}

	// The contract: the tail is byte for byte what one pass prints. Only the
	// accumulated stream can say so — a round opens on ground that already
	// ends in endGap and lays down the difference.
	if want := onePass(t, d, roundOpts(t, d)); !slices.Equal(f.stream, want) {
		t.Errorf("rounds:\n%s\n\nnot one pass:\n%s", strings.Join(f.stream, "\n"), strings.Join(want, "\n"))
	}
}

// TestFollowHoldsOnlyItsSession pins the hold to the session: a step waiting
// on its call holds the rest of its own session, and every other session
// keeps printing. Once the session goes on, the step comes out as a run
// without --follow draws it, under the late flag: another session printed
// past it meanwhile.
func TestFollowHoldsOnlyItsSession(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.session("ses_b", "", "B", "/p", clock(0))
	d.userText("ses_a", clock(1), "in A")
	d.step("ses_a", clock(2), "", callJSON("c1", "shell", `{"cmd":"sleep 20"}`))
	d.userText("ses_b", clock(5), "in B")
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}

	opening := f.round()
	if len(opening.held) != 1 || opening.held[0].tag != "ses_a" || opening.held[0].what != "shell" {
		t.Fatalf("held = %+v, want ses_a's step waiting on shell", opening.held)
	}
	out := strings.Join(opening.lines, "\n")
	if !strings.Contains(out, "in A") || !strings.Contains(out, "in B") || strings.Contains(out, "sleep 20") {
		t.Errorf("want both prompts and not the held step:\n%s", out)
	}

	d.userText("ses_a", clock(10), "go on")
	round := f.round()
	if len(round.held) != 0 {
		t.Errorf("held = %+v, want the step released by the next prompt", round.held)
	}
	out = strings.Join(round.lines, "\n")
	for _, want := range []string{lateFlag + " " + tagOpen + "ses_a" + tagClose, "sleep 20", "no result", "go on"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q not in the round:\n%s", want, out)
		}
	}
	if got, want := heads(f.stream), heads(onePass(t, d, roundOpts(t, d))); !slices.Equal(got, want) {
		t.Errorf("heads drawn:\n%s\n\nnot one pass's:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestFollowTransferBurst pins the stall breaker's other side: a transfer_task
// runs for as long as its subagent does, and nothing landing in its session
// meanwhile — a background subagent's marker, an evaluator's row — says the
// step is over. When the subagent ends, its run, the marker and the result
// arrive together, and come out in the order one pass gives them.
func TestFollowTransferBurst(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_p", "", "P", "/p", clock(0))
	d.userText("ses_p", clock(0), "delegate")
	d.step("ses_p", clock(1), "", callJSON("c1", "transfer_task", `{"agent":"helper","task":"look it up"}`))
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}
	waiting := func(r *transcript) bool { return len(r.held) == 1 && r.held[0].what == "transfer_task" }

	if opening := f.round(); !waiting(opening) {
		t.Fatalf("held = %+v, want the transfer_task", opening.held)
	}
	d.whole("ses_p", "bg_kid", clock(2), userJSON(clock(2), "bg-task"), stepJSON(clock(3), "bg-answer"))
	d.item("ses_p", "evaluation", `{"id":"e1","evaluator":"judge","created_at":"2026-08-18T12:00:03.5Z"}`)
	for range 2 {
		if r := f.round(); !waiting(r) {
			t.Fatalf("held = %+v, want the transfer_task still", r.held)
		}
	}
	if !strings.Contains(strings.Join(f.stream, "\n"), "bg-answer") {
		t.Errorf("the background subagent waited with the step:\n%s", strings.Join(f.stream, "\n"))
	}

	d.whole("ses_p", "tr_kid", clock(4), userJSON(clock(4), "look it up"), stepJSON(clock(20), "tr-answer"))
	d.toolResult("ses_p", "c1", clock(21), "found it")
	r := f.round()
	if len(r.held) != 0 {
		t.Fatalf("held = %+v, want the step released by its result", r.held)
	}
	out := strings.Join(r.lines, "\n")
	// The step with its call answered, the marker, then the subagent's run
	// under a banner of its own, dashed.
	last := -1
	for _, want := range []string{
		"transfer_task", "session " + tagOpen + "tr_kid↓" + tagClose,
		newFlag + " " + tagOpen + "tr_kid↓" + tagClose, "┈", "tr-answer",
	} {
		i := strings.Index(out, want)
		if i < 0 || i < last {
			t.Errorf("%q missing or out of order (at %d, after %d):\n%s", want, i, last, out)
		}
		last = i
	}
	if strings.Contains(out, "no result") {
		t.Errorf("the call was drawn without its result:\n%s", out)
	}
	if got, want := heads(f.stream), heads(onePass(t, d, roundOpts(t, d))); !slices.Equal(got, want) {
		t.Errorf("heads drawn:\n%s\n\nnot one pass's:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestFollowBackgroundBurst: a background subagent lands after its parent
// went on. Its run is stamped back to when it started, before what is already
// printed, so it comes in under the late flag; the parent's marker and the
// wait's result come where their rows sit. Every block once.
func TestFollowBackgroundBurst(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_p", "", "P", "/p", clock(0))
	d.userText("ses_p", clock(0), "start one")
	d.step("ses_p", clock(1), "", callJSON("b1", "run_background_agent", `{"agent":"helper","task":"sleep"}`))
	d.toolResult("ses_p", "b1", clock(1), "Background agent task started with ID: agent_task_1")
	d.step("ses_p", clock(5), "meanwhile")
	d.step("ses_p", clock(6), "", callJSON("w1", "wait_background_agents", `{}`))
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}

	if opening := f.round(); len(opening.held) != 1 || opening.held[0].what != "wait_background_agents" {
		t.Fatalf("held = %+v, want the wait", opening.held)
	}
	d.whole("ses_p", "bg_kid", clock(2), userJSON(clock(2), "sleep"), stepJSON(clock(4), "done sleeping"))
	d.toolResult("ses_p", "w1", clock(7),
		`{"all_done":true,"tasks":[{"task_id":"agent_task_1","status":"completed","done":true,"output":"done sleeping"}]}`)
	r := f.round()
	if len(r.held) != 0 {
		t.Fatalf("held = %+v, want the wait released by its result", r.held)
	}
	out := strings.Join(r.lines, "\n")
	last := -1
	for _, want := range []string{
		lateFlag + " " + tagOpen + "bg_kid↓" + tagClose, "done sleeping",
		"wait_background_agents", "task agent_task_1 completed", "session " + tagOpen + "bg_kid↓" + tagClose,
	} {
		i := strings.Index(out, want)
		if i < 0 || i < last {
			t.Errorf("%q missing or out of order (at %d, after %d):\n%s", want, i, last, out)
		}
		last = i
	}
	if got, want := heads(f.stream), heads(onePass(t, d, roundOpts(t, d))); !slices.Equal(got, want) {
		t.Errorf("heads drawn:\n%s\n\nnot one pass's:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestFollowLateResult: a result whose step was already printed without it —
// released by the session going on, or skipped as older than --since while
// its call still ran — is drawn the way one pass draws a result no call
// answers, once. docker-agent itself writes neither, save at the window's
// edge.
func TestFollowLateResult(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	orphans := func(stream []string) int {
		return strings.Count(strings.Join(stream, "\n"), headSep+"tool")
	}

	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.userText("ses_a", clock(0), "go")
	d.step("ses_a", clock(1), "", callJSON("c1", "shell", `{"cmd":"sleep 20"}`))
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}
	f.round()
	d.userText("ses_a", clock(5), "never mind")
	f.round()
	d.toolResult("ses_a", "c1", clock(6), "woke")
	f.round()
	f.round()
	if n := orphans(f.stream); n != 1 {
		t.Errorf("a result after its step was released drawn %d times, want once:\n%s", n, strings.Join(f.stream, "\n"))
	}

	d = newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(-3600))
	d.userText("ses_a", clock(-3600), "go")
	d.step("ses_a", clock(-3599), "", callJSON("c1", "shell", `{"cmd":"sleep 3600"}`))
	opts := roundOpts(t, d, "--follow")
	opts.all, opts.since = false, new(ms(t, clock(0)))
	f = &tailer{t: t, st: d.open(t), opts: opts}
	if opening := f.round(); len(opening.held) != 0 || len(opening.lines) != 0 {
		t.Fatalf("opening held %+v and drew %d lines, want the step skipped as before --since",
			opening.held, len(opening.lines))
	}
	d.toolResult("ses_a", "c1", clock(1), "woke")
	f.round()
	f.round()
	if n := orphans(f.stream); n != 1 {
		t.Errorf("a result after its step was skipped drawn %d times, want once:\n%s", n, strings.Join(f.stream, "\n"))
	}
}

// TestFollowNewSession: the scope is read again every round, so a session
// started mid-tail shows up, as a beginning. A fork is a new session written
// whole with a copy of its parent's history, stamps and all, which is older
// than what is printed: it comes in under the late flag.
func TestFollowNewSession(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.userText("ses_a", clock(0), "hello")
	d.step("ses_a", clock(1), "hi")
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}
	f.round()

	d.session("ses_n", "", "N", "/p", clock(10))
	d.userText("ses_n", clock(10), "new here")
	if out := strings.Join(f.round().lines, "\n"); !strings.Contains(out, newFlag+" "+tagOpen+"ses_n"+tagClose) ||
		!strings.Contains(out, "new here") {
		t.Errorf("want the new session under a beginning's banner:\n%s", out)
	}

	d.whole("", "ses_f", clock(20), userJSON(clock(0), "hello"), stepJSON(clock(1), "hi"), userJSON(clock(20), "and then"))
	out := strings.Join(f.round().lines, "\n")
	if !strings.Contains(out, lateFlag+" "+tagOpen+"ses_f"+tagClose) || !strings.Contains(out, "and then") {
		t.Errorf("want the fork under the late flag:\n%s", out)
	}
	if got, want := heads(f.stream), heads(onePass(t, d, roundOpts(t, d))); !slices.Equal(got, want) {
		t.Errorf("heads drawn:\n%s\n\nnot one pass's:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestFollowSessionNewToScope covers a session written whole between a round's
// reading the scope and its asking for new rows: the round's id passes the
// rows of a session it has not seen. The next round finds the session in
// scope with no lane, and reads it whole.
func TestFollowSessionNewToScope(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.userText("ses_a", clock(0), "hello")
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}
	f.round()

	d.whole("", "ses_f", clock(5), userJSON(clock(5), "slipped in"))
	f.cur.lastID = d.lastID()
	f.round()
	f.round()
	if n := strings.Count(strings.Join(f.stream, "\n"), "slipped in"); n != 1 {
		t.Errorf("a session the id passed before it was in scope drawn %d times, want once:\n%s",
			n, strings.Join(f.stream, "\n"))
	}
}

// TestFollowNothingInScope: an opening that finds no session in scope hands on
// no cursor, and each round reads like an opening until one finds a session.
func TestFollowNothingInScope(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}
	if opening := f.round(); opening.cur != nil || opening.empty == "" {
		t.Fatalf("opening cursor = %+v, empty = %q, want no cursor and the reason", opening.cur, opening.empty)
	}

	d.session("ses_a", "", "A", "/p", clock(0))
	d.userText("ses_a", clock(0), "hello")
	f.round()
	f.round()
	out := strings.Join(f.stream, "\n")
	if strings.Count(out, "hello") != 1 || !strings.Contains(out, newFlag+" "+tagOpen+"ses_a"+tagClose) {
		t.Errorf("want the prompt once, under a beginning's banner:\n%s", out)
	}
}

// TestFollowCarriesTime: an item without a timestamp takes the time of the
// one before it, and when that one was read in an earlier round, the time
// comes across in the lane.
func TestFollowCarriesTime(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.userText("ses_a", clock(5), "hello")
	f := &tailer{t: t, st: d.open(t), opts: roundOpts(t, d, "--follow")}
	f.round()

	d.summary("ses_a", "") // a compaction carries no stamp of its own
	want := stamp(ms(t, clock(5))) + headSep + "compaction"
	if out := strings.Join(f.round().lines, "\n"); !strings.Contains(out, want) {
		t.Errorf("want %q, the time of the prompt before it:\n%s", want, out)
	}
}

// TestFollowWindow: a subagent landing mid-tail with the start of its run
// before --since has that start skipped, as one pass with the same --since
// skips it, and its banner says the session was joined part-way.
func TestFollowWindow(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_p", "", "P", "/p", clock(-900))
	d.userText("ses_p", clock(5), "delegate")
	since := new(ms(t, clock(0)))
	opts := roundOpts(t, d, "--follow")
	opts.all, opts.since = false, since
	f := &tailer{t: t, st: d.open(t), opts: opts}
	f.round()

	d.whole("ses_p", "ses_kid", clock(-600), userJSON(clock(-600), "old-task"), stepJSON(clock(-300), "old-step"),
		stepJSON(clock(10), "new-answer"))
	out := strings.Join(f.round().lines, "\n")
	if strings.Contains(out, "old-task") || strings.Contains(out, "old-step") || !strings.Contains(out, "new-answer") {
		t.Errorf("want only the subagent's step inside the window:\n%s", out)
	}
	if !strings.Contains(out, joinFlag+" "+tagOpen+"es_kid↓"+tagClose) {
		t.Errorf("want the subagent's banner flagged as joined:\n%s", out)
	}
	batch := roundOpts(t, d)
	batch.all, batch.since = false, since
	if got, want := heads(f.stream), heads(onePass(t, d, batch)); !slices.Equal(got, want) {
		t.Errorf("heads drawn:\n%s\n\nnot one pass's:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestUnsettledStepPrintsWithoutFollow pins the hold to the flag: a run
// without --follow prints what is in the store at that moment, steps being
// written and all, and has nothing to come back for.
func TestUnsettledStepPrintsWithoutFollow(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.userText("ses_a", clock(0), "hello")
	d.streaming("ses_a", "weighing the")

	got, err := build(context.Background(), d.open(t), roundOpts(t, d), nil, seam{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.held) != 0 {
		t.Errorf("held = %+v without --follow, want none", got.held)
	}
	if out := strings.Join(got.lines, "\n"); !strings.Contains(out, "weighing the") {
		t.Errorf("the step being written was withheld without --follow:\n%s", out)
	}
	if l := got.cur.lanes["ses_a"]; l.next != 2 || l.held {
		t.Errorf("lane = %+v, want it past the step", l)
	}
}

// TestSettledPrefix is the hold's table: what a step still lacks, what proves
// it over all the same, and what does not.
func TestSettledPrefix(t *testing.T) {
	row := func(typ, role string) itemRow {
		return itemRow{typ: typ, data: itemData{Role: flexString(role)}, timeCreated: 10}
	}
	user := row("message", "user")
	live := row("message", "assistant") // no finish_reason: streaming, or cut off mid-stream
	reasoning := live
	reasoning.data.ReasoningContent = "weighing"
	step := func(calls ...string) itemRow {
		it := row("message", "assistant")
		it.data.FinishReason = "stop"
		for _, c := range calls {
			it.data.FinishReason = "tool_calls"
			it.data.ToolCalls = append(it.data.ToolCalls, toolCall{ID: flexString(c)})
		}
		return it
	}
	result := func(call string) itemRow {
		it := row("message", "tool")
		it.data.ToolCallID = flexString(call)
		return it
	}
	old := step("c1")
	old.timeCreated = 1
	outside := func(b block) bool { return b.head.timeCreated < 5 }

	for _, c := range []struct {
		name  string
		items []itemRow
		want  int // blocks emitted
	}{
		{"all complete", []itemRow{user, step("c1"), result("c1"), step()}, 3},
		{"a step being written", []itemRow{user, live}, 1},
		{"a step that streamed only reasoning", []itemRow{user, reasoning}, 1},
		{"a step waiting on a call", []itemRow{user, step("c1", "c2"), result("c2")}, 1},
		{"what follows waits with it", []itemRow{user, live, row("subsession", ""), row("evaluation", "")}, 1},
		{"released by a prompt", []itemRow{live, user}, 2},
		{"released by a step", []itemRow{step("c1"), step()}, 2},
		{"released by an error", []itemRow{step("c1"), row("error", "")}, 2},
		{"released by a compaction", []itemRow{live, row("summary", "")}, 2},
		{"not by a subagent's marker", []itemRow{step("c1"), row("subsession", "")}, 0},
		{"not by an evaluation", []itemRow{step("c1"), row("evaluation", "")}, 0},
		{"not by a result no call answers", []itemRow{step("c1"), result("c9")}, 0},
		{"before --since", []itemRow{old, row("subsession", "")}, 2},
	} {
		if got := settledPrefix(blocks(c.items), outside); got != c.want {
			t.Errorf("%s: settledPrefix = %d, want %d", c.name, got, c.want)
		}
	}

	// A subagent is written whole when it ends, so what it holds is final: it
	// is never held, not even a step it left unfinished.
	t.Setenv("COLUMNS", "100")
	d := newTestDB(t)
	d.session("ses_a", "", "A", "/p", clock(0))
	d.session("ses_kid", "ses_a", "", "/p", clock(1))
	d.userText("ses_kid", clock(1), "task")
	d.streaming("ses_kid", "cut off")
	got, err := build(context.Background(), d.open(t), roundOpts(t, d, "--follow"), nil, seam{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.held) != 0 || !strings.Contains(strings.Join(got.lines, "\n"), "cut off") {
		t.Errorf("held = %+v, lines:\n%s\nwant the subagent's step printed", got.held, strings.Join(got.lines, "\n"))
	}
}
