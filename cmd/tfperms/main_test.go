package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestBuildMetadataDefaults locks in the dev-time defaults for the three
// build-metadata variables. If someone reverts `version` to a `const` (the
// trap called out in the implementation plan), this test will not catch it
// directly — `const` would still produce the same string here — but the
// companion TestVersionStringIncludesAllThree guarantees that the Version
// field on the cobra command threads all three vars, so a missing var would
// fail to compile.
func TestBuildMetadataDefaults(t *testing.T) {
	if version != "0.0.0-dev" {
		t.Errorf("version default = %q, want %q", version, "0.0.0-dev")
	}
	if commit != "none" {
		t.Errorf("commit default = %q, want %q", commit, "none")
	}
	if date != "unknown" {
		t.Errorf("date default = %q, want %q", date, "unknown")
	}
}

// TestVersionStringIncludesAllThree verifies that the cobra command's Version
// field surfaces all three build-metadata vars. This is what `--version`
// renders, and it is the observable proof that ldflags injection during
// `goreleaser build` reaches the user.
func TestVersionStringIncludesAllThree(t *testing.T) {
	cmd := newRootCmd()
	v := cmd.Version
	for _, want := range []string{version, commit, date} {
		if !strings.Contains(v, want) {
			t.Errorf("cmd.Version = %q, missing substring %q", v, want)
		}
	}
}

// writeFixture writes a minimal Terraform configuration into a fresh
// temp dir and returns its path. Centralised so the integration tests
// below don't each repeat the directory-and-file dance.
//
// The fixture is intentionally small (one resource, no module calls)
// because the goal of the cmd-level tests is the wiring — that the
// pipeline runs and the reporter fires — not the resolver semantics
// (covered in internal/resolver) or the format byte layout (covered in
// internal/reporter). The chosen resource (`google_storage_bucket`) is
// in the embedded catalog so the run produces a non-trivial permission
// set; if storage.yaml ever drops the entry, the tests below will fail
// loudly rather than silently exercise a no-op pipeline.
func writeFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

// TestRootCommandRunsPipeline is the end-to-end wiring test. It builds
// the root cobra command, points it at a fixture directory containing a
// known catalogued resource, captures stdout, and asserts that the
// summary line and every required permission row appear.
//
// The fixture uses google_storage_bucket because that entry is in the
// embedded catalog with a non-empty plan / create / update / delete set,
// so the run exercises every section the reporter writes. Substring
// matching (rather than byte-exact comparison) is appropriate here:
// catalog content can grow without invalidating the wiring test, and
// the byte-stable format is already pinned by reporter_test.go.
func TestRootCommandRunsPipeline(t *testing.T) {
	// uniform_bucket_level_access is set to a literal `false` so the
	// catalog's conditional on that attribute fires definitively as
	// "no" — without an explicit literal, the parser would mark the
	// attribute as unresolved and the resolver would surface an
	// unresolved-conditional row. Pinning it false keeps this wiring
	// test on the diagnostic-free happy path.
	dir := writeFixture(t, `
resource "google_storage_bucket" "primary" {
  name                        = "tfperms-fixture"
  location                    = "US"
  uniform_bucket_level_access = false
}
`)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{dir})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()

	// Summary line is the first line of output. The wiring test
	// asserts only the stable parts: resource count (1), zero
	// unknowns, zero unresolved conditionals. The leading
	// permission count is parsed and asserted > 0 rather than
	// pinned to a specific number — catalog entries can
	// legitimately gain or lose permissions over time, and this is
	// a wiring test, not a catalog-content test. Byte-stable
	// summary formatting is pinned by reporter_test.go.
	summary, _, ok := strings.Cut(got, "\n")
	if !ok {
		t.Fatalf("output has no summary line; got:\n%s", got)
	}
	leading, _, ok := strings.Cut(summary, " ")
	if !ok {
		t.Fatalf("summary line missing leading count; got: %q", summary)
	}
	permCount, err := strconv.Atoi(leading)
	if err != nil {
		t.Fatalf("summary line leading token %q is not an integer: %v", leading, err)
	}
	if permCount <= 0 {
		t.Errorf("summary permission count = %d, want > 0; got: %q", permCount, summary)
	}
	// The stable parts of the summary are asserted as substrings —
	// resource count, zero unknowns, zero unresolved conditionals.
	// Accept either singular or plural for "resource" so a catalog
	// shape change cannot trip this test.
	wantSummaryParts := []string{
		"for 1 resource",
		"0 unknowns",
		"0 unresolved conditionals",
	}
	for _, part := range wantSummaryParts {
		if !strings.Contains(summary, part) {
			t.Errorf("summary line missing %q; got: %q", part, summary)
		}
	}

	// Anchor on a couple of representative permission rows that are
	// unlikely to churn in the storage-bucket catalog entry. The
	// goal is presence, not exhaustiveness — a catalog edit that
	// adds rows must not break this test, but a regression that
	// drops the .create permission entirely should.
	wantRows := []string{
		"  storage.buckets.get",
		"  storage.buckets.create",
	}
	for _, row := range wantRows {
		if !strings.Contains(got, row) {
			t.Errorf("output missing row %q.\noutput:\n%s", row, got)
		}
	}

	// No diagnostic sections — the type is catalogued and the
	// configuration is fully literal.
	if strings.Contains(got, "unknown resources (") {
		t.Errorf("output unexpectedly contained 'unknown resources' header.\noutput:\n%s", got)
	}
	if strings.Contains(got, "unresolved conditionals (") {
		t.Errorf("output unexpectedly contained 'unresolved conditionals' header.\noutput:\n%s", got)
	}
}

// TestRootCommandReportsUnknown drives the unknowns path: a resource
// type the embedded catalog does not cover must appear under the
// `unknown resources` header with file:line context. Pins the
// Journey-3 contract (catalog gap discovery) at the CLI surface.
func TestRootCommandReportsUnknown(t *testing.T) {
	dir := writeFixture(t, `
resource "google_made_up_thing" "x" {
  name = "nope"
}
`)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{dir})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()

	if !strings.HasPrefix(got, "0 permissions for 1 resource, 1 unknown, 0 unresolved conditionals\n") {
		t.Errorf("summary line wrong; got:\n%s", got)
	}
	if !strings.Contains(got, "unknown resources (1):") {
		t.Errorf("output missing 'unknown resources' header.\noutput:\n%s", got)
	}
	if !strings.Contains(got, "google_made_up_thing") {
		t.Errorf("output missing unknown resource type.\noutput:\n%s", got)
	}
}

// TestRootCommandRejectsExtraArgs guards the cobra.MaximumNArgs(1)
// constraint. Two positional arguments must fail at parse time so the
// help text and the implemented surface stay aligned — accepting more
// arguments silently would be a maintainability trap (which one is
// the directory? what are the rest? a reader of the help text would
// not be able to tell).
func TestRootCommandRejectsExtraArgs(t *testing.T) {
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"a", "b"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("Execute with two positional args returned nil; expected cobra to reject the extra arg.\noutput: %s", out.String())
	}
}

// TestRootCommandDeterministic asserts that two consecutive runs
// against the same fixture produce byte-identical output. This is the
// stable-diff contract from Journey 2 (author checking a module mid-
// development) and it exercises the resolver's deterministic-sort
// promise end-to-end through the reporter. A regression that
// introduced map-iteration leakage or any other non-deterministic
// rendering would flake this test.
func TestRootCommandDeterministic(t *testing.T) {
	// uniform_bucket_level_access is set on both buckets to keep the
	// test out of the unresolved-conditional path. Determinism is
	// what is being exercised; an unresolved row would be stable
	// across runs, but adding it expands the assertion surface
	// without testing anything new.
	dir := writeFixture(t, `
resource "google_storage_bucket" "a" {
  name                        = "a"
  location                    = "US"
  uniform_bucket_level_access = false
}

resource "google_storage_bucket" "b" {
  name                        = "b"
  location                    = "US"
  uniform_bucket_level_access = false
}
`)

	run := func() string {
		root := newRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs([]string{dir})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v\noutput: %s", err, out.String())
		}
		return out.String()
	}

	first := run()
	second := run()
	if first != second {
		t.Errorf("output not deterministic across runs.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}
