package main

import "testing"

// TestNormalizeASCII verifies the trivial path: ASCII text gets lowercased,
// otherwise unchanged. The happy path for English-only contacts.
func TestNormalizeASCII(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"Hello", "hello"},
		{"HELLO WORLD", "hello world"},
		{"already lower", "already lower"},
		{"123abc", "123abc"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeSpanishAccents is the search-correctness gate for Spanish-
// language contacts. "Muñoz" / "munoz", "José" / "jose" must all collapse
// to the same normalized form so search_contacts matches without the user
// having to type accent marks.
func TestNormalizeSpanishAccents(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Muñoz", "munoz"},
		{"MUÑOZ", "munoz"},
		{"José", "jose"},
		{"María", "maria"},
		{"Pérez", "perez"},
		{"Núñez", "nunez"},
		{"Ñoño", "nono"},
		{"Málaga", "malaga"},
		{"Niño", "nino"},
		{"Mañana", "manana"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeEuropeanAccents covers diacritics common in European names.
func TestNormalizeEuropeanAccents(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// German umlauts — NFD strips the diacritic, so ä → a (not the German
		// transliteration "ae"). Document the choice so it's not a surprise.
		{"Müller", "muller"},
		{"Zürich", "zurich"},
		{"Köln", "koln"},
		// French
		{"François", "francois"},
		{"École", "ecole"},
		{"Café", "cafe"},
		// Portuguese
		{"São Paulo", "sao paulo"},
		{"Conceição", "conceicao"},
		// Nordic (NFD-decomposable diacritics)
		{"Ångström", "angstrom"},
		{"Naïve", "naive"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeIdempotent verifies the function is idempotent — Normalize on
// already-normalized text returns the same thing. Important because the DB
// stores the normalized form and search queries also normalize their input;
// any drift between those two calls breaks the search.
func TestNormalizeIdempotent(t *testing.T) {
	inputs := []string{"Muñoz Perez", "José María", "Mañana Niño", "hello world"}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			once := Normalize(in)
			twice := Normalize(once)
			if once != twice {
				t.Errorf("Normalize not idempotent\n  in:    %q\n  once:  %q\n  twice: %q", in, once, twice)
			}
		})
	}
}

// TestNormalizeNonLatinPassthrough verifies non-Latin scripts pass through
// (lowercased where the script has casing). The point of Normalize is to
// strip diacritics for accent-insensitive Latin-script search, not to
// transliterate Cyrillic or Greek to Latin. Those scripts should match
// against themselves.
func TestNormalizeNonLatinPassthrough(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Cyrillic (Russian, Ukrainian)
		{"Москва", "москва"},
		// Greek
		{"Αθήνα", "αθηνα"}, // η carries no separate diacritic; ή decomposes
		// Arabic (no casing)
		{"مرحبا", "مرحبا"},
		// CJK (no casing, no diacritics)
		{"東京", "東京"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
