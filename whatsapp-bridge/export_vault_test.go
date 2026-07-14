package main

import "testing"

// Windows-safety cases for exported chat filenames: reserved device stems
// and trailing dots/spaces produce files Windows refuses to create (or, for
// NUL, silently swallows). Same class shipped in whatsapp-vault-sync.
func TestSanitizeFilenameWindowsSafety(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Maria/Jose", "Maria-Jose"}, // path separators (pre-existing behavior)
		{"a\\b?c%d*e:f|g\"h<i>j[k]", "a-b-c-d-e-f-g-h-i-j-k-"},
		{"CON", "CON_"},
		{"con", "con_"},
		{"NUL", "NUL_"},
		{"COM1", "COM1_"},
		{"lpt9", "lpt9_"},
		{"COM0", "COM0"},           // 0 is not reserved
		{"CONTRERAS", "CONTRERAS"}, // longer than the device stem
		{"name...", "name"},
		{"name   ", "name"},
		{"", "Unknown"},
		{". .", "Unknown"},
	}
	for _, c := range cases {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatPairingCode(t *testing.T) {
	if got := FormatPairingCode("ABCD1234"); got != "ABCD-1234" {
		t.Errorf("FormatPairingCode(ABCD1234) = %q", got)
	}
	if got := FormatPairingCode("ABC"); got != "ABC" {
		t.Errorf("FormatPairingCode(ABC) = %q", got)
	}
	// Already-hyphenated input from whatsmeow stays stable.
	if got := FormatPairingCode("ABCD-1234"); got != "ABCD-1234" {
		t.Errorf("FormatPairingCode(ABCD-1234) = %q", got)
	}
}
