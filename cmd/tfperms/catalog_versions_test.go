package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thoreinstein/tfperms/internal/catalog"
)

// TestCatalogVersionsGolden is the format-stability gate for the
// `catalog versions` renderer. The fixture below is a synthetic
// []VersionGroup — not the result of catalog.Load() — because the
// renderer's job is to format the slice deterministically and the
// production catalog changes on every contributor PR. Pinning a
// synthetic slice keeps the assertion sensitive to renderer drift
// while immune to catalog content drift.
//
// The fixture exercises every code path in renderCatalogVersions:
//
//   - multiple groups with different counts (sort key exercise),
//   - a tie on count (lexicographic tiebreak exercise),
//   - the footer always prints regardless of group count.
//
// Add a case to the fixture when adding a renderer feature; do not
// edit the .golden by hand. Use `-update` to regenerate after the
// fixture changes and review the diff in the same commit. The
// `updateGolden` flag lives in catalog_stats_test.go and is shared
// across every renderer in this package — both tests honour the same
// `-update` invocation.
func TestCatalogVersionsGolden(t *testing.T) {
	groups := []catalog.VersionGroup{
		{TestedAgainstProvider: ">=6.0.0,<7.0.0", Count: 41},
		{TestedAgainstProvider: ">=5.0.0,<7.0.0", Count: 6},
		{TestedAgainstProvider: ">=4.0.0,<5.0.0", Count: 2},
		// Tied with the row above on Count=2; lexicographic tiebreak
		// places ">=4.0.0,<5.0.0" first, so this row appears second.
		{TestedAgainstProvider: ">=4.5.0,<5.0.0", Count: 2},
	}

	var buf bytes.Buffer
	if err := renderCatalogVersions(&buf, groups); err != nil {
		t.Fatalf("renderCatalogVersions: %v", err)
	}
	got := buf.Bytes()

	goldenPath := filepath.Join("testdata", "catalog_versions.golden")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("renderCatalogVersions output diverged from %s.\n--- want ---\n%s\n--- got ---\n%s",
			goldenPath, want, got)
	}
}

// TestCatalogVersionsGoldenEmpty pins the "(none)" branch of the
// renderer. The empty census never occurs in production (the validator
// enforces a non-empty tested_against_provider on every entry), but a
// hand-rolled in-memory caller can drive the renderer with an empty
// slice and the output must remain a complete report — header,
// "(none)" placeholder, and footer — rather than degenerate to a
// single line. Pinning the layout here means a regression that drops
// the footer in the empty branch is caught at test time rather than
// surfacing as confused user output.
func TestCatalogVersionsGoldenEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCatalogVersions(&buf, []catalog.VersionGroup{}); err != nil {
		t.Fatalf("renderCatalogVersions: %v", err)
	}
	got := buf.Bytes()

	goldenPath := filepath.Join("testdata", "catalog_versions_empty.golden")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("renderCatalogVersions empty-census output diverged from %s.\n--- want ---\n%s\n--- got ---\n%s",
			goldenPath, want, got)
	}
}

// TestCatalogVersionsCommandLoadsEmbeddedCatalog is the end-to-end
// counterpart to TestCatalogVersionsGolden: it exercises the cobra
// wiring against the actual embedded catalog and asserts that the
// command exits cleanly and produces output whose structural anchors
// match what the renderer promises. Content (counts, exact constraint
// strings) is not asserted because the embedded catalog evolves —
// that is what the golden fixture pins.
func TestCatalogVersionsCommandLoadsEmbeddedCatalog(t *testing.T) {
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"catalog", "versions"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	// Section headers and footer are stable contract — if any
	// disappears the renderer regressed. Content (counts, constraint
	// strings) is locked elsewhere via the synthetic-fixture golden.
	wantAnchors := []string{
		"tfperms catalog versions",
		"tested_against_provider census:",
		"count",
		"Note: groups are matched by literal string.",
		"`tfperms catalog stats` for semantic drift detection.",
	}
	output := out.String()
	for _, h := range wantAnchors {
		if !strings.Contains(output, h) {
			t.Errorf("output missing anchor %q.\noutput:\n%s", h, output)
		}
	}
}

// TestCatalogVersionsRendererPropagatesFlushErrors guards against
// regression of the same error-suppression class that
// TestCatalogStatsRendererPropagatesFlushErrors covers for the stats
// renderer: a `_ = tw.Flush()` would let a broken-pipe / short-write
// stdout produce truncated output under exit code 0. With error
// propagation in place, a failing writer must cause
// renderCatalogVersions to return a non-nil error.
//
// The fixture is sized so the failingWriter (defined in
// catalog_stats_test.go and reused here because both renderers live in
// the same package) hits its byte budget during the tabwriter flush,
// not during a plain Fprintln before it.
func TestCatalogVersionsRendererPropagatesFlushErrors(t *testing.T) {
	groups := []catalog.VersionGroup{
		{TestedAgainstProvider: ">=6.0.0,<7.0.0", Count: 41},
		{TestedAgainstProvider: ">=5.0.0,<7.0.0", Count: 6},
	}

	w := &failingWriter{byteBudget: 16}
	err := renderCatalogVersions(w, groups)
	if err == nil {
		t.Fatal("renderCatalogVersions with a failing writer returned nil; " +
			"expected the broken-pipe error to be propagated to the caller")
	}
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("renderCatalogVersions error chain does not wrap the underlying writer error.\nerr: %v", err)
	}
}

// TestCatalogVersionsRendererPropagatesPlainWriteErrors is the
// post-flush companion: the renderer's footer is a sequence of plain
// fmt.Fprintln calls with no tabwriter behind them, so a regression
// that drops the final ew.err check would let a broken-pipe error in
// the footer go unreported. The fixture uses substringFailingWriter
// (defined in catalog_stats_test.go) to fail deterministically on the
// "Note:" header — which is reached only after every tabwriter has
// already flushed.
func TestCatalogVersionsRendererPropagatesPlainWriteErrors(t *testing.T) {
	groups := []catalog.VersionGroup{
		{TestedAgainstProvider: ">=6.0.0,<7.0.0", Count: 41},
	}

	w := &substringFailingWriter{failOn: "Note: groups are matched"}
	err := renderCatalogVersions(w, groups)
	if err == nil {
		t.Fatalf("renderCatalogVersions with a writer that fails on the footer "+
			"returned nil; expected the broken-pipe error from the post-flush "+
			"plain-write path.\ncaptured output:\n%s", w.captured.String())
	}
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("renderCatalogVersions error chain does not wrap the underlying writer error.\nerr: %v", err)
	}
}

// TestCatalogVersionsRendererPropagatesShortWrites pins the short-
// write contract: errWriter must detect writers returning
// (n < len(p), nil) and surface io.ErrShortWrite, even though such
// writers violate io.Writer's contract. Without the latch, every
// Fprintln/Fprintf would silently lose bytes and renderCatalogVersions
// would return nil — exactly the silent truncation it is supposed to
// prevent.
func TestCatalogVersionsRendererPropagatesShortWrites(t *testing.T) {
	groups := []catalog.VersionGroup{
		{TestedAgainstProvider: ">=6.0.0,<7.0.0", Count: 1},
	}

	err := renderCatalogVersions(shortWriter{}, groups)
	if err == nil {
		t.Fatal("renderCatalogVersions with a short writer returned nil; " +
			"expected io.ErrShortWrite to be latched and surfaced")
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("renderCatalogVersions error chain does not wrap io.ErrShortWrite.\nerr: %v", err)
	}
}
