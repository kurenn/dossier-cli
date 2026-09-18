package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerRoundTrip(t *testing.T) {
	paths := PathsIn(t.TempDir())
	created := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	entry := &MintEntry{
		Key:         "3f7a1c2e-4b5d-4e6f-8a9b-0c1d2e3f4a5b",
		Body:        []byte(`{"field_ids":[1],"expires_at":null}`),
		Digest:      "abc123",
		Host:        "https://dossier.global",
		ProfileName: "default",
		CreatedAt:   created,
	}
	if err := paths.SaveMint(entry); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}

	loaded, err := paths.LoadMint(entry.Key)
	if err != nil {
		t.Fatalf("LoadMint: %v", err)
	}
	if string(loaded.Body) != string(entry.Body) {
		t.Errorf("body = %q, want %q", loaded.Body, entry.Body)
	}
	if !loaded.CreatedAt.Equal(created) {
		t.Errorf("created_at = %s, want %s", loaded.CreatedAt, created)
	}
	if loaded.Host != entry.Host || loaded.ProfileName != entry.ProfileName {
		t.Errorf("host/profile did not survive: %+v", loaded)
	}

	if err := paths.DeleteMint(entry.Key); err != nil {
		t.Fatalf("DeleteMint: %v", err)
	}
	if _, err := paths.LoadMint(entry.Key); !errors.Is(err, ErrNoEntry) {
		t.Errorf("after delete, LoadMint returned %v, want ErrNoEntry", err)
	}

	// Deleting twice is not an error: the caller's intent is "this key is resolved", and
	// it being already gone satisfies that.
	if err := paths.DeleteMint(entry.Key); err != nil {
		t.Errorf("a second DeleteMint failed: %v", err)
	}
}

// The ledger names which fields of a vault went to whom, which is not something to leave
// world-readable on a shared machine — even though it holds no secret.
func TestLedgerFileAndDirectoryAreLockedDown(t *testing.T) {
	paths := PathsIn(t.TempDir())

	entry := &MintEntry{Key: "a-key", Body: []byte("{}"), CreatedAt: time.Now()}
	if err := paths.SaveMint(entry); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}

	info, err := os.Stat(filepath.Join(paths.StateDir, "mints", "a-key.json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}

	dir, err := os.Stat(filepath.Join(paths.StateDir, "mints"))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700", perm)
	}
}

// The ledger lives under StateDir, not CacheDir. A cache is disposable by definition and
// something will eventually clear it; an unresolved mint must outlive that, or the ledger
// guarantees nothing.
func TestLedgerLivesInStateNotCache(t *testing.T) {
	root := t.TempDir()
	paths := PathsIn(root)

	if err := paths.SaveMint(&MintEntry{Key: "k", Body: []byte("{}"), CreatedAt: time.Now()}); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}

	if _, err := os.Stat(filepath.Join(paths.StateDir, "mints", "k.json")); err != nil {
		t.Errorf("not under StateDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.CacheDir, "mints")); err == nil {
		t.Error("the ledger wrote into the cache, which is safe to delete at any time")
	}
}

// A key from --idempotency-key reaches the filesystem, so anything that could name a path
// is refused before it becomes one.
func TestValidKeyRefusesAnythingThatCouldBeAPath(t *testing.T) {
	good := []string{
		"3f7a1c2e-4b5d-4e6f-8a9b-0c1d2e3f4a5b",
		"batch_2026_09_18",
		"a",
		"A1-b2_C3",
	}
	for _, key := range good {
		if !ValidKey(key) {
			t.Errorf("ValidKey(%q) = false, want true", key)
		}
	}

	bad := []string{
		"", "..", "../etc/passwd", "a/b", "a\\b", "with space", "n\nl",
		"semi;colon", "quote\"", "dot.json", "tilde~",
		string(make([]byte, 256)),
	}
	for _, key := range bad {
		if ValidKey(key) {
			t.Errorf("ValidKey(%q) = true, want false", key)
		}
	}
}

func TestSaveMintRefusesAPathLikeKey(t *testing.T) {
	paths := PathsIn(t.TempDir())

	if err := paths.SaveMint(&MintEntry{Key: "../escape", Body: []byte("{}")}); err == nil {
		t.Fatal("SaveMint accepted a key that names a parent directory")
	}
	if _, err := paths.LoadMint("../escape"); err == nil {
		t.Fatal("LoadMint accepted a traversing key")
	}
}

// 24 h is the server's own claim TTL, and matching it is the point. Past that line the
// same key is a new mint rather than a replay of the old one.
func TestStalenessMatchesTheServersClaimTTL(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		age       time.Duration
		wantStale bool
	}{
		{time.Minute, false},
		{23 * time.Hour, false},
		{LedgerTTL - time.Second, false},
		{LedgerTTL, true},
		{25 * time.Hour, true},
	}

	for _, testCase := range cases {
		entry := &MintEntry{CreatedAt: now.Add(-testCase.age)}
		if got := entry.Stale(now); got != testCase.wantStale {
			t.Errorf("at %s old, Stale() = %t, want %t", testCase.age, got, testCase.wantStale)
		}
	}

	if LedgerTTL != 24*time.Hour {
		t.Errorf("LedgerTTL = %s; it must match the API's 24h idempotency claim", LedgerTTL)
	}
}

// The one field that makes §6.3 decidable: whether a 5xx has been seen under this key is
// what tells an ordinary concurrent claim apart from one that will never resolve.
func TestServerErrorSurvivesAReload(t *testing.T) {
	paths := PathsIn(t.TempDir())
	at := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	entry := &MintEntry{Key: "k", Body: []byte("{}"), CreatedAt: at}
	if entry.SawServerError() {
		t.Error("a fresh entry claims to have seen a server error")
	}

	entry.ServerErrorAt = &at
	if err := paths.SaveMint(entry); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}

	loaded, err := paths.LoadMint("k")
	if err != nil {
		t.Fatalf("LoadMint: %v", err)
	}
	if !loaded.SawServerError() {
		t.Error("the recorded server error did not survive a reload, which would make a " +
			"409 look like a claim that resolves itself")
	}
}

func TestListMintsIsOldestFirstAndSurvivesARottenFile(t *testing.T) {
	paths := PathsIn(t.TempDir())
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	for i, key := range []string{"third", "first", "second"} {
		age := []time.Duration{3 * time.Hour, time.Hour, 2 * time.Hour}[i]
		if err := paths.SaveMint(&MintEntry{
			Key:       key,
			Body:      []byte("{}"),
			CreatedAt: now.Add(-age),
		}); err != nil {
			t.Fatalf("SaveMint: %v", err)
		}
	}

	// One corrupt entry must not hide the others: a holder running this is trying to find
	// out what is outstanding, and a total failure tells them nothing.
	if err := os.WriteFile(filepath.Join(paths.StateDir, "mints", "rotten.json"),
		[]byte("not json at all"), 0o600); err != nil {
		t.Fatalf("writing the rotten file: %v", err)
	}

	entries, err := paths.ListMints()
	if err != nil {
		t.Fatalf("ListMints: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d entries, want 3", len(entries))
	}
	for i, want := range []string{"third", "second", "first"} {
		if entries[i].Key != want {
			t.Errorf("entry %d = %s, want %s (oldest first)", i, entries[i].Key, want)
		}
	}
}

func TestListMintsOnAVirginMachineIsEmptyNotAnError(t *testing.T) {
	paths := PathsIn(t.TempDir())

	entries, err := paths.ListMints()
	if err != nil {
		t.Fatalf("ListMints on a machine that has never minted: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d entries, want none", len(entries))
	}
}

// A file whose contents name a different key than its filename is refused. It means the
// ledger was edited by hand, and resuming from it would send bytes under a key they were
// never claimed with.
func TestLoadMintRefusesAMisfiledEntry(t *testing.T) {
	paths := PathsIn(t.TempDir())

	if err := paths.SaveMint(&MintEntry{Key: "real-key", Body: []byte("{}")}); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}
	if err := os.Rename(
		filepath.Join(paths.StateDir, "mints", "real-key.json"),
		filepath.Join(paths.StateDir, "mints", "other-key.json"),
	); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if _, err := paths.LoadMint("other-key"); err == nil {
		t.Fatal("a misfiled entry was accepted")
	}
}
