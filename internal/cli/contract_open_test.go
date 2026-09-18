//go:build contract

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
)

// M4's done-whens, against the real recipient boundary.
//
// This file is the only place the CLI's recipient side meets a server that actually
// burns things, and the fixture's burn share can be opened exactly once per seed — so
// the tests that spend it say so, loudly, and there is at most one of them.

// openHarnessFor builds a CLI pointed at the live host with no credential at all. That
// is the point: `open` is the one command a recipient runs without an account, and a
// harness that quietly seeded a profile would not be testing it.
func openHarnessFor(t *testing.T) *harness {
	t.Helper()

	h := newHarness(t)
	h.app.HostFlag = contractHost(t)
	return h
}

func shareByKey(t *testing.T, data contractData, key string) (token, pin string) {
	t.Helper()

	for _, share := range data.Shares {
		if share.Key == key {
			return share.Token, share.PIN
		}
	}
	t.Fatalf("the fixture has no %q share; contract.json is from an older seed", key)
	return "", ""
}

// --- done-when 1: the right PIN opens, exit 0 -------------------------------------------

func TestContractOpenWithTheRightPINSucceeds(t *testing.T) {
	data := contractFixture(t)
	token, pin := shareByKey(t, data, "live")

	h := openHarnessFor(t)
	h.stdinString(pin + "\n")

	code := h.run("open", token, "--pin-stdin", "--host", contractHost(t))
	if code != exitcode.OK {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	stdout := h.stdout.String()
	// The fixture's live share releases the active fields, so a released block with real
	// values is the proof the whole round trip worked.
	if !strings.Contains(stdout, "RELEASED") {
		t.Errorf("no released block:\n%s", stdout)
	}
	if !strings.Contains(h.stderr.String(), "This open has been recorded for the holder.") {
		t.Errorf("the recipient was not told the open is recorded:\n%s", h.stderr.String())
	}
}

// The shape the renderer decodes is the shape the server sends. The fake in open_test.go
// claims a particular set of keys; this is what stops it drifting.
func TestContractTheOpenedShapeIsWhatTheFakeClaims(t *testing.T) {
	data := contractFixture(t)
	token, pin := shareByKey(t, data, "no_expiry")

	client, err := api.New(contractHost(t), api.WithNoWait(true))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	result, err := client.OpenDossier(context.Background(), token, pin, 30*time.Second)
	if err != nil {
		t.Fatalf("OpenDossier: %v", err)
	}

	// Every key the server actually sent, against the struct's tags. One-directional for
	// the same reason as TestContractResponseFieldsAreAllDecoded: the CLI may model keys
	// the server omits, but must not be blind to one it sends.
	var wire struct {
		Dossier map[string]json.RawMessage `json:"dossier"`
	}
	if err := json.Unmarshal(result.Raw, &wire); err != nil {
		t.Fatalf("unmarshalling the raw body: %v", err)
	}

	known := jsonTags(api.Dossier{})
	for key := range wire.Dossier {
		if !slices.Contains(known, key) {
			t.Errorf("the server sends dossier.%s but api.Dossier has no field for it", key)
		}
	}

	// A no-expiry dossier really does send null rather than omitting the key, which is
	// what lets the renderer tell "no deadline" from "a deadline it failed to parse".
	if result.Dossier.ExpiresAt != nil {
		t.Errorf("expires_at = %q on the no-expiry share", *result.Dossier.ExpiresAt)
	}
	if result.Dossier.Holder.Name == "" {
		t.Error("the holder snapshot came back empty")
	}
}

// --- done-when 2: a wrong PIN is exit 10 and leaves a pin-fail row -----------------------

func TestContractAWrongPINIsRefusedAndAudited(t *testing.T) {
	data := contractFixture(t)
	token, _ := shareByKey(t, data, "expiring")

	h := openHarnessFor(t)
	h.stdinString("000000\n")

	code := h.run("open", token, "--pin-stdin", "--host", contractHost(t))
	if code != exitcode.PinRefused {
		t.Fatalf("exit = %d, want %d:\n%s", code, exitcode.PinRefused, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), lockWarning) {
		t.Errorf("the lock warning is missing:\n%s", h.stderr.String())
	}

	// The other half of the done-when, and the reason it is worth asserting: the holder
	// can see the attempt. Read back through the holder's own API, which is where rule 9
	// says it lives.
	client := contractClient(t, contractToken(t, data, sharesReader))
	if !auditHasKind(t, client, token, "pin-fail") {
		t.Error("no pin-fail row appeared in the holder's audit trail")
	}
}

// --- done-whens 3 and 4: the burn dossier ------------------------------------------------

// TestContractTheBurnDossier is one test because the burn share is one resource.
//
// It can be opened exactly once per seed, so exactly one test may own it. Splitting the
// document refusal and the open into two tests looked tidier and was wrong twice over:
// the second to run found a share the first had spent, and which failed depended on
// source order. A single-use fixture gets a single owner, in the order the product
// defines rather than the order the file happens to be written in.
//
// The sequence matters and is the argument. The document refusal runs first, against a
// live share, which is the only way to show it costs nothing — the API refuses documents
// on a burn dossier before it checks the PIN. Then the open, which succeeds. Then the
// second open, which is refused because the first spent it.
func TestContractTheBurnDossier(t *testing.T) {
	data := contractFixture(t)
	token, pin := shareByKey(t, data, "burn")

	reader := contractClient(t, contractToken(t, data, sharesReader))
	before := auditKinds(t, reader, token)
	if countKind(before, "opened") != 0 {
		t.Fatalf("the burn share has already been opened, so this run needs a fresh "+
			"`cli:seed_contract`: %v", before)
	}

	// Done-when 4. --document sends no view, so this must not spend the one open.
	documents := openHarnessFor(t)
	documents.stdinString(pin + "\n")
	code := documents.run("open", token, "--document", "1",
		"--out", filepath.Join(t.TempDir(), "x.pdf"), "--pin-stdin", "--host", contractHost(t))

	if code != exitcode.NotFound {
		t.Fatalf("the document fetch exited %d, want %d:\n%s", code, exitcode.NotFound,
			documents.stderr.String())
	}
	if !strings.Contains(documents.stderr.String(), burnDocumentNote) {
		t.Errorf("§8.3's line is missing:\n%s", documents.stderr.String())
	}

	afterDocuments := auditKinds(t, reader, token)
	if countKind(afterDocuments, "doc-view") != countKind(before, "doc-view") {
		t.Error("a doc-view row was written for a refused document")
	}
	if countKind(afterDocuments, "opened") != 0 {
		t.Fatal("the refused document fetch opened the dossier; --document must send no view")
	}

	// Done-when 3, first half.
	first := openHarnessFor(t)
	first.stdinString(pin + "\n")
	if code := first.run("open", token, "--pin-stdin", "--host", contractHost(t)); code != exitcode.OK {
		t.Fatalf("the first open exited %d:\n%s", code, first.stderr.String())
	}

	// Second half: spent, and the server says so rather than the client guessing.
	second := openHarnessFor(t)
	second.stdinString(pin + "\n")
	code = second.run("open", token, "--pin-stdin", "--host", contractHost(t))

	if code != exitcode.ShareClosed {
		t.Fatalf("the second open exited %d, want %d:\n%s", code, exitcode.ShareClosed,
			second.stderr.String())
	}
	if second.stdout.Len() != 0 {
		t.Errorf("the second open printed dossier data:\n%s", second.stdout.String())
	}

	// A burn is not an ordinary revoke, and the trail has to be able to tell them apart —
	// gap 15's `revoked_reason` says the same thing on the share itself.
	final := auditKinds(t, reader, token)
	if countKind(final, "opened") != 1 {
		t.Errorf("the trail records %d opens of a burn share: %v", countKind(final, "opened"), final)
	}
}

// --- done-when 5: the download round-trips ------------------------------------------------

// TestContractADocumentDownloadsAndMatches needs the blob the fixture grew for M4. Before
// that the API answered 404 here, because it refuses a document with nothing attached.
func TestContractADocumentDownloadsAndMatches(t *testing.T) {
	data := contractFixture(t)
	token, pin := shareByKey(t, data, "live")

	client, err := api.New(contractHost(t), api.WithNoWait(true))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	opened, err := client.OpenDossier(context.Background(), token, pin, 30*time.Second)
	if err != nil {
		t.Fatalf("OpenDossier: %v", err)
	}
	if len(opened.Dossier.Documents) == 0 {
		t.Skip("the fixture's live share releases no documents")
	}
	document := opened.Dossier.Documents[0]

	out := filepath.Join(t.TempDir(), "downloaded.pdf")
	h := openHarnessFor(t)
	h.stdinString(pin + "\n")

	code := h.run("open", token, "--document", fmt.Sprint(document.ID),
		"--out", out, "--pin-stdin", "--host", contractHost(t))
	if code != exitcode.OK {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the download: %v", err)
	}
	if len(saved) == 0 {
		t.Fatal("the download was empty")
	}
	// The fixture plants a PDF header; anything else means the bytes took a wrong turn.
	if !strings.HasPrefix(string(saved), "%PDF") {
		t.Errorf("the downloaded bytes are not the fixture's document: %q", first64(saved))
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %04o, want 0600", mode)
	}
}

// The signed URL is the one request the CLI makes outside /api/v1, and it is short-lived
// on purpose. This pins the budget the API actually publishes rather than the five
// minutes the plan assumes.
func TestContractTheSignedURLIsShortLived(t *testing.T) {
	data := contractFixture(t)
	token, pin := shareByKey(t, data, "live")

	client, err := api.New(contractHost(t), api.WithNoWait(true))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	opened, err := client.OpenDossier(context.Background(), token, pin, 30*time.Second)
	if err != nil {
		t.Fatalf("OpenDossier: %v", err)
	}
	if len(opened.Dossier.Documents) == 0 {
		t.Skip("the fixture's live share releases no documents")
	}

	link, err := client.DocumentLink(context.Background(), token,
		opened.Dossier.Documents[0].ID, pin, 30*time.Second)
	if err != nil {
		t.Fatalf("DocumentLink: %v", err)
	}

	if link.ExpiresIn <= 0 || link.ExpiresIn > 900 {
		t.Errorf("expires_in = %d; the CLI tells the recipient this link lasts about five "+
			"minutes, and that sentence is now wrong", link.ExpiresIn)
	}
	if !strings.HasPrefix(link.URL, "http://") && !strings.HasPrefix(link.URL, "https://") {
		t.Errorf("url = %q, which is not fetchable", link.URL)
	}
}

// --- done-when 6: the closed states ------------------------------------------------------

func TestContractClosedDossiersRefuseWithTheirOwnReason(t *testing.T) {
	data := contractFixture(t)

	cases := []struct {
		key  string
		code string
	}{
		{"expired", "share_expired"},
		{"revoked", "share_revoked"},
	}

	for _, testCase := range cases {
		t.Run(testCase.key, func(t *testing.T) {
			token, pin := shareByKey(t, data, testCase.key)

			h := openHarnessFor(t)
			h.stdinString(pin + "\n")
			code := h.run("open", token, "--pin-stdin", "--host", contractHost(t))

			if code != exitcode.ShareClosed {
				t.Errorf("exit = %d, want %d:\n%s", code, exitcode.ShareClosed, h.stderr.String())
			}
			if h.stdout.Len() != 0 {
				t.Errorf("a closed dossier printed data:\n%s", h.stdout.String())
			}
		})
	}
}

// Gap 17, pinned so the day it is fixed this test says so rather than the copy silently
// changing under the CLI. The recipient boundary currently answers an unknown token with
// the holder API's not_found hint, which talks about a token's account to a caller that
// has neither.
func TestContractAnUnknownTokenStillCarriesTheHolderAPIsHint(t *testing.T) {
	h := openHarnessFor(t)
	h.stdinString("111111\n")

	code := h.run("open", "ZZZZZZZZZ", "--pin-stdin", "--host", contractHost(t))
	if code != exitcode.NotFound {
		t.Fatalf("exit = %d, want %d:\n%s", code, exitcode.NotFound, h.stderr.String())
	}

	if !strings.Contains(h.stderr.String(), "token's account") {
		t.Log("gap 17 appears to be closed: the recipient boundary no longer serves the " +
			"holder API's not_found hint. Update §12 and delete this test.")
	}
}

// --- helpers -------------------------------------------------------------------------

// auditKinds reads the holder's audit trail for a share, by token.
func auditKinds(t *testing.T, client *api.Client, shareToken string) []string {
	t.Helper()

	id := shareIDByToken(t, client, shareToken)
	detail, err := client.ShowShare(context.Background(), id, 30*time.Second)
	if err != nil {
		t.Fatalf("ShowShare: %v", err)
	}

	kinds := make([]string, 0, len(detail.AuditEvents))
	for _, event := range detail.AuditEvents {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func auditHasKind(t *testing.T, client *api.Client, shareToken, kind string) bool {
	t.Helper()
	return countKind(auditKinds(t, client, shareToken), kind) > 0
}

func countKind(kinds []string, kind string) int {
	n := 0
	for _, candidate := range kinds {
		if candidate == kind {
			n++
		}
	}
	return n
}

func shareIDByToken(t *testing.T, client *api.Client, shareToken string) int {
	t.Helper()

	after := 0
	for page := 0; page < 20; page++ {
		query := api.ShareQuery{Limit: 100}
		if after > 0 {
			query.After = &after
		}
		result, err := client.ListShares(context.Background(), query, 30*time.Second)
		if err != nil {
			t.Fatalf("ListShares: %v", err)
		}
		for _, share := range result.Shares {
			if strings.EqualFold(share.Token, shareToken) {
				return share.ID
			}
		}
		if result.NextAfter == nil {
			break
		}
		after = *result.NextAfter
	}
	t.Fatalf("no share with token %q in the holder's own list", shareToken)
	return 0
}

func first64(b []byte) string {
	if len(b) > 64 {
		return string(b[:64])
	}
	return string(b)
}
