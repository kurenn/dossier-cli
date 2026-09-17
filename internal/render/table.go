package render

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Gutter is the gap between columns, and between a Block's label and its value. One
// constant so a table and a block printed in the same output do not drift apart by a
// space.
const Gutter = "  "

// Table is one record per line with a header row and aligned columns, so that grep, awk
// and sort work on the human output and --json is not the only machine-readable form
// (§7.3).
//
// Widths are computed from the page, not fixed: a page whose longest title is 12
// characters should not be padded to the width of a title that is not on it.
type Table struct {
	headers []string
	rows    [][]string
}

// NewTable starts a table. Headers are upper-case by convention — they are the label
// column of §7.1 rule 2, doing in a header row what a Block does per line — but they are
// used exactly as given, because the caller knows whether a header is a key name.
func NewTable(headers ...string) *Table {
	return &Table{headers: headers}
}

// Add appends a row. A cell that is empty renders as EmptyCell, matching the web's own
// fallback, so "this value is absent" is distinguishable from "this column stopped".
//
// Cells may already carry SGR sequences — a colourised state word or countdown. Width is
// computed on the visible text, so that is safe.
func (t *Table) Add(cells ...string) *Table {
	row := make([]string, len(cells))
	for i, cell := range cells {
		if cell == "" {
			cell = EmptyCell
		}
		row[i] = cell
	}
	t.rows = append(t.rows, row)
	return t
}

// Rows is how many records the table holds, not counting the header.
func (t *Table) Rows() int { return len(t.rows) }

// Render writes the header and every row.
//
// Padding is computed on *visible* width: runes, and with SGR sequences discounted. Both
// halves matter and each has already cost a bug in this repository's history. Counting
// bytes misaligns the moment a holder's name carries an accent or a cell is the em dash;
// counting a colourised cell's raw length pads it by the nine invisible bytes of
// "\x1b[32m...\x1b[0m", which misaligns every column after the state — and only in colour
// mode, which is the mode a golden file captured without colour would never reveal.
func (t *Table) Render(w io.Writer) error {
	widths := t.widths()

	if len(t.headers) > 0 {
		if err := writeRow(w, t.headers, widths); err != nil {
			return err
		}
	}
	for _, row := range t.rows {
		if err := writeRow(w, row, widths); err != nil {
			return err
		}
	}
	return nil
}

// String renders the table to a string.
func (t *Table) String() string {
	var out strings.Builder
	_ = t.Render(&out)
	return out.String()
}

func (t *Table) widths() []int {
	widths := make([]int, len(t.headers))
	for i, header := range t.headers {
		widths[i] = visibleWidth(header)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i >= len(widths) {
				// More cells than headers is a programming error, but truncating the row
				// silently would hide data. Grow instead and let the header column be
				// blank, which is visible.
				widths = append(widths, 0)
			}
			if n := visibleWidth(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	return widths
}

func writeRow(w io.Writer, cells []string, widths []int) error {
	var line strings.Builder
	for i, cell := range cells {
		if i > 0 {
			line.WriteString(Gutter)
		}
		line.WriteString(cell)
		// The last column is never padded; trailing whitespace on every line of a table
		// is noise in a diff and in a copy-paste.
		if i < len(cells)-1 {
			pad := widths[i] - visibleWidth(cell)
			if pad > 0 {
				line.WriteString(strings.Repeat(" ", pad))
			}
		}
	}
	_, err := fmt.Fprintln(w, line.String())
	return err
}

// visibleWidth is the number of runes a string occupies on screen, ignoring SGR escape
// sequences.
//
// It does not attempt East Asian wide-character or grapheme-cluster width. That would need
// a Unicode table this binary has no other use for, and the columns it renders are API
// identifiers, ISO codes, timestamps, states and counts — none of which can be wide. The
// one free-text column is a share title, where a wide character misaligns that page's
// trailing columns and nothing worse.
func visibleWidth(s string) int {
	return utf8.RuneCountInString(StripSGR(s))
}
