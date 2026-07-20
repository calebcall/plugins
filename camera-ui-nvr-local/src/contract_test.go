package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestContract_DeclaresOAuthCapableInterface proves ../contract.ts (the
// PluginContract this plugin ships, read by the closed frontend/host to
// decide which RPC handlers/UI affordances to wire up — see
// PluginInterface's own doc comment in the SDK) declares
// PluginInterface.OAuthCapable in its interfaces list, and deliberately
// does NOT declare PluginInterface.OAuthDeviceFlow (this plugin implements
// no real device-flow — or any other — authentication, just the
// OAuthCapable base interface's fixed, always-connected state; see
// oauth.go).
//
// This package (npm run test: "go test ./...") has no TypeScript test
// runner configured (no vitest/jest in package.json), so contract.ts's own
// content can't be exercised by importing it directly the way a Go
// dependency could be — this test instead parses the declared `interfaces`
// array out of the checked-in TypeScript source text, the same guarantee a
// TS-level test would give for this specific regression (the array losing
// or never gaining PluginInterface.OAuthCapable), without needing a second
// test toolchain for one field.
func TestContract_DeclaresOAuthCapableInterface(t *testing.T) {
	path := filepath.Join("..", "contract.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)

	matches := regexp.MustCompile(`interfaces:\s*\[([^\]]*)\]`).FindStringSubmatch(src)
	if matches == nil {
		t.Fatalf("could not find an `interfaces: [...]` array in %s", path)
	}
	interfacesList := matches[1]

	if !regexp.MustCompile(`PluginInterface\.OAuthCapable\b`).MatchString(interfacesList) {
		t.Fatalf("expected contract.ts's interfaces list to declare PluginInterface.OAuthCapable, got: %s", interfacesList)
	}
	if regexp.MustCompile(`PluginInterface\.OAuthDeviceFlow\b`).MatchString(interfacesList) {
		t.Fatalf("expected contract.ts NOT to declare PluginInterface.OAuthDeviceFlow (this plugin implements no device flow), got: %s", interfacesList)
	}
}
