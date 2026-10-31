package main

import (
	"context"
	"log"
	"time"
)

// WhatsApp serves a note's audio for about 14 days. With transcription off, the
// media keys stored at receive time cannot bring a note back after that, so
// "turn it on later" would only ever cover the last two weeks. Saving the audio
// (a voice note is a few tens of KB) lets the backfill sweep transcribe notes of
// any age once transcription is on.
//
// Saving is deliberately narrow, because these files are not encrypted like the
// database. See keepsVoiceAudio.

// keepSlots bounds how many notes are being saved at once, so a burst (a
// reconnect that delivers a day of notes) cannot open that many downloads.
var keepSlots = make(chan struct{}, 4)

// keepsVoiceAudio reports whether an incoming note's audio is saved for later.
//   - Only while transcription is off. Once it is on, a note is read straight
//     away and nothing needs saving for later.
//   - Only voice notes, not forwarded music or long recordings.
//   - Never a view-once or disappearing note: its sender asked for it not to
//     stick around.
//   - Never a chat the member keeps out of transcription.
func (b *Bridge) keepsVoiceAudio(msgType, chatJID string, viewOnce, ephemeral bool) bool {
	return b.cfg.KeepVoiceAudio &&
		b.cfg.WhisperBackend == "off" &&
		msgType == "voice" &&
		!viewOnce && !ephemeral &&
		!b.transcriber.chatExcluded(chatJID)
}

// keepVoiceAudio saves one note's audio. DownloadMedia fetches it once, writes
// the file (0600, under MediaPath) and records media_path; the backfill sweep
// reads that file instead of asking WhatsApp for the note again.
//
// It runs apart from the transcription queue on purpose: saving must not depend
// on a validation step, a free queue slot or a transcription that never starts.
func (b *Bridge) keepVoiceAudio(id string) {
	keepSlots <- struct{}{}
	defer func() { <-keepSlots }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, _, _, err := b.DownloadMedia(ctx, id); err != nil {
		log.Printf("voice note %s: saving the audio for later failed: %v", id, err)
	}
}
