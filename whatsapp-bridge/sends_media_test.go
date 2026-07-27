package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
)

// A minimal but real PNG. Used where the test needs bytes that content
// sniffing will actually recognise as an image, rather than a string that
// only looks like one.
var tinyPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{MediaPath: t.TempDir()}
}

// --- pickMediaType ----------------------------------------------------------

func TestPickMediaType(t *testing.T) {
	tests := []struct {
		name       string
		sendType   string
		mime       string
		asDocument bool
		want       whatsmeow.MediaType
	}{
		{"image goes as photo by default", "file", "image/jpeg", false, whatsmeow.MediaImage},
		{"image forced to document", "file", "image/jpeg", true, whatsmeow.MediaDocument},
		{"video goes as video", "file", "video/mp4", false, whatsmeow.MediaVideo},
		{"video forced to document", "file", "video/mp4", true, whatsmeow.MediaDocument},
		{"pdf is a document", "file", "application/pdf", false, whatsmeow.MediaDocument},
		{"unknown type falls back to document", "file", "application/octet-stream", false, whatsmeow.MediaDocument},
		{"audio ignores mime", "audio", "audio/mpeg", false, whatsmeow.MediaAudio},
		// as_document is rejected for audio at the validator, but the mapping
		// must still not silently turn a voice note into a document if it
		// ever slipped through.
		{"audio ignores as_document", "audio", "audio/ogg", true, whatsmeow.MediaAudio},
		{"empty mime is a document", "file", "", false, whatsmeow.MediaDocument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickMediaType(tc.sendType, tc.mime, tc.asDocument); got != tc.want {
				t.Fatalf("pickMediaType(%q,%q,%v) = %q, want %q",
					tc.sendType, tc.mime, tc.asDocument, got, tc.want)
			}
		})
	}
}

// --- resolveMIME ------------------------------------------------------------

func TestResolveMIMEPrecedence(t *testing.T) {
	// An explicit type must beat both the extension and the bytes: the caller
	// knows things the file does not, and a .bin full of PDF is still a PDF.
	if got := resolveMIME("application/pdf", "thing.png", tinyPNG); got != "application/pdf" {
		t.Fatalf("explicit mime should win, got %q", got)
	}
	// Extension beats sniffing.
	if got := resolveMIME("", "doc.pdf", []byte("not really a pdf")); !strings.HasPrefix(got, "application/pdf") {
		t.Fatalf("extension should win over sniffing, got %q", got)
	}
	// Sniffing is the last resort.
	if got := resolveMIME("", "noextension", tinyPNG); got != "image/png" {
		t.Fatalf("sniffing should identify png, got %q", got)
	}
	// Case and padding from a caller-supplied value are normalised.
	if got := resolveMIME("  IMAGE/JPEG  ", "", nil); got != "image/jpeg" {
		t.Fatalf("explicit mime should be trimmed and lowercased, got %q", got)
	}
}

// --- checkSizeLimit ---------------------------------------------------------

func TestCheckSizeLimit(t *testing.T) {
	tests := []struct {
		name     string
		sendType string
		mime     string
		size     int
		wantErr  bool
	}{
		{"small image ok", "file", "image/jpeg", 1024, false},
		{"image at the limit ok", "file", "image/jpeg", maxImageBytes, false},
		{"image over the limit", "file", "image/jpeg", maxImageBytes + 1, true},
		{"document may exceed the image limit", "file", "application/pdf", maxImageBytes + 1, false},
		{"document at the limit ok", "file", "application/pdf", maxDocumentBytes, false},
		{"document over the limit", "file", "application/pdf", maxDocumentBytes + 1, true},
		{"audio over the limit", "audio", "audio/mpeg", maxAudioBytes + 1, true},
		// An image sent as a document is checked against the image ceiling
		// here because the limit is chosen from the MIME type. Documented as
		// deliberate: WhatsApp applies the stricter ceiling to image content
		// regardless of the envelope.
		{"image bytes as document still image-limited", "file", "image/png", maxImageBytes + 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSizeLimit(tc.sendType, tc.mime, tc.size)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for size %d", tc.size)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// --- materializeOutboundFile ------------------------------------------------

func TestMaterializeFromPath(t *testing.T) {
	cfg := testConfig(t)
	src := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(src, tinyPNG, 0o600); err != nil {
		t.Fatal(err)
	}

	got1, err := materializeOutboundFile(t.Context(), cfg, "draft-1", createDraftRequest{
		SendType: "file",
		FilePath: src,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got1.MIME != "image/png" {
		t.Fatalf("mime = %q, want image/png", got1.MIME)
	}
	// The name the recipient sees comes from the source file, not from the
	// draft id the bytes are stored under.
	if got1.Filename != "photo.png" {
		t.Fatalf("filename = %q, want photo.png", got1.Filename)
	}
	// The bytes must be copied into the bridge's own directory, not merely
	// referenced: the caller is free to delete or overwrite their file
	// between draft and confirm.
	if !strings.HasPrefix(got1.Path, outboundDir(cfg)) {
		t.Fatalf("file should live under the outbound dir, got %q", got1.Path)
	}
	got, err := os.ReadFile(got1.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(tinyPNG) {
		t.Fatal("copied bytes differ from the source")
	}
}

func TestMaterializeFromBase64(t *testing.T) {
	cfg := testConfig(t)
	got, err := materializeOutboundFile(t.Context(), cfg, "draft-2", createDraftRequest{
		SendType:   "file",
		FileBase64: base64.StdEncoding.EncodeToString(tinyPNG),
		Filename:   "recibo.png",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MIME != "image/png" {
		t.Fatalf("mime = %q, want image/png", got.MIME)
	}
	if filepath.Ext(got.Path) != ".png" {
		t.Fatalf("extension should come from the filename, got %q", got.Path)
	}
}

func TestMaterializeRejectsBadInput(t *testing.T) {
	cfg := testConfig(t)
	existing := filepath.Join(t.TempDir(), "f.png")
	if err := os.WriteFile(existing, tinyPNG, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		req  createDraftRequest
		want string
	}{
		{
			"no source at all",
			createDraftRequest{SendType: "file"},
			"required",
		},
		{
			"both path and bytes",
			createDraftRequest{SendType: "file", FilePath: existing, FileBase64: "aGk="},
			"exactly one",
		},
		{
			"both path and url",
			createDraftRequest{SendType: "file", FilePath: existing, FileURL: "https://example.com/a.png"},
			"exactly one",
		},
		{
			"all three sources",
			createDraftRequest{
				SendType: "file", FilePath: existing, FileBase64: "aGk=",
				FileURL: "https://example.com/a.png",
			},
			"exactly one",
		},
		{
			"missing file",
			createDraftRequest{SendType: "file", FilePath: filepath.Join(t.TempDir(), "nope.png")},
			"file_path",
		},
		{
			"directory instead of file",
			createDraftRequest{SendType: "file", FilePath: t.TempDir()},
			"directory",
		},
		{
			"invalid base64",
			createDraftRequest{SendType: "file", FileBase64: "!!!not base64!!!"},
			"base64",
		},
		{
			"empty payload",
			createDraftRequest{SendType: "file", FileBase64: ""},
			"required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := materializeOutboundFile(t.Context(), cfg, "d", tc.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestMaterializeRejectsEmptyFile(t *testing.T) {
	cfg := testConfig(t)
	empty := filepath.Join(t.TempDir(), "empty.pdf")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty file uploads "successfully" and arrives as an unopenable
	// attachment, so it is worth failing before the network call.
	if _, err := materializeOutboundFile(t.Context(), cfg, "d", createDraftRequest{
		SendType: "file", FilePath: empty,
	}); err == nil {
		t.Fatal("expected an empty file to be rejected")
	}
}

func TestMaterializeEnforcesSizeLimit(t *testing.T) {
	cfg := testConfig(t)
	// Declared as an image so the 16 MiB ceiling applies rather than the
	// document one, keeping the allocation modest.
	big := make([]byte, maxImageBytes+1)
	_, err := materializeOutboundFile(t.Context(), cfg, "d", createDraftRequest{
		SendType:   "file",
		FileBase64: base64.StdEncoding.EncodeToString(big),
		Filename:   "huge.jpg",
	})
	if err == nil {
		t.Fatal("expected the size limit to reject this")
	}
	if !strings.Contains(err.Error(), "WhatsApp") {
		t.Fatalf("the error should explain the limit is WhatsApp's: %v", err)
	}
}

// --- nonEmpty ---------------------------------------------------------------

func TestNonEmptyCaption(t *testing.T) {
	// An empty caption must be nil, not a pointer to "": WhatsApp renders the
	// latter as a blank line beneath the media.
	if nonEmpty("") != nil {
		t.Fatal("empty caption should be nil")
	}
	got := nonEmpty("hola")
	if got == nil || *got != "hola" {
		t.Fatalf("caption not preserved: %v", got)
	}
}

// --- removeOutboundFile -----------------------------------------------------

func TestRemoveOutboundFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOutboundFile(f); err != nil {
		t.Fatalf("removing an existing file: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("file should be gone")
	}
	// Idempotent: expiry sweeps and the insert-failure path can both target
	// the same draft, and the second one must not report a failure.
	if err := removeOutboundFile(f); err != nil {
		t.Fatalf("removing a missing file should be a no-op: %v", err)
	}
	if err := removeOutboundFile(""); err != nil {
		t.Fatalf("empty path should be a no-op: %v", err)
	}
}

// --- voice notes ------------------------------------------------------------

func TestOpusOggRecognised(t *testing.T) {
	// These skip transcoding entirely; getting the set wrong means every
	// voice note pays for an ffmpeg round trip, or worse, fails on a machine
	// without ffmpeg despite already being in the right format.
	for _, m := range []string{"audio/ogg", "audio/opus"} {
		if !opusOggMIMEs[m] {
			t.Fatalf("%q should be accepted as-is for a voice note", m)
		}
	}
	for _, m := range []string{"audio/mpeg", "audio/mp4", "audio/wav", ""} {
		if opusOggMIMEs[m] {
			t.Fatalf("%q should require transcoding", m)
		}
	}
}

func TestTranscodeReportsMissingFFmpeg(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.mp3")
	if err := os.WriteFile(src, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := transcodeToOpusOgg(t.Context(), "definitely-not-a-real-binary-xyz", src)
	if err == nil {
		t.Fatal("expected an error when ffmpeg is absent")
	}
	// The message has to tell the user how to proceed, not just what broke.
	if !strings.Contains(err.Error(), "voice_note=false") {
		t.Fatalf("the error should offer the no-transcode escape hatch: %v", err)
	}
	if !strings.Contains(err.Error(), "WHATSAPP_FFMPEG_BIN_PATH") {
		t.Fatalf("the error should name the config var: %v", err)
	}
}
