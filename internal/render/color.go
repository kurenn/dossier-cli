// Package render turns API data into terminal output under the product's typography and
// colour rules.
//
// Two rules from CLAUDE.md are re-earned here, because a terminal gives neither for free.
// Rule 7 separates prose from data by typeface on the web; a terminal has one typeface, so
// the separation becomes structural — data in labelled blocks and aligned tables, prose in
// sentences, and never both on one line. Rule 1 permits colour only as state; here that
// means colour touches the state word and the countdown figure and nothing else, ever.
package render

import (
	"io"
	"os"
	"strings"
)

// State is a share state as the API reports it. The CLI never computes one (rule 3): it
// renders the string it was given.
type State string

const (
	StateLive     State = "live"
	StateExpiring State = "expiring"
	StateRevoked  State = "revoked"
	StateExpired  State = "expired"
)

// ANSI SGR sequences. Deliberately the terminal's own 3-bit colours rather than 256-colour
// or truecolour approximations of the product's pine/ochre/oxblood: the holder's palette
// decides the exact hue, which is both more legible in whatever theme they run and more
// honest than claiming to reproduce a design token in a terminal.
const (
	sgrReset  = "\x1b[0m"
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[33m"
	sgrRed    = "\x1b[31m"
)

// Colors decides whether SGR sequences are emitted.
type Colors struct {
	enabled bool
}

// NewColors applies the detection rules from §7.2: colour is on when stdout is a terminal
// *and* NO_COLOR is unset *and* TERM is not "dumb". Anything else is off.
//
// There is deliberately no --color=always. Forcing colour into a pipe serves nothing this
// product needs, and every state is required to be complete as text before colour is
// applied — so a pipe losing colour loses nothing.
func NewColors(out io.Writer, forceOff bool) Colors {
	if forceOff {
		return Colors{enabled: false}
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return Colors{enabled: false}
	}
	if os.Getenv("TERM") == "dumb" {
		return Colors{enabled: false}
	}
	return Colors{enabled: isTerminal(out)}
}

// Enabled reports whether colour will be emitted.
func (c Colors) Enabled() bool { return c.enabled }

// ColorsEnabled builds a colour decision directly, bypassing detection.
//
// This exists for tests, and nothing in the command tree reaches it from a flag. Terminal
// detection type-asserts to *os.File, so a test handed a buffer can only ever get the
// no-colour answer — which would leave the colour path, and the column alignment that
// depends on discounting invisible bytes, checked by nothing.
//
// It is emphatically not a --color=always. §7.2 rejected that: forcing colour into a pipe
// serves nothing this product needs, and every state is required to be complete as text
// before colour is applied, so a pipe losing colour loses nothing.
func ColorsEnabled(on bool) Colors { return Colors{enabled: on} }

// StateWord renders a state as the upper-case word the web pill shows, colourised only
// when colour is on.
//
// Expired gets no colour at all, in either mode. On the web it is deliberately hueless —
// "absence, not an alarm" — and a terminal that painted it red would turn a deadline that
// simply arrived into a failure. That asymmetry is the rule working, not an omission.
func (c Colors) StateWord(state State) string {
	word := strings.ToUpper(string(state))
	if !c.enabled {
		return word
	}

	switch state {
	case StateLive:
		return sgrGreen + word + sgrReset
	case StateExpiring:
		return sgrYellow + word + sgrReset
	case StateRevoked:
		return sgrRed + word + sgrReset
	case StateExpired:
		return word
	default:
		// A state this binary does not know is rendered plainly rather than guessed at.
		// The server is the authority on state; inventing a colour for an unknown one
		// would be the client deciding what it means.
		return word
	}
}

// Countdown colourises a deadline figure to match its state. The countdown is the only
// other thing rule 1 allows colour on, and only for a share that is still open — a closed
// share shows the closing fact, which is not a countdown and takes no colour.
func (c Colors) Countdown(state State, text string) string {
	if !c.enabled || text == "" {
		return text
	}
	switch state {
	case StateLive:
		return sgrGreen + text + sgrReset
	case StateExpiring:
		return sgrYellow + text + sgrReset
	default:
		return text
	}
}

// StripSGR removes every SGR sequence from a string. Used by the golden tests to assert
// that the text is complete without colour — the property that makes a pipe safe.
func StripSGR(s string) string {
	var out strings.Builder
	for {
		start := strings.Index(s, "\x1b[")
		if start < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:start])
		end := strings.IndexByte(s[start:], 'm')
		if end < 0 {
			return out.String()
		}
		s = s[start+end+1:]
	}
}
