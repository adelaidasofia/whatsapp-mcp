package main

import (
	"context"
	"regexp"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

// browserOS is the `Browser (OS)` shape whatsmeow's PairPhone documents for the
// companion display name. v0.4.1 sent "Chrome (whatsapp-mcp)", and
// --pair-phone / POST /api/auth/pair-phone failed with
// "info query returned status 400: bad-request".
var browserOS = regexp.MustCompile(`^Chrome \((Windows|Mac OS|Linux)\)$`)

func TestPairPhoneDisplayNameIsTheProvenBrowserOSName(t *testing.T) {
	if browserOS.MatchString("Chrome (whatsapp-mcp)") {
		t.Fatal("control: the shape check accepts the name the server rejected")
	}
	if !browserOS.MatchString(pairPhoneDisplayName) {
		t.Fatalf("pairPhoneDisplayName = %q is not Browser (OS)", pairPhoneDisplayName)
	}
	// Pinned to the value with production evidence behind it (see the
	// constant's comment). Changing it needs that evidence for the new value.
	if pairPhoneDisplayName != "Chrome (Linux)" {
		t.Fatalf("pairPhoneDisplayName = %q; only %q has production evidence behind it", pairPhoneDisplayName, "Chrome (Linux)")
	}
}

// Asserted where the value leaves the bridge, not on the constant: a constant
// test stays green when the call site stops using the constant.
func TestRequestPairingCodeSendsABrowserOSDisplayName(t *testing.T) {
	type call struct {
		phone      string
		push       bool
		clientType whatsmeow.PairClientType
		name       string
	}
	var calls []call
	b := &Bridge{
		client:       &whatsmeow.Client{Store: &store.Device{}},
		loginRunning: true,
		pairPhone: func(_ context.Context, phone string, push bool, clientType whatsmeow.PairClientType, name string) (string, error) {
			calls = append(calls, call{phone, push, clientType, name})
			return "ABCDEFGH", nil
		},
	}

	code, err := b.RequestPairingCode(context.Background(), "+1 (555) 555-0100")
	if err != nil {
		t.Fatalf("RequestPairingCode: %v", err)
	}
	if code != "ABCDEFGH" {
		t.Fatalf("code = %q, want the one PairPhone returned", code)
	}
	if len(calls) != 1 {
		t.Fatalf("PairPhone called %d times, want 1", len(calls))
	}
	c := calls[0]
	if c.name != pairPhoneDisplayName {
		t.Errorf("display name sent to WhatsApp = %q, want %q", c.name, pairPhoneDisplayName)
	}
	if !browserOS.MatchString(c.name) {
		t.Errorf("display name sent to WhatsApp = %q, which the server answers with 400 bad-request", c.name)
	}
	if c.clientType != whatsmeow.PairClientChrome {
		t.Errorf("client type = %q, want PairClientChrome to match the Chrome display name", c.clientType)
	}
	if c.phone != "15555550100" {
		t.Errorf("phone sent = %q, want digits only with country code", c.phone)
	}
	if !c.push {
		t.Error("showPushNotification = false; the phone would not prompt for the code")
	}
	if snap := b.AuthSnapshot(); snap.State != AuthStatePairingPending || snap.PairingCode != "ABCDEFGH" {
		t.Errorf("auth snapshot = %+v, want pairing_pending carrying the code", snap)
	}
}
