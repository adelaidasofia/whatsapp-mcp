package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// iMessageContact is one 1:1 iMessage handle (phone or email) with stats.
type iMessageContact struct {
	Handle    string // raw handle from chat.db (phone like "+15555551234" or email)
	Service   string // "iMessage" | "SMS"
	Phone     string // E.164 with leading +; "" if handle is an email
	IsEmail   bool
	MsgCount  int
	LastTSSec int64 // unix seconds
}

// LoadIMessage1to1Contacts opens the macOS Messages chat.db and returns all
// 1:1 chats (no group chats, no broadcasts) along with message counts and
// last-interaction timestamps.
//
// Permission: chat.db is protected by Full Disk Access (TCC). If the parent
// process lacks FDA, db.Open returns "authorization denied" — we surface that
// as a typed error so the caller can decide to skip iMessage gracefully
// instead of failing the whole subcommand.
//
// Apple stores message.date as nanoseconds since 2001-01-01 (Apple absolute
// time). We convert to unix seconds: unix = appleNs / 1e9 + 978307200.
func LoadIMessage1to1Contacts(ctx context.Context) ([]iMessageContact, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	dbPath := filepath.Join(home, "Library", "Messages", "chat.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("imessage db not found: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open imessage db: %w", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		// TCC denial surfaces as "authorization denied" or similar.
		if strings.Contains(err.Error(), "authorization denied") {
			return nil, ErrIMessagePermissionDenied
		}
		return nil, fmt.Errorf("ping imessage db: %w", err)
	}

	// chat.style: 45 = 1:1 direct chat, 43 = group chat.
	// chat.service_name: "iMessage" or "SMS".
	// chat_handle_join.handle_id → handle.ROWID (the other party).
	// message.date is Apple absolute time in NANOSECONDS (macOS 10.13+) or seconds (older);
	// we coerce both to unix seconds in Go after the query.
	rows, err := db.QueryContext(ctx, `
		SELECT
			h.id            AS handle,
			h.service       AS service,
			COUNT(m.ROWID)  AS msg_count,
			MAX(m.date)     AS last_apple_date
		FROM chat c
		JOIN chat_handle_join chj ON chj.chat_id = c.ROWID
		JOIN handle h             ON h.ROWID    = chj.handle_id
		JOIN chat_message_join cmj ON cmj.chat_id = c.ROWID
		JOIN message m            ON m.ROWID    = cmj.message_id
		WHERE c.style = 45
		GROUP BY h.id
		ORDER BY MAX(m.date) DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query imessage 1:1 chats: %w", err)
	}
	defer rows.Close()

	var contacts []iMessageContact
	for rows.Next() {
		var c iMessageContact
		var lastApple sql.NullInt64
		if err := rows.Scan(&c.Handle, &c.Service, &c.MsgCount, &lastApple); err != nil {
			continue
		}
		c.LastTSSec = appleDateToUnixSec(lastApple)
		c.IsEmail = strings.Contains(c.Handle, "@")
		if !c.IsEmail {
			c.Phone = normalizeIMessageHandle(c.Handle)
		}
		contacts = append(contacts, c)
	}
	return contacts, nil
}

// ErrIMessagePermissionDenied indicates the parent process lacks Full Disk
// Access; callers should skip iMessage processing and warn the user.
var ErrIMessagePermissionDenied = errors.New("imessage chat.db requires Full Disk Access (System Settings → Privacy & Security → Full Disk Access)")

// appleDateToUnixSec converts Apple absolute time to Unix seconds.
// macOS 10.13+ uses nanoseconds since 2001-01-01; older versions used seconds.
// We detect by magnitude: anything > 10^11 is nanoseconds.
func appleDateToUnixSec(v sql.NullInt64) int64 {
	if !v.Valid || v.Int64 == 0 {
		return 0
	}
	const epoch2001 = 978307200
	apple := v.Int64
	if apple > 100_000_000_000 { // > ~3 years in seconds → nanoseconds
		apple = apple / 1_000_000_000
	}
	return apple + epoch2001
}

// normalizeIMessageHandle returns an E.164-style phone with leading + for a
// numeric handle. iMessage already stores phones in E.164 format, but we
// defend against missing + and stray characters.
func normalizeIMessageHandle(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	digits := DigitsOnly(h)
	if len(digits) < 7 {
		return ""
	}
	return "+" + digits
}
