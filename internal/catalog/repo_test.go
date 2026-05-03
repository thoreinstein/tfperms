package catalog

import (
	"io/fs"
	"strings"
	"testing"

	catalogdata "github.com/thoreinstein/tfperms/catalog"
)

// TestRepositoryCatalogIsValid loads the actual embedded catalog
// (everything currently committed under catalog/*.yaml) and asserts
// the validator accepts every entry. It is the consistency gate for
// catalog contributions: any future PR adding or modifying a YAML
// entry has to pass this test, which means no schema-violating entry
// can land via the normal review process.
//
// This test is intentionally separate from TestLoadProductionEmbed
// (in loader_test.go) which is a smoke test — that test only checks
// "Load() returns a non-nil catalog without an error". The two could
// be consolidated, but keeping them split makes the intent clear:
// loader_test.go pins loader behaviour, repo_test.go pins repository
// content.
//
// Failure mode: if this test fails on a commit that did not modify a
// YAML file, the validator itself was tightened and existing entries
// no longer satisfy it. The fix is to either relax the validator (if
// the new rule was wrong) or update the offending YAML entry to
// satisfy it (if the rule was right).
func TestRepositoryCatalogIsValid(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() rejected committed catalog: %v", err)
	}

	// Sanity floor: the catalog should never go empty. If a refactor
	// accidentally points the embed FS at a directory with no YAML
	// files, Load() would still succeed with an empty catalog and the
	// rest of the suite would not catch it. This explicit check makes
	// that misconfiguration fail loudly.
	totalEntries := len(cat.Resources) + len(cat.DataSources) + len(cat.IAMBindings)
	if totalEntries == 0 {
		t.Fatal("repository catalog is empty — embed FS likely misconfigured")
	}
}

// TestRepositoryCatalogIAMBindingsResolve confirms every IAM binding
// in the committed catalog points at a parent_resource that is also
// committed. The validator already enforces this at load time, so
// TestRepositoryCatalogIsValid would catch the same condition. The
// reason this dedicated assertion exists is documentation: when this
// test fails the message is unambiguous about what went wrong, which
// cuts the time to a fix on a contributor's first attempt.
func TestRepositoryCatalogIAMBindingsResolve(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	for _, typ := range sortedKeys(cat.IAMBindings) {
		binding := cat.IAMBindings[typ]
		if _, ok := cat.Resources[binding.ParentResource]; !ok {
			t.Errorf(
				"iam binding %q at %s references parent_resource %q which is not declared in the catalog",
				typ, binding.Position, binding.ParentResource,
			)
		}
	}
}

// TestRepositoryCatalogEveryFileContributes is a defense-in-depth
// assertion that every committed catalog YAML file produces at least
// one merged entry. Strict decoding (loader.go's KnownFields(true)) is
// the primary mechanism that prevents a misspelled top-level key from
// silently emptying a file — a typo there is now a hard parse error.
// This test is the second line of defense: if a future change relaxed
// strict mode, an empty file would still be caught here.
//
// The previous review specifically flagged that asserting only
// "merged catalog is non-empty in aggregate" allowed a misspelled file
// to coast through CI as long as some other file kept the total count
// above zero. By indexing entries back to their source file via
// Position.File and counting per file, this test makes that scenario
// fail loudly with a message that names the offending filename.
func TestRepositoryCatalogEveryFileContributes(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Enumerate every YAML file in the embedded catalog directly so
	// the test sees files even if the loader silently skipped them.
	files, err := listCatalogYAMLFiles()
	if err != nil {
		t.Fatalf("list catalog files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("embedded catalog has no YAML files — repo is misconfigured")
	}

	// Count entries by source file. Position.File is the per-file
	// path that loader.go assigned to each entry, so a file that
	// contributed nothing has zero entries here even though its
	// filename appears in `files`.
	perFile := make(map[string]int, len(files))
	for _, f := range files {
		perFile[f] = 0
	}
	for _, e := range cat.Resources {
		perFile[e.Position.File]++
	}
	for _, e := range cat.DataSources {
		perFile[e.Position.File]++
	}
	for _, e := range cat.IAMBindings {
		perFile[e.Position.File]++
	}

	for _, f := range files {
		if perFile[f] == 0 {
			t.Errorf(
				"catalog file %q contributed zero entries — likely a misspelled top-level section "+
					"(expected one of: resources, data_sources, iam_bindings)",
				f,
			)
		}
	}
}

// listCatalogYAMLFiles returns the *.yaml / *.yml bare filenames in the
// root of the embedded catalog FS. It mirrors loader.go's file selection
// (suffix filter, skip directories) so the test compares apples to apples
// with the Position.File values that the loader stamps onto each entry.
// If the loader ever moves to subdirectories, both this helper and
// loader.go will need updating together.
func listCatalogYAMLFiles() ([]string, error) {
	entries, err := fs.ReadDir(catalogdata.FS, ".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		// Bare name matches the Position.File the loader records for
		// each entry from this file (loader.go calls
		// mergeFile(cat, firstSeen, name, data) with the bare name).
		out = append(out, name)
	}
	return out, nil
}
