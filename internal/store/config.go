package store

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultProfileName is the profile used when nothing else says otherwise.
const DefaultProfileName = "default"

// DefaultHost is the production vault. A holder running against a local Rails app passes
// --host; this is the value that makes the common case need no flags at all.
const DefaultHost = "https://dossier.global"

// Config is the non-secret half of what the CLI stores. It lives in its own file at 0644
// because there is nothing in it to protect, and putting it at 0600 beside the
// credentials would blur which of the two files actually matters.
type Config struct {
	DefaultProfile string `toml:"default_profile"`

	// existed records whether a config file was actually on disk. Needed because
	// DefaultProfile is populated with a fallback when it is not, so its value alone
	// cannot distinguish "the holder chose `default`" from "nobody has chosen anything".
	// login relies on the difference: on a fresh machine it should adopt whatever profile
	// was just created, and on a configured one it must not silently move the default.
	existed bool
}

// Existed reports whether a config file was present when this was loaded.
func (c *Config) Existed() bool { return c.existed }

// LoadConfig reads config.toml. A missing file yields defaults — the CLI has to work on a
// machine it has never run on.
func LoadConfig(p Paths) (*Config, error) {
	path := p.configFile()

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return &Config{DefaultProfile: DefaultProfileName}, nil
	} else if err != nil {
		return nil, fmt.Errorf("could not inspect %s: %w", path, err)
	}

	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	if config.DefaultProfile == "" {
		config.DefaultProfile = DefaultProfileName
	}
	config.existed = true
	return &config, nil
}

// Save writes config.toml.
func (c *Config) Save(p Paths) error {
	if err := ensureDir(p.ConfigDir, dirMode); err != nil {
		return err
	}

	var out strings.Builder
	out.WriteString("# dossier configuration. Nothing secret lives here — the token is in\n")
	out.WriteString("# credentials.toml, which is mode 0600.\n\n")
	if err := toml.NewEncoder(&out).Encode(c); err != nil {
		return fmt.Errorf("could not encode the config: %w", err)
	}
	return writeFileAtomic(p.configFile(), []byte(out.String()), configMode)
}

// Resolution is the outcome of working out which profile, host and token to use.
type Resolution struct {
	ProfileName string
	Profile     Profile
	Host        string
	Token       string

	// TokenFromEnv records that DOSSIER_TOKEN supplied the token. `whoami` reports this,
	// because a holder who cannot find the token in the file needs to be told why.
	TokenFromEnv bool

	// ProfileMissing means no stored profile of this name exists. Not an error here —
	// `dossier schema` needs no profile at all, and `login` is how one comes to exist — so
	// the command decides whether it matters.
	ProfileMissing bool
}

// Resolve applies the precedence order from §5.3, highest first:
//
//  1. DOSSIER_TOKEN — for CI and cron, where a credentials file may not exist at all.
//     When set, it supplies only the *token*; the profile is still consulted for the host,
//     because a token with no host is not enough to make a request.
//  2. An explicit --profile.
//  3. DOSSIER_PROFILE.
//  4. The default_profile from config.toml.
//
// There is deliberately no --token flag at any level. A token on the command line is in
// the shell history and in `ps` output for every user on the machine; login refuses it
// for the same reason (§5.1), and adding it here would reopen the hole from the other
// side.
func Resolve(config *Config, creds *Credentials, flagProfile, flagHost string) Resolution {
	name := firstNonEmpty(
		flagProfile,
		os.Getenv("DOSSIER_PROFILE"),
		config.DefaultProfile,
		DefaultProfileName,
	)

	resolution := Resolution{ProfileName: name}

	profile, found := creds.Get(name)
	if !found {
		resolution.ProfileMissing = true
	}
	resolution.Profile = profile
	resolution.Token = profile.Token
	resolution.Host = profile.Host

	if envToken := strings.TrimSpace(os.Getenv("DOSSIER_TOKEN")); envToken != "" {
		resolution.Token = envToken
		resolution.TokenFromEnv = true
	}

	// --host beats the profile's host, and the built-in default is the last resort. A
	// stored profile with no host is a file someone hand-edited; falling through to the
	// default is friendlier than refusing, and `whoami` shows which host is in play.
	if flagHost != "" {
		resolution.Host = flagHost
	}
	if resolution.Host == "" {
		resolution.Host = DefaultHost
	}

	return resolution
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
