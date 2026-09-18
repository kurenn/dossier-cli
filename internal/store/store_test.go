package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	return PathsIn(t.TempDir())
}

// The permission guarantee, asserted on the artefacts rather than the intent: a comment
// saying 0600 is worth nothing, the mode bits on disk are the claim.
func TestCredentialsAreWrittenAt0600InA0700Directory(t *testing.T) {
	paths := testPaths(t)

	creds := &Credentials{Profiles: map[string]Profile{
		"default": {Host: "https://dossier.global", Token: "dsk_secret", Prefix: "abcd1234"},
	}}
	if err := creds.Save(paths); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Asserted through CheckOwnerOnly rather than by comparing a mode, so this means
	// the same thing on Windows — where the mode is synthetic and a comparison would
	// pass or fail for reasons unrelated to who can read the file.
	if err := CheckOwnerOnly(paths.credentialsFile()); err != nil {
		t.Errorf("credentials.toml is not owner-only: %v", err)
	}
	if err := CheckOwnerOnly(paths.ConfigDir); err != nil {
		t.Errorf("the config directory is not owner-only: %v", err)
	}
}

// The refusal, which is the whole posture: a credential the machine can read is already a
// different credential than the one you thought you had. There is deliberately no
// degraded mode that carries on with a warning.
func TestLoadCredentialsRefusesAWorldReadableFile(t *testing.T) {
	paths := testPaths(t)

	creds := &Credentials{Profiles: map[string]Profile{"default": {Token: "dsk_secret"}}}
	if err := creds.Save(paths); err != nil {
		t.Fatalf("Save: %v", err)
	}

	expose(t, paths.credentialsFile())

	_, err := LoadCredentials(paths)

	var permErr *PermissionError
	if !errors.As(err, &permErr) {
		t.Fatalf("a file others can read was accepted (err = %v)", err)
	}
	// The message must name the fix, not just the problem — and the fix differs by
	// platform, which is why PermissionError carries it rather than formatting one.
	if !strings.Contains(permErr.Error(), exposureRemedy) {
		t.Errorf("the error does not name the remedy:\n%s", permErr)
	}
	if !strings.Contains(permErr.Error(), "will not use a credentials file it cannot vouch for") {
		t.Errorf("the error does not state the refusal:\n%s", permErr)
	}
}

// The round trip, and on Windows the regression test for this whole area: Save writes
// the file, Load reads it back without refusing. Before the DACL work, Load rejected
// every credentials file on every Windows machine — including the one login had just
// written — because Go reports mode 0666 for an ordinary file there and the Unix check
// saw group and other bits that do not exist.
func TestSaveThenLoadIsAcceptedOnThisPlatform(t *testing.T) {
	paths := testPaths(t)
	creds := &Credentials{Profiles: map[string]Profile{"default": {Token: "dsk_secret"}}}
	if err := creds.Save(paths); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadCredentials(paths)
	if err != nil {
		t.Fatalf("a file this package just wrote was refused: %v", err)
	}
	if loaded.Profiles["default"].Token != "dsk_secret" {
		t.Errorf("token = %q, want dsk_secret", loaded.Profiles["default"].Token)
	}
}

// Every machine starts with no credentials file, and `dossier schema` and `dossier login`
// both have to work there. Absence is a state, not a failure.
func TestLoadCredentialsTreatsAMissingFileAsEmpty(t *testing.T) {
	creds, err := LoadCredentials(testPaths(t))
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if len(creds.Profiles) != 0 {
		t.Errorf("expected no profiles, got %v", creds.Profiles)
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	paths := testPaths(t)

	original := Profile{
		Host:       "http://localhost:3000",
		Token:      "dsk_abcdefghijklmnopqrstuvwxyz123456",
		Prefix:     "abcdefgh",
		Name:       "laptop",
		Scopes:     []string{"fields:read", "shares:mint"},
		ExpiresOn:  "2026-12-14",
		VerifiedAt: "2026-09-16T12:00:00Z",
	}

	creds := &Credentials{}
	creds.Put("work", original)
	if err := creds.Save(paths); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadCredentials(paths)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	got, found := reloaded.Get("work")
	if !found {
		t.Fatal("the profile did not survive the round trip")
	}
	if got.Token != original.Token || got.Prefix != original.Prefix || got.ExpiresOn != original.ExpiresOn {
		t.Errorf("profile changed: %+v", got)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "fields:read" {
		t.Errorf("scopes changed: %v", got.Scopes)
	}
}

// Go randomises map iteration, so an unsorted list would reorder itself between runs and
// could not be diffed or golden-tested.
func TestNamesAreSorted(t *testing.T) {
	creds := &Credentials{}
	for _, name := range []string{"zulu", "alpha", "mike"} {
		creds.Put(name, Profile{})
	}

	got := creds.Names()
	if len(got) != 3 || got[0] != "alpha" || got[1] != "mike" || got[2] != "zulu" {
		t.Errorf("Names() = %v, want sorted", got)
	}
}

func TestDelete(t *testing.T) {
	creds := &Credentials{}
	creds.Put("default", Profile{})

	if !creds.Delete("default") {
		t.Error("Delete reported nothing removed")
	}
	if creds.Delete("default") {
		t.Error("Delete reported a second removal")
	}
}

// A crash or a full disk mid-write must never leave a half-written token: the file is
// either the old one or the new one.
func TestWriteFileAtomicLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.toml")

	if err := writeFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeFileAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q", got)
	}

	// No temporary files left behind. A .tmp-* file holding a token would defeat the
	// point of the 0600 file next to it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("a temporary file survived: %s", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly one file, found %d", len(entries))
	}
}

// MkdirAll applies its mode only when it creates. A directory that already existed as
// 0755 would otherwise keep those permissions and the 0700 guarantee would be a comment
// rather than a fact.
func TestEnsureDirTightensAnExistingLooseDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dossier")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := ensureDir(dir, 0o700); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	if err := CheckOwnerOnly(dir); err != nil {
		t.Errorf("the directory is not owner-only: %v", err)
	}
}

func TestConfigRoundTripAndDefaults(t *testing.T) {
	paths := testPaths(t)

	config, err := LoadConfig(paths)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.DefaultProfile != DefaultProfileName {
		t.Errorf("a missing config gave DefaultProfile = %q", config.DefaultProfile)
	}

	config.DefaultProfile = "work"
	if err := config.Save(paths); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadConfig(paths)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if reloaded.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q", reloaded.DefaultProfile)
	}

	// config.toml holds nothing secret, and making it 0600 beside the credentials would
	// blur which of the two files actually matters. Asserted per platform, because
	// "0644" is not a thing Windows has.
	assertNotSecret(t, paths.configFile())
}

// The precedence order from §5.3, each level asserted against the one below it.
func TestResolvePrecedence(t *testing.T) {
	// Cleared because DOSSIER_TOKEN sits at the top of the very order under test: a
	// developer with one exported — which anyone testing the CLI against a real vault will
	// have — would see the two subtests that assert a *stored* token fail with the token
	// from their shell. The subtests that want one set it themselves.
	t.Setenv("DOSSIER_TOKEN", "")
	t.Setenv("DOSSIER_PROFILE", "")

	creds := &Credentials{Profiles: map[string]Profile{
		"default": {Host: "https://default.example", Token: "dsk_default"},
		"work":    {Host: "https://work.example", Token: "dsk_work"},
		"env":     {Host: "https://env.example", Token: "dsk_env"},
	}}
	config := &Config{DefaultProfile: "default"}

	t.Run("the config's default profile is the floor", func(t *testing.T) {
		got := Resolve(config, creds, "", "")
		if got.ProfileName != "default" || got.Token != "dsk_default" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("DOSSIER_PROFILE beats the config", func(t *testing.T) {
		t.Setenv("DOSSIER_PROFILE", "work")
		got := Resolve(config, creds, "", "")
		if got.ProfileName != "work" || got.Token != "dsk_work" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("--profile beats DOSSIER_PROFILE", func(t *testing.T) {
		t.Setenv("DOSSIER_PROFILE", "work")
		got := Resolve(config, creds, "env", "")
		if got.ProfileName != "env" {
			t.Errorf("ProfileName = %q", got.ProfileName)
		}
	})

	// DOSSIER_TOKEN supplies only the token: a token with no host is not enough to make a
	// request, so the profile is still consulted for the host.
	t.Run("DOSSIER_TOKEN overrides the token but not the host", func(t *testing.T) {
		t.Setenv("DOSSIER_TOKEN", "dsk_fromenv")
		got := Resolve(config, creds, "work", "")

		if got.Token != "dsk_fromenv" {
			t.Errorf("Token = %q", got.Token)
		}
		if !got.TokenFromEnv {
			t.Error("TokenFromEnv was not set")
		}
		if got.Host != "https://work.example" {
			t.Errorf("Host = %q, want the profile's", got.Host)
		}
	})

	t.Run("--host beats the profile's host", func(t *testing.T) {
		got := Resolve(config, creds, "work", "http://localhost:3000")
		if got.Host != "http://localhost:3000" {
			t.Errorf("Host = %q", got.Host)
		}
	})

	t.Run("an unknown profile is reported, not invented", func(t *testing.T) {
		got := Resolve(config, creds, "nope", "")
		if !got.ProfileMissing {
			t.Error("ProfileMissing was not set")
		}
		if got.Host != DefaultHost {
			t.Errorf("Host = %q, want the built-in default", got.Host)
		}
	})

	t.Run("a whitespace-only DOSSIER_TOKEN is ignored", func(t *testing.T) {
		// Otherwise `export DOSSIER_TOKEN=` in a shell profile would silently blank the
		// stored token for every command.
		t.Setenv("DOSSIER_TOKEN", "   ")
		got := Resolve(config, creds, "work", "")
		if got.TokenFromEnv || got.Token != "dsk_work" {
			t.Errorf("got %+v", got)
		}
	})
}

// The cache is keyed per host so a holder switching between production and a local Rails
// app cannot read one's discovery document while talking to the other.
func TestSchemaCacheIsKeyedPerHost(t *testing.T) {
	paths := testPaths(t)

	if err := SaveSchema(paths, "https://a.example", []byte(`{"scopes":["a"]}`)); err != nil {
		t.Fatalf("SaveSchema: %v", err)
	}
	if err := SaveSchema(paths, "https://b.example", []byte(`{"scopes":["b"]}`)); err != nil {
		t.Fatalf("SaveSchema: %v", err)
	}

	a, err := LoadSchema(paths, "https://a.example")
	if err != nil || a == nil {
		t.Fatalf("LoadSchema(a) = %v, %v", a, err)
	}
	if !strings.Contains(string(a.Document), `"a"`) {
		t.Errorf("host a got %q", a.Document)
	}

	b, _ := LoadSchema(paths, "https://b.example")
	if b == nil || !strings.Contains(string(b.Document), `"b"`) {
		t.Errorf("host b got %v", b)
	}

	missing, err := LoadSchema(paths, "https://c.example")
	if err != nil || missing != nil {
		t.Errorf("an uncached host returned %v, %v", missing, err)
	}
}

func TestSchemaCacheFreshness(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	fresh := &CachedSchema{FetchedAt: now.Add(-23 * time.Hour)}
	if !fresh.Fresh(now) {
		t.Error("a 23h-old cache was treated as stale")
	}

	stale := &CachedSchema{FetchedAt: now.Add(-25 * time.Hour)}
	if stale.Fresh(now) {
		t.Error("a 25h-old cache was treated as fresh")
	}
	if stale.Age(now) != 25*time.Hour {
		t.Errorf("Age = %s", stale.Age(now))
	}
}

// The cache is disposable by definition — the authoritative copy is one request away — so
// a truncated file must be a cache miss, not an outage.
func TestLoadSchemaTreatsCorruptionAsAMiss(t *testing.T) {
	paths := testPaths(t)
	if err := ensureDir(paths.CacheDir, dirMode); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	for _, content := range []string{"", "not json", `{"host":"https://other.example","document":{}}`, `{"host":"https://a.example"}`} {
		if err := os.WriteFile(paths.schemaCacheFile("https://a.example"), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		got, err := LoadSchema(paths, "https://a.example")
		if err != nil {
			t.Errorf("content %q returned an error: %v", content, err)
		}
		if got != nil {
			t.Errorf("content %q was accepted as a hit", content)
		}
	}
}

func TestForgetSchema(t *testing.T) {
	paths := testPaths(t)

	if err := SaveSchema(paths, "https://a.example", []byte(`{}`)); err != nil {
		t.Fatalf("SaveSchema: %v", err)
	}
	if err := ForgetSchema(paths, "https://a.example"); err != nil {
		t.Fatalf("ForgetSchema: %v", err)
	}
	if got, _ := LoadSchema(paths, "https://a.example"); got != nil {
		t.Error("the cache survived ForgetSchema")
	}

	// Forgetting something that is not there is the state the caller asked for, so it
	// must not error — logout calls this unconditionally.
	if err := ForgetSchema(paths, "https://never-cached.example"); err != nil {
		t.Errorf("ForgetSchema on a miss returned %v", err)
	}
}

func TestConfigExistedDistinguishesAbsenceFromAChoice(t *testing.T) {
	paths := testPaths(t)

	// DefaultProfile is populated with a fallback when there is no file, so its value
	// alone cannot tell "the holder chose `default`" from "nobody has chosen anything".
	// login depends on the difference: it adopts the new profile on a fresh machine and
	// must not move the default on a configured one.
	fresh, err := LoadConfig(paths)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if fresh.Existed() {
		t.Error("a missing config reported that it existed")
	}
	if fresh.DefaultProfile != DefaultProfileName {
		t.Errorf("DefaultProfile = %q", fresh.DefaultProfile)
	}

	if err := fresh.Save(paths); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadConfig(paths)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reloaded.Existed() {
		t.Error("a saved config reported that it did not exist")
	}
}
