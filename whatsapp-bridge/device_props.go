package main

import (
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// applyDeviceProps sets what this bridge tells WhatsApp about itself when it
// links: the name the phone lists under Linked devices, and whether to send
// the whole chat history rather than the recent months it sends by default
// (whatsapp-vault-sync's `syncFullHistory: true`, so a member links once and
// gets both). WhatsApp reads these only from the registration of a device
// that has not linked yet, so on an already linked device they change
// nothing until it is unlinked and links again. Call it before the first
// pairing; it writes whatsmeow's package-level store.DeviceProps.
func applyDeviceProps(cfg *Config) {
	if name := strings.TrimSpace(cfg.DeviceName); name != "" {
		store.SetOSInfo(name, versionTriple(bridgeVersion))
		store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	}
	if cfg.FullHistory {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
	}
}

// versionTriple turns "0.6.0" into [0 6 0]; a part that is not a number
// reads as 0, so a local `-X main.bridgeVersion=v0.6.0-3-gabc` build still
// links.
func versionTriple(version string) [3]uint32 {
	var out [3]uint32
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	for i, part := range parts {
		digits := part
		if end := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
			digits = part[:end]
		}
		if n, err := strconv.ParseUint(digits, 10, 32); err == nil {
			out[i] = uint32(n)
		}
	}
	return out
}
