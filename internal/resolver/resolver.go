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
//   - Plan / Apply permission union from the matched entry's base
//     PermissionSet. Apply is the union of Create, Update, and Delete.
//   - Honoring `lifecycle { prevent_destroy = true }` as a literal: a
//     resource carrying that flag does not contribute Delete
//     permissions to the Apply set.
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
//     fails, the resolver emits a `<type>.<name>: <attribute>` entry
//     into Resolution.Unresolved so the reporter can flag it.
//   - Unknown resource detection: types that match neither Resources,
//     DataSources, nor IAMBindings surface in Resolution.Unknowns.
//
// What this iteration does NOT yet handle (Epic 5 stories tracked
// elsewhere):
//
//   - --include-delete / --exclude-delete flag plumbing — Delete is
//     unconditionally included unless prevent_destroy fires.
//   - Source-location enrichment on Unresolved entries beyond
//     "<type>.<name>: <attribute>". Future shape will carry file:line
//     so the reporter can quote the offending block; the current
//     string form is intentionally simple and stable.
//
// When Epic 5's full feature set lands here, the per-resource catalog
// goldens at internal/catalog/testdata/<service>/<type>/expected.json
// must be regenerated via `go test ./internal/catalog -update`.
package resolver

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/zclconf/go-cty/cty"

	"github.com/thoreinstein/tfperms/internal/catalog"
	"github.com/thoreinstein/tfperms/internal/parser"
)

// Resolution is the output of permission resolution: the IAM permissions
// a service account needs to run `terraform plan` (Plan) and
// `terraform apply` (Apply) against the analysed configuration, plus
// diagnostic lists for resources the catalog does not cover (Unknowns)
// and conditionals whose gating attributes could not be statically
// resolved (Unresolved).
//
// Field invariants:
//
//   - Every field is a non-nil slice on a Resolution returned by
//     Resolve. Empty results render as `[]` in JSON rather than `null`,
//     so golden files stay shape-stable across runs and downstream
//     consumers (the reporter, future JSON output formats) never have
//     to nil-check.
//   - Plan and Apply are sorted ascending and deduplicated. Order is a
//     contract: the reporter pins flat-list output to the resolver's
//     order, and the per-resource catalog tests compare byte-by-byte
//     against goldens.
//   - Unknowns is the deduplicated, sorted set of Terraform resource
//     types observed in the input that were not present in any of the
//     catalog's three sections. The same type appearing on N different
//     blocks contributes one entry.
//   - Unresolved is the deduplicated, sorted set of conditional
//     gating attributes that could not be evaluated against a parsed
//     resource. Each entry is formatted as
//     "<type>.<name>: <attribute>" (e.g.
//     "google_storage_bucket.primary: uniform_bucket_level_access")
//     so the reporter can identify both which resource block and
//     which attribute the resolver could not pin down.
type Resolution struct {
	Plan       []string `json:"plan"`
	Apply      []string `json:"apply"`
	Unknowns   []string `json:"unknowns"`
	Unresolved []string `json:"unresolved"`
}

// Resolve combines the parsed resource set with the merged catalog and
// returns a Resolution. See the package doc for the current scope and
// what is deferred to Epic 5.
//
// A nil catalog is treated as "no catalog loaded": every resource type
// surfaces as an Unknown and Plan / Apply are empty. This avoids a panic
// on early-startup callers that have a partial pipeline; production
// callers should always pass a catalog produced by catalog.Load.
//
// Resolve is pure — it does not mutate either input. The returned slices
// are freshly allocated so a caller can sort or filter them in place
// without affecting subsequent calls.
func Resolve(resources []parser.Resource, cat *catalog.Catalog) Resolution {
	plan := make(map[string]struct{})
	apply := make(map[string]struct{})
	unknowns := make(map[string]struct{})
	unresolved := make(map[string]struct{})

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
						unresolved[fmt.Sprintf("%s.%s: %s", r.Type, r.Name, attr)] = struct{}{}
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
				continue
			}
			unknowns[r.Type] = struct{}{}
		case "data":
			if entry := lookupDataSource(cat, r.Type); entry != nil {
				addAll(plan, entry.Permissions.Plan)
				for _, cond := range entry.Conditionals {
					matched, missing := matchesConditional(r.Attrs, cond.When)
					if matched {
						addAll(plan, cond.Permissions.Plan)
					}
					for _, attr := range missing {
						unresolved[fmt.Sprintf("%s.%s: %s", r.Type, r.Name, attr)] = struct{}{}
					}
				}
				continue
			}
			unknowns[r.Type] = struct{}{}
		}
		// Unknown Kind (neither "resource" nor "data") is silently
		// ignored. The parser's contract guarantees Kind is one of
		// these two for every Resource it emits, so this branch is
		// defensive only.
	}

	return Resolution{
		Plan:       sortedSet(plan),
		Apply:      sortedSet(apply),
		Unknowns:   sortedSet(unknowns),
		Unresolved: sortedSet(unresolved),
	}
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
// status. This keeps Resolution.Unresolved free of noise from sibling
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
		return numberEquals(actual, float64(e))
	case int64:
		return numberEquals(actual, float64(e))
	case float64:
		return numberEquals(actual, e)
	}
	return false
}

// numberEquals reports whether actual is a cty.Number numerically equal
// to expected. Comparison goes through *big.Float so a fractional
// expected value or a large integer expected value (within float64's
// range) does not lose precision in the conversion. The catalog's
// numeric predicates today are all small integers, but routing through
// big.Float removes a footgun if a future entry expresses something
// like `quota_limit: 9007199254740993` (2^53 + 1, beyond float64's
// integer-exact range).
func numberEquals(actual cty.Value, expected float64) bool {
	if !actual.Type().Equals(cty.Number) {
		return false
	}
	expectedBF := new(big.Float).SetFloat64(expected)
	return actual.AsBigFloat().Cmp(expectedBF) == 0
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
// marshalling renders `[]` rather than `null`; the Resolution field
// invariants depend on this.
func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
