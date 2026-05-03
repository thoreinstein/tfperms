package catalog

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// TestLoadMergesFiles confirms that LoadFS reads every *.yaml file in
// the directory and merges entries keyed by Terraform type. The fixtures
// are split into two files so the test catches a regression where the
// loader stops reading after the first file.
func TestLoadMergesFiles(t *testing.T) {
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan:
        - storage.buckets.get
      apply:
        - storage.buckets.create
data_sources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan:
        - storage.buckets.get
iam_bindings:
  google_storage_bucket_iam_binding:
    parent_resource: google_storage_bucket
    verification:
      method: rest
    permissions:
      plan:
        - storage.buckets.getIamPolicy
      apply:
        - storage.buckets.setIamPolicy
`)},
		"compute.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_compute_instance:
    verification:
      method: gcloud
    permissions:
      plan:
        - compute.instances.get
      apply:
        - compute.instances.create
        - compute.instances.delete
`)},
		// Files that are not YAML must be ignored, not parsed.
		"README.md": &fstest.MapFile{Data: []byte("# not a yaml file\n")},
	}

	cat, err := LoadFS(fs, ".")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}

	if _, ok := cat.Resources["google_storage_bucket"]; !ok {
		t.Errorf("missing google_storage_bucket from storage.yaml")
	}
	if _, ok := cat.Resources["google_compute_instance"]; !ok {
		t.Errorf("missing google_compute_instance from compute.yaml")
	}
	if _, ok := cat.DataSources["google_storage_bucket"]; !ok {
		t.Errorf("missing data source google_storage_bucket")
	}
	if _, ok := cat.IAMBindings["google_storage_bucket_iam_binding"]; !ok {
		t.Errorf("missing iam binding google_storage_bucket_iam_binding")
	}

	// Type and Position must be populated even though both fields are
	// yaml:"-" — the loader copies them in after Decode.
	bucket := cat.Resources["google_storage_bucket"]
	if bucket.Type != "google_storage_bucket" {
		t.Errorf("Type = %q, want google_storage_bucket", bucket.Type)
	}
	if bucket.Position.File != "storage.yaml" {
		t.Errorf("Position.File = %q, want storage.yaml", bucket.Position.File)
	}
	if bucket.Position.Line == 0 {
		t.Errorf("Position.Line = 0, want non-zero")
	}
}

// TestLoadDuplicateResourceTypes asserts that the loader rejects two
// files defining the same resource type. The error message must quote
// both source positions so a contributor can locate the conflict
// without grep.
func TestLoadDuplicateResourceTypes(t *testing.T) {
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: [storage.buckets.get]
`)},
		"storage_dup.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: [storage.buckets.get]
`)},
	}

	_, err := LoadFS(fs, ".")
	if err == nil {
		t.Fatal("LoadFS: expected duplicate-type error, got nil")
	}
	if !errors.Is(err, ErrCatalog) {
		t.Errorf("error not wrapped with ErrCatalog: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"duplicate resource type", "google_storage_bucket", "storage.yaml", "storage_dup.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

// TestLoadMalformedYAML verifies that a YAML parse error is wrapped with
// ErrCatalog and quotes the offending file. The malformed payload uses a
// tab in indentation, which yaml.v3 rejects with a clear error.
func TestLoadMalformedYAML(t *testing.T) {
	fs := fstest.MapFS{
		"broken.yaml": &fstest.MapFile{Data: []byte("resources:\n\tgoogle_x: {}\n")},
	}
	_, err := LoadFS(fs, ".")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !errors.Is(err, ErrCatalog) {
		t.Errorf("error not wrapped with ErrCatalog: %v", err)
	}
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("error message missing filename: %v", err)
	}
}

// TestLoadEmptyDirectory exercises the no-files path. An empty catalog
// directory is a degenerate-but-valid state; downstream consumers should
// see an empty Catalog with non-nil maps rather than nil or an error.
func TestLoadEmptyDirectory(t *testing.T) {
	fs := fstest.MapFS{}
	cat, err := LoadFS(fs, ".")
	if err != nil {
		t.Fatalf("LoadFS empty: %v", err)
	}
	if cat == nil || cat.Resources == nil || cat.DataSources == nil || cat.IAMBindings == nil {
		t.Fatalf("LoadFS empty returned malformed catalog: %+v", cat)
	}
	if len(cat.Resources)+len(cat.DataSources)+len(cat.IAMBindings) != 0 {
		t.Errorf("expected empty catalog, got %+v", cat)
	}
}

// TestLoadAnnotatesConditionalPositions confirms the loader populates the
// per-conditional Position with the line of the conditional list item,
// not the line of the surrounding entry. Line 11 below is the line of
// `- when:` (the first conditional) — assertion uses an inequality
// against the entry line so the test does not break under whitespace
// edits to the fixture.
func TestLoadAnnotatesConditionalPositions(t *testing.T) {
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: [storage.buckets.get]
      apply: [storage.buckets.create]
    conditionals:
      - when:
          uniform_bucket_level_access: true
        permissions:
          plan: []
          apply: [storage.buckets.update]
`)},
	}
	cat, err := LoadFS(fs, ".")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	bucket := cat.Resources["google_storage_bucket"]
	if bucket == nil {
		t.Fatal("missing entry")
	}
	if len(bucket.Conditionals) != 1 {
		t.Fatalf("conditionals len = %d, want 1", len(bucket.Conditionals))
	}
	condPos := bucket.Conditionals[0].Position
	if condPos.File != "storage.yaml" {
		t.Errorf("conditional File = %q, want storage.yaml", condPos.File)
	}
	if condPos.Line == 0 {
		t.Errorf("conditional Line = 0, want non-zero")
	}
	if condPos.Line <= bucket.Position.Line {
		t.Errorf("conditional Line %d not greater than entry Line %d — annotation likely wrong",
			condPos.Line, bucket.Position.Line)
	}
}

// TestLoadProductionEmbed exercises the package-level Load() entry point
// against the actual embedded catalog. It is a smoke test: as long as
// the embedded files parse and pass validation we are happy. Full
// repository-consistency checks live in catalog_repo_test.go (Phase 5).
func TestLoadProductionEmbed(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed against embedded catalog: %v", err)
	}
	if cat == nil {
		t.Fatal("Load() returned nil catalog")
	}
}
