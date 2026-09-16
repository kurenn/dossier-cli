package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

// Streams are the process's three standard files, injected so a test can drive the whole
// program — including the exit code, which is the part scripts actually depend on — without
// running a subprocess.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Run is the whole program. main does nothing but call it and exit with what it returns.
//
// Everything lives here rather than in package main so that the error-reporting path and
// the exit contract are reachable from a test. A CLI whose least-tested code is the bit
// that decides its exit status has the priorities backwards.
func Run(args []string, streams Streams, version string) int {
	Version = version

	paths, err := store.DefaultPaths()
	if err != nil {
		fmt.Fprintf(streams.Err, "%s\n", err)
		return exitcode.Unexpected
	}

	// Interrupts cancel the in-flight request's context rather than killing the process
	// outright. It matters for exactly one command: a mint that has been sent must be
	// allowed to finish writing its ledger entry, because a key whose response was never
	// recorded is the state `shares mint --resume` exists to recover, and the state a
	// hard kill mid-write would make unrecoverable.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return RunContext(ctx, args, streams, paths, time.Now)
}

// RunContext is Run with the environment supplied, for tests.
func RunContext(ctx context.Context, args []string, streams Streams, paths store.Paths, now func() time.Time) int {
	app := &App{
		Stdout: streams.Out,
		Stderr: streams.Err,
		Stdin:  streams.In,
		Paths:  paths,
		Now:    func() time.Time { return now().UTC() },
	}

	root := NewRoot(app)
	root.SetOut(streams.Out)
	root.SetErr(streams.Err)
	root.SetArgs(args)

	if err := root.ExecuteContext(ctx); err != nil {
		return Report(streams.Err, err)
	}
	return exitcode.OK
}

// Report prints an error the way the CLI promises to, and returns its exit code.
//
// The API's own message and hint are printed verbatim — they are the product's copy,
// written for the person reading them. Nothing is prefixed with "Error:": a message that
// already reads as a sentence does not need a label, and the exit code is the
// machine-readable half of the answer.
func Report(stderr io.Writer, err error) int {
	var cliErr *Error
	if !errors.As(err, &cliErr) {
		cliErr = Classify(err)
	}

	message := cliErr.Message
	if message == "" {
		message = cliErr.Error()
	}

	fmt.Fprint(stderr, message)
	if len(message) == 0 || message[len(message)-1] != '\n' {
		fmt.Fprintln(stderr)
	}
	return cliErr.Code
}
