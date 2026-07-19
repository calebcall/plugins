package main

import sdk "github.com/cameraui/sdk/go"

// NVRPlugin is the minimal boot skeleton for the local NVR hub plugin.
// Storage, recording, and playback are implemented in later tasks.
type NVRPlugin struct {
	sdk.BasePlugin
}

func NewPlugin(logger *sdk.Logger, api *sdk.PluginAPI, storage *sdk.DeviceStorage) sdk.Plugin {
	p := &NVRPlugin{BasePlugin: sdk.NewBasePlugin(logger, api, storage)}

	api.On(string(sdk.APIEventFinishLaunching), func(...any) { p.Logger.Log("nvr-local: finished launching") })
	api.On(string(sdk.APIEventShutdown), func(...any) { p.Logger.Log("nvr-local: shutdown") })

	return p
}

// ConfigureCameras, OnCameraAdded and OnCameraReleased satisfy sdk.Plugin.
// This is a Hub-role plugin with no managed cameras of its own, so these are
// no-ops for now; camera-aware recording logic lands in a later task.
func (p *NVRPlugin) ConfigureCameras(cameras []*sdk.CameraDevice) error { return nil }

func (p *NVRPlugin) OnCameraAdded(camera *sdk.CameraDevice) error { return nil }

func (p *NVRPlugin) OnCameraReleased(cameraID string) error { return nil }
