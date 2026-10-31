package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenGuardRefusesRequestsWithoutTheToken(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	guard := newTokenGuard(next, "s3cret-token")

	for _, header := range []string{"", "Bearer wrong", "s3cret-token", "Basic s3cret-token"} {
		reached = false
		req := httptest.NewRequest(http.MethodGet, "/api/messages?chat_jid=x", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || reached {
			t.Errorf("Authorization %q: status %d, reached handler %v; want 401 and not reached", header, rec.Code, reached)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages?chat_jid=x", nil)
	req.Header.Set("Authorization", "Bearer s3cret-token")
	guard.ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Fatal("the right token did not reach the handler")
	}
}

func TestNoTokenKeepsTheAPIOpenForAHandStartedBridge(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	newTokenGuard(next, "").ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if !reached {
		t.Fatal("with no token set the API must stay as it was")
	}
}
