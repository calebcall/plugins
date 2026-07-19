package main

import "github.com/calebcall/plugins/camera-ui-nvr-local/src/store"

// wire.go holds the msgpack request/response types mirroring the frontend's
// reconstructed contract (docs/superpowers/specs/2026-07-19-nvr-frontend-contract.d.ts
// in the camera.ui repo), for the RPC handlers later tasks add to this
// package.
//
// DetectionEvent, GetEventsOptions, and GetEventsResult are declared here as
// type aliases for the canonical definitions in package store
// (src/store/events.go) rather than as separate struct types. EventStore
// (store.EventStore) already needs exactly these shapes for Upsert/Query,
// msgpack tags and all — defining them a second time here, with the same
// field names and tags, would just be a duplicate that could silently drift
// out of sync with what EventStore actually persists/returns. Aliasing keeps
// a single source of truth: this package's future getEvents/getCameraEvents
// RPC handlers can use the bare names below and pass the result straight
// through to/from store.EventStore with no conversion step.
type (
	DetectionEvent   = store.DetectionEvent
	GetEventsOptions = store.GetEventsOptions
	GetEventsResult  = store.GetEventsResult
)
