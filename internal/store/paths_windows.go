//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPaths puts everything where Windows expects it, which is what §5.2 asked for:
// `%APPDATA%\dossier\`.
//
// Not XDG. The specification is a Unix desktop convention, and a Windows machine that
// grew a `.config` directory in the user's profile would be one where nothing else looks
// — not the holder, not their backup software, and not the roaming profile machinery
// that syncs %APPDATA% between machines on a domain. Reusing the XDG code here would also
// have been quietly broken rather than merely unidiomatic: filepath.IsAbs("/custom") is
// false on Windows, so every XDG variable a holder set would have been silently ignored.
//
// The split between roaming and local is the one Windows itself draws, and it lines up
// exactly with the lifetimes §5.2 gave the three roots:
//
//   - %APPDATA% roams with the user. Config and credentials belong there — a token is
//     not machine-specific, and a holder signing in on a second domain-joined machine
//     should find their profiles.
//   - %LOCALAPPDATA% stays on the machine. The schema cache is disposable and
//     host-specific, and the mint ledger describes requests this machine sent; neither
//     is meaningful anywhere else, and roaming the ledger would be actively wrong.
func DefaultPaths() (Paths, error) {
	// os.UserConfigDir is %AppData%, os.UserCacheDir is %LocalAppData%. Both fall back
	// to the profile directory if the variable is missing, which is the behaviour a
	// service account with a stripped environment needs.
	roaming, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("could not find your application data directory: %w", err)
	}
	local, err := os.UserCacheDir()
	if err != nil {
		return Paths{}, fmt.Errorf("could not find your local application data directory: %w", err)
	}

	return Paths{
		ConfigDir: filepath.Join(roaming, "dossier"),
		CacheDir:  filepath.Join(local, "dossier", "cache"),
		StateDir:  filepath.Join(local, "dossier", "state"),
	}, nil
}
