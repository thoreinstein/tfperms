package resolver

// Direct resolver unit tests. The per-resource catalog harness in
// internal/catalog/resource_test.go covers the happy paths through real
// fixtures; this file fills the gaps that would be awkward to express
// as fixtures — definitive-mismatch conditionals (where the gating
// attribute is resolved but does not equal the expected value),
// unresolved conditionals (where the gating attribute is cty.NilVal,
// which the parser emits for unresolved expressions but our literal
// fixtures never produce), and the three-set permission partition
// (PlanPerms / ApplyOnlyPerms / TotalApplyPerms).
//
// Tests construct catalogs in-memory rather than going through
// catalog.Load so a single test can isolate one branch of Resolve at a
// time.

import (
	"reflect"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/thoreinstein/tfperms/internal/catalog"
	"github.com/thoreinstein/tfperms/internal/parser"
)

// TestResolveConditionalFires verifies the happy path: a resource block
// whose attribute matches the conditional's `when:` predicate gets the
// conditional's permissions unioned onto the base set. None of the
// permissions overlap between Plan and Apply for this fixture, so
// ApplyOnlyPerms equals (Create ∪ Update ∪ Delete) and TotalApplyPerms
// is the union of every permission in play.
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
	wantApplyOnly := []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.setIamPolicy",
		"storage.buckets.update",
	}
	wantTotalApply := []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.get",
		"storage.buckets.getIamPolicy",
		"storage.buckets.setIamPolicy",
		"storage.buckets.update",
	}
	assertSliceEqual(t, "plan_perms", res.PlanPerms, wantPlan)
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, wantApplyOnly)
	assertSliceEqual(t, "total_apply_perms", res.TotalApplyPerms, wantTotalApply)
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveConditionalDoesNotFire verifies that a resource whose
// gating attribute is resolved but unequal to the predicate's expected
// value does NOT receive the conditional's permissions, AND does NOT
// surface anything in Result.Unresolved (definitive mismatch is not
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

	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.update",
	})
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveConditionalUnresolved verifies that when the gating
// attribute is unresolved (cty.NilVal — what the parser emits for an
// expression it cannot statically evaluate, e.g. a `var.X` whose
// variable has no default), the resolver flags it via
// Result.Unresolved and does NOT union the conditional's permissions.
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

	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.update",
	})
	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{{
		Address:   "google_storage_bucket.primary",
		Attribute: "uniform_bucket_level_access",
	}})
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

	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveConditionalNumberInt verifies that an `int` literal in a
// catalog `when:` clause matches a cty.Number attribute carrying the
// same value. yaml.v3 decodes small integer YAML scalars to Go `int`,
// so this is the typical numeric-predicate path.
func TestResolveConditionalNumberInt(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"versioning_count": int(3)},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"versioning_count": cty.NumberIntVal(3),
		},
	}}, cat)

	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{
		"storage.buckets.get",
		"storage.buckets.getIamPolicy",
	})
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveConditionalNumberInt64Exact pins the precision contract
// for large integer predicates. yaml.v3 decodes integer literals that
// overflow `int` into Go `int64`. The previous implementation cast
// int64 → float64 before comparing, which silently rounded values
// above 2^53 (float64's integer-exact range): `9007199254740993` and
// `9007199254740992` would compare equal because both float64-round
// to 2^53. After the fix, the resolver routes int64 predicates through
// big.Float.SetInt64, which is exact for the entire int64 range.
//
// This test distinguishes the two values: a resource with attribute
// 9007199254740993 must NOT match a `when:` predicate of
// 9007199254740992, and vice versa.
func TestResolveConditionalNumberInt64Exact(t *testing.T) {
	const (
		twoTo53     int64 = 1 << 53       // 9007199254740992
		twoTo53Plus int64 = (1 << 53) + 1 // 9007199254740993
	)

	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"quota_limit": twoTo53Plus},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	// Attribute = 2^53 + 1, predicate = 2^53 + 1: must match.
	exact := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"quota_limit": cty.NumberIntVal(twoTo53Plus),
		},
	}}, cat)
	assertSliceEqual(t, "exact plan_perms", exact.PlanPerms, []string{
		"storage.buckets.get",
		"storage.buckets.getIamPolicy",
	})

	// Attribute = 2^53, predicate = 2^53 + 1: must NOT match. Under the
	// old float64-cast path both sides would float64-round to 2^53 and
	// compare equal — that is the regression.
	off := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"quota_limit": cty.NumberIntVal(twoTo53),
		},
	}}, cat)
	assertSliceEqual(t, "off plan_perms", off.PlanPerms, []string{"storage.buckets.get"})
}

// TestResolveConditionalNumberFloat64 verifies that a float64 literal
// in the catalog `when:` clause matches a cty.Number carrying the
// same fractional value. yaml.v3 decodes scalars with a decimal point
// or exponent into Go float64.
func TestResolveConditionalNumberFloat64(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"sample_ratio": float64(0.25)},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"sample_ratio": cty.NumberFloatVal(0.25),
		},
	}}, cat)

	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{
		"storage.buckets.get",
		"storage.buckets.getIamPolicy",
	})
}

// TestResolveConditionalNumberMismatch verifies that a numeric
// predicate whose actual value is a definitive mismatch does NOT fire
// the conditional and does NOT surface anything in Unresolved.
func TestResolveConditionalNumberMismatch(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"versioning_count": int(3)},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"versioning_count": cty.NumberIntVal(7),
		},
	}}, cat)

	// The conditional did not fire — base permissions only.
	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveConditionalNumberWrongType verifies that a numeric
// predicate compared against a non-Number attribute (e.g. a string
// that happens to look like a digit) returns false rather than
// panicking or coercing. The catalog YAML schema gives `when:` values
// stable Go types; a type mismatch is a definitive non-match, not
// an unresolved one.
func TestResolveConditionalNumberWrongType(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"versioning_count": int(3)},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			// String "3" must not equal numeric predicate 3.
			"versioning_count": cty.StringVal("3"),
		},
	}}, cat)

	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolvePreventDestroy verifies that
// `lifecycle { prevent_destroy = true }` (surfaced as
// parser.Resource.PreventDestroy) suppresses the catalog entry's Delete
// permissions from the apply sets, while leaving Plan / Create / Update
// untouched.
func TestResolvePreventDestroy(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	res := Resolve([]parser.Resource{{
		Kind:           "resource",
		Type:           "google_storage_bucket",
		Name:           "primary",
		PreventDestroy: true,
	}}, cat)

	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
	// storage.buckets.delete must NOT appear in either apply set.
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
		"storage.buckets.create",
		"storage.buckets.update",
	})
	assertSliceEqual(t, "total_apply_perms", res.TotalApplyPerms, []string{
		"storage.buckets.create",
		"storage.buckets.get",
		"storage.buckets.update",
	})
}

// TestResolvePreventDestroySuppressesConditionalDelete verifies that
// prevent_destroy suppresses Delete contributions from a *fired*
// conditional, not just from the base PermissionSet. The catalog has no
// real conditional that exercises this today (storage's IAM conditional
// has no Delete entry), but the contract is symmetric and we want a
// regression test pinning it.
//
// The assertions match the shape used elsewhere in this file: full
// expected slices in lexicographic order. A presence-only check on
// `storage.buckets.delete*` would miss a regression where Resolve
// dropped Create or Update from the conditional (still satisfying the
// negative assertion while breaking the contract); the equality check
// pins both the suppression of Delete and the union of every other
// permission stage.
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

	// The base PermissionSet has no Plan beyond storage.buckets.get,
	// and the conditional contributes only Delete (which prevent_destroy
	// suppresses). Plan therefore stays at the base entry.
	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
	// Apply: base Create + Update only. Both Delete sources —
	// base storage.buckets.delete and the fired conditional's
	// storage.buckets.deleteIamPolicy — must be suppressed.
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
		"storage.buckets.create",
		"storage.buckets.update",
	})
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveThreeSetPartition pins the three-set partition contract:
// a permission appearing in BOTH the entry's Plan list (refresh) AND
// its Update list (apply) must surface in PlanPerms and TotalApplyPerms
// but NOT in ApplyOnlyPerms. The PDR's Epic 5 calls this out as the
// distinguishing characteristic of `apply_only_perms`: "apply
// permissions that are not also plan permissions". A naive resolver
// that just unions Create / Update / Delete into a separate set would
// silently double-count overlapping permissions when the reporter
// concatenates PlanPerms + ApplyOnlyPerms; the partition prevents that.
//
// The fixture is contrived but the situation is real: imagine a
// hypothetical resource whose Update API call requires `.get` to read
// the existing state before writing the change. A literal entry with
// Plan = [".get"] and Update = [".get"] expresses this without
// double-counting.
func TestResolveThreeSetPartition(t *testing.T) {
	cat := &catalog.Catalog{
		Resources: map[string]*catalog.ResourceEntry{
			"google_storage_bucket": {
				Type: "google_storage_bucket",
				Permissions: catalog.PermissionSet{
					// .get appears in BOTH stages.
					Plan:   []string{"storage.buckets.get"},
					Create: []string{"storage.buckets.create"},
					// Update needs the same .get the refresh needs.
					Update: []string{"storage.buckets.get", "storage.buckets.update"},
					Delete: []string{"storage.buckets.delete"},
				},
			},
		},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
	}}, cat)

	// PlanPerms gets .get.
	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
	// ApplyOnlyPerms is (Create ∪ Update ∪ Delete) \ PlanPerms.
	// The Update list contributes .update (kept) and .get (dropped — in PlanPerms).
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.update",
	})
	// TotalApplyPerms is the union — .get appears here exactly once.
	assertSliceEqual(t, "total_apply_perms", res.TotalApplyPerms, []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.get",
		"storage.buckets.update",
	})
}

// TestResolveUnresolvedAddressDistinguishesModulesAndKinds pins the
// dedup key format for Result.Unresolved. The same `<type>.<name>`
// block can appear multiple times in a resolution input — once at the
// root, again inside a reused module, and once more as a `data` block
// — and each instance must surface a distinct Unresolved entry when
// its gating attribute is unresolved. Otherwise a single noisy module
// call would silently mask its sibling and the reporter would
// under-report.
//
// This test seeds four resources sharing the type/name
// `google_storage_bucket.primary` but differing in (Kind, ModulePath):
//
//	resource at root
//	data     at root
//	resource in module ["foo"]
//	resource in module ["foo", "bar"]
//
// All four have an unresolved gating attribute, so all four must land
// in Unresolved as separate entries, sorted lexicographically by the
// resourceAddress shape (module-path dotted in front, `data.` segment
// for data blocks, then `<type>.<name>`).
func TestResolveUnresolvedAddressDistinguishesModulesAndKinds(t *testing.T) {
	cat := &catalog.Catalog{
		Resources: map[string]*catalog.ResourceEntry{
			"google_storage_bucket": {
				Type: "google_storage_bucket",
				Permissions: catalog.PermissionSet{
					Plan: []string{"storage.buckets.get"},
				},
				Conditionals: []catalog.Conditional{{
					When: map[string]any{"uniform_bucket_level_access": true},
					Permissions: catalog.PermissionSet{
						Plan: []string{"storage.buckets.getIamPolicy"},
					},
				}},
			},
		},
		DataSources: map[string]*catalog.DataSourceEntry{
			"google_storage_bucket": {
				Type: "google_storage_bucket",
				Permissions: catalog.DataSourcePermissions{
					Plan: []string{"storage.buckets.get"},
				},
				Conditionals: []catalog.DataSourceConditional{{
					When: map[string]any{"uniform_bucket_level_access": true},
					Permissions: catalog.DataSourcePermissions{
						Plan: []string{"storage.buckets.getIamPolicy"},
					},
				}},
			},
		},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}

	unresolvedAttrs := map[string]cty.Value{"uniform_bucket_level_access": cty.NilVal}

	res := Resolve([]parser.Resource{
		{Kind: "resource", Type: "google_storage_bucket", Name: "primary", Attrs: unresolvedAttrs},
		{Kind: "data", Type: "google_storage_bucket", Name: "primary", Attrs: unresolvedAttrs},
		{Kind: "resource", Type: "google_storage_bucket", Name: "primary", ModulePath: []string{"foo"}, Attrs: unresolvedAttrs},
		{Kind: "resource", Type: "google_storage_bucket", Name: "primary", ModulePath: []string{"foo", "bar"}, Attrs: unresolvedAttrs},
	}, cat)

	// Sorted lexicographically by Address: "data..." < "foo.bar..." <
	// "foo..." < "google_..." — 'd' < 'f' < 'g', and within "foo*",
	// "foo.bar.google_..." < "foo.google_...". Spell out the expected
	// order so the test fails loudly if anyone shuffles resourceAddress.
	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{
		{Address: "data.google_storage_bucket.primary", Attribute: "uniform_bucket_level_access"},
		{Address: "foo.bar.google_storage_bucket.primary", Attribute: "uniform_bucket_level_access"},
		{Address: "foo.google_storage_bucket.primary", Attribute: "uniform_bucket_level_access"},
		{Address: "google_storage_bucket.primary", Attribute: "uniform_bucket_level_access"},
	})
}

// TestResolveUnknownResourceType pins the Unknowns branch for `resource`
// blocks: when a parsed resource's type is in neither Catalog.Resources
// nor Catalog.IAMBindings, it surfaces in Result.Unknowns and
// contributes nothing to plan / apply. Uses an empty catalog so the
// unknown branch is the only path through Resolve.
func TestResolveUnknownResourceType(t *testing.T) {
	cat := &catalog.Catalog{
		Resources:   map[string]*catalog.ResourceEntry{},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_unknown_resource",
		Name: "x",
	}}, cat)

	assertSliceEqual(t, "plan_perms", res.PlanPerms, nil)
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, nil)
	assertSliceEqual(t, "total_apply_perms", res.TotalApplyPerms, nil)
	assertUnknownsEqual(t, res.Unknowns, []UnknownResource{
		{Type: "google_unknown_resource"},
	})
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveUnknownDataSourceType pins the Unknowns branch for `data`
// blocks: a data block whose type is absent from Catalog.DataSources
// surfaces in Result.Unknowns. The Unknowns key encodes the parser's
// File and Line in addition to the Type, so a `data` block and a
// `resource` block of the same type at different source locations
// produce distinct entries — this test does not exercise that
// distinction (zero-valued File/Line on both sides would collapse a
// resource and a data block of the same Type), and the resource_test.go
// fixture for google_project_iam_policy is the regression for the
// real-source-location case.
func TestResolveUnknownDataSourceType(t *testing.T) {
	cat := &catalog.Catalog{
		Resources:   map[string]*catalog.ResourceEntry{},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}

	res := Resolve([]parser.Resource{{
		Kind: "data",
		Type: "google_unknown_data",
		Name: "x",
	}}, cat)

	assertSliceEqual(t, "plan_perms", res.PlanPerms, nil)
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, nil)
	assertSliceEqual(t, "total_apply_perms", res.TotalApplyPerms, nil)
	assertUnknownsEqual(t, res.Unknowns, []UnknownResource{
		{Type: "google_unknown_data"},
	})
	assertUnresolvedEqual(t, res.Unresolved, nil)
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

// assertUnknownsEqual fails the test when got and want differ in
// length, order, or any field of any entry. A nil want is treated as
// the empty slice so callers can write `nil` for the no-unknowns case.
//
// Uses reflect.DeepEqual on each pair so a missing field comparison
// (e.g. a future field added to UnknownResource that the test forgot
// to set) is caught structurally rather than requiring per-field
// asserts.
func assertUnknownsEqual(t *testing.T, got, want []UnknownResource) {
	t.Helper()
	if want == nil {
		want = []UnknownResource{}
	}
	if len(got) != len(want) {
		t.Errorf("unknowns length: got %d %v, want %d %v", len(got), got, len(want), want)
		return
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("unknowns[%d]: got %#v, want %#v (full got=%#v, want=%#v)", i, got[i], want[i], got, want)
			return
		}
	}
}

// assertUnresolvedEqual fails the test when got and want differ in
// length, order, or any field of any entry. A nil want is treated as
// the empty slice so callers can write `nil` for the no-unresolved
// case.
//
// Uses reflect.DeepEqual on each pair so a missing field comparison
// (e.g. a future field added to UnresolvedConditional that the test
// forgot to set) is caught structurally rather than requiring
// per-field asserts.
func assertUnresolvedEqual(t *testing.T, got, want []UnresolvedConditional) {
	t.Helper()
	if want == nil {
		want = []UnresolvedConditional{}
	}
	if len(got) != len(want) {
		t.Errorf("unresolved length: got %d %v, want %d %v", len(got), got, len(want), want)
		return
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("unresolved[%d]: got %#v, want %#v (full got=%#v, want=%#v)", i, got[i], want[i], got, want)
			return
		}
	}
}
