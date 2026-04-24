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
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	importBaileys := flag.String("import-baileys", "",
		"Path to a baileys_store.json from the prior Baileys sync. If set, the bridge imports that store into SQLite and exits without connecting to WhatsApp.")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	log.Println("whatsapp-mcp bridge starting")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	log.Printf("config loaded: bind=%s:%d db=%s crm=%s whisper=%s",
		cfg.BridgeHost, cfg.BridgePort, cfg.DBPath, truncateForLog(cfg.VaultCRMPath), cfg.WhisperBackend)

	var dbKey string
	if cfg.EncryptDB {
		dbKey, err = GetOrCreateDBKey(cfg.KeychainService, cfg.KeychainAccount)
		if err != nil {
			log.Fatalf("DB key: %v", err)
		}
		log.Println("DB key resolved from platform secret store")
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

	bridge, err := NewBridge(ctx, cfg, db, dbKey)
	if err != nil {
		log.Fatalf("bridge init: %v", err)
	}
	defer bridge.Disconnect()

	// Connect triggers QR flow on first run or reconnect on subsequent runs.
	if err := bridge.Connect(ctx); err != nil {
		log.Fatalf("bridge connect: %v", err)
	}

	server := NewServer(cfg, db, bridge)

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
