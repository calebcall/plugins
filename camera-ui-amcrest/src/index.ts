import { BasePlugin } from '@camera.ui/sdk';

import type { CameraDevice } from '@camera.ui/sdk';

export default class AmcrestPlugin extends BasePlugin {
  public async configureCameras(_cameras: CameraDevice[]): Promise<void> {}

  public async onCameraAdded(_camera: CameraDevice): Promise<void> {}

  public async onCameraReleased(_cameraId: string): Promise<void> {}
}
