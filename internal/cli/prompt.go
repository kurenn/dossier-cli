package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// scanner returns the one scanner this run reads stdin through.
//
// Shared rather than created per prompt, which is not a tidiness point: bufio.Scanner
// reads ahead into its own buffer, so a second scanner over the same reader starts after
// whatever the first one consumed. `login` asks up to three questions in a row, and with
// a scanner per prompt the second answer is silently swallowed.
func (a *App) scanner() *bufio.Scanner {
	if a.stdinScanner == nil {
		a.stdinScanner = bufio.NewScanner(a.Stdin)
	}
	return a.stdinScanner
}

// interactive reports whether there is a human at the other end of stdin.
//
// Behind a function field so tests can drive the prompting paths without allocating a
// pty. The production value is the real terminal check; nothing else may set it.
func (a *App) interactive() bool {
	if a.IsInteractive != nil {
		return a.IsInteractive()
	}
	file, ok := a.Stdin.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// readSecret reads a value without echoing it, when it can.
//
// The prompt goes to stderr, not stdout: a holder piping stdout to a file should still
// see what is being asked of them, and the answer is never printed anywhere regardless.
//
// When stdin is not a terminal the value is read as a line with no echo suppression,
// because there is no terminal to suppress it on — that is the --token-stdin path, where
// the caller is a script and the secret arrived through a pipe.
func (a *App) readSecret(prompt string) (string, error) {
	if file, ok := a.Stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(a.Stderr, prompt)
		raw, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(a.Stderr)
		if err != nil {
			return "", Unexpectedf("could not read from the terminal: %s\n", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}

	scanner := a.scanner()
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", Unexpectedf("could not read from standard input: %s\n", err)
		}
		return "", Usagef("Nothing arrived on standard input.\n")
	}
	return strings.TrimSpace(scanner.Text()), nil
}

// readLine reads a visible line, for non-secret answers.
func (a *App) readLine(prompt string) (string, error) {
	fmt.Fprint(a.Stderr, prompt)

	scanner := a.scanner()
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", Unexpectedf("could not read from standard input: %s\n", err)
		}
		// End of input is not an error here: every prompt readLine serves is optional,
		// and a holder pressing Ctrl-D means "skip", not "fail".
		return "", nil
	}
	return strings.TrimSpace(scanner.Text()), nil
}
