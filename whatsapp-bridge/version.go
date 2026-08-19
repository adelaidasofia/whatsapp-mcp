package main

// bridgeVersion is the single source of truth for the version the bridge
// reports at /healthcheck.
//
// It used to be a string literal inline in the healthcheck handler, which
// drifted: the endpoint said 0.3.0 while the newest release tag was v0.3.1,
// plugin.json said 1.0.0, manifest.json said 0.1.0 and pyproject.toml said
// 0.1.1 — five numbers, no two the same, so "what version am I running?" had
// no answer. Everything is aligned on this value now.
//
// Overridable at build time so a release binary can carry its exact tag:
//
//	go build -ldflags="-X main.bridgeVersion=$(git describe --tags)" .
var bridgeVersion = "0.4.0"
