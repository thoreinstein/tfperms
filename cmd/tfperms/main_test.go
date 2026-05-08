package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// TestRootAnalyze_SurfacesParseWarnings is the end-to-end proof that a
// non-fatal "could not load local module" warning from
// parser.LoadRecursive flows through runAnalyze and reaches the user
// in the parse-warnings section of the report.
//
// The fixture is intentionally small: a single .tf file with a
// resource that the catalog covers (so the run produces a non-empty
// permission set and exercises the resolver path) plus a `module`
// block whose local source does not exist. parser.LoadRecursive
// classifies the broken module as a warning rather than a hard error;
// runAnalyze must propagate the warning to reporter.Render rather
// than swallowing it or mis-elevating it to a fatal.
//
// Asserting on substrings rather than the entire output keeps the
// test resilient to non-load-warning catalog churn (a future
// permission added to google_storage_bucket should not require
// updating this golden).
func TestRootAnalyze_SurfacesParseWarnings(t *testing.T) {
	dir := t.TempDir()
	src := `module "broken" {
  source = "./does-not-exist"
}

resource "google_storage_bucket" "kept" {
  name     = "demo"
  location = "US"
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(src), 0o644); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}

	var stdout bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{dir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\n--- output ---\n%s", err, stdout.String())
	}

	got := stdout.String()
	for _, want := range []string{
		"tfperms analyze",
		"parse warnings:",
		"could not load local module",
		// The detail line carries the module name and source so a user
		// can locate the offending block without re-running with a
		// verbose flag — assert on a fragment that survives even if
		// the wrapped fs error message changes between Go versions.
		`module "broken"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, got)
		}
	}
}
