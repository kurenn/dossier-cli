package render

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// EmptyCell is what a missing value renders as, matching the web's own fallback.
const EmptyCell = "—"

// Block is a key–value record: a fixed-width upper-case label column, then values
// starting at one aligned column.
//
// The label column *is* the mono face (§7.1, rule 2). On the web, IBM Plex Mono tells the
// eye "this is a record, not a paragraph"; in a terminal the aligned upper-case label
// column does the same job. Which is why a Block never contains a sentence: prose goes
// above or below it, as its own paragraph, never in a cell.
type Block struct {
	rows     []blockRow
	minLabel int
}

type blockRow struct {
	label string
	value string

	// blank marks a spacer row, used to group a block into sections — the "Stated at
	// login" split in `whoami`, for instance, where the divide between fact and statement
	// is the whole point of the output.
	blank bool

	// prose marks a full-width line that is not a label/value pair. It exists only for
	// the section headings inside a block, which are prose *about* the following rows.
	prose string
}

// NewBlock starts a block. minLabel sets a floor for the label column so that several
// blocks printed in sequence line up with each other even when their longest labels
// differ — a `whoami` output whose second half was indented differently would read as two
// unrelated things.
func NewBlock(minLabel int) *Block {
	return &Block{minLabel: minLabel}
}

// Add appends a label/value row. An empty value renders as EmptyCell rather than a blank,
// so a reader can tell "this field is empty" from "this line is missing".
func (b *Block) Add(label, value string) *Block {
	if value == "" {
		value = EmptyCell
	}
	b.rows = append(b.rows, blockRow{label: label, value: value})
	return b
}

// AddRaw appends a row whose value is used exactly as given, including an empty one.
// For values that are already rendered — a colourised state word, or a deliberately
// blank cell.
func (b *Block) AddRaw(label, value string) *Block {
	b.rows = append(b.rows, blockRow{label: label, value: value})
	return b
}

// Blank appends a spacer.
func (b *Block) Blank() *Block {
	b.rows = append(b.rows, blockRow{blank: true})
	return b
}

// Prose appends a full-width sentence inside the block. Used for a section heading such
// as "Stated at login — Dossier does not report these over the API:", which is prose and
// must not be dressed as a datum.
func (b *Block) Prose(text string) *Block {
	b.rows = append(b.rows, blockRow{prose: text})
	return b
}

// Width returns the computed label column width.
func (b *Block) Width() int {
	width := b.minLabel
	for _, row := range b.rows {
		if row.blank || row.prose != "" {
			continue
		}
		if n := utf8.RuneCountInString(row.label); n > width {
			width = n
		}
	}
	return width
}

// Render renders the block.
//
// Values are written after padding computed in *runes*, not bytes, so a label column
// stays aligned when a value or label carries a non-ASCII character. The em dash in an
// empty cell is the immediate case, and holder names make it a certainty.
func (b *Block) Render(w io.Writer) error {
	width := b.Width()

	for _, row := range b.rows {
		switch {
		case row.blank:
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		case row.prose != "":
			if _, err := fmt.Fprintln(w, row.prose); err != nil {
				return err
			}
		default:
			pad := width - utf8.RuneCountInString(row.label)
			if pad < 0 {
				pad = 0
			}
			line := row.label + strings.Repeat(" ", pad) + "  " + row.value
			if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
				return err
			}
		}
	}
	return nil
}

// String renders the block to a string.
func (b *Block) String() string {
	var out strings.Builder
	_ = b.Render(&out)
	return out.String()
}
