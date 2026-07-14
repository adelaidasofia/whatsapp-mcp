package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// loadDotEnv loads KEY=VALUE pairs from the first .env file found (working
// directory, then the executable's directory, then the repo root one level
// up from the executable — covering `./bin/whatsapp-bridge` layouts).
//
// SETUP.md has always told users to `cp .env.example .env` and edit it, but
// nothing ever read that file — the bridge only saw process env, so every
// .env-configured install silently ran on defaults. This closes that gap
// with zero dependencies.
//
// Semantics (deliberately minimal):
//   - lines starting with # and blank lines are skipped
//   - values may be wrapped in single or double quotes (stripped)
//   - real process env ALWAYS wins — a var that is already set is not touched
//   - no variable interpolation here; path vars like ${HOME}/... are expanded
//     later by config.go's expandPath at the point of use
func loadDotEnv() {
	for _, dir := range dotEnvSearchDirs() {
		path := filepath.Join(dir, ".env")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		applied, err := applyEnvFile(path)
		if err != nil {
			log.Printf("env file %s: %v (continuing with process env)", path, err)
			return
		}
		log.Printf("env file loaded: %s (%d new vars; process env wins on overlap)", path, applied)
		return
	}
}

func dotEnvSearchDirs() []string {
	dirs := []string{"."}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		dirs = append(dirs, exeDir, filepath.Dir(exeDir))
	}
	return dirs
}

// applyEnvFile sets each new KEY=VALUE from the file into the process env.
// Returns how many vars were newly set.
func applyEnvFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	applied := 0
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue // tolerate stray prose instead of failing the boot
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		value = strings.TrimSpace(value)
		value = stripInlineComment(value)
		value = unquoteEnvValue(value)
		if _, exists := os.LookupEnv(key); exists {
			continue // explicit process env always wins
		}
		if err := os.Setenv(key, value); err != nil {
			return applied, fmt.Errorf("line %d: %w", lineNo, err)
		}
		applied++
	}
	return applied, scanner.Err()
}

// stripInlineComment removes a trailing ` # comment` from an unquoted value.
// Quoted values keep their content verbatim (handled by unquoteEnvValue).
func stripInlineComment(v string) string {
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'") {
		return v
	}
	if i := strings.Index(v, " #"); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return v
}

func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
