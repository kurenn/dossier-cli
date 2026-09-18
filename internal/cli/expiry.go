package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Expiry is a resolved deadline: either an instant, or the labelled exception.
//
// A struct rather than a *time.Time because the two are not "a value and its absence".
// Product rule 2 makes "no expiry" a deliberate, named choice, and an unset pointer is
// exactly what a client that forgot to ask would also produce. `stated` is what says the
// holder chose.
type Expiry struct {
	At       *time.Time
	stated   bool
	Shown    string // what --expires was given, for the confirmation prose
	Preset   bool   // whether it came from the schema's preset list
	NoExpiry bool
}

// Stated reports whether a deadline was actually chosen. Nothing may mint without one.
func (e Expiry) Stated() bool { return e.stated }

// ISO renders the value for the request body: a timestamp, or the empty string for the
// JSON null the API requires for no-expiry.
func (e Expiry) ISO() string {
	if e.At == nil {
		return ""
	}
	return e.At.UTC().Format(time.RFC3339)
}

// NoExpiryChosen is the labelled exception, chosen explicitly.
func NoExpiryChosen() Expiry {
	return Expiry{stated: true, NoExpiry: true, Shown: "No expiry"}
}

// isoDuration matches the subset of ISO 8601 durations this accepts, which is every part
// except the ones that cannot appear in a deadline a human picks.
//
// Years and months are kept even though they are not fixed spans, because they are
// legitimate ISO and refusing them would be a surprise — they are applied with calendar
// arithmetic below rather than converted to a count of seconds, so P1M lands on the same
// day of the next month rather than 30 days out.
var isoDuration = regexp.MustCompile(
	`^P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// shorthand matches 24h, 7d, 2w — the spellings a holder types when they are not thinking
// in ISO.
var shorthand = regexp.MustCompile(`^(\d+)([a-zA-Z]+)$`)

// ParseExpires resolves the --expires value against a clock.
//
// Three shapes, in the order they are tried: an ISO 8601 duration, a shorthand, or an ISO
// 8601 timestamp. A duration is added to now *here*, client-side, because the schema's own
// expiry.note says to: the API takes a timestamp and does not accept a duration, so the
// arithmetic is the client's job. What is sent and what is printed back is the computed
// instant, so the holder confirms a deadline rather than a sum.
func ParseExpires(value string, now time.Time, presets []string) (Expiry, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return Expiry{}, Usagef("--expires needs a value.\n\n%s", expiresForms(presets))
	}

	if matches := isoDuration.FindStringSubmatch(strings.ToUpper(trimmed)); matches != nil {
		// "P" alone matches the pattern with every group empty, which is a valid regexp
		// result and not a duration.
		if strings.Trim(strings.ToUpper(trimmed), "PT") == "" {
			return Expiry{}, Usagef("%q names no length of time.\n\n%s", value, expiresForms(presets))
		}
		at := addISO(now, matches)
		return Expiry{
			At:     &at,
			stated: true,
			Shown:  strings.ToUpper(trimmed),
			Preset: containsFold(presets, trimmed),
		}, nil
	}

	if matches := shorthand.FindStringSubmatch(trimmed); matches != nil {
		at, err := addShorthand(now, matches[1], matches[2])
		if err != nil {
			return Expiry{}, err
		}
		return Expiry{At: &at, stated: true, Shown: trimmed}, nil
	}

	if at, err := parseTimestamp(trimmed); err == nil {
		// Checked here as well as at the server, because the refusal is better early: the
		// API's own message for a past timestamp is a validation failure among several,
		// and this one can name the value and the clock it was compared against.
		if !at.After(now) {
			return Expiry{}, Usagef(
				"%s is not in the future.\n\nIt is now %s. A dossier that has already expired cannot be minted.\n",
				at.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		}
		return Expiry{At: &at, stated: true, Shown: at.UTC().Format(time.RFC3339)}, nil
	}

	return Expiry{}, Usagef("%q is not a duration or a timestamp.\n\n%s", value, expiresForms(presets))
}

// addISO applies a parsed ISO duration with calendar arithmetic for the date parts.
//
// AddDate for years, months and days, and Add for the clock parts. The split matters:
// a month is not 30 days and a day is not always 24 hours — across a DST boundary in a
// zoned clock, AddDate keeps the wall-clock time and Add does not. A holder who asks for
// P1D means tomorrow, not "in 86400 seconds".
func addISO(now time.Time, matches []string) time.Time {
	group := func(i int) int {
		if matches[i] == "" {
			return 0
		}
		n, _ := strconv.Atoi(matches[i])
		return n
	}

	years, months, weeks, days := group(1), group(2), group(3), group(4)
	hours, minutes, seconds := group(5), group(6), group(7)

	return now.
		AddDate(years, months, weeks*7+days).
		Add(time.Duration(hours)*time.Hour +
			time.Duration(minutes)*time.Minute +
			time.Duration(seconds)*time.Second)
}

func addShorthand(now time.Time, count, unit string) (time.Time, error) {
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 {
		return time.Time{}, Usagef("%s%s is not a length of time.\n", count, unit)
	}

	switch strings.ToLower(unit) {
	case "h":
		return now.Add(time.Duration(n) * time.Hour), nil
	case "d":
		return now.AddDate(0, 0, n), nil
	case "w":
		return now.AddDate(0, 0, n*7), nil
	case "m":
		// Refused rather than guessed. In ISO, M means months in the date part and minutes
		// in the time part, and this shorthand has no parts — so "30m" is genuinely
		// ambiguous between half an hour and two and a half years. A client that picked
		// one would be right half the time on the most consequential field in the product.
		return time.Time{}, Usagef(
			"%sm is ambiguous: it could be %s minutes or %s months.\n\n"+
				"Write it in ISO 8601, where the two are different: PT%sM for minutes, P%sM for months.\n",
			count, count, count, count, count)
	default:
		return time.Time{}, Usagef(
			"%q is not a unit this understands. Use h (hours), d (days) or w (weeks),\n"+
				"or an ISO 8601 duration like PT24H or P7D.\n", unit)
	}
}

// parseTimestamp accepts RFC 3339, and the same thing without a zone.
//
// A bare local time is accepted because "2026-12-01T09:00" is what a holder types, and
// refusing it over a missing Z would be pedantry — it is interpreted in the machine's own
// zone and echoed back in UTC, so the confirmation shows what was understood.
func parseTimestamp(value string) (time.Time, error) {
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if at, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a timestamp", value)
}

// expiresForms is the help that accompanies every --expires refusal. It lists the schema's
// presets rather than a set of examples chosen here, so the CLI never suggests a preset the
// product does not offer.
func expiresForms(presets []string) string {
	var builder strings.Builder
	builder.WriteString("Accepted:\n\n")
	builder.WriteString("    an ISO 8601 duration     PT24H, P7D, P30D\n")
	builder.WriteString("    a shorthand              24h, 7d, 2w\n")
	builder.WriteString("    an exact timestamp       2026-12-01T09:00:00Z\n")
	if len(presets) > 0 {
		builder.WriteString("\nThe presets this vault offers: " + strings.Join(presets, ", ") + "\n")
	}
	builder.WriteString("\nOr --no-expiry, which is the labelled exception.\n")
	return builder.String()
}

func containsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(value, needle) {
			return true
		}
	}
	return false
}
