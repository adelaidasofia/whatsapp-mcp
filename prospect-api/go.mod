module github.com/adelaidasofia/whatsapp-mcp/prospect-api

go 1.25.0

toolchain go1.26.5

// Phase A: lookup, pull-context, update-crm, preset, check-preset.
// Phase B (Sprints 7-10) lands here too as additional handlers.
//
// Dependencies populated by `go mod tidy`. SQLCipher driver is the same
// one the whatsapp-bridge uses; preset DB and message DB share the key.

require (
	github.com/google/uuid v1.6.0
	github.com/mutecomm/go-sqlcipher/v4 v4.4.2
	golang.org/x/text v0.39.0
	gopkg.in/yaml.v3 v3.0.1
)
