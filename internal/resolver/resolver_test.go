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

	assertSliceEqual(t, "plan", res.Plan, []string{
		"storage.buckets.get",
		"storage.buckets.getIamPolicy",
	})
	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
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
	assertSliceEqual(t, "exact plan", exact.Plan, []string{
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
	assertSliceEqual(t, "off plan", off.Plan, []string{"storage.buckets.get"})
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

	assertSliceEqual(t, "plan", res.Plan, []string{
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
	assertSliceEqual(t, "plan", res.Plan, []string{"storage.buckets.get"})
	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
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

	assertSliceEqual(t, "plan", res.Plan, []string{"storage.buckets.get"})
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
//
// The assertions match the shape used elsewhere in this file: full
// expected plan / apply slices in lexicographic order. A
// presence-only check on `storage.buckets.delete*` would miss a
// regression where Resolve dropped Create or Update from the conditional
// (still satisfying the negative assertion while breaking the contract);
// the equality check pins both the suppression of Delete and the union
// of every other permission stage.
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
	assertSliceEqual(t, "plan", res.Plan, []string{"storage.buckets.get"})
	// Apply: base Create + Update only. Both Delete sources —
	// base storage.buckets.delete and the fired conditional's
	// storage.buckets.deleteIamPolicy — must be suppressed.
	assertSliceEqual(t, "apply", res.Apply, []string{
		"storage.buckets.create",
		"storage.buckets.update",
	})
	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
}

// TestResolveUnresolvedKeyDistinguishesModulesAndKinds pins the dedup
// key format for Resolution.Unresolved. The same `<type>.<name>` block
// can appear multiple times in a resolution input — once at the root,
// again inside a reused module, and once more as a `data` block — and
// each instance must surface a distinct Unresolved entry when its
// gating attribute is unresolved. Otherwise a single noisy module call
// would silently mask its sibling and the reporter would under-report.
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
// unresolvedKey shape (module-path dotted in front, `data.` segment
// for data blocks, then `<type>.<name>: <attribute>`).
func TestResolveUnresolvedKeyDistinguishesModulesAndKinds(t *testing.T) {
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

	// Sorted lexicographically: "data..." < "foo.bar..." < "foo..." <
	// "google_..." — actually 'd' < 'f' < 'g', and within "foo*",
	// "foo.bar.google_..." < "foo.google_...". Spell out the expected
	// order so the test fails loudly if anyone shuffles unresolvedKey.
	assertSliceEqual(t, "unresolved", res.Unresolved, []string{
		"data.google_storage_bucket.primary: uniform_bucket_level_access",
		"foo.bar.google_storage_bucket.primary: uniform_bucket_level_access",
		"foo.google_storage_bucket.primary: uniform_bucket_level_access",
		"google_storage_bucket.primary: uniform_bucket_level_access",
	})
}

// TestResolveUnknownResourceType pins the Unknowns branch for `resource`
// blocks: when a parsed resource's type is in neither Catalog.Resources
// nor Catalog.IAMBindings, it surfaces in Resolution.Unknowns and
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

	assertSliceEqual(t, "plan", res.Plan, nil)
	assertSliceEqual(t, "apply", res.Apply, nil)
	assertSliceEqual(t, "unknowns", res.Unknowns, []string{"google_unknown_resource"})
	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
}

// TestResolveUnknownDataSourceType pins the Unknowns branch for `data`
// blocks: a data block whose type is absent from Catalog.DataSources
// surfaces in Resolution.Unknowns. The current key shape does not
// distinguish unknown resources from unknown data sources (both store
// r.Type), so a sibling `resource` block of the same unknown type
// would collapse into the same entry — that is intentional under
// today's Unknowns contract and beyond the scope of the unresolved-key
// disambiguation. The test pins the type-only key so a future change
// (e.g. mirroring unresolvedKey's `data.` prefix into Unknowns) is a
// deliberate, test-flagged decision rather than a silent shift.
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

	assertSliceEqual(t, "plan", res.Plan, nil)
	assertSliceEqual(t, "apply", res.Apply, nil)
	assertSliceEqual(t, "unknowns", res.Unknowns, []string{"google_unknown_data"})
	assertSliceEqual(t, "unresolved", res.Unresolved, nil)
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
