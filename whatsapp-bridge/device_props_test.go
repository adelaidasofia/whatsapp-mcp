package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// restoreDeviceProps puts whatsmeow's package-level props back after a test
// that changes them, so test order cannot leak a device name or full sync
// into another test.
func restoreDeviceProps(t *testing.T) {
	t.Helper()
	saved := proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps)
	savedOS := store.BaseClientPayload.UserAgent.GetOsVersion()
	t.Cleanup(func() {
		store.DeviceProps = saved
		store.BaseClientPayload.UserAgent.OsVersion = proto.String(savedOS)
		store.BaseClientPayload.UserAgent.OsBuildNumber = proto.String(savedOS)
	})
}

func TestApplyDevicePropsNamesTheDeviceAndAsksForFullHistory(t *testing.T) {
	restoreDeviceProps(t)
	applyDeviceProps(&Config{DeviceName: "Mycelium", FullHistory: true})

	if got := store.DeviceProps.GetOs(); got != "Mycelium" {
		t.Errorf("Linked devices name = %q, want Mycelium", got)
	}
	if got := store.DeviceProps.GetPlatformType(); got != waCompanionReg.DeviceProps_DESKTOP {
		t.Errorf("platform = %v, want DESKTOP", got)
	}
	if !store.DeviceProps.GetRequireFullSync() {
		t.Error("full history was not requested")
	}
}

func TestApplyDevicePropsLeavesDefaultsWhenUnset(t *testing.T) {
	restoreDeviceProps(t)
	before := proto.Clone(store.DeviceProps)
	applyDeviceProps(&Config{})
	if !proto.Equal(before, store.DeviceProps) {
		t.Errorf("an unset name and FullHistory=false changed the device props: %v", store.DeviceProps)
	}
}

func TestVersionTriple(t *testing.T) {
	cases := map[string][3]uint32{
		"0.6.0":          {0, 6, 0},
		"v1.2.3":         {1, 2, 3},
		"0.6.0-3-gabc12": {0, 6, 0},
		"dev":            {0, 0, 0},
	}
	for in, want := range cases {
		if got := versionTriple(in); got != want {
			t.Errorf("versionTriple(%q) = %v, want %v", in, got, want)
		}
	}
}
