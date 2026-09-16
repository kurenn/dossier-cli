package render

import (
	"bytes"
	"strings"
	"testing"
)

// The property that makes a pipe safe: every state is complete as text before colour is
// applied, so stripping SGR loses nothing. §7.2.
func TestStateWordIsCompleteWithoutColour(t *testing.T) {
	plain := Colors{enabled: false}
	coloured := Colors{enabled: true}

	for _, state := range []State{StateLive, StateExpiring, StateRevoked, StateExpired} {
		want := strings.ToUpper(string(state))

		if got := plain.StateWord(state); got != want {
			t.Errorf("without colour, %s rendered %q, want %q", state, got, want)
		}
		if got := StripSGR(coloured.StateWord(state)); got != want {
			t.Errorf("with colour stripped, %s rendered %q, want %q", state, got, want)
		}
	}
}

// Expired is hueless in both modes. On the web it is deliberately so — "absence, not an
// alarm" — and a terminal painting it red would turn a deadline that simply arrived into
// a failure. This is the one state whose colour output must equal its plain output.
func TestExpiredCarriesNoColourEvenWhenColourIsOn(t *testing.T) {
	coloured := Colors{enabled: true}

	got := coloured.StateWord(StateExpired)
	if got != "EXPIRED" {
		t.Errorf("EXPIRED rendered %q; it must carry no SGR sequence at all", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Error("EXPIRED carried an escape sequence")
	}
}

func TestStateWordColourisesTheOtherThree(t *testing.T) {
	coloured := Colors{enabled: true}

	for _, state := range []State{StateLive, StateExpiring, StateRevoked} {
		if !strings.Contains(coloured.StateWord(state), "\x1b[") {
			t.Errorf("%s was not colourised with colour on", state)
		}
	}
}

// A state the server invents later is rendered plainly rather than assigned a colour this
// client made up. Rule 1 permits colour only as state, and only the server knows what a
// new state means.
func TestUnknownStateIsNotColourised(t *testing.T) {
	coloured := Colors{enabled: true}

	got := coloured.StateWord(State("quarantined"))
	if got != "QUARANTINED" {
		t.Errorf("an unknown state rendered %q, want the bare upper-case word", got)
	}
}

func TestCountdownFollowsStateAndSkipsClosedShares(t *testing.T) {
	coloured := Colors{enabled: true}

	if !strings.Contains(coloured.Countdown(StateLive, "5d 15h left"), "\x1b[") {
		t.Error("a live countdown was not colourised")
	}
	// A closed share shows the closing fact, which is not a countdown and takes no colour.
	if got := coloured.Countdown(StateExpired, "2026-09-01T09:00Z"); strings.Contains(got, "\x1b[") {
		t.Errorf("an expired share's deadline was colourised: %q", got)
	}
	if got := coloured.Countdown(StateLive, ""); got != "" {
		t.Errorf("an empty countdown rendered %q", got)
	}
}

// Colour detection. There is no --color=always, so every path here is a way of turning it
// off; a bytes.Buffer is not a terminal, which is exactly the piped case.
func TestNewColorsDetection(t *testing.T) {
	t.Run("off when the writer is not a terminal", func(t *testing.T) {
		if NewColors(&bytes.Buffer{}, false).Enabled() {
			t.Error("colour was enabled for a non-terminal writer")
		}
	})

	t.Run("off when forced off", func(t *testing.T) {
		if NewColors(&bytes.Buffer{}, true).Enabled() {
			t.Error("colour was enabled despite --no-color")
		}
	})

	t.Run("off when NO_COLOR is set, even empty", func(t *testing.T) {
		// The NO_COLOR convention is presence, not value: an empty NO_COLOR still means
		// no colour, which a naive os.Getenv("NO_COLOR") != "" check would miss.
		t.Setenv("NO_COLOR", "")
		if NewColors(&bytes.Buffer{}, false).Enabled() {
			t.Error("colour was enabled with NO_COLOR set to the empty string")
		}
	})

	t.Run("off when TERM is dumb", func(t *testing.T) {
		t.Setenv("TERM", "dumb")
		if NewColors(&bytes.Buffer{}, false).Enabled() {
			t.Error("colour was enabled with TERM=dumb")
		}
	})
}

func TestStripSGR(t *testing.T) {
	cases := map[string]string{
		"\x1b[32mLIVE\x1b[0m": "LIVE",
		"plain":               "plain",
		"":                    "",
		"a\x1b[1mb\x1b[0mc":   "abc",
		"\x1b[32mLIVE":        "LIVE",
		"truncated\x1b[":      "truncated",
	}

	for input, want := range cases {
		if got := StripSGR(input); got != want {
			t.Errorf("StripSGR(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBlockAlignsOnRunesNotBytes(t *testing.T) {
	// The em dash and an accented name are multi-byte. Padding computed in bytes would
	// under-indent every row containing one, which is precisely the rows holding holder
	// names and empty cells.
	block := NewBlock(0).
		Add("RECIPIENT", "Tomás Herrera").
		Add("TITLE", "").
		Add("ID", "88")

	out := block.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), lines)
	}

	// Each value must begin at the same *rune* offset. Measured by locating the value in
	// its own line and counting runes before it, because counting bytes is the bug this
	// test exists to catch: "Tomás" is six bytes and five runes.
	values := []string{"Tomás Herrera", EmptyCell, "88"}
	want := -1
	for index, line := range lines {
		byteOffset := strings.Index(line, values[index])
		if byteOffset < 0 {
			t.Fatalf("line %q does not contain its value %q", line, values[index])
		}
		runeOffset := len([]rune(line[:byteOffset]))

		if want == -1 {
			want = runeOffset
			continue
		}
		if runeOffset != want {
			t.Errorf("value column starts at rune %d in %q, want %d", runeOffset, line, want)
		}
	}

	if !strings.Contains(out, EmptyCell) {
		t.Errorf("an empty value did not render as %q:\n%s", EmptyCell, out)
	}
}

func TestBlockMinLabelKeepsSeparateBlocksAligned(t *testing.T) {
	// Two blocks printed in sequence must share a value column, or the output reads as two
	// unrelated things — which is what `whoami`'s fact/statement split would become.
	first := NewBlock(12).Add("HOST", "https://dossier.global")
	second := NewBlock(12).Add("SCOPES", "fields:read")

	firstOffset := strings.Index(first.String(), "https")
	secondOffset := strings.Index(second.String(), "fields")

	if firstOffset != secondOffset {
		t.Errorf("value columns diverged: %d vs %d\n%s%s", firstOffset, secondOffset, first, second)
	}
}

func TestBlockProseAndBlankRows(t *testing.T) {
	block := NewBlock(6).
		Add("ID", "1").
		Blank().
		Prose("Stated at login:").
		Add("SCOPES", "fields:read")

	out := block.String()
	if !strings.Contains(out, "\n\n") {
		t.Error("the blank row did not produce an empty line")
	}
	// Prose is full-width and must not be indented into the value column, or it reads as
	// a datum.
	if !strings.Contains(out, "\nStated at login:\n") {
		t.Errorf("prose was not rendered full-width:\n%q", out)
	}
}

func TestBlockAddRawKeepsEmptyValues(t *testing.T) {
	// AddRaw exists for values that are already rendered — a colourised state word, or a
	// deliberately blank cell that must not become an em dash.
	block := NewBlock(0).AddRaw("STATE", "")

	if strings.Contains(block.String(), EmptyCell) {
		t.Errorf("AddRaw substituted an empty cell:\n%q", block.String())
	}
}

func TestBlockRenderPropagatesWriterErrors(t *testing.T) {
	block := NewBlock(0).Add("ID", "1")

	if err := block.Render(failingWriter{}); err == nil {
		t.Error("Render swallowed a writer error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

var errWrite = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "write failed" }
