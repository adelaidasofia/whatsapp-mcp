package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/mdp/qrterminal/v3"
)

// renderQRToTerminal draws a rotated QR in place. Four rules learned from
// users getting stuck:
//
//  1. Clear before each draw — appending rotated QRs stacks them and people
//     scan a dead one ("the QR is static").
//  2. Half-block glyphs (▀▄█) mojibake under legacy Windows console
//     codepages (cp437/cp850) into an unscannable grid. Outside Windows
//     Terminal, render with ANSI-colored spaces through go-colorable, which
//     translates ANSI to console API calls on legacy conhost.
//  3. Not a TTY (supervised run): no ANSI art at all — one parseable log
//     line; the supervisor renders the code itself from /api/auth/qr.
//  4. A draw must never take the bridge down. This runs on the login loop's
//     goroutine, so a panic here kills the whole process mid-pairing; see
//     drawQR and qrConfig.
func renderQRToTerminal(code string, attempt int, expiresIn time.Duration) {
	if !isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd()) {
		logQRRotated(attempt, expiresIn)
		return
	}

	var w io.Writer = os.Stdout
	if runtime.GOOS == "windows" {
		w = colorable.NewColorableStdout()
	}
	drawQR(w, code, attempt, expiresIn, runtime.GOOS, os.Getenv("WT_SESSION") != "")
}

// drawQR is renderQRToTerminal after the environment reads, so tests can drive
// every branch. A panic while drawing is logged with its stack and the run
// carries on: the code is still served at /api/auth/qr, and the login loop
// that called this has to keep rotating it.
func drawQR(w io.Writer, code string, attempt int, expiresIn time.Duration, goos string, windowsTerminal bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("auth: could not draw the QR in this console: %v\n%s", r, debug.Stack())
			logQRRotated(attempt, expiresIn)
		}
	}()

	clearTerminal(w)
	fmt.Fprintln(w, "────────────────────────────────────────────────────────")
	fmt.Fprintln(w, "  whatsapp-mcp — Scan to connect")
	fmt.Fprintln(w, "────────────────────────────────────────────────────────")
	fmt.Fprintln(w, "  On your phone:")
	fmt.Fprintln(w, "  WhatsApp › Settings › Linked Devices › Link a Device")
	fmt.Fprintln(w)

	qrterminal.GenerateWithConfig(code, qrConfig(w, goos, windowsTerminal))

	fmt.Fprintf(w, "\n  QR #%d · expires in ~%ds · refreshes here automatically — always scan the one on screen\n",
		attempt, int(expiresIn.Seconds()))
	if goos == "windows" && !windowsTerminal {
		fmt.Fprintln(w, "  Garbled square? Use Windows Terminal, or restart the bridge with")
		fmt.Fprintln(w, "  --pair-phone +15551234567 (your own number) to pair by typed code instead.")
	}
}

// qrConfig picks the glyphs for one draw, always as an explicit Config.
// qrterminal.Generate would first probe the terminal for sixel support, and on
// a Windows console that probe panics: with VT processing on, go-colorable
// returns os.Stdout itself, so the probe calls term.MakeRaw on the OUTPUT
// handle; MakeRaw asks for an input-only mode, fails and returns nil, and the
// probe's already-deferred term.Restore(fd, nil) dereferences it (x/term
// term_windows.go:47). The two configs are what Generate builds minus the
// probe, and what GenerateHalfBlock builds.
func qrConfig(w io.Writer, goos string, windowsTerminal bool) qrterminal.Config {
	if goos == "windows" && !windowsTerminal {
		// ANSI-colored full blocks: wider but codepage-proof.
		return qrterminal.Config{
			Level:     qrterminal.L,
			Writer:    w,
			BlackChar: qrterminal.BLACK,
			WhiteChar: qrterminal.WHITE,
			QuietZone: qrterminal.QUIET_ZONE,
		}
	}
	return qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         w,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      qrterminal.QUIET_ZONE,
	}
}

// logQRRotated is the one parseable line a run with no drawable console gets
// per rotation.
func logQRRotated(attempt int, expiresIn time.Duration) {
	log.Printf("auth: QR rotated (attempt %d, expires in ~%ds); GET /api/auth/qr for the raw code", attempt, int(expiresIn.Seconds()))
}

// clearTerminal clears the screen + homes the cursor via ANSI, which
// go-colorable translates for legacy Windows consoles.
func clearTerminal(w io.Writer) {
	fmt.Fprint(w, "\x1b[2J\x1b[H")
}
