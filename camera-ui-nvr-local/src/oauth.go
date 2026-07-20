// oauth.go implements sdk.OAuthCapable (Feature #2) purely to satisfy the
// core UI's License & Cloud panel, which shows "connected as {email}" when a
// plugin's OAuth status is 'connected' and "Not connected" otherwise (see
// the frontend's useOAuth(pluginName), which polls GetOAuthState). This
// plugin has no cloud/IdP integration at all — no token is ever fetched,
// stored, or used anywhere in this codebase — so every method here is a
// fixed, static answer: "connected, as the local user Local", with no real
// authentication flow behind it. This is deliberately NOT a real OAuth
// integration; it exists solely so the panel renders a friendlier state
// than "Not connected" for an NVR that is, by design, entirely local.
//
// Wire exposure (see the casing/allow-list findings atop plugin.go):
// GetOAuthState/GetOAuthMetadata/Disconnect are ordinary exported Go methods
// on *NVRPlugin, dispatched by the exact same rpc.ExtractMethods/
// RPCMethodAllowlist mechanism as every other RPC method this plugin
// exposes — sdk.Run registers the plugin struct itself
// (client.RegisterHandler(namespaces.PluginChildRPC, plugin)), and
// rpc/go@v1.0.6's ExtractMethods (handler.go) has no OAuthCapable-specific
// branch at all (confirmed by reading it: the only special-casing is for
// map[string]any handlers, the `rpc`/`rpc_prop` struct tags, and pull-
// iterator/callback parameter shapes — nothing keys off an interface named
// OAuthCapable). Since NVRPlugin implements RPCMethodAllowlist
// (RPCMethods(), plugin.go), these three methods would NOT be reachable
// over the wire without explicit "getOAuthMetadata"/"getOAuthState"/
// "disconnect" entries there — declaring OAuthCapable in contract.ts alone
// does not auto-register anything on the Go side. See RPCMethods' own
// updated doc comment for the wire names added.
package main

import (
	"time"

	sdk "github.com/cameraui/sdk/go"
)

// localOAuthUserEmail is the fixed "identity" this plugin reports as
// connected — deliberately not a real email address: there is no user
// account, no IdP, and nothing to log into. The License & Cloud panel
// renders this verbatim as "Connected as Local".
const localOAuthUserEmail = "Local"

// localOAuthIdpDisplayName is the fixed IdP display name GetOAuthMetadata
// reports — shown in the connect dialog the panel would otherwise render
// for a real OAuth-capable plugin. "no cloud" makes it clear to anyone who
// does open that dialog that this isn't backed by an actual identity
// provider.
const localOAuthIdpDisplayName = "Local (no cloud)"

// Compile-time assertion that NVRPlugin implements sdk.OAuthCapable.
var _ sdk.OAuthCapable = (*NVRPlugin)(nil)

// GetOAuthState reports a permanently "connected" state as the fixed local
// user (see localOAuthUserEmail) — there is no real lifecycle to track (no
// flow ever starts, polls, errors, or expires), so this always returns the
// same snapshot. ConnectedAt is stamped with the current time on every call
// rather than a fixed value: nothing in this plugin persists an actual
// "connection" moment (there was never a real connection event to record),
// and a fresh timestamp is a harmless, honest way to fill a field the host
// UI doesn't currently render anyway for this panel.
func (p *NVRPlugin) GetOAuthState() (*sdk.OAuthState, error) {
	p.logRPC("getOAuthState")
	return &sdk.OAuthState{
		Status:      sdk.OAuthStatusConnected,
		UserEmail:   localOAuthUserEmail,
		ConnectedAt: time.Now().Unix(),
	}, nil
}

// GetOAuthMetadata returns minimal, honest metadata for the connect dialog
// the host UI could render: no scopes are ever requested (this plugin never
// asks for any), Required is false (this plugin functions fully without any
// "connection" at all — GetOAuthState's permanently-connected answer is
// cosmetic, not a gate on functionality), and SupportedFlows is empty since
// this plugin implements only the OAuthCapable base interface, no Device/
// AuthCode/ClientCredentials flow sub-interface (see contract.ts: only
// PluginInterface.OAuthCapable is declared, deliberately not
// PluginInterface.OAuthDeviceFlow or any other flow interface).
func (p *NVRPlugin) GetOAuthMetadata() (*sdk.OAuthMetadata, error) {
	p.logRPC("getOAuthMetadata")
	return &sdk.OAuthMetadata{
		IdpDisplayName:    localOAuthIdpDisplayName,
		ScopeDescriptions: map[string]string{},
		SupportedFlows:    []sdk.PluginInterface{},
	}, nil
}

// Disconnect is a no-op: there is no grant to revoke and no token to clear
// (this plugin never acquires one), so this only exists to satisfy
// sdk.OAuthCapable's method set. Always returns nil.
func (p *NVRPlugin) Disconnect() error {
	p.logRPC("disconnect")
	return nil
}
