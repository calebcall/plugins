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
  //
  // NOT PluginInterface.Notifier: this plugin is a notification PUBLISHER
  // (PluginCapability.PublishNotifications — it calls api.NotificationManager
  // .Publish for object-detection events, delivered to the in-app notification
  // center), NOT a device-owning notifier. Declaring the Notifier interface
  // without implementing its device methods (getDevices/registerDevice/
  // sendNotification/…) made the host's NotificationManager repeatedly call
  // getDevices on us ("no responders" log spam) and made the mobile app's
  // device registration fail against us. Real background push to the camera.ui
  // app requires camera.ui's proprietary FCM/APNs cloud relay, which cannot be
  // done locally — so we stay a pure publisher.
  interfaces: [PluginInterface.NVR, PluginInterface.OAuthCapable],
  capabilities: [PluginCapability.PublishNotifications],
};

export default contract;
