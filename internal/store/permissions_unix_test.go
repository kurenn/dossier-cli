//go:build !windows

package store

import (
	"os"
	"strings"
	"testing"
)

// exposureRemedy is the fix the refusal must name on this platform.
const exposureRemedy = "chmod 600"

// expose makes a file readable by everyone, so a test can assert the refusal.
func expose(t *testing.T, path string) {
	t.Helper()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
}

// The mode check itself, which only means anything here. Kept as a table because the
// distinction that matters is between "some bit outside owner is set" and "none is" —
// not between 0644 and 0666, which a single case would leave untested.
func TestCheckOwnerOnlyReadsTheMode(t *testing.T) {
	path := filepathJoinTemp(t)

	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if err := CheckOwnerOnly(path); err != nil {
			t.Errorf("mode %04o was refused: %v", mode, err)
		}
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666, 0o660} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if err := CheckOwnerOnly(path); err == nil {
			t.Errorf("mode %04o was accepted", mode)
		}
	}
}

// RestrictToOwner uses 0700 for a directory and 0600 for a file. Getting that backwards
// produces a directory nobody can enter, which is not a stricter permission but a broken
// one — and is exactly how this was first written.
func TestRestrictToOwnerKeepsADirectoryUsable(t *testing.T) {
	dir := t.TempDir()

	if err := RestrictToOwner(dir); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}
	if mode := statMode(t, dir); mode != 0o700 {
		t.Errorf("directory is %04o, want 0700", mode)
	}
	// The actual property: it can still be written into.
	if err := os.WriteFile(dir+"/probe", []byte("x"), 0o600); err != nil {
		t.Errorf("the restricted directory cannot be written to: %v", err)
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return info.Mode().Perm()
}

func filepathJoinTemp(t *testing.T) string {
	t.Helper()

	path := t.TempDir() + "/secret"
	if err := os.WriteFile(path, []byte("dsk_secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// assertNotSecret checks the deliberately looser mode on config.toml. There is nothing
// in it to protect, and 0600 would imply otherwise.
func assertNotSecret(t *testing.T, path string) {
	t.Helper()

	if mode := statMode(t, path); mode != 0o644 {
		t.Errorf("config.toml is %04o, want 0644", mode)
	}
}

// XDG is followed rather than inventing ~/.dossier, but only for absolute values — the
// specification requires it, and a relative value would put the credentials file
// somewhere relative to whatever directory the CLI was invoked from.
func TestDefaultPathsHonoursAbsoluteXDGOnly(t *testing.T) {
	t.Run("absolute values are used", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/custom/config")
		paths, err := DefaultPaths()
		if err != nil {
			t.Fatalf("DefaultPaths: %v", err)
		}
		if paths.ConfigDir != "/custom/config/dossier" {
			t.Errorf("ConfigDir = %q", paths.ConfigDir)
		}
	})

	t.Run("relative values fall back to the specification's default", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "relative/path")
		paths, err := DefaultPaths()
		if err != nil {
			t.Fatalf("DefaultPaths: %v", err)
		}
		if strings.Contains(paths.ConfigDir, "relative/path") {
			t.Errorf("a relative XDG_CONFIG_HOME was honoured: %q", paths.ConfigDir)
		}
		if !strings.HasSuffix(paths.ConfigDir, "/.config/dossier") {
			t.Errorf("ConfigDir = %q, want the ~/.config default", paths.ConfigDir)
		}
	})
}
