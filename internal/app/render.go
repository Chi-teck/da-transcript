package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// First argument key that says what a tool call was actually about. Covers
// docker-agent's built-ins and lets an MCP tool name itself through whichever
// of these it happens to take, without a per-tool table. oc-transcript's list
// in its order, with docker-agent's own names put in: cmd first, for the shell
// and background jobs; query before path, for search_files_content; task
// before agent and name, for transfer_task, run_background_agent and
// run_skill.
var argKeys = []string{
	"cmd", "command", "filePath", "pattern", "urls", "url", "query", "channel", "username",
	"post_id", "task", "prompt", "thought", "memory", "path", "paths", "description",
	"descriptions", "task_ids", "task_id", "job_id", "session_id", "agent", "name", "file",
	"code", "entity_type", "message",
}

// oneLine collapses a value to a single line of at most limit cells. A
// non-string value is its compact JSON. The cap is measured in display cells,
// not code points, so CJK and emoji count for the room they take.
func oneLine(raw json.RawMessage, limit int) string {
	s, ok := unquoteFast(raw)
	if !ok {
		if err := json.Unmarshal(raw, &s); err != nil {
			if string(raw) == "null" || len(raw) == 0 {
				return ""
			}
			s = reformatJSON(raw, -1)
		}
	}
	return oneLineText(s, limit)
}

func oneLineText(value string, limit int) string {
	// Neutralised before the whitespace goes, so a summary never carries an
	// escape and a CRLF collapses like any other line break.
	value = strings.Join(strings.Fields(neutralise(value)), " ")
	if limit < 1 {
		if value != "" {
			return "…"
		}
		return ""
	}
	return truncateCells(value, limit)
}

// toolSummary is the one-line gist of a tool call: the first known argument
// that has a value, else the whole arguments. Arguments that were not JSON, or
// were a bare JSON string, are kept as a JSON string of their text, which is no
// object, so they come out as that text.
func toolSummary(args toolArguments, limit int) string {
	input := json.RawMessage(args)
	var keyed map[string]json.RawMessage
	if truthy(input) {
		if err := json.Unmarshal(input, &keyed); err != nil {
			keyed = nil
		}
		for _, key := range argKeys {
			if v, ok := keyed[key]; ok && truthy(v) {
				return oneLine(v, limit)
			}
		}
	}
	if truthy(input) {
		return oneLine(input, limit)
	}
	return ""
}

// renderWait draws what wait_background_agents returned, which is how
// docker-agent hands a background agent's end back to its parent where
// opencode injects a <task> envelope. It is a record, not prose, so it is drawn
// as one: a line per task with its id and the state the wait found it in —
// under compact too, the way the envelope's line is, so a task that failed or
// is still running shows without asking for more — and under --tools full,
// what the task returned or the error it failed with. An error of the wait's
// own comes first. A result that is not this JSON comes back ok=false and is
// drawn like any other output.
//
// No session tag on the line: a task id is docker-agent's own counter, not a
// session id, and the subsession marker names the session. Nor is the line
// charged to --arg-width, which caps a summary of what the model wrote; this
// line holds only docker-agent's id and status.
func renderWait(content string, opts *options) ([]string, bool) {
	var w waitResult
	if err := json.Unmarshal([]byte(content), &w); err != nil || w.Tasks == nil {
		return nil, false
	}
	paint := opts.paint
	var lines []string
	if w.Error != "" {
		lines = append(lines, indentBlock("err", neutralise(string(w.Error)), opts.maxChars, paint, errColor))
	}
	for _, t := range w.Tasks {
		state := neutralise(cmp.Or(string(t.TaskID), "?") + " " + cmp.Or(string(t.Status), "?"))
		line := "  " + paint.paint("task", grey) + " " + paint.paint(state, argColor)
		// Cut at 4 KB by docker-agent; the rest is not kept anywhere.
		if truthy(t.Truncated) {
			line += " " + paint.paint("[output truncated]", dim)
		}
		lines = append(lines, line)
		if opts.tools != "full" {
			continue
		}
		if t.Output != "" {
			lines = append(lines, indentBlock("result", neutralise(string(t.Output)), opts.maxChars, paint, grey))
		}
		if t.Error != "" {
			lines = append(lines, indentBlock("err", neutralise(string(t.Error)), opts.maxChars, paint, errColor))
		}
	}
	return lines, true
}

// capRunes caps text at limit code points, saying how much was dropped. The
// cap is on bulk, not width — the terminal width is the wrapper's business.
func capRunes(body string, limit int) string {
	limit = max(limit, 0)
	// Byte length bounds rune count, so most bodies skip the counting.
	if len(body) > limit {
		if n := utf8.RuneCountInString(body); n > limit {
			runes := []rune(body)
			body = string(runes[:limit]) + fmt.Sprintf("\n… [%d more chars]", n-limit)
		}
	}
	return body
}

// indentBlock renders a labelled, indented quotation capped at limit
// characters.
func indentBlock(label, body string, limit int, paint Paint, color int) string {
	body = capRunes(strings.TrimRightFunc(body, unicode.IsSpace), limit)
	tag := paint.paint(label, color)
	if strings.Contains(body, "\n") {
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			// The line's own indentation stays outside the span, so the wrapper
			// can still fold it into the prefix and keep continuations aligned.
			trimmed := strings.TrimLeft(line, " ")
			lead := line[:len(line)-len(trimmed)]
			lines[i] = "    " + lead + paint.paint(trimmed, argColor)
		}
		return "  " + tag + "\n" + strings.Join(lines, "\n")
	}
	return "  " + tag + " " + paint.paint(body, argColor)
}

// outcomeWidth is the right-hand block of a tool line: the mark and the
// duration together, one column whatever either of them turns out to be.
const outcomeWidth = 12

// outcomeCell lays that block out. Fixed fields, so outcome and duration each
// line up as their own column: ok/ERR sit right-aligned in the first three
// cells with the duration after them, and a longer mark such as `no result` is
// right-aligned over the whole block instead of spilling left into the
// summary's room.
func outcomeCell(mark, took string) (markCell, tookCell string) {
	if cells(mark) > 3 {
		if took == "" {
			return fmt.Sprintf("%*s", outcomeWidth, mark), ""
		}
		// One space between them here rather than the usual two, so a wide
		// status and a duration would still fit the block together. Kept as
		// oc has it, though no call drawn here is both: `no result`, the one
		// wide mark, never has a duration.
		return fmt.Sprintf("%*s", max(cells(mark), outcomeWidth-1-cells(took)), mark), " " + took
	}
	return fmt.Sprintf("%3s", mark), fmt.Sprintf("  %7s", took)
}

// toolDuration is how long a call took, in whole seconds, or "" when it got
// no result. It runs from the call's step, which docker-agent stamps when the
// model finished it, to the result, so the time a call waited for approval is
// in it; the store keeps no time of the tool's own.
func toolDuration(called int64, result *itemRow) string {
	if result == nil || result.timeCreated < called {
		return ""
	}
	return fmt.Sprintf("%ds", (result.timeCreated-called)/1000)
}

// renderTool is one tool call: a head line with the outcome at the right
// margin, then, with --tools full, the arguments and the output or error. A
// result no call answers comes with call nil and is drawn nameless, with no
// arguments and no duration; a call that got no result comes with result nil.
// called is the time of the call's step.
func renderTool(call *toolCall, called int64, result *itemRow, opts *options) []string {
	var name, took string
	var args toolArguments
	if call != nil {
		name, args = string(call.Function.Name), call.Function.Arguments
		took = toolDuration(called, result)
	}
	mark := toolOutcome(name, result)
	failed := mark == "ERR"
	markCell, tookCell := outcomeCell(mark, took)
	right := markCell + tookCell

	paint := opts.paint
	label := cmp.Or(neutralise(name), "?")
	truncated := outputTruncated(result)
	note := ""
	if truncated {
		note = "[output truncated]"
	}

	// The outcome sits at the right margin so a run of calls scans as a column
	// of ok/ERR; the summary takes what is left between the name and it. Short
	// of room, the summary yields entirely and then the name is cut short,
	// sooner than either push past the margin. The note never yields: it is the
	// one thing saying the output is not all there. Only a line too narrow for
	// the note beside a one-cell name runs past the margin and is wrapped, and
	// the 60 columns the layout is held to never come to that.
	avail := opts.width - bodyIndent - cells(right) - 2
	if note != "" {
		avail -= 1 + cells(note)
	}
	if cells(label) > avail {
		label = truncateCells(label, max(1, avail))
	}
	rem := avail - cells(label)
	summary := ""
	if rem >= 10 {
		summary = toolSummary(args, min(opts.argWidth, rem-1))
	}
	head := paint.paint(label, grey)
	if summary != "" {
		head += " " + paint.paint(summary, argColor)
	}
	if note != "" {
		head += " " + paint.paint(note, dim)
	}
	markColor := okColor
	if failed {
		markColor = errColor
	}
	// A call with no duration leaves the cell blank, and blanks are not painted:
	// wrapped in a span they would survive the trim at the end of the line, and
	// the coloured run would carry trailing cells the plain one does not.
	if strings.TrimSpace(tookCell) != "" {
		tookCell = paint.paint(tookCell, dim)
	}
	outcome := paint.paint(markCell, markColor, failed) + tookCell
	lines := []string{padTo(head, outcome, opts.width-bodyIndent)}

	// The output is the result's text, neutralised, and a line naming each image
	// or document that came with it by its type. Their data is the file itself,
	// base64, so it is never printed, and the text part docker-agent puts in
	// front of them is the content over again.
	var outputs []string
	if result != nil {
		if text := strings.TrimRightFunc(neutralise(string(result.data.Content)), unicode.IsSpace); text != "" {
			outputs = append(outputs, text)
		}
		for _, p := range result.data.MultiContent {
			switch p.Type {
			case "document":
				mime := ""
				if p.Document != nil {
					mime = string(p.Document.MimeType)
				}
				outputs = append(outputs, "file ("+cmp.Or(neutralise(mime), "?")+")")
			case "image_url":
				// Its type is inside the data: URL, which is never decoded.
				outputs = append(outputs, "file (image)")
			}
		}
	}
	output := strings.Join(outputs, "\n")

	var wait []string
	decoded := false
	if result != nil && name == "wait_background_agents" {
		wait, decoded = renderWait(string(result.data.Content), opts)
	}
	switch {
	case opts.tools == "full":
		if input := json.RawMessage(args); truthy(input) {
			// Arguments kept as a JSON string — not JSON, or a bare JSON string
			// — are printed as the text the model wrote.
			in := reformatJSON(input, 2)
			var text string
			if json.Unmarshal(input, &text) == nil {
				in = text
			}
			lines = append(lines, indentBlock("in", neutralise(in), opts.maxChars, paint, grey))
		}
		switch {
		case decoded:
			lines = append(lines, wait...)
		case failed:
			lines = append(lines, indentBlock("err", cmp.Or(output, "?"), opts.maxChars, paint, errColor))
		case output != "":
			lines = append(lines, indentBlock("out", output, opts.maxChars, paint, grey))
		}
	case decoded:
		lines = append(lines, wait...)
	case failed:
		lines = append(lines, indentBlock("err", cmp.Or(output, "?"), opts.maxChars, paint, errColor))
	}
	return lines
}

// cancelledCall holds the texts docker-agent stores for a call cut off while
// it ran (pkg/runtime/toolexec/dispatcher.go:cancellationMessage). Inside a
// subagent they are stored without is_error.
var cancelledCall = []string{
	"The tool call was canceled by the user.",
	"The tool call was canceled because another tool call in the same batch was canceled by the user.",
	"The tool call was skipped because a post_tool_use hook signalled run termination.",
}

// toolOutcome is the mark a call gets: ok, ERR, or no result when no result
// row answers it — still running, waiting for approval, cut off, or left
// there; the store does not say which. name is the call's tool, "" for a
// result no call answers.
//
// docker-agent keeps no status for a call, only is_error, which it sets for
// what it refused or could not run — a rejection, a denial, a hook's block, a
// handler's error — and not for most of what failed while running. So the text
// is read too. A call cut off while it ran reads as one of cancelledCall,
// without is_error inside a subagent. And a shell command that failed comes
// back as output, opening with what went wrong (shell.go:formatCommandOutput,
// after a [shell-hint] paragraph when there is one): an exit status or a
// signal, a timeout, or the command cancelled. opencode records such a command
// as completed and oc-transcript shows it ok; it is ERR here so that a failed
// build or test run stands out. The one exception is WaitDelay, which Go
// returns only for a command that exited 0 but left a child holding its output
// open. A failing command whose output was over 50 KB still reads ok:
// docker-agent then keeps only the end of it, and the line saying it failed was
// at the start.
func toolOutcome(name string, result *itemRow) string {
	if result == nil {
		return "no result"
	}
	text := string(result.data.Content)
	if truthy(result.data.IsError) || slices.Contains(cancelledCall, text) {
		return "ERR"
	}
	if name == "shell" {
		if strings.HasPrefix(text, "[shell-hint] ") {
			if _, rest, ok := strings.Cut(text, "\n\n"); ok {
				text = rest
			}
		}
		const failure = "Error executing command: "
		if text == "Command cancelled" || strings.HasPrefix(text, "Command timed out after ") ||
			(strings.HasPrefix(text, failure) && !strings.HasPrefix(text, failure+"exec: WaitDelay expired")) {
			return "ERR"
		}
	}
	return "ok"
}

// outputTruncated reports that docker-agent cut the result short. Its
// limit_large_tool_results hook, always on, swaps a result over 50 KB for a
// notice naming the file the whole of it went to, then an excerpt
// (pkg/hooks/builtins/limit_large_tool_results_other.go). The notice is all
// the store keeps, and the file is deleted when the session ends.
func outputTruncated(result *itemRow) bool {
	return result != nil && strings.HasPrefix(string(result.data.Content), "Tool call result was too large (")
}

// headSep joins the fields of a header — stamp, role, model. A gap alone reads
// as alignment that failed to line up; a bullet says the fields are separate
// and deliberate. A full bullet rather than the middle dot the tags use: the
// head is read across, at a glance, and the smaller glyph vanishes at that
// distance. One cell wide, like every other glyph the layout budgets for.
const headSep = " • "

// bodyRail runs down the left of a turn, from its head to its last line. The
// head's underline closes the envelope along the top and the rail closes it
// along the side, which is what an indent alone could not do: an indent says
// how deep a line sits, not where the thing it belongs to ends. Light
// box-drawing, to match the rule under a session banner, and one cell so the
// body keeps its indent of four.
const bodyRail = "│"

// turnColor is the hue a whole turn is drawn in — its head and the rail down
// its body, one colour because they are one envelope — and is how a prompt is
// told from a reply now that the head carries no direction glyph. Colour is
// doing work here that no character does, so with --color never the role
// named in the head is the only thing left saying which way a turn went.
func turnColor(role string) int {
	switch role {
	case "user":
		return userColor
	case "assistant":
		return modelColor
	}
	// A role that is neither takes no side. The label grey rather than the
	// quiet one: this colours a header, and a header has to be readable even
	// when it has nothing to declare.
	return grey
}

// A chunk is one content item's lines; tool chunks pack together, everything else gets
// a blank line around it. A chunk colour is applied line by line, after the
// lines are split and before they are wrapped, so a span never has to survive
// being cut in two.
//
// Chunks are built through newChunk and never as a literal: the zero value of
// color is 0, which is a real colour — black — and would paint anything that
// forgot to say otherwise.
type chunk struct {
	text  string
	tool  bool
	color int
}

func newChunk(text string) chunk { return chunk{text: text, color: noColor} }

// renderAttachment names a file the turn carried. Only the name and type: the
// data is the whole file, base64.
//
// One space in front of the type, like everywhere else: the parentheses already
// say it is an aside and the tier already says it is the quiet one, so a wider
// gap was a third way of saying the same thing and the only one that cost a
// hole in the line.
func renderAttachment(name, mime string, paint Paint) string {
	return paint.paint("attachment", grey) + " " + paint.paint(cmp.Or(neutralise(name), "?"), argColor) +
		" " + paint.paint("("+cmp.Or(neutralise(mime), "?")+")", dim)
}

// renderStats is the --stats line: what one step of a turn spent.
//
// Drawn the way a tool call is drawn: grey on the label, the light tier on the
// value beside it. A count is the value here and the word after it says which
// count, so the figures come up a tier and the nouns stay put. The whole row
// was one dim grey before, which gave the only part of it worth reading the
// same weight as the words labelling it, and left the line reading as a total
// rather than as five of them.
func renderStats(data *itemData, paint Paint) string {
	usage := data.Usage
	if usage == nil {
		usage = &stepUsage{}
	}
	cost, _ := numberValue(data.Cost)
	field := func(value, label string) string {
		return paint.paint(value, argColor) + " " + paint.paint(label, grey)
	}
	stats := paint.paint("tokens", grey) + " " +
		field(numberText(usage.Input, "0"), "in") + " " +
		field(numberText(usage.Output, "0"), "out") + " " +
		field(numberText(usage.CacheRead, "0"), "cached") + " " +
		field(numberText(usage.CacheWrite, "0"), "written") + " " +
		paint.paint(fmt.Sprintf("$%.4f", cost), argColor)
	// The finish reason is the one thing on the line nobody asked for, so it
	// keeps the quiet tier — and the tier is what sets it apart now that the gap
	// in front of it is one space like every other.
	if reason := neutralise(string(data.FinishReason)); reason != "" {
		stats += " " + paint.paint(reason, dim)
	}
	return stats
}

// attachedFile matches the line opening a text file docker-agent inlined into
// a prompt's text part (chat.ReadFileForInline), the path Go-quoted. The store
// keeps no type for it.
var attachedFile = regexp.MustCompile(`(?m)^<attached_file path=("(?:[^"\\]|\\.)*")>$`)

// renderBlock renders one block as a run of lines, or nothing when it has
// nothing to show. The header names the turn, the rail beside it says how far
// the turn runs, and blank lines do the separating within.
//
// What a block shows is its head item, and a step's tool calls with the
// results riding along with it. A result no call answers is a turn of its own,
// and so is a subagent marker; an evaluator's call draws nothing.
//
// Everything printed from the store goes through neutralise first: bodies are
// printed as the agent received them, except that escape sequences and
// control characters are made harmless, format characters that draw nothing
// are shown as their code points and tabs are expanded to spaces.
func renderBlock(b block, opts *options) []string {
	it := b.head
	data := &it.data
	paint := opts.paint
	var body []chunk
	add := func(text string) { body = append(body, newChunk(text)) }
	// A body's text, or nothing when it is only whitespace.
	addText := func(text string) {
		if text = strings.TrimRightFunc(neutralise(text), unicode.IsSpace); text != "" {
			add(text)
		}
	}
	// One tool call's lines. Tool chunks pack together, with no blank line
	// between them, so their outcomes scan as a column.
	addTool := func(lines []string) {
		c := newChunk(strings.Join(lines, "\n"))
		c.tool = true
		body = append(body, c)
	}
	kind := it.typ
	switch {
	case it.unknown != "":
		// Named and said to be unread rather than dropped: a newer docker-agent
		// wrote something here, and a transcript that skipped it would hide that.
		kind = it.unknown
		add(paint.paint("not shown: unknown to this version of da-transcript", dim))

	case it.typ == "message":
		kind = string(data.Role)
		switch data.Role {
		case "user", "system":
			// A subagent's task and a prompt docker-agent injected are printed
			// as they were sent, in full, like a prompt the user typed. A body
			// holding a NUL byte is not text, whatever its role says: the store
			// has prompts that are a PNG file's bytes, and printed they are
			// hundreds of lines of noise and control bytes. No typed text has
			// one.
			//
			// The prompt is content, as typed. A row whose content is empty can
			// still hold one in its text parts, what the model got: serve api
			// stores what its client sent. Then the text parts are the prompt,
			// each up to the first text file inlined into it, with a blank line
			// between them, as docker-agent's own transcript has it.
			text := string(data.Content)
			if strings.TrimSpace(text) == "" {
				var parts []string
				for _, p := range data.MultiContent {
					if p.Type != "text" {
						continue
					}
					part := string(p.Text)
					if loc := attachedFile.FindStringIndex(part); loc != nil {
						part = part[:loc[0]]
					}
					if part = strings.TrimRightFunc(part, unicode.IsSpace); part != "" {
						parts = append(parts, part)
					}
				}
				text = cmp.Or(strings.Join(parts, "\n\n"), text)
			}
			switch {
			case strings.Contains(text, "\x00"):
				add(paint.paint(fmt.Sprintf("not shown: binary data (%d bytes)", len(text)), dim))
			default:
				addText(text)
			}
			for _, p := range data.MultiContent {
				// The attachments, named. A document part is how docker-agent
				// has written one since v1.55; a store upgraded since then still
				// holds the older file part, which has only a path, and the older
				// image_url part, which has nothing but its data.
				var name, mime string
				switch p.Type {
				case "text":
					// A text file is no part of its own: docker-agent inlines
					// it into the text part, after what was typed. It is named
					// like any other attachment; its text, and the note on a
					// resized image, are what the model was given and are left
					// out like an image's bytes. A prompt held in the text parts
					// alone (above) keeps a note that comes before the first file.
					rest := strings.TrimPrefix(string(p.Text), string(data.Content))
					for _, m := range attachedFile.FindAllStringSubmatch(rest, -1) {
						name := ""
						if path, _ := strconv.Unquote(m[1]); path != "" {
							name = filepath.Base(path)
						}
						add(renderAttachment(name, "", paint))
					}
					continue
				case "document":
					if d := p.Document; d != nil {
						name, mime = string(d.Name), string(d.MimeType)
					}
				case "file":
					if f := p.File; f != nil {
						if f.Path != "" {
							name = filepath.Base(string(f.Path))
						}
						mime = string(f.MimeType)
					}
				case "image_url":
					mime = "image"
				default:
					continue
				}
				add(renderAttachment(name, mime, paint))
			}

		case "assistant":
			reasoning := strings.TrimRightFunc(neutralise(string(data.ReasoningContent)), unicode.IsSpace)
			if opts.reasoning && reasoning != "" {
				// No label: the colour is the label. Reasoning is the one item whose
				// whole body is a single kind of thing, so a hue over all of it says
				// what a word in front of the first line said, without competing with
				// the prose for the eye.
				c := newChunk(capRunes(reasoning, opts.maxChars))
				c.color = reasonColor
				body = append(body, c)
			}
			addText(string(data.Content))
			if opts.tools != "none" {
				// A call in the order the model made it, each with its result.
				paired, rest := b.answers()
				for i := range data.ToolCalls {
					addTool(renderTool(&data.ToolCalls[i], it.timeCreated, paired[i], opts))
				}
				// An id answered twice leaves a result no call is left to
				// claim. It is drawn without one rather than dropped.
				for _, r := range rest {
					addTool(renderTool(nil, 0, r, opts))
				}
			}
			// A step cut off gets no line: docker-agent writes its usage when the
			// step finishes, with the finish reason.
			if opts.stats && data.FinishReason != "" {
				add(renderStats(data, paint))
			}

		case "tool":
			// A result no call answers.
			if opts.tools != "none" {
				addTool(renderTool(nil, 0, it, opts))
			}
		}

	case it.typ == "summary":
		// The digest the model wrote is not drawn; it is the history above,
		// retold.
		kind = "compaction"
		add(paint.paint("context compacted", dim))

	case it.typ == "error":
		// A failed run, drawn the way a step that failed is.
		kind = "assistant"
		text := neutralise(errorText(string(data.Code), string(data.Message)))
		add(indentBlock("error", text, opts.maxChars, paint, errColor))

	case it.typ == "termination":
		add(paint.paint(neutralise(cmp.Or(string(data.Message), string(data.Reason), "?")), dim))

	case it.typ == "subsession":
		// Where a subagent's run was folded back into this session, under
		// every --tools: the tag on the subagent's own banner, so the two can
		// be matched by eye. The ↓ is always due, since the subagent's parent is
		// this session and this session is on screen. A subagent since deleted
		// leaves no id, and nothing to name.
		kind = "subagent"
		if it.subsession != "" {
			tag := tagOpen + tagFor(it.subsession) + "↓" + tagClose
			add(paint.paint("session", grey) + " " + paint.paint(tag, sessionColor))
		}
	}

	if len(body) == 0 {
		return nil
	}

	// The banner carries the session, so the header only says what changed within
	// it. It is drawn in the speaker's own hue, the same one the rail beside it
	// takes: head and rail are two sides of one envelope, and painting them
	// apart made the head look like it belonged to the frame rather than to the
	// turn under it. The role is still named in words, which is what is left of
	// the distinction under --color never.
	//
	// The stamp is dated in full rather than a bare clock. A transcript is read in
	// pieces — scrolled into, grepped, piped through less — and a piece that only
	// says 10:27:51 is a piece that has to be traced back to whatever drew the last
	// date. Every line answering for itself costs eleven cells and no thought.
	head := stamp(it.timeCreated) + headSep + kind
	if suffix := headSuffix(it); len(suffix) > 0 {
		head += headSep + strings.Join(suffix, " ")
	}
	// The role, model and agent named in it come from the store too.
	head = neutralise(head)
	color := turnColor(kind)
	if it.implicit {
		// What docker-agent said in the user's place is not the user's turn,
		// and takes the hue of the task it came with.
		color = grey
	}
	return turnEnvelope(head, body, color, opts)
}

// headSuffix is what a head says after the role: which model answered, under
// which agent, and whether the step finished; on an error item, that it is
// one; on a prompt docker-agent injected, that it did. The provider is left off
// the model, and the default agent is not worth naming — it is what @ is
// measured against.
func headSuffix(it *itemRow) []string {
	if it.unknown != "" {
		return nil
	}
	data := &it.data
	var suffix []string
	agent := func(name string) {
		if name != "" && name != "root" {
			suffix = append(suffix, "@"+name)
		}
	}
	switch {
	case it.typ == "message" && data.Role == "assistant":
		if m := string(data.Model); m != "" {
			if _, name, ok := strings.Cut(m, "/"); ok {
				m = name
			}
			suffix = append(suffix, m)
		}
		agent(it.agent)
		// The store keeps no mark for a step cut off: a step without a finish
		// reason was interrupted, or is still being written, and the two look
		// the same.
		if data.FinishReason == "" {
			suffix = append(suffix, "unfinished")
		}
	case it.typ == "error":
		agent(string(data.AgentName))
		suffix = append(suffix, "ERROR")
	case it.typ == "message" && data.Role == "user" && it.implicit:
		suffix = append(suffix, "implicit")
	}
	return suffix
}

// turnEnvelope draws one turn around its body: the head across the top, ruled
// underneath, and the rail down the left of the whole thing.
//
// The rail runs down the whole turn, head included: it is the left side of the
// envelope and the head is inside the envelope, so a rail that started under
// the head left the line naming the turn standing outside the thing it names.
// It takes the rest of the body's indent as its gap, so the body sits where a
// plain indent put it, and it is drawn on every blank line within: broken at
// any gap it would read as a column of ticks rather than as one line, and the
// whole point of it is to say where the turn ends without a line saying so.
func turnEnvelope(head string, body []chunk, color int, opts *options) []string {
	paint := opts.paint
	pad := strings.Repeat(" ", railIndent) + paint.paint(bodyRail, color) +
		strings.Repeat(" ", bodyIndent-railIndent-cells(bodyRail))
	blank := strings.TrimRight(pad, " ")
	out := wrap(pad, paint.underline(head, color), opts.width)
	// One row between the head and the body. The head is the envelope and the
	// body is what came in it; hard against each other, a message's first line
	// read as the last field of its own header. The row carries the rail like
	// every other blank inside the turn — the head sits inside the envelope
	// now, so a bare row here would cut the rail in two and leave the head with
	// a tick of its own above the body's.
	out = append(out, blank)
	for i, c := range body {
		// One blank line between chunks keeps prose, blocks and labels apart; a
		// run of tool calls stays packed so ok/ERR scan as a column.
		if i > 0 && (!c.tool || !body[i-1].tool) {
			out = append(out, blank)
		}
		for line := range strings.SplitSeq(c.text, "\n") {
			if line == "" {
				out = append(out, blank)
				continue
			}
			// A line's own indentation folds into the wrap prefix, so
			// continuations line up under the content, not the margin — but a
			// pathological indent must not eat the width the content needs.
			trimmed := strings.TrimLeft(line, " ")
			lead := line[:len(line)-len(trimmed)]
			if maxLead := opts.width - bodyIndent - 20; len(lead) > maxLead {
				lead = lead[:max(0, maxLead)]
			}
			out = append(out, wrap(pad+lead, paint.paint(trimmed, c.color), opts.width)...)
		}
	}
	return out
}

// The flag on a banner says what that banner is doing with the session named
// on it: planting it, joining one already under way, or coming back to one the
// transcript has shown before — or, in a --follow tail, bringing in what is
// older than something already printed above it. Text-presentation code
// points, not emoji: one cell each, and they take the session's colour rather
// than the font's.
const (
	newFlag  = "⚑"
	joinFlag = "⚐"
	backFlag = "↻"
	lateFlag = "◷"
)

// The brackets around a banner's tag. Angle bracket ornaments rather than the
// ASCII pair: a transcript is full of ASCII brackets it did not write — a
// bridge's prefix, a title, a line of code quoted back — and a glyph no body
// text is going to contain says the tag is the transcript's own furniture
// before it is read. They point at what they hold, which suits a tag that is
// the one thing on the banner naming the session. One cell each, like every
// other glyph the layout budgets for.
const (
	tagOpen  = "❬"
	tagClose = "❭"
)

// bannerFlag picks the flag a banner opens with. Two different things put a
// session's beginning out of view and they are not answered the same way: a
// session the window cut into — above is set when a block of it was left out
// as older than --since — has the rest of itself outside the window, and
// widening --since is what fetches it; a session the stream left and came back
// to has the rest of itself further up this same output, and scrolling is what
// fetches it. A flag each, and the plain one for a session that starts where
// it says it does, leaves nothing to be inferred from a tag the reader would
// first have to remember having seen.
//
// What the window cut is read off the blocks it left out, not off the
// session's start: a session is created when its tab opens, possibly long
// before its first prompt, and a fork after the history it copies. With --all
// nothing is cut and every session begins in view.
//
// A late banner says the most: what follows it belongs further up, among what
// was printed before it — a subagent that ran in the background, a step held
// while other sessions went on, a session forked with its old stamps. It wins
// over the rest.
func bannerFlag(above, seen, late bool) string {
	switch {
	case late:
		return lateFlag
	case seen:
		return backFlag
	case above:
		return joinFlag
	}
	return newFlag
}

// sessionBanner names the session a run of blocks belongs to.
//
// Drawn on every change of session rather than on every message, and it is the
// only thing that says which session a block came from — neither indentation
// nor colour carries that.
func sessionBanner(s *session, flag string, paint Paint, width int) []string {
	// The id is bracketed. It is an identifier rather than a word — six random
	// characters, sometimes with an arrow hung off them — and the brackets say
	// so before it is read, the way they do around a token in prose. They also close the tag on its right, which is what lets a title
	// sit one space away without the two reading as one phrase.
	tag := flag + " " + tagOpen + s.tag + tagClose
	// The banner line is inset one cell — the rule runs the full width, and a
	// flag hard against column zero reads as that rule's end-cap rather than as
	// the first thing on the line. One cell for that inset, one for the gap
	// after the tag.
	room := max(1, width-cells(tag)-2)
	// The directory the session ran in, out at the right margin. A title says
	// what the work was and the path says what it was done to, which is the
	// half a title routinely leaves out — two sessions can carry the same
	// generated title, and under --everywhere a transcript runs across
	// projects, where the path is the only thing on the line telling them
	// apart.
	//
	// Whole, and not cut down to --root the way the session table's column is.
	// The table is read in the terminal that ran the command, with a summary
	// line above it naming the root; a transcript is routinely redirected to a
	// file and read somewhere else entirely, where `sub/` names nothing and
	// `.` names less. The one shortening it keeps is ~ for home, which is
	// unambiguous wherever the file is read.
	//
	// The title is served first and the path takes what is left: a path cut
	// from the left still names the directory it ends in, while a title cut
	// anywhere loses its subject. So the title keeps up to titleFloor cells —
	// the same floor the session table holds for it — before the path gets
	// any, and the pair are held two cells apart, which is padTo's own floor.
	//
	// What is left then has to be worth spending: a path squeezed to a couple
	// of cells is an ellipsis and one letter, noise at the margin rather than
	// an answer, so below whereFloor the path is dropped and the title takes
	// the width back. Like the table's floors this one is not owed — a path
	// short enough to fit whole is drawn whole, however little room there is.
	title, where := titleOf(s), neutraliseLine(homeTilde(s.directory))
	w := max(0, room-min(cells(title), titleFloor)-2)
	if w < min(cells(where), whereFloor) {
		w = 0
	}
	w = min(w, cells(where))
	if w == 0 {
		title, where = truncateCells(title, room), ""
	} else {
		title, where = truncateCells(title, room-w-2), tailCells(where, w)
	}
	// The rule under the tag is the envelope the rail used to draw: one line
	// clear across the width, saying everything below it belongs to this
	// session until the next rule. Unlike the rail it costs no column, and it
	// is drawn, not coloured, so --color never keeps it whole.
	//
	// Its weight says depth. A subagent's run is work some other session asked
	// for, so its rule is dashed where a top-level session's is solid — the
	// subordination reads off the line itself, at the width of the screen,
	// rather than off the ↓ hiding at the end of a tag.
	glyph := "─"
	if s.sub {
		glyph = "┈"
	}
	// Tag, title and rule wear the session's own hue rather than the frame blue
	// a turn head takes, so the banner reads as the thing the heads under it
	// sit inside rather than as another one of them — the tag and the rule
	// solid, the title a lighter step, one object across the line. One hue for
	// every banner: it says a line is a banner, and the tag beside it says
	// which.
	//
	// The rule is measured, not counted: under East Asian width a box-drawing
	// glyph takes two cells, and width of them would be a rule twice the width.
	// An odd cell over is left blank.
	rule := paint.paint(strings.Repeat(glyph, max(1, width)/cells(glyph)), sessionColor)
	body := paint.paint(title, sessionTitle)
	if where != "" {
		body = padTo(body, paint.paint(where, sessionWhere), room)
	}
	// Two lines and no blanks. The air a banner needs on both sides to read as
	// one object is sessionGap above and bannerGap below, and both belong to
	// whoever is laying banners down rather than to the banner.
	return []string{
		" " + paint.paint(tag, sessionColor, true) + " " + body,
		rule,
	}
}

func titleOf(s *session) string {
	if s.title == "" {
		return "(untitled)"
	}
	return neutraliseLine(s.title)
}
