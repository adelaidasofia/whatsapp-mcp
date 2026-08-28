package main

import (
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestClientErrLogRecordsLastError(t *testing.T) {
	c := newClientErrLog(waLog.Noop)

	if msg, at := c.Last(); msg != "" || !at.IsZero() {
		t.Fatalf("fresh recorder should be empty, got %q at %v", msg, at)
	}

	c.Errorf("Client outdated (%d) connect failure", 405)
	msg, at := c.Last()
	if msg != "Client outdated (405) connect failure" {
		t.Fatalf("args not formatted into recorded message: %q", msg)
	}
	if at.IsZero() {
		t.Fatal("timestamp not recorded")
	}

	c.Errorf("newer failure")
	if msg, _ := c.Last(); msg != "newer failure" {
		t.Fatalf("recorder should keep the LATEST error, got %q", msg)
	}
}

// whatsmeow logs socket failures through a Sub("Socket") logger. Those are the
// errors worth surfacing, so a sublogger must write into the parent's slot.
func TestClientErrLogSubSharesSlot(t *testing.T) {
	parent := newClientErrLog(waLog.Noop)
	sub := parent.Sub("Socket")

	sub.Errorf("error reading from websocket: EOF")

	msg, at := parent.Last()
	if msg != "error reading from websocket: EOF" {
		t.Fatalf("sublogger error did not reach parent, got %q", msg)
	}
	if at.IsZero() {
		t.Fatal("sublogger did not record a timestamp on the parent")
	}
}

// Only Errorf is diagnostic. Warnf fires constantly in normal operation
// (expired media, odd notifications) and must not overwrite a real failure.
func TestClientErrLogIgnoresNonErrors(t *testing.T) {
	c := newClientErrLog(waLog.Noop)
	c.Errorf("the real failure")

	c.Warnf("Failed to download media: invalid media hmac")
	c.Infof("connected")
	c.Debugf("noise")

	if msg, _ := c.Last(); msg != "the real failure" {
		t.Fatalf("non-error level overwrote the recorded error: %q", msg)
	}
}
