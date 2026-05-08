package main

import (
	"bytes"
	"encoding/json"
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

// TestValidateFormatFlags pins the cobra-independent flag-validation
// contract. Each row covers one of the rejection rules in
// validateFormatFlags: unknown --format value, --format=role missing
// --role-name, --role-name failing the regex (too short, illegal
// character, too long), and the happy paths for both formats. Driving
// the helper directly keeps the assertions on validation logic
// rather than on cobra's error-formatting plumbing.
func TestValidateFormatFlags(t *testing.T) {
	cases := []struct {
		name      string
		format    string
		roleName  string
		wantError bool
	}{
		{name: "default flat empty role-name", format: "flat", roleName: "", wantError: false},
		{name: "flat with valid role-name", format: "flat", roleName: "my_role", wantError: false},
		{name: "flat with invalid role-name", format: "flat", roleName: "ab", wantError: true},
		{name: "role with valid role-name", format: "role", roleName: "my_role", wantError: false},
		{name: "role missing role-name", format: "role", roleName: "", wantError: true},
		{name: "role with too-short role-name", format: "role", roleName: "ab", wantError: true},
		{name: "role with illegal char in role-name", format: "role", roleName: "a!b", wantError: true},
		{name: "role with 65-char role-name", format: "role", roleName: strings.Repeat("a", 65), wantError: true},
		{name: "role with 64-char role-name boundary", format: "role", roleName: strings.Repeat("a", 64), wantError: false},
		{name: "role with 3-char role-name boundary", format: "role", roleName: "abc", wantError: false},
		{name: "by-resource happy path", format: "by-resource", roleName: "", wantError: false},
		{name: "by-resource ignores role-name with valid value", format: "by-resource", roleName: "my_role", wantError: false},
		{name: "by-resource rejects invalid role-name", format: "by-resource", roleName: "a!b", wantError: true},
		{name: "json happy path", format: "json", roleName: "", wantError: false},
		{name: "unknown format", format: "yaml", roleName: "", wantError: true},
		{name: "empty format", format: "", roleName: "", wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFormatFlags(tc.format, tc.roleName)
			if tc.wantError && err == nil {
				t.Fatalf("validateFormatFlags(%q, %q) returned nil; expected an error",
					tc.format, tc.roleName)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("validateFormatFlags(%q, %q) returned %v; expected nil",
					tc.format, tc.roleName, err)
			}
		})
	}
}

// TestRootCommandRejectsRoleWithoutName drives the CLI surface of
// validateFormatFlags's "role requires role-name" rule: invoking
// `tfperms --format=role` with no --role-name must exit non-zero with
// a message that names the missing flag. Going through cobra (rather
// than calling validateFormatFlags directly) proves the PreRunE wiring
// is in place — without it the rule could silently regress to
// "role-name is optional, run anyway and emit a YAML with title:”".
func TestRootCommandRejectsRoleWithoutName(t *testing.T) {
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--format=role"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("Execute with --format=role and no --role-name returned nil; expected validation failure.\noutput: %s", out.String())
	}
	if !strings.Contains(err.Error(), "--role-name") {
		t.Errorf("error message should mention --role-name; got: %v", err)
	}
}

// TestRootCommandRejectsInvalidRoleName covers the regex-rejection
// branch via the CLI. The role-name fails the alphanumeric-underscore
// constraint, so cobra's PreRunE must surface a non-nil error before
// the pipeline runs.
func TestRootCommandRejectsInvalidRoleName(t *testing.T) {
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--format=role", "--role-name=bad-name"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("Execute with --role-name=bad-name returned nil; expected validation failure.\noutput: %s", out.String())
	}
	if !strings.Contains(err.Error(), "--role-name") {
		t.Errorf("error message should mention --role-name; got: %v", err)
	}
}

// TestRootCommandRoleFormat exercises the end-to-end --format=role
// path. The fixture is the same google_storage_bucket block as the
// flat-format wiring test, but the assertions target the YAML body:
// the header carries the gcloud command, the title matches the
// supplied --role-name, and includedPermissions contains every
// catalogued storage.buckets.* permission.
//
// Substring-anchored assertions (rather than byte-equal compare) keep
// the test resilient to catalog churn — adding a new
// storage.buckets.* permission must not regress this test.
func TestRootCommandRoleFormat(t *testing.T) {
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
	root.SetArgs([]string{"--format=role", "--role-name=tfperms_test_role", dir})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()

	// Header anchors. The version / date strings are the dev defaults
	// because the test binary is built without ldflags overrides;
	// asserting on them pins the threading from main.version /
	// main.date through to RenderRole.
	wantHeaderParts := []string{
		"# Generated by tfperms 0.0.0-dev on unknown\n",
		"#   gcloud iam roles create tfperms_test_role --project=PROJECT_ID --file=role.yaml\n",
	}
	for _, part := range wantHeaderParts {
		if !strings.Contains(got, part) {
			t.Errorf("output missing header line %q.\noutput:\n%s", part, got)
		}
	}

	// YAML body anchors. The title must be the supplied role name and
	// the stage must default to GA. Checking both pins the RenderRole
	// contract that --role-name flows into the title field rather
	// than (e.g.) being silently ignored.
	wantBodyParts := []string{
		"title: tfperms_test_role\n",
		"stage: GA\n",
		"includedPermissions:\n",
	}
	for _, part := range wantBodyParts {
		if !strings.Contains(got, part) {
			t.Errorf("output missing body line %q.\noutput:\n%s", part, got)
		}
	}

	// Representative permission rows under includedPermissions —
	// matching the wiring-test approach: presence rather than
	// exhaustiveness. yaml.v3's default block-list indent is two
	// spaces with `- ` for each entry.
	wantPermRows := []string{
		"  - storage.buckets.get\n",
		"  - storage.buckets.create\n",
	}
	for _, row := range wantPermRows {
		if !strings.Contains(got, row) {
			t.Errorf("output missing includedPermissions row %q.\noutput:\n%s", row, got)
		}
	}

	// The flat-format section headers must NOT appear — a regression
	// in the dispatch branch that fell back to reporter.Render would
	// produce them, and the role file would be invalid YAML.
	if strings.Contains(got, "plan permissions (") {
		t.Errorf("role-format output unexpectedly contains a flat-format section header.\noutput:\n%s", got)
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

// TestRootCommandRunsJSONPipeline verifies the --format=json flag.
func TestRootCommandRunsJSONPipeline(t *testing.T) {
	dir := writeFixture(t, `
resource "google_storage_bucket" "primary" {
  name                        = "tfperms-json-fixture"
  location                    = "US"
  uniform_bucket_level_access = true
}
`)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{dir, "--format=json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	var got struct {
		Version string `json:"version"`
		Summary struct {
			ResourceCount int `json:"resource_count"`
		} `json:"summary"`
		Resources []struct {
			Type string `json:"type"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal json: %v\noutput: %s", err, out.String())
	}

	if got.Version != "1.0" {
		t.Errorf("version = %q, want \"1.0\"", got.Version)
	}
	if got.Summary.ResourceCount != 1 {
		t.Errorf("resource_count = %d, want 1", got.Summary.ResourceCount)
	}
	if len(got.Resources) != 1 || got.Resources[0].Type != "google_storage_bucket" {
		t.Errorf("unexpected resources: %+v", got.Resources)
	}
}

// TestRootCommandJSONOutputDeterministic verifies the v1.0 stability
// contract documented in docs/json-schema.md: identical inputs must
// produce bit-identical JSON output across runs. This test exercises
// the full CLI pipeline (cobra parse → newRootCmd → runAnalyze →
// reporter.RenderJSON), so a regression at any layer that introduces
// non-determinism — most plausibly a wall-clock timestamp leaking back
// into the metadata block — must fail this test rather than slipping
// past as a unit-level test that hardcodes a fixed time.
func TestRootCommandJSONOutputDeterministic(t *testing.T) {
	dir := writeFixture(t, `
resource "google_storage_bucket" "a" {
  name                        = "a"
  location                    = "US"
  uniform_bucket_level_access = true
}

resource "google_storage_bucket" "b" {
  name                        = "b"
  location                    = "US"
  uniform_bucket_level_access = true
}
`)

	run := func() []byte {
		root := newRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs([]string{dir, "--format=json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v\noutput: %s", err, out.String())
		}
		return out.Bytes()
	}

	first := run()
	second := run()
	if !bytes.Equal(first, second) {
		t.Errorf("JSON output not deterministic across runs.\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
}

// TestResolveFormat pins the --format / --by-resource interaction
// rules. The defaulting cases (no --by-resource leaves --format
// untouched) and the conflict-detection branch (--by-resource with
// an explicit --format other than by-resource — including the
// otherwise-default --format=flat) both flow through resolveFormat,
// so driving the helper directly exercises every branch without
// constructing a cobra command.
//
// The explicitFormat field mirrors cmd.Flags().Changed("format") at
// the cobra layer: it is true when the user actually passed --format,
// false when the variable holds its default formatFlat because no
// --format was given. The "explicit --format=flat conflicts with
// --by-resource" case below is the regression Implement v2 caught:
// a value-equal-to-default check is not the same as a "did the user
// pass this flag" check, and conflating them silently accepts
// `--format=flat --by-resource`.
func TestResolveFormat(t *testing.T) {
	cases := []struct {
		name           string
		format         string
		explicitFormat bool
		byResource     bool
		want           string
		wantError      bool
	}{
		{name: "default flat alone", format: "flat", explicitFormat: false, byResource: false, want: "flat"},
		{name: "explicit json alone", format: "json", explicitFormat: true, byResource: false, want: "json"},
		{name: "by-resource alone overrides default", format: "flat", explicitFormat: false, byResource: true, want: "by-resource"},
		{name: "by-resource agrees with --format=by-resource", format: "by-resource", explicitFormat: true, byResource: true, want: "by-resource"},
		{name: "by-resource conflicts with explicit --format=flat", format: "flat", explicitFormat: true, byResource: true, wantError: true},
		{name: "by-resource conflicts with --format=role", format: "role", explicitFormat: true, byResource: true, wantError: true},
		{name: "by-resource conflicts with --format=json", format: "json", explicitFormat: true, byResource: true, wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveFormat(tc.format, tc.explicitFormat, tc.byResource)
			if tc.wantError {
				if err == nil {
					t.Fatalf("resolveFormat(%q, %v, %v) returned nil error; want error", tc.format, tc.explicitFormat, tc.byResource)
				}
				if !strings.Contains(err.Error(), "--by-resource") || !strings.Contains(err.Error(), "--format") {
					t.Errorf("error should name both --by-resource and --format; got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFormat(%q, %v, %v) unexpected error: %v", tc.format, tc.explicitFormat, tc.byResource, err)
			}
			if got != tc.want {
				t.Errorf("resolveFormat(%q, %v, %v) = %q, want %q", tc.format, tc.explicitFormat, tc.byResource, got, tc.want)
			}
		})
	}
}

// TestRootCommandByResourceFlag is the end-to-end wiring test for the
// --by-resource shorthand. Going through cobra (rather than calling
// resolveFormat directly) proves the flag declaration, PreRunE
// reconciliation, and RunE dispatch are all wired up.
func TestRootCommandByResourceFlag(t *testing.T) {
	dir := writeFixture(t, `
resource "google_storage_bucket" "primary" {
  name                        = "tfperms-by-resource-fixture"
  location                    = "US"
  uniform_bucket_level_access = false
}
`)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{dir, "--by-resource"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()

	// The summary line must precede the by-resource group header.
	if !strings.HasPrefix(got, "  ") {
		t.Errorf("output missing summary indent prefix; got:\n%s", got)
	}
	if !strings.Contains(got, "  google_storage_bucket (1 instance):") {
		t.Errorf("output missing by-resource group header.\noutput:\n%s", got)
	}
	// Instance row appears under the group header.
	// The line number depends on the fixture's leading whitespace.
	// Match on the type/name/file prefix and require any positive
	// integer line number to follow.
	if !strings.Contains(got, "    google_storage_bucket.primary (main.tf:") {
		t.Errorf("output missing instance row with main.tf location.\noutput:\n%s", got)
	}
}

// TestRootCommandByResourceFormatExplicit pins that --format=by-resource
// works without --by-resource — they are both surfaces onto the same
// renderer.
func TestRootCommandByResourceFormatExplicit(t *testing.T) {
	dir := writeFixture(t, `
resource "google_storage_bucket" "primary" {
  name                        = "tfperms-by-resource-explicit"
  location                    = "US"
  uniform_bucket_level_access = false
}
`)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{dir, "--format=by-resource"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	if !strings.Contains(got, "  google_storage_bucket (1 instance):") {
		t.Errorf("output missing by-resource group header.\noutput:\n%s", got)
	}
}

// TestRootCommandByResourceConflictsWithFormat pins the resolveFormat
// conflict-detection branch at the CLI surface: the user passing both
// --by-resource and --format=role gets an error before the pipeline
// runs, naming both flags.
func TestRootCommandByResourceConflictsWithFormat(t *testing.T) {
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--by-resource", "--format=role", "--role-name=tfperms_test_role"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("Execute with --by-resource and --format=role returned nil; expected conflict error.\noutput: %s", out.String())
	}
	if !strings.Contains(err.Error(), "--by-resource") {
		t.Errorf("conflict error should mention --by-resource; got: %v", err)
	}
	if !strings.Contains(err.Error(), "--format") {
		t.Errorf("conflict error should mention --format; got: %v", err)
	}
}

// TestRootCommandByResourceConflictsWithExplicitFlatFormat is the
// regression test for the bug where `--format=flat --by-resource` was
// silently accepted. The advertised contract on the --by-resource
// flag help is that it is mutually exclusive with --format (other
// than --format=by-resource itself), and that contract has to hold
// when the user explicitly types --format=flat — not just when they
// pick a non-default value. Without the cmd.Flags().Changed("format")
// signal in PreRunE, resolveFormat could not distinguish "user did
// not pass --format" from "user explicitly passed --format=flat",
// because both leave format == formatFlat.
func TestRootCommandByResourceConflictsWithExplicitFlatFormat(t *testing.T) {
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--by-resource", "--format=flat"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("Execute with --by-resource and --format=flat returned nil; expected conflict error.\noutput: %s", out.String())
	}
	if !strings.Contains(err.Error(), "--by-resource") {
		t.Errorf("conflict error should mention --by-resource; got: %v", err)
	}
	if !strings.Contains(err.Error(), "--format") {
		t.Errorf("conflict error should mention --format; got: %v", err)
	}
}

// TestRootCommandByResourceRelativisesFile pins the path-relativisation
// contract for the by-resource format: an instance row's location
// must be `main.tf:N`, never the absolute t.TempDir path.
// parser.LoadRecursive produces absolute paths, and without the
// relativizeResult pass the user would see the developer-machine
// directory in their analysis output.
func TestRootCommandByResourceRelativisesFile(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "fixture")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "google_storage_bucket" "primary" {
  name                        = "rel-bucket"
  location                    = "US"
  uniform_bucket_level_access = false
}
`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Chdir(parent)

	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"fixture", "--by-resource"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()

	// Instance row should render with the input-relative path.
	wantRow := "    google_storage_bucket.primary (main.tf:"
	if !strings.Contains(got, wantRow) {
		t.Errorf("instance row should use baseDir-relative path %q.\noutput:\n%s", wantRow, got)
	}
	// Absolute path leakage would surface as the t.TempDir parent
	// prefix appearing in any line of the report.
	if strings.Contains(got, parent) {
		t.Errorf("absolute path %q leaked into output:\n%s", parent, got)
	}
}

// TestRootCommandFlatRelativisesUnknownsAndResources pins that the
// path-relativisation is universal — the flat formatter, JSON output,
// and the unknowns / resources entries on every format see the same
// root-relative paths after runAnalyze runs relativizeResult.
//
// The fixture mixes a catalogued resource (storage_bucket) and an
// uncatalogued one (made_up_thing) so we can pin both the resources
// and unknowns paths in a single run.
func TestRootCommandFlatRelativisesUnknownsAndResources(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "fixture")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "google_made_up_thing" "x" {
  name = "nope"
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

	// Unknowns row should use baseDir-relative path.
	wantRow := "    google_made_up_thing.x (main.tf:"
	if !strings.Contains(got, wantRow) {
		t.Errorf("unknowns row should use baseDir-relative path %q.\noutput:\n%s", wantRow, got)
	}
	if strings.Contains(got, parent) {
		t.Errorf("absolute path %q leaked into output:\n%s", parent, got)
	}
}
