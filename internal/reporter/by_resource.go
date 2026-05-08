package reporter

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

// RenderByResource writes the by-resource grouped representation of
// res to w. This is the format Journey 4 of docs/tfperms_pdr.md
// describes — a user investigating an unexpected permission reruns
// `tfperms --by-resource` to see which resource block contributed it.
//
// resourceCount is defined identically to Render's parameter — every
// distinct Terraform resource block the parser observed, with each
// module-instance copy and data source counted once.
//
// Output layout:
//
//	42 permissions for 17 resources, 2 unknowns, 3 unresolved conditionals
//
//	google_storage_bucket (2 instances):
//	    google_storage_bucket.primary (main.tf:10)
//	    google_storage_bucket.lookup (main.tf:16)
//	  plan permissions (2):
//	    storage.buckets.get
//	    storage.buckets.getIamPolicy  # from conditional uniform_bucket_level_access=true
//	  apply-only permissions (3):
//	    storage.buckets.create
//	    storage.buckets.delete
//	    storage.buckets.setIamPolicy  # from conditional uniform_bucket_level_access=true
//
//	unknown resources (1):
//	  google_dataplex_lake.primary (main.tf:42)
//
// Indentation is part of the format contract:
//
//   - Summary, group headers, and section headers carry a 2-space leading
//     indent (matching Render).
//   - Resource instance rows under a group header carry a 4-space indent
//     so they read visually as children of the group.
//   - Permission rows under a section header carry a 4-space indent
//     (same as Render) so a `grep -E '^    '` filter still picks them up.
//
// Section visibility: groups, unknowns, unresolved, and warnings are
// each omitted (header, body, and the leading blank line) when their
// underlying slice is empty — the same collapse contract Render
// follows. A fully-empty Result still produces the summary line so
// downstream `diff` consumers always have a stable first line.
//
// Permissions sourced from a firing conditional (i.e. present in the
// group's conditional contribution but not in the group's base
// contribution) are annotated with `# from conditional <when>` so
// readers can attribute the permission to the catalog rule that
// pulled it in. <when> is the conditional's predicate map rendered
// as `key=value` pairs joined by `,` in sorted-key order.
//
// All writes flow through the same errWriter Render uses, so a broken
// stdout pipe surfaces as a non-nil return rather than silent
// truncation.
func RenderByResource(w io.Writer, res resolver.Result, resourceCount int) error {
	res = Canonicalize(res)
	ew := &errWriter{w: w}

	fmt.Fprintf(ew,
		"  %d %s for %d %s, %d %s, %d %s\n",
		len(res.TotalApplyPerms),
		plural(len(res.TotalApplyPerms), "permission", "permissions"),
		resourceCount,
		plural(resourceCount, "resource", "resources"),
		len(res.Unknowns),
		plural(len(res.Unknowns), "unknown", "unknowns"),
		len(res.Unresolved),
		plural(len(res.Unresolved), "unresolved conditional", "unresolved conditionals"),
	)

	groups := groupByType(res.Resources)
	for _, g := range groups {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "  %s (%d %s):\n",
			g.Type, len(g.Instances), plural(len(g.Instances), "instance", "instances"))
		for _, inst := range g.Instances {
			fmt.Fprintf(ew, "    %s%s.%s (%s:%d)\n",
				modulePrefix(inst.ModulePath), inst.Type, inst.Name, inst.File, inst.Line)
		}

		if len(g.PlanPerms) > 0 {
			fmt.Fprintf(ew, "  plan permissions (%d):\n", len(g.PlanPerms))
			for _, p := range g.PlanPerms {
				if origin := g.Origins[planKey(p)]; origin != "" {
					fmt.Fprintf(ew, "    %s  # from conditional %s\n", p, origin)
					continue
				}
				fmt.Fprintf(ew, "    %s\n", p)
			}
		}

		if len(g.ApplyOnlyPerms) > 0 {
			fmt.Fprintf(ew, "  apply-only permissions (%d):\n", len(g.ApplyOnlyPerms))
			for _, p := range g.ApplyOnlyPerms {
				if origin := g.Origins[applyOnlyKey(p)]; origin != "" {
					fmt.Fprintf(ew, "    %s  # from conditional %s\n", p, origin)
					continue
				}
				fmt.Fprintf(ew, "    %s\n", p)
			}
		}
	}

	if len(res.Diagnostics) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "  warnings (%d):\n", len(res.Diagnostics))
		for _, d := range res.Diagnostics {
			fmt.Fprintf(ew, "    %s (%s:%d)\n", d.Summary, d.File, d.Line)
		}
	}

	if len(res.Unknowns) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "  unknown resources (%d):\n", len(res.Unknowns))
		for _, u := range res.Unknowns {
			fmt.Fprintf(ew, "    %s%s.%s (%s:%d)\n",
				modulePrefix(u.ModulePath), u.Type, u.Name, u.File, u.Line)
		}
	}

	if len(res.Unresolved) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "  unresolved conditionals (%d):\n", len(res.Unresolved))
		for _, u := range res.Unresolved {
			fmt.Fprintf(ew, "    %s%s.%s: %s (%s:%d) — %s\n",
				modulePrefix(u.ModulePath), u.ResourceType, u.ResourceName, u.Attribute, u.File, u.Line, u.Reason)
		}
	}

	if ew.err != nil {
		return fmt.Errorf("write report: %w", ew.err)
	}
	return nil
}

// resourceGroup carries everything RenderByResource needs to render a
// single Type's section: the list of resource instances (sorted by
// the same (File, Line, Name, ModulePath) tuple Canonicalize uses for
// the global Resources slice), the unioned per-stage permission lists,
// and an Origins map keyed by planKey/applyOnlyKey markers identifying
// permissions sourced from a firing conditional rather than the base
// entry.
type resourceGroup struct {
	Type           string
	Instances      []resolver.ResourceResult
	PlanPerms      []string
	ApplyOnlyPerms []string
	// Origins maps planKey(perm) / applyOnlyKey(perm) to a serialised
	// representation of the conditional's When map (e.g.
	// "uniform_bucket_level_access=true"). A permission absent from
	// the map is base-only — no annotation is emitted for it.
	Origins map[string]string
}

// groupByType groups every resource in resources by its Terraform
// Type, unions per-stage permissions across instances within a group,
// and computes the conditional-origin map for the by-resource
// reporter's `# from conditional` annotations. Returns groups sorted
// alphabetically by Type — the only deterministic order across map
// iteration that does not depend on input order. Within each group,
// instances retain Canonicalize's (File, Line, Name, ModulePath) sort
// because resources arrives already canonicalised.
//
// Permission attribution rules per group:
//
//   - A permission contributed by ANY instance's BasePlan / BaseApplyOnly
//     is treated as base-derived for the whole group: a permission is
//     "base" if at least one instance has it in its base contribution.
//   - A permission ONLY contributed by firing conditionals (never in any
//     base) is annotated with `# from conditional <when>` using the
//     first conditional whose contribution included it (in the same
//     order Canonicalize produces). "First wins" because emitting one
//     annotation keeps each row scannable; users investigating an
//     unexpected permission can drop into the source if multiple
//     conditionals fired and they need the full origin trail.
//
// The plan and apply-only origin maps share a single Origins map keyed
// via planKey() / applyOnlyKey() so two permissions with the same
// string but different stages do not collide.
func groupByType(resources []resolver.ResourceResult) []resourceGroup {
	if len(resources) == 0 {
		return nil
	}
	byType := make(map[string]*resourceGroup)
	// Accumulators per group: base-contribution sets keep the
	// "originated from base anywhere in the group" rule, and the
	// per-permission firstConditional map remembers the When string
	// of the earliest firing conditional that contributed each
	// permission (used only for permissions never in base).
	type groupAccumulator struct {
		basePlan        map[string]struct{}
		baseApplyOnly   map[string]struct{}
		condPlanOrigin  map[string]string
		condApplyOrigin map[string]string
		allPlanSet      map[string]struct{}
		allApplyOnlySet map[string]struct{}
	}
	accs := make(map[string]*groupAccumulator)

	for _, r := range resources {
		g, ok := byType[r.Type]
		if !ok {
			g = &resourceGroup{Type: r.Type, Origins: map[string]string{}}
			byType[r.Type] = g
			accs[r.Type] = &groupAccumulator{
				basePlan:        map[string]struct{}{},
				baseApplyOnly:   map[string]struct{}{},
				condPlanOrigin:  map[string]string{},
				condApplyOrigin: map[string]string{},
				allPlanSet:      map[string]struct{}{},
				allApplyOnlySet: map[string]struct{}{},
			}
		}
		g.Instances = append(g.Instances, r)
		acc := accs[r.Type]

		for _, p := range r.BasePlan {
			acc.basePlan[p] = struct{}{}
			acc.allPlanSet[p] = struct{}{}
		}
		for _, p := range r.BaseApplyOnly {
			acc.baseApplyOnly[p] = struct{}{}
			acc.allApplyOnlySet[p] = struct{}{}
		}
		for _, applied := range r.Applied {
			when := serialiseWhen(applied.When)
			for _, p := range applied.Plan {
				acc.allPlanSet[p] = struct{}{}
				if _, seen := acc.condPlanOrigin[p]; !seen {
					acc.condPlanOrigin[p] = when
				}
			}
			for _, p := range applied.ApplyOnly {
				acc.allApplyOnlySet[p] = struct{}{}
				if _, seen := acc.condApplyOrigin[p]; !seen {
					acc.condApplyOrigin[p] = when
				}
			}
		}
	}

	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	out := make([]resourceGroup, 0, len(types))
	for _, t := range types {
		g := byType[t]
		acc := accs[t]
		g.PlanPerms = sortedKeys(acc.allPlanSet)
		g.ApplyOnlyPerms = sortedKeys(acc.allApplyOnlySet)
		// A permission is annotated `# from conditional` only when
		// every instance's base contribution lacks it — i.e. it
		// originates exclusively from a firing conditional.
		for _, p := range g.PlanPerms {
			if _, inBase := acc.basePlan[p]; inBase {
				continue
			}
			if when := acc.condPlanOrigin[p]; when != "" {
				g.Origins[planKey(p)] = when
			}
		}
		for _, p := range g.ApplyOnlyPerms {
			if _, inBase := acc.baseApplyOnly[p]; inBase {
				continue
			}
			if when := acc.condApplyOrigin[p]; when != "" {
				g.Origins[applyOnlyKey(p)] = when
			}
		}
		out = append(out, *g)
	}
	return out
}

// planKey returns the Origins-map key for plan-stage permission p.
// The "plan:" prefix keeps a permission that appears in both stages
// (impossible by construction today, but cheap defence) from
// colliding in the shared Origins map.
func planKey(p string) string { return "plan:" + p }

// applyOnlyKey returns the Origins-map key for apply-only permission p.
// See planKey for the rationale on the prefix.
func applyOnlyKey(p string) string { return "apply:" + p }

// serialiseWhen renders a conditional's When map as `key=value` pairs
// joined by ", " in alphabetical key order. The serialisation is
// identical to the appliedSortKey "key=value" segments minus the NUL
// separators — a human-readable form for the reporter's annotation
// row, kept in sync semantically with the deterministic sort key so a
// reader investigating "from conditional X=true" sees exactly the
// catalog predicate the resolver matched.
//
// Empty / nil maps render as the empty string. The callers only
// invoke serialiseWhen on AppliedConditionals (which the catalog
// validator guarantees have non-empty When maps), but the empty-map
// path is defensive.
func serialiseWhen(when map[string]any) string {
	if len(when) == 0 {
		return ""
	}
	keys := make([]string, 0, len(when))
	for k := range when {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", when[k])
	}
	return b.String()
}

// sortedKeys returns set's keys in lexicographic order. Local helper
// for groupByType — the reporter package's sortedStrings only takes
// a []string, and converting a set first would force an extra copy.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
