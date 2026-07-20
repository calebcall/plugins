package main

import (
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// TestGetOAuthState_ReportsConnectedAsLocal proves GetOAuthState — the
// method the frontend's useOAuth(pluginName) polls — always reports a
// connected state with UserEmail "Local", the exact combination the
// License & Cloud panel renders as "Connected as Local".
func TestGetOAuthState_ReportsConnectedAsLocal(t *testing.T) {
	p := newTestPlugin(t)

	state, err := p.GetOAuthState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil {
		t.Fatalf("expected a non-nil OAuthState")
	}
	if state.Status != sdk.OAuthStatusConnected {
		t.Fatalf("expected Status %q, got %q", sdk.OAuthStatusConnected, state.Status)
	}
	if state.UserEmail != "Local" {
		t.Fatalf("expected UserEmail %q, got %q", "Local", state.UserEmail)
	}
	if state.ConnectedAt <= 0 {
		t.Fatalf("expected a positive ConnectedAt timestamp, got %d", state.ConnectedAt)
	}
}

// TestGetOAuthMetadata_ReturnsSaneNonNilMetadata proves GetOAuthMetadata
// never returns nil (which would be a wire-serialization footgun for the
// host's connect dialog) and reports a non-empty display name, a
// non-required connection, and no flow sub-interfaces (this plugin
// implements only the OAuthCapable base interface).
func TestGetOAuthMetadata_ReturnsSaneNonNilMetadata(t *testing.T) {
	p := newTestPlugin(t)

	meta, err := p.GetOAuthMetadata()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta == nil {
		t.Fatalf("expected a non-nil OAuthMetadata")
	}
	if meta.IdpDisplayName == "" {
		t.Fatalf("expected a non-empty IdpDisplayName")
	}
	if meta.ScopeDescriptions == nil {
		t.Fatalf("expected a non-nil (even if empty) ScopeDescriptions map")
	}
	if len(meta.SupportedFlows) != 0 {
		t.Fatalf("expected no supported flow sub-interfaces, got %v", meta.SupportedFlows)
	}
}

// TestDisconnect_IsANoOp proves Disconnect always succeeds without needing
// any prior "connected" state — there is nothing to revoke.
func TestDisconnect_IsANoOp(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.Disconnect(); err != nil {
		t.Fatalf("expected Disconnect to be a no-op, got error: %v", err)
	}
}

// TestNVRPlugin_ImplementsOAuthCapable is a compile-time-adjacent runtime
// check that *NVRPlugin genuinely satisfies sdk.OAuthCapable — the
// interface the frontend's useOAuth/License & Cloud panel depends on this
// plugin implementing.
func TestNVRPlugin_ImplementsOAuthCapable(t *testing.T) {
	var _ sdk.OAuthCapable = newTestPlugin(t)
}

// TestRPCMethods_IncludesOAuthCapableWireNames proves the three
// OAuthCapable methods are explicitly allow-listed in RPCMethods() — see
// oauth.go's package doc comment for why declaring OAuthCapable alone
// (contract.ts) does not put them on the wire: rpc/go's ExtractMethods has
// no auto-registration for that interface, and NVRPlugin implements
// RPCMethodAllowlist, so anything not listed here stays Go-callable but
// invisible to the frontend's poll.
func TestRPCMethods_IncludesOAuthCapableWireNames(t *testing.T) {
	p := newTestPlugin(t)
	allowed := map[string]bool{}
	for _, name := range p.RPCMethods() {
		allowed[name] = true
	}

	for _, want := range []string{"getOAuthMetadata", "getOAuthState", "disconnect"} {
		if !allowed[want] {
			t.Fatalf("expected RPCMethods() to include %q, got %v", want, p.RPCMethods())
		}
	}
}
