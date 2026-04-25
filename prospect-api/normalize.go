package main

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Normalize returns an ASCII-folded, lowercase form of s.
// Used for accent-insensitive matching: "Muñoz" -> "munoz", "José" -> "jose".
func Normalize(s string) string {
	if s == "" {
		return ""
	}
	t := transform.Chain(
		norm.NFD,
		runes.Remove(runes.In(unicode.Mn)),
		norm.NFC,
	)
	out, _, err := transform.String(t, s)
	if err != nil {
		return strings.ToLower(s)
	}
	return strings.ToLower(out)
}

// DigitsOnly strips non-digit characters from s.
func DigitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NormalizeEmail returns a lowercase, trimmed email for case-insensitive comparison.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
