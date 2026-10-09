package app

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// zooDB builds the fixture the goldens are drawn from: three sessions (one a
// subagent) over two days, holding just enough to give every --list figure a
// meaning — a model step with usage and reasoning, its tool result, a step
// cut off mid-stream, a subagent marker, a compaction with usage, an error, an
// item type this version does not know, and a CJK prompt.
func zooDB(t *testing.T) *testDB {
	t.Helper()
	d := newTestDB(t)
	const (
		zoo1 = "0b6e1f2a-4c3d-4e5f-8a9b-1c2d3ezoo001"
		zoo2 = "7d4a9c1e-2b8f-4a6d-9e3c-5f1a2bzoo002"
		zoo3 = "e2c8b5d1-6f4a-4c9e-8b2d-3a7f1czoo003"
	)
	// Written with the offset docker-agent writes local time with: 15:00+05:00
	// is the 10:00 UTC the tests run in.
	d.session(zoo1, "", "Zoo main", "/proj", "2026-08-17T15:00:00+05:00")
	d.session(zoo2, "", "", "/proj/sub", "2026-08-17T10:00:01Z")
	d.session(zoo3, zoo1, "Transferred task", "/proj", "2026-08-17T15:00:02+05:00")

	// zoo001 runs nine minutes, to the error at the end; the item after it has
	// no time of its own and carries the error's. Four of its rows are
	// messages, and the step, the compaction and nothing else report usage.
	d.userText(zoo1, "2026-08-17T15:01:00+05:00", "[chat | user=x]\nhello world")
	d.item(zoo1, "message", `{"role":"assistant","message_id":"m1","created_at":"2026-08-17T15:02:00+05:00",
	  "content":"I will run tools.","reasoning_content":"weighing the options\nacross two lines",
	  "tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":`+
		jsonStr(`{"cmd":"echo hi"}`)+`}}],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls","cost":0.0123,
	  "usage":{"input_tokens":12000,"output_tokens":3400,"cached_input_tokens":88000,
	           "cached_write_tokens":1200,"reasoning_tokens":31}}`)
	d.item(zoo1, "message", `{"role":"tool","tool_call_id":"call_1","content":"hi\n",
	  "created_at":"2026-08-17T15:02:00+05:00"}`)
	// Interrupted mid-stream: no created_at, no usage, no finish_reason.
	d.item(zoo1, "message", `{"role":"assistant","message_id":"m2","content":"partial answer"}`)
	d.subsession(zoo1, zoo3)
	d.summary(zoo1, `{"input_tokens":400,"output_tokens":100,"cached_input_tokens":0,"cached_write_tokens":0}`)
	d.item(zoo1, "error", `{"message":"model failed: HTTP 400","code":"model_error",
	  "created_at":"2026-08-17T15:09:00+05:00"}`)
	d.item(zoo1, "mystery", `{"note":"a type this version does not know"}`)

	// Day two, second session: started on day one, so its span runs past a
	// day; CJK and emoji in the prompt.
	d.userText(zoo2, "2026-08-18T09:30:00Z", "日本語のコマンド 🎌 とても長い行")
	d.item(zoo2, "message", `{"role":"assistant","created_at":"2026-08-18T09:31:00Z","content":"ok",
	  "model":"zai/glm-5.3","finish_reason":"stop",
	  "usage":{"input_tokens":900,"output_tokens":120,"cached_input_tokens":0,"cached_write_tokens":0}}`)

	// The subagent's task and its opening prompt are stamped with its start,
	// and its one step was cut off before it reported usage.
	d.item(zoo3, "message", `{"role":"system","created_at":"2026-08-17T15:00:02+05:00",
	  "content":"You are a member of a team of agents. Your goal is to complete the following task: <task>do the subtask</task>"}`)
	d.userText(zoo3, "2026-08-17T15:00:02+05:00", "Please proceed.")
	d.item(zoo3, "message", `{"role":"assistant","content":"working on it"}`)
	return d
}

// runArgs runs the tool over the fixture at the given terminal width and hands
// back what it wrote to stdout; columns "" leaves COLUMNS unset, so the width
// falls back to 100.
func runArgs(t *testing.T, d *testDB, columns string, args ...string) string {
	t.Helper()
	t.Setenv("COLUMNS", columns)
	var stdout, stderr bytes.Buffer
	full := append([]string{"--db", d.path, "--everywhere", "--all"}, args...)
	if err := run(full, &stdout, &stderr); err != nil {
		t.Fatalf("run(%v): %v\nstderr: %s", args, err, stderr.String())
	}
	return stdout.String()
}

// runGoldenAt is runGolden at a given terminal width.
func runGoldenAt(t *testing.T, d *testDB, name, columns string, args ...string) string {
	t.Helper()
	got := runArgs(t, d, columns, args...)
	golden := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (run with -update after intended changes)\n%s",
			name, golden, diffReport("golden", string(want), "got", got))
	}
	return got
}

// diffReport is what a failed comparison of two transcripts says. Printing the
// whole of one of them was no use: these run to a hundred lines and the
// question is always which one moved, so the answer is the first line they
// disagree on and the two readings of it, side by side and quoted so a
// trailing space or a stray escape is visible. The line counts come along
// because a line inserted or dropped shows up there and nowhere in the pair.
func diffReport(wantName, want, gotName, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	n, w, g, differs := firstDiff(wantLines, gotLines)
	if !differs {
		return "the two are identical" // the caller thought otherwise
	}
	return fmt.Sprintf("first difference at line %d:\n  %s: %q\n  %s: %q\n  (%d lines %s, %d %s)",
		n, wantName, w, gotName, g, len(wantLines), wantName, len(gotLines), gotName)
}

// firstDiff names the first line two texts disagree on: its 1-based number and
// the two readings, with a line the shorter text does not reach reported as
// absent rather than as empty.
func firstDiff(want, got []string) (n int, w, g string, differs bool) {
	at := func(lines []string, i int) string {
		if i < len(lines) {
			return lines[i]
		}
		return "<no such line>"
	}
	for i := range max(len(want), len(got)) {
		if w, g := at(want, i), at(got, i); w != g {
			return i + 1, w, g, true
		}
	}
	return 0, "", "", false
}

func TestRenderGolden(t *testing.T) {
	d := zooDB(t)
	cases := []struct {
		name    string
		columns string
		args    []string
	}{
		{"list", "", []string{"--list"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := runGoldenAt(t, d, c.name, c.columns, c.args...)
			if strings.Contains(out, "data:image") || strings.Contains(out, "QUJD") {
				t.Error("a data: URI reached the output")
			}
		})
	}
}
