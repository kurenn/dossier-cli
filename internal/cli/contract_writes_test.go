//go:build contract

// M2's half of the contract suite: the two write endpoints.
//
// These differ from every other test here in one way that governs how they are written —
// they leave things behind. A read can be repeated forever against the seeded vault; a
// create adds a field to it, and an attach adds a document. So every label below carries
// a per-run suffix, no test asserts a total count of anything, and the assertions are
// about the shape and the rules rather than about the state of the vault.
//
// The write budget is 30/min per token and these tests are the only things spending it,
// which is why they read through `fields_write` and `documents_write` rather than `all`.
package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
)

const (
	fieldsWriter    = "fields_write"
	documentsWriter = "documents_write"
)

// uniqueLabel keeps each run's creates from colliding with the last run's, which on this
// endpoint is not a flake but a duplicate-label 422 — a real answer to a question the
// test was not asking.
func uniqueLabel(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s %d", prefix, time.Now().UnixNano())
}

func contractToken(t *testing.T, data contractData, handle string) string {
	t.Helper()

	token, ok := data.Tokens[handle]
	if !ok {
		t.Fatalf("the fixture has no %q token; the seed task and this suite disagree", handle)
	}
	return token.Raw
}

// TestContractCreatedFieldIsAFieldRow asserts the thing the CLI assumes everywhere: that
// POST /fields answers with the same per-field shape GET /fields returns, so one struct
// and one renderer serve both.
func TestContractCreatedFieldIsAFieldRow(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, fieldsWriter))

	label := uniqueLabel(t, "Contract create")
	country := "MEX"
	sensitivity := 2

	field, raw, err := client.CreateField(context.Background(), api.CreateFieldInput{
		Label:       label,
		Value:       "ROGV880801MDFSZL09",
		Category:    "identity",
		Country:     country,
		Sensitivity: &sensitivity,
	}, contractTimeout)
	if err != nil {
		t.Fatalf("POST /api/v1/fields: %v", err)
	}

	if field.ID == 0 {
		t.Error("the created field has no id")
	}
	if field.Label != label {
		t.Errorf("label = %q, want %q", field.Label, label)
	}
	if field.Status != "active" {
		t.Errorf("status = %q, want active — a freshly created field is active", field.Status)
	}
	if field.HasDocument {
		t.Error("has_document is true on a field created without one")
	}
	if len(field.Documents) != 0 {
		t.Errorf("documents = %v, want empty", field.Documents)
	}
	if field.Country == nil || *field.Country != country {
		t.Errorf("country = %v, want %q", field.Country, country)
	}
	if field.Sensitivity != sensitivity {
		t.Errorf("sensitivity = %d, want %d", field.Sensitivity, sensitivity)
	}

	// The create response is the one place the value could plausibly come back, since the
	// caller just sent it. Product rule 5 says it must not, and this is the byte-level
	// check that it does not.
	if strings.Contains(string(raw), "ROGV880801MDFSZL09") {
		t.Errorf("the value came back in the create response:\n%s", raw)
	}
}

// TestContractCreateFieldResponseFieldsAreAllDecoded is the M1 struct-conformity check
// applied to the write: every key the schema says this endpoint returns must have
// somewhere in api.Field to land.
func TestContractCreateFieldResponseFieldsAreAllDecoded(t *testing.T) {
	schema := liveSchema(t)
	endpoint := findEndpoint(t, schema, "POST", "/api/v1/fields")

	if len(endpoint.ResponseFields) == 0 {
		t.Skip("the schema publishes no response_fields for POST /api/v1/fields")
	}

	decoded := jsonTags(api.Field{})
	for _, key := range endpoint.ResponseFields {
		name := strings.TrimPrefix(key, "documents[].")
		if name != key {
			if !slices.Contains(jsonTags(api.Document{}), name) {
				t.Errorf("the API returns documents[].%s and api.Document has no field for it", name)
			}
			continue
		}
		if !slices.Contains(decoded, key) {
			t.Errorf("the API returns %q from POST /fields and api.Field has no field for it", key)
		}
	}
}

// TestContractTheDocumentsCategoryIsRefusedByBoth is the one that keeps the CLI's local
// refusal honest.
//
// `documents` is a real member of FieldDefinition::CATEGORIES and a virtual filter in the
// vault UI, but it is not a category a field can be created under. The CLI refuses it
// before sending, on the strength of the schema's field_categories omitting it. That is
// only correct while the server agrees, so both halves are asserted here: the schema does
// not publish it, and the server rejects it if sent anyway.
func TestContractTheDocumentsCategoryIsRefusedByBoth(t *testing.T) {
	schema := liveSchema(t)
	if slices.Contains(schema.FieldCategories, "documents") {
		t.Errorf("the schema publishes `documents` as a field category, so the CLI would send it and the server would refuse: %v", schema.FieldCategories)
	}

	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, fieldsWriter))

	_, _, err := client.CreateField(context.Background(), api.CreateFieldInput{
		Label:    uniqueLabel(t, "Contract category"),
		Value:    "x",
		Category: "documents",
	}, contractTimeout)

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("sending category=documents did not fail with an API error: %v", err)
	}
	if apiErr.Code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", apiErr.Code)
	}
}

// TestContractADuplicateLabelSaysWhatWasDuplicated pins the message a retrying agent is
// most likely to meet.
//
// It is the closest thing this endpoint has to a confirmation — no Idempotency-Key exists
// here, so a resend after an ambiguous failure gets this instead of the original 201 —
// and it is only useful if it names the label. An attribute-prefixed or `key`-flavoured
// message would be a different answer to the same question.
func TestContractADuplicateLabelSaysWhatWasDuplicated(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, fieldsWriter))

	label := uniqueLabel(t, "Contract duplicate")
	input := api.CreateFieldInput{Label: label, Value: "first"}

	if _, _, err := client.CreateField(context.Background(), input, contractTimeout); err != nil {
		t.Fatalf("the first create failed: %v", err)
	}

	_, _, err := client.CreateField(context.Background(), input, contractTimeout)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("the second create with the same label succeeded or failed oddly: %v", err)
	}
	if apiErr.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", apiErr.Code)
	}

	want := fmt.Sprintf("You already have a field named “%s”.", label)
	if apiErr.Message != want {
		t.Errorf("message = %q,\n    want %q", apiErr.Message, want)
	}
}

// TestContractAttachedDocumentIsItsOwnShape checks the assumption behind having two
// structs: the attach response and a field's nested documents are not the same object.
func TestContractAttachedDocumentIsItsOwnShape(t *testing.T) {
	data := contractFixture(t)
	fieldID := contractWritableField(t, data)
	client := contractClient(t, contractToken(t, data, documentsWriter))

	document, raw, err := client.AttachDocument(context.Background(), fieldID, api.Upload{
		Filename:    "contract.pdf",
		ContentType: "application/pdf",
		Content:     []byte("%PDF-1.4 contract suite"),
	}, contractTimeout)
	if err != nil {
		t.Fatalf("POST /api/v1/fields/%d/documents: %v", fieldID, err)
	}

	if document.ID == 0 {
		t.Error("the attached document has no id")
	}
	if document.FieldID != fieldID {
		t.Errorf("field_id = %d, want %d", document.FieldID, fieldID)
	}
	if document.ByteSize != int64(len("%PDF-1.4 contract suite")) {
		t.Errorf("byte_size = %d, want %d — the server measures the upload, not a declared size",
			document.ByteSize, len("%PDF-1.4 contract suite"))
	}
	if document.ContentType != "application/pdf" {
		t.Errorf("content_type = %q", document.ContentType)
	}

	// The envelope is the shape the client unwraps. If the server ever flattened it, the
	// decode would silently produce a zero-valued document rather than an error.
	if !strings.Contains(string(raw), `"document"`) {
		t.Errorf("the response is not wrapped in a \"document\" envelope:\n%s", raw)
	}
}

// TestContractAnAttachedDocumentAppearsOnTheField closes the loop between the two
// endpoints, and is what validates api.Document — the nested row — against a document
// this suite actually created rather than one the seed planted.
func TestContractAnAttachedDocumentAppearsOnTheField(t *testing.T) {
	data := contractFixture(t)
	fieldID := contractWritableField(t, data)

	const filename = "appears.pdf"
	body := []byte("%PDF-1.4 appears on the field")

	writer := contractClient(t, contractToken(t, data, documentsWriter))
	attached, _, err := writer.AttachDocument(context.Background(), fieldID, api.Upload{
		Filename:    filename,
		ContentType: "application/pdf",
		Content:     body,
	}, contractTimeout)
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}

	reader := contractClient(t, contractToken(t, data, fieldsReader))
	page, err := reader.ListFields(context.Background(), api.FieldQuery{Limit: 200}, contractTimeout)
	if err != nil {
		t.Fatalf("GET /api/v1/fields: %v", err)
	}

	var found *api.Field
	for i := range page.Fields {
		if page.Fields[i].ID == fieldID {
			found = &page.Fields[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("field %d is not in the listing", fieldID)
	}
	if !found.HasDocument {
		t.Error("has_document is false on a field that was just given one")
	}

	var row *api.Document
	for i := range found.Documents {
		if found.Documents[i].ID == attached.ID {
			row = &found.Documents[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("document %d is not in the field's documents array", attached.ID)
	}
	if row.Filename != filename {
		t.Errorf("filename = %q, want %q", row.Filename, filename)
	}
	if row.ContentType != "application/pdf" {
		t.Errorf("content_type = %q", row.ContentType)
	}
	if row.ByteSize != int64(len(body)) {
		t.Errorf("byte_size = %d, want %d", row.ByteSize, len(body))
	}
}

// TestContractAnOffAllowlistTypeIs415WithAHint is the test that justifies the plan's gap
// 5 being an accepted gap rather than a bug.
//
// The CLI cannot pre-check the content-type allow-list, because the schema does not
// publish it. That is only tolerable while the server's refusal carries enough for a
// holder to act on, so the refusal is asserted to be exactly that: the documented code,
// and a hint that is actually there.
func TestContractAnOffAllowlistTypeIs415WithAHint(t *testing.T) {
	data := contractFixture(t)
	fieldID := contractWritableField(t, data)
	client := contractClient(t, contractToken(t, data, documentsWriter))

	_, _, err := client.AttachDocument(context.Background(), fieldID, api.Upload{
		Filename:    "notes.txt",
		ContentType: "text/plain",
		Content:     []byte("plain text"),
	}, contractTimeout)

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("a text/plain part was not refused: %v", err)
	}
	if apiErr.Code != "unsupported_media_type" {
		t.Errorf("code = %q, want unsupported_media_type", apiErr.Code)
	}
	if strings.TrimSpace(apiErr.Hint) == "" {
		t.Error("the 415 carried no hint, and the CLI has nothing else to tell the holder with")
	}

	// The schema still not publishing the list is what keeps this test load-bearing. If
	// it ever does, the CLI should pre-check and this becomes a different test.
	if uploads, ok := liveSchema(t).Limits["uploads"].(map[string]any); ok {
		if _, published := uploads["content_types"]; published {
			t.Log("the schema now publishes limits.uploads.content_types — the CLI can pre-check the allow-list (the plan's gap 5)")
		}
	}
}

// TestContractTheUploadCapIsPublishedAndEnforced checks both ends of the local size
// pre-check: that the number the CLI refuses against is really published, and that it is
// really the server's bound.
//
// Only the published number is tested against the server by *not* sending anything — a
// test that uploaded 25 MiB to watch it be rejected would move 25 MiB over the CI network
// to learn what the schema already says.
func TestContractTheUploadCapIsPublishedAndEnforced(t *testing.T) {
	schema := liveSchema(t)

	max, published := schema.MaxFileBytes()
	if !published {
		t.Fatal("the schema publishes no limits.uploads.max_file_bytes, so `documents attach` cannot pre-check a size")
	}
	if max <= 0 {
		t.Fatalf("max_file_bytes = %d", max)
	}

	// The pre-check is the CLI's, so it is the CLI's behaviour being asserted: one byte
	// over the published cap is refused here, with nothing sent.
	if err := checkSize(max+1, schema); err == nil {
		t.Error("a file one byte over the published cap was not refused")
	}
	if err := checkSize(max, schema); err != nil {
		t.Errorf("a file exactly at the cap was refused: %v", err)
	}
}

// TestContractWritesRefuseWithoutTheScope is the 403 path for the two write endpoints.
//
// Read tokens are used deliberately: they authenticate, so this is the scope check
// failing rather than the credential, and it costs nothing from the failed-auth budget.
func TestContractWritesRefuseWithoutTheScope(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, fieldsReader))

	t.Run("fields create", func(t *testing.T) {
		_, _, err := client.CreateField(context.Background(), api.CreateFieldInput{
			Label: uniqueLabel(t, "Contract scope"),
			Value: "x",
		}, contractTimeout)
		assertInsufficientScope(t, err)
	})

	t.Run("documents attach", func(t *testing.T) {
		_, _, err := client.AttachDocument(context.Background(), contractWritableField(t, data), api.Upload{
			Filename:    "scope.pdf",
			ContentType: "application/pdf",
			Content:     []byte("%PDF"),
		}, contractTimeout)
		assertInsufficientScope(t, err)
	})
}

func assertInsufficientScope(t *testing.T, err error) {
	t.Helper()

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("a token without the scope was not refused: %v", err)
	}
	if apiErr.Code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", apiErr.Code)
	}
	if apiErr.Status != 403 {
		t.Errorf("status = %d, want 403", apiErr.Status)
	}
}

// TestContractAMissingFieldIsIndistinguishable asserts the posture the API documents: a
// field that is not yours, is withdrawn, or never existed all answer identically, so none
// of the three can be probed for.
func TestContractAMissingFieldIsIndistinguishable(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, contractToken(t, data, documentsWriter))

	upload := api.Upload{Filename: "probe.pdf", ContentType: "application/pdf", Content: []byte("%PDF")}

	_, _, absent := client.AttachDocument(context.Background(), 2147483000, upload, contractTimeout)

	var withdrawnID int
	for _, field := range data.Fields {
		if field.Status == "withdrawn" {
			withdrawnID = field.ID
			break
		}
	}
	if withdrawnID == 0 {
		t.Skip("the fixture has no withdrawn field to compare against")
	}
	_, _, withdrawn := client.AttachDocument(context.Background(), withdrawnID, upload, contractTimeout)

	var absentErr, withdrawnErr *api.Error
	if !errors.As(absent, &absentErr) || !errors.As(withdrawn, &withdrawnErr) {
		t.Fatalf("one of the two did not fail with an API error: absent=%v withdrawn=%v", absent, withdrawn)
	}
	if absentErr.Code != "not_found" {
		t.Errorf("a nonexistent field gave %q, want not_found", absentErr.Code)
	}
	if absentErr.Code != withdrawnErr.Code || absentErr.Message != withdrawnErr.Message {
		t.Errorf("a withdrawn field is distinguishable from an absent one:\n absent: %s / %s\n withdrawn: %s / %s",
			absentErr.Code, absentErr.Message, withdrawnErr.Code, withdrawnErr.Message)
	}
}

// contractWritableField picks a field the write tokens may attach to: one of the seeded
// account's own, and not withdrawn (the lookup excludes those and would 404).
func contractWritableField(t *testing.T, data contractData) int {
	t.Helper()

	for _, field := range data.Fields {
		if field.Status == "active" {
			return field.ID
		}
	}
	t.Fatal("the fixture has no active field to attach a document to")
	return 0
}
