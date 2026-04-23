package main

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Normalize returns an ASCII-folded, lowercase form of s.
// Used for accent-insensitive search: "Muñoz" -> "munoz", "José" -> "jose".
// Implementation: NFD decomposition, then strip combining marks, then lowercase.
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
		// Fall back to lower-only on transform failure.
		return strings.ToLower(s)
	}
	return strings.ToLower(out)
}
