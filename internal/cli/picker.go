package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kurenn/dossier-cli/internal/api"
)

// pickExpiry runs the interactive picker of §6.1.
//
// The shape is the product argument, not decoration. Product rule 2 says expiry is
// required and not a setting, and that "no expiry" exists as the one labelled exception.
// So the presets are numbered together, a rule separates them, and the exception sits
// alone below it carrying the API's own note. A picker that listed five equal options
// would be contradicting the rule while appearing to implement it.
//
// Everything here comes from the schema: the presets, their labels, and the exception's
// label and note. Nothing is a literal in this file.
func (a *App) pickExpiry(schema *api.Schema, now time.Time) (Expiry, error) {
	presets := schema.Expiry.Presets
	if len(presets) == 0 {
		// Nothing to offer, and inventing a list here would be the one thing non-negotiable
		// 3 forbids. --expires still works, so the refusal points at it.
		return Expiry{}, Usagef(
			"This vault's API publishes no expiry presets, so there is nothing to choose from.\n\n" +
				"Pass --expires with a duration or a timestamp, or --no-expiry.\n")
	}

	fmt.Fprintf(a.Stderr, "Expiry (required)\n")

	exception := schema.Expiry.NoExpiry
	label := exception.Label
	if label == "" {
		label = NoExpiry
	}

	labels := make([]string, len(presets))
	for i, preset := range presets {
		labels[i] = presetLabel(preset)
	}

	// The exception's label is measured with the presets, because it sits in the same
	// column: "No expiry" is longer than any of "24 hours", "7 days", "30 days" or
	// "90 days", so a width taken from the presets alone would push the last row's note
	// out of line with the durations above it. Rune count, not byte length — a label from
	// a vault whose copy is not ASCII would otherwise over-pad.
	width := utf8.RuneCountInString(label)
	for _, candidate := range labels {
		if count := utf8.RuneCountInString(candidate); count > width {
			width = count
		}
	}

	for i, preset := range presets {
		fmt.Fprintf(a.Stderr, "  %d  %-*s  %s\n", i+1, width, labels[i], preset)
	}

	fmt.Fprintf(a.Stderr, "  %s\n", rule)
	fmt.Fprintf(a.Stderr, "  %d  %-*s  %s\n", len(presets)+1, width, label, exception.Note)

	answer, err := a.readLine("\nChoose 1–" + strconv.Itoa(len(presets)+1) + ": ")
	if err != nil {
		return Expiry{}, err
	}

	choice, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || choice < 1 || choice > len(presets)+1 {
		return Expiry{}, Usagef("%q is not one of the choices. Nothing was minted.\n", answer)
	}

	if choice == len(presets)+1 {
		return NoExpiryChosen(), nil
	}
	return ParseExpires(presets[choice-1], now, presets)
}

// rule is the divider between the presets and the exception — §7.2's "a rule" as the one
// piece of chrome a terminal can draw without colour.
const rule = "─"

// NoExpiry is the fallback label, used only if the schema omits one.
const NoExpiry = "No expiry"

// presetLabel turns an ISO 8601 duration into the words the product uses for it.
//
// The prototype's picker reads "24 hours", "7 days"; the API publishes "PT24H", "P7D". The
// duration is still shown beside the label, so this is a translation the holder can check
// rather than one they have to trust — and an unrecognised shape falls back to the
// duration itself rather than to a guess, so a preset this does not know about still
// renders as something true.
func presetLabel(preset string) string {
	matches := isoDuration.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(preset)))
	if matches == nil {
		return preset
	}

	part := func(i int, unit string) string {
		if matches[i] == "" || matches[i] == "0" {
			return ""
		}
		count, _ := strconv.Atoi(matches[i])
		if count == 1 {
			return "1 " + unit
		}
		return strconv.Itoa(count) + " " + unit + "s"
	}

	for _, candidate := range []string{
		part(1, "year"), part(2, "month"), part(3, "week"), part(4, "day"),
		part(5, "hour"), part(6, "minute"), part(7, "second"),
	} {
		if candidate != "" {
			// The first non-empty part only. Every preset the product ships is a single
			// unit, and composing "7 days 12 hours" here would invent a label format the
			// prototype never designed.
			return candidate
		}
	}
	return preset
}
