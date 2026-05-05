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
//   - Unknown resource detection: types that match neither Resources,
//     DataSources, nor IAMBindings surface in Resolution.Unknowns.
//
// What this iteration does NOT yet handle (Epic 5 stories tracked
// elsewhere):
//
//   - Conditional permission application based on attribute matching
//     against Conditional.When clauses. Conditionals are silently
//     skipped, so any conditional permissions stay out of Plan / Apply
//     until the full implementation lands.
//   - Surfacing unresolved conditionals (gating attributes that did not
//     resolve to a literal) as warnings tied to the resource and
//     attribute. Resolution.Unresolved is always an empty slice today.
//   - --include-delete / --exclude-delete flag plumbing — Delete is
//     unconditionally included unless prevent_destroy fires.
//
// When Epic 5's full feature set lands here, the per-resource catalog
// goldens at internal/catalog/testdata/<service>/<type>/expected.json
// must be regenerated via `go test ./internal/catalog -update`.
package resolver

import (
	"sort"

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
//   - Unresolved currently always has length zero. Future shape: one
//     entry per conditional that could not be evaluated, identified
//     by resource type, attribute name, and source location.
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
		Unresolved: []string{},
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
