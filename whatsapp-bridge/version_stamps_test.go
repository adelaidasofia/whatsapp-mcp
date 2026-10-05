package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// v0.4.1 was tagged with every stamp still at 0.4.0, so its binaries reported
// 0.4.0 at /healthcheck and plugin.json, manifest.json and pyproject.toml all
// named the release before it. bridgeVersion (version.go) is the single source
// of truth: every stamp a user or an installer reads has to agree with it, and
// the CHANGELOG has to carry its section. The tag itself is checked by
// release.yml, which refuses to build a tag that does not match bridgeVersion.
//
// server.json is left out on purpose: it describes what is published to the
// MCP registry, which is republished on its own schedule, not per tag.
func TestVersionStampsAgree(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(bridgeVersion) {
		t.Fatalf("bridgeVersion = %q is not a semantic version", bridgeVersion)
	}
	root := ".."

	for _, rel := range []string{"manifest.json", filepath.Join(".claude-plugin", "plugin.json")} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var stamp struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &stamp); err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		if stamp.Version != bridgeVersion {
			t.Errorf("%s says %q, bridgeVersion says %q", rel, stamp.Version, bridgeVersion)
		}
	}

	pyproject, err := os.ReadFile(filepath.Join(root, "whatsapp-mcp-server", "pyproject.toml"))
	if err != nil {
		t.Fatalf("read pyproject.toml: %v", err)
	}
	if got := projectVersion(string(pyproject)); got != bridgeVersion {
		t.Errorf("whatsapp-mcp-server/pyproject.toml [project] version is %q, bridgeVersion says %q", got, bridgeVersion)
	}

	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	if !strings.Contains(string(changelog), "\n## ["+bridgeVersion+"]") {
		t.Errorf("CHANGELOG.md has no \"## [%s]\" section", bridgeVersion)
	}
}

// projectVersion returns the version from pyproject's [project] table only, so
// a version key in some other table cannot stand in for it.
func projectVersion(pyproject string) string {
	start := strings.Index(pyproject, "[project]\n")
	if start < 0 {
		return ""
	}
	table := pyproject[start+len("[project]\n"):]
	if next := strings.Index(table, "\n["); next >= 0 {
		table = table[:next]
	}
	m := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]*)"`).FindStringSubmatch(table)
	if m == nil {
		return ""
	}
	return m[1]
}
