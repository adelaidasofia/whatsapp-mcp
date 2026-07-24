package main

import "testing"

// Zero-config boot: a fresh install with no env must validate. The old
// default (local-cpp) demanded a ~3 GB whisper model path at startup and
// stranded every new install on every OS.
func TestConfigDefaultsBootWithZeroEnv(t *testing.T) {
	t.Setenv("WHATSAPP_WHISPER_BACKEND", "")
	t.Setenv("WHATSAPP_WHISPER_MODEL_PATH", "")
	t.Setenv("WHATSAPP_DB_KEY", "")
	t.Setenv("WHATSAPP_BRIDGE_PORT", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with defaults must succeed, got: %v", err)
	}
	if cfg.WhisperBackend != "off" {
		t.Fatalf("default whisper backend = %q, want off", cfg.WhisperBackend)
	}
}

func TestConfigLocalCppStillFailsLoudWithoutModel(t *testing.T) {
	t.Setenv("WHATSAPP_WHISPER_BACKEND", "local-cpp")
	t.Setenv("WHATSAPP_WHISPER_MODEL_PATH", "")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("local-cpp without a model path must refuse to boot (Lesson 20)")
	}
}

func TestConfigRejectsUnknownWhisperBackend(t *testing.T) {
	t.Setenv("WHATSAPP_WHISPER_BACKEND", "cloudinator")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("unknown whisper backend must be rejected")
	}
}

func TestConfigAllowsPortZeroForOSAssignment(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_PORT", "0")
	t.Setenv("WHATSAPP_WHISPER_BACKEND", "off")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("port 0 must be allowed (OS-assigned): %v", err)
	}
	if cfg.BridgePort != 0 {
		t.Fatalf("BridgePort = %d, want 0", cfg.BridgePort)
	}
}

func TestConfigReadsExplicitDBKey(t *testing.T) {
	t.Setenv("WHATSAPP_DB_KEY", "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899")
	t.Setenv("WHATSAPP_WHISPER_BACKEND", "off")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.DBKey) != 64 {
		t.Fatalf("DBKey not threaded through config (len=%d)", len(cfg.DBKey))
	}
}

func TestSplitNormalizedCSV(t *testing.T) {
	got := splitNormalizedCSV(" Mamá, Pablo Tucu ,, FAVORITOS ")
	want := []string{"mama", "pablo tucu", "favoritos"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if r := splitNormalizedCSV(""); r != nil {
		t.Errorf("empty input should give nil, got %v", r)
	}
}
