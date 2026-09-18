package store

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profile is one (host, token) pair, plus what the holder told us about it.
//
// The split between fact and statement is the important part and is preserved all the way
// to `whoami` (§5.4). Host, Token, Prefix and VerifiedAt are things the CLI knows.
// Scopes and ExpiresOn are things the holder typed at login, because the API has no
// introspection endpoint and will not confirm either — so they are stored, but never
// presented as though the server agreed with them.
type Profile struct {
	Host   string `toml:"host"`
	Token  string `toml:"token"`
	Prefix string `toml:"prefix"`
	Name   string `toml:"name"`

	// Stated by the holder at login. Not authoritative — see §12 gap 1, which asks for the
	// token introspection endpoint that would make these facts.
	Scopes    []string `toml:"scopes"`
	ExpiresOn string   `toml:"expires_on"`

	// VerifiedAt is when a probe last proved the token authenticates. A fact, but only
	// about that instant: the token may have been revoked a second later.
	VerifiedAt string `toml:"verified_at"`
}

// Credentials is the whole file.
type Credentials struct {
	Profiles map[string]Profile `toml:"profiles"`
}

// PermissionError is returned when the credentials file is readable by anyone but its
// owner. It is a distinct type because the command layer maps it to exit 2 — a refusal
// before anything was sent — and because the remedy is a specific command the message
// should name.
//
// Finding and Remedy are filled in by the platform, because the question "can anyone else
// read this?" has two genuinely different answers. On Unix it is a mode and the fix is
// chmod. On Windows there is no mode — Go synthesises one from the read-only attribute
// and it says nothing about who can read the file — so the answer is in the DACL and the
// fix is icacls. A single message would have had to be vague about both.
type PermissionError struct {
	Path string

	// Finding states what is wrong, in the terms the operating system uses.
	Finding string

	// Remedy is the command that fixes it, ready to paste.
	Remedy string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf(
		"%s\n"+
			"dossier will not use a credentials file it cannot vouch for.\n\n"+
			"Fix it with:\n\n    %s\n",
		e.Finding, e.Remedy,
	)
}

// CheckOwnerOnly reports whether path is readable only by the account running this
// process, returning a *PermissionError describing the exposure if it is not.
//
// Exported as the counterpart to RestrictToOwner, so that anything writing something
// private can assert the property it just asked for rather than assuming. On Unix that
// assertion is a mode comparison and looks redundant; on Windows it is the only way to
// know, because the mode there is synthetic and says nothing about access.
func CheckOwnerOnly(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("could not inspect %s: %w", path, err)
	}
	return checkOwnerOnly(path, info)
}

// LoadCredentials reads the credentials file.
//
// A missing file is not an error: it is the state every machine starts in, and `dossier
// schema` and `dossier login` both have to work there. An empty Credentials is returned
// instead, and it is the *command* that decides whether the absence of a token matters.
func LoadCredentials(p Paths) (*Credentials, error) {
	path := p.credentialsFile()

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &Credentials{Profiles: map[string]Profile{}}, nil
	case err != nil:
		return nil, fmt.Errorf("could not inspect %s: %w", path, err)
	}

	// The refusal. If anyone but the owner can read the file, the token is exposed, and
	// there is no degraded mode where the CLI carries on with a warning: a warning on
	// stderr in a cron job is a line nobody reads, and the credential is exposed either
	// way. What "anyone but the owner" means is the platform's to answer — see
	// permissions_unix.go and permissions_windows.go.
	if err := checkOwnerOnly(path, info); err != nil {
		return nil, err
	}

	var creds Credentials
	if _, err := toml.DecodeFile(path, &creds); err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	if creds.Profiles == nil {
		creds.Profiles = map[string]Profile{}
	}
	return &creds, nil
}

// Save writes the credentials file, creating its directory at 0700 and the file at 0600.
func (c *Credentials) Save(p Paths) error {
	if err := ensureDir(p.ConfigDir, dirMode); err != nil {
		return err
	}

	var out strings.Builder
	out.WriteString("# dossier credentials. Mode 0600 — dossier refuses to read this file if\n")
	out.WriteString("# anyone else on this machine can. Tokens here are bearer credentials:\n")
	out.WriteString("# they are the whole authentication, and dossier cannot revoke them.\n")
	out.WriteString("# Revoke from Settings -> API tokens.\n\n")

	if err := toml.NewEncoder(&out).Encode(c); err != nil {
		return fmt.Errorf("could not encode the credentials: %w", err)
	}
	return writeFileAtomic(p.credentialsFile(), []byte(out.String()), fileMode)
}

// Names returns the profile names, sorted, so `profiles list` has a stable order rather
// than Go's randomised map iteration.
func (c *Credentials) Names() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get returns a profile by name.
func (c *Credentials) Get(name string) (Profile, bool) {
	profile, ok := c.Profiles[name]
	return profile, ok
}

// Put stores a profile.
func (c *Credentials) Put(name string, profile Profile) {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = profile
}

// Delete removes a profile, reporting whether it was there.
func (c *Credentials) Delete(name string) bool {
	if _, ok := c.Profiles[name]; !ok {
		return false
	}
	delete(c.Profiles, name)
	return true
}
