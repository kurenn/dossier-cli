//go:build !windows

package store

import (
	"fmt"
	"os"
)

// The Unix answer to "can anyone but the owner read this?" is the mode, and it is
// exactly the check ssh makes on a private key, for the same reason.

// checkOwnerOnly refuses a credentials file with any group or other bit set.
func checkOwnerOnly(path string, info os.FileInfo) error {
	mode := info.Mode().Perm()
	if mode&0o077 == 0 {
		return nil
	}
	return &PermissionError{
		Path: path,
		Finding: fmt.Sprintf(
			"%s is mode %04o, so other users on this machine can read your API token.",
			path, mode),
		Remedy: fmt.Sprintf("chmod 600 %s", path),
	}
}

// RestrictToOwner narrows a file or directory to its owner. On Unix that is a chmod, and
// callers that created the file at 0600 already have what they need — but they call this
// anyway, so the Windows path is never the one nobody remembered.
//
// A directory gets 0700, not 0600. Without the execute bit a directory cannot be entered
// at all, so 0600 on one is not a stricter permission but a broken one — which is how
// this was first written, and the whole store package stopped being able to write a
// temporary file.
func RestrictToOwner(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("could not inspect %s: %w", path, err)
	}

	mode := os.FileMode(0o600)
	if info.IsDir() {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("could not restrict %s to your account: %w", path, err)
	}
	return nil
}
