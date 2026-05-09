package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/thoreinstein/tfperms/internal/catalog"
)

// Exit-code contract pinned by these tests:
//
//	0  Analysis completed successfully (advisory unknowns are not failures).
//	1  Usage error (bad flag, extra positional arg, third-party error).
//	2  Execution error (parse/load failure, ErrCatalog, recovered panic).
//
// Each branch is covered explicitly. The contract is a CI/CD interface
// — a regression that flips a code-2 error onto code 1 (or code 0) is
// a behaviour change a downstream pipeline would absorb silently, so
// we want the test failure to be loud and per-branch when the wiring
// drifts. TestRunRecoversFromPanic in main_test.go covers the panic →
// code 2 branch on its own; the cases here cover the rest.

// runRoot drives a freshly-built rootCmd through run() with the given
// args and returns (exitCode, stderrText). Output is sunk into a buffer
// so a test can assert on the rendered error message alongside the
// classification. SilenceUsage / SilenceErrors are inherited from
// newRootCmd; we set Out/Err to the same buffer so cobra's own writes
// (warnings emitted from PreRunE) and run()'s stderr writes are
// distinguishable in the assertions below.
func runRoot(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := newRootCmd()
	stderr := &bytes.Buffer{}
	// Out goes to /dev/null-equivalent — none of the exit-code tests
	// inspect stdout. Cobra's own error output goes to ErrBuf via
	// SetErr, but with SilenceErrors=true (set by newRootCmd) cobra
	// does not print the error there; run() writes the rendered
	// message to its stderr argument instead.
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	code := run(cmd, stderr)
	return code, stderr.String()
}

// writeExitCodeFixture writes a minimal valid Terraform file to a
// fresh temp dir and returns its path. Kept local to this file so
// adding a new exit-code test does not couple to writeFixture in
// main_test.go (which is shared with non-exit-code tests). Uses a
// catalogued resource so the run produces a non-empty permission set
// and exits 0 on success — driving the happy path through the same
// pipeline the failure paths exit through, so a regression that broke
// the entire pipeline cannot masquerade as "all exit codes correct".
func writeExitCodeFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

// TestRunExitsZeroOnSuccess pins the success branch. A valid fixture
// against the embedded catalog must exit 0 with empty stderr; advisory
// output appears on stdout (not exercised here — the wiring tests in
// main_test.go cover that). A regression that flipped success to a
// non-zero code would let CI consumers treat every tfperms run as a
// failure.
func TestRunExitsZeroOnSuccess(t *testing.T) {
	dir := writeExitCodeFixture(t, `
resource "google_storage_bucket" "primary" {
  name                        = "tfperms-fixture"
  location                    = "US"
  uniform_bucket_level_access = false
}
`)
	code, stderr := runRoot(t, dir)
	if code != 0 {
		t.Errorf("run() exit code = %d, want 0; stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty on success; got %q", stderr)
	}
}

// TestRunExitsZeroOnAdvisoryUnknown pins the v1-spec contract that an
// unknown resource type is advisory output, NOT a failure. The
// reporter writes the unknown row on stdout and the run still exits 0
// — a CI pipeline that treats unknowns as failures would have to grep
// stdout itself, never the exit code. A regression that escalated
// unknowns to a non-zero code would silently break every consumer.
func TestRunExitsZeroOnAdvisoryUnknown(t *testing.T) {
	dir := writeExitCodeFixture(t, `
resource "google_made_up_thing" "x" {
  name = "nope"
}
`)
	code, stderr := runRoot(t, dir)
	if code != 0 {
		t.Errorf("advisory unknown should exit 0; got code=%d stderr=%q", code, stderr)
	}
}

// TestRunExitsOneOnUsageErrors covers every documented code-1 branch:
// bad --format value, --format=role missing --role-name, --by-resource
// conflicting with --format, and an extra positional argument
// (cobra's own argument-validation surface). Each row drives run()
// through a fresh root command so PreRunE / Args wiring fires exactly
// as it would in production.
func TestRunExitsOneOnUsageErrors(t *testing.T) {
	dir := writeExitCodeFixture(t, `
resource "google_storage_bucket" "primary" {
  name = "tfperms-fixture"
}
`)
	cases := []struct {
		name string
		args []string
		// wantStderrContains anchors on a substring rather than the
		// full message because the underlying error texts are pinned
		// by the existing PreRunE/cobra tests; here we just confirm
		// each path actually reaches run() with a usage error.
		wantStderrContains string
	}{
		{
			name:               "invalid --format value",
			args:               []string{"--format=yaml", dir},
			wantStderrContains: "invalid --format value",
		},
		{
			name:               "--format=role without --role-name",
			args:               []string{"--format=role", dir},
			wantStderrContains: "--role-name is required",
		},
		{
			name:               "--by-resource conflicts with --format=flat",
			args:               []string{"--by-resource", "--format=flat", dir},
			wantStderrContains: "--by-resource conflicts with --format",
		},
		{
			name: "extra positional arg",
			args: []string{dir, dir},
			// cobra's own MaximumNArgs(1) error. The exact wording
			// is cobra-version-dependent; assert just the prefix
			// applied by run() so the test is hermetic.
			wantStderrContains: "tfperms: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := runRoot(t, tc.args...)
			if code != 1 {
				t.Errorf("run() exit code = %d, want 1; stderr=%q", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantStderrContains) {
				t.Errorf("stderr = %q, want to contain %q", stderr, tc.wantStderrContains)
			}
		})
	}
}

// TestRunExitsTwoOnNonexistentPath pins the parseLoadError → code 2
// branch for a missing directory. The existing
// TestRootCommandErrorsOnNonexistentPath asserts the error message;
// this asserts the exit code, which is what a CI pipeline actually
// branches on.
func TestRunExitsTwoOnNonexistentPath(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "no-such-dir")

	code, stderr := runRoot(t, missing)
	if code != 2 {
		t.Errorf("run() exit code = %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "directory not found") {
		t.Errorf("stderr = %q, want to contain 'directory not found'", stderr)
	}
}

// TestRunExitsTwoOnFileArg pins the parseLoadError → code 2 branch
// for a file-argument-where-directory-expected. Companion to
// TestRootCommandErrorsOnFile (which asserts the exact message).
func TestRunExitsTwoOnFileArg(t *testing.T) {
	dir := writeExitCodeFixture(t, `
resource "google_storage_bucket" "primary" {
  name = "tfperms-fixture"
}
`)
	filePath := filepath.Join(dir, "main.tf")

	code, stderr := runRoot(t, filePath)
	if code != 2 {
		t.Errorf("run() exit code = %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "expected a directory, got a file") {
		t.Errorf("stderr = %q, want to contain 'expected a directory, got a file'", stderr)
	}
}

// TestRunExitsTwoOnMalformedHCL pins the parser-failure → code 2
// branch. parser.LoadRecursive returns an error on a syntactically
// invalid .tf file, runAnalyze wraps it with parseLoadError, and run()
// classifies it as exit 2 — the right CI signal because the analyser
// could not consume the configuration at all.
func TestRunExitsTwoOnMalformedHCL(t *testing.T) {
	// Unterminated block — the hcl parser rejects this with a
	// diagnostic that LoadRecursive surfaces as a non-nil error.
	dir := writeExitCodeFixture(t, `
resource "google_storage_bucket" "primary" {
  name = "tfperms-fixture
`)
	code, stderr := runRoot(t, dir)
	if code != 2 {
		t.Errorf("malformed HCL should exit 2; got code=%d stderr=%q", code, stderr)
	}
	if !strings.HasPrefix(stderr, "tfperms: ") {
		t.Errorf("stderr should carry tfperms: prefix; got %q", stderr)
	}
}

// TestRunExitsTwoOnErrCatalog covers the catalog-corruption branch.
// The embedded catalog is committed and validated, so we can't trigger
// catalog.Load() failure from a real fixture — instead we drive run()
// with an inline cobra command whose RunE returns an error that wraps
// catalog.ErrCatalog. The classification logic in run() uses
// errors.Is(err, catalog.ErrCatalog) to detect this branch, so any
// error chain reaching it counts. A regression that lost ErrCatalog
// from the chain (e.g. dropping %w in runAnalyze's wrap) would let
// catalog corruption silently exit 1.
func TestRunExitsTwoOnErrCatalog(t *testing.T) {
	cmd := &cobra.Command{
		Use:           "catalog-failer",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("tfperms: catalog corrupt — please file an issue: %w", catalog.ErrCatalog)
		},
	}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)

	stderr := &bytes.Buffer{}
	code := run(cmd, stderr)
	if code != 2 {
		t.Errorf("ErrCatalog-wrapped error should exit 2; got code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "catalog corrupt") {
		t.Errorf("stderr = %q, want to contain 'catalog corrupt'", stderr.String())
	}
}

// TestRunExitsTwoOnParseLoadErrorMarker pins the marker contract
// directly. A future regression might keep parseLoadError around as a
// type but stop wrapping pipeline errors with it — the existing
// per-branch tests would still pass (the messages are still right)
// because the message is unchanged by the wrapper. This test fails
// the moment errors.As(err, &*parseLoadError) stops being the trigger
// for exit 2.
func TestRunExitsTwoOnParseLoadErrorMarker(t *testing.T) {
	cmd := &cobra.Command{
		Use:           "parseload-failer",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return newParseLoadError(fmt.Errorf("tfperms: synthetic parse failure"))
		},
	}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)

	stderr := &bytes.Buffer{}
	code := run(cmd, stderr)
	if code != 2 {
		t.Errorf("parseLoadError-wrapped error should exit 2; got code=%d stderr=%q", code, stderr.String())
	}
}

// TestParseLoadErrorPreservesMessage pins the Error()/Unwrap()
// pass-through contract. parseLoadError must surface the wrapped
// error verbatim (no prefix shift, no quoting) so the existing
// per-message tests in main_test.go keep working unchanged. errors.Is
// against the inner error must succeed — that is what keeps the
// "errors.Is(err, fs.ErrNotExist)" style of caller-reachable sentinel
// detection alive across the wrapper.
func TestParseLoadErrorPreservesMessage(t *testing.T) {
	inner := fmt.Errorf("tfperms: directory not found: /no/such/dir")
	wrapped := newParseLoadError(inner)
	if got := wrapped.Error(); got != inner.Error() {
		t.Errorf("wrapper Error() = %q, want %q (must be identical)", got, inner.Error())
	}
	if !errors.Is(wrapped, inner) {
		t.Error("errors.Is(wrapped, inner) returned false; wrapper must preserve identity")
	}

	// Nil pass-through: callers use newParseLoadError unconditionally
	// and rely on it returning nil when there is nothing to wrap.
	if got := newParseLoadError(nil); got != nil {
		t.Errorf("newParseLoadError(nil) = %v, want nil", got)
	}
}

// TestHelpDocumentsExitCodes pins the user-facing documentation of the
// exit-code contract. `tfperms --help` is the discoverability surface
// for CI operators wiring the tool into a pipeline — without this
// section, the contract would only live in the README, and a docs
// reviewer cannot tell from a code diff whether the help text was
// updated alongside the dispatch logic. The substring assertions below
// are deliberately loose (one anchor per code) so a future copy edit
// of the section does not require updating this test for cosmetic
// changes — only a structural omission of an exit code makes it fail.
func TestHelpDocumentsExitCodes(t *testing.T) {
	cmd := newRootCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help should not error: %v", err)
	}
	help := out.String()
	for _, want := range []string{
		"Exit codes:",
		"  0  Analysis completed successfully",
		"  1  Usage error",
		"  2  Execution error",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help output missing %q; full help:\n%s", want, help)
		}
	}
}
