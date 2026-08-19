package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func guardFor(t *testing.T, allowed []string) (http.Handler, *bool) {
	t.Helper()
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	return newOriginGuard(inner, allowed), &reached
}

// The clients this bridge is actually for send no Origin header, so they must
// pass through untouched. A regression here breaks every install.
func TestOriginGuardAllowsNonBrowserClients(t *testing.T) {
	for _, tc := range []struct{ name, host string }{
		{"loopback ip with port", "127.0.0.1:8080"},
		{"localhost with port", "localhost:8080"},
		{"loopback ip no port", "127.0.0.1"},
		{"ipv6 loopback", "[::1]:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, reached := guardFor(t, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/presence/typing", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			if !*reached {
				t.Fatalf("no-Origin request was blocked (status %d); this is the MCP server's path", rec.Code)
			}
		})
	}
}

// The measured issue #34 vector: a text/plain POST from a web page is a CORS
// simple request, so it is never preflighted and the handler used to run.
func TestOriginGuardBlocksCrossOriginSimpleRequest(t *testing.T) {
	g, reached := guardFor(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/media/download", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if *reached {
		t.Fatalf("cross-origin request reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

// The headless auth API exists so a GUI can drive pairing, and a browser GUI
// legitimately sends Origin. Named origins must get through.
func TestOriginGuardAllowsConfiguredOrigin(t *testing.T) {
	g, reached := guardFor(t, []string{"http://localhost:5173"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/pair-phone", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if !*reached {
		t.Fatalf("allowlisted origin was blocked (status %d)", rec.Code)
	}

	// A near-miss must still be refused.
	g2, reached2 := guardFor(t, []string{"http://localhost:5173"})
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/pair-phone", nil)
	req2.Host = "127.0.0.1:8080"
	req2.Header.Set("Origin", "http://localhost:5174")
	g2.ServeHTTP(httptest.NewRecorder(), req2)
	if *reached2 {
		t.Fatalf("a non-allowlisted origin got through")
	}
}

// DNS rebinding: the connection really is local, so the bind address proves
// nothing. Only the Host header distinguishes it.
func TestOriginGuardBlocksNonLoopbackHost(t *testing.T) {
	g, reached := guardFor(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/sends", nil)
	req.Host = "rebind.evil.example:8080"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if *reached {
		t.Fatalf("rebound Host reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

func TestHostIsLoopback(t *testing.T) {
	for _, c := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"127.0.0.1", true},
		{"127.5.5.5:8080", true}, // all of 127/8 is loopback
		{"localhost:8080", true},
		{"LocalHost:8080", true},
		{"[::1]:8080", true},
		{"::1", true},
		{"", false},
		{"evil.example:8080", false},
		{"192.168.1.10:8080", false},
		{"0.0.0.0:8080", false},
	} {
		if got := hostIsLoopback(c.host); got != c.want {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
