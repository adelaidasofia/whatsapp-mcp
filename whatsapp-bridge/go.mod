module github.com/adelaidasofia/whatsapp-mcp/whatsapp-bridge

go 1.25.0

toolchain go1.26.5

// Dependencies are populated by `go mod tidy` on first build.
// Upgrading any dependency requires a manual diff review per SECURITY.md.

require (
	github.com/google/uuid v1.6.0
	github.com/mattn/go-colorable v0.1.14
	github.com/mattn/go-isatty v0.0.20
	github.com/mdp/qrterminal/v3 v3.2.1
	github.com/mutecomm/go-sqlcipher/v4 v4.4.2
	go.mau.fi/whatsmeow v0.0.0-20260421083005-5b8886176ff7
	golang.org/x/sys v0.45.0
	golang.org/x/text v0.39.0
	google.golang.org/protobuf v1.36.11
)

require (
	filippo.io/edwards25519 v1.1.1 // indirect
	github.com/beeper/argo-go v1.1.2 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/elliotchance/orderedmap/v3 v3.1.0 // indirect
	github.com/petermattis/goid v0.0.0-20260330135022-df67b199bc81 // indirect
	github.com/rs/zerolog v1.35.0 // indirect
	github.com/vektah/gqlparser/v2 v2.5.27 // indirect
	go.mau.fi/libsignal v0.2.1 // indirect
	go.mau.fi/util v0.9.8 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/exp v0.0.0-20260410095643-746e56fc9e2f // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/term v0.43.0 // indirect
	rsc.io/qr v0.2.0 // indirect
)
