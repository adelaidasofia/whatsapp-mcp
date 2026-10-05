//go:build windows

package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/sys/windows"
)

// useVTConsoleAsStdout points os.Stdout at this process's real console screen
// buffer with VT processing on: what a VS Code terminal, PowerShell 7 or any
// ConPTY host hands the bridge. A CI step's stdout is a pipe, but the step
// normally still has a console behind it, reachable as CONOUT$; a process
// without one gets one.
func useVTConsoleAsStdout(t *testing.T) {
	t.Helper()
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	if r, _, _ := kernel32.NewProc("AllocConsole").Call(); r != 0 {
		freeConsole := kernel32.NewProc("FreeConsole")
		t.Cleanup(func() { freeConsole.Call() })
	}
	name, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("open CONOUT$: %v; without a console this test cannot reproduce anything", err)
	}
	var orig uint32
	if err := windows.GetConsoleMode(h, &orig); err != nil {
		windows.CloseHandle(h)
		t.Fatalf("GetConsoleMode(CONOUT$): %v", err)
	}
	if err := windows.SetConsoleMode(h, orig|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		windows.CloseHandle(h)
		t.Fatalf("turn on VT processing for CONOUT$: %v", err)
	}
	console := os.NewFile(uintptr(h), "CONOUT$")
	prev := os.Stdout
	os.Stdout = console
	t.Cleanup(func() {
		os.Stdout = prev
		windows.SetConsoleMode(h, orig)
		console.Close()
	})
}

// panicValueWithin runs f on its own goroutine and returns what it panicked
// with, or nil. The limit turns a hang into a failure instead of a CI timeout.
func panicValueWithin(t *testing.T, limit time.Duration, f func()) any {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		f()
	}()
	select {
	case v := <-done:
		return v
	case <-time.After(limit):
		t.Fatalf("still running after %s", limit)
		return nil
	}
}

// The field crash, rebuilt on the windows-latest runner: a real console with VT
// processing on and WT_SESSION empty, the one combination where go-colorable
// returns os.Stdout itself and qrterminal's sixel probe gets far enough to
// panic (see qrterminalProbes in qr_render_test.go).
func TestRenderQRDrawsOnAVTConsoleOutsideWindowsTerminal(t *testing.T) {
	useVTConsoleAsStdout(t)
	t.Setenv("WT_SESSION", "")

	// Preconditions, asserted: on a pipe or a legacy console this test would
	// pass without reproducing anything.
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		t.Fatal("precondition: os.Stdout is not a console")
	}
	if w := colorable.NewColorableStdout(); w != io.Writer(os.Stdout) {
		t.Fatalf("precondition: go-colorable wrapped stdout (%T), so the probe would never run", w)
	}

	// Control: the upstream helper still panics on this console. If it stops,
	// qrterminal or x/term fixed it upstream; the bridge stays correct, but this
	// test no longer proves it fixes anything, so it fails to say so.
	v := panicValueWithin(t, 10*time.Second, func() { qrterminal.Generate("control", qrterminal.L, os.Stdout) })
	if v == nil {
		t.Fatal("control: qrterminal.Generate drew without panicking; the field crash no longer reproduces here")
	}
	t.Logf("control panicked the way the field report did: %v", v)

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	if v := panicValueWithin(t, 10*time.Second, func() { renderQRToTerminal("2@code", 1, 20*time.Second) }); v != nil {
		t.Fatalf("renderQRToTerminal panicked: %v", v)
	}
	if strings.Contains(logs.String(), "could not draw") {
		t.Fatalf("renderQRToTerminal fell back to the log line instead of drawing:\n%s", logs.String())
	}
}
