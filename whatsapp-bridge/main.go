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
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	log.Println("whatsapp-mcp bridge starting")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	log.Printf("config loaded: bind=%s:%d db=%s crm=%s whisper=%s",
		cfg.BridgeHost, cfg.BridgePort, cfg.DBPath, truncateForLog(cfg.VaultCRMPath), cfg.WhisperBackend)

	// Obtain DB encryption key from the platform secret store.
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bridge = whatsmeow client, wired to our SQLite-backed event handlers.
	// Full implementation lands in bridge.go in the next commit.
	// For v0.1.0, boot the HTTP server with a healthcheck so installs can be validated end-to-end.
	server := NewServer(cfg, db)

	// Handle graceful shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("received signal %s; shutting down", sig)
		cancel()
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
