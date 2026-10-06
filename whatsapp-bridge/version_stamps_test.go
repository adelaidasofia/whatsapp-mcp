package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// v0.4.1 was tagged with version.go, manifest.json, plugin.json and
// pyproject.toml all still at 0.4.0, so its binaries reported 0.4.0 at
// /healthcheck. bridgeVersion (version.go) is the single source of truth: those
// four stamps have to agree with it, and the CHANGELOG has to carry a dated,
// non-empty section for it. The tag itself is checked by release.yml, which
// refuses to build a tag that is not "v" + bridgeVersion.
//
// Left out on purpose:
//   - server.json: the MCP-registry entry, frozen at 0.1.x; no automation
//     republishes it.
//   - prospect-api: its own Go module with its own version, not built by
//     release.yml.
//   - hooks/install-ping.py: reads plugin.json at runtime instead of carrying a
//     stamp (whatsapp-mcp-server/tests/test_install_ping_version.py).
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
	heading := regexp.MustCompile(`(?m)^## \[` + regexp.QuoteMeta(bridgeVersion) + `\] - \d{4}-\d{2}-\d{2}\r?$`)
	loc := heading.FindIndex(changelog)
	if loc == nil {
		t.Fatalf("CHANGELOG.md has no \"## [%s] - YYYY-MM-DD\" section", bridgeVersion)
	}
	section := string(changelog[loc[1]:])
	if next := strings.Index(section, "\n## ["); next >= 0 {
		section = section[:next]
	}
	if !regexp.MustCompile(`(?m)^- `).MatchString(section) {
		t.Errorf("CHANGELOG.md's [%s] section has no entries", bridgeVersion)
	}
}

// release.yml reads bridgeVersion with sed, not with the Go compiler, so a
// harmless-looking rewrite of version.go (a type annotation, a var block, a
// trailing comment) compiles and passes every other test, then fails every
// release leg after the release is already public. This pins the line to the
// shape the workflow parses, and pins the workflow to that same pattern.
func TestReleaseCanReadBridgeVersion(t *testing.T) {
	src, err := os.ReadFile("version.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := regexp.MustCompile(`(?m)^var bridgeVersion = "([^"]*)"\r?$`).FindAllSubmatch(src, -1)
	if len(lines) != 1 {
		t.Fatalf("version.go has %d lines of the form `var bridgeVersion = \"X.Y.Z\"`, want exactly 1: release.yml cannot read anything else", len(lines))
	}
	if got := string(lines[0][1]); got != bridgeVersion {
		t.Fatalf("release.yml would read %q from version.go, the binary reports %q", got, bridgeVersion)
	}

	workflow, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const sedExpr = `s/^var bridgeVersion = "\([^"]*\)"$/\1/p`
	if !strings.Contains(string(workflow), sedExpr) {
		t.Fatalf("release.yml no longer extracts the version with %s; update this test and version.go's comment together with it", sedExpr)
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
