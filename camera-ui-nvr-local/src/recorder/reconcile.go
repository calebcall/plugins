package recorder

import "time"

// defaultReconcileInterval is how often the background reconcile pass
// (StartReconcile) re-reads every managed camera's stored recording config
// and brings its live recorder in line. A minute is frequent enough that a
// camera the user just finished configuring starts recording within ~a
// minute, without re-hitting each camera's DeviceStorage (an RPC to the host)
// more often than necessary.
const defaultReconcileInterval = 60 * time.Second

// Reconcile re-reads every managed camera's stored recording config and
// brings live recorders in line with it:
//
//   - a camera that SHOULD be recording (mode != off) but has no active
//     recorder is started — this is the fix for the gap Add documents:
//     a camera adopted at runtime (OnCameraAdded) whose recordingMode was
//     still the default "off" at add time, then later switched to
//     "continuous"/"events" in the UI, has no SDK hook to trigger its
//     recorder; this pass picks that up on its next tick. It also retries a
//     camera whose earlier start failed (e.g. its stream wasn't ready at
//     OnCameraAdded time — previously that needed a full plugin restart).
//   - a camera whose mode is now "off" but still has an active recorder is
//     stopped (it stays registered, so retention still cleans its footage).
//
// It deliberately does NOT restart an already-active recorder to pick up a
// roles/pre-roll edit — only mode on/off transitions — so it never churns a
// healthy ffmpeg process on every tick. A roles change to an
// already-recording camera still takes effect on the next restart, same as
// before this pass existed.
//
// A no-op before StartAll has run (nothing has been launched to reconcile
// against yet). Safe to call directly (tests do); production calls it on a
// timer via StartReconcile.
func (m *RecorderManager) Reconcile() {
	m.mu.RLock()
	launched := m.launched
	m.mu.RUnlock()
	if !launched {
		return
	}

	for _, entry := range m.entriesSnapshot() {
		// Re-read the camera's CURRENT stored config — its mode may have
		// changed since it was registered (the whole point of this pass).
		if entry.Storage != nil {
			cfg := readRecordingConfig(entry.Storage)
			m.updateConfig(entry.CameraID, cfg)
			entry.Config = cfg
		}

		desired := entry.Config.Mode != RecordingModeOff
		active := m.IsActive(entry.CameraID)

		switch {
		case desired && !active:
			// Should be recording but isn't — start it. startOrRestartRecorder's
			// "stop" half is a no-op here (nothing active), and it handles the
			// per-camera lock + start-state notification itself.
			if err := m.startOrRestartRecorder(entry); err != nil {
				m.warnf("recorder: reconcile: camera %s start failed: %v", entry.CameraID, err)
			}
		case !desired && active:
			// Mode switched to off — stop it, but keep it registered so
			// retention still ages out its footage.
			m.stopActiveKeepRegistered(entry.CameraID)
			m.logf("recorder: reconcile: stopped camera %s (mode now off)", entry.CameraID)
		}
	}
}

// updateConfig replaces the stored Config for a registered camera so
// ManagedCameraIDs / IsRecording reflect a mode change picked up by
// Reconcile. A no-op for an unknown/removed id.
func (m *RecorderManager) updateConfig(cameraID string, cfg RecordingConfig) {
	m.mu.Lock()
	if r, ok := m.recorders[cameraID]; ok {
		r.Config = cfg
	}
	m.mu.Unlock()
}

// stopActiveKeepRegistered stops cameraID's active recorder (if any) under
// its per-camera lock and fires the stop notification once the lock is
// released — the same lock-scope discipline as Remove, but WITHOUT
// unregistering the camera (Reconcile stops an off-mode camera but leaves it
// in the registry so retention keeps managing its old footage).
func (m *RecorderManager) stopActiveKeepRegistered(cameraID string) {
	lock := m.camLock(cameraID)
	lock.Lock()
	stopped := m.stopRecorder(cameraID)
	lock.Unlock()

	if stopped {
		m.notifyState(cameraID, false)
	}
}

// StartReconcile launches the background reconciliation ticker: every
// interval (defaultReconcileInterval when <= 0), it runs Reconcile. Intended
// to be started once, right after StartAll, so runtime camera add/config
// changes take effect without a plugin restart. Idempotent — a second call
// while already running is a no-op. Uses the same cancel-then-wait ticker
// shape as StartRetention, so StopReconcile is leak-safe.
func (m *RecorderManager) StartReconcile(interval time.Duration) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	if m.reconcileRunning {
		return
	}

	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	newTicker := m.reconcileNewTicker
	if newTicker == nil {
		newTicker = newRealTicker
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	m.reconcileCancel = func() { close(stop) }
	m.reconcileDone = done
	m.reconcileRunning = true

	go func() {
		defer close(done)
		t := newTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C():
				m.Reconcile()
			}
		}
	}()
}

// StopReconcile cancels the ticker started by StartReconcile and blocks until
// its goroutine has fully exited — cannot return while a reconcile goroutine
// is still running, so a shutdown caller never leaves it behind. Idempotent:
// a no-op when not running.
func (m *RecorderManager) StopReconcile() {
	m.reconcileMu.Lock()
	if !m.reconcileRunning {
		m.reconcileMu.Unlock()
		return
	}
	cancel := m.reconcileCancel
	done := m.reconcileDone
	m.reconcileRunning = false
	m.reconcileCancel = nil
	m.reconcileDone = nil
	m.reconcileMu.Unlock()

	cancel()
	<-done
}
