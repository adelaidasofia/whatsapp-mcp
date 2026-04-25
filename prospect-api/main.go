// Package main is the prospect-api service.
//
// Phase A (this build):
//   - POST /api/lookup-prospect    bearer  (low-trust; existence + category enum + confidence)
//   - POST /api/pull-context       bearer  (high-trust; gated by server-verified identity)
//   - POST /api/update-crm         bearer  (whitelist enforced; field allowlist below)
//   - POST /api/check-preset       bearer  (read; matches arriving guest to a pre-warm preset)
//   - POST /api/preset             admin   (write; pre-warm a guest by email/phone)
//   - GET  /healthz                public  (basic up check for Cloudflare)
//
// Phase B (subsequent commits, Sprints 7-10):
//   - POST /api/get-negotiator-terms
//   - POST /api/notify-adelaida
//   - POST /api/relay-note
//   - POST /api/morning-digest
//
// Architecture:
//   - Reads the whatsapp-bridge SQLCipher DB read-only for message/contact/chat lookups.
//   - Owns its own preset + audit DB (unencrypted SQLite, low-sensitivity).
//   - Talks to the whatsapp-bridge HTTP API on :8080 only when sending WhatsApp messages.
//   - Loopback bind only; public exposure is via Cloudflare Tunnel + Access policy.
//
// See SECURITY-PROSPECT.md for the threat model specific to this public-facing service.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	backfillPhones := flag.Bool("backfill-phones", false,
		"Cross-reference WhatsApp contacts with vault CRM notes and write matched phones into CRM frontmatter. Default is dry-run; pass --apply to write.")
	backfillApply := flag.Bool("apply", false,
		"With --backfill-phones: actually write changes (default is dry-run).")
	backfillIncludeWeak := flag.Bool("include-weak", false,
		"With --backfill-phones: include substring fuzzy matches (default off; substring matches have real false-positive risk).")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	log.Println("prospect-api starting")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("config loaded: bind=%s:%d crm=%s preset_db=%s",
		cfg.BindHost, cfg.BindPort, cfg.VaultCRMPath, cfg.PresetDBPath)

	dbKey, err := ReadDBKey(cfg.KeychainService, cfg.KeychainAccount)
	if err != nil {
		log.Fatalf("read db key: %v", err)
	}

	messageDB, err := OpenMessageDB(cfg, dbKey)
	if err != nil {
		log.Fatalf("open message db: %v", err)
	}
	defer messageDB.Close()
	log.Printf("message db opened (read-only): %s", cfg.MessageDBPath)

	presetDB, err := OpenPresetDB(cfg)
	if err != nil {
		log.Fatalf("open preset db: %v", err)
	}
	defer presetDB.Close()
	log.Printf("preset db opened: %s", cfg.PresetDBPath)

	crm, err := NewCRMStore(cfg.VaultCRMPath)
	if err != nil {
		log.Fatalf("crm store: %v", err)
	}

	// One-shot CLI subcommand: --backfill-phones runs the cross-reference
	// (dry-run by default, --apply to write) and exits. Does not bind HTTP.
	if *backfillPhones {
		log.Println("backfill mode (no HTTP server will start)")
		if err := crm.indexAll(); err != nil {
			log.Fatalf("crm index for backfill: %v", err)
		}
		mode := backfillMode{
			DryRun:      !*backfillApply,
			IncludeWeak: *backfillIncludeWeak,
		}
		if err := RunBackfillPhones(context.Background(), cfg, messageDB, crm, mode); err != nil {
			log.Fatalf("backfill failed: %v", err)
		}
		return
	}

	// Async first index pass; iCloud demand-paging may take time.
	go func() {
		start := time.Now()
		if err := crm.indexAll(); err != nil {
			log.Printf("crm initial index failed: %v", err)
		} else {
			log.Printf("crm indexed in %s (%d phone keys, %d email keys)",
				time.Since(start).Round(time.Millisecond), len(crm.byPhone), len(crm.byEmail))
		}
	}()

	server := NewServer(cfg, messageDB, presetDB, crm)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("received %s; shutting down", sig)
		_ = server.Shutdown()
	}()

	log.Printf("HTTP listening on http://%s:%d", cfg.BindHost, cfg.BindPort)
	if err := server.ListenAndServe(nil); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Println("prospect-api stopped cleanly")
}
