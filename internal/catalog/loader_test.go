package catalog

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

// validProvenance is the chunk of YAML every loader / validator
// fixture needs to satisfy the verification + tested_against_provider
// rules so each test can isolate ONE schema violation.
//
// Indented six spaces because the typical fixture nests it under an
// entry key two levels deep (e.g. resources / google_storage_bucket /
// verification). Tests that need a different indentation embed the
// fields inline.
const validProvenance = `    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
`

// TestLoadMergesFiles confirms that LoadFS reads every *.yaml file in
// the directory and merges entries keyed by Terraform type. The fixtures
// are split into two files so the test catches a regression where the
// loader stops reading after the first file.
func TestLoadMergesFiles(t *testing.T) {
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_storage_bucket:
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
      create: [storage.buckets.create]
data_sources:
  google_storage_bucket:
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
iam_bindings:
  google_storage_bucket_iam_binding:
    parent_resource: google_storage_bucket
` + validProvenance + `    permissions:
      plan: [storage.buckets.getIamPolicy]
      create: [storage.buckets.setIamPolicy]
      update: [storage.buckets.setIamPolicy]
      delete: [storage.buckets.setIamPolicy]
`)},
		"compute.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_compute_instance:
` + validProvenance + `    permissions:
      plan: [compute.instances.get]
      create: [compute.instances.create]
      delete: [compute.instances.delete]
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
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
`)},
		"storage_dup.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_storage_bucket:
` + validProvenance + `    permissions:
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
// not the line of the surrounding entry. The fixture intentionally puts
// the conditional several lines below the entry header so the
// inequality-based assertion (cond line > entry line) is meaningful.
func TestLoadAnnotatesConditionalPositions(t *testing.T) {
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_storage_bucket:
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
      create: [storage.buckets.create]
    conditionals:
      - when:
          uniform_bucket_level_access: true
        permissions:
          update: [storage.buckets.update]
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

// TestLoadAnnotatesDataSourceConditionalPositions is the sibling of
// TestLoadAnnotatesConditionalPositions for the read-only data-source
// shape. The position-extraction helper is shared between resources and
// data sources, so this test catches a regression where one of the call
// sites stops invoking it.
func TestLoadAnnotatesDataSourceConditionalPositions(t *testing.T) {
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(`
data_sources:
  google_storage_bucket:
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when:
          include_iam: true
        permissions:
          plan: [storage.buckets.getIamPolicy]
`)},
	}
	cat, err := LoadFS(fs, ".")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	ds := cat.DataSources["google_storage_bucket"]
	if ds == nil {
		t.Fatal("missing data source entry")
	}
	if len(ds.Conditionals) != 1 {
		t.Fatalf("conditionals len = %d, want 1", len(ds.Conditionals))
	}
	condPos := ds.Conditionals[0].Position
	if condPos.Line == 0 {
		t.Errorf("data source conditional Line = 0, want non-zero")
	}
	if condPos.Line <= ds.Position.Line {
		t.Errorf("data source conditional Line %d not greater than entry Line %d", condPos.Line, ds.Position.Line)
	}
}

// TestLoadProductionEmbed exercises the package-level Load() entry point
// against the actual embedded catalog. It is a smoke test: as long as
// the embedded files parse and pass validation we are happy. Full
// repository-consistency checks live in repo_test.go.
func TestLoadProductionEmbed(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed against embedded catalog: %v", err)
	}
	if cat == nil {
		t.Fatal("Load() returned nil catalog")
	}
}

// TestLoadStrictRejectsUnknownTopLevelKey is the regression test for the
// silent-drop bug the previous review caught. Before strict decoding was
// engaged on the top-level rawFile, a misspelled section name like
// `resource:` (singular) would deserialise into nothing — the file
// contributed zero entries to the merged catalog and the load happily
// reported success. After this fix yaml.NewDecoder.KnownFields(true)
// raises a hard error so contributor typos surface at CI rather than
// quietly disappearing the entry.
//
// The cases below cover every entry kind so we catch a regression where
// only one of the three sections (resources / data_sources / iam_bindings)
// gains a strictness bypass.
func TestLoadStrictRejectsUnknownTopLevelKey(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		typoIn string // the misspelled key we expect to see in the error
	}{
		{
			name: "misspelled resources",
			yaml: `
resource:
  google_storage_bucket:
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
`,
			typoIn: "resource",
		},
		{
			name: "misspelled data_sources",
			yaml: `
data_source:
  google_storage_bucket:
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
`,
			typoIn: "data_source",
		},
		{
			name: "misspelled iam_bindings",
			yaml: `
iam_binding:
  google_storage_bucket_iam_binding:
    parent_resource: google_storage_bucket
` + validProvenance + `    permissions:
      plan: [storage.buckets.getIamPolicy]
`,
			typoIn: "iam_binding",
		},
		{
			name: "completely unknown section",
			yaml: `
resources_typo:
  google_storage_bucket:
` + validProvenance + `    permissions:
      plan: [storage.buckets.get]
`,
			typoIn: "resources_typo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := fstest.MapFS{
				"x.yaml": &fstest.MapFile{Data: []byte(tc.yaml)},
			}
			_, err := LoadFS(fs, ".")
			if err == nil {
				t.Fatalf("expected strict-mode error for unknown top-level key %q, got nil", tc.typoIn)
			}
			if !errors.Is(err, ErrCatalog) {
				t.Errorf("error not wrapped with ErrCatalog: %v", err)
			}
			msg := err.Error()
			// The error must name the offending file AND the typo'd field
			// so the contributor knows where to look.
			if !strings.Contains(msg, "x.yaml") {
				t.Errorf("error missing filename: %v", err)
			}
			if !strings.Contains(msg, tc.typoIn) {
				t.Errorf("error missing typo %q: %v", tc.typoIn, err)
			}
		})
	}
}

// TestLoadStrictRejectsUnknownEntryField confirms strict decoding propagates
// to the per-entry layer. The previous loader called yaml.Node.Decode,
// which yaml.v3 does NOT subject to KnownFields, so a typo'd entry field
// (e.g. `verifications:` plural instead of `verification:`) was silently
// ignored — the entry decoded with a zero Verification, then the validator
// reported "verification.method is required" which superficially looks
// like a missing-field error rather than a typo. After the strict-decode
// fix the loader rejects the typo with a yaml-level "field not found"
// error before validation runs, which is the correct diagnostic.
func TestLoadStrictRejectsUnknownEntryField(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		typoIn string
	}{
		{
			name: "resource entry typo",
			yaml: `
resources:
  google_storage_bucket:
    verifications:
      method: docs+source
    permissions:
      plan: [storage.buckets.get]
`,
			typoIn: "verifications",
		},
		{
			name: "data source entry typo",
			yaml: `
data_sources:
  google_storage_bucket:
    verification:
      method: docs+source
    permission:
      plan: [storage.buckets.get]
`,
			typoIn: "permission",
		},
		{
			name: "iam binding entry typo",
			yaml: `
iam_bindings:
  google_storage_bucket_iam_binding:
    parent: google_storage_bucket
    verification:
      method: docs+source
    permissions:
      plan: [storage.buckets.getIamPolicy]
`,
			typoIn: "parent",
		},
		{
			name: "verification block typo",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      methode: docs+source
    permissions:
      plan: [storage.buckets.get]
`,
			typoIn: "methode",
		},
		{
			name: "permissions block typo",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
    permissions:
      plans: [storage.buckets.get]
`,
			typoIn: "plans",
		},
		{
			name: "conditional entry typo",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - whne:
          uniform_bucket_level_access: true
        permissions:
          update: [storage.buckets.update]
`,
			typoIn: "whne",
		},
		{
			name: "data source permissions reject create",
			yaml: `
data_sources:
  google_storage_bucket:
    verification:
      method: docs+source
    permissions:
      plan: [storage.buckets.get]
      create: [storage.buckets.create]
`,
			typoIn: "create",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := fstest.MapFS{
				"x.yaml": &fstest.MapFile{Data: []byte(tc.yaml)},
			}
			_, err := LoadFS(fs, ".")
			if err == nil {
				t.Fatalf("expected strict-mode error for unknown entry field %q, got nil", tc.typoIn)
			}
			if !errors.Is(err, ErrCatalog) {
				t.Errorf("error not wrapped with ErrCatalog: %v", err)
			}
			if !strings.Contains(err.Error(), tc.typoIn) {
				t.Errorf("error missing typo %q: %v", tc.typoIn, err)
			}
		})
	}
}

// TestLoadPreservesUnderlyingErrorChain is the regression test for the
// review feedback that called out using %v instead of %w on the underlying
// I/O and decode errors. The package doc on ErrCatalog promises callers
// can use errors.Is to inspect both the catalog category AND the
// underlying error; if anyone re-introduces %v formatting on those
// fmt.Errorf calls, the underlying error gets stripped from the chain
// and these assertions fail.
//
// Two cases:
//
//   - Missing directory: the fs.ReadDir error path. fs.ErrNotExist is the
//     canonical sentinel and gives us a direct errors.Is target.
//   - Malformed YAML inside a file: the per-file decode path. yaml.v3
//     does not export a parse-error sentinel, so we use errors.As against
//     *yaml.TypeError plus a chain-walker that handles Go 1.20's
//     multi-%w wrapping (which exposes Unwrap() []error, not
//     Unwrap() error).
func TestLoadPreservesUnderlyingErrorChain(t *testing.T) {
	t.Run("missing directory wraps fs.ErrNotExist", func(t *testing.T) {
		// fstest.MapFS returns fs.ErrNotExist for ReadDir on an unknown
		// path; the loader must propagate that error in the chain so a
		// caller can errors.Is it. This single assertion is the contract
		// the %v → %w fix exists to satisfy.
		emptyFS := fstest.MapFS{}
		_, err := LoadFS(emptyFS, "does-not-exist")
		if err == nil {
			t.Fatal("expected error for missing directory, got nil")
		}
		if !errors.Is(err, ErrCatalog) {
			t.Errorf("error not wrapped with ErrCatalog: %v", err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error chain dropped fs.ErrNotExist; %%v formatting may have been re-introduced: %v", err)
		}
	})

	t.Run("malformed YAML preserves underlying decode error in chain", func(t *testing.T) {
		// A tab in YAML indentation triggers a yaml.v3 parser error;
		// the loader must keep that error in the chain. yaml.v3 does
		// not expose its parser errors as a public sentinel, so we
		// assert structurally: at least one link in the chain other
		// than ErrCatalog must exist. With %v formatting only
		// ErrCatalog would be reachable.
		mfs := fstest.MapFS{
			"broken.yaml": &fstest.MapFile{Data: []byte("resources:\n\tgoogle_x: {}\n")},
		}
		_, err := LoadFS(mfs, ".")
		if err == nil {
			t.Fatal("expected parse error, got nil")
		}
		if !errors.Is(err, ErrCatalog) {
			t.Errorf("error not wrapped with ErrCatalog: %v", err)
		}
		if !hasNonCatalogCause(err) {
			t.Errorf("error chain only contains ErrCatalog; underlying YAML error was lost: %v", err)
		}
		// Best-effort: when yaml.v3 produces a TypeError, errors.As
		// should reach it through the chain. Not all parser-level
		// errors surface as *yaml.TypeError, so this is informative
		// rather than a hard assertion.
		var typeErr *yaml.TypeError
		_ = errors.As(err, &typeErr)
	})
}

// hasNonCatalogCause reports whether err's tree contains any error other
// than ErrCatalog. fmt.Errorf with multiple %w verbs produces an error
// whose Unwrap method returns []error (Go 1.20+), so we recurse with the
// multi-wrap-aware visitor pattern rather than calling errors.Unwrap in a
// loop (which only handles the single-wrap form and would miss the
// second %w branch entirely).
func hasNonCatalogCause(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrCatalog { //nolint:errorlint // sentinel identity check, not a wrap traversal
		return false
	}
	switch u := err.(type) { //nolint:errorlint // we are walking the wrap tree, not asking errors.As
	case interface{ Unwrap() error }:
		inner := u.Unwrap()
		if inner == nil {
			// Leaf error that is not ErrCatalog — that is the
			// underlying cause we want to see.
			return true
		}
		return hasNonCatalogCause(inner)
	case interface{ Unwrap() []error }:
		for _, child := range u.Unwrap() {
			if hasNonCatalogCause(child) {
				return true
			}
		}
		return false
	default:
		// A leaf error that is not ErrCatalog (handled above) — that
		// is by definition a non-catalog cause.
		return true
	}
}
