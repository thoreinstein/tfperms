package resolver

// Direct resolver unit tests. The per-resource catalog harness in
// internal/catalog/resource_test.go covers the happy paths through real
// fixtures; this file fills the gaps that would be awkward to express
// as fixtures — definitive-mismatch conditionals (where the gating
// attribute is resolved but does not equal the expected value) and
// unresolved conditionals (where the gating attribute is cty.NilVal,
// which the parser emits for unresolved expressions but our literal
// fixtures never produce).
//
// Tests construct catalogs in-memory rather than going through
// catalog.Load so a single test can isolate one branch of Resolve at a
// time.

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/thoreinstein/tfperms/internal/catalog"
	"github.com/thoreinstein/tfperms/internal/parser"
)

// TestResolveConditionalFires verifies the happy path: a resource block
// whose attribute matches the conditional's `when:` predicate gets the
// conditional's permissions unioned onto the base set.
func TestResolveConditionalFires(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan:   []string{"storage.buckets.getIamPolicy"},
			Create: []string{"storage.buckets.setIamPolicy"},
			Update: []string{"storage.buckets.setIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"uniform_bucket_level_access": cty.True,
		},
	}}, cat)

	wantPlan := []string{"storage.buckets.get", "storage.buckets.getIamPolicy"}
	wantApply := []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.setIamPolicy",
		"storage.buckets.update",
	}
	assertSliceEqual(t, "plan", res.Plan, wantPlan)
	assertSliceEqual(t, "apply", res.Apply, wantApply)
	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
}

// TestResolveConditionalDoesNotFire verifies that a resource whose
// gating attribute is resolved but unequal to the predicate's expected
// value does NOT receive the conditional's permissions, AND does NOT
// surface anything in Resolution.Unresolved (definitive mismatch is not
// noise-worthy).
func TestResolveConditionalDoesNotFire(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan:   []string{"storage.buckets.getIamPolicy"},
			Create: []string{"storage.buckets.setIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"uniform_bucket_level_access": cty.False,
		},
	}}, cat)

	assertSliceEqual(t, "plan", res.Plan, []string{"storage.buckets.get"})
	assertSliceEqual(t, "apply", res.Apply, []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.update",
	})
	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
}

// TestResolveConditionalUnresolved verifies that when the gating
// attribute is unresolved (cty.NilVal — what the parser emits for an
// expression it cannot statically evaluate, e.g. a `var.X` whose
// variable has no default), the resolver flags it via
// Resolution.Unresolved and does NOT union the conditional's permissions.
func TestResolveConditionalUnresolved(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan:   []string{"storage.buckets.getIamPolicy"},
			Create: []string{"storage.buckets.setIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"uniform_bucket_level_access": cty.NilVal,
		},
	}}, cat)

	assertSliceEqual(t, "plan", res.Plan, []string{"storage.buckets.get"})
	assertSliceEqual(t, "apply", res.Apply, []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.update",
	})
	assertSliceEqual(t, "unresolved", res.Unresolved, []string{
		"google_storage_bucket.primary: uniform_bucket_level_access",
	})
}

// TestResolveConditionalDefinitiveMismatchSwallowsUnresolved verifies
// the short-circuit semantics in matchesConditional: when one predicate
// definitively fails (resolved-but-unequal) and another is unresolved,
// the conditional cannot fire and the unresolved sibling is NOT reported
// — reporting it would be noise because resolving it would not have
// changed the outcome.
func TestResolveConditionalDefinitiveMismatchSwallowsUnresolved(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{
			"uniform_bucket_level_access": true,
			"location":                    "US",
		},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			// Definitive mismatch — conditional cannot fire.
			"location": cty.StringVal("EU"),
			// Unresolved — but irrelevant because of the mismatch above.
			"uniform_bucket_level_access": cty.NilVal,
		},
	}}, cat)

	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
}

// TestResolvePreventDestroy verifies that
// `lifecycle { prevent_destroy = true }` (surfaced as
// parser.Resource.PreventDestroy) suppresses the catalog entry's Delete
// permissions from the Apply set, while leaving Plan / Create / Update
// untouched.
func TestResolvePreventDestroy(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	res := Resolve([]parser.Resource{{
		Kind:           "resource",
		Type:           "google_storage_bucket",
		Name:           "primary",
		PreventDestroy: true,
	}}, cat)

	assertSliceEqual(t, "plan", res.Plan, []string{"storage.buckets.get"})
	// storage.buckets.delete must NOT appear.
	assertSliceEqual(t, "apply", res.Apply, []string{
		"storage.buckets.create",
		"storage.buckets.update",
	})
}

// TestResolvePreventDestroySuppressesConditionalDelete verifies that
// prevent_destroy suppresses Delete contributions from a *fired*
// conditional, not just from the base PermissionSet. The catalog has no
// real conditional that exercises this today (storage's IAM conditional
// has no Delete entry), but the contract is symmetric and we want a
// regression test pinning it.
func TestResolvePreventDestroySuppressesConditionalDelete(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Delete: []string{"storage.buckets.deleteIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind:           "resource",
		Type:           "google_storage_bucket",
		Name:           "primary",
		PreventDestroy: true,
		Attrs: map[string]cty.Value{
			"uniform_bucket_level_access": cty.True,
		},
	}}, cat)

	for _, p := range res.Apply {
		if p == "storage.buckets.deleteIamPolicy" {
			t.Errorf("apply unexpectedly contains conditional Delete %q under prevent_destroy=true; got apply=%v", p, res.Apply)
		}
		if p == "storage.buckets.delete" {
			t.Errorf("apply unexpectedly contains base Delete %q under prevent_destroy=true; got apply=%v", p, res.Apply)
		}
	}
}

// singleResourceCatalog returns a Catalog with one ResourceEntry for
// google_storage_bucket carrying the standard plan/create/update/delete
// permissions. Conditionals override is appended verbatim. Used to
// avoid retyping the same boilerplate in every conditional / prevent-
// destroy test.
//
// The verification fields on the entry are deliberately left empty:
// validate.go's checks only fire through catalog.Load, and Resolve
// itself does not consult them, so plumbing real verification metadata
// here would only obscure the test intent.
func singleResourceCatalog(t *testing.T, typ string, conds []catalog.Conditional) *catalog.Catalog {
	t.Helper()
	return &catalog.Catalog{
		Resources: map[string]*catalog.ResourceEntry{
			typ: {
				Type: typ,
				Permissions: catalog.PermissionSet{
					Plan:   []string{"storage.buckets.get"},
					Create: []string{"storage.buckets.create"},
					Update: []string{"storage.buckets.update"},
					Delete: []string{"storage.buckets.delete"},
				},
				Conditionals: conds,
			},
		},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}
}

// assertSliceEqual fails the test with a diff-friendly message when
// got and want are not element-wise equal. A nil want is treated as the
// empty slice so callers can write `nil` without sprinkling
// `[]string{}` across the test bodies.
func assertSliceEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if len(got) != len(want) {
		t.Errorf("%s length: got %d %v, want %d %v", label, len(got), got, len(want), want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %q, want %q (full got=%v, want=%v)", label, i, got[i], want[i], got, want)
			return
		}
	}
}
