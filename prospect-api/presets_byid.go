package main

// Phase 3 preset functions (lookup + consume by preset_id).
// Shipped for the pre-warm URL flow. Concierge calls /api/get-preset with the
// presetId from the URL's ?invite= param; calls /api/consume-preset after a
// successful booking originated from a pre-warm URL.

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GetPresetByID fetches an unexpired, unconsumed preset by its preset_id.
// Returns nil if not found, expired, or already consumed (concierge falls
// back to standard recognition flow without leaking that the preset existed).
func GetPresetByID(ctx context.Context, db *sql.DB, presetID string) (*Preset, error) {
	if presetID == "" {
		return nil, errors.New("presetID required")
	}
	now := time.Now().Unix()
	row := db.QueryRowContext(ctx, `
		SELECT preset_id, COALESCE(guest_email, ''), COALESCE(guest_phone, ''), guest_name, context, relationship, created_at, expires_at
		FROM presets
		WHERE preset_id = ? AND expires_at > ? AND consumed_at IS NULL
		LIMIT 1
	`, presetID, now)
	var p Preset
	if err := row.Scan(&p.PresetID, &p.GuestEmail, &p.GuestPhone, &p.GuestName, &p.Context, &p.Relationship, &p.CreatedAt, &p.ExpiresAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// ConsumePresetByID marks a preset consumed (single-use).
// Idempotent: calling on already-consumed preset returns existing consumed_at
// without error. Returns 0 if presetID does not exist.
func ConsumePresetByID(ctx context.Context, db *sql.DB, presetID, consumer string) (int64, error) {
	if presetID == "" {
		return 0, errors.New("presetID required")
	}
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx, `
		UPDATE presets SET consumed_at = ?, consumed_by = ? WHERE preset_id = ? AND consumed_at IS NULL
	`, now, consumer, presetID); err != nil {
		return 0, err
	}
	var consumedAt int64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(consumed_at, 0) FROM presets WHERE preset_id = ?`, presetID).Scan(&consumedAt)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return consumedAt, nil
}
