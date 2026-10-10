package app

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/mattn/go-runewidth"
)

func TestBudget(t *testing.T) {
	// Three fixed columns: 13+2 + 12+2 + 11+2 = 42 cells spoken for, plus the
	// gutter between the two flexible ones.
	fixed := []int{13, 12, 11}
	nat := func(w, tt int) []int { return append(slices.Clone(fixed), w, tt) }
	total := func(ws []int) int {
		sum := tableGutter * (len(ws) - 1)
		for _, w := range ws {
			sum += w
		}
		return sum
	}

	// Natural fit: nothing is squeezed.
	if got := budget(100, nat(20, 30), 3, 4); !slices.Equal(got, nat(20, 30)) {
		t.Errorf("natural fit: %v", got)
	}
	// `where` squeezed: room is 80-42-2 = 36, so `where` gets 45%% of it and
	// `title` the rest.
	if got := budget(80, nat(120, 60), 3, 4); got[3] != 16 || got[4] != 20 {
		t.Errorf("squeezed: %v", got)
	}
	// Both at their floors: room 16 cannot hold 12+14, `where` yields first.
	if got := budget(60, nat(120, 60), 3, 4); got[3] != 2 || got[4] != 14 || total(got) > 60 {
		t.Errorf("floors: %v (total %d)", got, total(got))
	}
	// Zero room: no panic, both flexible columns keep at least one cell.
	if got := budget(30, nat(120, 60), 3, 4); got[3] < 1 || got[4] < 1 {
		t.Errorf("zero room: %v", got)
	}

	// The flexible pair is named, not positional. This is the --list table's own
	// shape: seven columns with `where` second and `title` last, five fixed ones
	// between them. Room is 90-56-2 = 32, `where` bids 45%% of it and the room
	// `title` does not need comes back — and the row lands on the width exactly.
	wide := []int{12, 40, 16, 4, 8, 6, 60}
	if got := budget(90, wide, 1, 6); got[1] != 14 || got[6] != 18 || total(got) != 90 {
		t.Errorf("named pair: %v (total %d)", got, total(got))
	}
}

func TestFramedTable(t *testing.T) {
	eastAsian(t, false)
	heads, rows, widths := []string{"A", "B"}, [][]string{{"x", "yy"}, {"zzz", ""}}, []int{3, 2}
	got := framedTable(heads, rows, widths, 40, Paint{})
	// A closed box: corners at the top and bottom, tees where a vertical
	// starts or stops, a cross where it carries on through. An empty last cell
	// is padded out rather than dropped — the right edge has to be out there.
	want := []string{
		"┌─────┬────┐",
		"│ A   │ B  │",
		"├─────┼────┤",
		"│ x   │ yy │",
		"│ zzz │    │",
		"└─────┴────┘",
	}
	if !slices.Equal(got, want) {
		t.Errorf("framedTable = %q, want %q", got, want)
	}
	// Every line is exactly as wide as every other — that is what makes the
	// junctions land on the verticals.
	for i, line := range got {
		if cells(line) != cells(got[0]) {
			t.Errorf("line %d is %d cells, want %d: %q", i, cells(line), cells(got[0]), line)
		}
	}

	// Painted, the furniture wears a hue and the cells do not, a head's hue
	// stops at its last letter, and nothing the paint added is measurable.
	lit := framedTable(heads, rows, widths, 40, Paint{enabled: true})
	pipe, rules, heading := fg(dim)+"│"+reset, fg(dim), fg(headColor, "1")
	if lit[0] != rules+"┌─────┬────┐"+reset {
		t.Errorf("painted top rule = %q", lit[0])
	}
	if lit[1] != pipe+" "+heading+"A"+reset+"   "+pipe+" "+heading+"B"+reset+"  "+pipe {
		t.Errorf("painted head = %q", lit[1])
	}
	if lit[2] != rules+"├─────┼────┤"+reset {
		t.Errorf("painted head rule = %q", lit[2])
	}
	if last := lit[len(lit)-1]; last != rules+"└─────┴────┘"+reset {
		t.Errorf("painted foot rule = %q", last)
	}
	for i := range lit {
		if visibleCells(lit[i]) != cells(got[i]) {
			t.Errorf("paint moved line %d: %d cells, want %d", i, visibleCells(lit[i]), cells(got[i]))
		}
	}

	// Too narrow to hold the budget: the rows are cut to the width, and give
	// up their colour rather than being measured with it.
	cut := framedTable(heads, rows, widths, 5, Paint{enabled: true})
	for i, line := range cut {
		if strings.Contains(line, "\x1b") {
			t.Errorf("clamped line %d kept colour: %q", i, line)
		}
		if cells(line) > 5 {
			t.Errorf("clamped line %d is %d cells: %q", i, cells(line), line)
		}
	}

	// Where box drawing is two cells a glyph, a rule is measured out rather
	// than counted: half the bars, the odd cell over left blank, so the
	// junctions still land on the verticals.
	eastAsian(t, true)
	wide := framedTable(heads, rows, widths, 40, Paint{})
	want = []string{
		"┌── ┬──┐",
		"│ A   │ B  │",
		"├── ┼──┤",
		"│ x   │ yy │",
		"│ zzz │    │",
		"└── ┴──┘",
	}
	if !slices.Equal(wide, want) {
		t.Errorf("east asian framedTable = %q, want %q", wide, want)
	}
	for i, line := range wide {
		if cells(line) != cells(wide[0]) {
			t.Errorf("east asian line %d is %d cells, want %d: %q", i, cells(line), cells(wide[0]), line)
		}
	}
}

func TestPadTo(t *testing.T) {
	if got := padTo("ab", "cd", 10); got != "ab      cd" {
		t.Errorf("padTo = %q", got)
	}
	// Both sides are measured by what they show, not by what they carry.
	red := "\x1b[38;5;203m"
	if got := padTo(red+"ab"+reset, "cd", 10); got != red+"ab"+reset+"      cd" {
		t.Errorf("painted padTo = %q", got)
	}
	// Too narrow to hold both: the field grows rather than overlapping them.
	if got := padTo("aaaa", "bbbb", 6); got != "aaaa  bbbb" {
		t.Errorf("floor = %q", got)
	}
}

// tailCells keeps a path's tail in the cells it is given, the ellipsis in
// front of it counted — as two where East Asian width makes it two.
func TestTailCells(t *testing.T) {
	eastAsian(t, false)
	if got := tailCells("/a/long/path", 8); got != "…ng/path" {
		t.Errorf("tailCells = %q", got)
	}
	eastAsian(t, true)
	if got := tailCells("/a/long/path", 8); got != "…g/path" {
		t.Errorf("east asian tailCells = %q (%d cells)", got, cells(got))
	}
}

// A grapheme cluster is measured whole, in the cells a terminal draws it in: a
// letter and its accents in the letter's, an emoji drawn as a picture in two
// however narrow its first character — a flag, VS16, a ZWJ sequence — a wide
// character in two, and an ambiguous one in two only under East Asian width.
func TestClusterCells(t *testing.T) {
	scotland := "🏴\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"
	for _, wide := range []bool{false, true} {
		eastAsian(t, wide)
		for _, c := range []struct {
			name         string
			s            string
			narrow, wide int // cells without and with East Asian width
		}{
			{"ascii", "abc", 3, 3},
			{"combining accents", "e\u0301\u0308", 1, 1},
			{"cjk", "日本語", 6, 6},
			{"ellipsis", "…", 1, 2},
			{"zwj family", "👨\u200d👩\u200d👧", 2, 2},
			{"flags", "🇰🇿🇰🇿🇰🇿", 6, 6},
			{"lone regional indicator", "🇺", 1, 1},
			{"vs16", "❤\ufe0f☺\ufe0f", 4, 4},
			{"vs15", "❤\ufe0e", 1, 1},
			{"vs16 on what has no picture", "a\ufe0f \ufe0f", 2, 2},
			{"keycap", "1\ufe0f\u20e3", 2, 2},
			{"vs16 on the one letter with a picture", "ℹ\ufe0f", 2, 2},
			{"rainbow flag", "🏳\ufe0f\u200d🌈", 2, 2},
			{"skin tone", "👍🏽", 2, 2},
			{"skin tone on a narrow base", "☝🏽", 2, 2},
			{"zwj sequence with a skin tone", "👩🏽\u200d💻", 2, 2},
			{"a run of skin tones", "👍🏽🏽", 4, 4},
			{"skin tone on a letter", "e🏽", 3, 3},
			{"scotland", scotland, 2, 2},
			{"conjunct", "क्ष", 2, 2},
			{"zwj in a conjunct", "क्\u200dष", 2, 2},
			{"spacing vowel sign", "का", 2, 2},
			{"hebrew points", "שָׁלוֹם", 4, 4},
			{"hangul jamo", "\u1100\u1161\u11a8", 2, 2},
			{"hangul syllable and final jamo", "가\u11a8", 2, 2},
			{"zwj joining nothing on", "ب\u200dا", 2, 2},
		} {
			want := c.narrow
			if wide {
				want = c.wide
			}
			if got := cells(c.s); got != want {
				t.Errorf("east asian %v: %s: cells(%q) = %d, want %d", wide, c.name, c.s, got, want)
			}
		}
	}
}

// Cutting and padding measure what cells() measures, and a cut falls between
// grapheme clusters, never inside one.
func TestCutClusters(t *testing.T) {
	fam := "👨\u200d👩\u200d👧"
	flags := "🇰🇿🇰🇿🇰🇿"
	for _, c := range []struct{ name, got, want string }{
		{"truncate keeps a family whole", truncateCells(fam+fam, 3), fam + "…"},
		{"truncate counts a flag two", truncateCells(flags, 5), "🇰🇿🇰🇿…"},
		{"tail keeps a family whole", tailCells(fam+fam, 3), "…" + fam},
		{"tail counts a flag two", tailCells(flags, 5), "…🇰🇿🇰🇿"},
		{"tail spaces out a wide cluster it cuts", tailCells("a"+fam+"bc", 4), "… bc"},
		{"ljust", ljust(flags, 8), flags + "  "},
		{"rjust", rjust("❤\ufe0f", 3), " ❤\ufe0f"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestWrapPrefix(t *testing.T) {
	// The indent prefix is re-emitted on every continuation line.
	got := wrap("    ", "aaa bbb ccc ddd", 12)
	want := []string{"    aaa bbb", "    ccc ddd"}
	if !slices.Equal(got, want) {
		t.Errorf("wrap = %q, want %q", got, want)
	}
	// A word too long for the room is broken mid-word rather than overflowing.
	got = wrap("  ", "abcdefghij", 7)
	want = []string{"  abcde", "  fghij"}
	if !slices.Equal(got, want) {
		t.Errorf("hard break = %q, want %q", got, want)
	}
	// A line that fits comes back byte-for-byte — internal spacing preserved.
	got = wrap("  ", "a  b   c", 40)
	if !slices.Equal(got, []string{"  a  b   c"}) {
		t.Errorf("fitting line = %q", got)
	}
}

func TestWrapCJK(t *testing.T) {
	// Double-width runes: two fill four cells, the third would cross five.
	got := wrap("", "日本語", 5)
	want := []string{"日本", "語"}
	if !slices.Equal(got, want) {
		t.Errorf("cjk = %q, want %q", got, want)
	}
	for _, line := range got {
		if cells(line) > 5 {
			t.Errorf("cjk line %q is %d cells", line, cells(line))
		}
	}
}

// wrap breaks between grapheme clusters and measures each the way cells()
// does: a family of emoji is not split over two lines, a flag is two cells and
// stays a flag, and a picture wider than the room goes on a line of its own. A
// cluster wider than the room and any picture is cut, a code point at a time.
func TestWrapClusters(t *testing.T) {
	fam := "👨\u200d👩\u200d👧"
	flag := "🇰🇿"
	heart := "❤\ufe0f"
	scotland := "🏴\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"
	for _, c := range []struct {
		name  string
		text  string
		width int
		want  []string
	}{
		{"zwj families", strings.Repeat(fam, 5), 5, []string{fam + fam, fam + fam, fam}},
		{"flags", strings.Repeat(flag, 5), 5, []string{flag + flag, flag + flag, flag}},
		{"vs16", strings.Repeat(heart, 3), 4, []string{heart + heart, heart}},
		{"scotland", scotland + " " + scotland + " " + scotland, 5, []string{scotland + " " + scotland, scotland}},
		{"combining accents", strings.Repeat("e\u0301", 3), 2, []string{"e\u0301e\u0301", "e\u0301"}},
		{"conjuncts", strings.Repeat("क्ष", 3), 4, []string{"क्षक्ष", "क्ष"}},
		{"conjunct wider than the room", "स्त्री", 3, []string{"स्त्र", "ी"}},
		{"word carried to a line with no room left", "a bस्त्र", 3, []string{"a", "b", "स्त्र"}},
		{"wider than the room", fam + fam, 1, []string{fam, fam}},
	} {
		if got := wrap("", c.text, c.width); !slices.Equal(got, c.want) {
			t.Errorf("%s: wrap = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWrapANSI(t *testing.T) {
	red := "\x1b[38;5;203m"
	got := wrap("| ", red+"aaa bbb"+reset, 7)
	// The span open at the break is closed there and reopened after the prefix.
	want := []string{"| " + red + "aaa" + reset, "| " + red + "bbb" + reset}
	if !slices.Equal(got, want) {
		t.Errorf("ansi = %q, want %q", got, want)
	}
	// Sequences are never split and never counted.
	for _, line := range got {
		if strings.Count(line, "\x1b") != strings.Count(stripSGR(line), "\x1b")+2 {
			t.Errorf("ansi line %q lost or split a sequence", line)
		}
		if visibleCells(line) > 7 {
			t.Errorf("ansi line %q is %d visible cells", line, visibleCells(line))
		}
	}
	// A space a span closes over is trimmed like any other trailing space, on
	// a line that fits and on the last piece of one that folds.
	for _, line := range []string{"aaa ", "aaa bbb ccc "} {
		plain := wrap("| ", line, 9)
		coloured := wrap("| ", red+line+reset, 9)
		for i := range coloured {
			coloured[i] = stripSGR(coloured[i])
		}
		if !slices.Equal(coloured, plain) {
			t.Errorf("wrap(%q) coloured = %q, plain = %q", line, coloured, plain)
		}
	}
}

// Text from the store is neutralised before it is laid out, but a line that
// carried escapes it did not write — a hyperlink from a fetch, a colour or a
// screen clear from a shell call — would still have to be measured at the
// nothing the terminal advances for them, or it would be budgeted against a
// width it does not have. sgrSeq answers a narrower question and is left
// answering it — what a --color always run added, and nothing else.
func TestVisibleCellsForeignEscapes(t *testing.T) {
	for _, c := range []struct {
		name string
		line string
		want int
	}{
		{"osc 8 hyperlink", "\x1b]8;;http://example.com/x\x07link\x1b]8;;\x07", 4},
		{"osc terminated by st", "\x1b]0;a title\x1b\\abc", 3},
		{"csi erase", "\x1b[2Jabc", 3},
		{"csi cursor move", "\x1b[10;20Habc", 3},
		{"csi private mode", "\x1b[?25labc", 3},
		{"two-character escape", "\x1bMabc", 3},
		{"our own colour", fg(grey) + "abc" + reset, 3},
		{"nothing to strip", "abc", 3},
	} {
		if got := visibleCells(c.line); got != c.want {
			t.Errorf("%s: visibleCells(%q) = %d, want %d", c.name, c.line, got, c.want)
		}
	}
	// stripSGR still answers only for the colour this program writes: an
	// escape the transcript is quoting is not ours to take back.
	quoted := "\x1b[2Jabc"
	if got := stripSGR(quoted); got != quoted {
		t.Errorf("stripSGR(%q) = %q, want it left alone", quoted, got)
	}
	if got := stripSGR(fg(grey) + "abc" + reset); got != "abc" {
		t.Errorf("stripSGR dropped the wrong thing: %q", got)
	}
}

// Wrapping folds on what is visible, and a sequence it did not write is
// carried where it stood rather than reopened on the next line — reopening a
// cursor move would be re-running it.
func TestWrapForeignEscapes(t *testing.T) {
	// Five cells of room for seven cells of text, so it has to fold — the
	// escape in front of it buys no room and must not be counted as any.
	clear := "\x1b[2J"
	got := wrap("", clear+"aaa bbb", 5)
	want := []string{clear + "aaa", "bbb"}
	if !slices.Equal(got, want) {
		t.Errorf("wrap = %q, want %q", got, want)
	}
	for _, line := range got {
		if visibleCells(line) > 5 {
			t.Errorf("line %q is %d visible cells", line, visibleCells(line))
		}
		if strings.Contains(strings.TrimPrefix(line, clear), "\x1b") {
			t.Errorf("line %q reopened a sequence that is not a colour span", line)
		}
	}
	// Nothing the transcript was given may be dropped on its way through the
	// wrapper. A colour span discarded with the spaces it sat among comes back
	// out of `open` on the next line; a sequence that cannot be reopened has
	// nothing to come back from, so the gap it sits in must stop being a place
	// to break rather than take it down with the spaces.
	for _, line := range []string{
		"aaa " + clear + " bbb",
		clear + "aaa " + clear + "bbb",
		"aaa " + clear + clear + " bbb",
	} {
		got := wrap("", line, 5)
		folded := strings.Join(got, "\n")
		if in, out := strings.Count(line, "\x1b"), strings.Count(folded, "\x1b"); out != in {
			t.Errorf("wrap(%q) kept %d of %d escapes: %q", line, out, in, folded)
		}
		// The cost of keeping them is a break the gap would have taken more
		// neatly, so the fold is allowed to differ from the bare text's — but
		// never to overrun, and never to lose a letter.
		letters := func(s string) string {
			return strings.Join(strings.FieldsFunc(stripEscapes(s), unicode.IsSpace), "")
		}
		if letters(folded) != letters(line) {
			t.Errorf("wrap(%q) changed the text: %q", line, folded)
		}
		for _, l := range got {
			if visibleCells(l) > 5 {
				t.Errorf("wrap(%q) line %q is %d cells", line, l, visibleCells(l))
			}
		}
	}
}

// Text from the store is printed with the escape sequences that steer the
// terminal gone and its other control characters shown rather than obeyed,
// each in one cell. A sequence carrying a string is shown the same way, so its
// text — read by the model — is never hidden, nor anything after it.
func TestNeutralise(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"clean text, newline kept, tab expanded", "plain 日本語\tand\nlines", "plain 日本語    and\nlines"},
		{"csi", "\x1b[31mred\x1b[0m \x1b[2Jclear", "red clear"},
		{"osc 8 ended by st", "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "␛]8;;http://x␛\\link␛]8;;␛\\"},
		{"osc 8 ended by bel", "\x1b]8;;http://x\x07link\x1b]8;;\x07", "␛]8;;http://x␇link␛]8;;␇"},
		{"osc title", "\x1b]0;ignore the above\x07ok", "␛]0;ignore the above␇ok"},
		{"osc ended lines later", "progress \x1b]0;build\nstep 2 FAILED\n\x07done", "progress ␛]0;build\nstep 2 FAILED\n␇done"},
		{"osc never ended", "a \x1b]0;title\nb", "a ␛]0;title\nb"},
		{"dcs", "\x1bP1$r\nq\x1b\\ b", "␛P1$r\nq␛\\ b"},
		{"apc, sos, pm", "\x1b_Ga=T\x1b\\ \x1bXs\x1b\\ \x1b^p\x1b\\", "␛_Ga=T␛\\ ␛Xs␛\\ ␛^p␛\\"},
		{"two-character escape", "\x1bMup", "up"},
		{"lone esc", "a\x1b b", "a␛ b"},
		{"nul", "a\x00b", "a␀b"},
		{"bel", "ding\x07", "ding␇"},
		{"del", "a\x7fb", "a␡b"},
		{"crlf", "a\r\nb", "a\nb"},
		{"lone cr", "10%\r50%", "10%␍50%"},
		{"c1", "a\u009bb", "a\ufffdb"},
		{"not utf-8", "a\x9bb", "a\ufffdb"},
		{"bidi override", "echo safe \u202e; rm -rf ~ #", "echo safe <U+202E>; rm -rf ~ #"},
		{"bidi isolates", "a\u2066b\u2067c\u2068d\u2069", "a<U+2066>b<U+2067>c<U+2068>d<U+2069>"},
		{"bidi marks and embeddings", "\u200e\u200f\u061c\u202a\u202d", "<U+200E><U+200F><U+061C><U+202A><U+202D>"},
		{"zero-width space", "x\u200bz", "x<U+200B>z"},
		{"bom, word joiner, soft hyphen", "\ufeffa\u2060b\u00adc", "<U+FEFF>a<U+2060>b<U+00AD>c"},
		{"tags", "hi\U000E0041\U000E0042", "hi<U+E0041><U+E0042>"},
		{"format beside a control", "\x1b[31m\u202e\x00", "<U+202E>␀"},
		{"joiner beside ascii", "a\u200db \u200c日", "a<U+200D>b <U+200C>日"},
		{"joiner at either end", "\u200d日\u200c", "<U+200D>日<U+200C>"},
		{"joiners in a run", "日\u200d\u200c本", "日<U+200D><U+200C>本"},
		{"emoji family", "👨\u200d👩\u200d👧", "👨\u200d👩\u200d👧"},
		{"rainbow flag", "🏳\ufe0f\u200d🌈", "🏳\ufe0f\u200d🌈"},
		{"skin tone", "👩🏽\u200d💻", "👩🏽\u200d💻"},
		{"zwnj in persian", "می\u200cخواهم", "می\u200cخواهم"},
		{"zwj in a conjunct", "क्\u200dष", "क्\u200dष"},
		{"scotland", "🏴\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F", "🏴\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"},
		{"flag tags too many", "🏴\U000E0061\U000E0062\U000E0063\U000E0064\U000E0065\U000E0066\U000E0067\U000E0068\U000E007F",
			"🏴<U+E0061><U+E0062><U+E0063><U+E0064><U+E0065><U+E0066><U+E0067><U+E0068><U+E007F>"},
		{"flag tags left open", "🏴\U000E0067\U000E0062\U000E0065 x", "🏴<U+E0067><U+E0062><U+E0065> x"},
		{"flag tags not lowercase", "🏴\U000E0047\U000E0042\U000E0045\U000E007F", "🏴<U+E0047><U+E0042><U+E0045><U+E007F>"},
		{"cancel tag alone", "a\U000E007F", "a<U+E007F>"},
		{"variation selectors", "☺\ufe0f ❤\ufe0e 葛\U000E0100", "☺\ufe0f ❤\ufe0e 葛\U000E0100"},
		{"arabic number sign", "\u0600١٢٣", "\u0600١٢٣"},
	} {
		got := neutralise(c.in)
		if got != c.want {
			t.Errorf("%s: neutralise(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	for _, r := range []string{"␀", "␇", "␍", "␛", "␡", "\ufffd"} {
		if cells(r) != 1 {
			t.Errorf("%q is %d cells, want 1", r, cells(r))
		}
	}
}

// A tab is expanded to the spaces that take it to the next 8-column stop,
// counted from the start of its own line in the cells the line shows in once
// neutralised: a wide rune counts two, a control picture one and a deleted
// escape nothing. A tab is measured at nothing, so none may be left.
func TestNeutraliseTabs(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"leading", "\tx\n\t\ty", "        x\n                y"},
		{"to the next stop", "a\tb\nabcdefg\th", "a       b\nabcdefg h"},
		{"at a stop, a whole stop", "abcdefgh\ti", "abcdefgh        i"},
		{"columns line up", "FAIL\tpkg/parse\t0.012s\nok  \tpkg/tokens\t0.003s",
			"FAIL    pkg/parse       0.012s\nok      pkg/tokens      0.003s"},
		{"wide runes", "日本\tx", "日本    x"},
		{"control picture", "a\x00\tb", "a␀      b"},
		{"string escape shown", "\x1b]0;t\x07\tx", "␛]0;t␇  x"},
		{"deleted escape", "\x1b[31mab\x1b[0m\tc", "ab      c"},
		{"after a crlf", "abc\r\n\tx", "abc\n        x"},
		{"format character shown", "a\u200b\tx", "a<U+200B>       x"},
	} {
		got := neutralise(c.in)
		if got != c.want {
			t.Errorf("%s: neutralise(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// A field printed as one line is neutralised like a body, and its tabs and
// newlines are shown too, each in one cell.
func TestNeutraliseLine(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"clean text", "plain 日本語", "plain 日本語"},
		{"tab and newline", "a\tb\nc", "a␉b␊c"},
		{"crlf", "a\r\nb", "a␊b"},
		{"lone cr", "a\rb", "a␍b"},
		{"escape", "\x1b[31mred\x1b[0m\n", "red␊"},
		{"format character", "a\u202eb", "a<U+202E>b"},
	} {
		got := neutraliseLine(c.in)
		if got != c.want {
			t.Errorf("%s: neutraliseLine(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	for _, r := range []string{"␉", "␊"} {
		if cells(r) != 1 {
			t.Errorf("%q is %d cells, want 1", r, cells(r))
		}
	}
}

// eastAsian sets, for the rest of the test, whether the ambiguous-width
// characters measure two cells — box drawing, the ellipsis, the bullet, the
// arrows — the way they do in a CJK locale and a terminal set up for one.
func eastAsian(t *testing.T, on bool) {
	t.Helper()
	saved := runewidth.DefaultCondition.EastAsianWidth
	runewidth.DefaultCondition.EastAsianWidth = on
	t.Cleanup(func() { runewidth.DefaultCondition.EastAsianWidth = saved })
}

// TestWidthInvariant is what keeps the layout honest: whatever the terminal
// width, no emitted line may exceed it. Nor where the ambiguous-width
// characters are two cells, which makes every glyph of a rule or a frame two.
func TestWidthInvariant(t *testing.T) {
	d := zooDB(t)
	cases := [][]string{
		nil,
		{"--tools", "full", "--reasoning", "--stats"},
		{"--tools", "none"},
		{"--color", "always", "--tools", "full", "--reasoning", "--stats"},
		{"--list"},
		{"--list", "--color", "always"},
		{"--arg-width", "20", "--max-chars", "40", "--tools", "full"},
	}
	for _, wide := range []bool{false, true} {
		eastAsian(t, wide)
		for _, width := range []int{60, 80, 100, 120, 200} {
			for _, args := range cases {
				t.Setenv("COLUMNS", strconv.Itoa(width))
				var stdout, stderr bytes.Buffer
				full := append([]string{"--db", d.path, "--everywhere", "--all"}, args...)
				if err := run(full, &stdout, &stderr); err != nil {
					t.Fatalf("run(%v): %v", full, err)
				}
				for line := range strings.SplitSeq(stdout.String(), "\n") {
					if got := visibleCells(line); got > width {
						t.Errorf("COLUMNS=%d east asian %v %v: %d-cell line %q", width, wide, args, got, stripSGR(line))
					}
				}
			}
		}
	}
}
