//go:build windows

package store

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no file mode, and the one Go reports is a fiction.
//
// os.Stat on Windows synthesises a mode from the read-only attribute: an ordinary file
// comes back 0666, a read-only one 0444. Neither says anything about *who* may read it.
// Run the Unix check here and it refuses every credentials file on every Windows machine,
// including the one `dossier login` has just written — which is why this file exists
// rather than the platform being left to the generic path.
//
// The real answer lives in the file's DACL, and the question this package asks of it is
// the same one it asks of a Unix mode: can any account other than this one read the
// token? "No" here means the DACL grants read access to nobody outside the set below.
//
// SYSTEM and Administrators are permitted. Not because they are harmless — an
// administrator can read anything on the machine, and can take ownership of a file to do
// it — but because refusing them would be theatre. Windows will not let a file exist that
// an administrator cannot reach, so a check that rejected them would reject every
// possible file and teach the holder to ignore the error. The credential is protected
// from other *users*, which is the property the Unix check delivers too: mode 0600 does
// not keep a secret from root either.

// ownerOnlyAccess is the access the file's owner is granted, and the only ACE
// RestrictToOwner writes besides SYSTEM's.
const ownerOnlyAccess = windows.GENERIC_READ | windows.GENERIC_WRITE | windows.DELETE |
	windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER | windows.SYNCHRONIZE

// readBits is any access that would let a trustee read the token. WRITE_DAC and
// WRITE_OWNER are included: either one lets its holder grant themselves read access, so a
// trustee with them is a trustee who can read the file in two steps rather than one.
const readBits = windows.GENERIC_READ | windows.GENERIC_ALL | windows.FILE_READ_DATA |
	windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER | windows.MAXIMUM_ALLOWED

// checkOwnerOnly reads the file's DACL and refuses if anyone but this user, SYSTEM or
// the local Administrators group is granted read access.
//
// info is ignored, deliberately: it carries the synthetic mode, and consulting it would
// reintroduce exactly the fiction this file exists to avoid.
func checkOwnerOnly(path string, _ os.FileInfo) error {
	allowed, err := permittedTrustees()
	if err != nil {
		return fmt.Errorf("could not determine who you are, to check who can read %s: %w", path, err)
	}

	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("could not read the permissions on %s: %w", path, err)
	}

	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("could not read the permissions on %s: %w", path, err)
	}
	if dacl == nil {
		// A NULL DACL is not "no permissions", it is "everyone, everything" — the most
		// open a Windows object can be, and easy to misread as the most closed.
		return &PermissionError{
			Path:    path,
			Finding: fmt.Sprintf("%s has no access control list at all, which on Windows grants everyone full control of your API token.", path),
			Remedy:  resetCommand(path),
		}
	}

	var strangers []string
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var header *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &header); err != nil {
			return fmt.Errorf("could not read entry %d of the permissions on %s: %w", index, path, err)
		}
		// Only allow-ACEs can grant anything. A deny-ACE narrows access, so ignoring one
		// can make this check stricter than reality but never looser, which is the safe
		// direction for a refusal.
		if header.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if header.Mask&readBits == 0 {
			continue
		}

		sid := (*windows.SID)(unsafe.Pointer(&header.SidStart))
		if permitted(allowed, sid) {
			continue
		}
		strangers = append(strangers, describe(sid))
	}

	if len(strangers) == 0 {
		return nil
	}
	return &PermissionError{
		Path: path,
		Finding: fmt.Sprintf("%s can be read by %s, so your API token is not yours alone.",
			path, strings.Join(strangers, ", ")),
		Remedy: resetCommand(path),
	}
}

// RestrictToOwner replaces the file's DACL with two entries: this user, and SYSTEM.
//
// Inheritance is switched off by passing PROTECTED_DACL_SECURITY_INFORMATION, which is
// the whole point on Windows. A file created in a user's profile inherits that
// directory's ACL, and the defaults there routinely include groups the holder never
// chose. Setting an explicit DACL without protecting it would leave the inherited entries
// in place underneath — and checkOwnerOnly would then, correctly, refuse the file this
// function had just written.
func RestrictToOwner(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("could not inspect %s: %w", path, err)
	}

	user, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("could not determine who you are, to secure %s: %w", path, err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("could not secure %s: %w", path, err)
	}

	// A directory's entries are made inheritable so that anything created inside it
	// starts owner-only, rather than owner-only a moment later when the writer
	// remembers to say so. Files get no inheritance: nothing is created inside a file.
	inheritance := uint32(windows.NO_INHERITANCE)
	if info.IsDir() {
		inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}

	entries := []windows.EXPLICIT_ACCESS{
		explicitAccess(user, ownerOnlyAccess, inheritance),
		// SYSTEM is included because backup, antivirus and the installer all run as it,
		// and a file it cannot touch causes failures far from here that are very hard to
		// trace back. It can read the file anyway, by taking ownership; saying so in the
		// ACL is honest rather than permissive.
		explicitAccess(system, ownerOnlyAccess, inheritance),
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("could not build an access control list for %s: %w", path, err)
	}

	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("could not secure %s: %w", path, err)
	}
	return nil
}

func explicitAccess(sid *windows.SID, mask windows.ACCESS_MASK, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: mask,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

// currentUserSID is the SID of the account this process runs as.
func currentUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

// permittedTrustees are the SIDs allowed to appear in an allow-ACE: this user, SYSTEM,
// and the local Administrators group. See the file comment for why the last two.
func permittedTrustees() ([]*windows.SID, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	allowed := []*windows.SID{user}

	for _, known := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid,
		windows.WinBuiltinAdministratorsSid,
	} {
		sid, err := windows.CreateWellKnownSid(known)
		if err != nil {
			// Not fatal. A machine that cannot name its own Administrators group is
			// unusual, and the consequence is a stricter check, not a looser one.
			continue
		}
		allowed = append(allowed, sid)
	}
	return allowed, nil
}

func permitted(allowed []*windows.SID, sid *windows.SID) bool {
	for _, candidate := range allowed {
		if windows.EqualSid(candidate, sid) {
			return true
		}
	}
	return false
}

// describe renders a SID as a name where one exists, so the error names an account the
// holder recognises rather than a string of digits.
func describe(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return sid.String()
	}
	if domain == "" {
		return account
	}
	return domain + `\` + account
}

// resetCommand is the Windows equivalent of `chmod 600`: strip inheritance, drop
// everyone the file picked up from its parent, and grant this account alone.
func resetCommand(path string) string {
	return fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r "%%USERNAME%%:F"`, path)
}
