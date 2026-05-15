package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

// TranscriptBackfiller periodically scans the messages table for voice/audio
// rows that have re-download data (media_key) but no transcript, reconstitutes
// an AudioMessage protobuf from the stored fields, and re-enqueues them on
// the existing transcriber pool.
//
// This makes voice-note transcription self-healing: any failure mode that
// leaves a row with NULL voice_note_transcript (validation error, queue full,
// transient whisper-cli failure, ffmpeg crash, network blip during download)
// recovers on the next sweep, as long as WhatsApp's media CDN still has
// the audio bytes (the practical retention window is ~14 days; we cap our
// backfill window to match).
//
// The sweep is bounded by:
//   - WindowDays: ignore rows older than this (default 14).
//   - Limit: max rows per sweep (default 50).
//
// It is safe to run concurrently with live receive-time enqueues; both
// paths funnel through Transcriber.Enqueue, which de-dupes on MessageID
// at the DB-write level (the worker writes voice_note_transcript only
// if the column is still NULL — see whisper.go).
type TranscriptBackfiller struct {
	db          *sql.DB
	client      *whatsmeow.Client
	transcriber *Transcriber

	intervalSeconds int
	windowDays      int
	limit           int

	// Stats exposed on /healthcheck. Atomic so the HTTP handler can read
	// without locking against the sweeper goroutine.
	totalSwept       atomic.Int64
	totalEnqueued    atomic.Int64
	lastSweepUnix    atomic.Int64
	lastEnqueuedUnix atomic.Int64
}

func NewTranscriptBackfiller(db *sql.DB, client *whatsmeow.Client, transcriber *Transcriber, intervalSeconds, windowDays int) *TranscriptBackfiller {
	if intervalSeconds <= 0 {
		intervalSeconds = 900
	}
	if windowDays <= 0 {
		windowDays = 14
	}
	return &TranscriptBackfiller{
		db:              db,
		client:          client,
		transcriber:     transcriber,
		intervalSeconds: intervalSeconds,
		windowDays:      windowDays,
		limit:           50,
	}
}

// Run starts the periodic sweeper. Returns when ctx is canceled.
// The first sweep fires after a short startup delay so it doesn't compete
// with the initial whatsmeow history-sync ingest on cold boot.
func (b *TranscriptBackfiller) Run(ctx context.Context) {
	if b.client == nil || b.transcriber == nil {
		log.Printf("backfiller: disabled (no client or transcriber)")
		return
	}

	const startupDelay = 30 * time.Second
	timer := time.NewTimer(startupDelay)
	defer timer.Stop()

	tick := time.Duration(b.intervalSeconds) * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			n, err := b.SweepOnce(ctx)
			if err != nil {
				log.Printf("backfiller: sweep error: %v", err)
			} else if n > 0 {
				log.Printf("backfiller: sweep enqueued %d voice-note jobs", n)
			}
			timer.Reset(tick)
		}
	}
}

// SweepOnce runs one backfill pass and returns the number of jobs enqueued.
// Exposed so the manual /api/admin/backfill-transcripts endpoint can trigger
// the same logic on demand.
func (b *TranscriptBackfiller) SweepOnce(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-time.Duration(b.windowDays) * 24 * time.Hour).Unix()

	rows, err := b.db.QueryContext(ctx, `
		SELECT id, media_key, media_direct_path, media_url, media_enc_sha256, media_sha256, media_file_length, media_key_timestamp
		FROM messages
		WHERE type IN ('voice', 'audio')
		  AND voice_note_transcript IS NULL
		  AND media_key IS NOT NULL
		  AND timestamp >= ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, cutoff, b.limit)
	if err != nil {
		return 0, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()

	enqueued := 0
	swept := 0
	for rows.Next() {
		swept++
		var (
			id                                       string
			mediaKey, mediaEncSha, mediaSha          []byte
			directPath, mediaURL                     sql.NullString
			fileLength, mediaKeyTimestamp            sql.NullInt64
		)
		if err := rows.Scan(&id, &mediaKey, &directPath, &mediaURL, &mediaEncSha, &mediaSha, &fileLength, &mediaKeyTimestamp); err != nil {
			log.Printf("backfiller: row scan failed: %v", err)
			continue
		}

		// Reconstitute a minimal AudioMessage that whatsmeow.Client.Download
		// can consume. URL or DirectPath alone is enough for the CDN fetch;
		// the encryption fields decrypt the response.
		audio := &waE2E.AudioMessage{
			MediaKey:      mediaKey,
			FileEncSHA256: mediaEncSha,
			FileSHA256:    mediaSha,
		}
		if directPath.Valid {
			s := directPath.String
			audio.DirectPath = &s
		}
		if mediaURL.Valid {
			s := mediaURL.String
			audio.URL = &s
		}
		if fileLength.Valid {
			fl := uint64(fileLength.Int64)
			audio.FileLength = &fl
		}
		if mediaKeyTimestamp.Valid {
			mkt := mediaKeyTimestamp.Int64
			audio.MediaKeyTimestamp = &mkt
		}

		client := b.client
		messageID := id
		b.transcriber.Enqueue(transcriptionJob{
			MessageID: messageID,
			MimeType:  "audio/ogg; codecs=opus",
			AudioDownloader: func(ctx context.Context) ([]byte, error) {
				return client.Download(ctx, audio)
			},
		})
		enqueued++
	}
	if err := rows.Err(); err != nil {
		return enqueued, fmt.Errorf("rows iter: %w", err)
	}

	b.totalSwept.Add(int64(swept))
	if enqueued > 0 {
		b.totalEnqueued.Add(int64(enqueued))
		b.lastEnqueuedUnix.Store(time.Now().Unix())
	}
	b.lastSweepUnix.Store(time.Now().Unix())
	return enqueued, nil
}

// Stats returns a snapshot for the /healthcheck endpoint. Goroutine-safe.
func (b *TranscriptBackfiller) Stats() map[string]any {
	return map[string]any{
		"interval_seconds":   b.intervalSeconds,
		"window_days":        b.windowDays,
		"total_swept":        b.totalSwept.Load(),
		"total_enqueued":     b.totalEnqueued.Load(),
		"last_sweep_unix":    b.lastSweepUnix.Load(),
		"last_enqueued_unix": b.lastEnqueuedUnix.Load(),
	}
}
