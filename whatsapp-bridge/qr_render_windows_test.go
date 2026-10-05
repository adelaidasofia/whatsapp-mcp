//go:build windows

package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/sys/windows"
)

var kernel32 = windows.NewLazySystemDLL("kernel32.dll")

// useVTConsoleAsStdout points os.Stdout at this process's real console screen
// buffer with VT processing on, the kind of console the field report came
// from, and returns its handle. A CI step's stdout is a pipe, but the step
// normally still has a console behind it, reachable as CONOUT$; a process
// without one gets one.
func useVTConsoleAsStdout(t *testing.T) windows.Handle {
	t.Helper()
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
		// Close waits for every reader of the handle to let go, and a control
		// that hung is still reading it. On a failed run, leak the handle so
		// the test can end instead of blocking until the go test timeout.
		if !t.Failed() {
			console.Close()
		}
	})
	t.Cleanup(func() {
		// The sixel probe asks the terminal for its attributes and the console
		// queues the answer as INPUT. Drop it, so a run in a developer's own
		// console does not type it at their next prompt.
		in, err := windows.UTF16PtrFromString("CONIN$")
		if err != nil {
			return
		}
		ih, err := windows.CreateFile(in, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			return
		}
		windows.FlushConsoleInputBuffer(ih)
		windows.CloseHandle(ih)
	})
	return h
}

// consoleRowsAboveCursor reads back the last rows of the console screen buffer
// behind h, ending at the cursor, so a test can see what reached the console.
// One row per call keeps each read small on any Windows version.
func consoleRowsAboveCursor(t *testing.T, h windows.Handle, rows int) string {
	t.Helper()
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(h, &info); err != nil {
		t.Fatalf("GetConsoleScreenBufferInfo: %v", err)
	}
	width := int(info.Size.X)
	last := int(info.CursorPosition.Y)
	first := max(last-rows, 0)
	readChars := kernel32.NewProc("ReadConsoleOutputCharacterW")
	line := make([]uint16, width)
	var sb strings.Builder
	for y := first; y <= last; y++ {
		var read uint32
		// COORD{X: 0, Y: y} travels by value, packed into one word.
		at := uintptr(uint32(uint16(y)) << 16)
		if r, _, err := readChars.Call(uintptr(h), uintptr(unsafe.Pointer(&line[0])), uintptr(width), at, uintptr(unsafe.Pointer(&read))); r == 0 {
			t.Fatalf("ReadConsoleOutputCharacterW row %d: %v", y, err)
		}
		sb.WriteString(string(utf16.Decode(line[:read])))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// panicValueWithin runs f on its own goroutine and returns what it panicked
// with, or nil. The limit turns a hang into a failure instead of a CI timeout
// (the console cleanup above is what lets a hung run actually finish).
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
	h := useVTConsoleAsStdout(t)
	t.Setenv("WT_SESSION", "")

	// Preconditions, asserted: on a pipe or a legacy console this test would
	// pass without reproducing anything.
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		t.Fatal("precondition: os.Stdout is not a console")
	}
	if w := colorable.NewColorableStdout(); w != io.Writer(os.Stdout) {
		t.Fatalf("precondition: go-colorable wrapped stdout (%T), so the probe would never run", w)
	}

	// Control: the upstream helper still panics on this console, the same way
	// the field report did. If it stops, qrterminal or x/term fixed it
	// upstream; the bridge stays correct, but this test no longer proves it
	// fixes anything, so it fails to say so.
	v := panicValueWithin(t, 10*time.Second, func() { qrterminal.Generate("control", qrterminal.L, os.Stdout) })
	if err, ok := v.(runtime.Error); !ok || !strings.Contains(err.Error(), "nil pointer dereference") {
		t.Fatalf("control: qrterminal.Generate ended with %v (%T), not the field report's nil pointer dereference", v, v)
	}

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	// An attempt number nothing else prints, so finding it in the console
	// buffer means this draw reached the console, end to end.
	const attempt = 7193
	if v := panicValueWithin(t, 10*time.Second, func() { renderQRToTerminal("2@code", attempt, 20*time.Second) }); v != nil {
		t.Fatalf("renderQRToTerminal panicked: %v", v)
	}
	if strings.Contains(logs.String(), "could not draw") {
		t.Fatalf("renderQRToTerminal fell back to the log line instead of drawing:\n%s", logs.String())
	}
	screen := consoleRowsAboveCursor(t, h, 80)
	for _, want := range []string{"QR #7193", "--pair-phone"} {
		if !strings.Contains(screen, want) {
			t.Errorf("console is missing %q after the draw; last rows:\n%s", want, screen)
		}
	}
}
