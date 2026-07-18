export interface AmcrestCameraStorage {
  ip: string;
  username: string;
  password: string;
  port?: number;
  httpPort?: number;
  channel?: number;
}

// Adoption-form fields resolved during onAdoptCamera, bridged from the plugin's
// pendingSettings map into AmcrestCamera.initialize() so they get persisted to
// storage (the SDK only persists the returned CameraConfig, not these fields).
export interface AmcrestInitialSettings {
  ip: string;
  username: string;
  password: string;
  channel?: number;
  port?: number;
  httpPort?: number;
}

export interface AmcrestCapabilities {
  deviceType?: string;
  doorbell: boolean;
  ptz: boolean;
  ptzPan: boolean;
  ptzTilt: boolean;
  ptzZoom: boolean;
}
