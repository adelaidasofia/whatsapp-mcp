package main

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

// quietHoursChecker determines whether a given moment falls within the
// configured quiet window and computes the next wake time.
//
// QuietHoursStart / QuietHoursEnd are "HH:MM" strings (24h). The window
// may wrap midnight (e.g. 22:00 → 07:00). Both times are interpreted in
// cfg.TimeZone.
type quietHoursChecker struct {
	start    string // "HH:MM"
	end      string // "HH:MM"
	override bool   // if true, high-urgency ignores quiet hours
	loc      *time.Location
}

func newQuietHoursChecker(cfg *Config) (*quietHoursChecker, error) {
	loc, err := time.LoadLocation(cfg.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", cfg.TimeZone, err)
	}
	return &quietHoursChecker{
		start:    cfg.QuietHoursStart,
		end:      cfg.QuietHoursEnd,
		override: cfg.QuietHoursOverride,
		loc:      loc,
	}, nil
}

// InQuietHours returns true if now is within the quiet window.
func (qh *quietHoursChecker) InQuietHours(now time.Time) bool {
	local := now.In(qh.loc)
	startMin := parseHHMM(qh.start)
	endMin := parseHHMM(qh.end)
	nowMin := local.Hour()*60 + local.Minute()

	if startMin <= endMin {
		// Window does not wrap midnight.
		return nowMin >= startMin && nowMin < endMin
	}
	// Wraps midnight: quiet from start → midnight → end.
	return nowMin >= startMin || nowMin < endMin
}

// NextWakeTime returns the next time the quiet window ends (i.e. the moment
// the ping should be delivered).
func (qh *quietHoursChecker) NextWakeTime(now time.Time) time.Time {
	local := now.In(qh.loc)
	endMin := parseHHMM(qh.end)
	endH := endMin / 60
	endM := endMin % 60

	// Next occurrence of endH:endM.
	candidate := time.Date(local.Year(), local.Month(), local.Day(), endH, endM, 0, 0, qh.loc)
	if !candidate.After(local) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

// ShouldSendNow returns true if the ping should be sent immediately (either
// not in quiet hours, or urgency=high with override enabled).
func (qh *quietHoursChecker) ShouldSendNow(urgency string, now time.Time) bool {
	if !qh.InQuietHours(now) {
		return true
	}
	return urgency == "high" && qh.override
}

// EnqueuePing inserts a row into pending_pings to deliver after deliver_at.
func EnqueuePing(db *sql.DB, message, urgency, deeplink string, deliverAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO pending_pings (ts, deliver_at, message, urgency, deeplink, delivered)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		time.Now().Unix(), deliverAt.Unix(), message, urgency, deeplink,
	)
	if err != nil {
		return fmt.Errorf("enqueue pending ping: %w", err)
	}
	return nil
}

// pendingPingRow is one row fetched from pending_pings.
type pendingPingRow struct {
	ID       int64
	Message  string
	Urgency  string
	Deeplink string
}

// FlushPendingPings delivers all pending pings whose deliver_at has passed.
// Called by the goroutine in main.go that ticks every minute.
func FlushPendingPings(db *sql.DB, deliverFn func(row pendingPingRow) error) {
	now := time.Now().Unix()
	rows, err := db.Query(
		`SELECT id, message, urgency, deeplink FROM pending_pings
		 WHERE delivered = 0 AND deliver_at <= ?`, now,
	)
	if err != nil {
		log.Printf("flush_pending_pings: query: %v", err)
		return
	}
	defer rows.Close()

	var toDeliver []pendingPingRow
	for rows.Next() {
		var row pendingPingRow
		var deeplink sql.NullString
		if err := rows.Scan(&row.ID, &row.Message, &row.Urgency, &deeplink); err != nil {
			log.Printf("flush_pending_pings: scan: %v", err)
			continue
		}
		if deeplink.Valid {
			row.Deeplink = deeplink.String
		}
		toDeliver = append(toDeliver, row)
	}

	for _, row := range toDeliver {
		if err := deliverFn(row); err != nil {
			log.Printf("flush_pending_pings: deliver id=%d: %v", row.ID, err)
			continue
		}
		if _, err := db.Exec(
			`UPDATE pending_pings SET delivered = 1, delivered_at = ? WHERE id = ?`,
			time.Now().Unix(), row.ID,
		); err != nil {
			log.Printf("flush_pending_pings: mark delivered id=%d: %v", row.ID, err)
		}
	}
}

func parseHHMM(s string) int {
	if len(s) != 5 || s[2] != ':' {
		return 0
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	return h*60 + m
}
