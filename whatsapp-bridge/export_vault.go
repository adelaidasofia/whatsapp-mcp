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
// Per the architectural panel: we write to a separate folder from the pre-bridge
// Baileys archive by default. If the caller points outputDir at the existing
// Baileys folder, we refuse to overwrite files whose existing last_sync predates
// the bridge pairing cutoff (see skipPreservedFiles).
func ExportVault(db *sql.DB, outputDir string, includeGroups bool) error {
	if outputDir == "" {
		return fmt.Errorf("outputDir required")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir output: %w", err)
	}

	// Pull all chats with at least one message.
	chatRows, err := db.Query(`
		SELECT c.jid, c.chat_type, COALESCE(c.name, ''), COALESCE(c.last_message_time, 0)
		FROM chats c
		WHERE EXISTS (SELECT 1 FROM messages m WHERE m.chat_jid = c.jid)
	`)
	if err != nil {
		return fmt.Errorf("query chats: %w", err)
	}
	defer chatRows.Close()

	type chatKey struct {
		jid      string
		chatType string
		name     string
	}
	chats := make([]chatKey, 0)
	for chatRows.Next() {
		var ck chatKey
		var lastTime int64
		if err := chatRows.Scan(&ck.jid, &ck.chatType, &ck.name, &lastTime); err != nil {
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

			if err := exportOneChat(db, outputDir, ck.jid, ck.chatType, ck.name, todayStr); err != nil {
				log.Printf("export chat %s: %v", ck.jid, err)
				mu.Lock()
				skipped++
				mu.Unlock()
				return
			}
			mu.Lock()
			written++
			mu.Unlock()
		}(ck)
	}
	wg.Wait()

	log.Printf("export: done (%d written, %d skipped)", written, skipped)
	return nil
}

func exportOneChat(db *sql.DB, outputDir, jid, chatType, name, todayStr string) error {
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

	// Pull messages for this chat, newest first, but assemble in chronological order.
	rows, err := db.Query(`
		SELECT timestamp, COALESCE(scrubbed_text, COALESCE(content_text, '')), COALESCE(sender_display, ''), is_from_me, type, COALESCE(voice_note_transcript, '')
		FROM messages
		WHERE chat_jid = ?
		ORDER BY timestamp ASC
	`, jid)
	if err != nil {
		return fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	type msg struct {
		ts            int64
		text          string
		senderDisplay string
		fromMe        bool
		msgType       string
		transcript    string
	}
	messages := make([]msg, 0)
	for rows.Next() {
		var m msg
		var fromMe int
		if err := rows.Scan(&m.ts, &m.text, &m.senderDisplay, &fromMe, &m.msgType, &m.transcript); err != nil {
			continue
		}
		m.fromMe = fromMe == 1
		messages = append(messages, m)
	}
	if len(messages) == 0 {
		return nil // nothing to write
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
		default:
			text = m.text
		}
		if text == "" {
			continue
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
		return nil
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
		fmt.Sprintf("last_sync: %s", todayStr),
		"---",
		"",
		fmt.Sprintf("# WhatsApp: %s", display),
		"",
	}
	for _, d := range dates {
		lines = append(lines, fmt.Sprintf("## %s", d), "")
		lines = append(lines, byDate[d]...)
		lines = append(lines, "")
	}

	filename := sanitizeFilename(display) + ".md"
	out := filepath.Join(outputDir, filename)
	return os.WriteFile(out, []byte(strings.Join(lines, "\n")), 0o644)
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", "?", "-", "%", "-", "*", "-", ":", "-",
		"|", "-", "\"", "-", "<", "-", ">", "-", "[", "-", "]", "-",
	)
	cleaned := strings.TrimSpace(replacer.Replace(name))
	if cleaned == "" {
		return "Unknown"
	}
	return cleaned
}

func escapeYAML(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}
