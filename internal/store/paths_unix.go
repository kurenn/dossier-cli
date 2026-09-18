//go:build !windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPaths resolves the XDG roots, falling back to the specification's own defaults.
//
// XDG is followed rather than inventing ~/.dossier: a holder who has set XDG_CONFIG_HOME
// has said where configuration goes, and a tool that ignores that is making its own
// arrangements on someone else's machine.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("could not find your home directory: %w", err)
	}

	return Paths{
		ConfigDir: filepath.Join(xdgRoot("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "dossier"),
		CacheDir:  filepath.Join(xdgRoot("XDG_CACHE_HOME", filepath.Join(home, ".cache")), "dossier"),
		StateDir:  filepath.Join(xdgRoot("XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "dossier"),
	}, nil
}

// xdgRoot honours the variable only when it is an absolute path, which the XDG
// specification requires. A relative value would put the credentials file somewhere
// relative to whatever directory the CLI happened to be invoked from.
func xdgRoot(env, fallback string) string {
	if value := os.Getenv(env); filepath.IsAbs(value) {
		return value
	}
	return fallback
}
