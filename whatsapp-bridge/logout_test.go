package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

// A Disconnect on a bridge that never linked has nothing to unlink: it must
// answer cleanly, not fail, so the app can go on to remove the local data.
func TestLogoutWithNothingLinkedSaysSo(t *testing.T) {
	b := &Bridge{fatal: make(chan struct{}), client: whatsmeow.NewClient(&store.Device{}, nil)}
	s := &Server{bridge: b}
	rec := httptest.NewRecorder()
	s.handleAuthLogout(rec, httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["unlinked"] != false {
		t.Fatalf("body = %v, want unlinked=false", body)
	}
}
