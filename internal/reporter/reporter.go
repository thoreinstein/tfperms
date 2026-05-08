// Package reporter renders a resolver.Result into the user-facing text
// formats produced by the tfperms CLI. The flat-list format implemented
// here is the default output of `tfperms <path>` per Epic 6 of
// docs/tfperms_pdr.md and the Sample / Acceptance criteria pinned by
// beads issue tfperms-ftq.1.
//
// The reporter is intentionally a pure presentation layer:
//
//   - It takes a resolver.Result and a resource count and writes bytes
//     to an io.Writer. It does not parse, load the catalog, or compute
//     any permission semantics — those belong to the parser, catalog,
//     and resolver packages, respectively.
//   - It does not transform paths. resolver.Result carries whatever
//     File path the parser produced (absolute when reached via
//     parser.LoadRecursive). The caller is responsible for any
//     relativisation it wants the user to see; keeping the reporter
//     path-agnostic prevents two layers from disagreeing about what the
//     "root" is.
//   - It enforces a deterministic sort independently of the resolver
//     via the Canonicalize pass: every formatter calls Canonicalize on
//     entry, so format-specific sort drift is impossible. This is a
//     defence-in-depth on top of the resolver's own sort contract —
//     two runs against the same input produce byte-identical output for
//     every format, even if a future Resolve change emits Result fields
//     in a different order.
package reporter

import (
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

// Render writes the flat-list representation of res to w.
//
// resourceCount is the number of distinct Terraform resources (counting
// every module-instance copy as its own resource, and including data
// sources) the parser observed. The reporter cannot recover this count
// from a Result alone — Result carries permission sets and diagnostics,
// not resource bodies — so callers pass it explicitly. For pipelines
// rooted at parser.LoadRecursive, len(resources) is the right value.
//
// Output layout (sample with all sections populated):
//
//	42 permissions for 17 resources, 2 unknowns, 3 unresolved conditionals
//
//	plan permissions (3):
//	  bigquery.datasets.get
//	  storage.buckets.get
//	  storage.buckets.list
//
//	apply-only permissions (5):
//	  bigquery.datasets.create
//	  ...
//
//	unknown resources (2):
//	  google_dataplex_lake.primary (main.tf:42)
//	  ...
//
//	unresolved conditionals (3):
//	  google_storage_bucket.data: uniform_bucket_level_access (main.tf:14) — missing_variable
//	  ...
//
// The summary and section headers carry a 2-space leading indent;
// list items inside a section carry a 4-space leading indent. The
// indentation is part of the format contract so consumers can
// `grep -E '^    '` to extract just the rows.
//
// The summary line is always emitted, even on a fully-empty Result, so a
// downstream consumer piping into `diff` always has a stable first line.
//
// Section visibility:
//
//   - plan permissions: omitted when PlanPerms is empty.
//   - apply-only permissions: omitted when ApplyOnlyPerms is empty.
//   - unknown resources: omitted when Unknowns is empty.
//   - unresolved conditionals: omitted when Unresolved is empty.
//
// "Omitted" means no header, no body, no leading blank line — a
// fully-collapsed Result with zero permissions and zero diagnostics
// produces exactly one line of output (the summary). This matches the
// sample in tfperms-ftq.1: empty sections are collapsed entirely so
// readers piping into `grep` or `diff` see signal, not boilerplate.
//
// Counts in the summary line:
//
//   - The leading number is len(TotalApplyPerms) — the total
//     permissions a service account running `terraform apply` actually
//     needs (apply has to refresh first, so it inherits the plan
//     permissions). This equals len(PlanPerms) + len(ApplyOnlyPerms) by
//     construction; either expression is valid. We use TotalApplyPerms
//     directly so a future change to the resolver's set arithmetic
//     cannot silently desynchronise the summary number from the
//     printed sets.
//   - resourceCount is taken from the caller verbatim.
//   - The unknowns and unresolved counts are taken from the slices.
//
// All writes flow through an errWriter that latches the first underlying
// io.Writer error; the same pattern that renderCatalogStats uses in
// cmd/tfperms/catalog.go. The trailing ew.err check turns a broken
// stdout pipe into a non-nil return rather than silent truncation under
// exit code 0 — important for CI consumers that diff the output.
func Render(w io.Writer, res resolver.Result, resourceCount int) error {
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

	if len(res.PlanPerms) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "  plan permissions (%d):\n", len(res.PlanPerms))
		for _, p := range res.PlanPerms {
			fmt.Fprintf(ew, "    %s\n", p)
		}
	}

	if len(res.ApplyOnlyPerms) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "  apply-only permissions (%d):\n", len(res.ApplyOnlyPerms))
		for _, p := range res.ApplyOnlyPerms {
			fmt.Fprintf(ew, "    %s\n", p)
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
			fmt.Fprintf(ew, "    %s%s.%s (%s:%d)\n", modulePrefix(u.ModulePath), u.Type, u.Name, u.File, u.Line)
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

// plural picks singular when n == 1 and plural otherwise. Kept local
// to the reporter because this is the only place we need
// quantity-aware noun selection — a shared helper would invert the
// dependency direction for a five-line function.
func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// modulePrefix renders an UnresolvedConditional.ModulePath as the
// `module.a.module.b.` prefix used elsewhere in the codebase to
// disambiguate reused-module instantiations. An empty path yields the
// empty string so the caller can unconditionally concatenate.
func modulePrefix(path []string) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	for _, name := range path {
		b.WriteString("module.")
		b.WriteString(name)
		b.WriteByte('.')
	}
	return b.String()
}

// Canonicalize returns a new Result with every collection sorted into
// the deterministic order documented per field, leaving the input
// untouched. It is the single source of truth for output sort order
// across every format the reporter produces (flat list, by-resource,
// custom-role YAML, JSON) — formatters call Canonicalize on entry and
// then walk the slices in their natural order, so format-specific
// sort drift is impossible.
//
// Sort orders (matching the resolver's existing contract so calling
// Canonicalize on a Result returned by Resolve is the identity, modulo
// the freshly-allocated copies):
//
//   - PlanPerms / ApplyOnlyPerms / TotalApplyPerms: alphabetical.
//   - Diagnostics: (File, Line, Summary).
//   - Unknowns: (File, Line, Type, Name).
//   - Unresolved: (File, Line, ResourceType, Attribute, ResourceName,
//     ModulePath). The ModulePath comparator uses the same prefix-aware
//     ordering the resolver pins ([] < [a] < [a, b] < [b]).
//   - Resources: (File, Line, Type, Name, ModulePath). Within each
//     ResourceResult, BasePlan and BaseApplyOnly are each sorted
//     alphabetically and Applied is sorted by a deterministic key
//     derived from the When map's sorted-key serialisation, with each
//     AppliedConditional's Plan and ApplyOnly slices also alphabetised.
//
// Idempotency: Canonicalize(Canonicalize(r)) is byte-identical to
// Canonicalize(r). Every nested slice is also a fresh allocation, so a
// downstream caller can mutate the returned Result without touching
// either the input or any prior canonicalisation.
//
// Empty-slice contract: top-level slices (PlanPerms, ApplyOnlyPerms,
// etc.) are always non-nil so JSON marshals them as `[]` rather than
// `null`. A nil input slice canonicalises to a non-nil empty slice;
// this matches resolver.Resolve's field invariants. ModulePath is the
// one exception: it stays nil/empty so the `omitempty` JSON tag on
// UnresolvedConditional and ResourceResult continues to drop the
// field for root-level resources.
func Canonicalize(res resolver.Result) resolver.Result {
	return resolver.Result{
		PlanPerms:       sortedStrings(res.PlanPerms),
		ApplyOnlyPerms:  sortedStrings(res.ApplyOnlyPerms),
		TotalApplyPerms: sortedStrings(res.TotalApplyPerms),
		Diagnostics:     sortedDiagnostics(res.Diagnostics),
		Unknowns:        sortedUnknowns(res.Unknowns),
		Unresolved:      sortedUnresolved(res.Unresolved),
		Resources:       sortedResources(res.Resources),
	}
}

// sortedStrings returns a freshly-allocated alphabetically-sorted
// copy of in. A nil input yields a non-nil empty slice so the
// Canonicalize empty-slice contract holds for every top-level
// permission slice.
func sortedStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// sortedDiagnostics returns a freshly-allocated copy of in sorted by
// (File, Line, Summary). The Diagnostic struct value is copied; it has
// no slice fields, so a shallow copy is sufficient.
func sortedDiagnostics(in []resolver.Diagnostic) []resolver.Diagnostic {
	out := make([]resolver.Diagnostic, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Summary < out[j].Summary
	})
	return out
}

// sortedUnknowns returns a freshly-allocated copy of in sorted by
// (File, Line, Type, Name, ModulePath) — matching the resolver's own
// sortedUnknowns order so calling Canonicalize on a Result from
// Resolve is the identity. ModulePath is the final tiebreaker,
// compared via moduleLess (the same prefix-aware comparator the
// resolver uses: [] < [a] < [a, b] < [b]). The returned slice does
// not share ModulePath backing arrays with the input — each non-nil
// ModulePath is cloned, and nil stays nil to preserve the JSON
// `omitempty` behaviour for root-level resources.
func sortedUnknowns(in []resolver.UnknownResource) []resolver.UnknownResource {
	out := make([]resolver.UnknownResource, len(in))
	copy(out, in)
	for i := range out {
		out[i].ModulePath = cloneStrings(out[i].ModulePath)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return moduleLess(out[i].ModulePath, out[j].ModulePath)
	})
	return out
}

// sortedUnresolved returns a freshly-allocated copy of in sorted by
// (File, Line, ResourceType, Attribute, ResourceName, ModulePath).
// The order matches the resolver's own sortedUnresolved (see
// resolver.go) so calling Canonicalize on a Result from Resolve is
// the identity. ModulePath is compared via moduleLess, the same
// prefix-aware comparator the resolver uses
// ([] < [a] < [a, b] < [b]).
func sortedUnresolved(in []resolver.UnresolvedConditional) []resolver.UnresolvedConditional {
	out := make([]resolver.UnresolvedConditional, len(in))
	copy(out, in)
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
		return moduleLess(out[i].ModulePath, out[j].ModulePath)
	})
	return out
}

// sortedResources returns a freshly-allocated copy of in sorted by
// (File, Line, Type, Name, ModulePath). Each ResourceResult inside
// the slice is itself canonicalised: BasePlan / BaseApplyOnly are
// sorted alphabetically and Applied is sorted by a deterministic key
// derived from the When map's sorted-key serialisation, with each
// AppliedConditional's Plan / ApplyOnly also alphabetised. Slice
// fields are reallocated so the returned tree shares no backing
// arrays with the input — the by-resource reporter format can mutate
// the result freely.
func sortedResources(in []resolver.ResourceResult) []resolver.ResourceResult {
	out := make([]resolver.ResourceResult, len(in))
	for i, r := range in {
		out[i] = resolver.ResourceResult{
			Type:          r.Type,
			Name:          r.Name,
			File:          r.File,
			Line:          r.Line,
			ModulePath:    cloneStrings(r.ModulePath),
			BasePlan:      sortedStrings(r.BasePlan),
			BaseApplyOnly: sortedStrings(r.BaseApplyOnly),
			Applied:       sortedApplied(r.Applied),
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return moduleLess(out[i].ModulePath, out[j].ModulePath)
	})
	return out
}

// sortedApplied returns a freshly-allocated copy of in sorted by the
// deterministic key produced by appliedSortKey — the When map's
// sorted-key serialisation. Within each AppliedConditional, the When
// map is shallow-copied and Plan / ApplyOnly are alphabetised, so
// callers cannot mutate the catalog's storage through the result.
//
// The serialised When key is the only stable order we can produce on
// a slice of conditionals: the catalog does not expose an intrinsic
// identity for a Conditional (no name, no source location threaded
// through), so two conditionals with different When maps are
// distinguishable only by their predicates. A serialisation-based
// key gives us a deterministic ordering that survives the random
// iteration order Go's map runtime imposes on the input.
func sortedApplied(in []resolver.AppliedConditional) []resolver.AppliedConditional {
	out := make([]resolver.AppliedConditional, len(in))
	for i, a := range in {
		out[i] = resolver.AppliedConditional{
			When:      cloneWhen(a.When),
			Plan:      sortedStrings(a.Plan),
			ApplyOnly: sortedStrings(a.ApplyOnly),
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := appliedSortKey(out[i]), appliedSortKey(out[j])
		return ki < kj
	})
	return out
}

// appliedSortKey serialises an AppliedConditional into a string that
// orders the slice deterministically. The serialisation walks the
// When map's keys in sorted order and joins each `key=value` pair
// with NUL separators — the same separator the resolver's
// encodeModulePath uses, chosen because it cannot legally appear in
// either an HCL identifier or a YAML scalar. The Plan and ApplyOnly
// slices are appended after sentinel separators so two
// AppliedConditionals with the same When but different
// (legally-impossible-but-defensive) permission slices still order
// distinctly.
//
// Format: `<k1>=<v1>\x00<k2>=<v2>\x00...\x00\x00<plan1>\x00...\x00\x00<applyOnly1>\x00...`
//
// Values are formatted via fmt.Sprintf("%v") which renders bool /
// string / int / float64 unambiguously — the catalog's only legal
// scalar types per ctyValueEqualsLiteral.
func appliedSortKey(a resolver.AppliedConditional) string {
	var b strings.Builder
	keys := make([]string, 0, len(a.When))
	for k := range a.When {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", a.When[k])
		b.WriteByte('\x00')
	}
	// Sentinel separator: a doubled NUL after the When section so the
	// permission bytes cannot collide with a hypothetical When value
	// that ends in `=`.
	b.WriteByte('\x00')
	for _, p := range a.Plan {
		b.WriteString(p)
		b.WriteByte('\x00')
	}
	// A second doubled NUL separates the Plan slice from ApplyOnly so
	// two conditionals whose Plan and ApplyOnly slices concatenate to
	// the same byte sequence still order distinctly. Without this
	// boundary, `Plan=["ab"], ApplyOnly=["c"]` would tie with
	// `Plan=["a"], ApplyOnly=["bc"]`.
	b.WriteByte('\x00')
	for _, p := range a.ApplyOnly {
		b.WriteString(p)
		b.WriteByte('\x00')
	}
	return b.String()
}

// cloneStrings returns a shallow copy of in, or nil if in is empty,
// preserving the `omitempty` behaviour for ModulePath. This mirrors
// the resolver's cloneModulePath: nil in, nil out so root-level
// resources keep their `module_path` field absent from JSON output.
func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// cloneWhen returns a shallow copy of a When map. AppliedConditional
// consumers can mutate the result without leaking back into the
// catalog's loaded storage. nil in, nil out — matching the resolver's
// cloneWhen and the AppliedConditional contract that an absent When
// renders as JSON `null` rather than `{}`. The catalog's only legal
// When values are scalars (bool / string / int / float64), so a
// shallow value copy is sufficient — there is no nested storage to
// alias through.
func cloneWhen(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

// moduleLess reports whether a sorts before b under a lexicographic
// segment-by-segment comparison, with shorter paths sorting before
// their extensions ([] < [a] < [a, b] < [b]). Mirrors the resolver's
// own moduleLess so the two packages cannot disagree on
// ModulePath ordering.
func moduleLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// errWriter is an io.Writer adapter that latches the first underlying
// write error so the trailing post-write check inside Render can surface
// io errors that fmt.Fprint{ln,f} otherwise discards. The pattern mirrors
// the errWriter in cmd/tfperms/catalog.go: we deliberately duplicate the
// few lines rather than export an internal helper because the two
// callers live in different layers (cmd/ vs. internal/reporter) and
// crossing that boundary just to share a 20-line type would invert the
// dependency direction.
//
// Once an error is latched, subsequent Writes short-circuit with
// (0, err); this is consistent with io.Writer's "non-nil error if
// n < len(p)" contract and keeps callers (including any future
// tabwriter wrappers) from silently appending bytes to a broken pipe.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) Write(p []byte) (int, error) {
	if ew.err != nil {
		return 0, ew.err
	}
	n, err := ew.w.Write(p)
	if err != nil {
		ew.err = err
		return n, err
	}
	// A writer that returns (n < len(p), nil) violates io.Writer's
	// contract but is observable in the wild (a misbehaving stdout pipe,
	// a custom test double). Latch io.ErrShortWrite so the trailing
	// ew.err check still catches the truncation rather than letting
	// Render exit nil with a half-written report.
	if n < len(p) {
		ew.err = io.ErrShortWrite
		return n, io.ErrShortWrite
	}
	return n, nil
}
