package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
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
// backfill window to match) or the audio was saved here (WHATSAPP_KEEP_VOICE_AUDIO,
// see voice_audio.go), in which case the note's age does not matter.
//
// The sweep is bounded by:
//   - WindowDays: ignore rows older than this (default 14) unless their audio
//     is saved on this machine.
//   - Limit: max notes QUEUED per sweep (default 50). It looks at up to
//     sweepScanFactor times that many rows to find them, so notes it has to skip
//     (a chat kept out of transcription, a note that keeps failing, a note
//     already queued, a deleted file) do not use up the slots and starve the
//     older notes behind them.
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

// sweepScanFactor is how many candidate rows a sweep reads per note it may queue.
const sweepScanFactor = 10

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

	// Older than the window only when the audio is saved here (media_path, see
	// voice_audio.go): WhatsApp no longer has those notes, this machine does.
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, chat_jid, timestamp, media_path, media_key, media_direct_path, media_url, media_enc_sha256, media_sha256, media_file_length, media_key_timestamp
		FROM messages
		WHERE type IN ('voice', 'audio')
		  AND voice_note_transcript IS NULL
		  AND media_key IS NOT NULL
		  AND (timestamp >= ? OR media_path IS NOT NULL)
		ORDER BY timestamp DESC
		LIMIT ?
	`, cutoff, b.limit*sweepScanFactor)
	if err != nil {
		return 0, fmt.Errorf("query pending: %w", err)
	}

	// Buffer every candidate row and CLOSE the result set before doing any
	// other DB work. The pool runs SetMaxOpenConns(1): while `rows` is open
	// it owns the only connection, so a nested query from inside the loop
	// (chatExcluded's chats lookup) waits forever for a connection this same
	// goroutine holds — a self-deadlock that wedged every read until restart.
	type pendingRow struct {
		id, chatJID                     string
		timestamp                       int64
		mediaKey, mediaEncSha, mediaSha []byte
		directPath, mediaURL, mediaPath sql.NullString
		fileLength, mediaKeyTimestamp   sql.NullInt64
	}
	var pending []pendingRow
	for rows.Next() {
		var p pendingRow
		if err := rows.Scan(&p.id, &p.chatJID, &p.timestamp, &p.mediaPath, &p.mediaKey, &p.directPath, &p.mediaURL, &p.mediaEncSha, &p.mediaSha, &p.fileLength, &p.mediaKeyTimestamp); err != nil {
			log.Printf("backfiller: row scan failed: %v", err)
			continue
		}
		pending = append(pending, p)
	}
	iterErr := rows.Err()
	rows.Close()
	if iterErr != nil {
		return 0, fmt.Errorf("rows iter: %w", iterErr)
	}

	enqueued := 0
	for _, p := range pending {
		if enqueued >= b.limit {
			break
		}
		// Excluded chats: skip quietly here (this sweep re-visits the same
		// NULL-transcript rows every interval; logging the skip each time
		// would flood the log). Enqueue keeps its own guard as safety net.
		if b.transcriber.chatExcluded(p.chatJID) {
			continue
		}

		// A saved copy beats the CDN, and is the only source for a note past the
		// window. Without one, such a note cannot be fetched at all.
		saved := ""
		if p.mediaPath.Valid {
			st, statErr := os.Stat(p.mediaPath.String)
			switch {
			case statErr == nil && !st.IsDir():
				saved = p.mediaPath.String
			case errors.Is(statErr, fs.ErrNotExist):
				// The file was deleted: forget the path, so the row is not
				// selected again by every sweep. ONLY for "not there": a
				// permission error or an unplugged disk says nothing about
				// whether the file still exists. And only while the path is still
				// the one we read, in case it was just saved again.
				if _, err := b.db.ExecContext(ctx, `UPDATE messages SET media_path = NULL WHERE id = ? AND media_path = ?`, p.id, p.mediaPath.String); err != nil {
					log.Printf("backfiller: clearing the missing audio path for %s failed: %v", p.id, err)
				}
			}
		}
		if saved == "" && p.timestamp < cutoff {
			continue
		}

		// Reconstitute a minimal AudioMessage that whatsmeow.Client.Download
		// can consume. URL or DirectPath alone is enough for the CDN fetch;
		// the encryption fields decrypt the response.
		audio := &waE2E.AudioMessage{
			MediaKey:      p.mediaKey,
			FileEncSHA256: p.mediaEncSha,
			FileSHA256:    p.mediaSha,
		}
		if p.directPath.Valid {
			s := p.directPath.String
			audio.DirectPath = &s
		}
		if p.mediaURL.Valid {
			s := p.mediaURL.String
			audio.URL = &s
		}
		if p.fileLength.Valid {
			fl := uint64(p.fileLength.Int64)
			audio.FileLength = &fl
		}
		if p.mediaKeyTimestamp.Valid {
			mkt := p.mediaKeyTimestamp.Int64
			audio.MediaKeyTimestamp = &mkt
		}

		client := b.client
		messageID := p.id
		queued := b.transcriber.Enqueue(transcriptionJob{
			MessageID: messageID,
			ChatJID:   p.chatJID,
			MimeType:  "audio/ogg; codecs=opus",
			AudioDownloader: func(ctx context.Context) ([]byte, error) {
				if saved != "" {
					if data, err := os.ReadFile(saved); err == nil && len(data) > 0 {
						return data, nil
					}
				}
				return client.Download(ctx, audio)
			},
		})
		if queued {
			enqueued++
		}
	}
	swept := len(pending)

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
