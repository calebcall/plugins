package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveRecordingsBaseDir_EmptyConfigured_UsesDefault proves an unset
// (empty string) recordingPath falls back to defaultDir unchanged — the
// pre-existing behavior every camera already recording relies on.
func TestResolveRecordingsBaseDir_EmptyConfigured_UsesDefault(t *testing.T) {
	got := resolveRecordingsBaseDir("/default/storage", "", nil)
	if got != "/default/storage" {
		t.Fatalf("expected empty configured path to resolve to the default, got %q", got)
	}
}

// TestResolveRecordingsBaseDir_ConfiguredAndWritable_UsesConfigured proves a
// non-empty, actually-creatable-and-writable configured path is used as-is.
func TestResolveRecordingsBaseDir_ConfiguredAndWritable_UsesConfigured(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "custom-recordings")

	got := resolveRecordingsBaseDir("/default/storage", configured, nil)
	if got != configured {
		t.Fatalf("expected the configured path to be used, got %q", got)
	}

	info, err := os.Stat(configured)
	if err != nil || !info.IsDir() {
		t.Fatalf("expected resolveRecordingsBaseDir to have created %q, stat error: %v", configured, err)
	}
}

// TestResolveRecordingsBaseDir_CreatesMissingParents proves a configured
// path several directories deep, none of which exist yet, is created in
// full (os.MkdirAll semantics) rather than only the leaf directory.
func TestResolveRecordingsBaseDir_CreatesMissingParents(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "a", "b", "c")

	got := resolveRecordingsBaseDir("/default/storage", configured, nil)
	if got != configured {
		t.Fatalf("expected the configured path to be used, got %q", got)
	}
	if info, err := os.Stat(configured); err != nil || !info.IsDir() {
		t.Fatalf("expected every missing parent directory to be created, stat error: %v", err)
	}
}

// TestResolveRecordingsBaseDir_UnwritableConfigured_FallsBackToDefault
// proves a configured path that cannot be written to (here: an existing
// path that is a FILE, not a directory, so MkdirAll fails) falls back to
// defaultDir rather than being used anyway.
func TestResolveRecordingsBaseDir_UnwritableConfigured_FallsBackToDefault(t *testing.T) {
	// A regular file at this path means MkdirAll(configured, ...) fails
	// (can't mkdir where a file already exists) — ensureWritableDir's
	// create-directory step.
	configuredAsFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(configuredAsFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed fixture file: %v", err)
	}

	got := resolveRecordingsBaseDir("/default/storage", configuredAsFile, nil)
	if got != "/default/storage" {
		t.Fatalf("expected an unusable configured path to fall back to the default, got %q", got)
	}
}

// TestResolveRecordingsBaseDir_ReadOnlyDirectory_FallsBackToDefault proves
// an EXISTING directory that MkdirAll happily reports success against (no
// create needed) but that isn't actually writable is still detected and
// falls back — the exact gap a bare MkdirAll-only check would miss, which
// is why ensureWritableDir also probes with a real file write.
func TestResolveRecordingsBaseDir_ReadOnlyDirectory_FallsBackToDefault(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: directory permissions don't block writes")
	}

	readOnlyDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(readOnlyDir, 0o555); err != nil {
		t.Fatalf("seed read-only fixture dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyDir, 0o755) })

	got := resolveRecordingsBaseDir("/default/storage", readOnlyDir, nil)
	if got != "/default/storage" {
		t.Fatalf("expected a read-only configured directory to fall back to the default, got %q", got)
	}
}

// TestResolveRecordingsBaseDir_NilLoggerOnFallback_DoesNotPanic proves the
// fallback path tolerates a nil *sdk.Logger (the unit-test convention every
// other optional logger dependency in this package already establishes)
// without panicking.
func TestResolveRecordingsBaseDir_NilLoggerOnFallback_DoesNotPanic(t *testing.T) {
	configuredAsFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(configuredAsFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed fixture file: %v", err)
	}

	resolveRecordingsBaseDir("/default/storage", configuredAsFile, nil)
}
