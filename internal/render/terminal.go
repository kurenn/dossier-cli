package render

import (
	"io"
	"os"

	"golang.org/x/term"
)

// isTerminal reports whether w is a character device we should treat as a TTY.
//
// Kept behind a tiny interface check rather than taking an *os.File so the rest of the
// package can be handed a bytes.Buffer in tests and get the "not a terminal" answer
// naturally, which is exactly the no-colour case the golden files assert.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}
