package main

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// Loopback is not a trust boundary against the browser (issue #34).
//
// Binding to 127.0.0.1 keeps the API off the network, but it does NOT keep it
// away from any web page the user happens to load: a page can POST to
// http://127.0.0.1:8080 with Content-Type: text/plain, which is a CORS
// "simple request" and therefore sends NO preflight. The browser will refuse
// to show the attacker the RESPONSE, but the request still executes — and
// every state-changing route here (sends, presence, media download, admin
// backfills) does its work on the way in. Measured against the running
// bridge before this guard existed: a cross-origin text/plain POST carrying
// Origin: https://evil.example was accepted and processed, rejected only by
// business logic.
//
// Two checks, both cheap:
//
//  1. Origin. A browser attaches Origin to every cross-origin request (and to
//     every same-origin non-GET in current browsers). A non-browser client —
//     the Python MCP server's httpx, curl, a supervisor script — attaches
//     none. So "Origin present and not allowlisted" is a precise signal for
//     "a web page is driving this", with no false positives on the clients
//     this bridge is actually for.
//
//  2. Host. DNS rebinding defeats the bind address: an attacker-controlled
//     name that resolves to 127.0.0.1 reaches this server with its own Host
//     header, and the connection is genuinely local. Pinning Host to loopback
//     literals closes that.
//
// WHATSAPP_ALLOWED_ORIGINS is the escape hatch, because the headless auth API
// exists precisely so a GUI can drive pairing — and a browser-based GUI is a
// legitimate caller that WILL send Origin. Set it to a comma-separated list
// (e.g. "http://localhost:5173") to let that GUI through. Empty by default:
// no browser origin is trusted until someone names one.
type originGuard struct {
	next    http.Handler
	allowed map[string]bool
}

func newOriginGuard(next http.Handler, allowedOrigins []string) *originGuard {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[strings.ToLower(strings.TrimSuffix(o, "/"))] = true
		}
	}
	return &originGuard{next: next, allowed: allowed}
}

func (g *originGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hostIsLoopback(r.Host) {
		log.Printf("origin guard: rejected Host %q (not a loopback name; possible DNS rebinding)", r.Host)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "request Host is not a loopback address; the bridge only serves 127.0.0.1/localhost",
		})
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !g.allowed[strings.ToLower(strings.TrimSuffix(origin, "/"))] {
			log.Printf("origin guard: rejected cross-origin request from %q to %s %s", origin, r.Method, r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "cross-origin requests are refused; set WHATSAPP_ALLOWED_ORIGINS if a browser UI must call the bridge",
			})
			return
		}
	}
	g.next.ServeHTTP(w, r)
}

// hostIsLoopback reports whether the request's Host header names the loopback
// interface. A missing port is fine (HTTP/1.0, or :80). A bare hostname that
// is not "localhost" is refused even if it currently resolves to 127.0.0.1 —
// that resolution is exactly what an attacker controls.
func hostIsLoopback(host string) bool {
	if host == "" {
		return false
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // no port present
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
