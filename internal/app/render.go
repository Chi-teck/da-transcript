package app

import (
	"cmp"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

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
	return "  " + tag + " " + paint.paint(strings.TrimSpace(body), argColor)
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
	return paint.paint("attachment", grey) + " " + paint.paint(cmp.Or(name, "?"), argColor) +
		" " + paint.paint("("+cmp.Or(mime, "?")+")", dim)
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
	if reason := string(data.FinishReason); reason != "" {
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
// What a block shows is its head item. The tool results riding with a step
// draw nothing yet, and nor do a result no call answers, a subagent marker or
// an evaluator's call.
func renderBlock(b block, opts *options) []string {
	it := b.head
	data := &it.data
	paint := opts.paint
	var body []chunk
	add := func(text string) { body = append(body, newChunk(text)) }
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
			switch text := string(data.Content); {
			case strings.Contains(text, "\x00"):
				add(paint.paint(fmt.Sprintf("not shown: binary data (%d bytes)", len(text)), dim))
			case strings.TrimSpace(text) != "":
				add(strings.TrimRightFunc(text, unicode.IsSpace))
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
					// out like an image's bytes.
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
			if text := string(data.ReasoningContent); opts.reasoning && strings.TrimSpace(text) != "" {
				// No label: the colour is the label. Reasoning is the one item whose
				// whole body is a single kind of thing, so a hue over all of it says
				// what a word in front of the first line said, without competing with
				// the prose for the eye.
				c := newChunk(capRunes(strings.TrimRightFunc(text, unicode.IsSpace), opts.maxChars))
				c.color = reasonColor
				body = append(body, c)
			}
			if text := string(data.Content); strings.TrimSpace(text) != "" {
				add(strings.TrimRightFunc(text, unicode.IsSpace))
			}
			// A step cut off gets no line: docker-agent writes its usage when the
			// step finishes, with the finish reason.
			if opts.stats && data.FinishReason != "" {
				add(renderStats(data, paint))
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
		add(indentBlock("error", errorText(string(data.Code), string(data.Message)), opts.maxChars, paint, errColor))

	case it.typ == "termination":
		add(paint.paint(cmp.Or(string(data.Message), string(data.Reason), "?"), dim))
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
// transcript has shown before. Text-presentation code points, not emoji: one
// cell each, and they take the session's colour rather than the font's.
const (
	newFlag  = "⚑"
	joinFlag = "⚐"
	backFlag = "↻"
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

// ongoing reports whether a session was already running when the window
// opened, so its banner announces a conversation joined part-way rather than
// one starting here. It is a fact about the session rather than about the
// output — with --all nothing predates the window and every session begins in
// view — and it is asked only of a session's first banner, since a later one
// has a nearer answer to give.
func ongoing(s *session, since *int64) bool {
	return since != nil && s.timeCreated < *since
}

// bannerFlag picks the flag a banner opens with. Two different things put a
// session's beginning out of view and they are not answered the same way: a
// session already running when the window opened has the rest of itself
// outside the window, and widening --since is what fetches it; a session the
// stream left and came back to has the rest of itself further up this same
// output, and scrolling is what fetches it. A flag each, and the plain one for
// a session that starts where it says it does, leaves nothing to be inferred
// from a tag the reader would first have to remember having seen.
func bannerFlag(s *session, since *int64, seen bool) string {
	switch {
	case seen:
		return backFlag
	case ongoing(s, since):
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
	title, where := titleOf(s), homeTilde(s.directory)
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
	rule := paint.paint(strings.Repeat(glyph, max(1, width)), sessionColor)
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
	return s.title
}
