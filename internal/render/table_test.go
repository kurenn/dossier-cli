package render

import (
	"strings"
	"testing"
)

func TestTableAlignsColumnsToTheWidestCell(t *testing.T) {
	table := NewTable("ID", "TITLE", "STATE")
	table.Add("1", "Lease application", "LIVE")
	table.Add("840", "Bank KYC", "EXPIRING")

	lines := renderLines(t, table)

	if len(lines) != 3 {
		t.Fatalf("expected a header and two rows, got %d lines: %q", len(lines), lines)
	}
	// TITLE starts after the widest id ("840") plus the gutter, on every line.
	for _, line := range lines {
		assertColumnStarts(t, line, 1, 5)
	}
}

// The em dash is the empty cell, and it is one rune and three bytes. A width computed in
// bytes puts every column after it two characters out — which is why this asserts on the
// rune offset and not on the presence of the dash.
func TestTableEmptyCellIsAnEmDashAndDoesNotSkewAlignment(t *testing.T) {
	table := NewTable("ID", "RECIPIENT", "STATE")
	table.Add("1", "", "EXPIRED")
	table.Add("2", "Marisol Vega", "LIVE")

	lines := renderLines(t, table)

	if !strings.Contains(lines[1], EmptyCell) {
		t.Errorf("an absent recipient should render as %q, got %q", EmptyCell, lines[1])
	}
	// The recipient column is as wide as its widest *cell*, "Marisol Vega", not as wide
	// as its header — widths come from the page.
	for _, line := range lines {
		assertColumnStarts(t, line, 2, len("ID")+len(Gutter)+len("Marisol Vega")+len(Gutter))
	}
}

// The bug this exists for: a colourised cell carries SGR bytes that occupy no screen
// columns, so padding it by its raw length pushes every later column out by nine
// characters. It only shows in colour mode, and the golden files are captured with
// NO_COLOR set — so without this test the misalignment would ship invisibly.
func TestTableAlignsAroundInvisibleColourSequences(t *testing.T) {
	colors := Colors{enabled: true}

	table := NewTable("ID", "STATE", "OPENS")
	table.Add("1", colors.StateWord(StateLive), "0")
	table.Add("2", colors.StateWord(StateExpiring), "7")

	lines := renderLines(t, table)

	for i, line := range lines {
		if i > 0 && !strings.Contains(line, "\x1b[") {
			t.Fatalf("row %d should carry colour: %q", i, line)
		}
		// OPENS begins one gutter past the widest state word, "EXPIRING", measured on
		// what the eye sees rather than on what the bytes say.
		assertColumnStarts(t, StripSGR(line), 2, len("ID")+len(Gutter)+len("EXPIRING")+len(Gutter))
	}
}

func TestTableDoesNotPadTheLastColumn(t *testing.T) {
	table := NewTable("ID", "FIELDS")
	table.Add("1", "5")
	table.Add("2", "12")

	for _, line := range renderLines(t, table) {
		if strings.HasSuffix(line, " ") {
			t.Errorf("trailing whitespace in %q", line)
		}
	}
}

func TestTableWidthsComeFromThePageNotAFixedGuess(t *testing.T) {
	narrow := NewTable("TITLE", "STATE")
	narrow.Add("Bank KYC", "LIVE")

	wide := NewTable("TITLE", "STATE")
	wide.Add("A considerably longer share title", "LIVE")

	narrowOffset := runeIndex(renderLines(t, narrow)[1], "LIVE")
	wideOffset := runeIndex(renderLines(t, wide)[1], "LIVE")

	if narrowOffset >= wideOffset {
		t.Errorf("a page of short titles should not be padded to a longer page's width: %d vs %d",
			narrowOffset, wideOffset)
	}
}

func TestTableRendersWithoutAHeader(t *testing.T) {
	table := NewTable()
	table.Add("1", "LIVE")
	table.Add("840", "EXPIRING")

	lines := renderLines(t, table)

	if len(lines) != 2 {
		t.Fatalf("expected two rows and no header, got %q", lines)
	}
	// Widths still come from the rows, so the second column lines up.
	for _, line := range lines {
		assertColumnStarts(t, line, 1, len("840")+len(Gutter))
	}
}

func renderLines(t *testing.T, table *Table) []string {
	t.Helper()

	out := table.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("a table should end in a newline: %q", out)
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// assertColumnStarts checks that the nth column begins at the given rune offset. Counting
// runes rather than bytes is the whole point: an em dash or an accented name makes the two
// disagree, and bytes are the wrong answer.
func assertColumnStarts(t *testing.T, line string, column, offset int) {
	t.Helper()

	fields := splitColumns(line)
	if column >= len(fields) {
		t.Fatalf("line %q has no column %d", line, column)
	}
	got := runeIndex(line, fields[column])
	if got != offset {
		t.Errorf("column %d of %q starts at rune %d, want %d", column, line, got, offset)
	}
}

// splitColumns recovers cells by splitting on the gutter. Only safe because no cell in
// these tests contains a double space.
func splitColumns(line string) []string {
	var cells []string
	for _, part := range strings.Split(line, Gutter) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			cells = append(cells, trimmed)
		}
	}
	return cells
}

func runeIndex(haystack, needle string) int {
	byteIndex := strings.Index(haystack, needle)
	if byteIndex < 0 {
		return -1
	}
	return len([]rune(haystack[:byteIndex]))
}
