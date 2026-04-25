package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// createMissingCRMMode controls runtime behavior for the --create-missing-crm subcommand.
type createMissingCRMMode struct {
	DryRun         bool
	IncludeUnsaved bool // include phones NOT in macOS Contacts (default off; saved-only is safer)
	SkipIMessage   bool // skip iMessage even if FDA is granted
}

// actionKind describes what create-missing-crm will do for one merged contact.
type actionKind string

const (
	actionBackfillPhone   actionKind = "BACKFILL-PHONE"
	actionCreate          actionKind = "CREATE"
	actionSkipCRMPhone    actionKind = "SKIP-CRM-PHONE"
	actionSkipLowVolume   actionKind = "SKIP-LOW-VOLUME"
	actionSkipNotInBook   actionKind = "SKIP-NOT-IN-CONTACTS"
	actionSkipCollision   actionKind = "SKIP-COLLISION"
	actionSkipPathEscape  actionKind = "SKIP-PATH-ESCAPE"
)

// phoneChannel holds per-phone WhatsApp + iMessage stats for a single E.164 number.
// One mergedContact carries 1+ phoneChannels (one per phone the person has).
type phoneChannel struct {
	E164      string
	WAJID     string
	WAPushName string
	WAVerified string
	WAMsgs    int
	WALastTS  int64
	IMHandle  string
	IMService string // "iMessage" | "SMS"
	IMMsgs    int
	IMLastTS  int64
}

func (c *phoneChannel) totalMsgs() int { return c.WAMsgs + c.IMMsgs }
func (c *phoneChannel) lastTS() int64 {
	if c.WALastTS > c.IMLastTS {
		return c.WALastTS
	}
	return c.IMLastTS
}

// mergedContact is one human being, identified by macOS Contacts UUID when
// possible (so all of their phone numbers roll up to a single CRM note).
type mergedContact struct {
	MergeKey   string          // ZUNIQUEID if known, else phone-tail
	PersonUUID string          // empty if not in macOS Contacts
	SavedName  string          // canonical name from macOS Contacts.app
	AllSavedPhones []string    // ALL phones for this person from macOS Contacts (may include numbers we have no messages on)
	Channels   []*phoneChannel // one per E.164 we actually have messages on
}

func (m *mergedContact) totalMessages() int {
	n := 0
	for _, c := range m.Channels {
		n += c.totalMsgs()
	}
	return n
}

func (m *mergedContact) lastTS() int64 {
	var max int64
	for _, c := range m.Channels {
		if t := c.lastTS(); t > max {
			max = t
		}
	}
	return max
}

// phonesByMessages returns this person's E.164 numbers sorted by total
// message count desc — primary first, alt second, etc.
func (m *mergedContact) phonesByMessages() []string {
	type pc struct {
		e164  string
		count int
	}
	var pcs []pc
	for _, c := range m.Channels {
		pcs = append(pcs, pc{c.E164, c.totalMsgs()})
	}
	sort.Slice(pcs, func(i, j int) bool { return pcs[i].count > pcs[j].count })
	out := make([]string, 0, len(pcs))
	for _, p := range pcs {
		out = append(out, p.e164)
	}
	return out
}

// primaryPhone returns the phone with the most total messages — the value we
// write into the CRM frontmatter `phone:` field.
func (m *mergedContact) primaryPhone() string {
	if ps := m.phonesByMessages(); len(ps) > 0 {
		return ps[0]
	}
	return ""
}

// altPhone returns the second-most-messaged phone. "" if person has only one.
func (m *mergedContact) altPhone() string {
	if ps := m.phonesByMessages(); len(ps) > 1 {
		return ps[1]
	}
	return ""
}

// lastInteractionDate returns the most-recent message date as YYYY-MM-DD.
// Empty when no message timestamp is known.
func (m *mergedContact) lastInteractionDate() string {
	if ts := m.lastTS(); ts > 0 {
		return time.Unix(ts, 0).Format("2006-01-02")
	}
	return ""
}

// activeE164s returns every E.164 across this person's channels (deduped, ordered).
func (m *mergedContact) activeE164s() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range m.Channels {
		if !seen[c.E164] {
			seen[c.E164] = true
			out = append(out, c.E164)
		}
	}
	return out
}

// sources returns the unique set of channels ("whatsapp", "imessage") present.
func (m *mergedContact) sources() []string {
	hasWA, hasIM := false, false
	for _, c := range m.Channels {
		if c.WAMsgs > 0 {
			hasWA = true
		}
		if c.IMMsgs > 0 {
			hasIM = true
		}
	}
	var s []string
	if hasWA {
		s = append(s, "whatsapp")
	}
	if hasIM {
		s = append(s, "imessage")
	}
	return s
}

// bestPushName returns the longest WA push_name across all channels.
func (m *mergedContact) bestPushName() string {
	var best string
	for _, c := range m.Channels {
		if len(c.WAPushName) > len(best) {
			best = c.WAPushName
		}
	}
	return best
}

func (m *mergedContact) bestVerifiedName() string {
	var best string
	for _, c := range m.Channels {
		if len(c.WAVerified) > len(best) {
			best = c.WAVerified
		}
	}
	return best
}

// crmImportAction is one planned outcome for a single merged contact.
type crmImportAction struct {
	Contact          *mergedContact
	DisplayName      string
	Kind             actionKind
	BaseName         string
	FilePath         string
	Aliases          []string
	CRMMatchFile     string
	CRMMatchFilePath string
}

// safeFilename strips characters illegal in vault filenames: / \ ? % * : | " < > [ ]
func safeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '/', '\\', '?', '%', '*', ':', '|', '"', '<', '>', '[', ']':
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// nameKey heavily normalizes a name for matching: lowercase, accent-folded,
// emoji- and punctuation-stripped, single-spaced.
//   "Firi 🫶🏼🫶🏼🫶🏼"     → "firi"
//   "Angélica Barón"     → "angelica baron"
//   "María-Paula"        → "maria paula"
func nameKey(s string) string {
	if s == "" {
		return ""
	}
	folded := Normalize(s)
	var b strings.Builder
	prevSpace := true
	for _, r := range folded {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.':
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		default:
			// Drop emojis, symbols, punctuation.
		}
	}
	return strings.TrimSpace(b.String())
}

func nameTokens(s string) []string {
	k := nameKey(s)
	if k == "" {
		return nil
	}
	return strings.Fields(k)
}

// tokenSuperset reports whether wantTokens is a subset of haveTokens.
// Requires wantTokens length ≥ 2 to avoid single-name false positives.
func tokenSuperset(haveTokens, wantTokens []string) bool {
	if len(wantTokens) < 2 {
		return false
	}
	have := map[string]bool{}
	for _, t := range haveTokens {
		have[t] = true
	}
	for _, w := range wantTokens {
		if !have[w] {
			return false
		}
	}
	return true
}

// phoneTail returns the trailing-10-digit form. "" if <7 digits.
func phoneTail(phone string) string {
	digits := DigitsOnly(phone)
	if len(digits) < 7 {
		return ""
	}
	if len(digits) > 10 {
		return digits[len(digits)-10:]
	}
	return digits
}

// RunCreateMissingCRM is the unified-channel CRM bootstrap subcommand.
//
// Pipeline:
//   1. Load macOS Contacts → tailToPerson (phone-tail → MacPerson) and
//      personIndex (UUID → MacPerson). One person, many phones.
//   2. Load 1:1 WhatsApp contacts.
//   3. Load 1:1 iMessage contacts (skipped if FDA not granted).
//   4. Merge by macOS Contacts UUID (or phone-tail when not saved).
//      Multiple phone numbers for one person collapse into ONE mergedContact
//      with multiple phoneChannels.
//   5. For each mergedContact: name-match → BACKFILL-PHONE; phone-match → SKIP;
//      ≥6 total msgs and saved in Contacts → CREATE; else SKIP.
//   6. Print plan + (in --apply mode) write files.
func RunCreateMissingCRM(ctx context.Context, cfg *Config, messageDB *sql.DB, crm *CRMStore, mode createMissingCRMMode) error {
	today := time.Now().Format("2006-01-02")

	// --- Step 1: macOS Contacts (person-grouped) -----------------------------
	tailToPerson, personIndex, macRowCount, err := LoadMacContacts()
	if err != nil {
		log.Printf("warning: load macOS Contacts failed: %v", err)
	}
	log.Printf("create-missing-crm: macOS Contacts: %d people, %d total phone rows",
		len(personIndex), macRowCount)
	if len(personIndex) == 0 && !mode.IncludeUnsaved {
		return fmt.Errorf("no macOS Contacts loaded — grant Contacts permission to Claude.app/Terminal, or pass --include-unsaved to bypass the filter")
	}

	// --- Step 2: WhatsApp ----------------------------------------------------
	waContacts, err := loadWhatsAppContacts(ctx, messageDB)
	if err != nil {
		return fmt.Errorf("load whatsapp contacts: %w", err)
	}
	log.Printf("create-missing-crm: WhatsApp 1:1 contacts: %d", len(waContacts))

	// --- Step 3: iMessage ----------------------------------------------------
	var imContacts []iMessageContact
	if !mode.SkipIMessage {
		imContacts, err = LoadIMessage1to1Contacts(ctx)
		if errors.Is(err, ErrIMessagePermissionDenied) {
			log.Printf("warning: iMessage skipped — %v", err)
			imContacts = nil
		} else if err != nil {
			log.Printf("warning: iMessage load failed: %v", err)
			imContacts = nil
		}
	}
	log.Printf("create-missing-crm: iMessage 1:1 contacts: %d", len(imContacts))

	// --- Step 4: Merge by person UUID (or phone-tail when not saved) ---------
	merged := map[string]*mergedContact{}

	// Helper: get-or-create mergedContact for a phone, choosing UUID-based
	// merge key when the phone is in macOS Contacts.
	getOrCreate := func(rawPhone string) (*mergedContact, *phoneChannel) {
		tail := phoneTail(rawPhone)
		if tail == "" {
			return nil, nil
		}
		e164 := rawPhone
		if e164 != "" && !strings.HasPrefix(e164, "+") {
			e164 = "+" + DigitsOnly(e164)
		}
		var key string
		var person *MacPerson
		if p, ok := tailToPerson[tail]; ok {
			key = p.UUID
			person = p
		} else {
			key = "tail:" + tail
		}
		mc, ok := merged[key]
		if !ok {
			mc = &mergedContact{MergeKey: key}
			if person != nil {
				mc.PersonUUID = person.UUID
				mc.SavedName = person.Name
				mc.AllSavedPhones = append([]string{}, person.Phones...)
			}
			merged[key] = mc
		}
		// Find or create the phoneChannel for this E.164.
		for _, ch := range mc.Channels {
			if ch.E164 == e164 {
				return mc, ch
			}
		}
		ch := &phoneChannel{E164: e164}
		mc.Channels = append(mc.Channels, ch)
		return mc, ch
	}

	for _, w := range waContacts {
		// Normalize WA phone (prefer contacts.phone; fall back to JID prefix).
		phone := strings.TrimSpace(w.Phone)
		if phone == "" {
			if idx := strings.Index(w.JID, "@"); idx > 0 {
				prefix := w.JID[:idx]
				if colon := strings.Index(prefix, ":"); colon >= 0 {
					prefix = prefix[:colon]
				}
				phone = prefix
			}
		}
		_, ch := getOrCreate(phone)
		if ch == nil {
			continue
		}
		ch.WAJID = w.JID
		ch.WAPushName = w.PushName
		ch.WAVerified = w.VerifiedName
		ch.WAMsgs = w.MsgCount
		ch.WALastTS = w.LastTS
	}

	for _, im := range imContacts {
		if im.IsEmail || im.Phone == "" {
			continue // email-only iMessage handles can't merge with WA by phone
		}
		_, ch := getOrCreate(im.Phone)
		if ch == nil {
			continue
		}
		ch.IMHandle = im.Handle
		ch.IMService = im.Service
		ch.IMMsgs = im.MsgCount
		ch.IMLastTS = im.LastTSSec
	}

	// --- Step 5: Build CRM indexes -------------------------------------------
	type crmRef struct {
		base, path string
		tokens     []string
	}
	keyToCRM := map[string]*crmRef{}
	multiTokenRefs := []*crmRef{}
	existingNames := map[string]bool{}

	err = filepath.WalkDir(crm.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		base := strings.TrimSuffix(d.Name(), ".md")
		existingNames[strings.ToLower(base)] = true
		toks := nameTokens(base)
		ref := &crmRef{base: base, path: path, tokens: toks}
		if k := nameKey(base); k != "" {
			keyToCRM[k] = ref
		}
		if len(toks) >= 2 {
			multiTokenRefs = append(multiTokenRefs, ref)
		}
		rec, err := readCRMFile(path)
		if err != nil {
			return nil
		}
		for _, a := range rec.Aliases {
			if a == "" {
				continue
			}
			if k := nameKey(a); k != "" {
				if _, exists := keyToCRM[k]; !exists {
					keyToCRM[k] = ref
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk crm: %w", err)
	}
	log.Printf("create-missing-crm: CRM index: %d name keys, %d multi-token files, %d files",
		len(keyToCRM), len(multiTokenRefs), len(existingNames))

	// --- Step 6: Plan one action per merged contact --------------------------
	absRoot, _ := filepath.Abs(crm.root)
	var actions []crmImportAction

	// Stable iteration order: by lastTS desc, then by merge key.
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		mi, mj := merged[keys[i]], merged[keys[j]]
		if mi.lastTS() != mj.lastTS() {
			return mi.lastTS() > mj.lastTS()
		}
		return keys[i] < keys[j]
	})

	for _, k := range keys {
		mc := merged[k]
		act := crmImportAction{Contact: mc}

		// Display name priority: macOS saved → strongest WA push_name → verified → primary phone.
		displayName := firstNonEmpty(mc.SavedName, mc.bestPushName(), mc.bestVerifiedName(), mc.primaryPhone())
		act.DisplayName = displayName

		// --- Filter A: not saved in macOS Contacts (default behavior) -------
		if !mode.IncludeUnsaved && mc.SavedName == "" {
			act.Kind = actionSkipNotInBook
			actions = append(actions, act)
			continue
		}

		// --- Filter B: name-match against existing CRM (BACKFILL-PHONE) -----
		var matchedRef *crmRef
		nameCandidates := []string{mc.SavedName, mc.bestPushName(), mc.bestVerifiedName()}
		for _, candidate := range nameCandidates {
			if candidate == "" {
				continue
			}
			k := nameKey(candidate)
			if k == "" {
				continue
			}
			if ref, ok := keyToCRM[k]; ok {
				matchedRef = ref
				break
			}
			haveTokens := nameTokens(candidate)
			for _, ref := range multiTokenRefs {
				if tokenSuperset(haveTokens, ref.tokens) {
					matchedRef = ref
					break
				}
			}
			if matchedRef != nil {
				break
			}
		}
		if matchedRef != nil {
			act.Kind = actionBackfillPhone
			act.CRMMatchFile = matchedRef.base + ".md"
			act.CRMMatchFilePath = matchedRef.path
			actions = append(actions, act)
			continue
		}

		// --- Filter C: phone-match (note already has a phone field) ----------
		// Try every E.164 we know for this person; skip if any phone resolves.
		var phoneMatched *CRMRecord
		for _, ph := range mc.activeE164s() {
			existing, err := crm.FindByPhone(ph)
			if err == nil && existing != nil {
				phoneMatched = existing
				break
			}
		}
		if phoneMatched != nil {
			act.Kind = actionSkipCRMPhone
			act.CRMMatchFile = filepath.Base(phoneMatched.FilePath)
			act.CRMMatchFilePath = phoneMatched.FilePath
			actions = append(actions, act)
			continue
		}

		// --- Filter D: volume gate (only CREATE for ≥6 total messages) ------
		if mc.totalMessages() < 6 {
			act.Kind = actionSkipLowVolume
			actions = append(actions, act)
			continue
		}

		// --- Plan a CREATE ---------------------------------------------------
		baseName := safeFilename(displayName)
		if baseName == "" {
			baseName = mc.primaryPhone()
		}

		// Aliases: original name variants different from base.
		var aliases []string
		seen := map[string]bool{strings.ToLower(baseName): true}
		for _, candidate := range nameCandidates {
			if candidate == "" {
				continue
			}
			safe := safeFilename(candidate)
			key := strings.ToLower(safe)
			if key == "" || seen[key] {
				continue
			}
			aliases = append(aliases, candidate)
			seen[key] = true
		}
		act.Aliases = aliases

		// Collision: append last-4 of primary phone.
		finalBase := baseName
		if existingNames[strings.ToLower(finalBase)] {
			last4 := ""
			if d := DigitsOnly(mc.primaryPhone()); len(d) >= 4 {
				last4 = d[len(d)-4:]
			}
			if last4 == "" {
				act.Kind = actionSkipCollision
				act.BaseName = finalBase
				actions = append(actions, act)
				continue
			}
			disambig := baseName + " - " + last4
			if existingNames[strings.ToLower(disambig)] {
				act.Kind = actionSkipCollision
				act.BaseName = disambig
				actions = append(actions, act)
				continue
			}
			finalBase = disambig
		}

		targetPath := filepath.Join(crm.root, finalBase+".md")
		absTarget, _ := filepath.Abs(targetPath)
		if !strings.HasPrefix(absTarget, absRoot+string(os.PathSeparator)) {
			act.Kind = actionSkipPathEscape
			actions = append(actions, act)
			continue
		}

		act.Kind = actionCreate
		act.BaseName = finalBase
		act.FilePath = targetPath
		actions = append(actions, act)
		existingNames[strings.ToLower(finalBase)] = true // reserve within this run
	}

	// Sort for output: CREATE → BACKFILL → SKIP-PHONE → SKIP-LOW → ...
	kindOrder := map[actionKind]int{
		actionCreate:         0,
		actionBackfillPhone:  1,
		actionSkipCRMPhone:   2,
		actionSkipLowVolume:  3,
		actionSkipNotInBook:  4,
		actionSkipCollision:  5,
		actionSkipPathEscape: 6,
	}
	sort.Slice(actions, func(i, j int) bool {
		oi := kindOrder[actions[i].Kind]
		oj := kindOrder[actions[j].Kind]
		if oi != oj {
			return oi < oj
		}
		return actions[i].DisplayName < actions[j].DisplayName
	})

	tally := map[actionKind]int{}
	for _, a := range actions {
		tally[a.Kind]++
	}

	// --- Step 7: Print plan + (optionally) apply ----------------------------
	fmt.Println()
	fmt.Println("=== create-missing-crm plan ===")
	created := 0
	backfilled := 0
	createErrors := 0
	backfillErrors := 0

	for _, act := range actions {
		switch act.Kind {
		case actionCreate:
			lastDate := ""
			if ts := act.Contact.lastTS(); ts > 0 {
				lastDate = time.Unix(ts, 0).Format("2006-01-02")
			}
			srcs := strings.Join(act.Contact.sources(), "+")
			phones := len(act.Contact.activeE164s())
			fmt.Printf("CREATE             %-36s msgs=%-5d last=%-12s src=%-15s phones=%d  file=%s.md\n",
				act.DisplayName, act.Contact.totalMessages(), lastDate, srcs, phones, act.BaseName)
			if !mode.DryRun {
				if err := writeCRMNote(act, today, absRoot); err != nil {
					log.Printf("write %s: %v", act.FilePath, err)
					createErrors++
				} else {
					created++
				}
			}

		case actionBackfillPhone:
			extras := []string{}
			if act.Contact.altPhone() != "" {
				extras = append(extras, "alt="+act.Contact.altPhone())
			}
			if d := act.Contact.lastInteractionDate(); d != "" {
				extras = append(extras, "last="+d)
			}
			extraStr := ""
			if len(extras) > 0 {
				extraStr = "  " + strings.Join(extras, " ")
			}
			fmt.Printf("BACKFILL-PHONE     %-36s phone=%-22s crm=%s%s\n",
				act.DisplayName, act.Contact.primaryPhone(), act.CRMMatchFile, extraStr)
			if !mode.DryRun && act.Contact.primaryPhone() != "" && act.CRMMatchFilePath != "" {
				phone := act.Contact.primaryPhone()
				updates := CRMUpdates{Phone: &phone}
				if alt := act.Contact.altPhone(); alt != "" {
					updates.PhoneAlt = &alt
				}
				if d := act.Contact.lastInteractionDate(); d != "" {
					updates.LastInteraction = &d
				}
				written, err := crm.UpdateCRM(act.CRMMatchFilePath, updates, false)
				if err != nil {
					log.Printf("backfill %s → %s: %v", phone, act.CRMMatchFile, err)
					backfillErrors++
				} else if len(written) > 0 {
					backfilled++
				}
			}

		case actionSkipCRMPhone:
			fmt.Printf("SKIP-CRM-PHONE     %-36s phone=%-22s crm=%s\n",
				act.DisplayName, act.Contact.primaryPhone(), act.CRMMatchFile)

		case actionSkipNotInBook:
			// Suppress noisy output.
		case actionSkipLowVolume:
			// Suppress noisy output.

		case actionSkipCollision:
			fmt.Printf("SKIP-COLLISION     %-36s phone=%-22s would-be=%s.md\n",
				act.DisplayName, act.Contact.primaryPhone(), act.BaseName)
		case actionSkipPathEscape:
			fmt.Printf("SKIP-PATH-ESCAPE   %-36s\n", act.DisplayName)
		}
	}

	fmt.Println()
	fmt.Println("=== create-missing-crm summary ===")
	fmt.Printf("macOS Contacts loaded:                %d people (%d phones)\n", len(personIndex), macRowCount)
	fmt.Printf("WhatsApp 1:1 contacts:                %d\n", len(waContacts))
	fmt.Printf("iMessage 1:1 contacts:                %d\n", len(imContacts))
	fmt.Printf("Merged unique people (post-roll-up):  %d\n", len(merged))
	fmt.Println()
	fmt.Printf("Planned BACKFILL-PHONE:               %d  (existing CRM note → add phone field)\n", tally[actionBackfillPhone])
	fmt.Printf("Planned CREATE:                       %d  (new CRM note, ≥6 msgs total, saved in Contacts)\n", tally[actionCreate])
	fmt.Println()
	fmt.Printf("Skipped (not in macOS Contacts):      %d\n", tally[actionSkipNotInBook])
	fmt.Printf("Skipped (low volume <6 msgs total):   %d\n", tally[actionSkipLowVolume])
	fmt.Printf("Skipped (CRM note already has phone): %d\n", tally[actionSkipCRMPhone])
	fmt.Printf("Skipped (filename collision):          %d\n", tally[actionSkipCollision])
	if tally[actionSkipPathEscape] > 0 {
		fmt.Printf("Skipped (path escape):                %d\n", tally[actionSkipPathEscape])
	}
	if mode.DryRun {
		fmt.Println()
		fmt.Println("DRY-RUN: no files written. Re-run with --create-missing-crm --apply to write.")
	} else {
		fmt.Printf("New CRM notes written:                %d\n", created)
		fmt.Printf("Existing notes phone-backfilled:      %d\n", backfilled)
		if createErrors+backfillErrors > 0 {
			fmt.Printf("Errors:                               %d create, %d backfill\n", createErrors, backfillErrors)
		}
	}
	return nil
}

// --- WhatsApp loader -----------------------------------------------------

type waContact struct {
	JID, PushName, VerifiedName, Phone string
	MsgCount                           int
	LastTS                             int64
}

func loadWhatsAppContacts(ctx context.Context, db *sql.DB) ([]waContact, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			c.jid,
			COALESCE(c.push_name, '')     AS push_name,
			COALESCE(c.verified_name, '') AS verified_name,
			COALESCE(c.phone, '')         AS phone,
			COUNT(*)                      AS msg_count,
			MAX(m.timestamp)              AS last_ts
		FROM contacts c
		JOIN messages m ON m.chat_jid = c.jid
		WHERE c.jid LIKE '%@s.whatsapp.net'
		GROUP BY c.jid
		ORDER BY last_ts DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query 1:1 wa contacts: %w", err)
	}
	defer rows.Close()
	var out []waContact
	for rows.Next() {
		var w waContact
		if err := rows.Scan(&w.JID, &w.PushName, &w.VerifiedName, &w.Phone, &w.MsgCount, &w.LastTS); err != nil {
			continue
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// --- File writer --------------------------------------------------------

// writeCRMNote writes a single new CRM .md file inside absRoot.
// Frontmatter `phone:` carries the most-messaged number; the body lists every
// known phone with its per-channel activity so the user sees the full picture.
func writeCRMNote(act crmImportAction, today, absRoot string) error {
	absTarget, _ := filepath.Abs(act.FilePath)
	if !strings.HasPrefix(absTarget, absRoot+string(os.PathSeparator)) {
		return fmt.Errorf("path escape: %s outside crm root %s", act.FilePath, absRoot)
	}

	mc := act.Contact
	lastDate := ""
	if ts := mc.lastTS(); ts > 0 {
		lastDate = time.Unix(ts, 0).Format("2006-01-02")
	}

	// Aliases YAML block.
	var aliasBlock string
	if len(act.Aliases) == 0 {
		aliasBlock = "aliases: []\n"
	} else {
		var sb strings.Builder
		sb.WriteString("aliases:\n")
		for _, a := range act.Aliases {
			if strings.ContainsAny(a, `:"'{}[]|>&`) {
				a = strings.ReplaceAll(a, `"`, `\"`)
				sb.WriteString(fmt.Sprintf("  - \"%s\"\n", a))
			} else {
				sb.WriteString(fmt.Sprintf("  - %s\n", a))
			}
		}
		aliasBlock = sb.String()
	}

	sources := strings.Join(mc.sources(), "+")
	if sources == "" {
		sources = "unknown"
	}
	source := sources + "-import-" + today

	// Body: per-phone breakdown so the user sees both numbers if multiple.
	var phoneLines []string
	for _, ch := range mc.Channels {
		var bits []string
		if ch.WAMsgs > 0 {
			waLast := ""
			if ch.WALastTS > 0 {
				waLast = time.Unix(ch.WALastTS, 0).Format("2006-01-02")
			}
			bits = append(bits, fmt.Sprintf("WhatsApp %d msgs (last %s)", ch.WAMsgs, waLast))
		}
		if ch.IMMsgs > 0 {
			imLast := ""
			if ch.IMLastTS > 0 {
				imLast = time.Unix(ch.IMLastTS, 0).Format("2006-01-02")
			}
			svc := ch.IMService
			if svc == "" {
				svc = "iMessage"
			}
			bits = append(bits, fmt.Sprintf("%s %d msgs (last %s)", svc, ch.IMMsgs, imLast))
		}
		if len(bits) > 0 {
			phoneLines = append(phoneLines, fmt.Sprintf("- %s — %s", ch.E164, strings.Join(bits, ", ")))
		}
	}
	// Also list any saved phones we have NO messages on (for completeness).
	activeSet := map[string]bool{}
	for _, ch := range mc.Channels {
		activeSet[ch.E164] = true
	}
	var unusedPhones []string
	for _, p := range mc.AllSavedPhones {
		if !activeSet[p] {
			unusedPhones = append(unusedPhones, p)
		}
	}

	body := fmt.Sprintf("*Imported %s from %s. Total %d messages across %d phone(s).*\n\n## Channels\n%s",
		today, sources, mc.totalMessages(), len(mc.Channels), strings.Join(phoneLines, "\n"))
	if len(unusedPhones) > 0 {
		body += fmt.Sprintf("\n\n## Other saved phones (no message history)\n- %s",
			strings.Join(unusedPhones, "\n- "))
	}
	body += "\n\nFill in relationship, company, and any other context manually."

	displayName := act.DisplayName

	// Optional phone_alt for multi-phone people.
	phoneAltLine := ""
	if alt := mc.altPhone(); alt != "" {
		phoneAltLine = fmt.Sprintf("phone_alt: \"%s\"\n", alt)
	}

	content := fmt.Sprintf(
		"---\n"+
			"creationDate: %s\n"+
			"updated: %s\n"+
			"type: person\n"+
			"%s"+
			"relationship: \"\"\n"+
			"company: \"\"\n"+
			"status: \"\"\n"+
			"last_interaction: %s\n"+
			"next_step: \"\"\n"+
			"priority: medium\n"+
			"source: %s\n"+
			"phone: \"%s\"\n"+
			"%s"+
			"---\n\n"+
			"# %s\n\n"+
			"%s\n",
		today, today, aliasBlock, lastDate, source, mc.primaryPhone(),
		phoneAltLine, displayName, body,
	)

	return os.WriteFile(act.FilePath, []byte(content), 0o644)
}
