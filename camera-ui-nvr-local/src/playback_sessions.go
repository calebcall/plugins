// playback_sessions.go holds playbackSessionRegistry: the thread-safe
// sessionID -> *playbackSession lookup nvrPlaybackCmd (rpc_playback.go)
// needs to find the session a pause/resume/speed command targets.
// NvrPlayback (rpc_playback.go) adds a session before starting its
// goroutine and removes it once that goroutine returns (session ended,
// stopped, or the client disconnected) — see NvrPlayback's own doc
// comment.
//
// Zero value is immediately usable (the map is created lazily in add()),
// matching this package's existing recorderRegistry/
// recordingStateSubscribers convention (plugin.go/subscriptions.go).
package main

import "sync"

type playbackSessionRegistry struct {
	mu   sync.Mutex
	sess map[string]*playbackSession
}

// add registers sess under its own id, replacing any previous entry with
// the same id (never expected in practice — sessionIDs are freshly
// generated UUIDs — but harmless either way).
func (r *playbackSessionRegistry) add(sess *playbackSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == nil {
		r.sess = make(map[string]*playbackSession)
	}
	r.sess[sess.id] = sess
}

// remove unregisters id. A no-op for an unknown id.
func (r *playbackSessionRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sess, id)
}

// get looks up the live session for id. ok is false when no session with
// that id is currently registered — e.g. it already ended (a real
// recording gap, the client disconnected) by the time a queued
// nvrPlaybackCmd for it arrives; NvrPlaybackCmd treats that as a
// no-op, not an error, since the frontend has no way to know exactly when
// that race is possible.
func (r *playbackSessionRegistry) get(id string) (*playbackSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sess[id]
	return s, ok
}
