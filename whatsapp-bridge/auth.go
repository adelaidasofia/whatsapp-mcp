package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
)

// AuthState is the pairing lifecycle exposed over /api/status so a supervisor
// (Mycelium Studio, launchd wrapper, curl) can drive login without scraping
// stdout. Live-ness (socket up or down) is the separate `connected` boolean;
// this enum tracks whether a device identity exists.
type AuthState string

const (
	AuthStateUnauthenticated AuthState = "unauthenticated" // no device identity, login not started
	AuthStateQRPending       AuthState = "qr_pending"      // login running, QR available at /api/auth/qr
	AuthStatePairingPending  AuthState = "pairing_pending" // pairing code issued, waiting for phone entry
	AuthStatePaired          AuthState = "paired"          // device identity exists (connected or briefly offline)
	AuthStateLoggedOut       AuthState = "logged_out"      // WhatsApp revoked the session; re-pair required
	AuthStateTimedOut        AuthState = "timed_out"       // QR batch expired; a fresh batch is being requested
)

// AuthSnapshot is the full auth-side state for /api/status and /api/auth/qr.
type AuthSnapshot struct {
	State           AuthState
	QRCode          string
	QRExpiresAt     time.Time
	PairingCode     string
	LoggedOutReason string
}

func (b *Bridge) setAuthState(s AuthState) {
	b.mu.Lock()
	b.authState = s
	b.mu.Unlock()
}

func (b *Bridge) AuthSnapshot() AuthSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return AuthSnapshot{
		State:           b.authState,
		QRCode:          b.currentQR,
		QRExpiresAt:     b.qrExpiresAt,
		PairingCode:     b.pairingCode,
		LoggedOutReason: b.loggedOutReason,
	}
}

// RunAuth drives authentication to completion. Returning user: reconnect with
// the persisted identity. First run (or post-logout): run the QR/pairing-code
// login loop until paired or ctx is cancelled. Blocking; main() runs it in a
// goroutine so the HTTP API is up during pairing (a supervisor needs
// /api/status and /api/auth/* exactly then).
func (b *Bridge) RunAuth(ctx context.Context) error {
	if b.client.Store.ID != nil {
		// Returning user: reconnect with persisted identity.
		b.setAuthState(AuthStatePaired)
		if err := b.client.Connect(); err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
		b.mu.Lock()
		b.connected = true
		b.authenticated = true
		b.deviceJID = b.client.Store.ID.String()
		b.mu.Unlock()
		log.Printf("bridge connected; device=%s", b.DeviceJID())
		return nil
	}
	return b.loginLoop(ctx)
}

// loginLoop requests QR batches until pairing succeeds or ctx ends. whatsmeow
// emits ~6 rotating codes per batch then a terminal "timeout"; each timeout
// gets a fresh batch after a short pause instead of killing the process (the
// old behavior — users came back to a dead bridge and a stale QR).
func (b *Bridge) loginLoop(ctx context.Context) error {
	b.mu.Lock()
	if b.loginRunning {
		b.mu.Unlock()
		return nil // already looping; idempotent for /api/auth/reconnect
	}
	b.loginRunning = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.loginRunning = false
		b.mu.Unlock()
	}()

	attempt := 0
	for round := 1; ; round++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		qrChan, err := b.client.GetQRChannel(ctx)
		if err != nil {
			// Already-logged-in race (e.g. pairing finished between rounds).
			if b.client.Store.ID != nil {
				return nil
			}
			return fmt.Errorf("qr channel: %w", err)
		}
		if err := b.client.Connect(); err != nil {
			return fmt.Errorf("connect for pairing: %w", err)
		}
		if round == 1 {
			log.Println("waiting for pairing: scan the QR (WhatsApp > Settings > Linked Devices > Link a Device) or POST /api/auth/pair-phone for a typed code")
		}

		batchDone := false
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				attempt++
				b.mu.Lock()
				b.currentQR = evt.Code
				b.qrExpiresAt = time.Now().Add(evt.Timeout)
				if b.authState != AuthStatePairingPending {
					b.authState = AuthStateQRPending
				}
				pairingPending := b.authState == AuthStatePairingPending
				wantPair := b.pairPhoneOnStart
				b.mu.Unlock()
				// --pair-phone: the socket is live once codes flow, so this is
				// the earliest safe moment to request the typed code. Once.
				if wantPair != "" && !pairingPending {
					go func() {
						if _, err := b.RequestPairingCode(b.rootCtx, wantPair); err != nil {
							log.Printf("--pair-phone failed (%v); falling back to QR scanning", err)
							b.mu.Lock()
							b.pairPhoneOnStart = ""
							b.mu.Unlock()
						}
					}()
				}
				// While a typed pairing code is pending (or requested via
				// --pair-phone), keep the code on screen instead of stomping
				// it with QR art. The rotating QR is still served over
				// /api/auth/qr either way.
				if !pairingPending && wantPair == "" {
					renderQRToTerminal(evt.Code, attempt, evt.Timeout)
				}
			case "success":
				b.mu.Lock()
				b.currentQR = ""
				b.pairingCode = ""
				b.authState = AuthStatePaired
				b.mu.Unlock()
				log.Println("pairing success")
				return nil
			case "timeout":
				b.setAuthState(AuthStateTimedOut)
				b.mu.Lock()
				b.currentQR = ""
				b.mu.Unlock()
				log.Printf("QR batch expired (round %d); requesting a fresh batch in 5s — leave this running", round)
				batchDone = true
			default:
				if evt.Error != nil {
					log.Printf("pairing event: %s (%v)", evt.Event, evt.Error)
				} else {
					log.Printf("pairing event: %s", evt.Event)
				}
			}
			if batchDone {
				break
			}
		}

		if b.client.Store.ID != nil {
			// Paired via a path that closed the channel without "success"
			// (e.g. pairing code typed on the phone).
			b.setAuthState(AuthStatePaired)
			return nil
		}

		// Fresh batch requires a clean socket.
		b.client.Disconnect()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// SetPairPhoneOnStart arms the --pair-phone flow: the login loop requests a
// typed pairing code for this number as soon as the pairing socket is live.
func (b *Bridge) SetPairPhoneOnStart(phone string) {
	b.mu.Lock()
	b.pairPhoneOnStart = phone
	b.mu.Unlock()
}

var nonDigits = regexp.MustCompile(`\D`)

// RequestPairingCode wraps whatsmeow's PairPhone: the user types an 8-char
// code on the phone instead of scanning a QR. Works identically on Android
// and iOS ("Link with phone number instead"). Only valid while the login
// loop has an active pairing socket.
func (b *Bridge) RequestPairingCode(ctx context.Context, phone string) (string, error) {
	if b.client.Store.ID != nil {
		return "", fmt.Errorf("already paired as %s", b.client.Store.ID.String())
	}
	b.mu.RLock()
	running := b.loginRunning
	b.mu.RUnlock()
	if !running {
		return "", fmt.Errorf("login is not active; POST /api/auth/reconnect first")
	}
	digits := nonDigits.ReplaceAllString(phone, "")
	if len(digits) < 7 {
		return "", fmt.Errorf("phone_number must include country code, e.g. +15551234567")
	}
	code, err := b.client.PairPhone(ctx, digits, true, whatsmeow.PairClientChrome, "Chrome (whatsapp-mcp)")
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.pairingCode = code
	b.authState = AuthStatePairingPending
	b.mu.Unlock()
	log.Printf("pairing code issued; enter it on the phone: WhatsApp > Linked Devices > Link a Device > Link with phone number instead")
	fmt.Printf("\n  Pairing code: %s\n  On your phone: Settings > Linked Devices > Link a Device > \"Link with phone number instead\"\n\n", FormatPairingCode(code))
	return code, nil
}

// FormatPairingCode renders an 8-char pairing code as ABCD-EFGH for humans.
func FormatPairingCode(code string) string {
	c := strings.ReplaceAll(code, "-", "")
	if len(c) <= 4 {
		return c
	}
	var parts []string
	for i := 0; i < len(c); i += 4 {
		end := i + 4
		if end > len(c) {
			end = len(c)
		}
		parts = append(parts, c[i:end])
	}
	return strings.Join(parts, "-")
}

// Reconnect is the /api/auth/reconnect implementation. Paired → ensure the
// socket is up. Unpaired (incl. after a WhatsApp-side logout) → ensure the
// login loop is running so /api/auth/qr serves fresh codes. Idempotent.
func (b *Bridge) Reconnect(ctx context.Context) (AuthSnapshot, error) {
	if b.client.Store.ID != nil {
		if !b.client.IsConnected() {
			if err := b.client.Connect(); err != nil {
				return b.AuthSnapshot(), fmt.Errorf("reconnect: %w", err)
			}
		}
		b.setAuthState(AuthStatePaired)
		return b.AuthSnapshot(), nil
	}
	b.mu.RLock()
	running := b.loginRunning
	b.mu.RUnlock()
	if !running {
		go func() {
			if err := b.loginLoop(b.rootCtx); err != nil && b.rootCtx.Err() == nil {
				log.Printf("login loop ended: %v", err)
			}
		}()
	}
	return b.AuthSnapshot(), nil
}
