package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
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

// connectPaired dials WhatsApp with the persisted identity and publishes the
// resulting state under one lock. Every paired-path caller (RunAuth, Reconnect,
// the watchdog) goes through it so they cannot leave the bridge in DIFFERENT
// states — which is exactly what used to happen: RunAuth set authenticated,
// Reconnect did not, so a bridge recovered via /api/auth/reconnect reported
// connected=true and then silently refused every send and presence call,
// because Bridge.IsConnected is `connected && authenticated`.
//
// Publishing state when the socket is already up is deliberate, not a no-op:
// it is what repairs a stale authenticated=false left behind by an older
// recovery path.
func (b *Bridge) connectPaired() error {
	if b.client.Store.ID == nil {
		return errors.New("connectPaired: no persisted device identity")
	}
	if !b.client.IsConnected() {
		// ErrAlreadyConnected means whatsmeow's own auto-reconnect won the race
		// between the IsConnected check above and this dial. That is a success
		// for our purposes — a socket exists — and treating it as a failure
		// would skip the state publish below and leave the send gate shut for
		// another watchdog tick. This network drops the socket every few
		// minutes, so the race is routine, not theoretical.
		if err := b.client.Connect(); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return err
		}
	}
	b.markPairedLive(b.client.Store.ID.String())
	return nil
}

// markPairedLive publishes "socket up, device known" as one atomic update.
// Split out from connectPaired so the state half is reachable in tests without
// a live whatsmeow client — this is the half that was wrong, and asserting it
// needs no network.
//
// connected and authenticated MUST move together: Bridge.IsConnected (the gate
// on sends, presence and history sync) is their AND, so setting one without the
// other produces a bridge that looks online everywhere except where it counts.
func (b *Bridge) markPairedLive(deviceJID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connected = true
	b.authenticated = true
	b.deviceJID = deviceJID
	b.authState = AuthStatePaired
}

// needsRedial is the watchdog's per-tick decision, split from the loop so the
// truth table is testable without a client or a ticker.
//
// `paired` is the caller's read of the persisted device identity. Unpaired means
// the QR loop owns the client and a redial from here would drop the socket a
// human scan is already using; loginRunning means the same for an in-flight
// round. Otherwise anything short of fully live is worth a dial, INCLUDING
// connected-but-not-authenticated — that is the stale state an older recovery
// path left behind, and it silently refuses every send.
func (b *Bridge) needsRedial(paired bool) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return paired && !b.loginRunning && !(b.connected && b.authenticated)
}

// connectPairedWithRetry redials with capped exponential backoff until it wins
// or ctx ends.
//
// A single unretried attempt here caused a 20-hour silent outage: launchd's
// RunAtLoad started the bridge before DNS was up at boot, Connect failed with
// `lookup web.whatsapp.com: no such host`, RunAuth returned the error, main
// logged one line, and its goroutine exited. Nothing redialled afterwards —
// whatsmeow's auto-reconnect only covers a socket that was ESTABLISHED and then
// dropped, and this one never came up. Meanwhile setAuthState(paired) had
// already run and the process stays alive on purpose to keep the HTTP API
// serving, so /api/status read `auth_state: paired` and launchd's KeepAlive saw
// a perfectly healthy job. Nobody was watching the one field that was false.
func (b *Bridge) connectPairedWithRetry(ctx context.Context) error {
	const (
		initialBackoff = 5 * time.Second
		maxBackoff     = 2 * time.Minute
	)
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		err := b.connectPaired()
		if err == nil {
			if attempt > 1 {
				log.Printf("reconnect: connected on attempt %d", attempt)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("reconnect: attempt %d failed: %v — retrying in %s", attempt, err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// RunConnectionWatchdog redials whenever a paired bridge is found with no live
// socket. It is the backstop for every way this process can stay alive while
// being functionally offline — a dial that never succeeded, or a state where
// whatsmeow's own auto-reconnect has stopped trying.
//
// Process supervision cannot cover this and never could: launchd only knows
// whether the process is running, and the process deliberately stays running
// while offline so the HTTP API can serve /api/auth/*. Liveness is not
// readiness, so readiness needs its own loop.
func (b *Bridge) RunConnectionWatchdog(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !b.needsRedial(b.client.Store.ID != nil) {
			continue
		}
		log.Println("watchdog: paired but not live — redialling")
		if err := b.connectPaired(); err != nil {
			log.Printf("watchdog: redial failed: %v — retrying in %s", err, interval)
			continue
		}
		log.Printf("watchdog: reconnected; device=%s", b.DeviceJID())
	}
}

// RunAuth drives authentication to completion. Returning user: reconnect with
// the persisted identity, retrying until it succeeds — a boot-time network race
// must not cost the session. First run (or post-logout): run the QR/pairing-code
// login loop until paired or ctx is cancelled. Blocking; main() runs it in a
// goroutine so the HTTP API is up during pairing (a supervisor needs
// /api/status and /api/auth/* exactly then).
func (b *Bridge) RunAuth(ctx context.Context) error {
	if b.client.Store.ID != nil {
		// Returning user: reconnect with persisted identity.
		b.setAuthState(AuthStatePaired)
		if err := b.connectPairedWithRetry(ctx); err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
		log.Printf("bridge connected; device=%s", b.DeviceJID())
		return nil
	}
	return b.loginLoop(ctx)
}

// loginLoop requests QR batches until pairing succeeds or ctx ends. whatsmeow
// emits ~6 rotating codes per batch then a terminal "timeout"; each timeout
// gets a fresh batch after a short pause instead of killing the process (the
// old behavior — users came back to a dead bridge and a stale QR).
//
// Do NOT wrap the Connect below in a bounded ConnectContext. That was tried,
// and it broke pairing outright while every test stayed green.
//
// ConnectContext's ctx is the CONNECTION's lifetime context, not a handshake
// deadline. whatsmeow stores it as framesocket.parentCtx, derives cancelCtx
// from it for every outbound write, and passes it to readPump -> conn.Read(ctx)
// — where coder/websocket registers context.AfterFunc(ctx, close) per read, so
// cancelling HARD-CLOSES the socket. It also hands that ctx to
// handlerQueueLoop, which passes it to every node handler, including the
// pair-device and pair-success handlers whose Store.Save(ctx) is a ctx-bound
// DB write.
//
// Cancelling it right after ConnectContext returns therefore kills the socket
// ~10ms later, before the <iq> carrying the QR refs arrives one RTT away. Not
// one QR code is ever emitted: the channel sees events.Disconnected, qrchan
// maps that to QRChannelTimeout, and this loop logs "QR batch expired" every
// 5s forever. Deferring the cancel to the end of the round does not help — a
// QR batch is 60s + 5x20s = 160s, so any bound short enough to act as a
// handshake timeout kills a legitimate human scan.
//
// A bound is not needed: whatsmeow already caps this step internally (30s
// dial, 10s TLS, NoiseHandshakeResponseTimeout = 20s).
func (b *Bridge) loginLoop(ctx context.Context) (err error) {
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

	// Checked here rather than at each call site. loginLoop has THREE callers
	// (RunAuth, the LoggedOut handler in bridge.go, Reconnect) and it returns
	// nil to a caller when another goroutine already owns the loop — so a
	// per-caller check is reachable only by whichever goroutine happens to own
	// it, and after a restart that is RunAuth's, which had no check at all. One
	// check at the source covers all three unconditionally.
	defer func() { b.fatalIfDeviceDeleted(err) }()

	attempt := 0
	for round := 1; ; round++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		qrChan, qrErr := b.client.GetQRChannel(ctx)
		if qrErr != nil {
			// Returning nil here is correct ONLY for the genuine race where
			// pairing completed between rounds — which means paired AND live.
			// The old code checked Store.ID alone, and that is how a forced
			// logout produced two days of total silence: Store.Delete had not
			// landed yet, so Store.ID was still set, GetQRChannel returned
			// ErrQRStoreContainsID, and this reported SUCCESS to a caller that
			// then logged nothing at all.
			if b.client.Store.ID != nil && b.client.IsConnected() {
				b.setAuthState(AuthStatePaired)
				return nil
			}
			// A socket from the dead session is still up, which is why
			// GetQRChannel refused. Drop it and take a fresh round rather than
			// abandoning the loop.
			if errors.Is(qrErr, whatsmeow.ErrQRAlreadyConnected) {
				log.Printf("pairing round %d: a stale socket was still connected; dropping it and retrying", round)
				b.client.Disconnect()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
				}
				continue
			}
			return fmt.Errorf("qr channel: %w", qrErr)
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

// isDeviceDeleted reports whether err means b.client itself is permanently
// unusable — split out from exitIfDeviceDeleted so the matching logic is
// testable without actually exiting the test binary.
//
// store.ErrDeviceDeleted is whatsmeow's own sentinel (store/store.go): once
// Store.Delete has run, Store.Deleted latches true and every subsequent call on
// this *whatsmeow.Client — Connect, GetQRChannel, all of it — fails the same
// way. There is no reset; whatsmeow's own fix is a brand new device, and the
// only place that constructs one is main.go's container.GetFirstDevice at
// startup. Restarting the process is that fix, applied without rearchitecting
// every direct b.client read across the codebase (sends.go, presence.go,
// backfill.go, history_sync.go, media_download.go) into a mutex-guarded
// accessor just to support swapping the client in place.
//
// Any OTHER loginLoop error (a slow network, a bad QR scan, the ordinary batch
// timeout) does not match the sentinel and falls through untouched — this must
// stay narrow, or a transient failure would restart the bridge for no reason.
func isDeviceDeleted(err error) bool {
	return err != nil && errors.Is(err, store.ErrDeviceDeleted)
}

// fatalIfDeviceDeleted asks for a process restart when loginLoop's error means
// b.client is permanently unusable, so the supervisor in
// whatsapp-autostart/start-bridge.cmd relaunches with a fresh device instead of
// the loop sitting logged-out forever.
//
// It REQUESTS a shutdown rather than calling os.Exit. os.Exit would skip every
// deferred db.Close/transcriber.Close/bridge.Disconnect in main and kill
// in-flight HTTP handlers mid-write — including a confirm that has already
// delivered a message to WhatsApp but not yet recorded it, which is exactly the
// divergence sends_reconcile.go exists to clean up. Going through main's normal
// shutdown means those defers run and the HTTP server drains first.
func (b *Bridge) fatalIfDeviceDeleted(err error) {
	if !isDeviceDeleted(err) {
		return
	}
	log.Printf("FATAL: %v — this device's WhatsApp session was deleted (logged out from another device) and cannot be reused; shutting down so the supervisor restarts with a fresh device (see whatsapp-autostart/start-bridge.cmd)", err)
	b.requestFatalShutdown()
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
		if err := b.connectPaired(); err != nil {
			return b.AuthSnapshot(), fmt.Errorf("reconnect: %w", err)
		}
		return b.AuthSnapshot(), nil
	}
	b.mu.RLock()
	running := b.loginRunning
	b.mu.RUnlock()
	if !running {
		go func() {
			// No exitIfDeviceDeleted here: loginLoop checks it internally for
			// all three of its callers, including the ones this short-circuits.
			if err := b.loginLoop(b.rootCtx); err != nil && b.rootCtx.Err() == nil {
				log.Printf("login loop ended: %v", err)
			}
		}()
	}
	return b.AuthSnapshot(), nil
}
