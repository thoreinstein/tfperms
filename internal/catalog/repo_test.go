package catalog

import (
	"testing"
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
