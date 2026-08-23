package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// GetOrCreateDBKey returns a hex-encoded 256-bit key for SQLCipher.
// On first run, a random key is generated and stored in the platform secret
// store. On subsequent runs, the existing key is returned. The user is
// prompted by the OS when the store is first accessed (macOS).
//
// macOS: Keychain via `security`. Linux: libsecret via `secret-tool`.
// Windows: Credential Manager via advapi32 (keychain_windows.go).
//
// An explicit WHATSAPP_DB_KEY env var overrides all of these — main.go checks
// it BEFORE calling here (the escape hatch for headless/CI/custom setups, and
// the recovery path when the secret store cannot be read).
//
// Every platform read is CLASSIFIED (keyReadResult): a fresh key is minted
// ONLY on a proven not-found. A read that fails for any other reason — locked
// keychain, no D-Bus session, revoked access — means the key may still exist,
// so the bridge fails loud and writes nothing (MYC-3694). dbPath is the
// message-store file: even on a proven not-found, minting is refused while a
// non-empty store exists, because a fresh key can never decrypt it.
//
// The key is never logged, never written to disk outside the platform secret store,
// never committed to the repo.
func GetOrCreateDBKey(service, account, dbPath string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return getOrCreateDBKeyMacOS(service, account, dbPath)
	case "linux":
		return getOrCreateDBKeyLinux(service, account, dbPath)
	case "windows":
		return getOrCreateDBKeyWindows(service, account, dbPath)
	default:
		return "", fmt.Errorf("no secret store for %s; set WHATSAPP_DB_KEY (64 hex chars) directly", runtime.GOOS)
	}
}

// keyReadResult classifies a secret-store read. Three states, not two: the
// old `err != nil → mint` flow turned a TRANSIENT read failure (locked
// keychain, no session bus) into a fresh key written over the real one,
// permanently orphaning the encrypted message store (MYC-3694).
type keyReadResult int

const (
	keyReadOK       keyReadResult = iota // present and readable — use it
	keyReadNotFound                      // store answered definitively: no such item — the ONLY state that may mint
	keyReadError                         // locked / denied / transient — the key may exist; never mint, never write
)

// keyStore is one platform's secret-store binding. The platform supplies a
// classified read and a write; getOrCreate implements the shared contract so
// the mint/refuse decision cannot drift between platforms.
type keyStore struct {
	name   string // human name for errors, e.g. "macOS Keychain"
	remedy string // how the operator recovers a failed read without data loss
	read   func() (key string, res keyReadResult, detail error)
	write  func(key string) error // reachable ONLY after read returned keyReadNotFound
}

func (s keyStore) getOrCreate(dbPath string) (string, error) {
	key, res, detail := s.read()
	switch res {
	case keyReadOK:
		if len(key) == 64 { // 32 bytes hex-encoded
			return key, nil
		}
		// Present but malformed. The old flow minted a replacement here; if
		// an encrypted store exists, that destroys it. Same rule as a failed
		// read: only a proven not-found may mint.
		return "", fmt.Errorf("%s holds a DB key of unexpected length %d (want 64 hex chars); refusing to overwrite it — %s", s.name, len(key), s.remedy)
	case keyReadNotFound:
		if err := storeGuardBeforeMint(s.name, dbPath); err != nil {
			return "", err
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generate random key: %w", err)
		}
		fresh := hex.EncodeToString(b)
		if err := s.write(fresh); err != nil {
			return "", fmt.Errorf("store new DB key in %s: %w", s.name, err)
		}
		return fresh, nil
	default: // keyReadError
		return "", fmt.Errorf("%s: reading the DB key failed (%v); the key may still exist, so a fresh one will NOT be minted over it — %s", s.name, detail, s.remedy)
	}
}

// storeGuardBeforeMint refuses to mint a fresh key while an encrypted message
// store already exists. A populated store whose key is missing (wrong
// account/service env, migrated machine, deleted entry) is a recoverable
// operator problem; pairing it with a fresh key would make the loss permanent.
func storeGuardBeforeMint(storeName, dbPath string) error {
	if dbPath == "" {
		return nil
	}
	fi, err := os.Stat(dbPath)
	switch {
	case err == nil && !fi.IsDir() && fi.Size() > 0:
		return fmt.Errorf("%s has no DB key but an encrypted message store already exists (%s, %d bytes); refusing to mint a fresh key that could never decrypt it — restore the original key via WHATSAPP_DB_KEY, or move the store file aside to start over", storeName, dbPath, fi.Size())
	case err == nil || os.IsNotExist(err):
		return nil // no store yet (or an empty placeholder): a fresh mint is safe
	default:
		return fmt.Errorf("%s has no DB key and the message store at %s cannot be checked (%v); refusing to mint until the store is proven absent — fix the path or set WHATSAPP_DB_KEY", storeName, dbPath, err)
	}
}

// getOrCreateDBKeyMacOS uses the macOS Keychain via `security`.
//
// `security find-generic-password` exit codes, measured (MYC-3694):
//
//	0  — item present and readable: use it.
//	44 — errSecItemNotFound: PROVEN absent. The only state where minting is correct.
//	51 — observed while the login keychain was locked with no UI allowed
//	     (errSecAuthFailed-class; NOT errSecInteractionNotAllowed, which maps to
//	     exit 36): the key EXISTS but cannot be read right now. Minting here
//	     overwrites the real key.
//
// Anything that is not 0 or 44 is treated like 51: fail loud, write nothing.
func getOrCreateDBKeyMacOS(service, account, dbPath string) (string, error) {
	return keyStore{
		name:   "macOS Keychain",
		remedy: "unlock the login keychain (`security unlock-keychain`) and retry, or set WHATSAPP_DB_KEY to the original key",
		read: func() (string, keyReadResult, error) {
			out, err := outputWithTimeout(keychainCmd("security", "find-generic-password",
				"-s", service,
				"-a", account,
				"-w",
			))
			if err == nil {
				return strings.TrimSpace(string(out)), keyReadOK, nil
			}
			if execExitCode(err) == 44 { // errSecItemNotFound
				return "", keyReadNotFound, nil
			}
			return "", keyReadError, execErrDetail(err)
		},
		write: func(key string) error {
			// Create-only on purpose: no -U. This path is reachable only
			// after a PROVEN not-found; if an item appears concurrently,
			// exit 45 (errSecDuplicateItem) fails the write instead of
			// silently overwriting a key we never read.
			return runWithStderr(keychainCmd("security", "add-generic-password",
				"-s", service,
				"-a", account,
				"-w", key,
				"-T", "", // restrict access to this binary path; empty = default ACL
			))
		},
	}.getOrCreate(dbPath)
}

// getOrCreateDBKeyLinux uses libsecret (GNOME Keyring / KDE KWallet) via
// `secret-tool`.
//
// secret-tool's contract (libsecret tool/secret-tool.c): `lookup` prints the
// secret and exits 0 when found; exits 1 SILENTLY on a clean miss; prints an
// error to stderr and exits 1 when the read itself failed (no D-Bus session,
// locked collection, ...). "exit 1 + empty stderr" is therefore the only
// proven not-found; everything else fails loud and writes nothing (MYC-3694).
func getOrCreateDBKeyLinux(service, account, dbPath string) (string, error) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return "", fmt.Errorf("secret-tool (libsecret) not found; install it or set WHATSAPP_DB_KEY directly")
	}
	return keyStore{
		name:   "libsecret",
		remedy: "make sure the session keyring is unlocked and D-Bus is reachable, or set WHATSAPP_DB_KEY to the original key",
		read: func() (string, keyReadResult, error) {
			out, err := outputWithTimeout(keychainCmd("secret-tool", "lookup", "service", service, "account", account))
			if err == nil {
				return strings.TrimSpace(string(out)), keyReadOK, nil
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(ee.Stderr)) == 0 {
				return "", keyReadNotFound, nil
			}
			return "", keyReadError, execErrDetail(err)
		},
		write: func(key string) error {
			// Reachable only after a proven not-found. secret-tool store has
			// no create-only mode; the classification above is the guard
			// against ever storing over an unread key.
			cmd := keychainCmd("secret-tool", "store",
				"--label", fmt.Sprintf("%s db key", service),
				"service", service, "account", account)
			cmd.Stdin = strings.NewReader(key)
			return runWithStderr(cmd)
		},
	}.getOrCreate(dbPath)
}

// execExitCode extracts the process exit code, or -1 when the command did not
// run to completion (binary missing, signalled, ...).
func execExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// execErrDetail renders an exec failure with its exit code and stderr so the
// operator can see WHY the read failed (locked keychain, no D-Bus, missing
// binary). Output() populates ExitError.Stderr when cmd.Stderr is nil.
func execErrDetail(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return fmt.Errorf("exit %d: %s", ee.ExitCode(), msg)
		}
		return fmt.Errorf("exit %d", ee.ExitCode())
	}
	return err
}

// runWithStderr runs cmd and, on failure, folds its output into the error.
// Only used for secret-store writes, which never echo the secret.
func runWithStderr(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		if tErr := timeoutError(cmd, err); tErr != nil {
			return tErr
		}
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w (%s)", err, msg)
		}
		return err
	}
	return nil
}

// keychainTimeout bounds every secret-store subprocess.
//
// Nothing bounded these before, and the failure that exposed it was not
// theoretical: on a headless macOS runner `security add-generic-password`
// BLOCKED — the test harness killed it after 9m56s, stack parked in
// syscall.Wait4. A secret-store CLI that decides it needs user authorization
// waits for a human forever, and because this runs during startup the whole
// bridge hangs before it ever listens, with nothing in the log to say why.
//
// The window is generous on purpose: on a desktop Mac an authorization dialog
// is a legitimate reason to wait, and a human needs time to click it. It is
// bounded so that headless installs (launchd, systemd, containers, CI) get a
// named error and the WHATSAPP_DB_KEY escape hatch instead of a silent hang.
// Override with WHATSAPP_KEYCHAIN_TIMEOUT_SECONDS; tests set it low.
var keychainTimeout = time.Duration(getenvInt("WHATSAPP_KEYCHAIN_TIMEOUT_SECONDS", 120)) * time.Second

// keychainCmd builds a secret-store command whose context expires after
// keychainTimeout. The context is kept alongside the Cmd so the failure can
// be classified afterwards: ctx.Err() == DeadlineExceeded is the ONLY reliable
// "it never answered" signal. ProcessState is not — a killed process still
// reports an exit status, and a binary that does not exist never produces a
// ProcessState at all, so classifying on it mislabels both.
func keychainCmd(name string, args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	cmd := exec.CommandContext(ctx, name, args...)
	// Make the kill effective even if the child ignores the first signal.
	cmd.WaitDelay = 5 * time.Second
	keychainCtxs.Store(cmd, &keychainCtx{ctx: ctx, cancel: cancel})
	return cmd
}

type keychainCtx struct {
	ctx    context.Context
	cancel context.CancelFunc
}

var keychainCtxs sync.Map // *exec.Cmd -> *keychainCtx

// releaseKeychainCtx drops the Cmd's context and reports whether the deadline
// was what ended it.
func releaseKeychainCtx(cmd *exec.Cmd) (deadlineExceeded bool) {
	v, ok := keychainCtxs.LoadAndDelete(cmd)
	if !ok {
		return false
	}
	kc := v.(*keychainCtx)
	defer kc.cancel()
	return errors.Is(kc.ctx.Err(), context.DeadlineExceeded)
}

// timeoutError converts "we killed it because the clock ran out" into an
// actionable message, and returns nil for every other kind of failure so the
// caller's own error handling still applies.
func timeoutError(cmd *exec.Cmd, _ error) error {
	if !releaseKeychainCtx(cmd) {
		return nil
	}
	return fmt.Errorf(
		"%s did not respond within %s and was killed — the secret store is most likely "+
			"waiting on an authorization prompt that nothing can answer here. Set "+
			"WHATSAPP_DB_KEY to the 64-hex-char key to bypass the platform store, or run "+
			"this once from an interactive desktop session so the prompt can be approved "+
			"(raise WHATSAPP_KEYCHAIN_TIMEOUT_SECONDS if you need longer)",
		cmd.Path, keychainTimeout)
}

// outputWithTimeout is Output() with the same timeout classification.
func outputWithTimeout(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if err != nil {
		if tErr := timeoutError(cmd, err); tErr != nil {
			return nil, tErr
		}
		return out, err
	}
	releaseKeychainCtx(cmd)
	return out, nil
}
