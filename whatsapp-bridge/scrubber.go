package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// InjectionPatterns are phrases commonly used in prompt-injection attacks.
// Match is case-insensitive, substring.
// The original message text is always preserved in the database; scrubbed output
// is only what Claude sees via the API. This keeps the audit trail intact.
var InjectionPatterns = []string{
	"ignore previous instructions",
	"ignore all previous instructions",
	"ignore above instructions",
	"disregard all prior",
	"disregard prior instructions",
	"you are now",
	"system:",
	"<system>",
	"</system>",
	"assistant:",
	"<|im_start|>",
	"<|im_end|>",
	"reveal your instructions",
	"reveal your system prompt",
	"print your system prompt",
	"dump your system prompt",
	"tell me your instructions",
	"what are your instructions",
}

// injectionRegexps match InjectionPatterns case-insensitively, one per pattern.
//
// They replace a loop that found a pattern's position in strings.ToLower(text)
// and cut the ORIGINAL text at that byte offset. Some characters change length
// when lowercased (U+023A grows from 2 bytes to 3, U+0130 shrinks from 2 to 1),
// so the offset landed in the wrong place: "Ⱥsystem:" panicked with a
// slice-bounds error, and "İgnore previous instructions" came out with a letter
// of the pattern left behind. Both are text a stranger can send, and a
// voice-note transcript now goes through here too. A regexp matches on the text
// itself, so there is no offset to get wrong.
//
// strings.ToLower also turned the Turkish dotted capital İ into i, so the old
// matcher caught "İgnore previous instructions". (?i) folds I and i but not İ,
// so each i in a pattern also accepts İ here. Going the other way, (?i) folds
// the long s (ſ) and the Kelvin sign (K) onto s and k, which the old matcher did
// not do for ſ.
var injectionRegexps = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(InjectionPatterns))
	for i, pat := range InjectionPatterns {
		res[i] = regexp.MustCompile("(?i)" + strings.ReplaceAll(regexp.QuoteMeta(pat), "i", "[i\u0130]"))
	}
	return res
}()

// Scrub replaces every known injection pattern with "[REDACTED_INJECTION]" and returns
// both the scrubbed string and a list of matched pattern tags (for audit).
// If no patterns match, returns (input, nil).
func Scrub(s string) (string, []string) {
	if s == "" {
		return s, nil
	}
	flags := make([]string, 0)
	out := s
	for i, re := range injectionRegexps {
		// Flagged against the original text, replaced in the running result,
		// exactly as before.
		if !re.MatchString(s) {
			continue
		}
		flags = append(flags, InjectionPatterns[i])
		out = re.ReplaceAllLiteralString(out, "[REDACTED_INJECTION]")
	}
	if len(flags) == 0 {
		return s, nil
	}
	return out, flags
}

// ScrubFlagsJSON marshals scrub flags to a JSON array string for storage.
// Returns "" when flags is empty so we don't write empty-array noise to the DB.
func ScrubFlagsJSON(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	b, err := json.Marshal(flags)
	if err != nil {
		return ""
	}
	return string(b)
}
