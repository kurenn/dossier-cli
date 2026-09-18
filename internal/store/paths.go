// Package store owns everything the CLI keeps on disk: the config, the credentials, and
// the schema cache.
//
// Two properties drive the whole package. The token is a secret and the rest is not, so
// they live in separate files with separate modes (§5.2). And the permission check on the
// credentials file is a refusal, not a warning — the same posture ssh takes with a private
// key, for the same reason: a credential the whole machine can read is already a
// different credential than the one you thought you had.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Directory and file modes. 0700/0600 are the point of the exercise, not a default.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600

	// configMode is deliberately looser. config.toml holds a default profile name and a
	// colour preference; there is nothing in it to protect, and making it 0600 would
	// imply otherwise.
	configMode os.FileMode = 0o644
)

// Paths resolves where everything lives for one machine.
//
// XDG is followed rather than inventing ~/.dossier: a holder who has set
// XDG_CONFIG_HOME has said where configuration goes, and a tool that ignores that is
// making its own arrangements on someone else's machine. The three roots are separate
// because they have genuinely different lifetimes — config is edited by hand, the cache
// is disposable, and state (the mint ledger) must survive a cache wipe or the ledger's
// whole purpose is defeated.
type Paths struct {
	ConfigDir string
	CacheDir  string
	StateDir  string
}

// DefaultPaths resolves the XDG roots, falling back to the specification's own defaults.
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

// PathsIn puts all three roots under one directory. For tests, and for a caller that
// wants everything in one place.
func PathsIn(root string) Paths {
	return Paths{
		ConfigDir: filepath.Join(root, "config"),
		CacheDir:  filepath.Join(root, "cache"),
		StateDir:  filepath.Join(root, "state"),
	}
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

func (p Paths) configFile() string      { return filepath.Join(p.ConfigDir, "config.toml") }
func (p Paths) credentialsFile() string { return filepath.Join(p.ConfigDir, "credentials.toml") }

// schemaCacheFile names the cache per host, so a holder who switches between production
// and a local Rails app does not read one's discovery document while talking to the
// other. The host is hashed rather than sanitised into a filename: a hash cannot collide
// by accident and cannot be turned into a path traversal by a hostile --host.
func (p Paths) schemaCacheFile(host string) string {
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(p.CacheDir, "schema-"+hex.EncodeToString(sum[:8])+".json")
}

// ensureDir creates a directory with the given mode, and tightens it if it already exists
// with looser permissions than asked for.
//
// The tightening matters: MkdirAll applies the mode only when it creates, so a directory
// that already existed as 0755 — because an earlier version of this tool made it, or
// because the holder's umask is loose — would silently keep those permissions and the
// 0700 guarantee would be a comment rather than a fact.
func ensureDir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("could not create %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("could not inspect %s: %w", path, err)
	}
	if info.Mode().Perm() != mode {
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("could not set %s to %o: %w", path, mode, err)
		}
	}
	// On Windows the mode above is a fiction and this is the part that does the work;
	// on Unix it is a no-op. See permissions_windows.go.
	if mode&0o077 == 0 {
		return RestrictToOwner(path)
	}
	return nil
}

// writeFileAtomic writes via a temporary file in the same directory and renames it into
// place, so a crash or a full disk mid-write can never leave a half-written token behind
// — the file is either the old one or the new one.
//
// The temp file is created with the final mode rather than chmod'ed afterwards, so the
// secret is never briefly readable by anyone else. Same directory, because rename is only
// atomic within a filesystem.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("could not create a temporary file in %s: %w", dir, err)
	}
	tempName := temp.Name()

	cleanup := func() {
		temp.Close()
		os.Remove(tempName)
	}

	if err := temp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("could not set permissions on %s: %w", tempName, err)
	}
	// Applied to the temp file, before a byte is written and before the rename, so the
	// secret is never on disk at permissions anyone else could use. A mode with group or
	// other bits set is a file with nothing to protect — config.toml — and is left alone.
	if mode&0o077 == 0 {
		if err := RestrictToOwner(tempName); err != nil {
			cleanup()
			return err
		}
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("could not write %s: %w", tempName, err)
	}
	// Flushed before the rename: a rename is atomic with respect to the directory entry,
	// but it does not promise the file's contents reached the disk first.
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("could not flush %s: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("could not close %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		os.Remove(tempName)
		return fmt.Errorf("could not move %s into place at %s: %w", tempName, path, err)
	}
	return nil
}
