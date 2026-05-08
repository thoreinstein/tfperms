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

// TestResolveMultipleConditionals verifies that multiple conditionals on
// a single resource are evaluated independently and ALL matching ones
// contribute their permissions to the union.
func TestResolveMultipleConditionals(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{
		{
			When: map[string]any{"uniform_bucket_level_access": true},
			Permissions: catalog.PermissionSet{
				Plan: []string{"storage.buckets.getIamPolicy"},
			},
		},
		{
			When: map[string]any{"versioning": true},
			Permissions: catalog.PermissionSet{
				Plan: []string{"storage.buckets.getVersioning"},
			},
		},
	})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"uniform_bucket_level_access": cty.True,
			"versioning":                  cty.True,
		},
	}}, cat)

	// Base permissions + both conditionals' contributions.
	wantPlan := []string{
		"storage.buckets.get",
		"storage.buckets.getIamPolicy",
		"storage.buckets.getVersioning",
	}
	assertSliceEqual(t, "plan_perms", res.PlanPerms, wantPlan)
}

// TestResolveIAMBindingConditional verifies that a conditional on an
// IAM binding entry whose `when:` predicate matches the resource's
// resolved attribute fires and unions ALL four permission stages —
// plan, create, update, delete — onto the IAM binding's base set. This
// pins the conditional-application loop in Resolve's IAM binding branch
// (resolver.go: the `for _, cond := range entry.Conditionals` block on
// the IAM binding lookup), which mirrors the resource branch but goes
// through cat.IAMBindings rather than cat.Resources. A regression that
// dropped, say, the conditional Update union from the IAM binding branch
// would silently leak setIamPolicy from total_apply_perms here.
//
// The fixture uses distinct permission strings on the base set
// ("storage.buckets.*") and the conditional ("resourcemanager.projects.*")
// so each side's contribution is observable in every result slice. The
// three-set partition assertion (plan / apply-only / total-apply) is
// the same shape used in TestResolveConditionalFires for resources,
// which keeps the IAM binding test directly comparable with its
// resource sibling.
func TestResolveIAMBindingConditional(t *testing.T) {
	cat := &catalog.Catalog{
		Resources: map[string]*catalog.ResourceEntry{
			"google_storage_bucket": {Type: "google_storage_bucket"},
		},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{
			"google_storage_bucket_iam_binding": {
				Type:           "google_storage_bucket_iam_binding",
				ParentResource: "google_storage_bucket",
				Permissions: catalog.PermissionSet{
					Plan:   []string{"storage.buckets.getIamPolicy"},
					Create: []string{"storage.buckets.setIamPolicy"},
					Update: []string{"storage.buckets.setIamPolicy"},
					Delete: []string{"storage.buckets.setIamPolicy"},
				},
				Conditionals: []catalog.Conditional{{
					When: map[string]any{"role": "roles/owner"},
					Permissions: catalog.PermissionSet{
						Plan:   []string{"resourcemanager.projects.getIamPolicy"},
						Create: []string{"resourcemanager.projects.setIamPolicy"},
						Update: []string{"resourcemanager.projects.setIamPolicy"},
						Delete: []string{"resourcemanager.projects.setIamPolicy"},
					},
				}},
			},
		},
	}

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket_iam_binding",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"role": cty.StringVal("roles/owner"),
		},
	}}, cat)

	// Plan picks up both the base getIamPolicy and the conditional's
	// projects.getIamPolicy.
	wantPlan := []string{
		"resourcemanager.projects.getIamPolicy",
		"storage.buckets.getIamPolicy",
	}
	// ApplyOnly = (Create ∪ Update ∪ Delete) \ Plan. None of the
	// setIamPolicy permissions appear in Plan, so both base and
	// conditional setIamPolicy land here exactly once each (set
	// semantics: Create/Update/Delete all share the same string per
	// side).
	wantApplyOnly := []string{
		"resourcemanager.projects.setIamPolicy",
		"storage.buckets.setIamPolicy",
	}
	// TotalApply = Plan ∪ ApplyOnly — every permission contributed by
	// either side, exactly once.
	wantTotalApply := []string{
		"resourcemanager.projects.getIamPolicy",
		"resourcemanager.projects.setIamPolicy",
		"storage.buckets.getIamPolicy",
		"storage.buckets.setIamPolicy",
	}
	assertSliceEqual(t, "plan_perms", res.PlanPerms, wantPlan)
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, wantApplyOnly)
	assertSliceEqual(t, "total_apply_perms", res.TotalApplyPerms, wantTotalApply)
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveIAMBindingConditionalUnresolved verifies that the IAM
// binding branch of Resolve emits an UnresolvedConditional when a
// conditional's gating attribute is unresolved (cty.NilVal — the
// parser's marker for an expression like `var.X` it could not
// statically evaluate). Pins resolver.go's
//
//	for _, attr := range missing {
//	    unresolved[unresolvedRecordKey{...}] = struct{}{}
//	}
//
// inside the IAM binding lookup branch. Without this assertion, a
// regression that dropped the IAM binding `missing` walk would leave
// users without a warning for the exact case the warning was added
// for: a `var.role` whose default is unset.
//
// The conditional is NOT expected to fire (the gating attribute is
// unresolved), so PlanPerms must equal the base set and the conditional
// permissions must NOT leak into any apply slice. The Unresolved entry
// must carry the IAM binding's resourceAddress and the gating
// Attribute name.
func TestResolveIAMBindingConditionalUnresolved(t *testing.T) {
	cat := &catalog.Catalog{
		Resources: map[string]*catalog.ResourceEntry{
			"google_storage_bucket": {Type: "google_storage_bucket"},
		},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{
			"google_storage_bucket_iam_binding": {
				Type:           "google_storage_bucket_iam_binding",
				ParentResource: "google_storage_bucket",
				Permissions: catalog.PermissionSet{
					Plan:   []string{"storage.buckets.getIamPolicy"},
					Create: []string{"storage.buckets.setIamPolicy"},
				},
				Conditionals: []catalog.Conditional{{
					When: map[string]any{"role": "roles/owner"},
					Permissions: catalog.PermissionSet{
						// Distinctive permission so a leak would be
						// obvious in the assertion diff.
						Plan: []string{"resourcemanager.projects.getIamPolicy"},
					},
				}},
			},
		},
	}

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket_iam_binding",
		Name: "primary",
		Attrs: map[string]cty.Value{
			"role": cty.NilVal,
		},
	}}, cat)

	// Base permissions only — conditional did not fire.
	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.getIamPolicy"})
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{"storage.buckets.setIamPolicy"})
	// One unresolved entry from the IAM binding branch.
	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{{
		ResourceType: "google_storage_bucket_iam_binding",
		ResourceName: "primary",
		Attribute:    "role",
		Reason:       parser.ReasonOther,
	}})
}

// TestResolveIAMBindingPreventDestroySuppressesConditionalDelete is the
// IAM binding sibling of
// TestResolvePreventDestroySuppressesConditionalDelete. It pins that
// `lifecycle { prevent_destroy = true }` suppresses Delete contributions
// from a fired IAM binding conditional, not just from the base
// PermissionSet. The IAM binding branch in Resolve has its own
// `if !r.PreventDestroy` guard for the conditional Delete union; a
// regression that dropped the guard there would leak setIamPolicy
// (or any conditional-only Delete permission) into apply_only_perms
// even when the user opted out of destroy.
func TestResolveIAMBindingPreventDestroySuppressesConditionalDelete(t *testing.T) {
	cat := &catalog.Catalog{
		Resources: map[string]*catalog.ResourceEntry{
			"google_storage_bucket": {Type: "google_storage_bucket"},
		},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{
			"google_storage_bucket_iam_binding": {
				Type:           "google_storage_bucket_iam_binding",
				ParentResource: "google_storage_bucket",
				Permissions: catalog.PermissionSet{
					Plan:   []string{"storage.buckets.getIamPolicy"},
					Create: []string{"storage.buckets.setIamPolicy"},
					Delete: []string{"storage.buckets.setIamPolicy"},
				},
				Conditionals: []catalog.Conditional{{
					When: map[string]any{"role": "roles/owner"},
					Permissions: catalog.PermissionSet{
						// Distinctive Delete-only permission so a leak
						// is unambiguous.
						Delete: []string{"resourcemanager.projects.deleteIamPolicy"},
					},
				}},
			},
		},
	}

	res := Resolve([]parser.Resource{{
		Kind:           "resource",
		Type:           "google_storage_bucket_iam_binding",
		Name:           "primary",
		PreventDestroy: true,
		Attrs: map[string]cty.Value{
			"role": cty.StringVal("roles/owner"),
		},
	}}, cat)

	// Plan: base getIamPolicy only — conditional contributes only Delete.
	assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.getIamPolicy"})
	// Apply: base Create only. Both Delete sources — base
	// storage.buckets.setIamPolicy and conditional
	// resourcemanager.projects.deleteIamPolicy — must be suppressed.
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{"storage.buckets.setIamPolicy"})
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
		ResourceType: "google_storage_bucket",
		ResourceName: "primary",
		Attribute:    "uniform_bucket_level_access",
		Reason:       parser.ReasonOther,
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

// TestResolvePreventDestroyMultiInstance pins the cross-instance
// behaviour of `lifecycle { prevent_destroy = true }` when multiple
// resources of the same Terraform type appear in the same configuration.
//
// Resolve accumulates every resource's contributions into a single
// shared `apply` map keyed by permission string; the map is consumed
// once at the end of the loop to produce ApplyOnlyPerms /
// TotalApplyPerms. A consequence of that shape is a logical-OR over
// the per-instance prevent_destroy flag: a type's Delete permissions
// appear in the apply sets iff *at least one* instance of that type
// has PreventDestroy=false. They are suppressed iff *every* instance
// of the type opts out of destroy. That is exactly what the spec
// asks for — Terraform's `terraform apply` against the bucket whose
// destroy is allowed must still hold the delete permission, so the
// per-type union is the right granularity for the SA's permission
// set.
//
// The previous tests pinned the single-instance behaviour. This test
// pins the multi-instance behaviour explicitly so a future refactor
// that, say, switches the apply map to a per-instance slice of
// permission sets cannot quietly regress the OR semantics. Each
// subtest stands alone — they construct fresh catalogs and resource
// lists rather than sharing state — so a failure points at the
// specific scenario without test-ordering ambiguity.
func TestResolvePreventDestroyMultiInstance(t *testing.T) {
	// One instance protected, one unprotected: the unprotected
	// instance's iteration is what populates Delete in the shared
	// apply map, so Delete must appear in the apply sets even though
	// a sibling resource of the same type has prevent_destroy set.
	t.Run("mixed_protection_includes_delete", func(t *testing.T) {
		cat := singleResourceCatalog(t, "google_storage_bucket", nil)

		res := Resolve([]parser.Resource{
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "protected",
				PreventDestroy: true,
			},
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "unprotected",
				PreventDestroy: false,
			},
		}, cat)

		assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
		// Delete is present because the unprotected instance contributes it.
		assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
			"storage.buckets.create",
			"storage.buckets.delete",
			"storage.buckets.update",
		})
		assertSliceEqual(t, "total_apply_perms", res.TotalApplyPerms, []string{
			"storage.buckets.create",
			"storage.buckets.delete",
			"storage.buckets.get",
			"storage.buckets.update",
		})
		assertUnresolvedEqual(t, res.Unresolved, nil)
	})

	// Every instance of the type is protected: Delete must be
	// suppressed everywhere because no iteration adds it to the
	// shared apply map. This is the only configuration in which
	// prevent_destroy actually removes Delete from the SA's
	// permission set under multi-instance.
	t.Run("all_protected_excludes_delete", func(t *testing.T) {
		cat := singleResourceCatalog(t, "google_storage_bucket", nil)

		res := Resolve([]parser.Resource{
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "primary",
				PreventDestroy: true,
			},
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "secondary",
				PreventDestroy: true,
			},
		}, cat)

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
		assertUnresolvedEqual(t, res.Unresolved, nil)
	})

	// Conditional sibling of mixed_protection_includes_delete: a
	// fired conditional that contributes a distinctive Delete-only
	// permission must still surface that permission when at least
	// one instance is unprotected. The conditional fires on both
	// instances (their attributes match), so the per-instance
	// contribution is asymmetric: the protected instance does NOT
	// add the conditional Delete, the unprotected instance DOES.
	// The shared map collapses that asymmetry into "permission is
	// present", which is the OR property we want.
	t.Run("conditional_mixed_protection_includes_delete", func(t *testing.T) {
		cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
			When: map[string]any{"uniform_bucket_level_access": true},
			Permissions: catalog.PermissionSet{
				// Distinctive Delete-only perm so a leak / suppression
				// regression is unambiguous.
				Delete: []string{"storage.buckets.deleteIamPolicy"},
			},
		}})

		res := Resolve([]parser.Resource{
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "protected",
				PreventDestroy: true,
				Attrs: map[string]cty.Value{
					"uniform_bucket_level_access": cty.True,
				},
			},
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "unprotected",
				PreventDestroy: false,
				Attrs: map[string]cty.Value{
					"uniform_bucket_level_access": cty.True,
				},
			},
		}, cat)

		assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
		// Both Delete sources surface: base storage.buckets.delete
		// from the unprotected instance's base PermissionSet, and
		// storage.buckets.deleteIamPolicy from the unprotected
		// instance's fired conditional.
		assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
			"storage.buckets.create",
			"storage.buckets.delete",
			"storage.buckets.deleteIamPolicy",
			"storage.buckets.update",
		})
		assertUnresolvedEqual(t, res.Unresolved, nil)
	})

	// Conditional sibling of all_protected_excludes_delete: both
	// instances are protected AND both fire the conditional. Neither
	// the base nor the conditional Delete must appear, because no
	// iteration's `if !r.PreventDestroy` guard opens.
	t.Run("conditional_all_protected_excludes_delete", func(t *testing.T) {
		cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
			When: map[string]any{"uniform_bucket_level_access": true},
			Permissions: catalog.PermissionSet{
				Delete: []string{"storage.buckets.deleteIamPolicy"},
			},
		}})

		res := Resolve([]parser.Resource{
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "primary",
				PreventDestroy: true,
				Attrs: map[string]cty.Value{
					"uniform_bucket_level_access": cty.True,
				},
			},
			{
				Kind:           "resource",
				Type:           "google_storage_bucket",
				Name:           "secondary",
				PreventDestroy: true,
				Attrs: map[string]cty.Value{
					"uniform_bucket_level_access": cty.True,
				},
			},
		}, cat)

		assertSliceEqual(t, "plan_perms", res.PlanPerms, []string{"storage.buckets.get"})
		// Both Delete sources must be suppressed.
		assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, []string{
			"storage.buckets.create",
			"storage.buckets.update",
		})
		assertUnresolvedEqual(t, res.Unresolved, nil)
	})
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

// TestResolveUnresolvedDistinguishesResourceAndDataKinds pins the
// dedup key format for Result.Unresolved. After the schema change
// that replaced Address with ResourceType/ResourceName, the resource
// and data branches of Resolve dedup independently because they live
// in different (File, Line) tuples in real Terraform configurations.
// In this synthetic test File/Line are zero on both sides, so we
// distinguish the two unresolved entries by giving them different
// names; without that the dedup map would correctly collapse them
// to a single row.
//
// The reused-module case — same Type/Name/File/Line, distinguished
// only by ModulePath — is covered by
// TestResolveUnresolvedDistinguishesReusedModuleInstantiations
// below; this test focuses on the resource/data discrimination.
func TestResolveUnresolvedDistinguishesResourceAndDataKinds(t *testing.T) {
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
		{Kind: "resource", Type: "google_storage_bucket", Name: "primary_resource", Attrs: unresolvedAttrs},
		{Kind: "data", Type: "google_storage_bucket", Name: "primary_data", Attrs: unresolvedAttrs},
	}, cat)

	// sortedUnresolved sorts by
	// (File, Line, ResourceType, Attribute, ResourceName, ModulePath).
	// Both rows share File="", Line=0, ResourceType, and Attribute, so
	// the deterministic ordering here comes from the ResourceName
	// tiebreaker in the resolver's sort — not from Go's map iteration
	// order, which is intentionally randomised.
	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "primary_data",
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonOther,
		},
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "primary_resource",
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonOther,
		},
	})
}

// TestResolveUnresolvedDistinguishesReusedModuleInstantiations is the
// regression test for the dedup-key bug an earlier iteration of this
// schema introduced when it dropped module-path differentiation.
//
// LoadRecursive instantiates a single module template at every call
// site by deep-copying its resources and prepending the call-site
// module name to ModulePath, but it does not rewrite File or Line —
// those still point at the module's source file. So two parent call
// sites (`module "x"` and `module "y"`, both pointing at the same
// `./mod` directory) produce two parser.Resource values with
// identical (Type, Name, File, Line) tuples that differ only in
// ModulePath:
//
//	{Type: "google_storage_bucket", Name: "primary",
//	 File: "/abs/mod/main.tf", Line: 3, ModulePath: ["x"]}
//	{Type: "google_storage_bucket", Name: "primary",
//	 File: "/abs/mod/main.tf", Line: 3, ModulePath: ["y"]}
//
// If the resolver's dedup map keys on (Type, Name, Attribute, File,
// Line) only — the bug — both resources collapse to a single
// Unresolved row and the reporter under-reports: the user sees one
// warning for two genuinely distinct call sites and cannot tell
// which one needs the variable default. The dedup key therefore has
// to incorporate ModulePath (encoded via encodeModulePath into a
// string that is comparable for use as a map key); the JSON output
// surfaces the chain via UnresolvedConditional.ModulePath so the
// reporter can render the module path back to the user.
//
// This fixture mirrors the LoadRecursive output shape: same File,
// same Line, distinct ModulePath. The expected output has TWO rows.
func TestResolveUnresolvedDistinguishesReusedModuleInstantiations(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	unresolvedAttrs := map[string]cty.Value{"uniform_bucket_level_access": cty.NilVal}

	res := Resolve([]parser.Resource{
		{
			Kind:       "resource",
			Type:       "google_storage_bucket",
			Name:       "primary",
			File:       "/abs/mod/main.tf",
			Line:       3,
			ModulePath: []string{"x"},
			Attrs:      unresolvedAttrs,
		},
		{
			Kind:       "resource",
			Type:       "google_storage_bucket",
			Name:       "primary",
			File:       "/abs/mod/main.tf",
			Line:       3,
			ModulePath: []string{"y"},
			Attrs:      unresolvedAttrs,
		},
	}, cat)

	// Sort tier here is (File, Line, ResourceType, Attribute,
	// ResourceName, ModulePath). Both rows tie on the first five so
	// the ModulePath tiebreaker decides: ["x"] < ["y"].
	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "primary",
			ModulePath:   []string{"x"},
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonOther,
			File:         "/abs/mod/main.tf",
			Line:         3,
		},
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "primary",
			ModulePath:   []string{"y"},
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonOther,
			File:         "/abs/mod/main.tf",
			Line:         3,
		},
	})
}

// TestResolveUnresolvedModulePathSortOrder pins the lexicographic
// segment-by-segment sort on ModulePath that moduleLess implements:
// shorter prefixes sort before their extensions, and otherwise the
// comparison runs through segments in order. Without this assertion
// a future refactor could replace moduleLess with a strings.Join +
// string compare that breaks the prefix invariant
// ([] < [a] < [a, b] < [b]) — strings.Join("a", "") < "ab" but
// strings.Join("a", "b", "") > strings.Join("ab", "") only by accident
// of the separator chosen.
func TestResolveUnresolvedModulePathSortOrder(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	unresolvedAttrs := map[string]cty.Value{"uniform_bucket_level_access": cty.NilVal}

	mk := func(modulePath []string) parser.Resource {
		return parser.Resource{
			Kind:       "resource",
			Type:       "google_storage_bucket",
			Name:       "primary",
			File:       "/abs/mod/main.tf",
			Line:       3,
			ModulePath: modulePath,
			Attrs:      unresolvedAttrs,
		}
	}

	res := Resolve([]parser.Resource{
		mk([]string{"foo"}),
		mk(nil),
		mk([]string{"foo", "bar"}),
	}, cat)

	// Expected order: nil/empty < ["foo"] < ["foo", "bar"]. All three
	// rows tie on (File, Line, Type, Attribute, Name) so the only
	// discriminator is ModulePath.
	got := res.Unresolved
	if len(got) != 3 {
		t.Fatalf("unresolved length: got %d %#v, want 3", len(got), got)
	}
	if got[0].ModulePath != nil {
		t.Errorf("unresolved[0].ModulePath: got %#v, want nil (root)", got[0].ModulePath)
	}
	if want := []string{"foo"}; !reflect.DeepEqual(got[1].ModulePath, want) {
		t.Errorf("unresolved[1].ModulePath: got %#v, want %#v", got[1].ModulePath, want)
	}
	if want := []string{"foo", "bar"}; !reflect.DeepEqual(got[2].ModulePath, want) {
		t.Errorf("unresolved[2].ModulePath: got %#v, want %#v", got[2].ModulePath, want)
	}
}

// TestResolveUnresolvedClonesModulePath pins that the resolver does
// not share the parser's ModulePath backing array with the JSON
// output. LoadRecursive caches templates and shares the same slice
// across multiple call sites; if a downstream caller mutates
// UnresolvedConditional.ModulePath in place, that mutation must not
// leak back into the parser's cached state. cloneModulePath is the
// barrier; this test fails if the resolver ever drops it.
func TestResolveUnresolvedClonesModulePath(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	parserPath := []string{"original_module"}
	res := Resolve([]parser.Resource{{
		Kind:       "resource",
		Type:       "google_storage_bucket",
		Name:       "primary",
		File:       "/abs/mod/main.tf",
		Line:       3,
		ModulePath: parserPath,
		Attrs:      map[string]cty.Value{"uniform_bucket_level_access": cty.NilVal},
	}}, cat)

	if len(res.Unresolved) != 1 {
		t.Fatalf("unresolved length: got %d, want 1", len(res.Unresolved))
	}

	// Mutate the resolver-returned slice and verify the parser-side
	// slice is untouched.
	res.Unresolved[0].ModulePath[0] = "MUTATED"
	if parserPath[0] != "original_module" {
		t.Errorf("parser ModulePath was mutated through resolver output: got %q, want %q",
			parserPath[0], "original_module")
	}
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
		{Type: "google_unknown_resource", Name: "x"},
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
		{Type: "google_unknown_data", Name: "x"},
	})
	assertUnresolvedEqual(t, res.Unresolved, nil)
}

// TestResolveUnknownResourceCapturesSourceLocation pins that the
// resolver propagates parser.Resource.File and parser.Resource.Line
// onto UnknownResource entries. Reporters need this to point users at
// the offending block; without it the warning is just "tfperms does
// not know about google_unknown_resource" with no way to find which
// of the user's files declares it.
//
// Two unknown blocks of the SAME Terraform type at DIFFERENT source
// locations must surface as two distinct UnknownResource entries —
// otherwise a configuration with the same uncovered type used in
// several places would under-report.
func TestResolveUnknownResourceCapturesSourceLocation(t *testing.T) {
	cat := &catalog.Catalog{
		Resources:   map[string]*catalog.ResourceEntry{},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}

	res := Resolve([]parser.Resource{
		{Kind: "resource", Type: "google_unknown", Name: "a", File: "main.tf", Line: 10},
		{Kind: "resource", Type: "google_unknown", Name: "b", File: "main.tf", Line: 20},
		{Kind: "resource", Type: "google_unknown", Name: "c", File: "other.tf", Line: 5},
	}, cat)

	// Sort order is (File, Line, Type, Name): "main.tf" < "other.tf";
	// within "main.tf", line 10 < line 20.
	assertUnknownsEqual(t, res.Unknowns, []UnknownResource{
		{Type: "google_unknown", Name: "a", File: "main.tf", Line: 10},
		{Type: "google_unknown", Name: "b", File: "main.tf", Line: 20},
		{Type: "google_unknown", Name: "c", File: "other.tf", Line: 5},
	})
}

// TestResolveUnresolvedConditionalCapturesSourceLocation pins that the
// resolver propagates parser.Resource.File and parser.Resource.Line
// onto UnresolvedConditional entries. The reporter uses this to quote
// the offending block when warning about unresolved conditionals — Epic
// 5's "surface unresolved conditionals as warnings tied to the resource
// and attribute, with file:line context" story.
//
// Two resources sharing a type/name but declared in different files
// produce distinct entries; the address-only dedup of an earlier
// iteration would have collapsed them and lost the second file's
// warning.
func TestResolveUnresolvedConditionalCapturesSourceLocation(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	unresolvedAttrs := map[string]cty.Value{"uniform_bucket_level_access": cty.NilVal}

	res := Resolve([]parser.Resource{
		{
			Kind:  "resource",
			Type:  "google_storage_bucket",
			Name:  "primary",
			File:  "buckets.tf",
			Line:  3,
			Attrs: unresolvedAttrs,
		},
		{
			Kind:  "resource",
			Type:  "google_storage_bucket",
			Name:  "primary",
			File:  "other.tf",
			Line:  17,
			Attrs: unresolvedAttrs,
		},
	}, cat)

	// Sort order is (File, Line, ResourceType, Attribute): "buckets.tf"
	// before "other.tf".
	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "primary",
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonOther,
			File:         "buckets.tf",
			Line:         3,
		},
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "primary",
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonOther,
			File:         "other.tf",
			Line:         17,
		},
	})
}

// TestResolveUnresolvedReasonPropagatedFromParser pins the wire-up
// between parser.Resource.AttrReasons and
// UnresolvedConditional.Reason. The parser classifies an unresolved
// expression's failure mode (function_call, data_source,
// missing_variable, other); the resolver must surface that
// classification verbatim on every UnresolvedConditional it emits,
// not collapse it to ReasonOther.
//
// This test seeds three resources with different unresolved-attribute
// reasons and asserts the Reason field flows through as-is. Without
// this assertion a regression that drops the AttrReasons lookup in
// unresolvedReasonFor would silently emit ReasonOther for every
// entry, which the previous "every assertion uses ReasonOther"
// resolver tests would never catch.
func TestResolveUnresolvedReasonPropagatedFromParser(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	mkResource := func(name, reason string) parser.Resource {
		return parser.Resource{
			Kind: "resource",
			Type: "google_storage_bucket",
			Name: name,
			File: "main.tf",
			Line: 1,
			Attrs: map[string]cty.Value{
				"uniform_bucket_level_access": cty.NilVal,
			},
			AttrReasons: map[string]string{
				"uniform_bucket_level_access": reason,
			},
		}
	}

	res := Resolve([]parser.Resource{
		mkResource("a_func", parser.ReasonFunctionCall),
		mkResource("b_data", parser.ReasonDataSource),
		mkResource("c_var", parser.ReasonMissingVariable),
	}, cat)

	// Sort tier here is (File, Line, ResourceType, Attribute,
	// ResourceName). All three rows tie on the first four fields, so
	// ResourceName is the final discriminator: a_func < b_data < c_var.
	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "a_func",
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonFunctionCall,
			File:         "main.tf",
			Line:         1,
		},
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "b_data",
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonDataSource,
			File:         "main.tf",
			Line:         1,
		},
		{
			ResourceType: "google_storage_bucket",
			ResourceName: "c_var",
			Attribute:    "uniform_bucket_level_access",
			Reason:       parser.ReasonMissingVariable,
			File:         "main.tf",
			Line:         1,
		},
	})
}

// TestResolveUnresolvedReasonFallback pins the unresolvedReasonFor
// fallback contract: when the parser did not record a reason for an
// attribute (the attribute is absent from the resource entirely
// because the catalog's when: predicate names something the user
// never wrote), Reason falls back to parser.ReasonOther rather than
// the empty string. The fallback keeps the JSON shape always
// populated so downstream consumers do not have to handle a
// distinguishing-empty-string case.
func TestResolveUnresolvedReasonFallback(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: map[string]any{"uniform_bucket_level_access": true},
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind:  "resource",
		Type:  "google_storage_bucket",
		Name:  "primary",
		Attrs: map[string]cty.Value{
			// Intentionally omit uniform_bucket_level_access entirely:
			// matchesConditional will treat it as missing, but
			// AttrReasons has no entry to source from.
		},
		AttrReasons: map[string]string{},
	}}, cat)

	assertUnresolvedEqual(t, res.Unresolved, []UnresolvedConditional{{
		ResourceType: "google_storage_bucket",
		ResourceName: "primary",
		Attribute:    "uniform_bucket_level_access",
		Reason:       parser.ReasonOther,
	}})
}

// TestResolveResourcesPopulatedBaseOnly verifies the simplest path
// through the per-resource attribution layer: a catalog hit with no
// firing conditionals produces a single ResourceResult carrying the
// effective base permission set (Plan ∪ Create ∪ Update ∪ Delete with
// prevent_destroy filtering, sorted) and an empty (non-nil) Applied
// slice. Pins ResourceResult identity-field copying and the
// "no-conditionals → empty []" JSON shape contract.
func TestResolveResourcesPopulatedBaseOnly(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		File: "main.tf",
		Line: 7,
	}}, cat)

	if len(res.Resources) != 1 {
		t.Fatalf("Resources length: got %d, want 1; full=%#v", len(res.Resources), res.Resources)
	}
	got := res.Resources[0]
	want := ResourceResult{
		Type:          "google_storage_bucket",
		Name:          "primary",
		File:          "main.tf",
		Line:          7,
		BasePlan:      []string{"storage.buckets.get"},
		BaseApplyOnly: []string{"storage.buckets.create", "storage.buckets.delete", "storage.buckets.update"},
		Applied:       []AppliedConditional{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resources[0] mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestResolveResourcesPopulatedConditionalFires verifies that a
// resource whose `when:` predicate matches surfaces an
// AppliedConditional carrying the catalog's literal When map and the
// per-stage Plan / ApplyOnly contribution the conditional made. The
// per-stage split mirrors BasePlan / BaseApplyOnly so the by-resource
// reporter can render plan vs apply contributions per firing
// conditional without re-running the resolver.
func TestResolveResourcesPopulatedConditionalFires(t *testing.T) {
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
		File: "main.tf",
		Line: 12,
		Attrs: map[string]cty.Value{
			"uniform_bucket_level_access": cty.True,
		},
	}}, cat)

	if len(res.Resources) != 1 {
		t.Fatalf("Resources length: got %d, want 1; full=%#v", len(res.Resources), res.Resources)
	}
	got := res.Resources[0]
	want := ResourceResult{
		Type:          "google_storage_bucket",
		Name:          "primary",
		File:          "main.tf",
		Line:          12,
		BasePlan:      []string{"storage.buckets.get"},
		BaseApplyOnly: []string{"storage.buckets.create", "storage.buckets.delete", "storage.buckets.update"},
		Applied: []AppliedConditional{
			{
				When:      map[string]any{"uniform_bucket_level_access": true},
				Plan:      []string{"storage.buckets.getIamPolicy"},
				ApplyOnly: []string{"storage.buckets.setIamPolicy"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resources[0] mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestResolveResourcesPreventDestroyFiltersDelete verifies that a
// resource carrying `lifecycle { prevent_destroy = true }` does not
// include its Delete permissions in BaseApplyOnly — matching the
// global PlanPerms / TotalApplyPerms semantics. The per-resource
// attribution view must agree with the global sets, otherwise the
// by-resource reporter would misrepresent what `terraform apply`
// actually needs.
func TestResolveResourcesPreventDestroyFiltersDelete(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	res := Resolve([]parser.Resource{{
		Kind:           "resource",
		Type:           "google_storage_bucket",
		Name:           "primary",
		File:           "main.tf",
		Line:           4,
		PreventDestroy: true,
	}}, cat)

	if len(res.Resources) != 1 {
		t.Fatalf("Resources length: got %d, want 1; full=%#v", len(res.Resources), res.Resources)
	}
	wantPlan := []string{"storage.buckets.get"}
	wantApplyOnly := []string{"storage.buckets.create", "storage.buckets.update"}
	if !reflect.DeepEqual(res.Resources[0].BasePlan, wantPlan) {
		t.Errorf("BasePlan mismatch\n got: %#v\nwant: %#v", res.Resources[0].BasePlan, wantPlan)
	}
	if !reflect.DeepEqual(res.Resources[0].BaseApplyOnly, wantApplyOnly) {
		t.Errorf("BaseApplyOnly mismatch\n got: %#v\nwant: %#v", res.Resources[0].BaseApplyOnly, wantApplyOnly)
	}
}

// TestResolveResourcesUnknownExcluded verifies that resources whose
// type is not in the catalog do NOT contribute to Resources — they
// flow through Unknowns instead. A ResourceResult with no catalog
// entry has no permissions to attribute.
func TestResolveResourcesUnknownExcluded(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_unknown_thing",
		Name: "primary",
		File: "main.tf",
		Line: 9,
	}}, cat)

	if len(res.Resources) != 0 {
		t.Errorf("Resources should be empty for unknown type; got %#v", res.Resources)
	}
	if len(res.Unknowns) != 1 {
		t.Errorf("Unknowns should contain the unknown resource; got %#v", res.Unknowns)
	}
}

// TestResolveResourcesDisambiguatesReusedModuleInstantiations pins
// the same disambiguation contract UnresolvedConditional carries: two
// `module "x" { source = "./mod" }` and `module "y" { source = "./mod" }`
// instantiations of the same shared module produce two
// ResourceResult rows with identical (Type, Name, File, Line) tuples
// distinguished only by ModulePath. Without ModulePath participating
// in the result identity, the reporter's by-resource format would
// silently collapse two distinct call sites into a single row.
func TestResolveResourcesDisambiguatesReusedModuleInstantiations(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	res := Resolve([]parser.Resource{
		{
			Kind:       "resource",
			Type:       "google_storage_bucket",
			Name:       "primary",
			File:       "/abs/mod/main.tf",
			Line:       3,
			ModulePath: []string{"x"},
		},
		{
			Kind:       "resource",
			Type:       "google_storage_bucket",
			Name:       "primary",
			File:       "/abs/mod/main.tf",
			Line:       3,
			ModulePath: []string{"y"},
		},
	}, cat)

	if len(res.Resources) != 2 {
		t.Fatalf("Resources length: got %d, want 2 (one per module instantiation); full=%#v",
			len(res.Resources), res.Resources)
	}
	wantPaths := [][]string{{"x"}, {"y"}}
	got0, got1 := res.Resources[0].ModulePath, res.Resources[1].ModulePath
	// Resolve emits in source order; both pre-Canonicalize and
	// post-Canonicalize orderings yield ["x"] before ["y"] (lexicographic
	// segment compare), so we can assert the order directly.
	if !reflect.DeepEqual(got0, wantPaths[0]) || !reflect.DeepEqual(got1, wantPaths[1]) {
		t.Errorf("ModulePath sequence mismatch\n got: [%v %v]\nwant: %v",
			got0, got1, wantPaths)
	}
}

// TestResolveResourcesClonesModulePath pins that ResourceResult
// shares no backing array with the parser's Resource.ModulePath, so
// downstream mutation cannot reach back into the parser's cached
// state. cloneModulePath is the barrier; this test fails if Resolve
// ever drops it for the per-resource attribution layer.
func TestResolveResourcesClonesModulePath(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	parserPath := []string{"original_module"}
	res := Resolve([]parser.Resource{{
		Kind:       "resource",
		Type:       "google_storage_bucket",
		Name:       "primary",
		File:       "main.tf",
		Line:       3,
		ModulePath: parserPath,
	}}, cat)

	if len(res.Resources) != 1 {
		t.Fatalf("Resources length: got %d, want 1", len(res.Resources))
	}
	if &res.Resources[0].ModulePath[0] == &parserPath[0] {
		t.Errorf("ResourceResult.ModulePath shares backing array with parser path; cloneModulePath dropped")
	}
	res.Resources[0].ModulePath[0] = "mutated"
	if parserPath[0] != "original_module" {
		t.Errorf("mutating ResourceResult.ModulePath leaked into parser slice; got %q, want %q",
			parserPath[0], "original_module")
	}
}

// TestResolveResourcesClonesWhen pins that AppliedConditional.When
// shares no backing map with the catalog's Conditional.When, so
// downstream mutation cannot reach back into the catalog's loaded
// storage. cloneWhen is the barrier; this test fails if Resolve ever
// drops it.
func TestResolveResourcesClonesWhen(t *testing.T) {
	catalogWhen := map[string]any{"uniform_bucket_level_access": true}
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{{
		When: catalogWhen,
		Permissions: catalog.PermissionSet{
			Plan: []string{"storage.buckets.getIamPolicy"},
		},
	}})

	res := Resolve([]parser.Resource{{
		Kind: "resource",
		Type: "google_storage_bucket",
		Name: "primary",
		File: "main.tf",
		Line: 3,
		Attrs: map[string]cty.Value{
			"uniform_bucket_level_access": cty.True,
		},
	}}, cat)

	if len(res.Resources) != 1 || len(res.Resources[0].Applied) != 1 {
		t.Fatalf("expected one Resources entry with one Applied conditional; got %#v", res.Resources)
	}
	res.Resources[0].Applied[0].When["uniform_bucket_level_access"] = false
	if catalogWhen["uniform_bucket_level_access"] != true {
		t.Errorf("mutating AppliedConditional.When leaked into catalog map; got %v, want true",
			catalogWhen["uniform_bucket_level_access"])
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

// TestResolveUnknownResourceCapturesModulePath pins that the
// resolver propagates parser.Resource.ModulePath onto UnknownResource
// entries. Reused modules often have unknown resources; this keeps
// them distinct in the output.
func TestResolveUnknownResourceCapturesModulePath(t *testing.T) {
	cat := &catalog.Catalog{
		Resources:   map[string]*catalog.ResourceEntry{},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}

	res := Resolve([]parser.Resource{
		{
			Kind:       "resource",
			Type:       "google_unknown",
			Name:       "x",
			ModulePath: []string{"a", "b"},
			File:       "mod/main.tf",
			Line:       10,
		},
	}, cat)

	assertUnknownsEqual(t, res.Unknowns, []UnknownResource{
		{
			Type:       "google_unknown",
			Name:       "x",
			ModulePath: []string{"a", "b"},
			File:       "mod/main.tf",
			Line:       10,
		},
	})
}
