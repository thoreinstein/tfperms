// Package resolver combines parsed Terraform resources with the
// permission catalog to produce IAM permission sets a service account
// needs to run `terraform plan` and `terraform apply` against the
// configuration.
//
// This file establishes the API contract documented in Epic 5 of
// docs/tfperms_pdr.md so callers — initially the per-resource catalog
// regression tests in internal/catalog — can compile and exercise the
// pipeline end-to-end before Epic 5's full implementation lands.
//
// What this iteration handles:
//
//   - Type → catalog lookup for `resource`, `data`, and IAM-binding
//     blocks. Resources are checked against Catalog.Resources first,
//     then Catalog.IAMBindings, since the IAM binding types
//     (google_*_iam_{binding,member,policy}) are also `resource` blocks
//     in HCL.
//   - Three-set permission partition. Per Epic 5 of the PDR:
//     `plan_perms` are the permissions needed for `terraform plan`'s
//     state refresh (the entry's Plan list, plus any plan-stage entries
//     from fired conditionals). `apply_only_perms` are the permissions
//     needed for `terraform apply` *that are not also in plan_perms* —
//     computed as `(Create ∪ Update ∪ Delete) \ plan_perms`. A `.get`
//     permission needed both for refresh and for the update API call
//     therefore appears only in `plan_perms` and `total_apply_perms`,
//     not in `apply_only_perms`. `total_apply_perms` is the union of
//     the two — what an SA running `terraform apply` actually needs.
//   - Honoring `lifecycle { prevent_destroy = true }` as a literal: a
//     resource carrying that flag does not contribute Delete
//     permissions to the apply sets.
//   - Conditional permission application based on attribute matching
//     against Conditional.When clauses. For each catalog conditional
//     on the matched entry, the resolver compares the resolved
//     attribute value (parser.Resource.Attrs) against the YAML literal
//     in `when:` and unions the conditional's permissions when every
//     predicate matches. AND semantics across predicates: a single
//     non-matching predicate disables the whole conditional. The
//     comparison covers the YAML scalars catalog conditions actually
//     use today — bool, string, and number — see ctyValueEqualsLiteral.
//   - Surfacing unresolved conditionals: when a gating attribute is
//     present in the When clause but did not resolve to a literal in
//     the parser (cty.NilVal) AND no other predicate definitively
//     fails, the resolver emits an UnresolvedConditional carrying the
//     resource Type/Name, the ModulePath chain (so reused-module
//     instantiations remain distinct), the gating Attribute name,
//     the parser's Reason classification (function_call, data_source,
//     missing_variable, other), and the source File:Line so the
//     reporter can quote the offending block.
//   - Unknown resource detection: types that match neither Resources,
//     DataSources, nor IAMBindings surface as UnknownResource entries
//     carrying the Terraform Type and the source File:Line.
//
// What this iteration does NOT yet handle (Epic 5 stories tracked
// elsewhere):
//
//   - --include-delete / --exclude-delete flag plumbing — Delete is
//     unconditionally included unless prevent_destroy fires.
//
// When Epic 5's full feature set lands here, the per-resource catalog
// goldens at internal/catalog/testdata/<service>/<type>/expected.json
// must be regenerated via `go test ./internal/catalog -update`.
package resolver

import (
	"math/big"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/thoreinstein/tfperms/internal/catalog"
	"github.com/thoreinstein/tfperms/internal/parser"
)

// Result is the output of permission resolution. It carries three
// distinct permission sets per Epic 5 of docs/tfperms_pdr.md:
//
//   - PlanPerms: permissions a service account needs to run
//     `terraform plan` — dominated by `.get` permissions used during
//     state refresh, plus the read permissions on data sources.
//   - ApplyOnlyPerms: permissions needed only at apply time and not
//     already in PlanPerms — typically `.create` / `.update` /
//     `.delete`. Computed as `(Create ∪ Update ∪ Delete) \ PlanPerms`,
//     so a `.get` permission listed under both Plan and Update on a
//     catalog entry surfaces in PlanPerms only, not here.
//   - TotalApplyPerms: PlanPerms ∪ ApplyOnlyPerms — what a service
//     account running `terraform apply` actually needs (apply has to
//     refresh first, so it inherits the plan permissions).
//
// Diagnostic fields:
//
//   - Unknowns: Terraform resource types observed in the input that
//     were not present in any of the catalog's three sections, with
//     the source File:Line so the reporter can point the user at the
//     offending block.
//   - Unresolved: conditional gating attributes that could not be
//     statically evaluated against a parsed resource. Each entry
//     carries the resource Type/Name, the ModulePath chain (so
//     reused-module instantiations remain distinct), the unresolved
//     Attribute name, a stable Reason classification (function_call,
//     data_source, missing_variable, other), and the source File:Line.
//
// Field invariants:
//
//   - Every slice field is non-nil on a Result returned by Resolve.
//     Empty results render as `[]` in JSON rather than `null`, so
//     golden files stay shape-stable across runs and downstream
//     consumers (the reporter, future JSON output formats) never have
//     to nil-check.
//   - PlanPerms, ApplyOnlyPerms, and TotalApplyPerms are sorted
//     ascending and deduplicated. Order is a contract: the reporter
//     pins flat-list output to the resolver's order, and the
//     per-resource catalog tests compare byte-by-byte against goldens.
//   - Unknowns are sorted by (File, Line, Type) and Unresolved by
//     (File, Line, ResourceType, Attribute, ResourceName, ModulePath)
//     so multiple unknowns or unresolveds in the same configuration
//     produce stable golden output even when reused modules collapse
//     all the leading tiers to ties.
type Result struct {
	PlanPerms       []string                `json:"plan_perms"`
	ApplyOnlyPerms  []string                `json:"apply_only_perms"`
	TotalApplyPerms []string                `json:"total_apply_perms"`
	Unknowns        []UnknownResource       `json:"unknowns"`
	Unresolved      []UnresolvedConditional `json:"unresolved"`
}

// UnknownResource describes a single Terraform resource or data block
// whose type is absent from the merged catalog. The reporter surfaces
// these so users can see which resources tfperms could not analyse and
// where they live in the configuration.
//
// Type is the Terraform type (e.g. `google_iam_policy`). File and Line
// come from the parser's Resource.File / Resource.Line — for
// LoadRecursive callers this is an absolute path; the catalog
// regression harness relativises it before comparing to goldens.
type UnknownResource struct {
	Type string `json:"type"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// UnresolvedConditional describes a single conditional gating attribute
// that the resolver could not evaluate against a parsed resource. The
// reporter uses this to warn the user that the permission set may be
// incomplete because a `var.X` or function call hid an attribute the
// catalog's `when:` clause depends on.
//
// ResourceType / ResourceName are the Terraform `<type>` and `<name>`
// labels of the offending block (e.g. `google_storage_bucket` /
// `primary`).
//
// ModulePath is the chain of module call names from the root
// configuration down to the resource — the same value the parser
// records on Resource.ModulePath. Root-level resources have a
// nil/empty ModulePath. The field is preserved through to the JSON
// output (as `module_path`) precisely because it is what
// distinguishes two resources that LoadRecursive instantiated from
// the same module source at different call sites: their File and
// Line are identical (they both point at the shared module's source
// file), so without ModulePath the dedup map would silently collapse
// them into a single row and the reporter would under-report.
//
// Attribute is the bare attribute name from the catalog's `when:`
// clause; not prefixed with the resource type/name.
//
// Reason is the classification string from the parser's AttrReasons
// map for the offending attribute — one of parser.ReasonFunctionCall,
// parser.ReasonDataSource, parser.ReasonMissingVariable, or
// parser.ReasonOther. It tells the user *why* the attribute was
// unresolved, so a missing variable (user-fixable by adding a default)
// is distinguishable from a function call (catalog-side issue) at a
// glance. When the parser did not record a reason for the attribute
// (e.g. the attribute is absent from the resource entirely rather
// than unresolved), Reason falls back to parser.ReasonOther so the
// JSON shape is always populated.
//
// File and Line locate the resource block in the source. As with
// UnknownResource, this is whatever the parser recorded, so the catalog
// regression harness relativises it before comparing against goldens.
type UnresolvedConditional struct {
	ResourceType string   `json:"resource_type"`
	ResourceName string   `json:"resource_name"`
	ModulePath   []string `json:"module_path"`
	Attribute    string   `json:"attribute"`
	Reason       string   `json:"reason"`
	File         string   `json:"file"`
	Line         int      `json:"line"`
}

// Resolve combines the parsed resource set with the merged catalog and
// returns a Result. See the package doc and Result's doc for the
// permission-set semantics.
//
// A nil catalog is treated as "no catalog loaded": every resource type
// surfaces as an Unknown and the permission sets are empty. This
// avoids a panic on early-startup callers that have a partial pipeline;
// production callers should always pass a catalog produced by
// catalog.Load.
//
// Resolve is pure — it does not mutate either input. The returned slices
// are freshly allocated so a caller can sort or filter them in place
// without affecting subsequent calls.
func Resolve(resources []parser.Resource, cat *catalog.Catalog) Result {
	plan := make(map[string]struct{})
	apply := make(map[string]struct{})
	unknowns := make(map[unknownKey]struct{})
	unresolved := make(map[unresolvedRecordKey]unresolvedRecordValue)

	for _, r := range resources {
		switch r.Kind {
		case "resource":
			if entry := lookupResource(cat, r.Type); entry != nil {
				addAll(plan, entry.Permissions.Plan)
				addAll(apply, entry.Permissions.Create)
				addAll(apply, entry.Permissions.Update)
				if !r.PreventDestroy {
					addAll(apply, entry.Permissions.Delete)
				}
				for _, cond := range entry.Conditionals {
					matched, missing := matchesConditional(r.Attrs, cond.When)
					if matched {
						addAll(plan, cond.Permissions.Plan)
						addAll(apply, cond.Permissions.Create)
						addAll(apply, cond.Permissions.Update)
						if !r.PreventDestroy {
							addAll(apply, cond.Permissions.Delete)
						}
					}
					for _, attr := range missing {
						key := unresolvedRecordKey{
							ResourceType:  r.Type,
							ResourceName:  r.Name,
							ModulePathKey: encodeModulePath(r.ModulePath),
							Attribute:     attr,
							File:          r.File,
							Line:          r.Line,
						}
						unresolved[key] = unresolvedRecordValue{
							ModulePath: cloneModulePath(r.ModulePath),
							Reason:     unresolvedReasonFor(r, attr),
						}
					}
				}
				continue
			}
			if entry := lookupIAMBinding(cat, r.Type); entry != nil {
				addAll(plan, entry.Permissions.Plan)
				addAll(apply, entry.Permissions.Create)
				addAll(apply, entry.Permissions.Update)
				if !r.PreventDestroy {
					addAll(apply, entry.Permissions.Delete)
				}
				for _, cond := range entry.Conditionals {
					matched, missing := matchesConditional(r.Attrs, cond.When)
					if matched {
						addAll(plan, cond.Permissions.Plan)
						addAll(apply, cond.Permissions.Create)
						addAll(apply, cond.Permissions.Update)
						if !r.PreventDestroy {
							addAll(apply, cond.Permissions.Delete)
						}
					}
					for _, attr := range missing {
						key := unresolvedRecordKey{
							ResourceType:  r.Type,
							ResourceName:  r.Name,
							ModulePathKey: encodeModulePath(r.ModulePath),
							Attribute:     attr,
							File:          r.File,
							Line:          r.Line,
						}
						unresolved[key] = unresolvedRecordValue{
							ModulePath: cloneModulePath(r.ModulePath),
							Reason:     unresolvedReasonFor(r, attr),
						}
					}
				}
				continue
			}
			unknowns[unknownKey{Type: r.Type, File: r.File, Line: r.Line}] = struct{}{}
		case "data":
			if entry := lookupDataSource(cat, r.Type); entry != nil {
				addAll(plan, entry.Permissions.Plan)
				for _, cond := range entry.Conditionals {
					matched, missing := matchesConditional(r.Attrs, cond.When)
					if matched {
						addAll(plan, cond.Permissions.Plan)
					}
					for _, attr := range missing {
						key := unresolvedRecordKey{
							ResourceType:  r.Type,
							ResourceName:  r.Name,
							ModulePathKey: encodeModulePath(r.ModulePath),
							Attribute:     attr,
							File:          r.File,
							Line:          r.Line,
						}
						unresolved[key] = unresolvedRecordValue{
							ModulePath: cloneModulePath(r.ModulePath),
							Reason:     unresolvedReasonFor(r, attr),
						}
					}
				}
				continue
			}
			unknowns[unknownKey{Type: r.Type, File: r.File, Line: r.Line}] = struct{}{}
		}
		// Unknown Kind (neither "resource" nor "data") is silently
		// ignored. The parser's contract guarantees Kind is one of
		// these two for every Resource it emits, so this branch is
		// defensive only.
	}

	planPerms := sortedSet(plan)
	applyOnlyPerms := subtractSorted(apply, plan)
	totalApplyPerms := sortedUnion(plan, apply)

	return Result{
		PlanPerms:       planPerms,
		ApplyOnlyPerms:  applyOnlyPerms,
		TotalApplyPerms: totalApplyPerms,
		Unknowns:        sortedUnknowns(unknowns),
		Unresolved:      sortedUnresolved(unresolved),
	}
}

// unknownKey is the dedup key for entries surfaced into Result.Unknowns.
// Two unknown blocks with the same type at different file/line
// locations produce two entries — diagnostics that suppress one would
// under-report. Two blocks with literally identical (Type, File, Line)
// cannot coexist in well-formed Terraform, so the collapse is harmless.
type unknownKey struct {
	Type string
	File string
	Line int
}

// unresolvedRecordKey is the dedup key for entries surfaced into
// Result.Unresolved. (ResourceType, ResourceName, ModulePathKey,
// Attribute, File, Line) is the minimum tuple that distinguishes one
// unresolved entry from another within a configuration.
//
// ModulePathKey is required because LoadRecursive instantiates a
// shared module template at every call site without rewriting File or
// Line — those still point at the module's source file. Two call sites
// (`module "x"` and `module "y"`, both pointing at the same `./mod`)
// therefore produce two resources with identical (Type, Name, File,
// Line) tuples that differ only in their ModulePath chain. Without a
// module-path component in the key, the dedup map silently collapses
// them and the resulting JSON under-reports — the regression the
// review caught. The key is the encoded form of the parser's
// Resource.ModulePath (NUL-joined; see encodeModulePath); the slice
// itself is preserved on the unresolvedRecordValue side so the JSON
// output keeps it as `module_path`.
//
// Reason is intentionally NOT part of the key — the parser is
// deterministic, so the same (Type, Name, ModulePath, Attribute, File,
// Line) tuple should not appear with two different reasons in a single
// resolve pass; if it ever did, the second insertion would silently
// overwrite the first which is what we want (consistent classification).
type unresolvedRecordKey struct {
	ResourceType  string
	ResourceName  string
	ModulePathKey string
	Attribute     string
	File          string
	Line          int
}

// unresolvedRecordValue carries the per-row data that is not part of
// the dedup key but still needs to flow through to the rendered
// UnresolvedConditional. ModulePath is the parser's slice form (we
// re-derive the JSON output from it rather than re-splitting
// ModulePathKey, so the API surface stays a clean []string instead of
// inheriting our internal NUL encoding). Reason is the per-attribute
// classification from parser.AttrReasons, falling back to
// parser.ReasonOther via unresolvedReasonFor.
type unresolvedRecordValue struct {
	ModulePath []string
	Reason     string
}

// encodeModulePath joins a Resource.ModulePath chain into a string
// that can serve as a map key. The separator is the ASCII NUL byte
// (`\x00`) — it cannot legally appear in either an HCL identifier or
// a filesystem path on any platform we care about, so two distinct
// ModulePath slices encode to distinct strings without ambiguity.
//
// A nil/empty path encodes to "" so root-level resources (which have
// no module chain) collide with each other in the dedup map but never
// with a nested-module resource, which is the desired behaviour.
//
// We could in principle key the dedup map directly on a hashable
// struct that embedded a fixed-size array, but ModulePath length is
// unbounded; encoding to a string keeps the key type comparable while
// preserving full discrimination.
func encodeModulePath(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return strings.Join(path, "\x00")
}

// cloneModulePath returns a defensive copy of path so the resolver's
// output never shares a backing array with the parser's
// Resource.ModulePath. Without the copy, a downstream caller mutating
// UnresolvedConditional.ModulePath in place could corrupt the parser
// slice that the cache in LoadRecursive shares across module call
// sites. nil in, nil out — preserving the parser's "root resources
// have no ModulePath" convention so test fixtures that omit the field
// remain reflect.DeepEqual-equal to the resolver's output.
func cloneModulePath(path []string) []string {
	if len(path) == 0 {
		return nil
	}
	out := make([]string, len(path))
	copy(out, path)
	return out
}

// lookupResource returns the catalog entry for a `resource` block type,
// or nil if cat is nil or the type is not in cat.Resources.
func lookupResource(cat *catalog.Catalog, typ string) *catalog.ResourceEntry {
	if cat == nil {
		return nil
	}
	return cat.Resources[typ]
}

// lookupDataSource returns the catalog entry for a `data` block type,
// or nil if cat is nil or the type is not in cat.DataSources.
func lookupDataSource(cat *catalog.Catalog, typ string) *catalog.DataSourceEntry {
	if cat == nil {
		return nil
	}
	return cat.DataSources[typ]
}

// lookupIAMBinding returns the catalog entry for an IAM binding block
// type, or nil if cat is nil or the type is not in cat.IAMBindings.
// IAM binding entries are checked after Resources because their HCL
// block kind is also `resource`; a hypothetical type registered under
// both sections would resolve as a Resource, which the validator
// already prevents.
func lookupIAMBinding(cat *catalog.Catalog, typ string) *catalog.IAMBindingEntry {
	if cat == nil {
		return nil
	}
	return cat.IAMBindings[typ]
}

// matchesConditional evaluates a Conditional / DataSourceConditional
// `when:` clause against a resource block's resolved attributes. AND
// semantics: the conditional fires only when every predicate matches.
//
// Returns:
//
//   - matched: true iff every predicate's attribute is present, resolved
//     (not cty.NilVal), and equal to the YAML literal expected value.
//   - missing: the deterministically sorted, deduplicated list of
//     predicate attribute names that could not be evaluated against
//     attrs (the attribute is absent or unresolved). The list is empty
//     when every predicate evaluated to a definitive answer.
//
// Short-circuit on definitive failure: if any predicate evaluates to a
// resolved-but-unequal value, the conditional definitively does not
// fire and missing is returned empty regardless of other predicates'
// status. This keeps Result.Unresolved free of noise from sibling
// predicates whose evaluation would not have changed the outcome.
//
// Iteration order over `when` is randomised by Go's map runtime, so the
// keys are sorted before evaluation; that pins the order in which
// predicates are checked and (more importantly) the order of `missing`
// entries when multiple predicates are unresolved.
func matchesConditional(attrs map[string]cty.Value, when map[string]any) (matched bool, missing []string) {
	keys := make([]string, 0, len(when))
	for k := range when {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	matched = true
	for _, name := range keys {
		actual, ok := attrs[name]
		if !ok || actual == cty.NilVal {
			missing = append(missing, name)
			matched = false
			continue
		}
		if !ctyValueEqualsLiteral(actual, when[name]) {
			// Definitive mismatch — the conditional cannot fire
			// regardless of what the other predicates evaluate to,
			// so swallow any `missing` already accumulated. Reporting
			// "unresolved" for siblings of a definitively-failing
			// predicate would be noise: even if we had resolved them,
			// the conditional would still not have fired.
			return false, nil
		}
	}
	return matched, missing
}

// ctyValueEqualsLiteral compares a resolved cty.Value (from the parser's
// attribute extraction) against a YAML-decoded literal taken from a
// catalog Conditional.When map. Returns true when both sides have
// compatible types and equal values.
//
// yaml.v3 decodes scalars into the canonical Go scalar types:
//
//   - bool   for booleans
//   - string for strings
//   - int    for integers up to platform word size
//   - int64  for integers that overflow int
//   - float64 for floats
//
// Anything outside that set (slice, map, nil) returns false because
// the catalog's `when:` clauses are predicates against scalar
// resource attributes only — the validator rejects empty `when:` maps,
// but tolerates nested shapes that the resolver simply does not
// match. Returning false rather than erroring keeps a malformed
// conditional from torpedoing the rest of the resolution.
func ctyValueEqualsLiteral(actual cty.Value, expected any) bool {
	if actual.IsNull() {
		return expected == nil
	}
	switch e := expected.(type) {
	case bool:
		if !actual.Type().Equals(cty.Bool) {
			return false
		}
		return actual.True() == e
	case string:
		if !actual.Type().Equals(cty.String) {
			return false
		}
		return actual.AsString() == e
	case int:
		return numberEquals(actual, new(big.Float).SetInt64(int64(e)))
	case int64:
		return numberEquals(actual, new(big.Float).SetInt64(e))
	case float64:
		return numberEquals(actual, big.NewFloat(e))
	}
	return false
}

// numberEquals reports whether actual is a cty.Number numerically equal
// to expected. Both sides are *big.Float so integer literals above
// float64's integer-exact range (2^53) are compared without precision
// loss. The catalog's numeric predicates today are all small integers,
// but the routing avoids a footgun if a future entry expresses
// something like `quota_limit: 9007199254740993` (2^53 + 1): the int /
// int64 paths in ctyValueEqualsLiteral construct expected via
// big.Float.SetInt64, which is exact for any int64; the float64 path
// uses big.NewFloat which captures the float64 value bit-exactly. A
// previous iteration of this function cast int / int64 through
// float64 first, which silently rounded large literals — that is the
// regression resolver_test.go's TestResolveConditionalNumberInt64Exact
// pins.
func numberEquals(actual cty.Value, expected *big.Float) bool {
	if !actual.Type().Equals(cty.Number) {
		return false
	}
	return actual.AsBigFloat().Cmp(expected) == 0
}

// addAll inserts every element of items into set as a struct{} membership
// marker. Duplicates collapse naturally — that is the whole point of
// using a set for the per-stage union.
func addAll(set map[string]struct{}, items []string) {
	for _, s := range items {
		set[s] = struct{}{}
	}
}

// sortedSet returns set's keys in lexicographic order as a non-nil slice.
// An empty set yields an empty (length 0, non-nil) slice so JSON
// marshalling renders `[]` rather than `null`; the Result field
// invariants depend on this.
func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// subtractSorted returns the elements of left not present in right,
// sorted ascending. Always returns a non-nil slice so JSON marshals as
// `[]`. Used to compute ApplyOnlyPerms = (Create ∪ Update ∪ Delete) \
// PlanPerms — a permission appearing in both the apply maps and the
// plan map belongs to PlanPerms only, not ApplyOnlyPerms.
func subtractSorted(left, right map[string]struct{}) []string {
	out := make([]string, 0, len(left))
	for k := range left {
		if _, inRight := right[k]; inRight {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedUnion returns the union of left and right as a sorted, non-nil
// slice. Used to compute TotalApplyPerms = PlanPerms ∪ ApplyOnlyPerms
// — what an SA running `terraform apply` actually needs (apply has to
// refresh state first, so it inherits all plan permissions).
func sortedUnion(left, right map[string]struct{}) []string {
	out := make([]string, 0, len(left)+len(right))
	seen := make(map[string]struct{}, len(left)+len(right))
	for _, src := range []map[string]struct{}{left, right} {
		for k := range src {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// sortedUnknowns returns the unknown-resource set as a sorted, non-nil
// slice of UnknownResource. Sort order is (File, Line, Type) so that
// reporters listing unknowns in the order they appear in source files
// can rely on file:line monotonicity, with Type as the tiebreaker for
// the unlikely-but-defensible case of two unknown blocks at the same
// location (would happen only on a malformed parser pass).
func sortedUnknowns(set map[unknownKey]struct{}) []UnknownResource {
	out := make([]UnknownResource, 0, len(set))
	for k := range set {
		// unknownKey and UnknownResource carry the same field set —
		// the internal type stays unexported to keep map-key usage
		// from leaking into the API; convert at the boundary.
		out = append(out, UnknownResource(k))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// sortedUnresolved returns the unresolved-conditional set as a sorted,
// non-nil slice of UnresolvedConditional. Primary sort order is
// (File, Line, ResourceType, Attribute) per the spec; ResourceName is
// appended as a tiebreaker so two unrelated entries that share the
// first four fields (rare in production where File/Line disambiguate,
// common in unit tests with zero-valued File/Line) still produce a
// fully deterministic ordering. ModulePath is the final tiebreaker
// after ResourceName: two reused-module instantiations of the same
// `<type>.<name>` resource land at identical (File, Line, Type,
// Attribute, Name) tuples and only their module-path chain
// distinguishes them, so the chain has to participate in the sort or
// goldens flap across runs. Without the ResourceName / ModulePath
// tiebreakers sort.Slice is non-stable across the tied rows.
func sortedUnresolved(set map[unresolvedRecordKey]unresolvedRecordValue) []UnresolvedConditional {
	out := make([]UnresolvedConditional, 0, len(set))
	for k, v := range set {
		out = append(out, UnresolvedConditional{
			ResourceType: k.ResourceType,
			ResourceName: k.ResourceName,
			ModulePath:   v.ModulePath,
			Attribute:    k.Attribute,
			Reason:       v.Reason,
			File:         k.File,
			Line:         k.Line,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		if out[i].ResourceType != out[j].ResourceType {
			return out[i].ResourceType < out[j].ResourceType
		}
		if out[i].Attribute != out[j].Attribute {
			return out[i].Attribute < out[j].Attribute
		}
		if out[i].ResourceName != out[j].ResourceName {
			return out[i].ResourceName < out[j].ResourceName
		}
		// Final tiebreaker: walk module-path segments lexicographically.
		// strings.Join here is only used as a sort key and never as a
		// dedup key, so the encodeModulePath separator does not need to
		// be reused — the empty-string separator is fine because we
		// never compare across paths of unequal length: a length tier
		// is folded in via the segment-by-segment comparison below.
		return moduleLess(out[i].ModulePath, out[j].ModulePath)
	})
	return out
}

// moduleLess reports whether a sorts before b under a lexicographic
// segment-by-segment comparison, with shorter paths sorting before
// their extensions ([] < [a] < [a, b] < [b]). Centralising the
// comparison keeps sortedUnresolved's Less function readable and
// makes the ordering invariant explicit so future code that reorders
// unresolved entries elsewhere can reuse it.
func moduleLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// unresolvedReasonFor returns the parser's classification reason for
// attr on r, falling back to parser.ReasonOther when the parser did
// not record a reason — which only happens when the attribute is
// absent from the resource entirely (the catalog's when: predicate
// names an attribute the user never wrote). The fallback keeps the
// Reason JSON field always populated so downstream consumers do not
// have to handle an empty-string sentinel.
func unresolvedReasonFor(r parser.Resource, attr string) string {
	if reason, ok := r.AttrReasons[attr]; ok {
		return reason
	}
	return parser.ReasonOther
}
