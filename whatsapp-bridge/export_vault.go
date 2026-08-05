package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ExportVault regenerates one markdown file per chat from the SQLite DB.
//
// Format matches the Baileys export so downstream vault tooling (graphify,
// the auto-wikilink pipeline, Dataview queries over type: whatsapp-chat)
// continues to work without changes.
//
// Output layout (one file per HUMAN or group; groups are skipped by default):
//
//	<outputDir>/<Contact Name>.md            direct chat, push_name known
//	<outputDir>/+<phone>.md                  direct chat, only phone known
//	<outputDir>/<Group Name> (group).md      group chat (never a bare person name)
//
// YAML frontmatter:
//
//	type: whatsapp-chat
//	contact: "<display>"
//	phone: "+<phone>"            (only when a real phone is known; never a LID)
//	jid: "<primary jid>"
//	alias_jids: ["<jid>", ...]   (other JID forms of the same human, when known)
//	chat_type: "<direct|group|broadcast|community>"
//	message_count: <N>           (total across all merged JID forms)
//	first_message: YYYY-MM-DD
//	last_message: YYYY-MM-DD
//	last_message_ts: <unix>
//	last_sync: YYYY-MM-DD (date of export)
//	participants_count: <N>      (groups only; stored count or distinct-sender floor)
//
// Body: `## YYYY-MM-DD` section per date, `**HH:MM AM** You: <text>` per message.
//
// MYC-3555 invariants (the export used to drop entire direct chats while
// exiting 0 — see export_vault_loss_test.go for the measured failure shape):
//
//  1. One human = one file. The LID and phone-number JID forms of the same
//     person (jid_aliases) merge into a single file holding BOTH histories.
//  2. A group can never occupy a person-named file: non-direct chats always
//     carry their chat_type in the filename.
//  3. The unchanged-skip only fires when the existing file provably belongs to
//     this chat (its frontmatter jid set matches); a file another chat wrote
//     at the same path never suppresses an export again.
//  4. The run FAILS (non-nil error → non-zero exit) when any enumerated chat
//     produced neither a file nor a legitimate skip, and the error names the
//     dropped chats. A reconciliation line is logged so scripts can assert
//     counts. rc=0 means complete.
//
// Per design conversation 2026-04-24: the existing Baileys folder is the
// canonical chat-history write target. The bridge regenerates per-chat MDs
// from SQLite into that folder. minMessages filters out drive-by contacts
// (set to 0 for "everyone with at least one message", or 5 for the user's
// preferred volume threshold) and counts messages ACROSS a person's merged
// JID forms, so a split history cannot fall under the threshold on both rows.
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

	units, totalChats, err := buildExportUnits(db, includeGroups, minMessages)
	if err != nil {
		return err
	}

	// Heal files left by the pre-MYC-3555 naming (a group at a bare person
	// name, a chat filed under its raw LID digits) before the write pass, so
	// the old file's timestamp cannot shadow this run and the vault does not
	// accumulate duplicates for the same chat.
	healLegacyFilenames(outputDir, units)

	log.Printf("export: writing %d chats (%d chat rows in db) to %s", len(units), totalChats, outputDir)

	todayStr := time.Now().Format("2006-01-02")

	// Small bounded concurrency to hide I/O latency when the output folder is
	// on iCloud. Results land in per-unit slots, so no aggregation lock.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	results := make([]writeResult, len(units))
	errs := make([]error, len(units))

	for i := range units {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], errs[i] = exportOneUnit(db, outputDir, units[i], todayStr)
		}(i)
	}
	wg.Wait()

	// Reconciliation: every unit must have produced a file or a legitimate,
	// named skip. Anything else is a dropped chat and fails the run.
	written := 0
	skippedUnchanged := 0
	skippedEmpty := 0
	var dropped []string
	for i, u := range units {
		if errs[i] != nil {
			dropped = append(dropped, fmt.Sprintf("%s (→ %s): %v", u.primary, u.filename, errs[i]))
			continue
		}
		switch results[i] {
		case writeResultWrote, writeResultUnchanged:
			// The file must actually exist on disk; a claim without a file is
			// exactly the silent no-op this ticket exists to kill.
			if _, statErr := os.Stat(filepath.Join(outputDir, u.filename)); statErr != nil {
				dropped = append(dropped, fmt.Sprintf("%s (→ %s): result recorded but file missing: %v", u.primary, u.filename, statErr))
				continue
			}
			if results[i] == writeResultWrote {
				written++
			} else {
				skippedUnchanged++
			}
		case writeResultEmpty:
			skippedEmpty++
		}
	}

	log.Printf("export: done (%d written, %d skipped-unchanged, %d skipped-empty)",
		written, skippedUnchanged, skippedEmpty)
	log.Printf("export: reconcile chats_in_db=%d exported_units=%d written=%d skipped_unchanged=%d skipped_empty=%d dropped=%d",
		totalChats, len(units), written, skippedUnchanged, skippedEmpty, len(dropped))

	if len(dropped) > 0 {
		sort.Strings(dropped)
		shown := dropped
		const maxShown = 10
		suffix := ""
		if len(shown) > maxShown {
			suffix = fmt.Sprintf("; +%d more", len(shown)-maxShown)
			shown = shown[:maxShown]
		}
		return fmt.Errorf("export incomplete: %d chat(s) dropped: %s%s", len(dropped), strings.Join(shown, "; "), suffix)
	}
	return nil
}

// writeResult enumerates the outcomes of exportOneUnit so callers can
// distinguish "skipped because nothing changed since last export" from
// "skipped because the chat had zero messages."
type writeResult int

const (
	writeResultWrote writeResult = iota
	writeResultUnchanged
	writeResultEmpty
)

// exportUnit is one output file's worth of chat history: a single group, or a
// human with every JID form that is known to be the same person merged in.
type exportUnit struct {
	members           []string // chat JIDs whose messages land in this file; primary first
	aliasJIDs         []string // full alias closure minus primary, sorted (frontmatter alias_jids)
	primary           string   // JID written to frontmatter `jid:`
	chatType          string
	display           string
	phone             string // real phone digits, or "" when none is known (never a LID)
	participantsCount int    // stored chats.participants_count (groups)
	lastMessageTs     int64  // max across members
	msgCount          int    // total message rows across members
	filename          string // final basename, ".md" included
	forceRewrite      bool   // set when the file was healed/renamed and must be regenerated
}

// buildExportUnits enumerates every chat row, merges direct chats that
// jid_aliases links to the same human, resolves display names, and assigns
// collision-free filenames. Shared by ExportVault and ReconcileVault so the
// two can never disagree about what a complete export looks like.
//
// Returns the eligible units (post includeGroups/minMessages filter) and the
// total number of chat rows in the DB (for the reconciliation line).
func buildExportUnits(db *sql.DB, includeGroups bool, minMessages int) ([]exportUnit, int, error) {
	chatRows, err := db.Query(`
		SELECT jid, chat_type, COALESCE(name, ''), COALESCE(last_message_time, 0), COALESCE(participants_count, 0)
		FROM chats
	`)
	if err != nil {
		return nil, 0, fmt.Errorf("query chats: %w", err)
	}
	chats := map[string]chatRowLite{}
	for chatRows.Next() {
		var c chatRowLite
		if err := chatRows.Scan(&c.jid, &c.chatType, &c.name, &c.lastMessageTs, &c.participantsCount); err != nil {
			continue
		}
		chats[c.jid] = c
	}
	chatRows.Close()

	counts := map[string]int{}
	countRows, err := db.Query(`SELECT chat_jid, COUNT(*) FROM messages GROUP BY chat_jid`)
	if err != nil {
		return nil, 0, fmt.Errorf("query message counts: %w", err)
	}
	for countRows.Next() {
		var jid string
		var n int
		if err := countRows.Scan(&jid, &n); err != nil {
			continue
		}
		counts[jid] = n
	}
	countRows.Close()

	// Union-find over alias edges. An edge only merges DIRECT chats: any edge
	// touching a group/broadcast chat row is corrupt data and is ignored.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for jid := range chats {
		find(jid)
	}
	edgeRows, err := db.Query(`SELECT jid_a, jid_b FROM jid_aliases`)
	if err != nil {
		return nil, 0, fmt.Errorf("query jid_aliases: %w", err)
	}
	for edgeRows.Next() {
		var a, b string
		if err := edgeRows.Scan(&a, &b); err != nil {
			continue
		}
		if c, ok := chats[a]; ok && c.chatType != "direct" {
			continue
		}
		if c, ok := chats[b]; ok && c.chatType != "direct" {
			continue
		}
		union(a, b)
	}
	edgeRows.Close()

	// Components → units.
	components := map[string][]string{}
	for jid := range parent {
		root := find(jid)
		components[root] = append(components[root], jid)
	}

	var units []exportUnit
	for _, closure := range components {
		sort.Strings(closure)
		var members []string
		for _, jid := range closure {
			if _, ok := chats[jid]; ok {
				members = append(members, jid)
			}
		}
		if len(members) == 0 {
			continue // alias edges to JIDs that never got a chat row
		}
		if len(members) == 1 && chats[members[0]].chatType != "direct" {
			c := chats[members[0]]
			units = append(units, exportUnit{
				members:           []string{c.jid},
				primary:           c.jid,
				chatType:          c.chatType,
				participantsCount: c.participantsCount,
				lastMessageTs:     c.lastMessageTs,
				msgCount:          counts[c.jid],
			})
			continue
		}

		// Direct unit (possibly merged). Primary = most recent activity;
		// ties break on lexical order so the choice is deterministic.
		u := exportUnit{chatType: "direct"}
		for _, jid := range members {
			c := chats[jid]
			u.members = append(u.members, jid)
			u.msgCount += counts[jid]
			if c.lastMessageTs > u.lastMessageTs {
				u.lastMessageTs = c.lastMessageTs
			}
			if u.primary == "" || c.lastMessageTs > chats[u.primary].lastMessageTs ||
				(c.lastMessageTs == chats[u.primary].lastMessageTs && jid < u.primary) {
				u.primary = jid
			}
		}
		// Primary first in members so message queries and display resolution
		// prefer the freshest row.
		sort.Slice(u.members, func(i, j int) bool {
			if u.members[i] == u.primary {
				return true
			}
			if u.members[j] == u.primary {
				return false
			}
			return u.members[i] < u.members[j]
		})
		for _, jid := range closure {
			if jid != u.primary {
				u.aliasJIDs = append(u.aliasJIDs, jid)
			}
		}
		sort.Strings(u.aliasJIDs)
		units = append(units, u)
	}

	// Eligibility filter — mirrors the pre-existing flags, but counts a
	// human's messages across every merged JID form.
	filtered := units[:0]
	for _, u := range units {
		if !includeGroups && u.chatType != "direct" {
			continue
		}
		if u.msgCount < minMessages {
			continue
		}
		filtered = append(filtered, u)
	}
	units = filtered

	if err := resolveUnitIdentities(db, units, chats); err != nil {
		return nil, 0, err
	}
	assignFilenames(units)

	sort.Slice(units, func(i, j int) bool { return units[i].filename < units[j].filename })
	return units, len(chats), nil
}

// resolveUnitIdentities fills display + phone for every unit.
//
// Display precedence for direct chats: contacts.push_name (primary JID first),
// then chats.name, then "+<phone>", then the primary JID's digits. Groups use
// chats.name only — a group must never inherit a person's contact card.
//
// Phone rule: a phone is only a phone when it comes from an @s.whatsapp.net
// JID's user portion or a contacts.phone column that is NOT the digits of a
// LID (the legacy ingestion bug stored LIDs there). LID digits rendered as
// `phone:` would be fabricated data.
func resolveUnitIdentities(db *sql.DB, units []exportUnit, chats map[string]chatRowLite) error {
	// Bulk-load the contact rows for every JID any unit touches.
	var allJIDs []string
	for _, u := range units {
		allJIDs = append(allJIDs, u.members...)
		allJIDs = append(allJIDs, u.aliasJIDs...)
	}
	type contactLite struct {
		pushName string
		phone    string
	}
	contacts := map[string]contactLite{}
	for start := 0; start < len(allJIDs); start += 500 {
		end := start + 500
		if end > len(allJIDs) {
			end = len(allJIDs)
		}
		batch := allJIDs[start:end]
		rows, err := db.Query(fmt.Sprintf(`
			SELECT jid, COALESCE(push_name, ''), COALESCE(phone, '')
			FROM contacts WHERE jid IN (%s)
		`, inClausePlaceholders(len(batch))), jidsToArgs(batch)...)
		if err != nil {
			return fmt.Errorf("query contacts: %w", err)
		}
		for rows.Next() {
			var jid string
			var c contactLite
			if err := rows.Scan(&jid, &c.pushName, &c.phone); err != nil {
				continue
			}
			contacts[jid] = c
		}
		rows.Close()
	}

	for i := range units {
		u := &units[i]
		closure := append(append([]string{}, u.members...), u.aliasJIDs...)

		if u.chatType != "direct" {
			u.display = chats[u.primary].name
			if u.display == "" {
				u.display = extractPhone(u.primary)
			}
			if u.display == "" {
				u.display = u.primary
			}
			continue
		}

		// Phone: an @s.whatsapp.net form wins; else a contacts.phone that is
		// not secretly LID digits.
		lidDigits := map[string]bool{}
		for _, jid := range closure {
			if strings.HasSuffix(jid, "@lid") {
				lidDigits[extractPhone(jid)] = true
			}
		}
		for _, jid := range closure {
			if strings.HasSuffix(jid, "@s.whatsapp.net") {
				u.phone = extractPhone(jid)
				break
			}
		}
		if u.phone == "" {
			for _, jid := range closure {
				if c, ok := contacts[jid]; ok && c.phone != "" && !lidDigits[c.phone] {
					u.phone = c.phone
					break
				}
			}
		}

		for _, jid := range u.members { // members are primary-first
			if c, ok := contacts[jid]; ok && c.pushName != "" && !strings.HasPrefix(c.pushName, "+") {
				u.display = c.pushName
				break
			}
		}
		if u.display == "" {
			for _, jid := range u.members {
				if n := chats[jid].name; n != "" && !strings.HasPrefix(n, "+") {
					u.display = n
					break
				}
			}
		}
		if u.display == "" {
			if u.phone != "" {
				u.display = "+" + u.phone
			} else if d := extractPhone(u.primary); d != "" {
				u.display = "+" + d
			} else {
				u.display = u.primary
			}
		}
	}
	return nil
}

// chatRowLite is the slice of a chats row the export planner needs.
type chatRowLite struct {
	jid               string
	chatType          string
	name              string
	lastMessageTs     int64
	participantsCount int
}

// assignFilenames gives every unit a deterministic, collision-free filename.
//
// Direct chats keep the historical `<Display>.md` shape. Non-direct chats
// ALWAYS carry their chat_type (`<Display> (group).md`), so a group can never
// occupy a person-named file. When two units still collide (two humans with
// the same push name, two groups with the same subject), every collider gets
// its JID digits appended — deterministic, so names never churn between runs.
func assignFilenames(units []exportUnit) {
	base := func(u *exportUnit, disambiguate bool) string {
		name := sanitizeFilename(u.display)
		switch {
		case u.chatType == "direct" && !disambiguate:
			return name
		case u.chatType == "direct":
			tag := u.phone
			if tag != "" {
				tag = "+" + tag
			} else if tag = extractPhone(u.primary); tag == "" {
				tag = sanitizeFilename(u.primary)
			}
			return fmt.Sprintf("%s (%s)", name, tag)
		case !disambiguate:
			return fmt.Sprintf("%s (%s)", name, u.chatType)
		default:
			tag := extractPhone(u.primary)
			if tag == "" {
				tag = sanitizeFilename(u.primary)
			}
			return fmt.Sprintf("%s (%s %s)", name, u.chatType, tag)
		}
	}

	byName := map[string][]int{}
	for i := range units {
		n := strings.ToLower(base(&units[i], false))
		byName[n] = append(byName[n], i)
	}
	for _, idxs := range byName {
		disambiguate := len(idxs) > 1
		for _, i := range idxs {
			units[i].filename = base(&units[i], disambiguate) + ".md"
		}
	}
}

// chatFileIdentity is the slice of a chat file's frontmatter the exporter
// needs to decide ownership and freshness.
type chatFileIdentity struct {
	exists       bool
	fileType     string
	jid          string
	aliasJIDs    []string
	chatType     string
	lastTs       int64
	messageCount int
}

// readChatFileIdentity parses the identity fields out of an existing chat
// file. Zero values (exists=false) when the file is missing or has no
// frontmatter.
func readChatFileIdentity(path string) chatFileIdentity {
	var id chatFileIdentity
	data, err := os.ReadFile(path)
	if err != nil {
		return id
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return id
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return id
	}
	id.exists = true
	for _, line := range strings.Split(rest[:end], "\n") {
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "type":
			id.fileType = strings.Trim(val, `"`)
		case "jid":
			id.jid = strings.Trim(val, `"`)
		case "chat_type":
			id.chatType = strings.Trim(val, `"`)
		case "last_message_ts":
			fmt.Sscanf(val, "%d", &id.lastTs)
		case "message_count":
			fmt.Sscanf(val, "%d", &id.messageCount)
		case "alias_jids":
			var arr []string
			if err := json.Unmarshal([]byte(val), &arr); err == nil {
				id.aliasJIDs = arr
			}
		}
	}
	return id
}

// healLegacyFilenames renames files written under pre-MYC-3555 names to the
// unit's current filename: a group that sat at a bare person name, or a chat
// filed under raw LID digits before its push name was known. Only a file whose
// frontmatter jid provably belongs to the unit is touched, and only when the
// target name is free. Renamed units are force-rewritten so stale content
// (fabricated phone lines, participants_count: 0) regenerates immediately.
func healLegacyFilenames(outputDir string, units []exportUnit) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return
	}
	claimedBy := map[string]string{} // jid → basename of the file claiming it
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		id := readChatFileIdentity(filepath.Join(outputDir, e.Name()))
		if id.jid != "" && claimedBy[id.jid] == "" {
			claimedBy[id.jid] = e.Name()
		}
	}
	for i := range units {
		u := &units[i]
		if _, err := os.Stat(filepath.Join(outputDir, u.filename)); err == nil {
			continue // target already exists; ownership is checked at write time
		}
		for _, jid := range u.members {
			old := claimedBy[jid]
			if old == "" || old == u.filename {
				continue
			}
			if err := os.Rename(filepath.Join(outputDir, old), filepath.Join(outputDir, u.filename)); err != nil {
				log.Printf("export: heal rename %q → %q failed: %v", old, u.filename, err)
				continue
			}
			log.Printf("export: healed legacy filename %q → %q", old, u.filename)
			u.forceRewrite = true
			break
		}
	}
}

func exportOneUnit(db *sql.DB, outputDir string, u exportUnit, todayStr string) (writeResult, error) {
	memberSet := map[string]bool{}
	for _, jid := range u.members {
		memberSet[jid] = true
	}
	unitClosure := append([]string{u.primary}, u.aliasJIDs...)
	sort.Strings(unitClosure)

	out := filepath.Join(outputDir, u.filename)
	id := readChatFileIdentity(out)

	// The existing file only counts as "ours" when the jid it declares belongs
	// to this unit. A file some OTHER chat wrote at this path (the pre-fix
	// group-over-person collision) must be overwritten, never trusted for the
	// unchanged-skip and never mined for preserved frontmatter.
	fileIsOurs := !id.exists || id.jid == "" || memberSet[id.jid]

	// Unchanged-skip: same chat, same alias closure (a newly discovered alias
	// must trigger a merge rewrite), and nothing newer in the DB. Mirrors
	// iMessage's _is_unchanged behavior; saves significant I/O on
	// iCloud-synced vaults.
	if !u.forceRewrite && id.exists && fileIsOurs && id.jid != "" && u.lastMessageTs > 0 && id.lastTs >= u.lastMessageTs {
		fileClosure := append([]string{id.jid}, id.aliasJIDs...)
		sort.Strings(fileClosure)
		if strings.Join(fileClosure, "\n") == strings.Join(unitClosure, "\n") {
			return writeResultUnchanged, nil
		}
	}

	// Pull messages for every merged JID form, assembled chronologically.
	// The id tiebreak keeps equal-timestamp ordering deterministic across runs.
	query := fmt.Sprintf(`
		SELECT timestamp, COALESCE(scrubbed_text, COALESCE(content_text, '')), COALESCE(sender_display, ''), is_from_me, type, COALESCE(voice_note_transcript, '')
		FROM messages
		WHERE chat_jid IN (%s)
		ORDER BY timestamp ASC, id ASC
	`, inClausePlaceholders(len(u.members)))
	rows, err := db.Query(query, jidsToArgs(u.members)...)
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
				speaker = u.display
			}
		}

		text := renderMessageText(m.msgType, m.text, m.transcript)
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
		return writeResultEmpty, nil
	}

	lines := []string{
		"---",
		"type: whatsapp-chat",
		fmt.Sprintf(`contact: "%s"`, escapeYAML(u.display)),
	}
	// Only a real phone is ever rendered; a LID printed as a phone number
	// would be fabricated data, so an unknown phone is omitted entirely.
	if u.phone != "" {
		lines = append(lines, fmt.Sprintf(`phone: "+%s"`, u.phone))
	}
	lines = append(lines,
		fmt.Sprintf(`jid: "%s"`, u.primary),
	)
	if len(u.aliasJIDs) > 0 {
		quoted := make([]string, len(u.aliasJIDs))
		for i, a := range u.aliasJIDs {
			quoted[i] = fmt.Sprintf(`"%s"`, escapeYAML(a))
		}
		lines = append(lines, fmt.Sprintf("alias_jids: [%s]", strings.Join(quoted, ", ")))
	}
	lines = append(lines,
		fmt.Sprintf(`chat_type: "%s"`, u.chatType),
		fmt.Sprintf(`message_count: %d`, len(messages)),
		fmt.Sprintf("first_message: %s", dates[0]),
		fmt.Sprintf("last_message: %s", dates[len(dates)-1]),
		fmt.Sprintf("last_message_ts: %d", u.lastMessageTs),
		fmt.Sprintf("last_sync: %s", todayStr),
	)
	// Only emit participants_count for groups; direct chats stay clean.
	// Downstream tooling uses this to enforce per-vault group-size policies
	// (e.g. only auto-ingest groups with fewer than N participants). The
	// stored count comes from group metadata sync; when it is missing the
	// distinct-sender floor keeps the field evaluable — it can undercount a
	// group's lurkers but can never be the 0 that let every group through.
	if u.chatType == "group" {
		n := u.participantsCount
		if floor := distinctSenderFloor(db, u.members); floor > n {
			n = floor
		}
		lines = append(lines, fmt.Sprintf("participants_count: %d", n))
	}

	// Preserve extractor-added frontmatter from the existing file — but only
	// when that file is OURS. Without the ownership check, the person's chat
	// file would inherit whatever a colliding group's file carried. Without
	// preservation at all, every export nukes vault-metadata-extract's
	// whatsapp_* fields and any other downstream-added keys.
	if fileIsOurs {
		if extras := extractNonCanonicalFrontmatter(out); len(extras) > 0 {
			lines = append(lines, extras...)
		}
	}

	lines = append(lines,
		"---",
		"",
		fmt.Sprintf("# WhatsApp: %s", u.display),
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

// renderMessageText turns one stored message row into the text shown in the
// chat file, or "" when the row renders nothing (an empty text/system row).
//
// This is THE definition of "renderable" — exportOneUnit uses it to build the
// file and ReconcileVault uses it (via unitHasRenderableMessages) to decide
// whether a missing file is a dropped chat or a legitimately empty one. Keep
// it single; two copies of this switch would let the export and its auditor
// disagree about what a complete vault looks like.
func renderMessageText(msgType, text, transcript string) string {
	switch msgType {
	case "voice", "audio":
		label := "[Voice note]"
		if msgType == "audio" {
			label = "[Audio]"
		}
		if transcript != "" {
			return fmt.Sprintf("%s %s", label, transcript)
		}
		return label
	case "image":
		if text != "" {
			return fmt.Sprintf("[Image: %s]", text)
		}
		return "[Image]"
	case "video":
		if text != "" {
			return fmt.Sprintf("[Video: %s]", text)
		}
		return "[Video]"
	case "document":
		return "[Document] " + text
	case "sticker":
		return "[Sticker]"
	case "location":
		if text != "" {
			return fmt.Sprintf("[Location: %s]", text)
		}
		return "[Location]"
	case "contact":
		if text != "" {
			return fmt.Sprintf("[Contact: %s]", text)
		}
		return "[Contact]"
	case "reaction":
		return fmt.Sprintf("[Reaction: %s]", text)
	default:
		// MYC-3284 — a message the bridge could not decode carries an
		// explicit marker in its text (it is stored under an allowed type,
		// see extractContent). Render it the way [Voice note] marks an
		// unrecoverable voice note: the reader sees THAT a message exists,
		// who sent it and when (the line format supplies both), and why it
		// could not be read. Never a blank, never silently omitted.
		//
		// MYC-3569 adds the sibling case: a message that never reached the
		// decoder at all because it could not be DECRYPTED. Same treatment,
		// distinct wording, because the two mean different things to a
		// reader — "we could not read this type" versus "this did not
		// decrypt, and it may still arrive".
		if mode := undecryptableFailMode(text); mode != "" {
			return fmt.Sprintf("[Undecryptable message: %s]", mode)
		}
		if raw := unsupportedRawType(text); raw != "" {
			return fmt.Sprintf("[Unsupported message: %s]", raw)
		}
		return text
	}
}

// unitHasRenderableMessages reports whether at least one of the unit's rows
// would produce a line in the chat file, under exactly the rules
// exportOneUnit applies (renderMessageText + the ts >= 1000 epoch guard).
func unitHasRenderableMessages(db *sql.DB, members []string) (bool, error) {
	query := fmt.Sprintf(`
		SELECT timestamp, COALESCE(scrubbed_text, COALESCE(content_text, '')), type, COALESCE(voice_note_transcript, '')
		FROM messages
		WHERE chat_jid IN (%s)
	`, inClausePlaceholders(len(members)))
	rows, err := db.Query(query, jidsToArgs(members)...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var text, msgType, transcript string
		if err := rows.Scan(&ts, &text, &msgType, &transcript); err != nil {
			continue
		}
		if ts >= 1000 && renderMessageText(msgType, text, transcript) != "" {
			return true, nil
		}
	}
	return false, nil
}

// distinctSenderFloor counts the distinct senders recorded across a group's
// messages: an approximation of its participant count used only when group
// metadata sync has not stored the real number yet. It undercounts lurkers
// and can slightly overcount when one member appears under both JID forms,
// but it can never be the flat 0 that let every group slide under a
// group-size policy.
func distinctSenderFloor(db *sql.DB, members []string) int {
	query := fmt.Sprintf(`
		SELECT COUNT(DISTINCT sender_jid) FROM messages
		WHERE chat_jid IN (%s) AND COALESCE(sender_jid, '') <> ''
	`, inClausePlaceholders(len(members)))
	var n int
	if err := db.QueryRow(query, jidsToArgs(members)...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// canonicalFrontmatterKeys lists the keys this exporter owns. Anything else
// in an existing chat file's frontmatter is treated as user/extractor data
// and preserved across re-exports.
var canonicalFrontmatterKeys = map[string]bool{
	"type":               true,
	"contact":            true,
	"phone":              true,
	"jid":                true,
	"alias_jids":         true,
	"chat_type":          true,
	"message_count":      true,
	"first_message":      true,
	"last_message":       true,
	"last_message_ts":    true,
	"last_sync":          true,
	"participants_count": true,
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
