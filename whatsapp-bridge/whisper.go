package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Transcriber owns the voice-note transcription pipeline:
//
//  1. Download the audio blob from WhatsApp (via whatsmeow.Client.Download).
//  2. Convert the .ogg (Opus) container to 16 kHz mono WAV with ffmpeg.
//  3. Run whisper-cli (local-cpp) with the configured Spanish-tuned model,
//     OR call the OpenAI Whisper API when backend == "openai-api".
//  4. Write the transcript into messages.voice_note_transcript along with
//     the backend name + timestamp.
//
// Transcription runs on a bounded worker pool so high voice-note bursts do
// not overwhelm CPU or network. The default pool size is 1 because the
// large-v3 model is ~3x real-time on M-series silicon and the bottleneck
// is the model, not concurrency.
type Transcriber struct {
	cfg *Config
	db  *sql.DB

	jobs   chan transcriptionJob
	workers int
	wg     sync.WaitGroup
}

type transcriptionJob struct {
	MessageID string
	// AudioDownloader is a closure returning the decrypted audio bytes.
	// Passed by the bridge so the Transcriber does not need to import whatsmeow.
	AudioDownloader func(ctx context.Context) ([]byte, error)
	MimeType        string // e.g. "audio/ogg; codecs=opus"
}

func NewTranscriber(cfg *Config, db *sql.DB) *Transcriber {
	workers := 1
	t := &Transcriber{
		cfg:     cfg,
		db:      db,
		jobs:    make(chan transcriptionJob, 64),
		workers: workers,
	}
	for i := 0; i < workers; i++ {
		t.wg.Add(1)
		go t.worker(i)
	}
	return t
}

// Close drains pending jobs and stops workers.
func (t *Transcriber) Close() {
	close(t.jobs)
	t.wg.Wait()
}

// Enqueue schedules a transcription. Non-blocking; drops the job if the queue
// is full and logs a warning so the caller knows.
func (t *Transcriber) Enqueue(j transcriptionJob) {
	select {
	case t.jobs <- j:
	default:
		log.Printf("transcriber queue full; dropping job for message %s (increase queue or reduce traffic)", j.MessageID)
	}
}

func (t *Transcriber) worker(id int) {
	defer t.wg.Done()
	for j := range t.jobs {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		t.process(ctx, id, j)
		cancel()
	}
}

func (t *Transcriber) process(ctx context.Context, workerID int, j transcriptionJob) {
	start := time.Now()
	log.Printf("transcriber[%d]: starting job for message %s", workerID, j.MessageID)

	// Validate prerequisites early.
	switch t.cfg.WhisperBackend {
	case "local-cpp":
		if err := t.validateLocalCpp(); err != nil {
			log.Printf("transcriber[%d]: local-cpp validation failed: %v", workerID, err)
			return
		}
	case "openai-api":
		if t.cfg.WhisperAPIKey == "" {
			log.Printf("transcriber[%d]: openai-api backend requires WHATSAPP_WHISPER_API_KEY", workerID)
			return
		}
	default:
		log.Printf("transcriber[%d]: unknown backend %q", workerID, t.cfg.WhisperBackend)
		return
	}

	// Download the audio.
	audio, err := j.AudioDownloader(ctx)
	if err != nil {
		log.Printf("transcriber[%d]: download failed for %s: %v", workerID, j.MessageID, err)
		return
	}
	if len(audio) == 0 {
		log.Printf("transcriber[%d]: empty audio payload for %s; skipping", workerID, j.MessageID)
		return
	}

	// Write to a temp dir we own and clean up afterward.
	workDir, err := os.MkdirTemp("", "whatsapp-mcp-whisper-")
	if err != nil {
		log.Printf("transcriber[%d]: mkdir temp: %v", workerID, err)
		return
	}
	defer os.RemoveAll(workDir)

	oggPath := filepath.Join(workDir, "audio.ogg")
	if err := os.WriteFile(oggPath, audio, 0o600); err != nil {
		log.Printf("transcriber[%d]: write ogg: %v", workerID, err)
		return
	}

	wavPath, err := t.convertToWav(ctx, oggPath, workDir)
	if err != nil {
		log.Printf("transcriber[%d]: ffmpeg convert failed for %s: %v", workerID, j.MessageID, err)
		return
	}

	// Transcribe.
	var transcript string
	switch t.cfg.WhisperBackend {
	case "local-cpp":
		transcript, err = t.transcribeLocalCpp(ctx, wavPath)
	case "openai-api":
		transcript, err = t.transcribeOpenAI(ctx, wavPath)
	}
	if err != nil {
		log.Printf("transcriber[%d]: whisper failed for %s: %v", workerID, j.MessageID, err)
		return
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		log.Printf("transcriber[%d]: empty transcript for %s (silence or undetected speech)", workerID, j.MessageID)
		return
	}

	// Persist.
	now := time.Now().Unix()
	_, err = t.db.ExecContext(ctx, `
		UPDATE messages
		SET voice_note_transcript = ?,
		    voice_note_transcript_backend = ?,
		    voice_note_transcript_at = ?
		WHERE id = ?
	`, transcript, t.cfg.WhisperBackend, now, j.MessageID)
	if err != nil {
		log.Printf("transcriber[%d]: db update failed for %s: %v", workerID, j.MessageID, err)
		return
	}

	dur := time.Since(start).Round(time.Millisecond)
	log.Printf("transcriber[%d]: done %s in %s (%d chars)", workerID, j.MessageID, dur, len(transcript))
}

// --- local whisper.cpp ------------------------------------------------------

func (t *Transcriber) validateLocalCpp() error {
	bin := t.whisperBin()
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("whisper binary %q not found in PATH (install via `brew install whisper-cpp` or set WHATSAPP_WHISPER_BIN_PATH)", bin)
	}
	if t.cfg.WhisperModelPath == "" {
		return errors.New("WHATSAPP_WHISPER_MODEL_PATH is required for local-cpp backend")
	}
	if _, err := os.Stat(t.cfg.WhisperModelPath); err != nil {
		return fmt.Errorf("model file not found at %s: %w", t.cfg.WhisperModelPath, err)
	}
	// ffmpeg is required for every voice note (ogg → 16 kHz mono WAV before
	// whisper-cpp). Surfaced at startup instead of failing silently per-job:
	// non-interactive runners (launchd, systemd, supervisord, cron, Docker
	// without ENV) inherit a stripped PATH and `exec.LookPath("ffmpeg")` fails
	// even when Homebrew has ffmpeg installed. Set WHATSAPP_FFMPEG_BIN_PATH
	// to an absolute path (mirrors WHATSAPP_WHISPER_BIN_PATH).
	ffmpeg := t.ffmpegBin()
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return fmt.Errorf("ffmpeg binary %q not found in PATH (install via `brew install ffmpeg` or set WHATSAPP_FFMPEG_BIN_PATH to an absolute path)", ffmpeg)
	}
	return nil
}

func (t *Transcriber) whisperBin() string {
	if t.cfg.WhisperBinPath != "" {
		return t.cfg.WhisperBinPath
	}
	return "whisper-cli"
}

// ffmpegBin returns the ffmpeg executable to invoke for voice-note conversion.
// Mirrors whisperBin: env-override → bare name (resolved via PATH).
func (t *Transcriber) ffmpegBin() string {
	if t.cfg.FFmpegBinPath != "" {
		return t.cfg.FFmpegBinPath
	}
	return "ffmpeg"
}

// transcribeLocalCpp invokes whisper-cli and reads the resulting .json transcript file.
// whisper-cli emits `<output-stem>.json` next to the input wav when --output-json is set.
func (t *Transcriber) transcribeLocalCpp(ctx context.Context, wavPath string) (string, error) {
	stem := strings.TrimSuffix(wavPath, filepath.Ext(wavPath))
	jsonPath := stem + ".json"

	args := []string{
		"--model", t.cfg.WhisperModelPath,
		"--language", t.cfg.WhisperLanguage,
		"--output-json",
		"--output-file", stem,
		"--no-prints",
		wavPath,
	}
	cmd := exec.CommandContext(ctx, t.whisperBin(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper-cli exec: %w (stderr: %s)", err, strings.TrimSpace(string(out)))
	}

	b, err := os.ReadFile(jsonPath)
	if err != nil {
		// Some whisper-cli builds emit a sibling .txt; try that as fallback.
		txt, txtErr := os.ReadFile(stem + ".txt")
		if txtErr == nil {
			return string(txt), nil
		}
		return "", fmt.Errorf("read transcript json: %w", err)
	}

	// whisper-cli JSON shape: { "transcription": [ {"text": "..."}, ... ] }.
	var parsed struct {
		Transcription []struct {
			Text string `json:"text"`
		} `json:"transcription"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return "", fmt.Errorf("parse transcript json: %w", err)
	}
	parts := make([]string, 0, len(parsed.Transcription))
	for _, seg := range parsed.Transcription {
		if s := strings.TrimSpace(seg.Text); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " ")), nil
}

// --- OpenAI Whisper API -----------------------------------------------------

func (t *Transcriber) transcribeOpenAI(_ context.Context, _ string) (string, error) {
	// Documented as opt-in. Implementation lands when a user actually opts in;
	// the boilerplate is a multipart/form-data POST to /v1/audio/transcriptions.
	return "", errors.New("openai-api backend scaffold: implementation pending (opt-in only; set WHATSAPP_WHISPER_BACKEND=local-cpp for now)")
}

// --- ffmpeg helper ----------------------------------------------------------

// convertToWav normalizes any audio to 16 kHz mono 16-bit PCM WAV, the format
// whisper.cpp is tuned for. Returns the output path. The ffmpeg binary is
// resolved via t.ffmpegBin() so non-interactive runners (launchd, systemd,
// supervisord, cron) can set WHATSAPP_FFMPEG_BIN_PATH to an absolute path
// when the inherited PATH does not include the Homebrew/system ffmpeg dir.
func (t *Transcriber) convertToWav(ctx context.Context, inputPath, workDir string) (string, error) {
	outPath := filepath.Join(workDir, "audio.wav")
	args := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", inputPath,
		"-ar", "16000",
		"-ac", "1",
		"-c:a", "pcm_s16le",
		outPath,
	}
	cmd := exec.CommandContext(ctx, t.ffmpegBin(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffmpeg: %w (stderr: %s)", err, strings.TrimSpace(string(out)))
	}
	return outPath, nil
}
