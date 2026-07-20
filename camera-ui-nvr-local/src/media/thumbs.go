// Package media generates and persists the primary JPEG thumbnail for a
// detection event (Task 11): given the event's camera and timestamp, it
// finds the recorded segment covering that moment and extracts a single
// frame from it with ffmpeg, then stores the resulting JPEG on disk and
// records its path on the event's row so getEventThumbnails
// (rpc_events.go, camera-ui-nvr-local's main package) can serve it later.
//
// Nothing here duplicates ffmpeg binary path resolution — callers pass in
// the already-resolved path (see recorder.ResolveFFmpegSDK/ResolveFFmpeg,
// the single source of truth for resolving ffmpeg via the SDK's
// CoreManager.GetFFmpegPath RPC with an env/PATH fallback; see that
// package's ffmpeg.go for why). This package never uses ffprobe either —
// node-av (the core's bundled media toolchain) ships no ffprobe binary at
// all, and frame extraction only ever needs ffmpeg's own -ss/-frames:v.
package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/store"
)

// ThumbsDirName is the subdirectory (under the plugin's storage/data
// directory) generated event thumbnails are written under.
const ThumbsDirName = "notification-thumbs"

// defaultGenerateTimeout bounds how long a single ffmpeg frame-extraction
// invocation is allowed to run before it's killed: thumbnail generation is
// best-effort and must never let a hung/slow ffmpeg process stall or block
// detection-event ingestion (see GenerateAsync's doc comment) indefinitely.
const defaultGenerateTimeout = 8 * time.Second

// jpegQuality is the ffmpeg mjpeg encoder's -q:v value used for every
// extracted frame: qscale ranges 2 (best/largest) .. 31 (worst/smallest);
// 4 is a good size/quality tradeoff for a small notification/event-list
// thumbnail.
const jpegQuality = "4"

// SegmentFinder is the subset of *store.SegmentStore Generator needs:
// finding the recorded segment (if any) covering an event's timestamp.
// *store.SegmentStore satisfies this directly.
type SegmentFinder interface {
	CoveringSegment(cameraID string, atMs int64) (store.Segment, bool, error)
}

// ThumbRefSetter is the subset of *store.EventStore Generator needs:
// writing back a generated thumbnail's on-disk path once persisted.
// *store.EventStore satisfies this directly.
type ThumbRefSetter interface {
	SetThumbRef(eventID, thumbRef string) error
}

// commandRunner abstracts running one ffmpeg frame-extraction invocation so
// tests can inject a fake for the non-ffmpeg-dependent cases (e.g. "no
// covering segment") while the TDD-required real-media test still exercises
// a genuine ffmpeg process via execCommandRunner.
type commandRunner interface {
	Run(ctx context.Context, name string, args []string) error
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// Generator produces and persists a primary JPEG thumbnail for a
// DetectionEvent: it looks up the recorded segment covering the event's
// start timestamp (segments.CoveringSegment), extracts a single frame from
// it at the right offset via ffmpeg, writes the resulting JPEG under
// dir/<eventID>.jpg, and records that path on the event via
// events.SetThumbRef.
//
// Generation is best-effort and never a reason to lose or fail the event
// itself: every error Generate encounters (no covering segment, ffmpeg
// failure, a write error, ...) is logged (when log is non-nil) and
// returned, but GenerateAsync — the entry point event ingestion actually
// calls (events_ingest.go) — swallows it entirely. A missing covering
// segment (event before/without recording, or one landing inside the
// still-open segment ffmpeg hasn't finalized yet) is not logged as an error
// at all: it is the expected, common "nothing to do yet" case.
type Generator struct {
	ffmpegPath string
	segments   SegmentFinder
	events     ThumbRefSetter
	dir        string
	timeout    time.Duration
	log        *sdk.Logger
	runner     commandRunner

	wg sync.WaitGroup

	// mu/done deduplicate generation attempts per event ID: once a
	// thumbnail has been successfully generated and persisted for an
	// event, later calls (e.g. a subsequent lifecycle message for the same
	// event) are no-ops rather than re-running ffmpeg. An event whose
	// earlier attempt found no covering segment is NOT marked done, so a
	// later lifecycle message (once the segment has had time to finalize)
	// gets another chance.
	mu   sync.Mutex
	done map[string]bool
}

// NewGenerator returns a Generator that reads segments via segments, writes
// generated JPEGs under filepath.Join(dataDir, ThumbsDirName), and records
// each one's path via events.SetThumbRef, using ffmpegPath (the resolved
// ffmpeg binary — see recorder.ResolveFFmpeg().Path(); this package never
// resolves that path itself) to extract frames. log may be nil.
func NewGenerator(dataDir, ffmpegPath string, segments SegmentFinder, events ThumbRefSetter, log *sdk.Logger) *Generator {
	return &Generator{
		ffmpegPath: ffmpegPath,
		segments:   segments,
		events:     events,
		dir:        filepath.Join(dataDir, ThumbsDirName),
		timeout:    defaultGenerateTimeout,
		log:        log,
		runner:     execCommandRunner{},
	}
}

// GenerateAsync dispatches Generate for event in its own goroutine and
// returns immediately, swallowing (after logging) any error Generate
// returns — the fire-and-forget entry point event ingestion
// (events_ingest.go's detectionEventIngester.handle) calls on every
// lifecycle message, so a slow, hung, or failing ffmpeg process can never
// block or fail event ingestion itself. Tests use Wait to deterministically
// await completion instead of sleeping/polling; production code never calls
// Wait.
func (g *Generator) GenerateAsync(event store.DetectionEvent) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := g.Generate(context.Background(), event); err != nil {
			g.logf("media: generate thumbnail for event %s: %v", event.ID, err)
		}
	}()
}

// Wait blocks until every GenerateAsync call dispatched so far has
// completed. Only ever called by tests, to synchronize assertions against
// background generation without sleeping/polling — see GenerateAsync's doc
// comment.
func (g *Generator) Wait() { g.wg.Wait() }

// Generate synchronously produces and persists event's primary JPEG
// thumbnail, bounded by ctx and g.timeout. Returns nil (not an error) when
// event already has a persisted thumbnail (see the done map's doc comment
// on Generator) or when no recorded segment covers event.StartTime — both
// are expected "nothing to do" outcomes, not failures.
func (g *Generator) Generate(ctx context.Context, event store.DetectionEvent) error {
	if g.alreadyDone(event.ID) {
		return nil
	}
	if strings.ContainsAny(event.ID, `/\`) {
		return fmt.Errorf("media: event id %q is not a safe filename component", event.ID)
	}

	seg, ok, err := g.segments.CoveringSegment(event.CameraID, event.StartTime)
	if err != nil {
		return fmt.Errorf("media: find covering segment: %w", err)
	}
	if !ok {
		// Expected: no recording covers this event (yet, or ever). Not
		// logged as an error — see the type doc comment.
		return nil
	}

	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return fmt.Errorf("media: create thumbs dir %s: %w", g.dir, err)
	}
	outPath := filepath.Join(g.dir, event.ID+".jpg")

	genCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	args := extractFrameArgs(seg, event.StartTime, outPath)
	if err := g.runner.Run(genCtx, g.ffmpegPath, args); err != nil {
		return fmt.Errorf("media: ffmpeg extract frame: %w", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Errorf("media: read generated thumbnail %s: %w", outPath, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("media: ffmpeg produced an empty thumbnail file %s", outPath)
	}

	if err := g.events.SetThumbRef(event.ID, outPath); err != nil {
		return fmt.Errorf("media: set thumb_ref for event %s: %w", event.ID, err)
	}

	g.markDone(event.ID)
	return nil
}

// extractFrameArgs builds the ffmpeg CLI args that pull exactly one frame
// out of seg.Path at the offset atMs falls at within it, encoded as a JPEG
// at outPath: "-ss <offsetSeconds> -i <segment> -frames:v 1 -q:v <n>",
// exactly the invocation shape the task brief specifies. -ss before -i is
// input seeking (fast, keyframe-granularity) — acceptable here since a
// small notification thumbnail doesn't need frame-exact precision, and
// getting the whole thing wrong (way outside the segment) is already ruled
// out by frameOffsetSeconds' clamping.
func extractFrameArgs(seg store.Segment, atMs int64, outPath string) []string {
	offset := frameOffsetSeconds(seg, atMs)
	return []string{
		"-ss", strconv.FormatFloat(offset, 'f', 3, 64),
		"-i", seg.Path,
		"-frames:v", "1",
		"-q:v", jpegQuality,
		"-y",
		outPath,
	}
}

// frameOffsetSeconds computes how far into seg (in seconds) atMs falls,
// clamped to [0, seg duration) so a slightly-out-of-range timestamp (e.g.
// an event's StartTime a moment before the segment's own recorded StartMs,
// due to independent clock sources) still resolves to a valid seek target
// inside the file rather than one ffmpeg would refuse or seek past the end
// with.
func frameOffsetSeconds(seg store.Segment, atMs int64) float64 {
	offsetMs := atMs - seg.StartMs
	if offsetMs < 0 {
		offsetMs = 0
	}
	if durMs := seg.EndMs - seg.StartMs; durMs > 0 && offsetMs >= durMs {
		offsetMs = durMs - 1
	}
	return float64(offsetMs) / 1000.0
}

func (g *Generator) alreadyDone(eventID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done[eventID]
}

func (g *Generator) markDone(eventID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done == nil {
		g.done = make(map[string]bool)
	}
	g.done[eventID] = true
}

// logf logs through g.log if one was provided (log may be nil — see
// NewGenerator's doc comment).
func (g *Generator) logf(format string, args ...any) {
	if g.log == nil {
		return
	}
	g.log.Log(fmt.Sprintf(format, args...))
}
