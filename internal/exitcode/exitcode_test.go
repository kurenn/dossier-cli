package exitcode

import "testing"

// Every code the API publishes, with the exit it must produce. The plan's §9.1 asks for
// "exit-code mapping for every code in the envelope table", and the point of writing all
// nineteen out rather than looping over the map is that this table is a second, independent
// statement of the contract: a typo'd edit to the map fails here instead of silently
// changing what a script sees.
func TestFromErrorCode(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{"unauthenticated", Unauthenticated},
		{"insufficient_scope", ScopeMissing},
		{"not_found", NotFound},

		{"bad_request", Invalid},
		{"unsupported_media_type", Invalid},
		{"validation_failed", Invalid},
		{"expiry_required", Invalid},
		{"no_fields", Invalid},
		{"file_too_large", Invalid},
		{"vault_full", Invalid},
		{"idempotency_key_required", Invalid},

		{"idempotency_key_reused", IdempotencyConflict},

		{"rate_limited", TryLater},
		{"request_in_flight", TryLater},

		{"share_expired", ShareClosed},
		{"share_revoked", ShareClosed},

		{"pin_required", PinRefused},
		{"pin_incorrect", PinRefused},
		{"pin_locked", PinRefused},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			got, known := FromErrorCode(tc.code)
			if !known {
				t.Fatalf("FromErrorCode(%q) reported the code as unknown", tc.code)
			}
			if got != tc.want {
				t.Errorf("FromErrorCode(%q) = %d, want %d", tc.code, got, tc.want)
			}
		})
	}

	if len(byCode) != len(cases) {
		t.Errorf("the map holds %d codes but this table asserts %d; one of them was edited alone", len(byCode), len(cases))
	}
}

// An unknown code must not be guessed at from its status class. A script that branched on
// a bucket nobody chose would act confidently on a decision this binary never made.
func TestFromErrorCodeUnknown(t *testing.T) {
	got, known := FromErrorCode("teapot_overflow")
	if known {
		t.Fatal("an invented code was reported as known")
	}
	if got != Unexpected {
		t.Errorf("unknown code mapped to %d, want Unexpected (%d)", got, Unexpected)
	}
}

// The three codes that must never collapse into each other, asserted directly because
// each one exists to let a caller take a *different* action:
//
//   - TryLater: nothing was written, repeating is safe.
//   - IdempotencyConflict: retrying cannot help, the bytes will not match.
//   - MintUnconfirmed: something may have been written, and repeating is the one action
//     that could mint twice.
func TestAmbiguousOutcomesAreDistinct(t *testing.T) {
	codes := map[string]int{
		"TryLater":            TryLater,
		"IdempotencyConflict": IdempotencyConflict,
		"MintUnconfirmed":     MintUnconfirmed,
		"Unexpected":          Unexpected,
	}

	seen := map[int]string{}
	for name, code := range codes {
		if previous, clash := seen[code]; clash {
			t.Errorf("%s and %s share exit %d; a script cannot tell them apart", name, previous, code)
		}
		seen[code] = name
	}
}

func TestUnmapped(t *testing.T) {
	t.Run("reports nothing when the server publishes only known codes", func(t *testing.T) {
		if got := Unmapped([]string{"not_found", "rate_limited"}); got != nil {
			t.Errorf("Unmapped returned %v, want nil", got)
		}
	})

	t.Run("reports the codes this binary is too old to understand, sorted", func(t *testing.T) {
		got := Unmapped([]string{"zzz_new_code", "not_found", "aaa_new_code"})

		if len(got) != 2 || got[0] != "aaa_new_code" || got[1] != "zzz_new_code" {
			t.Errorf("Unmapped returned %v, want [aaa_new_code zzz_new_code]", got)
		}
	})
}
