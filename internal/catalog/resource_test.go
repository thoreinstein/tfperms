package catalog_test

// Per-resource catalog regression harness. Companion to
// internal/parser/golden_test.go — bridges the parser and the catalog
// by driving the full pipeline (catalog.Load → parser.LoadRecursive →
// resolver.Resolve) once per catalog entry and pinning the resolver's
// output to a golden file checked into the repo.
//
// Test layout:
//
//	internal/catalog/testdata/
//	  <service>/                  (matches catalog/<service>.yaml)
//	    <terraform-type>/
//	      main.tf                 (the fixture)
//	      expected.json           (the golden, generated)
//
// One fixture directory per Terraform type. A `resource` and a `data`
// entry that share a type (e.g. google_storage_bucket can be both)
// share a fixture: the fixture's main.tf may declare both blocks, and
// the same expected.json validates both subtests because the resolver
// sees the merged set.
//
// Re-generate the goldens after an intentional resolver / catalog
// change:
//
//	go test ./internal/catalog -run TestCatalogResources -update
//
// Coverage contract: every catalog entry MUST have a fixture
// directory. A missing testdata/<service>/<type>/ surfaces as a hard
// test failure naming the missing path. This is the regression bar
// from the Phase 4 of the implementation plan — adding a new resource
// to a catalog YAML without an accompanying fixture is rejected at
// CI rather than allowed to ship as untested.
//
// Why package catalog_test (external) rather than package catalog: the
// resolver imports internal/catalog, so a test in package catalog that
// imported resolver would create a cycle. The external test package
// breaks the cycle while keeping the test file co-located with the
// fixtures it drives.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thoreinstein/tfperms/internal/catalog"
	"github.com/thoreinstein/tfperms/internal/parser"
	"github.com/thoreinstein/tfperms/internal/resolver"
)

// updateGolden controls whether TestCatalogResources writes generated
// output to expected.json (true) or compares against it (false). Wired
// to the `-update` flag so contributors run
// `go test ./internal/catalog -update` to regenerate after an
// intentional behaviour change. Defined at package scope rather than
// init-time so the flag lookup happens once per binary invocation.
var updateGolden = flag.Bool("update", false, "rewrite internal/catalog/testdata/<service>/<type>/expected.json from current resolver output")

// TestCatalogResources drives every entry in the merged catalog through
// the parser → resolver pipeline and compares the result against the
// per-fixture golden file.
//
// Sub-test naming uses the kind/<type> shape — "resources/...",
// "data_sources/...", "iam_bindings/..." — so a failure unambiguously
// identifies which catalog section the entry lives in. Two entries
// from different sections that share the same Terraform type (legal:
// a resource and a data source can have the same type) produce
// distinct subtests against the same fixture, which is fine — the
// golden encodes the resolver's combined output.
func TestCatalogResources(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	for _, typ := range sortedKeys(cat.Resources) {
		entry := cat.Resources[typ]
		service := serviceName(entry.Position.File)
		t.Run("resources/"+typ, func(t *testing.T) {
			runResourceFixture(t, cat, service, typ)
		})
	}
	for _, typ := range sortedKeys(cat.DataSources) {
		entry := cat.DataSources[typ]
		service := serviceName(entry.Position.File)
		t.Run("data_sources/"+typ, func(t *testing.T) {
			runResourceFixture(t, cat, service, typ)
		})
	}
	for _, typ := range sortedKeys(cat.IAMBindings) {
		entry := cat.IAMBindings[typ]
		service := serviceName(entry.Position.File)
		t.Run("iam_bindings/"+typ, func(t *testing.T) {
			runResourceFixture(t, cat, service, typ)
		})
	}
}

// runResourceFixture executes the pipeline for one catalog entry:
//
//  1. Locate the fixture directory at testdata/<service>/<type>/.
//     A missing directory is a fatal test failure (Phase 4 enforcement).
//  2. Parse the fixture via parser.LoadRecursive so the test exercises
//     the same entry point production callers use, and so a fixture
//     can legitimately use local module calls if the resource needs
//     to be set up that way.
//  3. Hand the parsed resources to resolver.Resolve along with the
//     real catalog. The catalog passed in is the full merged catalog,
//     not a per-entry slice — the resolver is supposed to look up
//     types itself, and exercising the production lookup path is more
//     valuable than micro-testing a single entry.
//  4. Marshal the Resolution as indented JSON and compare against (or
//     write) expected.json.
//
// All path-bearing strings in the JSON shape are relativised to
// <SCENARIO_DIR> so goldens stay byte-identical across machines and OS
// path separators. The current Resolution shape (string slices only)
// has no path-bearing fields, so this is a no-op today; the helpers
// stay in place for the Epic 5 expansion that will add file:line
// context to Unknowns / Unresolved.
func runResourceFixture(t *testing.T, cat *catalog.Catalog, service, resourceType string) {
	t.Helper()

	scenarioDir := filepath.Join("testdata", service, resourceType)
	if _, err := os.Stat(scenarioDir); err != nil {
		t.Fatalf(
			"missing fixture directory %s for catalog entry %s/%s: %v\n"+
				"Add %s/main.tf with a representative configuration, then run\n"+
				"  go test ./internal/catalog -run TestCatalogResources -update\n"+
				"to generate the expected.json golden.",
			scenarioDir, service, resourceType, err, scenarioDir,
		)
	}

	absDir, err := filepath.Abs(scenarioDir)
	if err != nil {
		t.Fatalf("abs %q: %v", scenarioDir, err)
	}

	resources, _, _, err := parser.LoadRecursive(absDir)
	if err != nil {
		t.Fatalf("parser.LoadRecursive %s: %v", scenarioDir, err)
	}

	res := resolver.Resolve(resources, cat)

	got, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatalf("marshal resolution: %v", err)
	}
	got = append(got, '\n')

	expectedPath := filepath.Join(scenarioDir, "expected.json")

	if *updateGolden {
		if err := os.WriteFile(expectedPath, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", expectedPath, err)
		}
		return
	}

	want, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf(
			"read %s: %v\n"+
				"Run `go test ./internal/catalog -run TestCatalogResources -update` to create.",
			expectedPath, err,
		)
	}
	if string(want) != string(got) {
		t.Errorf(
			"golden mismatch for %s\n--- want ---\n%s--- got ---\n%s",
			scenarioDir, string(want), string(got),
		)
	}
}

// serviceName derives the testdata service-directory name from a
// catalog entry's source filename. Position.File is always a bare
// "<service>.yaml" because the loader records the entry name relative
// to catalog/, but we still go through filepath.Base defensively in
// case a future loader records a deeper path.
//
// A filename without the ".yaml" suffix (or an empty Position.File)
// returns the input as-is; the surrounding Stat call will then fail
// with a clear "missing fixture" error pointing at the malformed
// path, which is the most useful diagnostic for what is almost
// certainly a loader bug.
func serviceName(file string) string {
	base := filepath.Base(file)
	return strings.TrimSuffix(base, ".yaml")
}

// sortedKeys returns m's keys in lexicographic order. Used to keep
// subtest order deterministic across runs — Go's map iteration order
// is randomised, and a non-deterministic subtest order would make CI
// failure reports harder to read and diff.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
