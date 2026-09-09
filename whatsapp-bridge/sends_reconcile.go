// sends_reconcile.go — what to do about a send that was interrupted mid-flight.
//
// handleConfirmSend flips a row to 'confirmed' BEFORE calling SendMessage, so a
// double-confirm race is impossible. The cost of that ordering is a window: if
// the process stops between the flip and the write-back, the row stays
// 'confirmed' forever. Nothing else in the codebase ever reads that state —
// handleConfirmSend rejects anything that is not 'draft', and the expiry branch
// is unreachable for it — so the row is invisible, permanent, and the media
// bytes it points at are leaked with it.
//
// That window has three real triggers, in ascending order of how long they went
// unnoticed: the HTTP client hanging up (r.Context() cancelled — this
// deployment's logs show `context canceled` killing other queries), the MCP
// layer timing out, and the process exiting. sends.go now detaches the
// post-send writes with context.WithoutCancel, which closes the first two. This
// file handles what is left: the process stopping mid-send, which no amount of
// care inside the handler can prevent.
//
// The deliberate choice here is NOT to resend. Delivery is genuinely unknown —
// SendMessage may have reached WhatsApp and the recipient may already have the
// message — and a duplicate sent to a real person is worse than a gap in an
// archive. So reconciliation makes the row VISIBLE and actionable ('failed'
// with an explicit reason) and stops there, leaving the decision to a human who
// can look at the actual chat.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// inFlightSendNotice is stored in error_message. Phrased for whoever reads the
// row later: it has to say that delivery is unknown, or the natural reaction is
// to resend and risk duplicating a message that did arrive.
const inFlightSendNotice = "the bridge stopped between sending and recording this; " +
	"DELIVERY IS UNKNOWN — check the chat in WhatsApp before resending"

// ReconcileInFlightSends resolves every send left 'confirmed' by a previous
// run, and frees the outbound bytes it was holding. Returns how many rows it
// touched.
//
// Called once at startup, and only correct there: at that moment no send can
// legitimately be in flight, because the process that could have owned one is
// gone. Running this while the bridge is serving would race a live confirm and
// mark a perfectly good send as failed.
func ReconcileInFlightSends(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT draft_id, recipient_jid, COALESCE(recipient_display, ''),
		       send_type, COALESCE(content_file_path, ''), COALESCE(confirmed_at, 0)
		FROM sends
		WHERE status = 'confirmed'
		ORDER BY confirmed_at
	`)
	if err != nil {
		return 0, fmt.Errorf("ReconcileInFlightSends: query: %w", err)
	}

	type stuck struct {
		draftID, recipient, display, sendType, mediaPath string
		confirmedAt                                      int64
	}
	// Buffered before writing: SQLite does not like an UPDATE on the same table
	// while a SELECT cursor over it is still open.
	var found []stuck
	for rows.Next() {
		var s stuck
		if err := rows.Scan(&s.draftID, &s.recipient, &s.display, &s.sendType, &s.mediaPath, &s.confirmedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("ReconcileInFlightSends: scan: %w", err)
		}
		found = append(found, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("ReconcileInFlightSends: rows: %w", err)
	}
	rows.Close()

	if len(found) == 0 {
		return 0, nil
	}

	for _, s := range found {
		who := s.display
		if who == "" {
			who = s.recipient
		}
		// One WARN line per row, naming the recipient: this is the only place
		// the user is ever told that a message they approved may or may not have
		// arrived, so it must not be summarised away into a count.
		log.Printf("WARNING: send %s to %s (%s, confirmed at %d) was interrupted mid-flight; "+
			"marking it failed WITHOUT resending — delivery is unknown, verify in WhatsApp",
			s.draftID, who, s.sendType, s.confirmedAt)

		if _, err := db.ExecContext(ctx,
			`UPDATE sends SET status='failed', error_message=? WHERE draft_id=? AND status='confirmed'`,
			inFlightSendNotice, s.draftID); err != nil {
			// Keep going: one unwritable row must not leave the rest stuck.
			log.Printf("ReconcileInFlightSends: marking %s failed: %v", s.draftID, err)
			continue
		}
		// Only after the row no longer references them. A crash between the two
		// leaves the file behind, which the next run cannot find — the lesser of
		// the two orders, since deleting first could strand a row pointing at
		// bytes that are gone.
		if err := removeOutboundFile(s.mediaPath); err != nil {
			log.Printf("ReconcileInFlightSends: removing %s for %s: %v", s.mediaPath, s.draftID, err)
		}
	}
	return len(found), nil
}
