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
//   - It relies on the resolver's deterministic sort contract for output
//     stability. PlanPerms / ApplyOnlyPerms / TotalApplyPerms / Unknowns
//     / Unresolved are documented to be sorted on the way out of
//     Resolve, so the reporter does not re-sort. A future Canonicalize
//     pass (beads tfperms-ftq.5) would belong here as a defence in
//     depth, but is not required for byte-stable output today.
package reporter

import (
	"fmt"
	"io"
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
//	  google_dataplex_lake (main.tf:42)
//	  ...
//
//	unresolved conditionals (3):
//	  google_storage_bucket.data: uniform_bucket_level_access (main.tf:14) — missing_variable
//	  ...
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
	ew := &errWriter{w: w}

	fmt.Fprintf(ew,
		"%d %s for %d %s, %d %s, %d %s\n",
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
		fmt.Fprintf(ew, "plan permissions (%d):\n", len(res.PlanPerms))
		for _, p := range res.PlanPerms {
			fmt.Fprintf(ew, "  %s\n", p)
		}
	}

	if len(res.ApplyOnlyPerms) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "apply-only permissions (%d):\n", len(res.ApplyOnlyPerms))
		for _, p := range res.ApplyOnlyPerms {
			fmt.Fprintf(ew, "  %s\n", p)
		}
	}

	if len(res.Unknowns) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "unknown resources (%d):\n", len(res.Unknowns))
		for _, u := range res.Unknowns {
			fmt.Fprintf(ew, "  %s (%s:%d)\n", u.Type, u.File, u.Line)
		}
	}

	if len(res.Unresolved) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintf(ew, "unresolved conditionals (%d):\n", len(res.Unresolved))
		for _, u := range res.Unresolved {
			fmt.Fprintf(ew, "  %s%s.%s: %s (%s:%d) — %s\n",
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
