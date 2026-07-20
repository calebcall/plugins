import { PluginInterface, PluginRole, PluginCapability } from '@camera.ui/sdk';

import type { PluginContract } from '@camera.ui/sdk';

export const contract: PluginContract = {
  name: 'NVR (Local)',
  role: PluginRole.Hub,
  provides: [],
  consumes: [],
  // OAuthCapable (Feature #2): implements the OAuthCapable base interface
  // (getOAuthMetadata/getOAuthState/disconnect — see src/oauth.go) purely so
  // the core UI's License & Cloud panel renders "Connected as Local" instead
  // of "Not connected". Deliberately NOT PluginInterface.OAuthDeviceFlow (or
  // any other flow sub-interface) — this plugin implements no real
  // authentication flow at all, just a fixed, always-connected state.
  interfaces: [PluginInterface.NVR, PluginInterface.Notifier, PluginInterface.OAuthCapable],
  capabilities: [PluginCapability.PublishNotifications],
};

export default contract;
