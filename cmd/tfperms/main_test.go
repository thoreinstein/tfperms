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
	trimmed := strings.TrimSpace(summary)
	leading, _, ok := strings.Cut(trimmed, " ")
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
		"    storage.buckets.get",
		"    storage.buckets.create",
	}
	for _, row := range wantRows {
		if !strings.Contains(got, row) {
			t.Errorf("output missing row %q.\noutput:\n%s", row, got)
		}
	}

	// No diagnostic sections — the type is catalogued and the
	// configuration is fully literal.
	//
	// Anchor on the section-header form ("unknown resources (")
	// rather than the bare phrase: the summary line legitimately
	// contains the words "unknowns" and "unresolved conditionals"
	// with a count prefix, and a naive substring check would
	// false-positive against them.
	if strings.Contains(got, "  unknown resources (") {
		t.Errorf("output unexpectedly contained 'unknown resources' header.\noutput:\n%s", got)
	}
	if strings.Contains(got, "  unresolved conditionals (") {
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

	if !strings.HasPrefix(got, "  0 permissions for 1 resource, 1 unknown, 0 unresolved conditionals\n") {
		t.Errorf("summary line wrong; got:\n%s", got)
	}
	if !strings.Contains(got, "  unknown resources (1):") {
		t.Errorf("output missing 'unknown resources' header.\noutput:\n%s", got)
	}
	if !strings.Contains(got, "    google_made_up_thing.x") {
		t.Errorf("output missing unknown resource type.name.\noutput:\n%s", got)
	}
}

// TestRootCommandReportsWarning drives the warnings path: a parse-level
// warning (e.g. a non-local module source) must appear under the
// `warnings` header in the report. Pins the Epic 6 requirement to
// surface parser diagnostics.
func TestRootCommandReportsWarning(t *testing.T) {
	dir := writeFixture(t, `
module "remote" {
  source = "hashicorp/consul/aws"
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

	if !strings.HasPrefix(got, "  0 permissions for 0 resources, 0 unknowns, 0 unresolved conditionals\n") {
		t.Errorf("summary line wrong; got:\n%s", got)
	}
	if !strings.Contains(got, "  warnings (1):") {
		t.Errorf("output missing 'warnings' header.\noutput:\n%s", got)
	}
	if !strings.Contains(got, "    non-local module source (main.tf:2)") {
		t.Errorf("output missing expected warning summary and location.\noutput:\n%s", got)
	}
}

// TestRootCommandReportsWarningRelativePath pins the relative-path
// branch of relativizeDiags. parser.LoadRecursive runs the input dir
// through filepath.Abs before walking it, so diagnostic Subject
// filenames are absolute. If relativizeDiags compared the absolute
// filename against the raw (still-relative) input dir, filepath.Rel
// would fall into its error branch and the user would see absolute
// paths in warning rows — the bug caught by review on the previous
// iteration.
//
// We chdir into the parent of the fixture so the input ("fixture")
// is unambiguously relative, then assert that the warning location
// renders as "main.tf:2" — filepath.Rel against the absolutized
// input dir strips the "fixture/" prefix, leaving the path relative
// to baseDir. A regression that emitted the absolute t.TempDir path
// would surface the parent prefix instead.
func TestRootCommandReportsWarningRelativePath(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "fixture")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "remote" {
  source = "hashicorp/consul/aws"
}
`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(parent)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"fixture"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()

	if !strings.Contains(got, "non-local module source (main.tf:2)") {
		t.Errorf("warning row should render path relative to the input dir 'fixture', got:\n%s", got)
	}
	// Absolute-path leakage would surface as the t.TempDir prefix
	// (e.g. /var/folders/...) appearing in the warning row. Anchor
	// on the parent to make the regression assertion explicit.
	if strings.Contains(got, parent) {
		t.Errorf("warning row leaked absolute path %q into output:\n%s", parent, got)
	}
}

// TestRootCommandZeroArgsDefaultsToCwd exercises the bare-`tfperms`
// invocation path: when no positional argument is supplied, the root
// command falls through to rootDefaultDir ("."), which the parser
// resolves against the process's current working directory. The wiring
// test in TestRootCommandRunsPipeline only proves the explicit-path
// branch; this test pins the no-arg branch by chdir'ing into a fixture
// directory and executing the root command with an empty args slice.
func TestRootCommandZeroArgsDefaultsToCwd(t *testing.T) {
	dir := writeFixture(t, `
resource "google_storage_bucket" "primary" {
  name                        = "tfperms-fixture"
  location                    = "US"
  uniform_bucket_level_access = false
}
`)
	t.Chdir(dir)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()

	summary, _, ok := strings.Cut(got, "\n")
	if !ok {
		t.Fatalf("output has no summary line; got:\n%s", got)
	}
	if !strings.Contains(summary, "for 1 resource") {
		t.Errorf("summary line missing %q; got: %q", "for 1 resource", summary)
	}
	if !strings.Contains(got, "  storage.buckets.") {
		t.Errorf("output missing any storage.buckets.* permission row.\noutput:\n%s", got)
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
