package main

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sleepCmd names a command that blocks for roughly the given seconds on this
// platform, so the timeout plumbing can be exercised on every CI leg rather
// than only the one where the original hang happened to surface.
func sleepArgs(seconds int) (string, []string) {
	if runtime.GOOS == "windows" {
		// ping to loopback with N+1 echoes waits ~N seconds and needs no
		// shell. timeout.exe refuses to run without a console, so not that.
		return "ping", []string{"-n", strconv.Itoa(seconds + 1), "127.0.0.1"}
	}
	return "sleep", []string{strconv.Itoa(seconds)}
}

// A secret-store binary that never answers must be killed and reported, not
// waited on forever. Before keychainTimeout existed, `security
// add-generic-password` blocked for 9m56s on a headless macOS runner and the
// only thing that stopped it was the test harness's own panic.
func TestKeychainCommandTimesOutInsteadOfHanging(t *testing.T) {
	restore := keychainTimeout
	keychainTimeout = 1 * time.Second
	t.Cleanup(func() { keychainTimeout = restore })

	name, args := sleepArgs(30)
	start := time.Now()
	err := runWithStderr(keychainCmd(name, args...))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a timeout error, got nil after %s", elapsed)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("took %s to give up on a 1s timeout — the command was not killed", elapsed)
	}
	if !strings.Contains(err.Error(), "did not respond within") {
		t.Fatalf("killed on time but not classified as a timeout, so the operator gets no "+
			"actionable message: %v", err)
	}
	t.Logf("gave up after %s with: %v", elapsed, err)
}

// The same bound must apply to the read path, which is the one that runs on
// EVERY boot (the write only runs once, on first run).
func TestKeychainOutputTimesOutInsteadOfHanging(t *testing.T) {
	restore := keychainTimeout
	keychainTimeout = 1 * time.Second
	t.Cleanup(func() { keychainTimeout = restore })

	name, args := sleepArgs(30)
	start := time.Now()
	_, err := outputWithTimeout(keychainCmd(name, args...))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a timeout error, got nil after %s", elapsed)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("took %s to give up on a 1s timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "did not respond within") {
		t.Fatalf("read path killed on time but not classified as a timeout: %v", err)
	}
}

// A command that answers normally must NOT be reported as a timeout — the
// classification has to distinguish "the store said no" from "the store never
// answered", or every real error would be mislabelled.
func TestKeychainFastFailureIsNotReportedAsTimeout(t *testing.T) {
	restore := keychainTimeout
	keychainTimeout = 30 * time.Second
	t.Cleanup(func() { keychainTimeout = restore })

	// A binary that does not exist fails immediately.
	err := runWithStderr(keychainCmd("definitely-not-a-real-binary-xyzzy"))
	if err == nil {
		t.Fatalf("expected an error for a missing binary")
	}
	if strings.Contains(err.Error(), "did not respond within") {
		t.Fatalf("a missing binary was misreported as a timeout: %v", err)
	}
}
