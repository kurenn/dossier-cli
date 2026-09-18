//go:build !windows

package store

import (
	"os"
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
