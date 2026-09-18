package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LedgerTTL is how long an entry can be resumed for.
//
// It is the server's own claim TTL, and matching it is the whole point rather than a
// coincidence. Past 24 h the server may hand the claim to a new create
// (`ApiIdempotencyKey.reclaim_stale`), so a replay that looks like a replay to the CLI
// would be a *fresh mint* to the API — the exact double-mint the ledger exists to
// prevent. Refusing to resume past this line is what keeps the CLI from being the client
// that triggers it.
const LedgerTTL = 24 * time.Hour

// MintEntry is one in-flight or unresolved mint.
//
// What it does not hold is as important as what it does: no PIN, and no share token. The
// PIN exists in one response and is printed once; writing it here would turn a recovery
// aid into a secret at rest, with a 24 h lifetime and no reason to exist — a resume
// replays the server's own stored response, which carries the PIN again.
type MintEntry struct {
	Key string `json:"key"`

	// Body is the exact bytes sent. Stored rather than the input that produced them
	// because the API's idempotency identity is the key plus the raw body: a resume that
	// re-serialised would risk a different byte sequence and earn
	// `409 idempotency_key_reused`, which is unrecoverable for that key.
	Body []byte `json:"body"`

	// Digest is the SHA-256 of Body, so a truncated or hand-edited ledger file is caught
	// before the CLI sends something under a key that would then be permanently spent.
	Digest string `json:"digest"`

	Host        string `json:"host"`
	ProfileName string `json:"profile"`

	CreatedAt time.Time `json:"created_at"`

	// ServerErrorAt records that a 5xx was seen under this key.
	//
	// This single field is what makes the plan's §6.3 decidable. A
	// `409 request_in_flight` means "wait and retry" normally, but after a 5xx it means
	// the batch may have been written while the response could not be stored — a claim
	// that will never resolve itself. The two are the same status with the same body, and
	// the only thing that tells them apart is whether this is set.
	ServerErrorAt *time.Time `json:"server_error_at,omitempty"`

	// Attempts counts sends, for the prose that tells a holder what has happened so far.
	Attempts int `json:"attempts"`
}

// Stale reports whether the entry is past the server's claim TTL.
func (e *MintEntry) Stale(now time.Time) bool {
	return now.Sub(e.CreatedAt) >= LedgerTTL
}

// SawServerError reports whether a 5xx has been recorded under this key.
func (e *MintEntry) SawServerError() bool { return e.ServerErrorAt != nil }

// Age is how long the entry has existed.
func (e *MintEntry) Age(now time.Time) time.Duration { return now.Sub(e.CreatedAt) }

// ErrNoEntry is returned when a key has no ledger file.
var ErrNoEntry = errors.New("no ledger entry for that key")

// mintsDir is under StateDir, not CacheDir. A cache is disposable by definition and
// something will eventually clear it; an unresolved mint must outlive that or the ledger
// guarantees nothing.
func (p Paths) mintsDir() string { return filepath.Join(p.StateDir, "mints") }

// mintFile names the entry after its key.
//
// The key is validated before it reaches here (see ValidKey), because it can come from
// --idempotency-key: an unvalidated key containing a slash or `..` would write outside the
// ledger directory.
func (p Paths) mintFile(key string) string {
	return filepath.Join(p.mintsDir(), key+".json")
}

// ValidKey reports whether a key is safe to use as a filename and acceptable to the API.
//
// Deliberately narrow. A UUID is what the CLI generates, and the only reason to accept
// anything else is --idempotency-key, where a holder is coordinating with something
// outside this tool. Letters, digits, dash and underscore cover that without ever
// producing a path component.
func ValidKey(key string) bool {
	if key == "" || len(key) > 255 {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// SaveMint writes an entry, atomically and at 0600.
//
// 0600 even though there is no secret in it: the body names which fields of a vault were
// released to whom, which is not something to leave world-readable on a shared machine.
func (p Paths) SaveMint(entry *MintEntry) error {
	if !ValidKey(entry.Key) {
		return fmt.Errorf("%q is not a usable idempotency key", entry.Key)
	}
	if err := ensureDir(p.StateDir, dirMode); err != nil {
		return err
	}
	if err := ensureDir(p.mintsDir(), dirMode); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("could not encode the ledger entry: %w", err)
	}
	return writeFileAtomic(p.mintFile(entry.Key), append(raw, '\n'), fileMode)
}

// LoadMint reads one entry, and verifies the body against its digest.
func (p Paths) LoadMint(key string) (*MintEntry, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("%q is not a usable idempotency key", key)
	}

	raw, err := os.ReadFile(p.mintFile(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoEntry
		}
		return nil, fmt.Errorf("could not read the ledger entry for %s: %w", key, err)
	}

	var entry MintEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("the ledger entry for %s is not readable: %w", key, err)
	}
	if entry.Key != key {
		return nil, fmt.Errorf("the ledger entry filed under %s names the key %s", key, entry.Key)
	}
	return &entry, nil
}

// DeleteMint removes an entry. A missing file is not an error: the caller's intent is
// "this key is resolved", and it being already gone satisfies that.
func (p Paths) DeleteMint(key string) error {
	if !ValidKey(key) {
		return fmt.Errorf("%q is not a usable idempotency key", key)
	}
	if err := os.Remove(p.mintFile(key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not remove the ledger entry for %s: %w", key, err)
	}
	return nil
}

// ListMints returns every entry, oldest first.
//
// Unreadable files are skipped rather than failing the listing: one corrupt entry must not
// hide the others, because a holder running this is trying to find out what is outstanding
// and a total failure tells them nothing.
func (p Paths) ListMints() ([]*MintEntry, error) {
	names, err := os.ReadDir(p.mintsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("could not read %s: %w", p.mintsDir(), err)
	}

	var entries []*MintEntry
	for _, name := range names {
		if name.IsDir() || !strings.HasSuffix(name.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(name.Name(), ".json")
		entry, err := p.LoadMint(key)
		if err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].CreatedAt.Before(entries[j-1].CreatedAt); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	return entries, nil
}
