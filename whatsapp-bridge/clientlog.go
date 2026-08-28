package main

import (
	"fmt"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// clientErrLog wraps a waLog.Logger and remembers the most recent ERROR line
// whatsmeow emitted.
//
// Why this exists: whatsmeow reports connection failures ONLY through its own
// logger. When WhatsApp rejects the socket — "Client outdated (405) connect
// failure" is the one that bit us on 2026-08-24 — nothing reaches the bridge's
// own state: no events.Disconnected, no error from Connect(). The bridge kept
// reporting connected=true for four days while every server-side call failed
// with "websocket not connected", and the only trace was a line in stdout that
// nobody was reading. Recording the last error here lets /api/status say WHY
// the socket is down instead of pretending it is up.
//
// Sub() shares the parent's mutex and slot on purpose: whatsmeow logs socket
// failures through a "Socket" sublogger, and those are exactly the ones worth
// keeping.
type clientErrLog struct {
	inner waLog.Logger
	mu    *sync.Mutex
	last  *loggedErr
}

type loggedErr struct {
	msg string
	at  time.Time
}

func newClientErrLog(inner waLog.Logger) *clientErrLog {
	return &clientErrLog{inner: inner, mu: &sync.Mutex{}, last: &loggedErr{}}
}

func (c *clientErrLog) Errorf(msg string, args ...any) {
	c.mu.Lock()
	c.last.msg = fmt.Sprintf(msg, args...)
	c.last.at = time.Now()
	c.mu.Unlock()
	c.inner.Errorf(msg, args...)
}

func (c *clientErrLog) Warnf(msg string, args ...any)  { c.inner.Warnf(msg, args...) }
func (c *clientErrLog) Infof(msg string, args ...any)  { c.inner.Infof(msg, args...) }
func (c *clientErrLog) Debugf(msg string, args ...any) { c.inner.Debugf(msg, args...) }

func (c *clientErrLog) Sub(module string) waLog.Logger {
	return &clientErrLog{inner: c.inner.Sub(module), mu: c.mu, last: c.last}
}

// Last returns the most recent whatsmeow error and when it happened.
// Zero time means whatsmeow has not logged an error this process.
func (c *clientErrLog) Last() (string, time.Time) {
	if c == nil {
		return "", time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last.msg, c.last.at
}
