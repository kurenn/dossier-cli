//go:build contract

// M3's half of the contract suite: the mint.
//
// Three things make these tests unlike the rest of the suite, and all three shape how
// they are written.
//
// They are the most expensive requests in the API. The mint budget is 10/min per token —
// the tightest in the namespace, because a mint reads every released field's value back
// out through the recipient endpoint. Two seeded tokens carry `shares:mint`, and the tests
// below are split across both to stay inside it. Nothing here retries.
//
// They are irreversible. A minted share cannot be unminted, only revoked, so every test
// mints what it needs and asserts on that rather than on the state of the vault — and no
// test here counts shares, for the reason recorded in docs/dev-loop-learnings.md.
//
// And they are the only requests that return a secret. The raw PIN comes back once, in the
// 201, and can never be recovered. That makes one assertion possible here that is
// available nowhere else: the PIN the CLI renders can be spent against the recipient
// endpoint, which proves the block did not mangle it. A PIN with a space in it — "480 217"
// — is exactly the kind of value a renderer can quietly eat.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

const (
	// minter is the token these tests spend first, and allMinter is the second — the mint
	// limiter is per token, so splitting across the two seeded tokens that hold
	// `shares:mint` doubles the budget from 10/min to 20.
	minter    = "shares_mint"
	allMinter = "all"
)

// contractMintKey generates a key the way the CLI does, per call, so no two runs of this
// suite collide on one.
func contractMintKey(t *testing.T) string {
	t.Helper()

	key := newIdempotencyKey()
	if !store.ValidKey(key) {
		t.Fatalf("the CLI generated %q, which its own ledger would refuse", key)
	}
	return key
}

// activeFieldID is a field from the seeded vault that can actually be released.
func activeFieldID(t *testing.T, data contractData) int {
	t.Helper()

	for _, field := range data.Fields {
		if field.Status == "active" {
			return field.ID
		}
	}
	t.Fatal("the fixture seeds no active field, so nothing can be minted")
	return 0
}

// contractMint sends one mint and returns the decoded 201.
func contractMint(t *testing.T, tokenHandle string, input api.MintInput, key string) *api.MintResult {
	t.Helper()

	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, tokenHandle))

	body, err := api.BuildMintBody(input)
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}

	result, err := client.Mint(context.Background(), key, body, contractTimeout)
	if err != nil {
		t.Fatalf("POST /api/v1/shares: %v", err)
	}
	return result
}

// TestContractMintResponseFieldsAreAllDecoded is the assertion that catches a key the CLI
// does not model.
//
// The schema advertises what this endpoint returns. If it names something `MintResult` or
// `MintedShare` has no field for, the CLI is silently unable to render a value the API is
// sending — which is precisely the class of bug this suite found in M1, where `api.Endpoint`
// read `response_keys` for a document that says `response_fields`.
func TestContractMintResponseFieldsAreAllDecoded(t *testing.T) {
	schema := liveSchema(t)

	var advertised []string
	for _, endpoint := range schema.Endpoints {
		if endpoint.Method == http.MethodPost && endpoint.Path == "/api/v1/shares" {
			advertised = endpoint.ResponseFields
		}
	}
	if len(advertised) == 0 {
		t.Fatal("the schema advertises no response_fields for POST /api/v1/shares")
	}

	// The struct tags, which are what actually decode the body.
	modelled := map[string]bool{"batch_id": true}
	for _, name := range []string{
		"id", "token", "url", "pin", "pin_shown_once",
		"recipient", "state", "expires_at", "field_count", "email_queued",
	} {
		modelled["shares[]."+name] = true
	}

	for _, field := range advertised {
		if !modelled[field] {
			t.Errorf("the API returns %q and the CLI models no field for it", field)
		}
	}
}

// TestContractTheMintedShapeIsWhatTheFakeClaims mints one real share and checks the
// decoded result against what mintFixture asserts in the unit tests.
//
// This is the arbitration: every other test in this package renders mintFixture and
// believes it. Here the same struct is filled by a live server.
func TestContractTheMintedShapeIsWhatTheFakeClaims(t *testing.T) {
	data := contractFixture(t)
	at := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)

	result := contractMint(t, minter, api.MintInput{
		Title:      "Contract mint " + fmt.Sprint(time.Now().UnixNano()),
		FieldIDs:   []int{activeFieldID(t, data)},
		Recipients: []api.Recipient{{Label: "Marisol Vega", Email: "marisol@example.com"}},
		ExpiresAt:  &at,
	}, contractMintKey(t))

	if result.BatchID == "" {
		t.Error("the 201 carries no batch_id")
	}
	if len(result.Shares) != 1 {
		t.Fatalf("%d shares for one recipient, want 1", len(result.Shares))
	}

	share := result.Shares[0]
	if share.ID == 0 || share.Token == "" || share.URL == "" {
		t.Errorf("identity keys are empty: %+v", share)
	}

	// The PIN's shape, which the block renders verbatim. Three digits, a space, three
	// digits — and the space is the part a renderer could lose.
	if !pinShaped(share.PIN) {
		t.Errorf("pin = %q, want the API's three-space-three shape", share.PIN)
	}
	if !share.PINShownOnce {
		t.Error("pin_shown_once is false; the block's `shown once` phrase would be a lie")
	}
	if share.State != "live" {
		t.Errorf("state = %q, want live for a freshly minted share with a future expiry", share.State)
	}
	if share.ExpiresAt == nil || !share.ExpiresAt.Equal(at) {
		t.Errorf("expires_at = %v, want the %s that was sent", share.ExpiresAt, at.Format(time.RFC3339))
	}
	if share.FieldCount != 1 {
		t.Errorf("field_count = %d, want 1", share.FieldCount)
	}
	if !share.EmailQueued {
		t.Error("email_queued is false for a recipient with an address on file")
	}
}

// TestContractTheRenderedPINOpensTheDossier is the M3 done-when's live half.
//
// The plan words it as "the PIN in the block matches the emailed one in the test mailer".
// Go cannot see Rails' mailer — it is in another process's memory — and
// `spec/services/shares/mint_spec.rb` already asserts that the emailed PIN is the minted
// one, where the mailer is actually visible. So this asserts something stronger and
// entirely within reach: the PIN as it appears in the handover block, parsed back out of
// the rendered bytes, opens the dossier.
//
// That closes the gap that matters. The API returning a good PIN is worth nothing if the
// renderer eats the space, pads it, or truncates it — and a holder who reads a mangled PIN
// out of this block has no way to get another, because `pin_digest` is bcrypt.
func TestContractTheRenderedPINOpensTheDossier(t *testing.T) {
	data := contractFixture(t)
	host := contractHost(t)

	h := newHarness(t)
	h.app.Sleep = func(time.Duration) {}
	h.seedProfile("contract", store.Profile{Host: host, Token: contractToken(t, data, minter)})
	h.seedDefaultProfile("contract")

	code := h.run("shares", "mint",
		"--field", fmt.Sprint(activeFieldID(t, data)),
		"--to", "Marisol Vega",
		"--title", "Contract handover "+fmt.Sprint(time.Now().UnixNano()),
		"--expires", "P7D",
		"--yes")
	if code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	// Parsed out of the block rather than taken from the API response, because the bytes
	// the holder reads are the thing under test.
	token, pin := parseHandover(t, h.stdout.String())

	// No recipient address was given, so the CLI's output is the only copy of the PIN —
	// and it has to say so, or the PIN gets forwarded down the same channel as the link.
	if !strings.Contains(h.stderr.String(), "only copy of the PIN") {
		t.Errorf("a share with no address did not say the CLI holds the only copy:\n%s", h.stderr.String())
	}

	opened := viewDossier(t, host, token, pin)
	if opened.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(opened.Body)
		t.Fatalf("the PIN from the block did not open the dossier: %d %s", opened.StatusCode, body)
	}
	opened.Body.Close()

	// And the negative, so the test above is not passing because the gate is open to
	// anything. A different PIN of the same shape must be refused.
	refused := viewDossier(t, host, token, rotatePIN(pin))
	if refused.StatusCode == http.StatusOK {
		t.Error("a wrong PIN opened the dossier, so the gate is not checking it")
	}
	refused.Body.Close()
}

// TestContractAReplayIsByteIdenticalIncludingThePIN is what makes --resume safe.
//
// The whole ledger exists on the strength of this one behaviour: a repeat of the same key
// with the same bytes returns the original response rather than minting again. If the
// replay differed in any byte — a new PIN, a new token, a second share — then resuming an
// unknown outcome would be releasing a second dossier, and the CLI's advice to resume
// would be the most dangerous sentence it prints.
func TestContractAReplayIsByteIdenticalIncludingThePIN(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, minter))

	at := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	body, err := api.BuildMintBody(api.MintInput{
		Title:      "Contract replay " + fmt.Sprint(time.Now().UnixNano()),
		FieldIDs:   []int{activeFieldID(t, data)},
		Recipients: []api.Recipient{{Label: "Marisol Vega", Email: "marisol@example.com"}},
		ExpiresAt:  &at,
	})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	key := contractMintKey(t)

	first, err := client.Mint(context.Background(), key, body, contractTimeout)
	if err != nil {
		t.Fatalf("the first mint: %v", err)
	}
	second, err := client.Mint(context.Background(), key, body, contractTimeout)
	if err != nil {
		t.Fatalf("the replay: %v", err)
	}

	if !bytes.Equal(first.Raw, second.Raw) {
		t.Fatalf("the replay is not byte-identical:\n%s\n%s", first.Raw, second.Raw)
	}
	// Stated separately, because "byte-identical" passing on two error bodies would be a
	// vacuous green.
	if len(second.Shares) != 1 || second.Shares[0].PIN == "" {
		t.Fatalf("the replay is not a 201 carrying a share: %s", second.Raw)
	}
	if first.Shares[0].ID != second.Shares[0].ID {
		t.Errorf("the replay minted a second share: %d then %d",
			first.Shares[0].ID, second.Shares[0].ID)
	}
}

// TestContractTheSameKeyWithDifferentBytesIsRefused is the other half of the identity
// rule, and the reason the ledger stores the bytes rather than the input that made them.
func TestContractTheSameKeyWithDifferentBytesIsRefused(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, allMinter))

	fieldID := activeFieldID(t, data)
	at := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	key := contractMintKey(t)

	first, _ := api.BuildMintBody(api.MintInput{
		Title:     "Contract identity " + fmt.Sprint(time.Now().UnixNano()),
		FieldIDs:  []int{fieldID},
		ExpiresAt: &at,
	})
	if _, err := client.Mint(context.Background(), key, first, contractTimeout); err != nil {
		t.Fatalf("the first mint: %v", err)
	}

	// One key changed, everything else the same.
	second, _ := api.BuildMintBody(api.MintInput{
		Title:     "Contract identity, altered",
		FieldIDs:  []int{fieldID},
		ExpiresAt: &at,
	})
	_, err := client.Mint(context.Background(), key, second, contractTimeout)
	if err == nil {
		t.Fatal("the same key with different bytes was accepted, which would make the ledger pointless")
	}

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an API envelope: %v", err)
	}
	if apiErr.Code != "idempotency_key_reused" {
		t.Errorf("code = %q, want idempotency_key_reused", apiErr.Code)
	}
	if apiErr.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", apiErr.Status)
	}
	// The exit code the CLI maps this to is 7, and it must not be 8 — the difference is
	// whether trying again could ever work.
	if got := apiErr.ExitCode(); got != exitcode.IdempotencyConflict {
		t.Errorf("exit code = %d, want %d", got, exitcode.IdempotencyConflict)
	}
}

// TestContractAMintWithNoKeyIsRefused proves the header is mandatory, and therefore that
// the CLI's unconditional generation of one is load-bearing rather than belt-and-braces.
//
// Sent with net/http directly, because api.Client always sets the header — which is the
// point, and also why this cannot be tested through it.
func TestContractAMintWithNoKeyIsRefused(t *testing.T) {
	data := contractFixture(t)

	request, err := http.NewRequest(http.MethodPost,
		contractHost(t)+"/api/v1/shares",
		strings.NewReader(`{"field_ids":[1],"expires_at":null}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+contractToken(t, data, allMinter))
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /api/v1/shares: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", response.StatusCode)
	}
	if code := envelopeCode(t, response.Body); code != "idempotency_key_required" {
		t.Errorf("code = %q, want idempotency_key_required", code)
	}
}

// TestContractExpiryRequiredIsPublishedWithItsRemedy checks the copy the CLI puts in front
// of a holder who forgot the deadline.
//
// §6.1 refuses that before sending anything, in the API's own words — so those words have
// to be readable without triggering the error. Both halves: the message states the rule
// and the hint says what to do about it.
func TestContractExpiryRequiredIsPublishedWithItsRemedy(t *testing.T) {
	schema := liveSchema(t)

	var found *api.ErrorCode
	for i, row := range schema.ErrorCodes {
		if row.Code == "expiry_required" {
			found = &schema.ErrorCodes[i]
		}
	}
	if found == nil {
		t.Fatal("the schema does not publish expiry_required, so the CLI's pre-send refusal has no copy to quote")
	}
	if found.Message == "" {
		t.Error("expiry_required carries no message")
	}
	if found.Hint == "" {
		t.Error("expiry_required carries no hint, so the refusal can state the problem but not the remedy")
	}

	// And the fake says the same thing, since that is what the unit tests assert against.
	for _, row := range fakeSchema(t).ErrorCodes {
		if row.Code != "expiry_required" {
			continue
		}
		if row.Message != found.Message {
			t.Errorf("schemaFixture's message has drifted:\n fake %q\n live %q", row.Message, found.Message)
		}
		if row.Hint != found.Hint {
			t.Errorf("schemaFixture's hint has drifted:\n fake %q\n live %q", row.Hint, found.Hint)
		}
	}
}

// TestContractTheExpiryVocabularyIsWhatThePickerOffers arbitrates the picker.
//
// Every literal in that picker comes from here — the four presets, the exception's label
// and its note — so a fixture that drifted would mean the CLI offers a deadline the vault
// does not, or describes the exception in words the product did not choose.
func TestContractTheExpiryVocabularyIsWhatThePickerOffers(t *testing.T) {
	live := liveSchema(t)
	fake := fakeSchema(t)

	if !slices.Equal(live.Expiry.Presets, fake.Expiry.Presets) {
		t.Errorf("presets have drifted:\n fake %v\n live %v", fake.Expiry.Presets, live.Expiry.Presets)
	}
	if live.Expiry.NoExpiry.Label != fake.Expiry.NoExpiry.Label {
		t.Errorf("the exception's label has drifted:\n fake %q\n live %q",
			fake.Expiry.NoExpiry.Label, live.Expiry.NoExpiry.Label)
	}
	if live.Expiry.NoExpiry.Note != fake.Expiry.NoExpiry.Note {
		t.Errorf("the exception's note has drifted:\n fake %q\n live %q",
			fake.Expiry.NoExpiry.Note, live.Expiry.NoExpiry.Note)
	}

	// Each preset must parse with the CLI's own parser, or the picker would offer a choice
	// it cannot act on.
	now := time.Now()
	for _, preset := range live.Expiry.Presets {
		expiry, err := ParseExpires(preset, now, live.Expiry.Presets)
		if err != nil {
			t.Errorf("the CLI cannot parse the published preset %q: %v", preset, err)
			continue
		}
		if expiry.At == nil || !expiry.At.After(now) {
			t.Errorf("preset %q resolved to %v, which is not in the future", preset, expiry.At)
		}
	}
}

// TestContractANullExpiryMints is the labelled exception, end to end.
//
// Worth a real request because `null` is the *only* spelling the API accepts, and the
// plausible client mistakes — "" and false — are both 422s. A CLI that sent one of those
// would refuse to mint a permanent share while appearing to offer the option.
func TestContractANullExpiryMints(t *testing.T) {
	data := contractFixture(t)

	result := contractMint(t, allMinter, api.MintInput{
		Title:    "Contract no-expiry " + fmt.Sprint(time.Now().UnixNano()),
		FieldIDs: []int{activeFieldID(t, data)},
	}, contractMintKey(t))

	if len(result.Shares) != 1 {
		t.Fatalf("%d shares, want 1", len(result.Shares))
	}
	if result.Shares[0].ExpiresAt != nil {
		t.Errorf("expires_at = %v, want null for the exception", result.Shares[0].ExpiresAt)
	}
	if result.Shares[0].State != "live" {
		t.Errorf("state = %q, want live", result.Shares[0].State)
	}
}

// TestContractSilentExclusionIsVisibleOnlyInFieldCount is the behaviour a CLI can most
// easily misreport.
//
// Ids that do not resolve to the caller's own active fields are dropped without an error
// and without being named. So the only honest answer to "what actually shipped" is
// `field_count` on the response — never the number of --field flags the holder passed. A
// client that echoed its own count would tell a holder they released two fields when they
// released one.
func TestContractSilentExclusionIsVisibleOnlyInFieldCount(t *testing.T) {
	data := contractFixture(t)

	// One real id and one that cannot belong to this vault.
	result := contractMint(t, minter, api.MintInput{
		Title:    "Contract exclusion " + fmt.Sprint(time.Now().UnixNano()),
		FieldIDs: []int{activeFieldID(t, data), 999_999_999},
	}, contractMintKey(t))

	if len(result.Shares) != 1 {
		t.Fatalf("%d shares, want 1", len(result.Shares))
	}
	if got := result.Shares[0].FieldCount; got != 1 {
		t.Errorf("field_count = %d, want 1 — two ids were sent and one was releasable", got)
	}
}

// TestContractNoReleasableFieldsIsNoFields is the one failure Shares::Mint returns as a
// normal result rather than raising, and the CLI deletes its ledger entry on it: nothing
// was minted, and a corrected body needs a new key.
func TestContractNoReleasableFieldsIsNoFields(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, allMinter))

	body, _ := api.BuildMintBody(api.MintInput{FieldIDs: []int{999_999_999}})
	_, err := client.Mint(context.Background(), contractMintKey(t), body, contractTimeout)
	if err == nil {
		t.Fatal("a mint naming no releasable field succeeded")
	}

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an API envelope: %v", err)
	}
	if apiErr.Code != "no_fields" {
		t.Errorf("code = %q, want no_fields", apiErr.Code)
	}
	if apiErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", apiErr.Status)
	}
	if apiErr.Hint == "" {
		t.Error("no_fields carries no hint")
	}
}

// TestContractABatchIsOneSharePerRecipient checks the shape the handover block renders
// once per entry: one batch id, and a token and PIN of its own for each recipient.
//
// `batch_id` is a screen-only grouping key, not a shared secret — each sibling stands
// alone, and a CLI that implied otherwise would be describing a different product.
func TestContractABatchIsOneSharePerRecipient(t *testing.T) {
	data := contractFixture(t)

	result := contractMint(t, allMinter, api.MintInput{
		Title:    "Contract batch " + fmt.Sprint(time.Now().UnixNano()),
		FieldIDs: []int{activeFieldID(t, data)},
		Recipients: []api.Recipient{
			{Label: "Marisol Vega", Email: "marisol@example.com"},
			{Label: "Tomas Ruiz"},
		},
	}, contractMintKey(t))

	if len(result.Shares) != 2 {
		t.Fatalf("%d shares for two recipients, want 2", len(result.Shares))
	}
	first, second := result.Shares[0], result.Shares[1]

	if first.Token == second.Token {
		t.Error("the two shares in a batch share a token")
	}
	if first.PIN == second.PIN {
		t.Error("the two shares in a batch share a PIN, which would make one PIN open both")
	}
	if first.ID == second.ID {
		t.Error("the two shares in a batch share an id")
	}

	// The recipients come back in the order they were sent, which is what lets the CLI
	// render one block per --to in the order the holder typed them.
	if first.Recipient.Email != "marisol@example.com" {
		t.Errorf("shares[0].recipient = %+v, want the primary", first.Recipient)
	}
	if second.Recipient.Label != "Tomas Ruiz" || second.Recipient.Email != "" {
		t.Errorf("shares[1].recipient = %+v, want the label-only additional", second.Recipient)
	}

	// email_queued is exactly whether an address was on file, which is what the EMAILED
	// row reports and the closing sentence branches on.
	if !first.EmailQueued {
		t.Error("email_queued is false for the recipient with an address")
	}
	if second.EmailQueued {
		t.Error("email_queued is true for a recipient with no address, so the block would " +
			"tell a holder a mail went somewhere there is no address for")
	}
}

// TestContractMintRefusesWithoutTheScope is the 403 path, and the part that matters for
// the ledger: a refused scope claims no key, so the CLI deletes its entry.
func TestContractMintRefusesWithoutTheScope(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, "shares_read"))

	body, _ := api.BuildMintBody(api.MintInput{FieldIDs: []int{activeFieldID(t, data)}})
	key := contractMintKey(t)

	_, err := client.Mint(context.Background(), key, body, contractTimeout)
	if err == nil {
		t.Fatal("a token without shares:mint minted a share")
	}

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an API envelope: %v", err)
	}
	if apiErr.Code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", apiErr.Code)
	}

	// The key was never claimed, so it is still usable — which is why the CLI's ledger
	// entry for a 403 is deleted rather than kept. Proved by spending it on a real mint
	// with a token that does have the scope.
	minted := contractMint(t, minter, api.MintInput{
		Title:    "Contract unclaimed key " + fmt.Sprint(time.Now().UnixNano()),
		FieldIDs: []int{activeFieldID(t, data)},
	}, key)
	if len(minted.Shares) != 1 {
		t.Errorf("the key refused for scope could not then be used: %s", minted.Raw)
	}
}

// TestContractTheMintBudgetIsPublished keeps the plan's claim about this endpoint honest.
//
// The CLI's patience on a 429 — three waits, a minute each — is sized for this limit, and
// the comment in mint.go that explains why cites it. If the server's budget changed, that
// reasoning would be stale.
func TestContractTheMintBudgetIsPublished(t *testing.T) {
	limits := liveSchema(t).Limits

	rates, ok := limits["rate_limits"].(map[string]any)
	if !ok {
		t.Fatal("the schema publishes no rate_limits")
	}
	budget, ok := rates["share mint, per token"]
	if !ok {
		t.Fatalf("no published mint budget; rate_limits has %v", keysOf(rates))
	}
	if budget != "10/min" {
		t.Errorf("the mint budget is now %v, not the 10/min the CLI's retry bounds and this "+
			"suite's token splitting are sized for", budget)
	}

	if got := limits["idempotency_key_max_bytes"]; got != float64(api.IdempotencyKeyMaxBytes) {
		t.Errorf("published key ceiling = %v, CLI assumes %d", got, api.IdempotencyKeyMaxBytes)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// pinShaped reports whether a PIN is the API's three-space-three.
func pinShaped(pin string) bool {
	if len(pin) != 7 || pin[3] != ' ' {
		return false
	}
	for i, r := range pin {
		if i == 3 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseHandover pulls the token and PIN back out of the rendered block.
//
// Reading the bytes rather than the response is the whole point of the test that uses it:
// what the holder can act on is what is on the screen.
func parseHandover(t *testing.T, block string) (token, pin string) {
	t.Helper()

	for _, line := range strings.Split(block, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "TOKEN":
			token = fields[1]
		case "PIN":
			// Rejoined from the parts, because the PIN contains a space and `shown once`
			// follows it.
			if len(fields) >= 3 {
				pin = fields[1] + " " + fields[2]
			}
		}
	}

	if token == "" || pin == "" {
		t.Fatalf("could not read a token and a PIN out of the block:\n%s", block)
	}
	if !pinShaped(pin) {
		t.Fatalf("the PIN parsed out of the block is %q, which is not the API's shape — "+
			"the renderer has altered it", pin)
	}
	return token, pin
}

// viewDossier spends a PIN against the recipient endpoint.
//
// Sent with net/http rather than through api.Client because this is the un-authenticated
// surface — no bearer token — and because the CLI has no command for it until M4.
func viewDossier(t *testing.T, host, token, pin string) *http.Response {
	t.Helper()

	payload, err := json.Marshal(map[string]string{"pin": pin})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	request, err := http.NewRequest(http.MethodPost,
		host+"/api/v1/dossiers/"+token+"/view", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /api/v1/dossiers/%s/view: %v", token, err)
	}
	return response
}

// rotatePIN returns a different PIN of the same shape, for the negative half of the gate
// assertion.
func rotatePIN(pin string) string {
	rotated := []byte(pin)
	for i, b := range rotated {
		if b >= '0' && b <= '9' {
			rotated[i] = '0' + (b-'0'+1)%10
		}
	}
	return string(rotated)
}

func envelopeCode(t *testing.T, body io.Reader) string {
	t.Helper()

	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(body).Decode(&decoded); err != nil {
		t.Fatalf("decoding the error envelope: %v", err)
	}
	return decoded.Error.Code
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
