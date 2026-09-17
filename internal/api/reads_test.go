package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFieldQueryOmitsUnsetParameters(t *testing.T) {
	after := 42

	cases := []struct {
		name  string
		query FieldQuery
		want  string
	}{
		{"empty sends nothing", FieldQuery{}, ""},
		{"a status", FieldQuery{Status: "withdrawn"}, "status=withdrawn"},
		{"a limit", FieldQuery{Limit: 7}, "limit=7"},
		{"a cursor", FieldQuery{After: &after}, "after=42"},
		{"all three", FieldQuery{Status: "active", Limit: 100, After: &after}, "after=42&limit=100&status=active"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.query.values().Encode(); got != tc.want {
				t.Errorf("values() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A limit of zero is "unset", not "zero rows": the API's own default applies. Restating 25
// here would be a default in two places, which is a default that will eventually disagree
// with itself.
func TestShareQueryTreatsZeroLimitAsUnset(t *testing.T) {
	if got := (ShareQuery{Limit: 0}).values().Encode(); got != "" {
		t.Errorf("values() = %q, want empty", got)
	}
	if got := (ShareQuery{State: "live", Limit: 25}).values().Encode(); got != "limit=25&state=live" {
		t.Errorf("values() = %q", got)
	}
}

func TestListFieldsDecodesThePageAndKeepsTheRawBody(t *testing.T) {
	body := `{"fields":[{"id":12,"label":"U.S. Passport","category":"travel","country":"USA","sensitivity":3,"status":"active","has_document":true,"documents":[{"id":3,"filename":"p.pdf","content_type":"application/pdf","byte_size":248131}]}],"next_after":12}`

	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/fields" {
			t.Errorf("path %q", got)
		}
		if got := r.URL.Query().Get("status"); got != "active" {
			t.Errorf("status %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})

	page, err := client.ListFields(context.Background(), FieldQuery{Status: "active"}, time.Second)
	if err != nil {
		t.Fatalf("ListFields: %v", err)
	}

	if len(page.Fields) != 1 {
		t.Fatalf("got %d fields", len(page.Fields))
	}
	field := page.Fields[0]
	if field.ID != 12 || field.Label != "U.S. Passport" || field.Sensitivity != 3 {
		t.Errorf("decoded %+v", field)
	}
	if field.Country == nil || *field.Country != "USA" {
		t.Errorf("country = %v", field.Country)
	}
	if len(field.Documents) != 1 || field.Documents[0].ByteSize != 248131 {
		t.Errorf("documents = %+v", field.Documents)
	}
	if page.NextAfter == nil || *page.NextAfter != 12 {
		t.Errorf("next_after = %v", page.NextAfter)
	}
	// Kept verbatim for --json: re-marshalling the struct would publish a second shape.
	if string(page.Raw) != body {
		t.Errorf("Raw is not the body it was sent:\n%s", page.Raw)
	}
}

// A null country is a field with no country, not a field whose country is the empty
// string. The pointer is what keeps those distinguishable, so it is asserted.
func TestListFieldsKeepsNullDistinctFromEmpty(t *testing.T) {
	client := testClient(t, jsonHandler(`{"fields":[{"id":9,"country":null},{"id":10,"country":""}],"next_after":null}`))

	page, err := client.ListFields(context.Background(), FieldQuery{}, time.Second)
	if err != nil {
		t.Fatalf("ListFields: %v", err)
	}

	if page.Fields[0].Country != nil {
		t.Errorf("a null country should decode to nil, got %q", *page.Fields[0].Country)
	}
	if page.Fields[1].Country == nil || *page.Fields[1].Country != "" {
		t.Errorf("an empty country should decode to a pointer to \"\", got %v", page.Fields[1].Country)
	}
	if page.NextAfter != nil {
		t.Errorf("a null cursor should decode to nil, got %d", *page.NextAfter)
	}
}

func TestListSharesDecodesBothTimestamps(t *testing.T) {
	client := testClient(t, jsonHandler(`{"shares":[{"id":83,"token":"W9KMR2PQX","title":"Withdrawn","recipient":{"label":"Priya","email":"p@example.com"},"state":"revoked","expires_at":"2026-10-16T09:00:00Z","revoked_at":"2026-09-14T17:42:00Z","opens_count":1,"field_count":4,"batch_id":"b-1"}],"next_after":null}`))

	page, err := client.ListShares(context.Background(), ShareQuery{}, time.Second)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}

	share := page.Shares[0]
	if share.ExpiresAt == nil || share.ExpiresAt.Year() != 2026 || share.ExpiresAt.Month() != time.October {
		t.Errorf("expires_at = %v", share.ExpiresAt)
	}
	// The one that matters: a revoked share keeps a future expiry, so both have to arrive
	// for the renderer to show the closing fact rather than the countdown.
	if share.RevokedAt == nil || share.RevokedAt.Day() != 14 {
		t.Errorf("revoked_at = %v", share.RevokedAt)
	}
	if share.BatchID == nil || *share.BatchID != "b-1" {
		t.Errorf("batch_id = %v", share.BatchID)
	}
}

// A server that has not grown revoked_at yet leaves it nil rather than failing to decode.
func TestListSharesToleratesAMissingRevokedAt(t *testing.T) {
	client := testClient(t, jsonHandler(`{"shares":[{"id":83,"state":"revoked","expires_at":"2026-10-16T09:00:00Z"}],"next_after":null}`))

	page, err := client.ListShares(context.Background(), ShareQuery{}, time.Second)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if page.Shares[0].RevokedAt != nil {
		t.Errorf("revoked_at should be nil when absent, got %v", page.Shares[0].RevokedAt)
	}
}

func TestShowShareDecodesTheAuditTrail(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/shares/83" {
			t.Errorf("path %q, want the id in it", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"share":{"id":83,"state":"revoked"},"audit_events":[{"kind":"revoked","label":"Revoked","meaning":"The holder killed it early. It stopped working instantly.","occurred_at":"2026-09-14T17:42:00Z","detail":"Killed from the shares list"},{"kind":"opened","label":"Opened","meaning":"The dossier was rendered for the recipient.","occurred_at":"2026-09-13T08:15:00Z","detail":null}]}`))
	})

	detail, err := client.ShowShare(context.Background(), 83, time.Second)
	if err != nil {
		t.Fatalf("ShowShare: %v", err)
	}

	if detail.Share.ID != 83 {
		t.Errorf("share = %+v", detail.Share)
	}
	if len(detail.AuditEvents) != 2 {
		t.Fatalf("got %d events", len(detail.AuditEvents))
	}
	// Rule 9's pair: a name and a sentence, both the server's own words.
	first := detail.AuditEvents[0]
	if first.Label != "Revoked" || first.Meaning != "The holder killed it early. It stopped working instantly." {
		t.Errorf("event = %+v", first)
	}
	if first.Detail == nil || *first.Detail != "Killed from the shares list" {
		t.Errorf("detail = %v", first.Detail)
	}
	// A null detail stays null rather than becoming "", so a renderer can omit the line.
	if detail.AuditEvents[1].Detail != nil {
		t.Errorf("a null detail should be nil, got %q", *detail.AuditEvents[1].Detail)
	}
}

func TestReadsReturnTheErrorEnvelope(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"insufficient_scope","message":"This token cannot read shares.","hint":"Mint one with shares:read."}}`))
	})

	_, err := client.ListShares(context.Background(), ShareQuery{}, time.Second)

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *Error, got %T: %v", err, err)
	}
	if apiErr.Code != "insufficient_scope" || apiErr.Status != http.StatusForbidden {
		t.Errorf("envelope = %+v", apiErr)
	}
}

func TestReadsReportAnUnparseableBody(t *testing.T) {
	client := testClient(t, jsonHandler(`{"shares": [`))

	if _, err := client.ListShares(context.Background(), ShareQuery{}, time.Second); err == nil {
		t.Fatal("expected an error for a truncated body")
	}
}

func TestRecipientDisplayPrefersTheLabel(t *testing.T) {
	cases := []struct {
		name      string
		recipient Recipient
		want      string
	}{
		{"a label", Recipient{Label: "Marisol Vega", Email: "m@example.com"}, "Marisol Vega"},
		{"no label falls back to the email", Recipient{Email: "m@example.com"}, "m@example.com"},
		{"neither is empty, for the caller to render as absent", Recipient{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.recipient.Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := New(server.URL, WithToken("dsk_test"), WithNoWait(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}
