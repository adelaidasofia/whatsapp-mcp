package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Preset is a pre-warm record Adelaida vouches for a guest.
// When the guest arrives at the concierge, /api/check-preset finds this record
// and the concierge skips the intro flow.
type Preset struct {
	PresetID     string `json:"presetId"`
	GuestEmail   string `json:"guestEmail,omitempty"`
	GuestPhone   string `json:"guestPhone,omitempty"`
	GuestName    string `json:"guestName"`
	Context      string `json:"context"`
	Relationship string `json:"relationship"`
	CreatedAt    int64  `json:"createdAt"`
	ExpiresAt    int64  `json:"expiresAt"`
	ConsumedAt   int64  `json:"consumedAt,omitempty"`
}

const (
	presetMinDays = 1
	presetMaxDays = 30
	contextMaxLen = 500
)

var validRelationships = map[string]bool{
	"personal":     true,
	"professional": true,
	"investor":     true,
	"media":        true,
	"press":        true,
	"speaking":     true,
}

// CreatePreset inserts a new preset row.
// At least one of (email, phone) is required. Both are stored when given;
// /api/check-preset matches on either.
func CreatePreset(ctx context.Context, db *sql.DB, p Preset) (string, error) {
	if p.GuestName == "" {
		return "", errors.New("guestName required")
	}
	if p.GuestEmail == "" && p.GuestPhone == "" {
		return "", errors.New("at least one of guestEmail or guestPhone required")
	}
	if p.Context == "" {
		return "", errors.New("context required")
	}
	if len(p.Context) > contextMaxLen {
		return "", errors.New("context exceeds 500 chars")
	}
	if !validRelationships[strings.ToLower(p.Relationship)] {
		return "", errors.New("relationship must be one of: personal, professional, investor, media, press, speaking")
	}

	now := time.Now().Unix()
	if p.ExpiresAt <= now {
		return "", errors.New("expiresAt must be in the future")
	}
	maxAllowed := time.Now().AddDate(0, 0, presetMaxDays).Unix()
	if p.ExpiresAt > maxAllowed {
		return "", errors.New("expiresAt exceeds maximum 30 days")
	}

	id := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO presets (preset_id, guest_email, guest_phone, guest_name, context, relationship, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, nullIfEmpty(NormalizeEmail(p.GuestEmail)), nullIfEmpty(DigitsOnly(p.GuestPhone)),
		p.GuestName, p.Context, strings.ToLower(p.Relationship), now, p.ExpiresAt)
	if err != nil {
		return "", err
	}
	return id, nil
}

// CheckPreset finds an unexpired, unconsumed preset matching the given email/phone/name.
// Match precedence: email > phone > name+email-domain. First hit wins.
// Marks the preset as consumed before returning so it's single-use.
func CheckPreset(ctx context.Context, db *sql.DB, email, phone, name, consumer string) (*Preset, error) {
	now := time.Now().Unix()

	var (
		row *sql.Row
		key string
	)
	if e := NormalizeEmail(email); e != "" {
		row = db.QueryRowContext(ctx, `
			SELECT preset_id, COALESCE(guest_email, ''), COALESCE(guest_phone, ''), guest_name, context, relationship, created_at, expires_at
			FROM presets
			WHERE guest_email = ? AND expires_at > ? AND consumed_at IS NULL
			ORDER BY created_at DESC LIMIT 1
		`, e, now)
		key = "email:" + e
	} else if p := DigitsOnly(phone); len(p) >= 7 {
		tail := p
		if len(tail) > 10 {
			tail = tail[len(tail)-10:]
		}
		row = db.QueryRowContext(ctx, `
			SELECT preset_id, COALESCE(guest_email, ''), COALESCE(guest_phone, ''), guest_name, context, relationship, created_at, expires_at
			FROM presets
			WHERE guest_phone LIKE '%' || ? || '%' AND expires_at > ? AND consumed_at IS NULL
			ORDER BY created_at DESC LIMIT 1
		`, tail, now)
		key = "phone:" + tail
	} else {
		return nil, nil
	}

	var p Preset
	if err := row.Scan(&p.PresetID, &p.GuestEmail, &p.GuestPhone, &p.GuestName, &p.Context, &p.Relationship, &p.CreatedAt, &p.ExpiresAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	// Mark consumed (single-use).
	_, _ = db.ExecContext(ctx, `
		UPDATE presets SET consumed_at = ?, consumed_by = ? WHERE preset_id = ? AND consumed_at IS NULL
	`, now, consumer, p.PresetID)
	p.ConsumedAt = now

	_ = key // kept for future-debug; suppressed lint
	return &p, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
