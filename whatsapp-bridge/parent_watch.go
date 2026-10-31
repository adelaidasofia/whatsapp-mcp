package main

import (
	"io"
)

// watchParent returns a channel that closes when the app running this bridge
// goes away. With WHATSAPP_EXIT_WITH_PARENT=true the app hands the bridge a
// stdin pipe it never writes to; the OS closes that pipe when the app exits
// or crashes, on every desktop OS, and the read below returns. Without it a
// crashed app leaves the bridge running, and the app's next bridge fights it
// for the same WhatsApp link. When disabled the channel never closes, so a
// terminal user's stdin is left alone.
func watchParent(enabled bool, stdin io.Reader) <-chan struct{} {
	gone := make(chan struct{})
	if !enabled {
		return gone
	}
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		close(gone)
	}()
	return gone
}
