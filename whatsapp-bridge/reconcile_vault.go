package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReconcileVault compares the SQLite DB against an exported vault folder and
// reports every way the two disagree. It exists because MYC-3555 was found by
// hand-grepping 905 files: a run had dropped an entire direct chat and filed a
// group under a person's name, with rc=0 and nothing measuring the gap.
//
// Checks (each finding is printed on its own line, machine-greppable by the
// MISSING / MISFILED / DRIFT / DUPLICATE / ORPHAN / COLLISION prefixes):
//
//	MISSING    a chat in the DB with messages that no vault file covers
//	MISFILED   a non-direct chat sitting in a file without its type marker
//	           (a group occupying a person-named file)
//	DRIFT      a file whose message_count disagrees with the DB
//	DUPLICATE  a JID claimed by more than one file
//	ORPHAN     a chat file whose JID no longer exists in the DB
//	COLLISION  a unit's expected path RESOLVES (on this filesystem) to an
//	           existing file the unit does not own — an export would refuse
//	           to write it (read-only mirror of the export's pre-write guard)
//
// Same filters as the export (includeGroups, minMessages), and the same
// directory-aware name planning, so the comparison is against exactly what
// the export would produce. Returns the findings; the caller exits non-zero
// when there are any.
func ReconcileVault(db *sql.DB, vaultDir string, includeGroups bool, minMessages int) ([]string, error) {
	entries, err := scanVaultEntries(vaultDir)
	if err != nil {
		return nil, fmt.Errorf("read vault dir: %w", err)
	}

	units, totalChats, err := buildExportUnits(db, includeGroups, minMessages, entries)
	if err != nil {
		return nil, err
	}

	allChatJIDs := map[string]bool{}
	rows, err := db.Query(`SELECT jid FROM chats`)
	if err != nil {
		return nil, fmt.Errorf("query chats: %w", err)
	}
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err == nil {
			allChatJIDs[jid] = true
		}
	}
	rows.Close()

	// Index every whatsapp-chat file by every JID it claims.
	type vaultFile struct {
		name string
		id   chatFileIdentity
	}
	var files []vaultFile
	claims := map[string][]string{} // jid → file basenames claiming it
	for i := range entries {
		if entries[i].jid == "" {
			continue // not a chat file this exporter owns
		}
		id := readChatFileIdentity(filepath.Join(vaultDir, entries[i].name))
		if !id.exists || id.fileType != "whatsapp-chat" {
			continue
		}
		files = append(files, vaultFile{name: entries[i].name, id: id})
		for _, jid := range append([]string{id.jid}, id.aliasJIDs...) {
			claims[jid] = append(claims[jid], entries[i].name)
		}
	}

	var findings []string

	// MISSING — the ticket's headline failure: a chat with messages in the DB
	// and no file anywhere in the vault. A chat whose rows all render to
	// nothing (e.g. legacy empty system rows) is a legitimate export skip,
	// judged by the SAME renderability predicate the export uses.
	for _, u := range units {
		if u.msgCount == 0 {
			continue // legitimately skipped as empty by the export
		}
		covered := false
		for _, jid := range u.members {
			if len(claims[jid]) > 0 {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		renderable, err := unitHasRenderableMessages(db, u.members)
		if err != nil {
			return nil, fmt.Errorf("checking renderable messages for %s: %w", u.primary, err)
		}
		if renderable {
			findings = append(findings, fmt.Sprintf(
				"MISSING   %s (%d messages) has no vault file; expected %q",
				strings.Join(u.members, " + "), u.msgCount, u.filename))
		}
	}

	// MISFILED — a group/broadcast/community chat in a file whose name does
	// not mark it as one (the person-named group file).
	for _, f := range files {
		chatType := f.id.chatType
		if chatType == "" || chatType == "direct" {
			continue
		}
		if !strings.Contains(f.name, "("+chatType) {
			findings = append(findings, fmt.Sprintf(
				"MISFILED  %q holds %s chat %s but its filename carries no (%s) marker",
				f.name, chatType, f.id.jid, chatType))
		}
	}

	// DRIFT — file's recorded message_count vs the DB's current count for the
	// same chat (across merged JID forms).
	unitByJID := map[string]*exportUnit{}
	for i := range units {
		for _, jid := range units[i].members {
			unitByJID[jid] = &units[i]
		}
	}
	for _, f := range files {
		u := unitByJID[f.id.jid]
		if u == nil {
			continue // orphan or filtered-out; handled below
		}
		if f.id.messageCount != u.msgCount {
			findings = append(findings, fmt.Sprintf(
				"DRIFT     %q message_count=%d but the DB holds %d rows for %s",
				f.name, f.id.messageCount, u.msgCount, strings.Join(u.members, " + ")))
		}
	}

	// DUPLICATE — two files claiming the same JID (stale pre-fix leftovers).
	dupJIDs := make([]string, 0)
	for jid, names := range claims {
		if len(names) > 1 {
			dupJIDs = append(dupJIDs, jid)
		}
	}
	sort.Strings(dupJIDs)
	for _, jid := range dupJIDs {
		names := append([]string{}, claims[jid]...)
		sort.Strings(names)
		findings = append(findings, fmt.Sprintf(
			"DUPLICATE %s is claimed by %d files: %s", jid, len(names), strings.Join(names, ", ")))
	}

	// ORPHAN — a chat file whose primary JID is gone from the DB entirely.
	for _, f := range files {
		if !allChatJIDs[f.id.jid] {
			findings = append(findings, fmt.Sprintf(
				"ORPHAN    %q claims %s which no longer exists in the DB", f.name, f.id.jid))
		}
	}

	// COLLISION — the read-only mirror of the export's pre-write alias guard:
	// a unit's expected path resolving (on THIS filesystem) to a
	// differently-named file the unit does not own. Name planning avoids
	// every fold-predictable case, so this fires only where fold tables and
	// filesystem semantics diverge — exactly the shape an export run would
	// refuse to write.
	entryNames := map[string]bool{}
	for i := range entries {
		entryNames[entries[i].name] = true
	}
	for i := range units {
		u := units[i]
		st, statErr := os.Stat(filepath.Join(vaultDir, u.filename))
		if statErr != nil || entryNames[u.filename] {
			continue
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
				findings = append(findings, fmt.Sprintf(
					"COLLISION %s expects %q, which this filesystem resolves to %s — an export would refuse to write it",
					u.primary, u.filename, victim))
			}
			break
		}
	}

	sort.Strings(findings)
	for _, f := range findings {
		fmt.Println(f)
	}
	fmt.Printf("reconcile: chats_in_db=%d exportable_units=%d chat_files=%d findings=%d\n",
		totalChats, len(units), len(files), len(findings))
	return findings, nil
}
