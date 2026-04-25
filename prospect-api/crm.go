package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// CRMRecord is a normalized view of a vault CRM markdown file.
// We read it for matching (lookup, pull-context) and update it via a strict whitelist (update-crm).
type CRMRecord struct {
	// FilePath is the absolute path to the .md file.
	FilePath string `json:"-"`
	// Name is derived from the filename (sans .md) — vault convention.
	Name string `json:"name"`

	// Frontmatter fields (read-only consumers + whitelisted writers).
	Email        string   `json:"email,omitempty" yaml:"email,omitempty"`
	Phone        string   `json:"phone,omitempty" yaml:"phone,omitempty"`
	Company      string   `json:"company,omitempty" yaml:"company,omitempty"`
	Role         string   `json:"role,omitempty" yaml:"role,omitempty"`
	Relationship string   `json:"relationship,omitempty" yaml:"relationship,omitempty"`
	Tags         []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Aliases      []string `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	LastTopic    string   `json:"lastTopic,omitempty" yaml:"lastTopic,omitempty"`

	// Frontmatter raw map for whitelist write-back.
	rawFrontmatter map[string]any `json:"-" yaml:"-"`
	body           string         `json:"-" yaml:"-"`
}

// CRMStore wraps read + write access to a vault CRM folder.
// Maintains a small in-memory phone+email index for fast match.
type CRMStore struct {
	root string

	mu        sync.RWMutex
	byPhone   map[string]string // last-10-digits -> file path
	byEmail   map[string]string // lowercased -> file path
	loadedAt  time.Time
}

// NewCRMStore opens a store rooted at the configured CRM folder.
// Index is built lazily on first lookup; we tolerate iCloud demand-paging by
// allowing a partial first scan and re-indexing on demand.
func NewCRMStore(root string) (*CRMStore, error) {
	if root == "" {
		return nil, errors.New("crm root path required")
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat crm root: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("crm root is not a directory: %s", root)
	}
	return &CRMStore{
		root:    root,
		byPhone: map[string]string{},
		byEmail: map[string]string{},
	}, nil
}

// indexAll walks the CRM folder and rebuilds the phone/email indexes.
// Safe to call repeatedly; callers usually do this on lookup if the index is stale.
func (s *CRMStore) indexAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	byPhone := map[string]string{}
	byEmail := map[string]string{}

	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rec, err := readCRMFile(path)
		if err != nil {
			return nil
		}
		if p := DigitsOnly(rec.Phone); len(p) >= 7 {
			tail := p
			if len(tail) > 10 {
				tail = tail[len(tail)-10:]
			}
			byPhone[tail] = path
		}
		if e := NormalizeEmail(rec.Email); e != "" {
			byEmail[e] = path
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.byPhone = byPhone
	s.byEmail = byEmail
	s.loadedAt = time.Now()
	return nil
}

// FindByPhone returns the CRM record (if any) for a given E.164/raw phone.
// Matches by trailing-10-digits.
func (s *CRMStore) FindByPhone(phone string) (*CRMRecord, error) {
	digits := DigitsOnly(phone)
	if len(digits) < 7 {
		return nil, nil
	}
	tail := digits
	if len(tail) > 10 {
		tail = tail[len(tail)-10:]
	}
	if err := s.ensureIndexed(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	path, ok := s.byPhone[tail]
	s.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	return readCRMFile(path)
}

// FindByEmail returns the CRM record (if any) for a given email address.
// Case-insensitive.
func (s *CRMStore) FindByEmail(email string) (*CRMRecord, error) {
	e := NormalizeEmail(email)
	if e == "" {
		return nil, nil
	}
	if err := s.ensureIndexed(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	path, ok := s.byEmail[e]
	s.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	return readCRMFile(path)
}

// FindByName tries to locate a CRM record by name. Two passes:
//   1. exact filename match (vault convention)
//   2. aliases substring match within frontmatter aliases array
// Returns nil, nil when nothing matches. This is the lowest-confidence match path.
func (s *CRMStore) FindByName(name string) (*CRMRecord, error) {
	if name == "" {
		return nil, nil
	}
	target := Normalize(name)

	// Pass 1: filename match.
	candidate := filepath.Join(s.root, name+".md")
	if _, err := os.Stat(candidate); err == nil {
		return readCRMFile(candidate)
	}

	// Pass 2: walk and check aliases.
	var found *CRMRecord
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rec, err := readCRMFile(path)
		if err != nil {
			return nil
		}
		if Normalize(rec.Name) == target {
			found = rec
			return errStopWalk
		}
		for _, a := range rec.Aliases {
			if Normalize(a) == target {
				found = rec
				return errStopWalk
			}
		}
		return nil
	})
	if err != nil && err != errStopWalk {
		return nil, err
	}
	return found, nil
}

var errStopWalk = errors.New("stop")

func (s *CRMStore) ensureIndexed() error {
	s.mu.RLock()
	loaded := !s.loadedAt.IsZero()
	s.mu.RUnlock()
	if !loaded {
		return s.indexAll()
	}
	return nil
}

// readCRMFile parses a single .md file's YAML frontmatter into a CRMRecord.
func readCRMFile(path string) (*CRMRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rec := &CRMRecord{
		FilePath: path,
		Name:     strings.TrimSuffix(filepath.Base(path), ".md"),
	}
	content := string(b)
	if !strings.HasPrefix(content, "---") {
		// File has no frontmatter; return name-only record.
		return rec, nil
	}
	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return rec, nil
	}
	fm := rest[:end]
	body := ""
	if len(rest) > end+4 {
		body = strings.TrimLeft(rest[end+4:], "\n")
	}
	rec.body = body

	raw := map[string]any{}
	if err := yaml.Unmarshal([]byte(fm), &raw); err == nil {
		rec.rawFrontmatter = raw
		// Pull known fields out of raw for typed access.
		if v, ok := stringField(raw, "email"); ok {
			rec.Email = v
		}
		if v, ok := stringField(raw, "phone"); ok {
			rec.Phone = v
		}
		if v, ok := stringField(raw, "company"); ok {
			rec.Company = v
		}
		if v, ok := stringField(raw, "role"); ok {
			rec.Role = v
		}
		if v, ok := stringField(raw, "relationship"); ok {
			rec.Relationship = v
		}
		if v, ok := stringField(raw, "lastTopic"); ok {
			rec.LastTopic = v
		}
		rec.Tags = stringSliceField(raw, "tags")
		rec.Aliases = stringSliceField(raw, "aliases")
	}
	return rec, nil
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

func stringSliceField(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// --- Whitelisted write-back -------------------------------------------------

// CRMUpdates carries the agent's requested changes. Server-side whitelist filters
// to only allowed fields. Anything else is silently dropped (logged).
type CRMUpdates struct {
	Email           *string  `json:"email,omitempty"`
	Phone           *string  `json:"phone,omitempty"`
	PhoneAlt        *string  `json:"phoneAlt,omitempty"`
	Company         *string  `json:"company,omitempty"`
	Role            *string  `json:"role,omitempty"`
	LastTopic       *string  `json:"lastTopic,omitempty"`
	LastInteraction *string  `json:"lastInteraction,omitempty"`
	TagsAdd         []string `json:"tagsAdd,omitempty"`
}

// UpdateCRM applies whitelisted updates to a CRM file.
//
// Rules:
//   - email/phone/company/role/lastTopic: write only if the existing field is empty
//     OR forceOverwrite is true.
//   - tagsAdd: union with existing tags (no duplicates), regardless of forceOverwrite.
//   - Returns the actual list of fields written for audit.
//   - Never touches body content; only frontmatter.
func (s *CRMStore) UpdateCRM(filePath string, upd CRMUpdates, forceOverwrite bool) ([]string, error) {
	rec, err := readCRMFile(filePath)
	if err != nil {
		return nil, err
	}
	if rec.rawFrontmatter == nil {
		rec.rawFrontmatter = map[string]any{}
	}
	written := []string{}

	maybeWrite := func(key string, ptr *string) {
		if ptr == nil {
			return
		}
		val := strings.TrimSpace(*ptr)
		if val == "" {
			return
		}
		existing, _ := stringField(rec.rawFrontmatter, key)
		if existing != "" && !forceOverwrite {
			return
		}
		rec.rawFrontmatter[key] = val
		written = append(written, key)
	}
	maybeWrite("email", upd.Email)
	maybeWrite("phone", upd.Phone)
	maybeWrite("phone_alt", upd.PhoneAlt)
	maybeWrite("company", upd.Company)
	maybeWrite("role", upd.Role)
	maybeWrite("lastTopic", upd.LastTopic)
	maybeWrite("last_interaction", upd.LastInteraction)

	if len(upd.TagsAdd) > 0 {
		existing := stringSliceField(rec.rawFrontmatter, "tags")
		seen := map[string]bool{}
		for _, t := range existing {
			seen[Normalize(t)] = true
		}
		merged := append([]string{}, existing...)
		added := false
		for _, t := range upd.TagsAdd {
			t = strings.TrimSpace(t)
			if t == "" || seen[Normalize(t)] {
				continue
			}
			merged = append(merged, t)
			seen[Normalize(t)] = true
			added = true
		}
		if added {
			rec.rawFrontmatter["tags"] = merged
			written = append(written, "tags")
		}
	}

	if len(written) == 0 {
		return written, nil
	}

	// Re-marshal frontmatter and write back the file. Body preserved.
	fmBytes, err := yaml.Marshal(rec.rawFrontmatter)
	if err != nil {
		return nil, fmt.Errorf("marshal frontmatter: %w", err)
	}
	out := "---\n" + strings.TrimRight(string(fmBytes), "\n") + "\n---\n\n" + rec.body
	if err := os.WriteFile(filePath, []byte(out), 0o644); err != nil {
		return nil, fmt.Errorf("write crm file: %w", err)
	}
	return written, nil
}
