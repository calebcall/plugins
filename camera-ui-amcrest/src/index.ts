import { API_EVENT, BasePlugin } from '@camera.ui/sdk';

import { buildCameraConfig } from './adopt.js';
import { AmcrestClient } from './amcrest/api.js';
import { discover } from './amcrest/discovery.js';
import { AmcrestCamera } from './camera.js';

import type {
  CameraConfig,
  CameraDevice,
  DeviceStorage,
  DiscoveredCamera,
  DiscoveryProvider,
  JsonSchemaWithoutCallbacks,
  LoggerService,
  PluginAPI,
} from '@camera.ui/sdk';

const DISCOVERY_TIMEOUT_MS = 5000;

export default class AmcrestPlugin extends BasePlugin implements DiscoveryProvider {
  private cameras = new Map<string, AmcrestCamera>();
  private existing = new Map<string, CameraDevice>();

  constructor(logger: LoggerService, api: PluginAPI, storage: DeviceStorage<any>) {
    super(logger, api, storage);
    this.api.on(API_EVENT.SHUTDOWN, this.stop.bind(this));
  }

  async configureCameras(cameras: CameraDevice[]): Promise<void> {
    for (const camera of cameras) {
      this.existing.set(camera.id, camera);
      await this.initCamera(camera);
    }
  }

  async onCameraAdded(camera: CameraDevice): Promise<void> {
    this.existing.set(camera.id, camera);
    await this.initCamera(camera);
  }

  async onCameraReleased(cameraId: string): Promise<void> {
    this.cameras.get(cameraId)?.destroy();
    this.cameras.delete(cameraId);
    this.existing.delete(cameraId);
  }

  async onDiscoverCameras(): Promise<DiscoveredCamera[]> {
    const devices = await discover(DISCOVERY_TIMEOUT_MS, { debug: (...a: unknown[]) => this.logger.debug(...a) });
    return devices
      .filter((d) => !Array.from(this.existing.values()).some((c) => c.nativeId === `amcrest-${d.ip}`))
      .map((d) => ({ id: `amcrest-${d.ip}`, name: d.deviceType ? `Amcrest ${d.deviceType}` : `Amcrest (${d.ip})`, model: d.deviceType, address: d.ip }));
  }

  async onGetCameraSettings(_camera: DiscoveredCamera): Promise<JsonSchemaWithoutCallbacks[]> {
    return [
      { type: 'string', key: 'ip', title: 'IP Address', description: 'Camera IP address, e.g. 192.168.1.50', required: true },
      { type: 'string', key: 'username', title: 'Username', description: 'Amcrest account username.', required: true },
      { type: 'string', format: 'password', key: 'password', title: 'Password', description: 'Amcrest account password.', required: true },
      { type: 'number', key: 'channel', title: 'Channel', description: 'Camera channel (default 1).', required: false, defaultValue: 1 },
    ];
  }

  async onAdoptCamera(camera: DiscoveredCamera, settings: Record<string, unknown>): Promise<CameraConfig> {
    const ip = (settings.ip as string) || camera.address;
    const username = settings.username as string;
    const password = settings.password as string;
    const channel = (settings.channel as number) || 1;
    if (!ip || !username || !password) {
      throw new Error('IP address, username and password are required');
    }

    const client = new AmcrestClient({ ip, username, password });
    const info = await client.getSystemInfo(); // throws 'not amcrest' if wrong device
    const streams = await client.getStreams(channel);
    if (streams.length === 0) {
      throw new Error('No enabled video streams found on the device');
    }

    const name = camera.name || (info.deviceType ? `Amcrest ${info.deviceType}` : `Amcrest (${ip})`);
    const config = buildCameraConfig({
      name,
      nativeId: `amcrest-${ip}`,
      ip,
      username,
      password,
      port: 554,
      channel,
      info: { manufacturer: 'Amcrest', model: info.deviceType, serialNumber: info.serialNumber, firmwareVersion: info.hardwareVersion },
      streams,
    });
    this.logger.log(`Amcrest device adopted: ${name} (${streams.length} stream(s))`);
    return config;
  }

  private async initCamera(camera: CameraDevice): Promise<void> {
    const controller = new AmcrestCamera(camera);
    this.cameras.set(camera.id, controller);
    await controller.initialize();
  }

  private async stop(): Promise<void> {
    for (const c of this.cameras.values()) c.destroy();
    this.cameras.clear();
  }
}
