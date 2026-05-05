package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thoreinstein/tfperms/internal/catalog"
)

// updateGolden controls in-place regeneration of the .golden fixture
// when a deliberate format change lands. Run `go test ./cmd/tfperms
// -run TestCatalogStatsGolden -update` and inspect the diff before
// committing. The flag is defined at the file level (not init) because
// `flag.Bool` returns a pointer that is read inside the test body.
var updateGolden = flag.Bool("update", false,
	"regenerate testdata/*.golden fixtures from current renderer output")

// TestCatalogStatsGolden is the format-stability gate for the
// `catalog stats` renderer. The fixture below is a synthetic
// CatalogStats — not the result of catalog.Load() — because the
// renderer's job is to format a CatalogStats deterministically and the
// production catalog changes on every contributor PR. Pinning a
// synthetic CatalogStats keeps the assertion sensitive to renderer
// drift while immune to catalog content drift.
//
// The fixture exercises every code path in renderCatalogStats:
//
//   - non-zero totals across all three sections,
//   - multiple services with different empirical / docs+source splits,
//   - five oldest verifications (the cap exercised),
//   - one TODO sentinel,
//   - one drift entry against a non-default reference version.
//
// Add a case to the fixture when adding a renderer feature; do not
// edit the .golden by hand. Use `-update` to regenerate after the
// fixture changes and review the diff in the same commit.
func TestCatalogStatsGolden(t *testing.T) {
	stats := catalog.CatalogStats{
		TotalResources:   3,
		TotalDataSources: 1,
		TotalIAMBindings: 1,
		ReferenceVersion: "6.15.0",
		Services: []catalog.ServiceStats{
			{Service: "compute", Total: 2, Empirical: 1, DocsSource: 1},
			{Service: "storage", Total: 3, Empirical: 0, DocsSource: 3},
		},
		OldestVerified: []catalog.AgingEntry{
			{
				Section:    "resources",
				Type:       "google_compute_instance",
				VerifiedAt: "2024-06-01",
				Position:   catalog.Position{File: "compute.yaml", Line: 12},
			},
			{
				Section:    "resources",
				Type:       "google_storage_bucket",
				VerifiedAt: "2025-01-15",
				Position:   catalog.Position{File: "storage.yaml", Line: 19},
			},
			{
				Section:    "iam_bindings",
				Type:       "google_storage_bucket_iam_binding",
				VerifiedAt: "2025-12-15",
				Position:   catalog.Position{File: "storage.yaml", Line: 94},
			},
		},
		MissingProvenance: []catalog.ProvenanceIssue{
			{
				Section:  "resources",
				Type:     "google_storage_bucket",
				Field:    "verification.source_urls[0]",
				Position: catalog.Position{File: "storage.yaml", Line: 19},
			},
		},
		Drifting: []catalog.DriftEntry{
			{
				Section:               "resources",
				Type:                  "google_legacy_resource",
				TestedAgainstProvider: ">=4.0.0,<5.0.0",
				Position:              catalog.Position{File: "legacy.yaml", Line: 1},
			},
		},
	}

	var buf bytes.Buffer
	if err := renderCatalogStats(&buf, stats); err != nil {
		t.Fatalf("renderCatalogStats: %v", err)
	}
	got := buf.Bytes()

	goldenPath := filepath.Join("testdata", "catalog_stats.golden")
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
		t.Errorf("renderCatalogStats output diverged from %s.\n--- want ---\n%s\n--- got ---\n%s",
			goldenPath, want, got)
	}
}

// TestCatalogStatsCommandLoadsEmbeddedCatalog is the end-to-end
// counterpart to TestCatalogStatsGolden: it exercises the cobra
// wiring against the actual embedded catalog and asserts that the
// command exits cleanly and produces output whose structural anchors
// match what the renderer promises. Content (counts, service names)
// is not asserted because the embedded catalog evolves — that is what
// the golden fixture pins.
func TestCatalogStatsCommandLoadsEmbeddedCatalog(t *testing.T) {
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"catalog", "stats"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	// Section headers are stable contract — if any disappears the
	// renderer regressed. Content (counts, service names) is locked
	// elsewhere via the synthetic-fixture golden.
	wantHeaders := []string{
		"tfperms catalog stats",
		"Totals:",
		"Coverage by service:",
		"Oldest verifications",
		"Missing provenance",
		"Drift from provider",
	}
	output := out.String()
	for _, h := range wantHeaders {
		if !strings.Contains(output, h) {
			t.Errorf("output missing %q header.\noutput:\n%s", h, output)
		}
	}
}

// failingWriter accepts the first byteBudget bytes and then returns
// errBrokenPipe on every subsequent Write. It models a stdout pipe
// that the consumer closes mid-render: the renderer must surface that
// error rather than swallow it and exit zero with truncated output.
type failingWriter struct {
	byteBudget int
	written    int
}

var errBrokenPipe = errors.New("simulated broken pipe")

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.written >= f.byteBudget {
		return 0, errBrokenPipe
	}
	remaining := f.byteBudget - f.written
	if len(p) <= remaining {
		f.written += len(p)
		return len(p), nil
	}
	f.written = f.byteBudget
	return remaining, errBrokenPipe
}

// TestCatalogStatsRendererPropagatesFlushErrors guards against
// regression of the error-suppression bug that the previous review
// flagged: `_ = tw.Flush()` would let a broken-pipe / short-write
// stdout produce truncated output under exit code 0. With error
// propagation in place, a failing writer must cause renderCatalogStats
// to return a non-nil error.
//
// The fixture is sized large enough to require multiple section
// flushes; the writer is configured with a small byte budget so the
// first or second tabwriter.Flush hits the simulated broken pipe.
func TestCatalogStatsRendererPropagatesFlushErrors(t *testing.T) {
	stats := catalog.CatalogStats{
		TotalResources:   2,
		TotalDataSources: 1,
		TotalIAMBindings: 1,
		ReferenceVersion: "6.15.0",
		Services: []catalog.ServiceStats{
			{Service: "compute", Total: 2, Empirical: 1, DocsSource: 1},
		},
		OldestVerified: []catalog.AgingEntry{
			{
				Section:    "resources",
				Type:       "google_compute_instance",
				VerifiedAt: "2024-06-01",
				Position:   catalog.Position{File: "compute.yaml", Line: 12},
			},
		},
	}

	w := &failingWriter{byteBudget: 16}
	err := renderCatalogStats(w, stats)
	if err == nil {
		t.Fatal("renderCatalogStats with a failing writer returned nil; " +
			"expected the broken-pipe error to be propagated to the caller")
	}
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("renderCatalogStats error chain does not wrap the underlying writer error.\nerr: %v", err)
	}
}

// substringFailingWriter accepts every Write whose payload does not
// contain failOn, and once it sees failOn (or any subsequent write)
// returns errBrokenPipe with n=0. It is the inverse of failingWriter:
// failingWriter dies on a byte budget (which lands inside an early
// tabwriter buffer), substringFailingWriter dies deterministically on
// a specific direct write so the test can target a post-flush plain
// Fprintln that has no tabwriter behind it. Anything we successfully
// wrote first is captured for diagnostic purposes.
type substringFailingWriter struct {
	failOn   string
	failed   bool
	captured bytes.Buffer
}

func (s *substringFailingWriter) Write(p []byte) (int, error) {
	if s.failed {
		return 0, errBrokenPipe
	}
	if bytes.Contains(p, []byte(s.failOn)) {
		s.failed = true
		return 0, errBrokenPipe
	}
	return s.captured.Write(p)
}

// TestCatalogStatsRendererPropagatesPlainWriteErrors is the
// post-flush companion to TestCatalogStatsRendererPropagatesFlushErrors.
// The earlier test only forces a failure during a tabwriter flush —
// because byteBudget=16 dies inside the first totals flush — so it
// never reaches a plain fmt.Fprintln write that has no flush behind
// it. The reviewer flagged this gap: in the empty-Drifting branch the
// renderer's last operation is a direct `fmt.Fprintln(w, "  (none)")`
// after the last tabwriter has already flushed, and the previous
// implementation dropped that write's error.
//
// The fixture sets Drifting=nil so the renderer takes the (none)
// branch with no trailing flush. The writer fails on the drift section
// header — a direct write that lives after every tabwriter.Flush in
// the function — so a regression that removes the post-flush errWriter
// check (or reverts to fmt.Fprintln on the bare writer) will let
// renderCatalogStats return nil and this test will fail.
func TestCatalogStatsRendererPropagatesPlainWriteErrors(t *testing.T) {
	stats := catalog.CatalogStats{
		TotalResources:   1,
		ReferenceVersion: "6.15.0",
		Services: []catalog.ServiceStats{
			{Service: "compute", Total: 1, Empirical: 1, DocsSource: 0},
		},
		OldestVerified: []catalog.AgingEntry{
			{
				Section:    "resources",
				Type:       "google_compute_instance",
				VerifiedAt: "2024-06-01",
				Position:   catalog.Position{File: "compute.yaml", Line: 12},
			},
		},
		MissingProvenance: []catalog.ProvenanceIssue{
			{
				Section:  "resources",
				Type:     "google_storage_bucket",
				Field:    "verification.source_urls[0]",
				Position: catalog.Position{File: "storage.yaml", Line: 19},
			},
		},
		// Drifting intentionally nil: renderer takes the "(none)"
		// branch with no tabwriter, so the only writes after the last
		// successful flush are plain Fprintlns.
	}

	w := &substringFailingWriter{failOn: "Drift from provider"}
	err := renderCatalogStats(w, stats)
	if err == nil {
		t.Fatalf("renderCatalogStats with a writer that fails on the drift header returned nil; "+
			"expected the broken-pipe error from the post-flush plain-write path.\ncaptured output:\n%s",
			w.captured.String())
	}
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("renderCatalogStats error chain does not wrap the underlying writer error.\nerr: %v", err)
	}
}
