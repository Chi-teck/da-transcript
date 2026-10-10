package app

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/clipperhouse/uax29/v2/graphemes"
	"github.com/mattn/go-runewidth"
)

// ---------------------------------------------------------------- measuring

// cells is the number of terminal columns a string occupies. A code-point
// count is wrong for CJK and emoji, so every width decision goes through this,
// or through clusterCells a grapheme cluster at a time where a string has to be
// cut — never a rune at a time, or a cut would land inside an emoji.
func cells(s string) int {
	n := 0
	for g := graphemes.FromString(s); g.Next(); {
		n += clusterCells(g.Value())
	}
	return n
}

// clusterCells is the width of one grapheme cluster: what its characters take
// one by one, the way a terminal that draws a code point at a time lays them
// down (see runeCells). A letter and its accents take the letter's cells, an
// Indic conjunct both its consonants' and a spacing vowel sign one of its own,
// a Hangul syllable spelled in jamo its leading consonant's two, a flag's pair
// of regional indicators two and a subdivision flag its black flag's two.
//
// Where an emoji is drawn as one picture, the picture's two cells win. A ZWJ
// sequence that joins a picture on is one picture, however many characters
// spell it. A skin tone is drawn inside the symbol before it, which it makes
// two cells. VS16 (U+FE0F) asks for the two-cell picture of the narrow
// character before it, if that has one; a space does not, and a space with a
// stray VS16 after it — a body can begin with one, and a prefix ends in a
// space — stays the one cell. Each rule looks no further back than the
// character before, so a run of tones is not one picture either.
func clusterCells(g string) int {
	if _, joined, zwj := strings.Cut(g, "\u200d"); zwj && strings.IndexFunc(joined, unicode.IsSymbol) == 0 {
		return 2
	}
	w, prev := 0, rune(-1)
	for _, r := range g {
		switch {
		case r == '\ufe0f' && runeCells(prev) == 1 && picture(prev):
			w++
		case skinTone(r) && prev > unicode.MaxASCII && unicode.IsSymbol(prev) && !skinTone(prev):
			w += 2 - runeCells(prev)
		default:
			w += runeCells(r)
		}
		prev = r
	}
	return w
}

// runeCells is the width of one character on its own: go-runewidth's, but
// nothing for a combining mark, a Hangul vowel or final consonant jamo, or a
// tag, each drawn inside what comes before it. go-runewidth leaves most
// combining marks at a cell, Hebrew's points and the Indic viramas among them,
// because it measures a cluster by its first character and never reaches them.
func runeCells(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me),
		'\u1160' <= r && r <= '\u11ff', '\ud7b0' <= r && r <= '\ud7ff', // Hangul vowels and finals
		'\U000E0020' <= r && r <= '\U000E007F': // tags
		return 0
	}
	return runewidth.RuneWidth(r)
}

// picture reports a character with an emoji picture for VS16 to ask for. Every
// one is a symbol or punctuation outside ASCII, but for ℹ and a keycap's
// digit, # or *.
func picture(r rune) bool {
	return r > unicode.MaxASCII && (unicode.IsSymbol(r) || unicode.IsPunct(r) || r == 'ℹ') || strings.ContainsRune("#*0123456789", r)
}

// skinTone reports an emoji modifier, one of the five skin tones.
func skinTone(r rune) bool {
	return '\U0001F3FB' <= r && r <= '\U0001F3FF'
}

// ljust pads on the right to w cells, like str.ljust.
func ljust(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-cells(s)))
}

// rjust pads on the left to w cells, for the columns holding numbers: a figure
// is read from its right-hand end, so a column of them has to line up there.
func rjust(s string, w int) string {
	return strings.Repeat(" ", max(0, w-cells(s))) + s
}

// truncateCells cuts s to at most limit cells, ending in an ellipsis when
// anything was dropped — value[:limit-1] + "…", measured in cells.
func truncateCells(s string, limit int) string {
	if cells(s) <= limit {
		return s
	}
	room, cut := limit-cells("…"), 0
	for g := graphemes.FromString(s); g.Next(); cut = g.End() {
		if room -= clusterCells(g.Value()); room < 0 {
			break
		}
	}
	return s[:cut] + "…"
}

// tailCells keeps the last limit cells of s behind an ellipsis. The ellipsis
// is measured too: under East Asian width it is two cells, not one. A wide
// cluster the cut lands in goes, and a space holds its place.
func tailCells(s string, limit int) string {
	if cells(s) <= limit {
		return s
	}
	drop := cells(s) - (limit - cells("…"))
	for g := graphemes.FromString(s); g.Next(); {
		if drop -= clusterCells(g.Value()); drop <= 0 {
			return "…" + strings.Repeat(" ", -drop) + s[g.End():]
		}
	}
	return "…"
}

// Two questions about an escape sequence, and they are not the same one.
// sgrSeq asks which sequences are colour spans this program wrote — the ones
// wrapLine can close at a break and reopen after the prefix, and the ones a
// --color always run has to be able to strip back to what --color never
// printed. escSeq asks which byte runs the terminal will advance zero columns
// for, which is all of them, whoever wrote them. Text from the store goes
// through neutralise before it is laid out, so the layout only meets this
// program's own colour now; it still measures a foreign escape at nothing,
// so that one that slipped past would not budget a line it cannot fit.
var (
	sgrSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")
	escSeq = regexp.MustCompile(
		"\x1b\\[[0-?]*[ -/]*[@-~]" + // CSI: colour, cursor moves, erases
			"|\x1b\\][^\x07\x1b\n]*(?:\x07|\x1b\\\\)" + // OSC: titles, hyperlinks
			"|\x1b[\\x30-\\x5a\\x5c-\\x7e]") // two-character escapes
)

// steerSeq is the part of escSeq that neutralise deletes: the sequences that
// only steer the terminal. Not among them are the escapes that open a string —
// ESC ] (OSC), ESC P (DCS), ESC X (SOS), ESC ^ (PM), ESC _ (APC) — and the
// ESC \ that ends one, because the string is text: a window title, a
// hyperlink's address, whatever a fetched page chose to put there.
var steerSeq = regexp.MustCompile(
	"\x1b\\[[0-?]*[ -/]*[@-~]" + // CSI: colour, cursor moves, erases
		"|\x1b[\\x30-\\x4f\\x51-\\x57\\x59\\x5a\\x60-\\x7e]") // two-character escapes

// stripSGR drops the colour sequences this program writes, and only those.
// Anything the transcript is quoting is left where it was: what this answers
// is what the same output would have looked like with colour off.
func stripSGR(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return sgrSeq.ReplaceAllString(s, "")
}

// stripEscapes drops every escape sequence, leaving what the terminal actually
// shows.
func stripEscapes(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return escSeq.ReplaceAllString(s, "")
}

// neutralise makes text from the store harmless to print. A shell call's
// output carries colour, cursor moves, screen clears and hyperlinks, and a
// reply can quote a file's raw bytes; printed as they came, they would act on
// the reader's terminal instead of being read. A sequence that only steers the
// terminal goes and the text around it stays. One that opens a string stays
// too, shown rather than obeyed: the string is text the model read, and
// deleting it up to its terminator would hide that, and every line before the
// terminator with it. A CRLF becomes the newline it means, and every other
// control character but tab and newline shows as its control picture (␀, ␍,
// ␛, ␇, ␡ …) — C1 ones, and bytes that are not UTF-8, as U+FFFD — which is
// also how a string's ESC and BEL show: a hyperlink reads
// ␛]8;;https://…␛\link␛]8;;␛\. Each of those is one cell, like the character
// it stands for would have had to be. A format character that draws nothing
// shows as its code point instead, <U+202E>: see showFormat.
//
// A tab goes last, as spaces: see expandTabs. Of everything here it is the one
// change to text that was harmless already, and it is made because the layout
// would otherwise be measuring a line the terminal does not draw.
func neutralise(s string) string {
	if strings.IndexFunc(s, isControl) < 0 && utf8.ValidString(s) {
		return expandTabs(showFormat(s))
	}
	s = strings.ReplaceAll(steerSeq.ReplaceAllString(s, ""), "\r\n", "\n")
	return expandTabs(showFormat(strings.Map(func(r rune) rune {
		switch {
		case !isControl(r):
			return r
		case r < 0x20:
			return 0x2400 + r // the Control Pictures block, in C0 order
		case r == 0x7f:
			return '␡'
		}
		return utf8.RuneError
	}, s)))
}

// showFormat writes each format character that draws nothing out as its code
// point, <U+202E>. The reader cannot see one is there and the model read every
// one: a right-to-left override that makes a command read differently on a
// terminal that does bidi, as Konsole can, a zero-width space inside a word,
// a run of tag characters spelling out an instruction nobody sees. The width
// tables cannot agree with the terminal about them either, counting some a
// cell the terminal never draws. The code point is ASCII and measures what it
// shows, so this runs before the tabs are set.
//
// Two kinds stay where text needs them. A joiner, ZWNJ or ZWJ, between two
// letters, marks or symbols outside ASCII is how a Persian word keeps two
// letters apart, an Indic conjunct forms, an emoji family or a rainbow flag is
// put together; alone, or beside ASCII, it is shown. And tag characters stay
// as the tail of a subdivision flag — 🏴, three to seven tag letters and
// digits, the cancel tag — which is Scotland's, England's and Wales's; stray
// ones, a run too long or one left open are shown. Arabic's number signs and
// the other prepended concatenation marks are format characters that do draw,
// so they are not this function's business at all.
func showFormat(s string) string {
	if strings.IndexFunc(s, isFormat) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == '🏴' {
			n += flagTags(s[i+n:])
		}
		if isFormat(r) && !joins(s[:i], r, s[i+n:]) {
			fmt.Fprintf(&b, "<%U>", r)
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// joins reports a joiner between two characters it can join: letters, marks or
// symbols, none of them ASCII. U+FFFD is what a missing neighbour decodes to.
func joins(before string, r rune, after string) bool {
	joinable := func(r rune) bool {
		return r > 0x7f && r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsSymbol(r))
	}
	p, _ := utf8.DecodeLastRuneInString(before)
	q, _ := utf8.DecodeRuneInString(after)
	return (r == '\u200c' || r == '\u200d') && joinable(p) && joinable(q)
}

// flagTags is how many bytes at the start of s are the tags of a subdivision
// flag, its 🏴 just read, or 0 if they are not: three to seven tag letters and
// digits, as long as a subdivision's code can be, then the cancel tag.
func flagTags(s string) int {
	n := 0
	for i, r := range s {
		switch {
		case r == '\U000E007F' && n >= 3:
			return i + utf8.RuneLen(r)
		case n < 7 && ('\U000E0030' <= r && r <= '\U000E0039' || '\U000E0061' <= r && r <= '\U000E007A'):
			n++
		default:
			return 0
		}
	}
	return 0
}

// tabStop is how far apart a terminal sets its tab stops by default.
const tabStop = 8

// expandTabs swaps each tab for the spaces that take it to the next tab stop,
// counting from the start of its line in the text, not from wherever the
// transcript puts that line. cells() and wrap() measure a tab at nothing,
// where the terminal moves it up to eight columns, so a tab left in would run
// a line past the margin, the terminal would fold it back to column 0, and
// the rail beside it would break. Columns are counted relative to the text
// because the alignment the tabs were there for is the text's own: a test
// failure indented under its name, go test's package and timing columns.
//
// It runs on text that is already neutralised, so a control picture counts
// the cell it shows in and a deleted escape counts nothing.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for i, seg := range strings.Split(s, "\t") {
		if i > 0 {
			n := tabStop - col%tabStop
			b.WriteString(strings.Repeat(" ", n))
			col += n
		}
		b.WriteString(seg)
		if nl := strings.LastIndexByte(seg, '\n'); nl >= 0 {
			col = cells(seg[nl+1:])
		} else {
			col += cells(seg)
		}
	}
	return b.String()
}

// neutraliseLine is neutralise for a field printed as one line — a session's
// title and directory, a name in a warning. A body keeps its newlines because
// wrap() lays them out, and its tabs as the spaces they stood for; a field
// like these sits on a line of furniture nothing re-lays, where a newline
// would end that line and let the field draw the next one: a second banner,
// half a table row, a warning nobody gave. So the two show as their control
// pictures as well, ␉ and ␊, and the reader can see the field held them. The
// tab is swapped before neutralise can expand it, the newline after, once a
// CRLF has become one.
func neutraliseLine(s string) string {
	return strings.ReplaceAll(neutralise(strings.ReplaceAll(s, "\t", "␉")), "\n", "␊")
}

// isControl reports a C0 or C1 control character or DEL, tab and newline
// apart.
func isControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || (r >= 0x7f && r <= 0x9f)
}

// isFormat reports a format character (Cf) that draws nothing: the bidi
// controls, the zero-width ones, the soft hyphen, the tags and the rest.
// Missing are the prepended concatenation marks, Arabic's number sign among
// them, which draw a sign over the digits after them.
func isFormat(r rune) bool {
	return unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Prepended_Concatenation_Mark, r)
}

// visibleCells is cells() for a string that may carry escapes: none of them
// take a column.
//
// Every escape and not just our own, because the two ways one gets into a line
// fail the same way. A line this program painted is measured after the fact by
// wrap() and padTo(), and a line quoting text from the store could carry
// escapes of its own if neutralise ever let one through. Counting either as
// text budgets a line against a width it does not have and pushes it past the
// margin — which is the one thing TestWidthInvariant is here to catch.
//
// Reach for it only where painted text is unavoidable — wrap() and padTo() take
// what they are given, so they have to. Everywhere else the rule is to measure
// before painting and never after: renderTool budgets its margin against the
// bare name, summary and outcome and paints them once the widths are settled,
// and the session table measures its columns before a hue is anywhere near
// them. Kept that way, cells() is always the right question and the two never
// have to be told apart at a call site.
func visibleCells(s string) int {
	return cells(stripEscapes(s))
}

// padTo sets left at the margin and right against the far one, in a field
// width cells across, never letting the two touch: two cells between them is
// the floor, and a field too narrow to hold both grows rather than overlapping
// them. Widths are visible cells, so either side may already be painted.
//
// This is the only way anything reaches the right margin. A tool call's
// outcome does it, and so does anything else that wants a value out there —
// which is the point of having it: the alternative is each caller subtracting
// its own indent and its own gap from the width and getting a different answer.
func padTo(left, right string, width int) string {
	return left + strings.Repeat(" ", max(2, width-visibleCells(left)-visibleCells(right))) + right
}

// ---------------------------------------------------------------- tables

// The session table budgets its width rather than measuring it: every column
// but the flexible pair keeps its natural width, and `where` and `title` split
// what is left. `where` yields before `title` — a truncated path is still
// recognisable from its tail, while a truncated title loses its subject.
const (
	tableGutter = 2    // cells between columns
	whereFloor  = 12   // `where` holds this while `title` still fits
	titleFloor  = 14   // `title` holds this while any room remains
	whereShare  = 0.45 // `where`'s bid on the flexible room
)

// budget returns per-column widths for a table with two flexible columns, named
// by index rather than by position: the two are not neighbours in every table
// that has them. Fixed columns take their natural width; the flexible pair
// splits the remainder and truncates.
func budget(width int, naturals []int, whereIdx, titleIdx int) []int {
	widths := append([]int(nil), naturals...)
	fixed := 0
	for i, w := range widths {
		if i != whereIdx && i != titleIdx {
			fixed += w + tableGutter
		}
	}
	room := width - fixed - tableGutter // the gutter between the flexible pair
	wNat, tNat := widths[whereIdx], widths[titleIdx]
	if wNat+tNat <= room {
		return widths
	}
	// A floor is what a column may not be squeezed below, not what it is owed:
	// both are clamped back to the natural width, so a `where` column holding
	// nothing but "." stays one cell wide instead of taking twelve and a
	// `title` never rules past its longest title.
	w := min(wNat, max(whereFloor, int(float64(room)*whereShare)))
	t := min(tNat, max(titleFloor, room-w))
	// Room the titles turned out not to need goes back to `where` rather than
	// off the end of the row, so a run of long paths beside short titles is
	// cut at the margin and not at 45% of it.
	w = min(wNat, max(w, room-t))
	if w+t > room { // the floors collide: where yields first, down to one cell
		w = max(1, room-t)
	}
	if w+t > room { // still over: title yields too
		t = max(1, room-w)
	}
	widths[whereIdx], widths[titleIdx] = w, t
	return widths
}

const (
	// A framed row: an edge either side and a vertical between every pair of
	// columns, each with a space between it and the cell it touches, so nothing
	// is ever hard against a rule.
	frameLeft   = "│ "
	frameGutter = " │ "
	frameRight  = " │"
)

// frameCost is what a frame takes out of the width before the columns are
// budgeted. budget() strikes its widths for a bare table — two-space gutters,
// nothing at either end — and a frame widens each of the cols-1 gutters and
// puts an edge at each end: a cell per gutter and two per edge while a
// vertical is one cell, more where East Asian width makes it two. So it is
// measured off the frame's own pieces rather than counted.
func frameCost(cols int) int {
	return (cols-1)*(cells(frameGutter)-tableGutter) + cells(frameLeft) + cells(frameRight)
}

// framedTable lays heads and rows out on the given widths in a box: cells
// padded right, the columns divided by verticals, and three horizontals — over
// the heads, under them, and under the last row — meeting the verticals at
// corners and junctions. Furniture end to end, so it all goes in the quietest
// grey there is when colour is on, which is also what lets the transcript's
// key sit above the session rules without competing with them.
//
// It is what --list prints and what the transcript opens on: the rules carry
// the eye across a row, and the box says where the table stops without the
// reader having to work it out from the alignment.
//
// Cells must already be cut to their column's width; the clamp against
// `width` only guards the invariant, for the budgets too narrow to strike one
// that fits. The frame is not free either — see frameCost, which the caller
// has to have taken out of the budget before striking the widths.
//
// The clamp is why a row is assembled twice. A row is measured before it is
// painted — the rule the whole file keeps — so it is put together bare, and
// only once the width is known to hold is it put together again with the paint
// on. A row that overflows is cut instead, and gives up its colour with the
// cells it loses; nothing else here can overflow, so that is a pathologically
// narrow terminal or nothing.
func framedTable(heads []string, rows [][]string, widths []int, width int, paint Paint) []string {
	// lit paints the verticals in a piece of furniture and leaves the spaces
	// flanking them alone: a span around a blank cell shows nothing and says
	// nothing. Cells never go through here, so a title that happens to contain
	// a box-drawing character is left as the session named it.
	lit := func(s string) string {
		return strings.ReplaceAll(s, "│", paint.paint("│", dim))
	}
	line := func(cs []string, color int, bold bool) string {
		// An empty last cell is padded out like any other: the right edge has
		// to be out there whether the last cell has anything in it or not.
		parts := make([]string, len(cs))
		for i, c := range cs {
			parts[i] = ljust(truncateCells(c, widths[i]), widths[i])
		}
		if bare := frameLeft + strings.Join(parts, frameGutter) + frameRight; cells(bare) > width {
			return truncateCells(strings.TrimRight(bare, " "), width)
		}
		if color != noColor {
			for i, p := range parts {
				// The word is painted and the padding after it is not, for the
				// reason lit() leaves the spaces alone.
				word := strings.TrimRight(p, " ")
				parts[i] = paint.paint(word, color, bold) + p[len(word):]
			}
		}
		return lit(frameLeft) + strings.Join(parts, lit(frameGutter)) + lit(frameRight)
	}
	rule := func(left, mid, right string) string {
		return tableRule(widths, width, left, mid, right, paint)
	}
	out := make([]string, 0, len(rows)+4)
	out = append(out, rule("┌", "┬", "┐"))
	out = append(out, line(heads, headColor, headBold))
	out = append(out, rule("├", "┼", "┤"))
	for _, r := range rows {
		// Cells stay in the terminal's own foreground, the way message text
		// does: a hue on a tag or a path would be making a claim about that
		// column that is not true.
		out = append(out, line(r, noColor, false))
	}
	return append(out, rule("└", "┴", "┘"))
}

// tableRule is one horizontal of a frame. Each segment spans its column and
// the space either side of it, so the junctions land exactly on the verticals
// of the rows above and below and the rule comes out the same width as one.
// Which junctions those are is the whole difference between the three: corners
// at the top and bottom, tees where a vertical starts or stops, a cross where
// it carries on through. Being furniture end to end it is painted whole, after
// the same measure-then-paint as a row.
//
// A segment is measured, not counted: under East Asian width a box-drawing
// glyph takes two cells, the verticals as well as the bar, and a segment of
// w+2 bars would run twice its column. Where the span is odd the cell over is
// left blank, so the junctions still land on the verticals.
func tableRule(widths []int, width int, left, mid, right string, paint Paint) string {
	bar := cells("─")
	segs := make([]string, len(widths))
	for i, w := range widths {
		segs[i] = strings.Repeat("─", (w+2)/bar) + strings.Repeat(" ", (w+2)%bar)
	}
	rule := left + strings.Join(segs, mid) + right
	if cells(rule) > width {
		return truncateCells(rule, width)
	}
	return paint.paint(rule, dim)
}

// ---------------------------------------------------------------- wrapping

// wrap breaks text into lines of at most width cells, prefix included, and
// re-emits prefix on every line — the indent survives wrapping. Newlines in
// text are hard breaks. SGR sequences take no room and are never split, and
// a colour span open where a line breaks is closed there and reopened after
// the prefix, so colour survives too.
func wrap(prefix, text string, width int) []string {
	room := max(1, width-visibleCells(prefix))
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		out = append(out, wrapLine(prefix, line, room)...)
	}
	return out
}

// trimEnd drops a line's trailing spaces, the ones a colour span closes over
// included: a painted line ends in a reset, and a plain TrimRight stops there
// and keeps a space the uncoloured line does not have.
func trimEnd(s string) string {
	s = strings.TrimRight(s, " ")
	if rest, ok := strings.CutSuffix(s, reset); ok {
		return trimEnd(rest) + reset
	}
	return s
}

// wrapLine wraps one newline-free line into pieces of at most room cells,
// breaking at spaces where it can and mid-word where it must.
func wrapLine(prefix, line string, room int) []string {
	if visibleCells(line) <= room {
		return []string{trimEnd(prefix + line)}
	}
	var out []string
	emit := func(open, content, closing string) {
		content = strings.TrimRight(content, " ")
		if content != "" {
			content = open + content + closing
		}
		out = append(out, trimEnd(prefix+content))
	}
	var cur []byte
	curW := 0
	active := "" // the SGR span in force at the write position
	open := ""   // the span to reopen at the start of the current piece
	// The cut is the last gap seen: emitting keeps cur[:cutB] on this line
	// and resumes at cur[contB:], where the span state is contActive.
	cutB, cutW, cutActive := -1, 0, ""
	contB, contW, contActive := -1, 0, ""
	inGap := false
	cutEnd := 0 // the end of a cluster too wide for the room, while it is cut

	for i := 0; i < len(line); {
		if line[i] == '\x1b' {
			if loc := escSeq.FindStringIndex(line[i:]); loc != nil && loc[0] == 0 {
				seq := line[i : i+loc[1]]
				// Every escape passes through at no width, but only a colour
				// span is remembered as the one to close at a break and reopen
				// on the next line. Reopening a cursor move or a screen clear
				// would be re-running it, not restoring it — so a sequence the
				// transcript is merely quoting rides along where it was written
				// and says nothing about the lines after it.
				switch {
				case seq == reset:
					active = ""
				case sgrSeq.MatchString(seq):
					active = seq
				case inGap:
					// Breaking at a gap throws the gap away, which is fine for
					// spaces and not for this: a span would come back from
					// `open` and this has nothing to come back from, so it
					// would be bytes the transcript was given and did not
					// print. The gap stops being a place to break instead.
					cutB = -1
				}
				cur = append(cur, seq...)
				i += loc[1]
				continue
			}
		}
		// A grapheme cluster at a time, never a rune: a family of emoji or a
		// flag cut in two would come out as its pieces, on two lines. No cluster
		// holds an escape — ESC is a control, and a control stands alone.
		//
		// The one cluster cut inside is one wider than the room. That is wider
		// than any picture, so it is characters a terminal draws one by one —
		// prepended marks, a conjunct or a run of spacing signs strung out past
		// any word — and it goes a code point at a time, each counted at the
		// most it can add once the line is measured whole: a VS16 the cell it
		// gives a picture.
		c := graphemes.FromString(line[i:]).First()
		w := clusterCells(c)
		if i >= cutEnd && w > max(room, 2) {
			cutEnd = i + len(c)
		}
		if i < cutEnd {
			r, size := utf8.DecodeRuneInString(c)
			c, w = c[:size], runeCells(r)
			if r == '\ufe0f' {
				w = 1
			}
		}
		if c == " " {
			if !inGap {
				inGap, cutB, cutW, cutActive = true, len(cur), curW, active
				contB = -1
			}
		} else {
			if inGap {
				inGap, contB, contW, contActive = false, len(cur), curW, active
			}
			// A loop for the cluster wider than two cells: the word carried
			// over a break at a gap may still leave no room for it, and then
			// the word goes on a line of its own too.
			for curW+w > room && curW > 0 {
				if cutB > 0 && cutW > 0 && contB >= cutB {
					closing := ""
					if cutActive != "" {
						closing = reset
					}
					emit(open, string(cur[:cutB]), closing)
					cur = append([]byte(nil), cur[contB:]...)
					curW -= contW
					open = contActive
				} else {
					closing := ""
					if active != "" {
						closing = reset
					}
					emit(open, string(cur), closing)
					cur, curW, open = nil, 0, active
				}
				cutB, contB = -1, -1
			}
		}
		cur = append(cur, c...)
		curW += w
		i += len(c)
	}
	emit(open, string(cur), "")
	return out
}
