package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ExportVault regenerates one markdown file per direct chat from the SQLite DB.
//
// Format matches the Baileys export so downstream vault tooling (graphify,
// the auto-wikilink pipeline, Dataview queries over type: whatsapp-chat)
// continues to work without changes.
//
// Output layout (one file per direct chat; groups are skipped by default):
//
//    <outputDir>/<Contact Name>.md      when push_name is known
//    <outputDir>/+<phone>.md            when only phone is known
//
// YAML frontmatter:
//
//    type: whatsapp-chat
//    contact: "<display>"
//    phone: "+<phone>"
//    jid: "<jid>"
//    message_count: <N>
//    first_message: YYYY-MM-DD
//    last_message: YYYY-MM-DD
//    last_sync: YYYY-MM-DD (date of export)
//
// Body: `## YYYY-MM-DD` section per date, `**HH:MM AM** You: <text>` per message.
//
// Per design conversation 2026-04-24: the existing Baileys folder is the
// canonical chat-history write target. The bridge regenerates per-chat MDs
// from SQLite into that folder. minMessages filters out drive-by contacts
// (set to 0 for "everyone with at least one message", or 5 for the user's
// preferred volume threshold).
func ExportVault(db *sql.DB, outputDir string, includeGroups bool, minMessages int) error {
	if outputDir == "" {
		return fmt.Errorf("outputDir required")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir output: %w", err)
	}
	if minMessages < 0 {
		minMessages = 0
	}

	// Pull all chats whose message count meets the threshold.
	// participants_count is nullable (NULL for direct chats); COALESCE keeps
	// the Scan target a non-nullable int. The frontmatter writer below only
	// emits the field for group chats so direct files stay clean.
	chatRows, err := db.Query(`
		SELECT c.jid, c.chat_type, COALESCE(c.name, ''), COALESCE(c.last_message_time, 0),
		       COALESCE(c.participants_count, 0),
		       (SELECT COUNT(*) FROM messages m WHERE m.chat_jid = c.jid) AS msg_count
		FROM chats c
		WHERE (SELECT COUNT(*) FROM messages m WHERE m.chat_jid = c.jid) >= ?
	`, minMessages)
	if err != nil {
		return fmt.Errorf("query chats: %w", err)
	}
	defer chatRows.Close()

	type chatKey struct {
		jid               string
		chatType          string
		name              string
		participantsCount int
		lastMessageTs     int64
	}
	chats := make([]chatKey, 0)
	for chatRows.Next() {
		var ck chatKey
		var msgCount int
		if err := chatRows.Scan(&ck.jid, &ck.chatType, &ck.name, &ck.lastMessageTs, &ck.participantsCount, &msgCount); err != nil {
			continue
		}
		if !includeGroups && ck.chatType != "direct" {
			continue
		}
		chats = append(chats, ck)
	}

	log.Printf("export: writing %d chats to %s", len(chats), outputDir)

	written := 0
	skipped := 0
	skippedUnchanged := 0
	todayStr := time.Now().Format("2006-01-02")

	// Small bounded concurrency to hide I/O latency when the output folder is on iCloud.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex

	for _, ck := range chats {
		wg.Add(1)
		go func(ck chatKey) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result, err := exportOneChat(db, outputDir, ck.jid, ck.chatType, ck.name, ck.participantsCount, ck.lastMessageTs, todayStr)
			if err != nil {
				log.Printf("export chat %s: %v", ck.jid, err)
				mu.Lock()
				skipped++
				mu.Unlock()
				return
			}
			mu.Lock()
			switch result {
			case writeResultWrote:
				written++
			case writeResultUnchanged:
				skippedUnchanged++
			case writeResultEmpty:
				skipped++
			}
			mu.Unlock()
		}(ck)
	}
	wg.Wait()

	log.Printf("export: done (%d written, %d skipped-unchanged, %d skipped-empty)",
		written, skippedUnchanged, skipped)
	return nil
}

// writeResult enumerates the outcomes of exportOneChat so callers can
// distinguish "skipped because nothing changed since last export" from
// "skipped because the chat had zero messages."
type writeResult int

const (
	writeResultWrote writeResult = iota
	writeResultUnchanged
	writeResultEmpty
)

func exportOneChat(db *sql.DB, outputDir, jid, chatType, name string, participantsCount int, lastMessageTs int64, todayStr string) (writeResult, error) {
	// Resolve display name: prefer chats.name, else contacts.push_name, else phone.
	display := name
	var phone string
	err := db.QueryRow(`SELECT COALESCE(push_name, ''), COALESCE(phone, '') FROM contacts WHERE jid = ?`, jid).
		Scan(&display, &phone)
	if err == sql.ErrNoRows {
		display = name
	}
	if display == "" || strings.HasPrefix(display, "+") {
		if phone != "" {
			display = "+" + phone
		} else {
			display = extractPhone(jid)
			if display != "" {
				display = "+" + display
			} else {
				display = jid
			}
		}
	}
	if phone == "" {
		phone = extractPhone(jid)
	}

	// Unchanged-skip: if the existing chat file already covers up to lastMessageTs,
	// don't re-pull messages and re-write the file. Mirrors iMessage's _is_unchanged
	// behavior. Saves significant I/O on iCloud-synced vaults and avoids needless
	// extractor re-tagging cycles. The check works only after files have been
	// written once with the new last_message_ts field; on first run after the
	// migration, all files rewrite (existing ts = 0 < incoming).
	filename := sanitizeFilename(display) + ".md"
	out := filepath.Join(outputDir, filename)
	if lastMessageTs > 0 {
		if existingTs := readExistingLastMessageTs(out); existingTs >= lastMessageTs {
			return writeResultUnchanged, nil
		}
	}

	// Pull messages for this chat, newest first, but assemble in chronological order.
	rows, err := db.Query(`
		SELECT timestamp, COALESCE(scrubbed_text, COALESCE(content_text, '')), COALESCE(sender_display, ''), is_from_me, type, COALESCE(voice_note_transcript, ''), COALESCE(raw_type, '')
		FROM messages
		WHERE chat_jid = ?
		ORDER BY timestamp ASC
	`, jid)
	if err != nil {
		return writeResultEmpty, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	type msg struct {
		ts            int64
		text          string
		senderDisplay string
		fromMe        bool
		msgType       string
		transcript    string
		rawType       string
	}
	messages := make([]msg, 0)
	for rows.Next() {
		var m msg
		var fromMe int
		if err := rows.Scan(&m.ts, &m.text, &m.senderDisplay, &fromMe, &m.msgType, &m.transcript, &m.rawType); err != nil {
			continue
		}
		m.fromMe = fromMe == 1
		messages = append(messages, m)
	}
	if len(messages) == 0 {
		return writeResultEmpty, nil // nothing to write
	}

	byDate := map[string][]string{}
	for _, m := range messages {
		if m.ts < 1000 {
			continue
		}
		tm := time.Unix(m.ts, 0)
		date := tm.Format("2006-01-02")

		speaker := "You"
		if !m.fromMe {
			if m.senderDisplay != "" {
				speaker = m.senderDisplay
			} else {
				speaker = display
			}
		}

		var text string
		switch m.msgType {
		case "voice", "audio":
			if m.transcript != "" {
				if m.msgType == "voice" {
					text = fmt.Sprintf("[Voice note] %s", m.transcript)
				} else {
					text = fmt.Sprintf("[Audio] %s", m.transcript)
				}
			} else {
				if m.msgType == "voice" {
					text = "[Voice note]"
				} else {
					text = "[Audio]"
				}
			}
		case "image":
			if m.text != "" {
				text = fmt.Sprintf("[Image: %s]", m.text)
			} else {
				text = "[Image]"
			}
		case "video":
			if m.text != "" {
				text = fmt.Sprintf("[Video: %s]", m.text)
			} else {
				text = "[Video]"
			}
		case "document":
			text = "[Document] " + m.text
		case "sticker":
			text = "[Sticker]"
		case "location":
			if m.text != "" {
				text = fmt.Sprintf("[Location: %s]", m.text)
			} else {
				text = "[Location]"
			}
		case "contact":
			if m.text != "" {
				text = fmt.Sprintf("[Contact: %s]", m.text)
			} else {
				text = "[Contact]"
			}
		case "reaction":
			text = fmt.Sprintf("[Reaction: %s]", m.text)
		case "poll":
			if m.text != "" {
				text = fmt.Sprintf("[Poll] %s", m.text)
			} else {
				text = "[Poll]"
			}
		case "event":
			if m.text != "" {
				text = fmt.Sprintf("[Event] %s", m.text)
			} else {
				text = "[Event]"
			}
		case "unsupported":
			// A message the bridge could not read. It MUST still appear, with
			// enough detail to go find it in WhatsApp by hand: this line is the
			// only trace that something was said here at all. Rendering nothing
			// (the old behavior, via the blanket empty-text skip below) is what
			// made 41% of the store vanish from the vault silently — the export
			// looked healthy and the counts looked right.
			if m.rawType != "" {
				text = fmt.Sprintf("[Unsupported message — not captured (type: %s)]", m.rawType)
			} else {
				text = "[Unsupported message — not captured]"
			}
		default:
			text = m.text
		}
		// Only genuinely-contentless rows are skipped, and only when the
		// decoder positively classified them as such (a `system` protocol row
		// like a revoke or a key exchange). Anything else with empty text is a
		// decode gap, and gets a visible placeholder rather than silence.
		if text == "" {
			if m.msgType != "system" {
				text = fmt.Sprintf("[Empty %s message — not captured (type: %s)]",
					m.msgType, orUnknown(m.rawType))
			} else {
				continue
			}
		}
		timeStr := tm.Format("03:04 PM")
		line := fmt.Sprintf("**%s** %s: %s", timeStr, speaker, text)
		byDate[date] = append(byDate[date], line)
	}
	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	if len(dates) == 0 {
		return writeResultEmpty, nil
	}

	lines := []string{
		"---",
		"type: whatsapp-chat",
		fmt.Sprintf(`contact: "%s"`, escapeYAML(display)),
		fmt.Sprintf(`phone: "+%s"`, phone),
		fmt.Sprintf(`jid: "%s"`, jid),
		fmt.Sprintf(`chat_type: "%s"`, chatType),
		fmt.Sprintf(`message_count: %d`, len(messages)),
		fmt.Sprintf("first_message: %s", dates[0]),
		fmt.Sprintf("last_message: %s", dates[len(dates)-1]),
		fmt.Sprintf("last_message_ts: %d", lastMessageTs),
		fmt.Sprintf("last_sync: %s", todayStr),
	}
	// Only emit participants_count for groups; direct chats stay clean.
	// Downstream tooling can use this to enforce per-vault group-size policies
	// (e.g. only auto-ingest groups with fewer than N participants).
	if chatType == "group" {
		lines = append(lines, fmt.Sprintf("participants_count: %d", participantsCount))
	}

	// Preserve extractor-added frontmatter from the existing file. Without
	// this, every export nukes vault-metadata-extract's whatsapp_* fields and
	// any other downstream-added keys (word_count etc.). Path was already
	// computed at the top of this function for the unchanged-skip check.
	if extras := extractNonCanonicalFrontmatter(out); len(extras) > 0 {
		lines = append(lines, extras...)
	}

	lines = append(lines,
		"---",
		"",
		fmt.Sprintf("# WhatsApp: %s", display),
		"",
	)
	for _, d := range dates {
		lines = append(lines, fmt.Sprintf("## %s", d), "")
		lines = append(lines, byDate[d]...)
		lines = append(lines, "")
	}

	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return writeResultEmpty, err
	}
	return writeResultWrote, nil
}

// canonicalFrontmatterKeys lists the keys this exporter owns. Anything else
// in an existing chat file's frontmatter is treated as user/extractor data
// and preserved across re-exports.
var canonicalFrontmatterKeys = map[string]bool{
	"type":               true,
	"contact":            true,
	"phone":              true,
	"jid":                true,
	"chat_type":          true,
	"message_count":      true,
	"first_message":      true,
	"last_message":       true,
	"last_message_ts":    true,
	"last_sync":          true,
	"participants_count": true,
}

// readExistingLastMessageTs returns the int64 last_message_ts from an existing
// chat file, or 0 if the file is missing, has no frontmatter, or the field
// isn't present. Used by the unchanged-skip optimization.
func readExistingLastMessageTs(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return 0
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return 0
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.HasPrefix(line, "last_message_ts:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "last_message_ts:"))
			var ts int64
			if _, err := fmt.Sscanf(val, "%d", &ts); err == nil {
				return ts
			}
			return 0
		}
	}
	return 0
}

// extractNonCanonicalFrontmatter reads an existing chat file (if present)
// and returns the non-canonical frontmatter lines verbatim (preserving
// whatever extractors or downstream tools wrote). Returns empty slice if
// the file is missing, has no frontmatter, or the format is unexpected.
//
// Format assumption: the bridge writes single-line "key: value" frontmatter
// only — no multi-line YAML continuations. Any indented continuation lines
// are skipped for safety. Inline JSON arrays (`key: ["a", "b"]`) are kept
// since they fit on one line.
func extractNonCanonicalFrontmatter(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return nil
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil
	}
	fm := rest[:end]

	var extras []string
	for _, line := range strings.Split(fm, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Skip indented continuation lines. The bridge never writes them
		// and our extractor pipeline emits inline JSON arrays for lists.
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if canonicalFrontmatterKeys[key] {
			continue
		}
		extras = append(extras, line)
	}
	return extras
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", "?", "-", "%", "-", "*", "-", ":", "-",
		"|", "-", "\"", "-", "<", "-", ">", "-", "[", "-", "]", "-",
	)
	cleaned := strings.TrimSpace(replacer.Replace(name))
	// Windows refuses filenames ending in dots or spaces, and treats
	// reserved device stems (CON, NUL, COM1…) as devices, not files.
	cleaned = strings.TrimRight(cleaned, ". ")
	if isWindowsReservedName(cleaned) {
		cleaned += "_"
	}
	if cleaned == "" {
		return "Unknown"
	}
	return cleaned
}

func isWindowsReservedName(s string) bool {
	up := strings.ToUpper(s)
	switch up {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(up) == 4 && (strings.HasPrefix(up, "COM") || strings.HasPrefix(up, "LPT")) {
		c := up[3]
		return c >= '1' && c <= '9'
	}
	return false
}

func escapeYAML(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

// orUnknown labels a missing raw_type. Rows written before migration 004 have
// no decode path recorded; saying so is better than printing an empty pair of
// parentheses that reads like the field was checked and found blank.
func orUnknown(s string) string {
	if s == "" {
		return "unrecorded, pre-004 row"
	}
	return s
}
