package main

import (
	"database/sql"
	"fmt"
	"time"
)

// waPingBudget enforces the shared 10-pings-per-hour cap across notify-adelaida
// and relay-note. Both endpoints call CheckAndRecordPing before sending.
//
// Implementation: sliding-window COUNT(*) over wa_pings WHERE ts > now-3600.
// On success, inserts a row. On refusal, returns false — caller should treat
// the ping as queued (the vault inbox file persists regardless).
type waPingBudget struct {
	db       *sql.DB
	limitPH  int // pings per hour; from cfg.WhatsAppPingsPerHour
}

// CheckAndRecordPing checks the sliding-window budget and, if under the cap,
// records the ping and returns true. Returns false (budget exceeded) without
// inserting if at or over the cap.
func (b *waPingBudget) CheckAndRecordPing(endpoint, urgency string) (bool, error) {
	windowStart := time.Now().Unix() - 3600

	tx, err := b.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin budget tx: %w", err)
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM wa_pings WHERE ts > ? AND delivered = 1`, windowStart,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count wa_pings: %w", err)
	}
	if count >= b.limitPH {
		return false, nil // budget exhausted
	}

	if _, err := tx.Exec(
		`INSERT INTO wa_pings (ts, source_endpoint, urgency, delivered) VALUES (?, ?, ?, 1)`,
		time.Now().Unix(), endpoint, urgency,
	); err != nil {
		return false, fmt.Errorf("insert wa_ping: %w", err)
	}
	return true, tx.Commit()
}
