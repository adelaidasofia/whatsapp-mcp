package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/mdp/qrterminal/v3"
)

// renderQRToTerminal draws a rotated QR in place. Three rules learned from
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
func renderQRToTerminal(code string, attempt int, expiresIn time.Duration) {
	if !isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd()) {
		log.Printf("auth: QR rotated (attempt %d, expires in ~%ds); GET /api/auth/qr for the raw code", attempt, int(expiresIn.Seconds()))
		return
	}

	useHalfBlocks := runtime.GOOS != "windows" || os.Getenv("WT_SESSION") != ""
	var w io.Writer = os.Stdout
	if runtime.GOOS == "windows" {
		w = colorable.NewColorableStdout()
	}

	clearTerminal(w)
	fmt.Fprintln(w, "────────────────────────────────────────────────────────")
	fmt.Fprintln(w, "  whatsapp-mcp — Scan to connect")
	fmt.Fprintln(w, "────────────────────────────────────────────────────────")
	fmt.Fprintln(w, "  On your phone:")
	fmt.Fprintln(w, "  WhatsApp › Settings › Linked Devices › Link a Device")
	fmt.Fprintln(w)

	if useHalfBlocks {
		qrterminal.GenerateHalfBlock(code, qrterminal.L, w)
	} else {
		// ANSI-colored full blocks: wider but codepage-proof.
		qrterminal.Generate(code, qrterminal.L, w)
	}

	fmt.Fprintf(w, "\n  QR #%d · expires in ~%ds · refreshes here automatically — always scan the one on screen\n",
		attempt, int(expiresIn.Seconds()))
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" {
		fmt.Fprintln(w, "  Garbled square? Use Windows Terminal, or pair by typed code instead:")
		fmt.Fprintln(w, "    curl -X POST http://127.0.0.1:8080/api/auth/pair-phone -d \"{\\\"phone_number\\\":\\\"+15551234567\\\"}\"")
	}
}

// clearTerminal clears the screen + homes the cursor via ANSI, which
// go-colorable translates for legacy Windows consoles.
func clearTerminal(w io.Writer) {
	fmt.Fprint(w, "\x1b[2J\x1b[H")
}
