package render

import (
	"strings"
	"testing"
	"time"
)

var deadlineNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// The two formats and the boundary between them, matching FormatHelper#countdown_remain.
// The zero-padding of the second unit and its absence on the first are both part of the
// shape the web renders, so they are asserted literally.
func TestCountdownRemainMatchesTheProductsTwoFormats(t *testing.T) {
	cases := []struct {
		name  string
		after time.Duration
		want  string
	}{
		{"days and hours above the expiring window", 5*24*time.Hour + 15*time.Hour, "5d 15h"},
		{"hours and minutes below it", time.Hour + 7*time.Minute, "1h 07m"},
		{"exactly at the window renders as days", ExpiringWindow, "2d 00h"},
		{"one second below the window renders as hours", ExpiringWindow - time.Second, "47h 59m"},
		{"under an hour keeps the zero hour", 7 * time.Minute, "0h 07m"},
		{"a deadline that has passed has no countdown", -time.Hour, ""},
		{"the instant it arrives has no countdown", 0, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CountdownRemain(deadlineNow.Add(tc.after), deadlineNow)
			if got != tc.want {
				t.Errorf("CountdownRemain(+%s) = %q, want %q", tc.after, got, tc.want)
			}
		})
	}
}

func TestDeadlineRendersTheClosingFactForAClosedShare(t *testing.T) {
	future := deadlineNow.Add(30 * 24 * time.Hour)
	past := deadlineNow.Add(-2 * 24 * time.Hour)
	revoked := deadlineNow.Add(-36 * time.Hour)

	cases := []struct {
		name      string
		state     State
		expiresAt *time.Time
		revokedAt *time.Time
		want      string
	}{
		{"live counts down", StateLive, &future, nil, "30d 00h left"},
		{"expiring counts down in hours", StateExpiring, ptr(deadlineNow.Add(90 * time.Minute)), nil, "1h 30m left"},
		{"no expiry is labelled, not blank", StateLive, nil, nil, NoExpiry},
		{"expired shows when the deadline arrived", StateExpired, &past, nil, "2026-09-14T12:00Z"},
		{"revoked shows the revocation date", StateRevoked, &future, &revoked, "Revoked 2026-09-15"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Deadline(tc.state, tc.expiresAt, tc.revokedAt, deadlineNow)
			if got != tc.want {
				t.Errorf("Deadline(%s) = %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}

// The trap this guards, and the reason `revoked_at` was added to the API at all: revoking
// does not touch the expiry, so a revoked share keeps whatever future deadline it was
// minted with. Rendering that would tell a holder their killed link has three weeks left.
func TestDeadlineNeverShowsAFutureExpiryForARevokedShare(t *testing.T) {
	future := deadlineNow.Add(30 * 24 * time.Hour)
	revoked := deadlineNow.Add(-time.Hour)

	got := Deadline(StateRevoked, &future, &revoked, deadlineNow)

	if got != "Revoked 2026-09-16" {
		t.Errorf("Deadline = %q, want the revocation date", got)
	}
	for _, forbidden := range []string{"left", "29d", "30d", "2026-10"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("a revoked share's deadline must not mention %q: %q", forbidden, got)
		}
	}
}

// A server too old to send revoked_at leaves the CLI knowing the share is closed but not
// when. The honest answer is to say nothing here and let STATE carry it — not to fall back
// to the expiry, which is the lie above.
func TestDeadlineIsEmptyForARevokedShareWithNoRevokedAt(t *testing.T) {
	future := deadlineNow.Add(30 * 24 * time.Hour)

	if got := Deadline(StateRevoked, &future, nil, deadlineNow); got != "" {
		t.Errorf("Deadline = %q, want empty so the caller renders the em dash", got)
	}
}

// Clock skew: the server says live, this machine says the deadline passed. The server
// decides state, so the word stands; the figure does not get invented.
func TestDeadlineIsEmptyWhenAnOpenShareHasAlreadyPassedByThisClock(t *testing.T) {
	past := deadlineNow.Add(-time.Minute)

	if got := Deadline(StateLive, &past, nil, deadlineNow); got != "" {
		t.Errorf("Deadline = %q, want empty rather than a negative countdown", got)
	}
}

// An unknown state is treated as open and counted down from its timestamp, rather than
// guessed at. The server is the authority on what a state means.
func TestDeadlineTreatsAnUnknownStateAsOpen(t *testing.T) {
	future := deadlineNow.Add(3 * 24 * time.Hour)

	if got := Deadline(State("quarantined"), &future, nil, deadlineNow); got != "3d 00h left" {
		t.Errorf("Deadline = %q, want the countdown", got)
	}
}

// Expired with no expiry timestamp is a contradiction the server cannot produce — the
// state is derived from that very column. Asserted anyway because the renderer takes a
// pointer, and the alternative to an empty string is a panic.
func TestDeadlineIsEmptyForAnExpiredShareWithNoExpiry(t *testing.T) {
	if got := Deadline(StateExpired, nil, nil, deadlineNow); got != "" {
		t.Errorf("Deadline = %q, want empty", got)
	}
}

func ptr[T any](v T) *T { return &v }
