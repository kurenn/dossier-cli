// Package cli is the command tree and the glue between the transport, the stores and the
// renderer.
//
// Everything here is written so a test can drive a whole command with injected streams,
// an injected clock and a temporary set of XDG roots — the alternative is a CLI whose
// behaviour can only be checked by running the binary, which makes the exit-code contract
// (the thing scripts actually depend on) the least-tested part of the program.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/kurenn/dossier-cli/internal/store"
)

// Version is stamped at build time from the git tag. The zero value says so rather than
// claiming a version nobody released.
var Version = "dev"

// App carries the process's environment: streams, paths, and the global flags.
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	Paths store.Paths
	Now   func() time.Time

	// Sleep is every wait the retry table in §6.3 takes. Injected so a test can drive
	// three minutes of bounded patience in no time at all, and so it can assert *what* was
	// waited rather than merely that something was. Production leaves it nil.
	Sleep func(time.Duration)

	// IsInteractive overrides the terminal check on stdin. Production leaves it nil and
	// gets the real check; tests set it to drive the prompting paths without a pty.
	IsInteractive func() bool

	// ForceColor overrides the terminal check on stdout, for the same reason and with the
	// same rule: production leaves it nil. --no-color still wins over it, so the only
	// thing it can do is turn colour on for a test that is asserting the coloured output.
	ForceColor func() bool

	// Global flags, bound in root.go.
	ProfileFlag string
	HostFlag    string
	JSON        bool
	NoColor     bool
	NoWait      bool
	Quiet       bool
	Timeout     time.Duration

	// Cached per run so a command that touches both does not read the files twice.
	config *store.Config
	creds  *store.Credentials

	// One scanner for the whole run; see prompt.go for why it cannot be per-prompt.
	stdinScanner *bufio.Scanner
}

// Error is a failure that carries the exit code the process should end on.
//
// The code is chosen at the point the failure is understood, not inferred later from a
// message: an API envelope knows its own code, a refusal-before-sending knows it is a
// usage error, and a transport failure knows it never reached anyone. Deciding it here
// rather than in main is what keeps the mapping in one place and testable.
type Error struct {
	Code int

	// Message is prose for stderr. Rendered verbatim when it came from the API.
	Message string

	// Err is the underlying cause, for %w chains and tests.
	Err error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit %d", e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

// Usagef is a refusal before anything was sent.
func Usagef(format string, args ...any) *Error {
	return &Error{Code: exitcode.Usage, Message: fmt.Sprintf(format, args...)}
}

// Unexpectedf is a transport failure, an unparseable response, or a CLI bug.
func Unexpectedf(format string, args ...any) *Error {
	return &Error{Code: exitcode.Unexpected, Message: fmt.Sprintf(format, args...)}
}

// FromAPIError converts an API envelope into a CLI error, preserving the envelope's own
// copy verbatim and mapping its code to an exit status.
func FromAPIError(apiErr *api.Error) *Error {
	return &Error{Code: apiErr.ExitCode(), Message: apiErr.Render(), Err: apiErr}
}

// Classify turns any error from the api package into a CLI error. A transport failure is
// exit 1; an envelope is whatever its code maps to.
func Classify(err error) *Error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return FromAPIError(apiErr)
	}
	var cliErr *Error
	if errors.As(err, &cliErr) {
		return cliErr
	}
	return &Error{Code: exitcode.Unexpected, Message: err.Error(), Err: err}
}

// Config loads config.toml once per run.
func (a *App) Config() (*store.Config, error) {
	if a.config != nil {
		return a.config, nil
	}
	config, err := store.LoadConfig(a.Paths)
	if err != nil {
		return nil, Unexpectedf("%s", err)
	}
	a.config = config
	return config, nil
}

// Credentials loads credentials.toml once per run.
//
// A PermissionError is mapped to exit 2 here rather than 1: the CLI refused before
// sending anything, which is exactly what exit 2 means, and a script that sees 2 knows
// nothing reached the server and nothing needs reconciling.
func (a *App) Credentials() (*store.Credentials, error) {
	if a.creds != nil {
		return a.creds, nil
	}
	creds, err := store.LoadCredentials(a.Paths)
	if err != nil {
		var permErr *store.PermissionError
		if errors.As(err, &permErr) {
			return nil, &Error{Code: exitcode.Usage, Message: permErr.Error(), Err: permErr}
		}
		return nil, Unexpectedf("%s", err)
	}
	a.creds = creds
	return creds, nil
}

// Resolve works out the profile, host and token for this run.
func (a *App) Resolve() (store.Resolution, error) {
	config, err := a.Config()
	if err != nil {
		return store.Resolution{}, err
	}
	creds, err := a.Credentials()
	if err != nil {
		return store.Resolution{}, err
	}
	return store.Resolve(config, creds, a.ProfileFlag, a.HostFlag), nil
}

// Client builds a transport for the resolved host, carrying the resolved token.
func (a *App) Client(resolution store.Resolution, extra ...api.Option) (*api.Client, error) {
	opts := []api.Option{
		api.WithUserAgent("dossier-cli/" + Version),
		api.WithNoWait(a.NoWait),
	}
	// Appended after the defaults so a caller can override one. `shares mint` uses this to
	// force NoWait on at the transport level while still honouring --no-wait itself; see
	// mintClient.
	opts = append(opts, extra...)
	if resolution.Token != "" {
		opts = append(opts, api.WithToken(resolution.Token))
	}

	client, err := api.New(resolution.Host, opts...)
	if err != nil {
		return nil, Usagef("%s", err)
	}
	return client, nil
}

// Colors builds the colour decision for stdout.
//
// --no-color is checked first and wins outright, so an override cannot resurrect colour a
// holder asked not to have.
func (a *App) Colors() render.Colors {
	if a.NoColor {
		return render.ColorsEnabled(false)
	}
	if a.ForceColor != nil {
		return render.ColorsEnabled(a.ForceColor())
	}
	return render.NewColors(a.Stdout, false)
}

// Printf writes data to stdout.
func (a *App) Printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, format, args...)
}

// Println writes a data line to stdout.
func (a *App) Println(args ...any) {
	fmt.Fprintln(a.Stdout, args...)
}

// Notef writes prose *about* the run to stderr — a retry notice, a login preface, a
// warning. §7.5: stdout is data, stderr is commentary, so a script capturing stdout gets
// exactly what it asked for. Suppressed by --quiet.
func (a *App) Notef(format string, args ...any) {
	if a.Quiet {
		return
	}
	fmt.Fprintf(a.Stderr, format, args...)
}

// requireToken returns a usage error when no token is available, naming the ways one can
// be supplied. Commands that need a credential call this rather than sending a request
// with no Authorization header and letting the server answer 401 — that would spend a
// unit of the per-IP failed-auth limiter to discover something the CLI already knew.
func (a *App) requireToken(resolution store.Resolution) error {
	if resolution.Token != "" {
		return nil
	}
	return Usagef(
		"No API token for profile %q.\n\n"+
			"Mint one from Settings -> API tokens in your vault, then:\n\n"+
			"    dossier login\n\n"+
			"Or set DOSSIER_TOKEN for this shell.\n",
		resolution.ProfileName,
	)
}
