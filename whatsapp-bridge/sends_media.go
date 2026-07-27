// sends_media.go — outbound media for the draft/confirm send flow.
//
// The `sends` table has carried `content_file_path` and `content_media_mime`
// columns, and allowed send_type 'file' and 'audio', since the initial
// migration; only the code was missing. This file fills that gap.
//
// Two invariants shape the design:
//
//  1. The confirm step is the ONLY place an outbound network call happens
//     (see the contract at the top of sends.go). Uploading at draft time
//     would push the user's bytes to WhatsApp's servers before they approved
//     the send, quietly weakening the guarantee the two-step flow exists to
//     provide. So drafts only ever touch the local disk; Upload runs in
//     confirm.
//
//  2. Nothing here mutates a draft's stored bytes. A draft that expires or is
//     abandoned leaves a file behind, so expiry sweeps delete it.
//
// Voice notes are the one place a plain file is not enough: WhatsApp renders
// a message as a voice note (PTT) only when the audio is Opus in an Ogg
// container. Anything else has to be transcoded, which needs ffmpeg — already
// a dependency of the transcription path, so ffmpegBin() is reused rather
// than introducing a second way to find it.

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// WhatsApp's own ceilings. Documents are the most permissive; images and
// audio are rejected server-side well below that. Checking locally turns a
// confusing remote failure into a clear message before anything is uploaded.
const (
	maxImageBytes    = 16 << 20  // 16 MiB
	maxAudioBytes    = 16 << 20  // 16 MiB
	maxDocumentBytes = 100 << 20 // 100 MiB
)

// opusOggMIMEs are the containers WhatsApp accepts for a voice note as-is.
var opusOggMIMEs = map[string]bool{
	"audio/ogg":       true,
	"audio/ogg; codecs=opus": true,
	"audio/opus":      true,
}

// outboundDir is where draft bytes wait between create and confirm. Kept
// separate from the inbound media folder so a cleanup sweep over abandoned
// drafts can never touch downloaded history.
func outboundDir(cfg *Config) string {
	return filepath.Join(cfg.MediaPath, "outbound")
}

// materializeOutboundFile writes the draft's bytes to disk and reports the
// path and MIME type to store on the draft row.
//
// Accepts either a path already on this machine or inline base64. Both exist
// for a reason: a local stdio client can pass a path, but a remote client
// (claude.ai) has no filesystem here, so anything it generates can only
// arrive inline.
func materializeOutboundFile(cfg *Config, draftID string, req createDraftRequest) (string, string, error) {
	var data []byte
	var nameHint string

	switch {
	case req.FilePath != "" && req.FileBase64 != "":
		return "", "", fmt.Errorf("pass file_path or file_base64, not both")

	case req.FilePath != "":
		clean := filepath.Clean(req.FilePath)
		info, err := os.Stat(clean)
		if err != nil {
			return "", "", fmt.Errorf("file_path %q: %w", req.FilePath, err)
		}
		if info.IsDir() {
			return "", "", fmt.Errorf("file_path %q is a directory", req.FilePath)
		}
		data, err = os.ReadFile(clean)
		if err != nil {
			return "", "", fmt.Errorf("reading file_path %q: %w", req.FilePath, err)
		}
		nameHint = filepath.Base(clean)

	case req.FileBase64 != "":
		var err error
		data, err = base64.StdEncoding.DecodeString(req.FileBase64)
		if err != nil {
			return "", "", fmt.Errorf("file_base64 is not valid base64: %w", err)
		}
		nameHint = req.Filename

	default:
		return "", "", fmt.Errorf("file_path or file_base64 required for send_type=%q", req.SendType)
	}

	if len(data) == 0 {
		return "", "", fmt.Errorf("the file is empty")
	}
	if req.Filename != "" {
		nameHint = req.Filename
	}

	mimeType := resolveMIME(req.MediaMIME, nameHint, data)
	if err := checkSizeLimit(req.SendType, mimeType, len(data)); err != nil {
		return "", "", err
	}

	dir := outboundDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("creating outbound dir: %w", err)
	}

	ext := filepath.Ext(nameHint)
	if ext == "" {
		if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
			ext = exts[0]
		}
	}
	out := filepath.Join(dir, draftID+ext)
	if err := os.WriteFile(out, data, 0o600); err != nil {
		return "", "", fmt.Errorf("writing outbound file: %w", err)
	}
	return out, mimeType, nil
}

// resolveMIME picks the most trustworthy answer available: an explicit
// caller-supplied type, then the filename extension, then content sniffing.
// Sniffing is last because it returns application/octet-stream for plenty of
// real formats, which would push files that are really images down the
// document path.
func resolveMIME(explicit, nameHint string, data []byte) string {
	if explicit != "" {
		return strings.ToLower(strings.TrimSpace(explicit))
	}
	if ext := filepath.Ext(nameHint); ext != "" {
		if byExt := mime.TypeByExtension(ext); byExt != "" {
			return strings.ToLower(byExt)
		}
	}
	return strings.ToLower(http.DetectContentType(data))
}

func checkSizeLimit(sendType, mimeType string, size int) error {
	limit := maxDocumentBytes
	label := "documento"
	switch {
	case sendType == "audio":
		limit, label = maxAudioBytes, "audio"
	case strings.HasPrefix(mimeType, "image/"):
		limit, label = maxImageBytes, "imagen"
	}
	if size > limit {
		return fmt.Errorf("el %s pesa %d bytes y WhatsApp acepta hasta %d", label, size, limit)
	}
	return nil
}

// pickMediaType maps a draft onto one of whatsmeow's four upload kinds.
//
// Split out from buildMediaMessage so it can be tested without a live
// client: the branch that decides whether a JPEG becomes a photo or a
// document is exactly the one worth having tests on, and it is unreachable
// in a unit test if it only exists inline next to a network call.
//
// as_document deliberately wins over the MIME type. WhatsApp recompresses
// anything sent as an image, so "send this receipt as a document" has to be
// able to override what the bytes look like.
func pickMediaType(sendType, mimeType string, asDocument bool) whatsmeow.MediaType {
	if sendType == "audio" {
		return whatsmeow.MediaAudio
	}
	if asDocument {
		return whatsmeow.MediaDocument
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return whatsmeow.MediaImage
	case strings.HasPrefix(mimeType, "video/"):
		return whatsmeow.MediaVideo
	default:
		return whatsmeow.MediaDocument
	}
}

// mediaMessageOpts carries the per-draft choices confirm needs. Grouped into
// a struct because threading six positional arguments through a build
// function is how the wrong bool ends up in the wrong slot.
type mediaMessageOpts struct {
	SendType   string // "file" | "audio"
	Path       string
	MIME       string
	Filename   string
	Caption    string
	AsDocument bool // force the document path even for an image
	VoiceNote  bool // render as PTT rather than an audio attachment
	FFmpegBin  string
}

// buildMediaMessage uploads the draft's bytes and returns the protobuf to
// send. This is the only function here that touches the network, and it is
// called exclusively from the confirm handler.
func buildMediaMessage(ctx context.Context, cli *whatsmeow.Client, o mediaMessageOpts) (*waE2E.Message, error) {
	path := o.Path
	mimeType := o.MIME

	// A voice note has to be Opus/Ogg. Transcode only when it is not already,
	// so a correctly-formatted file never pays for ffmpeg — and a deployment
	// without ffmpeg can still send those.
	if o.SendType == "audio" && o.VoiceNote && !opusOggMIMEs[mimeType] {
		converted, err := transcodeToOpusOgg(ctx, o.FFmpegBin, path)
		if err != nil {
			return nil, err
		}
		defer os.Remove(converted)
		path = converted
		mimeType = "audio/ogg; codecs=opus"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading draft file: %w", err)
	}

	mediaType := pickMediaType(o.SendType, mimeType, o.AsDocument)

	up, err := cli.Upload(ctx, data, mediaType)
	if err != nil {
		return nil, fmt.Errorf("uploading media: %w", err)
	}

	switch mediaType {
	case whatsmeow.MediaImage:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption:       nonEmpty(o.Caption),
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}}, nil

	case whatsmeow.MediaVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			Caption:       nonEmpty(o.Caption),
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}}, nil

	case whatsmeow.MediaAudio:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype:      proto.String(mimeType),
			PTT:           proto.Bool(o.VoiceNote),
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}}, nil

	default:
		name := o.Filename
		if name == "" {
			name = filepath.Base(o.Path)
		}
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Caption:       nonEmpty(o.Caption),
			FileName:      proto.String(name),
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}}, nil
	}
}

// nonEmpty returns nil for an empty caption rather than a pointer to "".
// WhatsApp renders an empty-string caption as a blank line under the media.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

// transcodeToOpusOgg converts any ffmpeg-readable audio into the one format
// WhatsApp accepts for voice notes. Mirrors the ogg → wav call in whisper.go,
// including reusing its binary lookup, so there is a single answer to "where
// is ffmpeg" across the project.
func transcodeToOpusOgg(ctx context.Context, ffmpegBin, src string) (string, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	if _, err := exec.LookPath(ffmpegBin); err != nil {
		return "", fmt.Errorf(
			"las notas de voz requieren Opus/Ogg y el archivo no lo es; ffmpeg %q no esta en el PATH (%s, o define WHATSAPP_FFMPEG_BIN_PATH). "+
				"Envialo con voice_note=false para mandarlo como audio adjunto sin convertir",
			ffmpegBin, ffmpegInstallHint())
	}

	dst := strings.TrimSuffix(src, filepath.Ext(src)) + ".voice.ogg"
	cmd := exec.CommandContext(ctx, ffmpegBin,
		"-y",
		"-i", src,
		"-vn",              // discard any cover art; a video stream breaks the ogg
		"-ac", "1",         // WhatsApp voice notes are mono
		"-ar", "48000",     // Opus's native rate; anything else gets resampled anyway
		"-c:a", "libopus",
		"-b:a", "32k",      // what the WhatsApp client itself records at
		dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg no pudo convertir a Opus/Ogg: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return dst, nil
}

// removeOutboundFile deletes a draft's bytes. Errors are returned rather than
// swallowed so callers can log them; a leaked file is harmless but silently
// leaking every abandoned draft is not.
func removeOutboundFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
