package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thoreinstein/tfperms/internal/catalog"
)

// TestCatalogScaffoldCreatesFile drives the cobra command end-to-end:
// it builds a fresh root, runs `catalog scaffold <type>` with a custom
// --catalog-dir pointed at a t.TempDir(), and asserts the file appears.
// Running the command via the cobra entrypoint (rather than calling
// catalog.Scaffold directly) is the only way to assert that the flag
// wiring, argument validation, and output formatting are all connected.
func TestCatalogScaffoldCreatesFile(t *testing.T) {
	dir := t.TempDir()
	root := newRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"catalog", "scaffold", "google_dataplex_lake", "--catalog-dir", filepath.Join(dir, "catalog")})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}
	want := filepath.Join(dir, "catalog", "dataplex.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected file at %s, stat: %v", want, err)
	}
	if !strings.Contains(out.String(), "google_dataplex_lake") {
		t.Errorf("output did not mention type: %q", out.String())
	}
}

// TestCatalogScaffoldDuplicateNonZero confirms that re-scaffolding an
// existing entry returns an error from cobra.Execute. The cobra
// runner's non-nil return is what main.go converts into a non-zero
// process exit, so this test pins the contract from the user's side
// without a subprocess fork.
func TestCatalogScaffoldDuplicateNonZero(t *testing.T) {
	dir := t.TempDir()

	first := newRootCmd()
	first.SetOut(&bytes.Buffer{})
	first.SetErr(&bytes.Buffer{})
	first.SetArgs([]string{"catalog", "scaffold", "google_dataplex_lake", "--catalog-dir", filepath.Join(dir, "catalog")})
	if err := first.Execute(); err != nil {
		t.Fatalf("first Execute: %v", err)
	}

	second := newRootCmd()
	second.SetOut(&bytes.Buffer{})
	second.SetErr(&bytes.Buffer{})
	second.SetArgs([]string{"catalog", "scaffold", "google_dataplex_lake", "--catalog-dir", filepath.Join(dir, "catalog")})
	err := second.Execute()
	if err == nil {
		t.Fatalf("second Execute: expected error, got nil")
	}
	if !errors.Is(err, catalog.ErrDuplicateEntry) {
		t.Errorf("err = %v, want errors.Is(catalog.ErrDuplicateEntry) == true", err)
	}
}

// TestCatalogIntegrity is the cmd-level smoke check that the embedded
// catalog still passes catalog.Load(). The internal-package
// repo_test.go runs the same check, but exercising the load path from
// main rather than internal/catalog catches the regression where a
// future refactor moves the embed.FS or breaks the import graph at
// the cmd boundary. Cheap to run, distinct in scope, and its failure
// message is the one users see at runtime.
func TestCatalogIntegrity(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog.Load() rejected the embedded catalog: %v", err)
	}
	if cat == nil {
		t.Fatal("catalog.Load() returned nil with no error")
	}
	if len(cat.Resources)+len(cat.DataSources)+len(cat.IAMBindings) == 0 {
		t.Fatal("embedded catalog is empty — loader or //go:embed pattern likely misconfigured")
	}
}

// TestCatalogScaffoldMutuallyExclusiveFlags pins the input-validation
// guard so a future refactor that reorders flag handling cannot make
// --data-source and --iam-binding both apply at once (the section
// pick switch would silently choose one over the other).
func TestCatalogScaffoldMutuallyExclusiveFlags(t *testing.T) {
	dir := t.TempDir()
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"catalog", "scaffold", "google_storage_bucket",
		"--data-source", "--iam-binding",
		"--catalog-dir", filepath.Join(dir, "catalog")})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for conflicting flags, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want it to mention 'mutually exclusive'", err)
	}
}
