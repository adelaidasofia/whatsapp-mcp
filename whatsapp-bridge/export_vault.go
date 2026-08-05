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

	"golang.org/x/text/unicode/norm"
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

	// One scan of the directory feeds everything that follows: name planning
	// (existing out-of-population files are obstacles a unit's name must not
	// fold onto), healing, the pre-write alias guard, and the post-write
	// sweep's pre-state.
	entries, err := scanVaultEntries(outputDir)
	if err != nil {
		return fmt.Errorf("scan output dir: %w", err)
	}

	units, totalChats, err := buildExportUnits(db, includeGroups, minMessages, entries)
	if err != nil {
		return err
	}

	// Heal files left by the pre-MYC-3555 naming (a group at a bare person
	// name, a chat filed under its raw LID digits) before the write pass, so
	// the old file's timestamp cannot shadow this run and the vault does not
	// accumulate duplicates for the same chat.
	healLegacyFilenames(outputDir, units, entries)

	log.Printf("export: writing %d chats (%d chat rows in db) to %s", len(units), totalChats, outputDir)

	todayStr := time.Now().Format("2006-01-02")

	// Small bounded concurrency to hide I/O latency when the output folder is
	// on iCloud. Results land in per-unit slots, so no aggregation lock.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	results := make([]writeResult, len(units))
	errs := make([]error, len(units))

	// Pre-write alias guard: name planning avoided every KNOWN fold-collision
	// with an existing out-of-population file, but fold tables and filesystem
	// semantics can diverge (Turkish dotted-İ casing, exotic normalization).
	// Before writing, verify each target path does not RESOLVE to a
	// differently-named entry belonging to someone else; if it does, refuse
	// that unit's write entirely — prevention, because for a chat since
	// deleted from the DB the file on disk is the LAST copy and a post-write
	// detector could only name what it had already destroyed.
	entryNames := map[string]bool{}
	for i := range entries {
		entryNames[entries[i].name] = true
	}
	for i := range units {
		u := &units[i]
		st, statErr := os.Stat(filepath.Join(outputDir, u.filename))
		if statErr != nil {
			continue // fresh path; nothing to alias
		}
		if entryNames[u.filename] {
			continue // resolves to our own byte-named entry (owned or healed)
		}
		memberSet := map[string]bool{}
		for _, jid := range u.members {
			memberSet[jid] = true
		}
		for j := range entries {
			if entries[j].info == nil || !os.SameFile(entries[j].info, st) {
				continue
			}
			if !memberSet[entries[j].jid] {
				victim := fmt.Sprintf("file %q", entries[j].name)
				if entries[j].jid != "" {
					victim = fmt.Sprintf("file %q (chat %s)", entries[j].name, entries[j].jid)
				}
				errs[i] = fmt.Errorf("writing would destroy %s: the filesystem resolves both names to one file", victim)
			}
			break
		}
	}

	for i := range units {
		if errs[i] != nil {
			continue // blocked by the pre-write alias guard
		}
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
	type onDisk struct {
		i    int
		info os.FileInfo
	}
	var files []onDisk
	for i, u := range units {
		if errs[i] != nil {
			dropped = append(dropped, fmt.Sprintf("%s (→ %s): %v", u.primary, u.filename, errs[i]))
			continue
		}
		switch results[i] {
		case writeResultWrote, writeResultUnchanged:
			// The file must actually exist on disk; a claim without a file is
			// exactly the silent no-op this ticket exists to kill.
			info, statErr := os.Stat(filepath.Join(outputDir, u.filename))
			if statErr != nil {
				dropped = append(dropped, fmt.Sprintf("%s (→ %s): result recorded but file missing: %v", u.primary, u.filename, statErr))
				continue
			}
			files = append(files, onDisk{i: i, info: info})
		case writeResultEmpty:
			skippedEmpty++
		}
	}

	// The RECIPIENT's predicate (PR #64 review rounds 3+4): distinct units
	// must have landed on distinct FILES, and no unit's file may be the same
	// inode as ANY other directory entry it does not own. The measured
	// population is every regular entry of outputDir — not just this run's
	// units — because the exporter writes flat basenames into this one
	// directory, so the entire namespace it can destroy IS that entry list.
	// Round 4's operating rule made the gap concrete: "A Measured Population
	// Is a Floor Until the Measurement Itself Is Exhaustive" — the round-3
	// sweep measured units only, so a file whose chat was filtered out by
	// min-messages, excluded by includeGroups=false, or deleted from the DB
	// was unmeasured, and a case-straddling unit write destroyed it with
	// rc=0. Aliased units are dropped and NAMED; rc=0 keeps meaning complete.
	aliasPartner := map[int]int{}
	for x := 1; x < len(files); x++ {
		for y := 0; y < x; y++ {
			if os.SameFile(files[y].info, files[x].info) {
				if _, seen := aliasPartner[files[x].i]; !seen {
					aliasPartner[files[x].i] = files[y].i
				}
				if _, seen := aliasPartner[files[y].i]; !seen {
					aliasPartner[files[y].i] = files[x].i
				}
			}
		}
	}
	for i, j := range aliasPartner {
		dropped = append(dropped, fmt.Sprintf(
			"%s (→ %s): its path is the SAME FILE as %s (→ %s) — filesystem aliasing collapsed two chats onto one inode",
			units[i].primary, units[i].filename, units[j].primary, units[j].filename))
	}

	// Cross-population half of the sweep: re-list the directory and stat
	// EVERY regular entry that is not a unit's own file. An entry sharing an
	// inode with a unit's file means that unit's write went through a foreign
	// name — unless the entry provably belonged to the unit before the run
	// (the APFS in-place update of a legacy differently-normalized name).
	preJID := map[string]string{}
	for i := range entries {
		preJID[entries[i].name] = entries[i].jid
	}
	unitFilename := map[string]int{}
	for i := range units {
		unitFilename[units[i].filename] = i
	}
	if postEntries, err := os.ReadDir(outputDir); err == nil {
		for _, e := range postEntries {
			if e.IsDir() {
				continue
			}
			if _, isUnit := unitFilename[e.Name()]; isUnit {
				continue
			}
			st, statErr := os.Stat(filepath.Join(outputDir, e.Name()))
			if statErr != nil {
				continue
			}
			for _, f := range files {
				if !os.SameFile(st, f.info) {
					continue
				}
				u := units[f.i]
				legit := false
				if pj, known := preJID[e.Name()]; known && pj != "" {
					for _, jid := range u.members {
						if jid == pj {
							legit = true // the unit's own legacy file, updated in place
							break
						}
					}
				}
				if !legit {
					victim := fmt.Sprintf("pre-existing file %q", e.Name())
					if pj := preJID[e.Name()]; pj != "" {
						victim = fmt.Sprintf("pre-existing file %q (chat %s)", e.Name(), pj)
					}
					dropped = append(dropped, fmt.Sprintf(
						"%s (→ %s): its write went through %s — filesystem aliasing destroyed that file's content",
						u.primary, u.filename, victim))
					if _, seen := aliasPartner[f.i]; !seen {
						aliasPartner[f.i] = f.i // exclude from written counts below
					}
				}
				break
			}
		}
	}

	for _, f := range files {
		if _, isAliased := aliasPartner[f.i]; isAliased {
			continue
		}
		if results[f.i] == writeResultWrote {
			written++
		} else {
			skippedUnchanged++
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
// entries is the caller's scan of the output directory (nil when there is no
// directory to plan against). Existing files whose jid is OUTSIDE this run's
// unit set — chats filtered by min-messages, groups under includeGroups=false,
// chats deleted from the DB, or non-chat notes — are obstacles: a unit whose
// name would FOLD onto one of them escalates away instead of overwriting a
// file this run does not manage (PR #64 round 4).
//
// Returns the eligible units (post includeGroups/minMessages filter) and the
// total number of chat rows in the DB (for the reconciliation line).
func buildExportUnits(db *sql.DB, includeGroups bool, minMessages int, entries []vaultEntry) ([]exportUnit, int, error) {
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
		// The chats-row check above cannot see an edge endpoint with NO row;
		// the server-suffix check catches those, so a group-shaped JID can
		// never enter a person's closure and later mask a real chat at that
		// JID from reconcile's MISSING detection (PR #64 review, LOW 2).
		if chatTypeFromServerSuffix(a) != "direct" || chatTypeFromServerSuffix(b) != "direct" {
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
			// Same defense as the edge guard: only direct-shaped JIDs belong
			// in a person's alias closure.
			if jid != u.primary && chatTypeFromServerSuffix(jid) == "direct" {
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

	// Obstacle map for name planning: fold-keyed names of existing files this
	// run does NOT manage. A file claiming a jid inside the unit set is the
	// unit's own (or its healing source) and must NOT be an obstacle — a unit
	// never escalates away from its own file. Everything else (filtered-out
	// chats, deleted chats, jid-less notes) must never be overwritten via a
	// fold-equal name, so units escalate around them.
	memberJIDs := map[string]bool{}
	for i := range units {
		for _, jid := range units[i].members {
			memberJIDs[jid] = true
		}
	}
	taken := map[string]string{}
	for i := range entries {
		e := entries[i]
		nameLen := len(e.name)
		if nameLen < 3 || !strings.EqualFold(e.name[nameLen-3:], ".md") {
			continue // naming only ever produces .md files; others cannot fold-collide
		}
		if e.jid != "" && memberJIDs[e.jid] {
			continue
		}
		desc := fmt.Sprintf("existing file %q", e.name)
		if e.jid != "" {
			desc = fmt.Sprintf("existing file %q (chat %s, outside this run's filters)", e.name, e.jid)
		}
		taken[foldKey(e.name[:nameLen-3])] = desc
	}

	assignFilenames(units, taken)

	// Structural guarantee behind invariant 4: no two units may share a final
	// path, and no unit may land fold-equal to a file this run does not
	// manage. Anything the fixed-point escalation could not separate fails
	// the run here, naming both sides — never a silent last-writer-wins on
	// one file, and never two goroutines writing the same path (PR #64
	// reviews, HIGH findings rounds 2 and 4).
	byFinal := map[string]int{}
	for i := range units {
		key := foldKey(strings.TrimSuffix(units[i].filename, ".md"))
		if j, dup := byFinal[key]; dup {
			return nil, 0, fmt.Errorf("filename collision after disambiguation: %s and %s both resolve to %q — refusing to export",
				units[j].primary, units[i].primary, units[i].filename)
		}
		if desc, blocked := taken[key]; blocked {
			return nil, 0, fmt.Errorf("filename collision after disambiguation: %s resolves to %q, which collides with %s — refusing to export",
				units[i].primary, units[i].filename, desc)
		}
		byFinal[key] = i
	}

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

// assignFilenames gives every unit a deterministic filename, escalated to a
// FIXED POINT on the final (lowercased) names.
//
// Direct chats keep the historical `<Display>.md` shape; non-direct chats
// ALWAYS carry their chat_type (`<Display> (group).md`), so a group can never
// occupy a person-named file. Collisions escalate per unit through levels:
//
//	0  the historical shape above
//	1  phone / JID-digit tag appended (two humans sharing a push name)
//	2  the full sanitized primary JID appended
//
// Every member of a colliding FINAL-name set escalates and the loop
// re-checks, so the two shapes that slipped through a single
// undecorated-name pass (PR #64 review, HIGH finding) resolve here: both JID
// forms of one human carrying the same repaired phone with no alias edge
// (identical level-1 tags → distinct level-2 JIDs), and a push name that
// itself mimics a disambiguated name ("Name (+1555…)") colliding with a
// genuinely disambiguated file (both sides escalate until distinct).
//
// A name colliding with a `taken` obstacle (an existing file this run does
// not manage, keyed by foldKey) escalates exactly like an in-set collision —
// obstacles never move, units move around them — so a filtered-out chat's
// file or a user's note is never claimed by a fold-equal unit name (PR #64
// round 4).
//
// buildExportUnits verifies final uniqueness afterwards and FAILS the run on
// any residue (pathological JIDs that sanitize identically, or an obstacle
// squatting every level of a unit's name), so the concurrent write pass can
// never hold two units with the same target. Filesystem aliasing beyond the
// fold's knowledge is handled by the pre-write guard and the SameFile sweep
// in ExportVault.
func assignFilenames(units []exportUnit, taken map[string]string) {
	name := func(u *exportUnit, level int) string {
		base := sanitizeFilename(u.display)
		if u.chatType != "direct" {
			switch level {
			case 0:
				return fmt.Sprintf("%s (%s)", base, u.chatType)
			case 1:
				tag := extractPhone(u.primary)
				if tag == "" {
					tag = sanitizeFilename(u.primary)
				}
				return fmt.Sprintf("%s (%s %s)", base, u.chatType, tag)
			default:
				return fmt.Sprintf("%s (%s %s)", base, u.chatType, sanitizeFilename(u.primary))
			}
		}
		switch level {
		case 0:
			return base
		case 1:
			// The tag routes through sanitizeFilename like every other
			// filename component, so an NFC/NFD twin pair of malformed
			// contacts.phone values folds equal here and escalates to
			// distinct level-2 JIDs instead of producing byte-distinct
			// finals that alias on APFS (PR #64 round 4, finding 2).
			tag := u.phone
			if tag != "" {
				tag = sanitizeFilename("+" + tag)
			} else if tag = extractPhone(u.primary); tag == "" {
				tag = sanitizeFilename(u.primary)
			}
			return fmt.Sprintf("%s (%s)", base, tag)
		default:
			return fmt.Sprintf("%s (%s)", base, sanitizeFilename(u.primary))
		}
	}

	const maxLevel = 2
	levels := make([]int, len(units))
	// Run to the TRUE fixed point, not a capped round count. Termination is
	// still guaranteed: a round either escalates at least one unit or changes
	// nothing and exits, levels only grow, and each is capped at maxLevel —
	// so the loop runs at most maxLevel*len(units)+1 rounds. The cap that
	// used to sit here (3 rounds) turned a RESOLVABLE corpus into a
	// fail-closed residue error: a chain of push names mimicking each
	// successive decorated form needs one round per link, so four
	// attacker-influenceable contact names bricked the entire export
	// (PR #64 review round 3, MEDIUM). The residue check in buildExportUnits
	// remains the backstop for sets that genuinely cannot be separated.
	for {
		byName := map[string][]int{}
		for i := range units {
			key := foldKey(name(&units[i], levels[i]))
			byName[key] = append(byName[key], i)
		}
		changed := false
		for key, idxs := range byName {
			_, blocked := taken[key]
			if len(idxs) < 2 && !blocked {
				continue
			}
			for _, i := range idxs {
				if levels[i] < maxLevel {
					levels[i]++
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	for i := range units {
		units[i].filename = name(&units[i], levels[i]) + ".md"
	}
}

// foldKey is THE filename-equality predicate for planning: Unicode
// canonical composition (NFC) plus lowercasing, matching how sanitizeFilename
// writes names and approximating how case- and normalization-insensitive
// filesystems compare them. Anything the fold cannot model (exotic casefold
// tables, links) is caught by the SameFile layers instead.
func foldKey(s string) string {
	return strings.ToLower(norm.NFC.String(s))
}

// vaultEntry is one non-directory entry in the output directory, as seen by
// the planning scan: its exact byte name, the jid of the chat file WE own at
// that name ("" for anything else), and its Lstat FileInfo for
// inode-identity checks (nil when the entry could not be statted).
type vaultEntry struct {
	name string
	jid  string
	info os.FileInfo
}

// scanVaultEntries lists every non-directory entry in dir. Frontmatter
// identity is parsed for regular .md files (case-insensitive suffix — a
// case-insensitive volume resolves "X.MD" and "x.md" alike); everything else
// still enters the scan so the SameFile sweeps measure the ENTIRE namespace
// the exporter can write through, not a filtered subset.
//
// The .jid field means "this is a chat file WE own for that jid": it is set
// only when the file declares `type: whatsapp-chat`. A file that carries a
// chat's jid without that type — a user's annotated copy of a chat file —
// is somebody's own document: an OBSTACLE for naming, never a heal source,
// never a legitimate alias, never ours to overwrite (round 5, F1). This is
// also what makes the export's ownership definition identical to
// ReconcileVault's chat-file definition; the two halves used to disagree.
func scanVaultEntries(dir string) ([]vaultEntry, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []vaultEntry
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		// Lstat, not Stat: a symlink must be SEEN as a symlink. Stat followed
		// the link and skipped failures, so a dangling symlink squatting a
		// planned name was invisible — fail-open — and os.WriteFile then
		// followed it OUT of the output directory (round 5, F2). A symlink,
		// like an unstattable entry (nil info), now enters the scan as a
		// jid-less name obstacle: the planner escalates around it and the
		// SameFile passes skip it.
		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			entries = append(entries, vaultEntry{name: e.Name()})
			continue
		}
		ve := vaultEntry{name: e.Name(), info: info}
		if info.Mode()&os.ModeSymlink != 0 {
			entries = append(entries, ve)
			continue
		}
		if n := len(e.Name()); n >= 3 && strings.EqualFold(e.Name()[n-3:], ".md") {
			if id := readChatFileIdentity(filepath.Join(dir, e.Name())); id.fileType == "whatsapp-chat" {
				ve.jid = id.jid
			}
		}
		entries = append(entries, ve)
	}
	return entries, nil
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
func healLegacyFilenames(outputDir string, units []exportUnit, entries []vaultEntry) {
	claimedBy := map[string]int{} // jid → index into entries of the file claiming it
	for i := range entries {
		if entries[i].jid != "" {
			if _, seen := claimedBy[entries[i].jid]; !seen {
				claimedBy[entries[i].jid] = i
			}
		}
	}
	for i := range units {
		u := &units[i]
		if _, err := os.Stat(filepath.Join(outputDir, u.filename)); err == nil {
			continue // target already exists; ownership is checked at write time
		}
		for _, jid := range u.members {
			ei, ok := claimedBy[jid]
			if !ok || entries[ei].name == u.filename {
				continue
			}
			old := entries[ei].name
			if err := os.Rename(filepath.Join(outputDir, old), filepath.Join(outputDir, u.filename)); err != nil {
				log.Printf("export: heal rename %q → %q failed: %v", old, u.filename, err)
				continue
			}
			log.Printf("export: healed legacy filename %q → %q", old, u.filename)
			// Keep the shared scan truthful for the pre-write guard and the
			// post-write sweep: the entry now lives under the unit's name.
			entries[ei].name = u.filename
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

	// The existing file only counts as "ours" when it declares BOTH the
	// whatsapp-chat type and a jid belonging to this unit — the same
	// ownership definition the planning scan and ReconcileVault use (round
	// 5, F1). A file some OTHER chat wrote at this path (the pre-fix
	// group-over-person collision) must be overwritten, never trusted for
	// the unchanged-skip and never mined for preserved frontmatter. A
	// jid-less or non-chat file here is NOT ours either (round 5, F4): plan
	// time treats those as obstacles, so one at our target can only mean it
	// appeared after the scan — a TOCTOU residual we do not inherit
	// frontmatter from or trust for skips.
	fileIsOurs := !id.exists || (id.fileType == "whatsapp-chat" && memberSet[id.jid])

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
	// NFC-fold BOTH what is compared and what is written. Every filename
	// component derives from this function, so folding here is one chokepoint
	// covering dedup keys, final names, and disambiguation tags. The WRITTEN
	// bytes are folded too — not just the comparison key — because
	// filesystems disagree about normalization: APFS resolves names
	// normalization-insensitively (the NFC and NFD spellings of "José" open
	// ONE physical file — the PR #64 round-3 HIGH: two byte-distinct units
	// silently sharing one inode with rc=0), while ext4/NTFS compare bytes
	// (the same twins would be TWO files). Writing canonical NFC makes the
	// export behave identically on every platform, and the collision loop
	// inherits the fold because its keys are built from folded names.
	name = norm.NFC.String(name)
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
