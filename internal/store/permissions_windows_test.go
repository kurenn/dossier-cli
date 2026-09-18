//go:build windows

package store

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// exposureRemedy is the fix the refusal must name on this platform. There is no chmod
// here, and telling a Windows holder to run one would be advice they cannot follow.
const exposureRemedy = "icacls"

// expose grants Everyone read access, so a test can assert the refusal.
//
// This is what an inherited ACL from a badly configured profile directory looks like,
// and it is the case the whole DACL check exists to catch — the file's synthetic mode
// is identical before and after, so nothing in the Unix check would notice.
func expose(t *testing.T, path string) {
	t.Helper()

	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}

	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		explicitAccess(user, ownerOnlyAccess, windows.NO_INHERITANCE),
		explicitAccess(everyone, windows.GENERIC_READ, windows.NO_INHERITANCE),
	}, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}

	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		t.Fatalf("SetNamedSecurityInfo: %v", err)
	}
}

// The round trip that the Unix mode check cannot do here: lock a file down, then confirm
// the checker agrees it is locked down. If these two ever disagree, `dossier login`
// writes a file `dossier whoami` refuses, which is the worst possible failure mode —
// the CLI breaking on its own output.
func TestRestrictToOwnerSatisfiesTheCheck(t *testing.T) {
	path := t.TempDir() + `\secret`
	if err := os.WriteFile(path, []byte("dsk_secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := RestrictToOwner(path); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}
	if err := CheckOwnerOnly(path); err != nil {
		t.Errorf("a file RestrictToOwner just secured was refused by CheckOwnerOnly: %v", err)
	}
}

// The mode is a fiction here, and this is the test that says so out loud. A file Windows
// considers perfectly private reports a mode with group and other bits set, which is why
// the Unix check could not simply be reused.
func TestTheReportedModeSaysNothingAboutAccess(t *testing.T) {
	path := t.TempDir() + `\secret`
	if err := os.WriteFile(path, []byte("dsk_secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := RestrictToOwner(path); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 == 0 {
		t.Skipf("this Windows build reports %04o, so the Unix check would have worked "+
			"after all; the DACL check remains correct but this note is stale", mode)
	}
	// Having established the mode looks exposed, the DACL must still say otherwise.
	if err := CheckOwnerOnly(path); err != nil {
		t.Errorf("CheckOwnerOnly is reading the mode rather than the DACL: %v", err)
	}
}

func TestCheckOwnerOnlyNamesTheAccountThatCanRead(t *testing.T) {
	path := t.TempDir() + `\secret`
	if err := os.WriteFile(path, []byte("dsk_secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	expose(t, path)

	err := CheckOwnerOnly(path)
	if err == nil {
		t.Fatal("a file Everyone can read was accepted")
	}
	// "Everyone" is the account name Windows gives the World SID, and naming it is the
	// difference between an error a holder can act on and a string of digits.
	if !strings.Contains(err.Error(), "Everyone") {
		t.Errorf("the error does not name the account that can read the file:\n%s", err)
	}
}

// A directory's entries are made inheritable so that anything created inside starts
// owner-only. Without this, a file written into the config directory picks up whatever
// the parent profile directory grants, and CheckOwnerOnly refuses the CLI's own output.
func TestRestrictToOwnerMakesADirectoryUsableAndInherited(t *testing.T) {
	dir := t.TempDir()

	if err := RestrictToOwner(dir); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}
	if err := CheckOwnerOnly(dir); err != nil {
		t.Errorf("the directory is not owner-only: %v", err)
	}

	child := dir + `\probe`
	if err := os.WriteFile(child, []byte("x"), 0o600); err != nil {
		t.Fatalf("the restricted directory cannot be written to: %v", err)
	}
	if err := CheckOwnerOnly(child); err != nil {
		t.Errorf("a file created inside the restricted directory is not owner-only, so "+
			"the ACEs are not inheriting: %v", err)
	}
}

// assertNotSecret is a no-op here.
//
// config.toml is deliberately 0644 on Unix — it holds a default profile name and nothing
// worth protecting, and 0600 beside the credentials file would blur which of the two
// matters. Windows has no mode to make that statement with, and writing a permissive ACL
// to say "this is not secret" would be inventing a claim nobody asked for: an ordinary
// file in the user's own %APPDATA% is already exactly as accessible as it should be.
func assertNotSecret(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("config.toml was not written: %v", err)
	}
}

// §5.2 says `%APPDATA%\dossier\`, and until M5 the code said `~/.config/dossier` on
// every platform. Nothing failed, because nothing ran here.
func TestDefaultPathsUsesTheWindowsLocations(t *testing.T) {
	t.Setenv("AppData", `C:\Users\test\AppData\Roaming`)
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)

	paths, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}

	if want := `C:\Users\test\AppData\Roaming\dossier`; paths.ConfigDir != want {
		t.Errorf("ConfigDir = %q, want %q", paths.ConfigDir, want)
	}
	// Cache and state stay local: the schema cache is disposable and host-specific, and
	// the ledger describes requests this machine sent. Roaming either would be wrong.
	if want := `C:\Users\test\AppData\Local\dossier\cache`; paths.CacheDir != want {
		t.Errorf("CacheDir = %q, want %q", paths.CacheDir, want)
	}
	if want := `C:\Users\test\AppData\Local\dossier\state`; paths.StateDir != want {
		t.Errorf("StateDir = %q, want %q", paths.StateDir, want)
	}
}

// The XDG variables are not consulted here, and the reason is worth a test rather than a
// comment: filepath.IsAbs("/custom/config") is false on Windows, so the old shared code
// did not merely apply a Unix convention on Windows — it read the variable, silently
// decided it was relative, and discarded it. A holder setting XDG_CONFIG_HOME would have
// been ignored without a word.
func TestDefaultPathsIgnoresXDGHere(t *testing.T) {
	t.Setenv("AppData", `C:\Users\test\AppData\Roaming`)
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)
	t.Setenv("XDG_CONFIG_HOME", `D:\somewhere\else`)

	paths, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	if strings.Contains(paths.ConfigDir, "somewhere") {
		t.Errorf("XDG_CONFIG_HOME was honoured on Windows: %q", paths.ConfigDir)
	}
}
