package render

import (
	"fmt"
	"time"
)

// ExpiringWindow mirrors Share::EXPIRING_WINDOW. It is the threshold the product derives
// the `expiring` state at, and also the point where the countdown changes shape — days and
// hours above it, hours and minutes below.
//
// This is the one number in the CLI copied from the server rather than read from the
// schema, because the schema does not publish it. It only ever changes which of two
// formats a figure is printed in, never whether a link works, so a drift here is cosmetic.
// The alternative — deriving the format from the state word — would tie a number's shape
// to a string the server computed for a different purpose, and would print days-and-hours
// for a revoked share that happens to have weeks left on its expiry.
const ExpiringWindow = 48 * time.Hour

// NoExpiry is the label for a share minted without a deadline.
//
// Product rule 2: expiry is required, not a setting, and "no expiry" is the exception and
// is labelled as the exception. So this column says so in words rather than showing the em
// dash an absent value would otherwise get — a blank would read as "unknown", when it is
// in fact a deliberate and unusual choice the holder made.
const NoExpiry = "No expiry"

// CountdownRemain renders the time left as the product's static countdown: "5d 15h" above
// the expiring window, "1h 07m" below it. Empty when there is no expiry or the deadline
// has already passed.
//
// Ported from FormatHelper#countdown_remain, including the zero-padding of the second
// unit and the absence of it on the first. The rendered shape has to match the web
// exactly — it is the same product, and a holder reading "5d 15h left" on one surface and
// "5d 15h" or "5 days 15 hours" on the other would reasonably wonder which was rounded.
func CountdownRemain(expiresAt, now time.Time) string {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return ""
	}

	if remaining < ExpiringWindow {
		hours := int(remaining.Hours())
		minutes := int(remaining.Minutes()) % 60
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	}

	days := int(remaining.Hours()) / 24
	hours := int(remaining.Hours()) % 24
	return fmt.Sprintf("%dd %02dh", days, hours)
}

// Deadline renders the DEADLINE column for one share.
//
// §7.1 rule 3: the countdown is the only derived figure the CLI computes, it appears only
// here, and it never appears on a link that no longer works. For a closed share this shows
// the closing fact instead — the same split
// `SharesHelper#share_grid_deadline_text` makes for the web grid, with one deliberate
// difference: timestamps are ISO 8601 UTC as the API sent them, not the web's local
// "%Y-%m-%d %H:%M %Z", because rule 3 also says a timestamp in a data column is rendered
// exactly as the API sends it.
//
// expiresAt and revokedAt are nil when the API sent null, which is why they are pointers
// rather than zero times: a zero time and "no expiry" are different facts, and rule 2
// makes the difference a labelled one.
func Deadline(state State, expiresAt, revokedAt *time.Time, now time.Time) string {
	switch state {
	case StateExpired:
		// The deadline that arrived. Showing when is the closing fact; a countdown would
		// be negative and an em dash would throw away something known.
		if expiresAt == nil {
			return ""
		}
		return expiresAt.UTC().Format(isoMinutes)

	case StateRevoked:
		// A revoked share keeps whatever future expiry it was minted with, so its
		// `expires_at` must never be shown here — it would read as "still weeks left" on
		// a link that refuses every request. `revoked_at` is the closing fact, to the day,
		// matching the web's "Revoked 2026-09-14".
		//
		// Empty when the server did not send one. That is a server too old to have
		// `revoked_at` (gap 14), and the honest rendering is the em dash the caller
		// supplies: the CLI knows the share is closed, which the STATE column already
		// says, and does not know when.
		if revokedAt == nil {
			return ""
		}
		return "Revoked " + revokedAt.UTC().Format(time.DateOnly)

	default:
		// Open: live, expiring, or a state this binary does not know. An unknown state is
		// treated as open rather than guessed at, because the countdown is computed from
		// the timestamp and needs no opinion about the word.
		if expiresAt == nil {
			return NoExpiry
		}
		if remain := CountdownRemain(*expiresAt, now); remain != "" {
			return remain + " left"
		}
		// Open by the server's reckoning but already past by this clock. Rather than
		// print "0h 00m left" or a negative figure, say nothing and let the STATE column
		// stand: the server decides state (rule 2), and the disagreement is this
		// machine's clock, not news about the share.
		return ""
	}
}

// isoMinutes is ISO 8601 UTC to the minute, which is the resolution a deadline is set at.
// Seconds on an expiry would be noise in a column read by eye.
const isoMinutes = "2006-01-02T15:04Z"
