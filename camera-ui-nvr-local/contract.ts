import { PluginInterface, PluginRole, PluginCapability } from '@camera.ui/sdk';

import type { PluginContract } from '@camera.ui/sdk';

export const contract: PluginContract = {
  name: 'NVR (Local)',
  role: PluginRole.Hub,
  provides: [],
  consumes: [],
  interfaces: [PluginInterface.NVR, PluginInterface.Notifier],
  capabilities: [PluginCapability.PublishNotifications],
};

export default contract;
