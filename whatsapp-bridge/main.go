// Package main is the whatsapp-mcp Go bridge.
//
// Responsibilities:
//   - Wrap go.mau.fi/whatsmeow for the WhatsApp Web multidevice protocol.
//   - Own an SQLCipher-encrypted SQLite database of contacts, chats, messages, calls.
//   - Expose a localhost-only REST API consumed by the Python MCP server.
//   - Handle QR and pairing-code authentication.
//   - Log every tool call to audit.log.
//
// Security:
//   - Binds to 127.0.0.1 only. See config.Validate().
//   - DB encrypted at rest with SQLCipher. Key from macOS Keychain.
//   - No outbound network calls except to WhatsApp itself (and optional OpenAI Whisper if opted in).
//   - See SECURITY.md for the full threat model.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	importBaileys := flag.String("import-baileys", "",
		"Path to a baileys_store.json from the prior Baileys sync. If set, the bridge imports that store into SQLite and exits without connecting to WhatsApp.")
	exportVault := flag.String("export-to-vault", "",
		"Path to an output folder. If set, the bridge regenerates one markdown file per chat from SQLite into that folder and exits without connecting to WhatsApp.")
	exportIncludeGroups := flag.Bool("export-include-groups", false,
		"When set with --export-to-vault, also exports group chats (default exports direct chats only, matching Baileys behavior).")
	exportMinMessages := flag.Int("export-min-messages", 0,
		"With --export-to-vault: only export chats with at least this many messages (default 0 = export everything; recommended 5 to skip drive-by contacts).")
	pairPhone := flag.String("pair-phone", "",
		"Pair by typed code instead of QR scanning: your own phone number in international format (e.g. +15551234567). The 8-char code prints here; on the phone: WhatsApp > Settings > Linked Devices > Link a Device > 'Link with phone number instead'. Works on Android and iOS.")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	log.Println("whatsapp-mcp bridge starting")

	// SETUP.md tells users to `cp .env.example .env`; actually honor it.
	// Process env always wins over the file.
	loadDotEnv()

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	log.Printf("config loaded: bind=%s:%d db=%s crm=%s whisper=%s",
		cfg.BridgeHost, cfg.BridgePort, cfg.DBPath, truncateForLog(cfg.VaultCRMPath), cfg.WhisperBackend)

	// One-time visibility for the v0.2.0 default flip: users who relied on
	// the old local-cpp default would otherwise lose transcription silently.
	if cfg.WhisperBackend == "off" {
		log.Println("voice-note transcription is OFF (WHATSAPP_WHISPER_BACKEND=off, default since v0.2.0); set local-cpp or openai-api to enable — media keys are still stored, so recent notes backfill once enabled")
	}

	var dbKey string
	if cfg.EncryptDB {
		if cfg.DBKey != "" {
			// Explicit override (headless/CI/custom secret manager). This was
			// documented for a long time but never actually implemented.
			if len(cfg.DBKey) != 64 {
				log.Fatalf("WHATSAPP_DB_KEY must be 64 hex chars (32 bytes); got %d chars", len(cfg.DBKey))
			}
			if _, err := hex.DecodeString(cfg.DBKey); err != nil {
				log.Fatalf("WHATSAPP_DB_KEY is not valid hex: %v", err)
			}
			dbKey = cfg.DBKey
			log.Println("DB key: explicit WHATSAPP_DB_KEY override")
		} else {
			dbKey, err = GetOrCreateDBKey(cfg.KeychainService, cfg.KeychainAccount)
			if err != nil {
				log.Fatalf("DB key: %v", err)
			}
			log.Println("DB key resolved from platform secret store")
		}
	}

	db, err := OpenDB(cfg, dbKey)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	log.Printf("DB opened: %s (encrypted=%t)", cfg.DBPath, cfg.EncryptDB)

	// Import-only path: no whatsmeow connection, just read the Baileys store into SQLite.
	if *importBaileys != "" {
		if err := RunBaileysImport(cfg, db, *importBaileys); err != nil {
			log.Fatalf("import failed: %v", err)
		}
		log.Println("baileys import done")
		return
	}

	// Export-only path: regenerate markdown vault files from SQLite and exit.
	if *exportVault != "" {
		if err := ExportVault(db, *exportVault, *exportIncludeGroups, *exportMinMessages); err != nil {
			log.Fatalf("export failed: %v", err)
		}
		log.Println("export done")
		return
	}

	// CRM name-enrichment runs asynchronously after startup so it does not block
	// HTTP server bring-up or the whatsmeow connection. Vault folders are often
	// in iCloud and demand-paging slows file reads; we do not want that latency
	// on the main startup path. Safe to re-run; idempotent.
	if cfg.VaultCRMPath != "" {
		go func() {
			updated, err := EnrichContactsFromVault(db, cfg.VaultCRMPath)
			if err != nil {
				log.Printf("crm enrich failed (continuing): %v", err)
			} else {
				log.Printf("crm enrich: %d contacts updated with vault names", updated)
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the voice-note transcription worker pool. Always created; only
	// actually does work when voice messages arrive and the configured
	// whisper binary + model are present.
	transcriber := NewTranscriber(cfg, db)
	defer transcriber.Close()

	bridge, err := NewBridge(ctx, cfg, db, dbKey, transcriber)
	if err != nil {
		log.Fatalf("bridge init: %v", err)
	}
	defer bridge.Disconnect()
	if *pairPhone != "" {
		bridge.SetPairPhoneOnStart(*pairPhone)
	}

	// Self-healing voice-note transcript backfill. Periodically re-enqueues
	// any voice/audio rows that have re-download keys but NULL transcripts.
	// Recovers from any transient failure mode (validation, queue full,
	// transient whisper-cli, ffmpeg crash, network blip) automatically.
	backfiller := NewTranscriptBackfiller(db, bridge.Client(), transcriber, cfg.BackfillIntervalSeconds, cfg.BackfillWindowDays)
	go backfiller.Run(ctx)
	log.Printf("transcript backfiller started: interval=%ds window=%dd", cfg.BackfillIntervalSeconds, cfg.BackfillWindowDays)

	// JID-alias backfill. Asks whatsmeow's local LID store for the alt form of
	// every contact and records it in jid_aliases. Also repairs the legacy
	// "LID stored as phone" rows. Runs once at startup, async so it can't
	// block HTTP serving. Idempotent; cheap (single round-trip per contact
	// against an in-memory store). See aliases.go.
	go func() {
		// Small delay so whatsmeow has a chance to populate its LID store
		// from the initial connection before we ask it to enumerate.
		select {
		case <-time.After(15 * time.Second):
		case <-ctx.Done():
			return
		}
		written, repaired, err := BackfillJIDAliases(ctx, db, bridge.Client())
		if err != nil {
			log.Printf("jid alias backfill failed (non-fatal): %v", err)
			return
		}
		log.Printf("jid alias backfill: %d new edges written, %d phone columns repaired", written, repaired)
	}()

	server := NewServer(cfg, db, bridge, backfiller)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("received signal %s; shutting down", sig)
		cancel()
		bridge.Disconnect()
		if err := server.Shutdown(); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}()

	// Authentication runs in the background so the HTTP API is live DURING
	// first-run pairing — a supervisor drives /api/auth/* exactly then, and
	// the terminal QR loop re-requests fresh codes on timeout instead of
	// killing the process (auth.go). Auth errors no longer take the API down.
	go func() {
		if err := bridge.RunAuth(ctx); err != nil && ctx.Err() == nil {
			log.Printf("auth: %v — HTTP API stays up; POST /api/auth/reconnect to retry", err)
		}
	}()

	log.Printf("HTTP server listening on http://%s:%d", cfg.BridgeHost, cfg.BridgePort)
	if err := server.ListenAndServe(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Println("bridge stopped cleanly")
}

// truncateForLog returns a short version of a path for logging, redacting if empty.
func truncateForLog(p string) string {
	if p == "" {
		return "(unset)"
	}
	if len(p) > 60 {
		return p[:60] + "..."
	}
	return p
}
