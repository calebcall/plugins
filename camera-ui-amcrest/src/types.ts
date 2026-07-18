export interface AmcrestCameraStorage {
  ip: string;
  username: string;
  password: string;
  port?: number;
  httpPort?: number;
  channel?: number;
}

export interface AmcrestCapabilities {
  deviceType?: string;
  doorbell: boolean;
  ptz: boolean;
  ptzPan: boolean;
  ptzTilt: boolean;
  ptzZoom: boolean;
}
