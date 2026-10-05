package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mdp/qrterminal/v3"
)

// qrterminal.Generate runs IsSixelSupported first. On a Windows console with
// VT processing on, go-colorable hands back os.Stdout itself, so the probe
// gets past its writer check and calls term.MakeRaw on the OUTPUT handle;
// conhost rejects the input-only mode, MakeRaw returns nil, and the probe's
// deferred term.Restore(fd, nil) dereferences it (x/term term_windows.go:47).
// The QR loop runs in the auth goroutine, so that panic took the whole bridge
// down on every draw outside Windows Terminal. Bridge code must never reach
// either function; tests may, as controls.
var qrterminalProbes = map[string]bool{"Generate": true, "IsSixelSupported": true}

// qrterminalProbeRefs lists every reference to a probing qrterminal function
// in one Go file, under whatever name that file imports the package as.
func qrterminalProbeRefs(t *testing.T, filename string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	local := ""
	for _, imp := range f.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path != "github.com/mdp/qrterminal/v3" {
			continue
		}
		local = "qrterminal"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	switch local {
	case "":
		return nil
	case ".":
		t.Fatalf("%s dot-imports qrterminal, which hides its calls from this check", filename)
	}
	var refs []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == local && qrterminalProbes[sel.Sel.Name] {
			refs = append(refs, fset.Position(sel.Pos()).String()+": "+local+"."+sel.Sel.Name)
		}
		return true
	})
	return refs
}

func TestNoBridgeCodeProbesTheTerminalForSixel(t *testing.T) {
	// Controls first: a scan that misses a planted call proves nothing about
	// the real files.
	for name, src := range map[string]string{
		"plain.go": "package main\nimport \"github.com/mdp/qrterminal/v3\"\n" +
			"func f() { qrterminal.Generate(\"x\", qrterminal.L, nil) }\n",
		"aliased.go": "package main\nimport qt \"github.com/mdp/qrterminal/v3\"\n" +
			"var _ = qt.IsSixelSupported(nil)\n",
	} {
		if len(qrterminalProbeRefs(t, name, src)) == 0 {
			t.Fatalf("control: the scan missed the probe planted in %s", name)
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sawRenderer := false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawRenderer = sawRenderer || name == "qr_render.go"
		for _, ref := range qrterminalProbeRefs(t, name, nil) {
			t.Errorf("%s probes the terminal; build a qrterminal.Config and call GenerateWithConfig instead", ref)
		}
	}
	if !sawRenderer {
		t.Fatalf("scanned %v without reaching qr_render.go; the check is not looking where the renderer lives", files)
	}
}

// The explicit configs must draw byte-for-byte what the helpers drew, so
// dropping the probe changes nothing on screen. Calling Generate is safe here:
// its probe returns false at once for any writer that is not os.Stdout.
func TestQRConfigDrawsWhatTheHelpersDrew(t *testing.T) {
	const code = "2@Qm9ndXNQYWlyaW5nUmVmRm9yVGVzdHNPbmx5,dGVzdC1ub2lzZS1rZXk=,dGVzdC1pZGVudGl0eS1rZXk=,dGVzdC1hZHYtc2VjcmV0"
	fullBlocks := func(w *bytes.Buffer) { qrterminal.Generate(code, qrterminal.L, w) }
	halfBlocks := func(w *bytes.Buffer) { qrterminal.GenerateHalfBlock(code, qrterminal.L, w) }
	for _, tc := range []struct {
		name            string
		goos            string
		windowsTerminal bool
		reference       func(*bytes.Buffer)
	}{
		{"Windows console outside Windows Terminal", "windows", false, fullBlocks},
		{"Windows Terminal", "windows", true, halfBlocks},
		{"macOS", "darwin", false, halfBlocks},
		{"Linux", "linux", false, halfBlocks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want, got bytes.Buffer
			tc.reference(&want)
			cfg := qrConfig(&got, tc.goos, tc.windowsTerminal)
			if cfg.WithSixel {
				t.Fatal("config asks for sixel output")
			}
			qrterminal.GenerateWithConfig(code, cfg)
			if got.Len() == 0 {
				t.Fatal("nothing drawn")
			}
			if got.String() != want.String() {
				t.Fatalf("drew %d bytes that differ from the helper's %d", got.Len(), want.Len())
			}
		})
	}
}

// panicsOnQRWriter takes the header, then panics on the first write carrying
// QR art: a stand-in for anything inside the QR library failing mid-draw.
type panicsOnQRWriter struct{ bytes.Buffer }

func (w *panicsOnQRWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(qrterminal.WHITE_WHITE)) || bytes.Contains(p, []byte("\033[4")) {
		panic("simulated failure while drawing the QR")
	}
	return w.Buffer.Write(p)
}

// Drawing is cosmetic and the login loop that calls it is not: a draw that
// fails must leave the bridge running and say where the code still is.
func TestDrawQRSurvivesAPanicMidDraw(t *testing.T) {
	for _, tc := range []struct {
		goos            string
		windowsTerminal bool
	}{{"windows", false}, {"windows", true}, {"darwin", false}} {
		var logs bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&logs)

		w := &panicsOnQRWriter{}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.SetOutput(prev)
					t.Fatalf("%s (Windows Terminal %v): drawQR let a draw panic escape: %v", tc.goos, tc.windowsTerminal, r)
				}
			}()
			drawQR(w, "2@code", 3, 20*time.Second, tc.goos, tc.windowsTerminal)
		}()
		log.SetOutput(prev)

		if !strings.Contains(w.String(), "Scan to connect") {
			t.Fatalf("%s: control: the header never reached the writer, so the panic was not mid-draw", tc.goos)
		}
		got := logs.String()
		for _, want := range []string{"simulated failure while drawing the QR", "GET /api/auth/qr", "attempt 3"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s (Windows Terminal %v): log is missing %q:\n%s", tc.goos, tc.windowsTerminal, want, got)
			}
		}
	}
}

// The hint under a full-block QR is for someone whose square came out
// garbled. It used to be a curl command, which Windows PowerShell 5.1 rejects
// (curl is Invoke-WebRequest there) and which hardcoded port 8080 although
// WHATSAPP_BRIDGE_PORT moves it. A flag on the bridge works in every shell.
func TestWindowsConsoleHintWorksInEveryShell(t *testing.T) {
	var out bytes.Buffer
	drawQR(&out, "2@code", 1, 20*time.Second, "windows", false)
	s := out.String()
	if !strings.Contains(s, "--pair-phone") {
		t.Errorf("full-block hint does not offer --pair-phone:\n%s", s)
	}
	for _, bad := range []string{"curl", ":8080"} {
		if strings.Contains(s, bad) {
			t.Errorf("full-block hint still carries %q:\n%s", bad, s)
		}
	}

	out.Reset()
	drawQR(&out, "2@code", 1, 20*time.Second, "windows", true)
	if strings.Contains(out.String(), "--pair-phone") {
		t.Error("the garbled-square hint shows in Windows Terminal, where the half-block QR draws fine")
	}
}
