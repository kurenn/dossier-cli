package cli

import (
	"strings"
	"testing"
	"time"
)

// presets mirrors what the schema publishes, so a refusal's help lists the product's own
// four rather than four chosen here.
var testPresets = []string{"PT24H", "P7D", "P30D", "P90D"}

// The done-when for the arithmetic: --expires 7d resolves to now + P7D, and the computed
// instant is what gets sent. Each case is checked against a want computed from the same
// clock, so the assertion is the arithmetic rather than a literal that would have to be
// rewritten every time fixedNow moved.
func TestParseExpiresComputesTheInstantClientSide(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		value string
		want  time.Time
	}{
		{"P7D", now.AddDate(0, 0, 7)},
		{"7d", now.AddDate(0, 0, 7)},
		{"PT24H", now.Add(24 * time.Hour)},
		{"24h", now.Add(24 * time.Hour)},
		{"P30D", now.AddDate(0, 0, 30)},
		{"P90D", now.AddDate(0, 0, 90)},
		{"2w", now.AddDate(0, 0, 14)},
		{"PT30M", now.Add(30 * time.Minute)},
		{"P1DT12H", now.AddDate(0, 0, 1).Add(12 * time.Hour)},
		// Lower case accepted: a holder typing a duration should not have to hold shift.
		{"p7d", now.AddDate(0, 0, 7)},
		// A month is calendar arithmetic, not thirty days. September has thirty, so
		// P1M landing on 18 October rather than 18 October is the same here — but the
		// assertion is AddDate's answer, which is what makes February correct too.
		{"P1M", now.AddDate(0, 1, 0)},
	}

	for _, testCase := range cases {
		t.Run(testCase.value, func(t *testing.T) {
			expiry, err := ParseExpires(testCase.value, now, testPresets)
			if err != nil {
				t.Fatalf("ParseExpires(%q): %v", testCase.value, err)
			}
			if expiry.At == nil {
				t.Fatal("no instant was computed")
			}
			if !expiry.At.Equal(testCase.want) {
				t.Errorf("got %s, want %s", expiry.At.Format(time.RFC3339), testCase.want.Format(time.RFC3339))
			}
			if !expiry.Stated() {
				t.Error("a parsed value must count as a stated deadline")
			}
		})
	}
}

func TestParseExpiresAcceptsATimestamp(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	expiry, err := ParseExpires("2026-12-01T09:00:00Z", now, testPresets)
	if err != nil {
		t.Fatalf("ParseExpires: %v", err)
	}
	if got := expiry.ISO(); got != "2026-12-01T09:00:00Z" {
		t.Errorf("ISO() = %q", got)
	}
}

// A timestamp already past is refused here rather than at the server. The API rejects it
// too, among several validations under one code; this one can name the value and the clock
// it was compared against, and it costs nothing to say so before sending.
func TestParseExpiresRefusesAPastTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	_, err := ParseExpires("2026-01-01T00:00:00Z", now, testPresets)
	if err == nil {
		t.Fatal("a past timestamp was accepted")
	}
	if !strings.Contains(err.Error(), "is not in the future") {
		t.Errorf("message does not say why: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "2026-09-18T09:00:00Z") {
		t.Errorf("message does not name the clock it compared against: %q", err.Error())
	}
}

// "30m" is refused rather than guessed, because ISO gives M two meanings and this
// shorthand has no parts to disambiguate them. Half an hour and two and a half years are
// not a distinction to get wrong on the one field that decides when access stops.
func TestParseExpiresRefusesTheAmbiguousMinuteMonth(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	_, err := ParseExpires("30m", now, testPresets)
	if err == nil {
		t.Fatal("30m was accepted, which means one of minutes or months was guessed")
	}
	for _, want := range []string{"ambiguous", "PT30M", "P30M"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not offer %q: %q", want, err.Error())
		}
	}
}

func TestParseExpiresRefusesNonsenseAndListsTheProductsPresets(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

	for _, value := range []string{"soon", "", "P", "7", "-7d", "7y2"} {
		_, err := ParseExpires(value, now, testPresets)
		if err == nil {
			t.Fatalf("%q was accepted as a deadline", value)
		}
		if code := Classify(err).Code; code != 2 {
			t.Errorf("%q exited %d, want 2 — a malformed deadline is a usage error", value, code)
		}
	}

	_, err := ParseExpires("soon", now, testPresets)
	if !strings.Contains(err.Error(), "PT24H, P7D, P30D, P90D") {
		t.Errorf("the refusal does not list the presets the vault offers: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "--no-expiry") {
		t.Errorf("the refusal does not mention the labelled exception: %q", err.Error())
	}
}

// No-expiry is a choice, not an absence. The distinction is the whole of product rule 2:
// an unstated deadline must be refused, and this one must be sent as an explicit null.
func TestNoExpiryIsAStatedChoiceThatSendsNull(t *testing.T) {
	expiry := NoExpiryChosen()

	if !expiry.Stated() {
		t.Error("--no-expiry must count as a stated deadline")
	}
	if !expiry.NoExpiry {
		t.Error("NoExpiry must be set")
	}
	if expiry.At != nil {
		t.Error("no instant should be computed for the exception")
	}
	if expiry.ISO() != "" {
		t.Errorf("ISO() = %q, want empty so the body carries a JSON null", expiry.ISO())
	}

	// And the thing it must never be confused with: a zero Expiry, which is what a code
	// path that forgot to ask would produce.
	if (Expiry{}).Stated() {
		t.Error("an unstated Expiry must not pass for a choice")
	}
}

func TestPresetLabelsAreWordsWithTheDurationBeside(t *testing.T) {
	cases := map[string]string{
		"PT24H": "24 hours",
		"P7D":   "7 days",
		"P30D":  "30 days",
		"P90D":  "90 days",
		"P1D":   "1 day",
		"PT1H":  "1 hour",
		// Unrecognised shapes fall back to the duration itself rather than to a guess, so
		// a preset this does not know about still renders as something true.
		"whenever": "whenever",
	}

	for preset, want := range cases {
		if got := presetLabel(preset); got != want {
			t.Errorf("presetLabel(%q) = %q, want %q", preset, got, want)
		}
	}
}
