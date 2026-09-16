package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// SchemaTTL is how long a cached discovery document is trusted.
//
// Twenty-four hours is a deliberate middle: the document changes only when the API ships
// a new endpoint, scope or limit, so caching it is what keeps `fields list` from spending
// a request on discovery every time. Longer would mean a limit change takes days to
// reach a client that checks caps before uploading; shorter would spend requests to
// re-learn constants that had not moved. `--refresh` exists for the holder who knows
// something changed.
const SchemaTTL = 24 * time.Hour

// CachedSchema is the cache envelope: the document as the server sent it, plus when.
type CachedSchema struct {
	Host      string          `json:"host"`
	FetchedAt time.Time       `json:"fetched_at"`
	Document  json.RawMessage `json:"document"`
}

// Fresh reports whether the cache is still inside its TTL.
func (c *CachedSchema) Fresh(now time.Time) bool {
	return now.Sub(c.FetchedAt) < SchemaTTL
}

// Age returns how old the cached document is.
func (c *CachedSchema) Age(now time.Time) time.Duration {
	return now.Sub(c.FetchedAt)
}

// LoadSchema reads the cached document for a host. A cache miss returns (nil, nil): the
// absence of a cache is normal, not a failure.
//
// A corrupt or unreadable cache is also (nil, nil) rather than an error. The cache is
// disposable by definition — the authoritative copy is one HTTP request away — so
// failing a command because a cache file was truncated by a full disk would turn a
// non-problem into an outage.
func LoadSchema(p Paths, host string) (*CachedSchema, error) {
	raw, err := os.ReadFile(p.schemaCacheFile(host))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, nil
	}

	var cached CachedSchema
	if err := json.Unmarshal(raw, &cached); err != nil {
		return nil, nil
	}
	// A cache written for a different host means the hash collided or the file was moved
	// by hand. Either way it is not this host's document.
	if cached.Host != host {
		return nil, nil
	}
	if len(cached.Document) == 0 {
		return nil, nil
	}
	return &cached, nil
}

// SaveSchema caches a document for a host.
//
// Written at 0600 in a 0700 directory even though the discovery document is public and
// un-authenticated. Not because it is secret, but because the file name embeds a hash of
// the host: on a shared machine, a world-readable cache directory would tell every other
// user which vaults this account talks to, and that is inventory nobody needs.
func SaveSchema(p Paths, host string, document []byte) error {
	if err := ensureDir(p.CacheDir, dirMode); err != nil {
		return err
	}

	payload, err := json.Marshal(CachedSchema{
		Host:      host,
		FetchedAt: time.Now().UTC(),
		Document:  json.RawMessage(document),
	})
	if err != nil {
		return fmt.Errorf("could not encode the schema cache: %w", err)
	}
	return writeFileAtomic(p.schemaCacheFile(host), payload, fileMode)
}

// ForgetSchema removes a host's cached document, for --refresh and for logout.
func ForgetSchema(p Paths, host string) error {
	err := os.Remove(p.schemaCacheFile(host))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not clear the schema cache: %w", err)
	}
	return nil
}
