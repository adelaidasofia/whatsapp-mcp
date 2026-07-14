package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyEnvFileSetsNewVars(t *testing.T) {
	path := writeEnvFile(t, "# comment\n\nWHATSAPP_TEST_A=hello\nexport WHATSAPP_TEST_B=\"quoted value\"\nWHATSAPP_TEST_C='single'\n")
	for _, k := range []string{"WHATSAPP_TEST_A", "WHATSAPP_TEST_B", "WHATSAPP_TEST_C"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}

	applied, err := applyEnvFile(path)
	if err != nil {
		t.Fatalf("applyEnvFile: %v", err)
	}
	if applied != 3 {
		t.Fatalf("applied = %d, want 3", applied)
	}
	if got := os.Getenv("WHATSAPP_TEST_A"); got != "hello" {
		t.Errorf("A = %q", got)
	}
	if got := os.Getenv("WHATSAPP_TEST_B"); got != "quoted value" {
		t.Errorf("B = %q", got)
	}
	if got := os.Getenv("WHATSAPP_TEST_C"); got != "single" {
		t.Errorf("C = %q", got)
	}
}

func TestApplyEnvFileProcessEnvWins(t *testing.T) {
	path := writeEnvFile(t, "WHATSAPP_TEST_WINS=file\n")
	t.Setenv("WHATSAPP_TEST_WINS", "process")

	if _, err := applyEnvFile(path); err != nil {
		t.Fatalf("applyEnvFile: %v", err)
	}
	if got := os.Getenv("WHATSAPP_TEST_WINS"); got != "process" {
		t.Fatalf("process env must win, got %q", got)
	}
}

func TestApplyEnvFileTolerantOfJunk(t *testing.T) {
	path := writeEnvFile(t, "not a kv line\nWHATSAPP_TEST_D=ok # trailing comment\nBAD KEY=nope\n")
	os.Unsetenv("WHATSAPP_TEST_D")
	t.Cleanup(func() { os.Unsetenv("WHATSAPP_TEST_D") })

	applied, err := applyEnvFile(path)
	if err != nil {
		t.Fatalf("applyEnvFile: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	if got := os.Getenv("WHATSAPP_TEST_D"); got != "ok" {
		t.Fatalf("D = %q, want ok (inline comment stripped)", got)
	}
}
