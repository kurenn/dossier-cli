package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// expires_at is the one key that must always be on the wire. The API answers
// `422 expiry_required` when it is absent and accepts exactly two shapes — a future
// timestamp, or an explicit null — so a body that omitted it, or that spelled the
// exception as "" or false, would be rejected.
func TestMintBodyAlwaysCarriesExpiresAt(t *testing.T) {
	at := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		input MintInput
		want  string
	}{
		{
			name:  "a timestamp",
			input: MintInput{FieldIDs: []int{1}, ExpiresAt: &at},
			want:  `"expires_at":"2026-12-01T09:00:00Z"`,
		},
		{
			name:  "the labelled exception",
			input: MintInput{FieldIDs: []int{1}},
			want:  `"expires_at":null`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body, err := BuildMintBody(testCase.input)
			if err != nil {
				t.Fatalf("BuildMintBody: %v", err)
			}
			if !strings.Contains(string(body), testCase.want) {
				t.Errorf("body = %s, want it to contain %s", body, testCase.want)
			}
			for _, forbidden := range []string{`"expires_at":""`, `"expires_at":false`} {
				if strings.Contains(string(body), forbidden) {
					t.Errorf("body spells the exception as %s, which the API rejects", forbidden)
				}
			}
		})
	}
}

// A timestamp is normalised to UTC before it is sent, so the confirmation the holder reads
// and the value on the wire are the same instant written the same way.
func TestMintBodyNormalisesTheZone(t *testing.T) {
	zone := time.FixedZone("UTC-6", -6*60*60)
	at := time.Date(2026, 12, 1, 3, 0, 0, 0, zone)

	body, err := BuildMintBody(MintInput{FieldIDs: []int{1}, ExpiresAt: &at})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	if !strings.Contains(string(body), `"expires_at":"2026-12-01T09:00:00Z"`) {
		t.Errorf("body = %s", body)
	}
}

// document_ids has three states the API treats differently, and a plain slice can only
// express two of them.
func TestMintBodyDistinguishesOmittedFromEmptyDocumentIDs(t *testing.T) {
	empty := []int{}
	named := []int{7, 9}

	cases := []struct {
		name string
		ids  *[]int
		want string
		gone bool
	}{
		// Omitted: every document on every released field rides along.
		{name: "omitted", ids: nil, gone: true},
		// Present but empty: strips every document from the share.
		{name: "an explicit empty list", ids: &empty, want: `"document_ids":[]`},
		{name: "named ids", ids: &named, want: `"document_ids":[7,9]`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body, err := BuildMintBody(MintInput{FieldIDs: []int{1}, DocumentIDs: testCase.ids})
			if err != nil {
				t.Fatalf("BuildMintBody: %v", err)
			}
			if testCase.gone {
				if strings.Contains(string(body), "document_ids") {
					t.Errorf("body names document_ids when it should be absent: %s", body)
				}
				return
			}
			if !strings.Contains(string(body), testCase.want) {
				t.Errorf("body = %s, want %s", body, testCase.want)
			}
		})
	}
}

// An unset boolean is omitted, so the API leaves the column at its own default. Sending
// false because a flag defaulted to false would be the client overriding a server default
// it was never asked to touch — and for notify_on_open, whose default is true, it would
// silently turn the notification off.
func TestMintBodyOmitsUnsetBooleans(t *testing.T) {
	body, err := BuildMintBody(MintInput{FieldIDs: []int{1}})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	for _, key := range []string{"burn_after_read", "allow_document_download", "notify_on_open"} {
		if strings.Contains(string(body), key) {
			t.Errorf("body mentions %s without being asked to: %s", key, body)
		}
	}

	no := false
	body, err = BuildMintBody(MintInput{FieldIDs: []int{1}, NotifyOnOpen: &no})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	if !strings.Contains(string(body), `"notify_on_open":false`) {
		t.Errorf("a deliberate false was not sent: %s", body)
	}
}

// field_ids go in the order given, unsorted and undeduplicated. Either rewriting would be
// a different body — and the API's idempotency identity is the key plus the raw body, so a
// client that quietly tidied the request would break the one property the ledger exists to
// provide.
func TestMintBodyDoesNotTidyTheFieldIDs(t *testing.T) {
	body, err := BuildMintBody(MintInput{FieldIDs: []int{9, 1, 9, 4}})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	if !strings.Contains(string(body), `"field_ids":[9,1,9,4]`) {
		t.Errorf("body = %s, want the ids exactly as given", body)
	}
}

// An empty list rather than null: the API reads `[]` as "no ids given" and answers
// no_fields, which is the honest outcome. `null` is a shape its parameter handling does
// not document.
func TestMintBodySendsAnEmptyArrayNotNull(t *testing.T) {
	body, err := BuildMintBody(MintInput{})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	if !strings.Contains(string(body), `"field_ids":[]`) {
		t.Errorf("body = %s", body)
	}
}

// An absent title is omitted rather than sent as "". The API has no presence validation on
// it, so both mint — but they are different bodies under the same key, and only one of
// them is what the holder asked for.
func TestMintBodyOmitsAnAbsentTitle(t *testing.T) {
	body, err := BuildMintBody(MintInput{FieldIDs: []int{1}})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	if strings.Contains(string(body), "title") {
		t.Errorf("body = %s, want no title key", body)
	}

	body, err = BuildMintBody(MintInput{FieldIDs: []int{1}, Title: "Lease application"})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	if !strings.Contains(string(body), `"title":"Lease application"`) {
		t.Errorf("body = %s", body)
	}
}

// The same input produces the same bytes. The ledger stores the slice rather than the
// input precisely so this does not have to be relied on — but a build that was unstable
// would mean a resume could send different bytes than the digest it recorded.
func TestMintBodyIsStable(t *testing.T) {
	input := MintInput{
		Title:      "Lease application",
		FieldIDs:   []int{3, 1, 2},
		Recipients: []Recipient{{Label: "Marisol Vega", Email: "marisol@example.com"}},
	}

	first, err := BuildMintBody(input)
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := BuildMintBody(input)
		if err != nil {
			t.Fatalf("BuildMintBody: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("run %d produced different bytes:\n%s\n%s", i, first, again)
		}
		if BodyDigest(again) != BodyDigest(first) {
			t.Fatalf("run %d produced a different digest", i)
		}
	}
}

func TestBodyDigestChangesWithTheBody(t *testing.T) {
	one := BodyDigest([]byte(`{"field_ids":[1]}`))
	two := BodyDigest([]byte(`{"field_ids":[2]}`))

	if one == two {
		t.Error("two different bodies share a digest")
	}
	if len(one) != 64 {
		t.Errorf("digest is %d characters, want 64 for hex-encoded SHA-256", len(one))
	}
}

// The 201 is the only response in the API that carries a raw PIN, and every key of it has
// to be modelled — a dropped one would be a value the CLI silently could not render.
func TestMintResultDecodesEveryKeyOfThe201(t *testing.T) {
	const body = `{"batch_id":"b6f2b3f0","shares":[{"id":42,"token":"K7M2P9QRX","url":"https://d/x","pin":"480 217","pin_shown_once":true,"recipient":{"label":"Marisol Vega","email":"marisol@example.com"},"state":"live","expires_at":"2026-09-21T09:00:00Z","field_count":1,"email_queued":true}]}`

	var result MintResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if result.BatchID != "b6f2b3f0" || len(result.Shares) != 1 {
		t.Fatalf("result = %+v", result)
	}
	share := result.Shares[0]
	if share.ID != 42 || share.Token != "K7M2P9QRX" || share.URL != "https://d/x" {
		t.Errorf("identity keys: %+v", share)
	}
	if share.PIN != "480 217" || !share.PINShownOnce {
		t.Errorf("pin keys: %+v", share)
	}
	if share.State != "live" || share.FieldCount != 1 || !share.EmailQueued {
		t.Errorf("state keys: %+v", share)
	}
	if share.ExpiresAt == nil || !share.ExpiresAt.Equal(time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("expires_at = %v", share.ExpiresAt)
	}
	if share.Recipient.Label != "Marisol Vega" || share.Recipient.Email != "marisol@example.com" {
		t.Errorf("recipient = %+v", share.Recipient)
	}
}

// A no-expiry share's expires_at is null, which must stay distinguishable from a zero
// time — otherwise the block would render a deadline of year one for a share that has
// none.
func TestMintResultKeepsANullExpiryNull(t *testing.T) {
	const body = `{"batch_id":"b","shares":[{"id":1,"expires_at":null,"email_queued":false,"recipient":{"label":"Tomas Ruiz","email":null}}]}`

	var result MintResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if result.Shares[0].ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", result.Shares[0].ExpiresAt)
	}
	if result.Shares[0].Recipient.Email != "" {
		t.Errorf("a null email decoded to %q", result.Shares[0].Recipient.Email)
	}
}

func TestIdempotencyKeyMaxBytesMatchesTheAPI(t *testing.T) {
	if IdempotencyKeyMaxBytes != 255 {
		t.Errorf("IdempotencyKeyMaxBytes = %d, want 255", IdempotencyKeyMaxBytes)
	}
	if IdempotencyHeader != "Idempotency-Key" {
		t.Errorf("IdempotencyHeader = %q", IdempotencyHeader)
	}
}
