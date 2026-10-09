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

// zooDB builds the fixture the goldens are drawn from: four sessions over two
// days, one shape of docker-agent's store after another.
//
// zoo001 is the main session. Its first step reasons, writes and makes four
// calls — a shell call that works, one that fails without saying so, one the
// user rejected and an image read — and the prompt after it carries an
// attachment. Then a transfer_task, whose result arrives after the subagent's
// whole run, past its subsession row; a background agent, whose subsession row
// lands while the parent waits for its next step, before the call that waits
// for it; a prompt steered into the running turn; a step cut off mid-stream; a
// call that never got a result; a compaction; an error; an item type this
// version does not know; an evaluator's call; and a result no call answers.
//
// zoo002 is a second project, its turns on day two: a CJK prompt, a step by an
// agent other than root, a prompt from before docker-agent v1.55 with its
// attachments in the old shapes, a prompt whose text files are inlined into
// its text part, a prompt that is an image's bytes, a scheduler's prompt and a
// termination.
//
// zoo003 and zoo004 are the two subagents, the transfer_task's and the
// background one's, each opened by its task and an implicit prompt. zoo004
// ends while its parent is mid-run, so the two interleave.
func zooDB(t *testing.T) *testDB {
	t.Helper()
	d := newTestDB(t)
	const (
		zoo1 = "0b6e1f2a-4c3d-4e5f-8a9b-1c2d3ezoo001"
		zoo2 = "7d4a9c1e-2b8f-4a6d-9e3c-5f1a2bzoo002"
		zoo3 = "e2c8b5d1-6f4a-4c9e-8b2d-3a7f1czoo003"
		zoo4 = "5c1d7e3f-9a2b-4f8c-b3d6-7e4f9azoo004"
	)
	// Written with the offset docker-agent writes local time with: 15:00+05:00
	// is the 10:00 UTC the tests run in.
	d.session(zoo1, "", "Zoo main", "/proj", "2026-08-17T15:00:00+05:00")
	d.session(zoo2, "", "", "/proj/sub", "2026-08-17T10:00:01Z")
	d.session(zoo3, zoo1, "Transferred task", "/proj", "2026-08-17T15:03:30+05:00")
	d.session(zoo4, zoo1, "Background agent task", "/proj", "2026-08-17T15:05:30+05:00")

	// A base64 image of ~200 KB, the size a pasted screenshot is. Neither an
	// attachment nor a tool's image may put any of it on the screen.
	image := strings.Repeat("QUJD", 50_000)
	// call is one tool_calls entry; its arguments are the JSON string the
	// store keeps them as.
	call := func(id, name, args string) string {
		return `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + jsonStr(args) + `}}`
	}
	result := func(sid, agent, id, at, content string) {
		t.Helper()
		d.message(sid, agent, `{"role":"tool","tool_call_id":"`+id+`","content":`+jsonStr(content)+`,"created_at":"`+at+`"}`)
	}
	// A step's figures, for the steps the tests do not look at closely.
	const spent = `"cost":0.002,"usage":{"input_tokens":200,"output_tokens":40,"cached_input_tokens":12000,"cached_write_tokens":0}`
	// The task a subagent is handed, exactly as buildTaskSystemMessage writes
	// it (pkg/runtime/agent_delegation.go): the expected output only when the
	// call gave one, and the files the user had attached to the parent.
	task := func(task, expected string) string {
		text := "You are a member of a team of agents. Your goal is to complete the following task:\n\n<task>\n" +
			task + "\n</task>"
		if expected != "" {
			text += "\n\n<expected_output>\n" + expected + "\n</expected_output>"
		}
		return text + "\n\nThe user attached these files in the original conversation. They are available for you" +
			" to read at these absolute paths; prefer them over any bare filenames mentioned in <task>:\n" +
			"<attached_files>\n- /proj/shot.png\n</attached_files>" +
			"\n\nIf the task references files, treat any absolute paths in <task> as authoritative and use them" +
			` as-is. If a referenced file is given by name only (e.g. "foo.go"), do not guess: search the workspace` +
			" or ask the calling agent for the absolute path before reading or modifying the file."
	}

	d.userText(zoo1, "2026-08-17T15:01:00+05:00", "[chat | user=x]\nhello world")
	d.message(zoo1, "root", `{"role":"assistant","message_id":"m1","created_at":"2026-08-17T15:02:00+05:00",
	  "content":"I will run tools.","reasoning_content":"weighing the options \nacross two lines",
	  "tool_calls":[`+strings.Join([]string{
		call("call_1", "shell", `{"cmd":"echo hi"}`),
		call("call_2", "shell", `{"cmd":"ls /nope"}`),
		call("call_3", "shell", `{"cmd":"uname"}`),
		call("call_4", "read_file", `{"path":"/proj/logo.png"}`),
	}, ",")+`],
	  "tool_definitions":[{"name":"shell","category":"shell","description":"Run a shell command",
	                       "parameters":{"type":"object"}}],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls","cost":0.0123,
	  "usage":{"input_tokens":12000,"output_tokens":3400,"cached_input_tokens":88000,
	           "cached_write_tokens":1200,"reasoning_tokens":31}}`)
	result(zoo1, "root", "call_1", "2026-08-17T15:02:00+05:00", "hi\n")
	// A shell command that fails is not an error to docker-agent.
	result(zoo1, "root", "call_2", "2026-08-17T15:02:01+05:00",
		"Error executing command: exit status 2\nOutput: ls: cannot access '/nope': No such file or directory")
	d.message(zoo1, "root", `{"role":"tool","tool_call_id":"call_3","content":"The user rejected the tool call.",
	  "is_error":true,"created_at":"2026-08-17T15:02:09+05:00"}`)
	d.message(zoo1, "root", `{"role":"tool","tool_call_id":"call_4",
	  "content":"Read image file /proj/logo.png [image/png] (20439 bytes)","created_at":"2026-08-17T15:02:10+05:00",
	  "multi_content":[{"type":"text","text":"Read image file /proj/logo.png [image/png] (20439 bytes)"},
	                   {"type":"image_url","image_url":{"url":"data:image/png;base64,`+image+`"}}]}`)
	d.message(zoo1, "", `{"role":"user","content":"@/proj/shot.png see attached","created_at":"2026-08-17T15:03:00+05:00",
	  "multi_content":[{"type":"text","text":"@/proj/shot.png see attached"},
	    {"type":"document","document":{"name":"shot.png","mime_type":"image/png","size":150000,
	                                   "source":{"inline_data":"`+image+`"}}}]}`)

	// The transfer_task: the subagent is written whole when it finishes, its
	// subsession row here, and the result comes after it.
	d.message(zoo1, "root", `{"role":"assistant","message_id":"m2","created_at":"2026-08-17T15:03:30+05:00",
	  "tool_calls":[`+call("call_5", "transfer_task", `{"agent":"helper","task":"do the subtask","expected_output":"done"}`)+`],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls",`+spent+`}`)
	d.subsession(zoo1, zoo3)
	result(zoo1, "root", "call_5", "2026-08-17T15:05:00+05:00", "done: the subtask is finished")

	// The background agent: started, then waited for. It finished while the
	// parent was waiting for its next step, so its subsession row lands before
	// the call that waits for it.
	d.message(zoo1, "root", `{"role":"assistant","message_id":"m3","created_at":"2026-08-17T15:05:30+05:00",
	  "tool_calls":[`+call("call_6", "run_background_agent", `{"agent":"helper","task":"sleep 20"}`)+`],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls",`+spent+`}`)
	result(zoo1, "root", "call_6", "2026-08-17T15:05:30+05:00",
		"Background agent task started with ID: agent_task_1\nAgent: helper\nTask: sleep 20")
	d.message(zoo1, "root", `{"role":"assistant","message_id":"m4","created_at":"2026-08-17T15:06:00+05:00",
	  "content":"Waiting for the background agent.",
	  "tool_calls":[`+call("call_7", "shell", `{"cmd":"date"}`)+`],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls",`+spent+`}`)
	result(zoo1, "root", "call_7", "2026-08-17T15:06:01+05:00", "\x1b[1mMon Aug 17 10:06:01 UTC 2026\x1b[0m\n")
	d.subsession(zoo1, zoo4)
	d.message(zoo1, "root", `{"role":"assistant","message_id":"m5","created_at":"2026-08-17T15:06:30+05:00",
	  "tool_calls":[`+call("call_8", "wait_background_agents", `{"task_ids":["agent_task_1"]}`)+`],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls",`+spent+`}`)
	result(zoo1, "root", "call_8", "2026-08-17T15:06:30+05:00",
		`{"all_done":true,"tasks":[{"task_id":"agent_task_1","status":"completed","done":true,"output":"slept"}]}`)

	// Typed while the agent worked, and steered into the running turn.
	d.message(zoo1, "", `{"role":"user","message_id":"u1","content":"also check the logs",
	  "created_at":"2026-08-17T15:06:35+05:00"}`)
	// Cut off mid-stream: no created_at, finish_reason, usage, model or calls.
	d.message(zoo1, "root", `{"role":"assistant","message_id":"m6","content":"partial answer",
	  "reasoning_content":"half a thought"}`)
	// Cut off while its tool ran: the call has no result, and the session goes on.
	d.message(zoo1, "root", `{"role":"assistant","message_id":"m7","created_at":"2026-08-17T15:07:00+05:00",
	  "tool_calls":[`+call("call_9", "shell", `{"cmd":"sleep 20"}`)+`],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls",`+spent+`}`)
	d.summary(zoo1, `{"input_tokens":400,"output_tokens":100,"cached_input_tokens":0,"cached_write_tokens":0}`)
	d.userText(zoo1, "2026-08-17T15:08:30+05:00", "try again")
	d.item(zoo1, "error", `{"message":"model failed: HTTP 400","code":"model_error","agent_name":"root",
	  "created_at":"2026-08-17T15:09:00+05:00"}`)
	d.item(zoo1, "mystery", `{"note":"a type this version does not know"}`)
	d.item(zoo1, "evaluation", `{"id":"e1","evaluator":"judge","agent_name":"root","model":"zai/glm-5.3",
	  "usage":{"input_tokens":30,"output_tokens":3},"cost":0.0001,"created_at":"2026-08-17T15:09:30.5+05:00"}`)
	result(zoo1, "root", "call_orphan", "2026-08-17T15:09:40+05:00", "stray result")

	// Day two, second session: started on day one, so its span runs past a
	// day.
	d.userText(zoo2, "2026-08-18T09:30:00Z", "日本語のコマンド 🎌 とても長い行")
	d.message(zoo2, "viewer", `{"role":"assistant","created_at":"2026-08-18T09:31:00Z","content":"ok",
	  "model":"zai/glm-5.3-flash","finish_reason":"stop",
	  "usage":{"input_tokens":900,"output_tokens":120,"cached_input_tokens":0,"cached_write_tokens":0}}`)
	// Written before v1.55: an image as a nameless data: URL, a file by path.
	d.message(zoo2, "", `{"role":"user","content":"compare these","created_at":"2026-08-18T09:35:00Z",
	  "multi_content":[{"type":"text","text":"compare these"},
	    {"type":"image_url","image_url":{"url":"data:image/png;base64,`+image+`","detail":"auto"}},
	    {"type":"file","file":{"path":"/proj/spec.pdf","mime_type":"application/pdf"}}]}`)
	// The TUI's shape: a text file and an attachment carried over as text are
	// inlined into the text part after what was typed, a resized image leaves
	// a note there, and only the image is a part of its own.
	typed := "@/proj/main.go @/proj/diagram.png explain these"
	d.message(zoo2, "", `{"role":"user","content":`+jsonStr(typed)+`,"created_at":"2026-08-18T09:36:00Z",
	  "multi_content":[{"type":"text","text":`+jsonStr(typed+inlinedTail)+`},
	    {"type":"document","document":{"name":"diagram.png","mime_type":"image/jpeg","size":150000,
	                                   "source":{"inline_data":"`+image+`"}}}]}`)
	d.userText(zoo2, "2026-08-18T09:37:00Z", pngPrompt)
	d.message(zoo2, "", `{"role":"user","message_id":"u2",
	  "content":`+jsonStr(`⏰ Scheduled task "nightly" (sched-1) is due: run the report`)+`,
	  "created_at":"2026-08-18T09:40:00Z"}`)
	d.item(zoo2, "termination", `{"reason":"budget_exceeded","budget":"run","limit":"max_cost","used":"$0.51",
	  "max":"$0.50","message":"run budget exceeded: $0.51 of $0.50"}`)

	// The transfer_task's subagent, stamped from its start.
	d.message(zoo3, "", `{"role":"system","content":`+jsonStr(task("do the subtask", "done"))+`,
	  "created_at":"2026-08-17T15:03:30+05:00"}`)
	d.implicitUser(zoo3, "2026-08-17T15:03:30+05:00", "Please proceed.")
	d.message(zoo3, "helper", `{"role":"assistant","message_id":"h1","created_at":"2026-08-17T15:04:00+05:00",
	  "tool_calls":[`+call("call_h1", "shell", `{"cmd":"sleep 20"}`)+`],
	  "model":"zai/glm-5.3","finish_reason":"tool_calls",`+spent+`}`)
	result(zoo3, "helper", "call_h1", "2026-08-17T15:04:20+05:00", "<no output>")
	d.message(zoo3, "helper", `{"role":"assistant","message_id":"h2","created_at":"2026-08-17T15:04:50+05:00",
	  "content":"done: the subtask is finished","model":"zai/glm-5.3","finish_reason":"stop",`+spent+`}`)

	// The background subagent: no expected output, since the call gave none.
	d.message(zoo4, "", `{"role":"system","content":`+jsonStr(task("sleep 20", ""))+`,
	  "created_at":"2026-08-17T15:05:30+05:00"}`)
	d.implicitUser(zoo4, "2026-08-17T15:05:30+05:00", "Please proceed.")
	d.message(zoo4, "helper", `{"role":"assistant","message_id":"b1","created_at":"2026-08-17T15:06:10+05:00",
	  "content":"slept","model":"zai/glm-5.3","finish_reason":"stop",`+spent+`}`)
	return d
}

// inlinedTail is what docker-agent appends to a prompt's text part for its
// attachments: a text file (chat.ReadFileForInline), the note on a resized
// image (chat.FormatDimensionNote) and an attachment carried over as text,
// which has a name where a file has a path.
const inlinedTail = "\n\n<attached_file path=\"/proj/main.go\">\npackage main\n\nfunc main() {}\n</attached_file>" +
	"\n[Image: original 2080x302, displayed at 2000x290. Multiply coordinates by 1.04 to map to original image.]" +
	"\n\n<attached_file path=\"notes.txt\">\nkeep this out\n</attached_file>"

// pngPrompt is a prompt whose text is a PNG file's bytes, cut short, the way
// the live store holds six of them: the signature's first byte already U+FFFD
// and its CRLF an LF, NUL bytes throughout, and here an ESC as well, which
// must not reach the screen either.
const pngPrompt = "\ufffdPNG\n\x1a\n\x00\x00\x00\nIHDR\x00\x00\x02@\x00\x00\x00\ufffd\b\x06\x00\x00\x00+g\x07" +
	"\ufffd\x1b[2J\x00\x00\x00\x00IEND\ufffdB`\ufffd"

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
		{"none", "", []string{"--tools", "none"}},
		{"color", "", []string{"--color", "always", "--tools", "full", "--reasoning", "--stats"}},
		{"list", "", []string{"--list"}},
		// A terminal too narrow for the content, which is the only way to fix
		// what wrapping actually produces: every other golden renders at 100,
		// where nothing has to fold. The coloured twin is the one that matters
		// most — a span open where a line breaks has to be closed there and
		// reopened after the prefix, and nothing else in the suite says what
		// those bytes should be.
		{"narrow", "60", []string{"--tools", "full"}},
		{"narrow-color", "60", []string{"--color", "always", "--tools", "full"}},
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

// TestColourTransparency is the property the whole SGR apparatus exists to
// hold, and the one nothing else asserts: a coloured run prints what a plain
// run prints, with escapes threaded through it and not one visible cell moved.
//
// It is worth stating on its own because the layout has to take colour back out
// to do its job — visibleCells strips it to measure a line, and wrapLine has to
// recognise a span, close it where it breaks the line and reopen it after the
// prefix. All of that is machinery for keeping colour invisible to the layout,
// and this is the assertion that it did. The wrapping case is the one that can
// actually fail: a span dropped, doubled or reopened a cell late is invisible
// in a diff of coloured output and obvious here.
func TestColourTransparency(t *testing.T) {
	d := zooDB(t)
	for _, c := range []struct {
		name    string
		columns string
		args    []string
	}{
		{"nothing has to fold", "", []string{"--tools", "full", "--reasoning", "--stats"}},
		{"everything folds", "60", []string{"--tools", "full", "--reasoning", "--stats"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain := runArgs(t, d, c.columns, c.args...)
			coloured := runArgs(t, d, c.columns, append([]string{"--color", "always"}, c.args...)...)
			if coloured == plain {
				t.Fatal("--color always produced no escapes at all")
			}
			if got := stripSGR(coloured); got != plain {
				t.Error("stripped colour differs from the plain run\n" +
					diffReport("plain", plain, "stripped", got))
			}
		})
	}
}

func TestSessionBannerFlags(t *testing.T) {
	s := &session{sessionRow: sessionRow{id: "ses_aaaaaa", title: "Zoo", timeCreated: 100}, tag: "aaaaaa"}
	// Whether the session predates the window is a fact about the session, not
	// about what this particular run of blocks happens to contain — but a
	// banner for a session already announced answers a nearer question first,
	// and says so however the window fell.
	for _, c := range []struct {
		name  string
		since *int64
		seen  bool
		want  string
	}{
		{"started inside the window", new(int64(50)), false, newFlag},
		{"started before it", new(int64(200)), false, joinFlag},
		{"--all: no window to predate", nil, false, newFlag},
		{"come back to", new(int64(50)), true, backFlag},
		{"come back to, and older than the window", new(int64(200)), true, backFlag},
	} {
		if got := bannerFlag(s, c.since, c.seen); got != c.want {
			t.Errorf("%s: bannerFlag = %q, want %q", c.name, got, c.want)
		}
	}

	paint := Paint{}
	// A banner is its two own lines and no blank rows: the gap around it is the
	// assembler's.
	for _, flag := range []string{newFlag, joinFlag, backFlag} {
		want := " " + flag + " " + tagOpen + "aaaaaa" + tagClose + " Zoo"
		if got := sessionBanner(s, flag, paint, 40)[0]; !strings.HasPrefix(got, want) {
			t.Errorf("banner = %q, want prefix %q", got, want)
		}
	}
	// The flag is charged to the title's budget, so a banner still fits.
	wide := &session{sessionRow: sessionRow{title: strings.Repeat("t", 100)}, tag: strings.Repeat("g", 10)}
	for _, line := range sessionBanner(wide, newFlag, paint, 30) {
		if cells(line) > 30 {
			t.Errorf("banner line %q is %d cells", line, cells(line))
		}
	}
}

// The path on a banner answers what the title leaves out, and it is the one
// thing on the line that may be dropped: the title is what the banner is for.
func TestSessionBannerWhere(t *testing.T) {
	paint := Paint{}
	s := &session{sessionRow: sessionRow{title: "Zoo", directory: "/proj/sub"}, tag: "aaaaaa"}
	// Whole and absolute, out at the right margin, and the line still ends
	// exactly at the width. The root is not subtracted from it the way the
	// session table subtracts it: a transcript is read away from the terminal
	// that produced it, where a relative path names nothing.
	line := sessionBanner(s, newFlag, paint, 40)[0]
	if !strings.HasSuffix(line, " /proj/sub") || cells(line) != 40 {
		t.Errorf("banner = %q (%d cells), want a 40-cell line ending in the absolute path", line, cells(line))
	}
	// A session the store never gave a directory has nothing to say out there,
	// and says it without trailing blanks.
	blank := &session{sessionRow: sessionRow{title: "Zoo"}, tag: "aaaaaa"}
	if line := sessionBanner(blank, newFlag, paint, 40)[0]; line != strings.TrimRight(line, " ") {
		t.Errorf("banner = %q, want no trailing blanks where there is no directory", line)
	}
	// Too narrow to hold both: the title takes the room back rather than the
	// path being drawn as an ellipsis and a letter.
	narrow := &session{sessionRow: sessionRow{title: strings.Repeat("t", 100), directory: "/proj/deep/sub"}, tag: "aaaaaa"}
	for _, line := range sessionBanner(narrow, newFlag, paint, 30) {
		if cells(line) > 30 {
			t.Errorf("banner line %q is %d cells", line, cells(line))
		}
	}
	if line := sessionBanner(narrow, newFlag, paint, 30)[0]; strings.Contains(line, "…") && strings.Contains(line, "sub") {
		t.Errorf("banner = %q, want the path dropped at this width", line)
	}
}

func TestIndentBlockClamp(t *testing.T) {
	paint := Paint{}
	// A negative cap clamps to zero and reports what was actually dropped.
	got := indentBlock("out", "abcdef", -5, paint, grey)
	if !strings.Contains(got, "[6 more chars]") || strings.Contains(got, "abc") {
		t.Errorf("negative cap: %q", got)
	}
	// The cap counts runes, not bytes.
	got = indentBlock("out", "ééééé", 3, paint, grey)
	if !strings.Contains(got, "ééé\n") || !strings.Contains(got, "[2 more chars]") {
		t.Errorf("rune cap: %q", got)
	}
}

// TestEmptyReasoningIsNotABody pins the guard the reasoning case shares with
// text. docker-agent inserts a step's row at its first streamed chunk, before
// the model has written anything worth reading into it, so a row that exists
// but whose reasoning is still blank must not fabricate a block: a header over
// a blank rail.
func TestEmptyReasoningIsNotABody(t *testing.T) {
	d := newTestDB(t)
	d.session("aaaaaa", "", "A", "/p", "2026-08-18T12:00:00Z")
	d.message("aaaaaa", "root", `{"role":"assistant","message_id":"m1","reasoning_content":"  \n"}`)

	if got := runArgs(t, d, "", "--reasoning"); strings.Contains(got, "assistant") {
		t.Errorf("an empty reasoning item printed a turn:\n%s", got)
	}
}

// TestEmptyPromptIsNotABody is the same guard on a subagent's task and on an
// implicit prompt: one with no text must not print a head over a blank rail.
func TestEmptyPromptIsNotABody(t *testing.T) {
	d := newTestDB(t)
	d.session("aaaaaa", "", "A", "/p", "2026-08-18T12:00:00Z")
	d.message("aaaaaa", "", `{"role":"system","content":" \n","created_at":"2026-08-18T12:00:00Z"}`)
	d.implicitUser("aaaaaa", "2026-08-18T12:00:00Z", " \n")

	if got := runArgs(t, d, ""); strings.Contains(got, "system") || strings.Contains(got, "implicit") {
		t.Errorf("an empty prompt printed a turn:\n%s", got)
	}
}

// A head names the model without its provider and the agent unless it is
// root, docker-agent's default; it says so when a step did not finish, when an
// error item is one, and when a prompt was docker-agent's own.
func TestHeadSuffix(t *testing.T) {
	step := func(agent, model, finish string) *itemRow {
		return &itemRow{typ: "message", agent: agent,
			data: itemData{Role: "assistant", Model: flexString(model), FinishReason: flexString(finish)}}
	}
	for _, c := range []struct {
		name string
		it   *itemRow
		want string
	}{
		{"provider dropped", step("root", "zai/glm-5.3", "stop"), "glm-5.3"},
		{"no provider to drop", step("root", "glm-5.3", "stop"), "glm-5.3"},
		{"only the first provider", step("root", "openrouter/anthropic/claude", "stop"), "anthropic/claude"},
		{"no agent", step("", "zai/glm-5.3", "stop"), "glm-5.3"},
		{"another agent", step("helper", "zai/glm-5.3", "tool_calls"), "glm-5.3 @helper"},
		{"unfinished", step("root", "", ""), "unfinished"},
		{"unfinished, by another agent", step("helper", "", ""), "@helper unfinished"},
		{"an error", &itemRow{typ: "error", data: itemData{Message: "boom"}}, "ERROR"},
		{"an error by root", &itemRow{typ: "error", data: itemData{AgentName: "root"}}, "ERROR"},
		{"an error by another agent", &itemRow{typ: "error", data: itemData{AgentName: "helper"}}, "@helper ERROR"},
		{"an implicit prompt", &itemRow{typ: "message", implicit: true, data: itemData{Role: "user"}}, "implicit"},
		{"a typed prompt", &itemRow{typ: "message", data: itemData{Role: "user"}}, ""},
		{"a subagent's task", &itemRow{typ: "message", data: itemData{Role: "system"}}, ""},
		{"unreadable", &itemRow{typ: "error", unknown: "error (unreadable)"}, ""},
	} {
		if got := strings.Join(headSuffix(c.it), " "); got != c.want {
			t.Errorf("%s: head suffix %q, want %q", c.name, got, c.want)
		}
	}
}

// An item this version cannot read is drawn as a placeholder naming it, and
// the run warns once, at the first one drawn: not for one outside the window,
// and under --list too, where the placeholders are not shown.
func TestUnknownPlaceholder(t *testing.T) {
	d := newTestDB(t)
	d.session("aaaaaa", "", "A", "/p", "2026-08-17T09:00:00Z")
	d.item("aaaaaa", "ghost", `{"created_at":"2026-08-17T09:00:00Z"}`)
	d.userText("aaaaaa", "2026-08-17T11:00:00Z", "hi")
	d.item("aaaaaa", "mystery", `{"created_at":"2026-08-17T11:01:00Z"}`)
	d.message("aaaaaa", "", `{"role":"narrator","content":"x","created_at":"2026-08-17T11:02:00Z"}`)
	t.Setenv("COLUMNS", "")
	draw := func(args ...string) (stdout, stderr string) {
		t.Helper()
		var so, se bytes.Buffer
		if err := run(append([]string{"--db", d.path, "--everywhere"}, args...), &so, &se); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
		return so.String(), se.String()
	}
	const warning = "warning: some items are unknown to this version (first: mystery in aaaaaa)\n"

	out, stderr := draw("--since", "2026-08-17T10:00:00Z")
	for _, want := range []string{"• mystery", "• message (role narrator)"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript lacks the head %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "not shown: unknown to this version of da-transcript"); n != 2 {
		t.Errorf("%d placeholders, want 2:\n%s", n, out)
	}
	if strings.Contains(out, "ghost") {
		t.Errorf("the item outside the window was drawn:\n%s", out)
	}
	if stderr != warning {
		t.Errorf("stderr = %q, want %q", stderr, warning)
	}

	if _, stderr := draw("--since", "2026-08-17T10:00:00Z", "--list"); stderr != warning {
		t.Errorf("--list: stderr = %q, want %q", stderr, warning)
	}
	if out, stderr := draw("--since", "2026-08-17T10:00:00Z", "--until", "2026-08-17T11:01:00Z"); stderr != "" ||
		!strings.Contains(out, "hi") {
		t.Errorf("a window holding no unknown item: stderr %q, transcript:\n%s", stderr, out)
	}
}

// A text file docker-agent inlined into a prompt is named like any other
// attachment, with no type, since the store keeps none. Its text, and the note
// on a resized image, went to the model and are not printed.
func TestInlinedAttachments(t *testing.T) {
	out := runArgs(t, zooDB(t), "", "--tools", "full", "--reasoning", "--stats")
	for _, want := range []string{
		"@/proj/main.go @/proj/diagram.png explain these",
		"attachment main.go (?)",
		"attachment notes.txt (?)",
		"attachment diagram.png (image/jpeg)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
	for _, leak := range []string{"<attached_file ", "</attached_file>", "package main", "keep this out", "Multiply coordinates"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q reached the output", leak)
		}
	}
}

// A prompt whose text holds a NUL byte is not text: it is drawn as one line
// saying how big it is, and none of it reaches the output. A NUL is the whole
// test — an escape in a prompt that has none is the prompt's own, and is
// printed as it came.
func TestBinaryPrompt(t *testing.T) {
	d := newTestDB(t)
	d.session("aaaaaa", "", "A", "/p", "2026-08-18T12:00:00Z")
	d.userText("aaaaaa", "2026-08-18T12:00:00Z", pngPrompt)
	d.userText("aaaaaa", "2026-08-18T12:01:00Z", "red \x1b[31mtext\x1b[0m")

	out := runArgs(t, d, "")
	if want := fmt.Sprintf("not shown: binary data (%d bytes)", len(pngPrompt)); !strings.Contains(out, want) {
		t.Errorf("transcript lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "\x00") || strings.Contains(out, "IHDR") || strings.Contains(out, "\x1b[2J") {
		t.Errorf("the binary prompt reached the output:\n%q", out)
	}
	if !strings.Contains(out, "red \x1b[31mtext\x1b[0m") {
		t.Errorf("a text prompt was not printed as-is:\n%q", out)
	}
}
