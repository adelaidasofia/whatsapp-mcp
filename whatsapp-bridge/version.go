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
// Overridable at build time for a local build:
//
//	go build -ldflags="-X main.bridgeVersion=$(git describe --tags)" .
//
// release.yml does NOT override it: a release binary reports this value, and
// the release workflow refuses to build a tag that is not "v" + this value. It
// reads this exact one-line declaration, so keep it in the form
// `var bridgeVersion = "X.Y.Z"` (TestReleaseCanReadBridgeVersion checks it).
var bridgeVersion = "0.5.0"
