package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Colour marks what a line is, never which session it came from. The frame is
// drawn on two levels: the session banner and its rule outside, in one hue,
// and inside it a turn's head and the rail down its side, in the hue of
// whoever took the turn — so the eye can find where a turn starts, whose it
// is, and where the run of turns it sits in starts, without reading any of
// them; the rest is the three states a body line can be in. Handing
// sessions their own colours was tried and dropped: a merged stream routinely
// runs to dozens of them, past any number of hues a reader can tell apart, let
// alone remember, and the tag on the banner answers the question exactly. The
// session hue says a line is a banner, not which banner it is.
//
// Everything that is not what a person or the model actually said is painted,
// in three tiers: 250 for a value, 244 for the label in front of it, 240 for
// the metadata neither of those needs. Message text alone is left in the
// terminal's own foreground, and is the only thing that can be. The quiet tier
// used SGR 2 and no colour, which is not the same thing: faint default-white
// is still white, so a stamp beside a grey label read as the loudest cell on
// the line.
// 256-colour SGR, since every terminal that does colour at all does these.
const (
	grey     = 244
	argColor = 250 // the value beside a grey label, lighter than the label and quieter than prose
	errColor = 203
	okColor  = 108
	// Reasoning is the light tier too, and for the same reason: it is text,
	// and text wants the lightest paint there is. It wore a violet before,
	// which read as a highlight over a block that is the opposite of one —
	// thinking out loud is the least of what a turn says, and a hue no other
	// line had made it the first thing the eye landed on. Grey puts it a step
	// under the prose beside it, which is the whole claim being made about it.
	reasonColor = 250
	dim         = 240 // the quietest tier: stamps, mime types, finish reasons
	noColor     = -1  // sentinel: paint nothing

	// The session table's column heads are white and bold — the only place
	// anything here is either. A head row is neither a label beside a value nor
	// furniture: it is the row that says what every row beneath it means, read
	// once at the top and then not again, and weight says that better than any
	// hue can. Two hues were tried and dropped on the way. The label grey sank
	// the heads into the rules under them, leaving the table nothing above its
	// own furniture. The banner's violet had the pleasing argument that a
	// session table and a session banner name the same thing — but it spent
	// the outer frame's colour on something that is not a frame, and made the
	// head row a fifth kind of coloured line in a transcript that already runs
	// four. White is what is left when neither argument holds: the heads stand
	// off the table on weight alone, and every hue in the palette goes on
	// meaning exactly what it meant.
	headColor = 231
	headBold  = true
)

const reset = "\033[0m"

// fg opens a span: the colour, plus any attribute codes to carry with it. The
// attributes ride in the same escape rather than nesting a second span —
// wrap() carries exactly one open span across a line break, so a nested one
// would come back without its attribute.
func fg(color int, attrs ...string) string {
	attrs = append(attrs, fmt.Sprintf("38;5;%d", color))
	return "\033[" + strings.Join(attrs, ";") + "m"
}

// Paint wraps text in SGR codes, or hands it back untouched when colour is off.
type Paint struct {
	enabled bool
}

// paint returns text wrapped in SGR codes for color (bold when asked), or
// untouched when colour is off.
func (p Paint) paint(text string, color int, bold ...bool) string {
	if !p.enabled || color == noColor || text == "" {
		return text
	}
	if len(bold) > 0 && bold[0] {
		return fg(color, "1") + text + reset
	}
	return fg(color) + text + reset
}

// wantColor decides colour for a stream: an explicit mode wins, then https://no-color.org (any non-empty value disables colour),
// then whether the stream is a terminal.
func wantColor(mode string, stream any) bool {
	switch mode {
	case "never":
		return false
	case "always":
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTerminal(stream)
}

func isTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// terminalWidth is shutil.get_terminal_size: COLUMNS first, then the terminal
// the stream is attached to, then the fallback.
func terminalWidth(stream any, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS"))); err == nil && n > 0 {
		return n
	}
	if f, ok := stream.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			return w
		}
	}
	return fallback
}
