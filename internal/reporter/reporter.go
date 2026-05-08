// Package reporter renders the deterministic CLI report that the root
// `tfperms` command emits after running parser.LoadRecursive,
// catalog.Load, and resolver.Resolve.
//
// The report is a flat, multi-section text layout. Section ordering and
// line shape are part of the public contract: the table-driven tests in
// reporter_test.go pin the exact byte layout, and the integration test
// in cmd/tfperms/main_test.go reuses these strings to assert that
// diagnostics surface end-to-end.
//
// Layout (sections after the header are omitted when empty, except
// "plan permissions" and "additional apply permissions" which are
// always rendered so a reader skimming the output cannot mistake an
// omitted section for an analysis bug):
//
//	tfperms analyze
//
//	N permissions for M resources, X unknowns, Y unresolved conditionals, Z parse warning(s)
//
//	plan permissions:
//	  <perm>
//	  ...
//
//	additional apply permissions:
//	  <perm>
//	  ...
//
//	unresolved conditionals:
//	  <type>.<name>[ in module.<a>.module.<b>] attr=<attr> reason=<reason> (file:line)
//	  ...
//
//	parse warnings:
//	  <summary> (file:line)
//	    <detail>
//	  ...
//
//	unknown resources:
//	  <type> (file:line)
//	  ...
//
// Determinism is preserved by sorting parse warnings by (Filename, Line)
// before rendering. resolver.Result already sorts its slices, so the
// reporter does not re-sort them — re-sorting would risk silently
// drifting from the resolver's contract.
package reporter

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

// Render writes the deterministic report for res, diags, and
// resourceCount to w.
//
// resourceCount is the total number of resources the parser surfaced —
// used in the header line. It is passed in rather than derived from
// res because resolver.Result does not retain a count of resources
// it skipped via Unknown classification, and conflating "matched"
// with "total" in the header would understate the analyzer's coverage
// to the user.
//
// diags carries warning-severity entries from parser.LoadRecursive.
// Error-severity entries are rendered alongside warnings rather than
// suppressed: by the time Render is reached, hard parse errors have
// already short-circuited runAnalyze, so anything left in diags is by
// definition non-fatal context the user should still see.
//
// Render returns the first non-nil error from w.Write so callers (the
// cobra RunE) surface broken-pipe failures rather than exiting zero
// with a half-written report.
func Render(w io.Writer, res resolver.Result, diags hcl.Diagnostics, resourceCount int) error {
	ew := &errWriter{w: w}

	fmt.Fprintln(ew, "tfperms analyze")
	fmt.Fprintln(ew)

	totalPerms := len(res.TotalApplyPerms)
	unknownCount := len(res.Unknowns)
	unresolvedCount := len(res.Unresolved)
	warningCount := len(diags)

	fmt.Fprintf(ew, "%d permissions for %d resources, %d unknowns, %d unresolved conditionals, %d %s\n",
		totalPerms, resourceCount, unknownCount, unresolvedCount, warningCount, pluralize("parse warning", warningCount))
	fmt.Fprintln(ew)

	fmt.Fprintln(ew, "plan permissions:")
	if len(res.PlanPerms) == 0 {
		fmt.Fprintln(ew, "  (none)")
	} else {
		for _, p := range res.PlanPerms {
			fmt.Fprintf(ew, "  %s\n", p)
		}
	}
	fmt.Fprintln(ew)

	fmt.Fprintln(ew, "additional apply permissions:")
	if len(res.ApplyOnlyPerms) == 0 {
		fmt.Fprintln(ew, "  (none)")
	} else {
		for _, p := range res.ApplyOnlyPerms {
			fmt.Fprintf(ew, "  %s\n", p)
		}
	}

	if len(res.Unresolved) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintln(ew, "unresolved conditionals:")
		for _, u := range res.Unresolved {
			modSuffix := ""
			if len(u.ModulePath) > 0 {
				// Render the chain as "module.a.module.b" so a reader
				// can paste it straight into a `terraform state` query
				// without translating tfperms-internal vocabulary.
				modSuffix = " in module." + strings.Join(u.ModulePath, ".module.")
			}
			fmt.Fprintf(ew, "  %s.%s%s attr=%s reason=%s (%s:%d)\n",
				u.ResourceType, u.ResourceName, modSuffix, u.Attribute, u.Reason, u.File, u.Line)
		}
	}

	if len(diags) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintln(ew, "parse warnings:")
		for _, d := range sortedDiags(diags) {
			file, line := diagLocation(d)
			fmt.Fprintf(ew, "  %s (%s:%d)\n", d.Summary, file, line)
			if d.Detail != "" {
				fmt.Fprintf(ew, "    %s\n", d.Detail)
			}
		}
	}

	if len(res.Unknowns) > 0 {
		fmt.Fprintln(ew)
		fmt.Fprintln(ew, "unknown resources:")
		for _, u := range res.Unknowns {
			fmt.Fprintf(ew, "  %s (%s:%d)\n", u.Type, u.File, u.Line)
		}
	}

	if ew.err != nil {
		return fmt.Errorf("write analyze report: %w", ew.err)
	}
	return nil
}

// sortedDiags returns a copy of diags sorted by (Filename, Line). The
// copy keeps Render side-effect-free with respect to its caller, so a
// caller that wants to re-render the same diagnostics later (e.g. a
// future --json formatter) sees identical input. Diagnostics whose
// Subject is nil sort first under the empty-string filename so they
// render in a stable position rather than crashing on a nil deref.
func sortedDiags(diags hcl.Diagnostics) hcl.Diagnostics {
	out := make(hcl.Diagnostics, len(diags))
	copy(out, diags)
	sort.SliceStable(out, func(i, j int) bool {
		fi, li := diagLocation(out[i])
		fj, lj := diagLocation(out[j])
		if fi != fj {
			return fi < fj
		}
		return li < lj
	})
	return out
}

// diagLocation extracts the source location from d, defending against
// a nil Subject. moduleLoadWarning always populates Subject, but
// future diagnostic producers may not — returning ("", 0) keeps the
// renderer total rather than panicking.
func diagLocation(d *hcl.Diagnostic) (string, int) {
	if d == nil || d.Subject == nil {
		return "", 0
	}
	return d.Subject.Filename, d.Subject.Start.Line
}

// pluralize returns "<word>" for n == 1 and "<word>s" otherwise. The
// header uses this so "1 parse warning" reads naturally and "0 parse
// warnings" / "2 parse warnings" stay grammatical.
func pluralize(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// errWriter latches the first underlying io.Writer error so callers
// see a broken pipe / short write surface as a single error from
// Render rather than exiting zero with a half-written report.
//
// This mirrors the pattern in cmd/tfperms/catalog.go to keep the
// failure-handling vocabulary consistent across the CLI surface.
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
	if n < len(p) {
		ew.err = io.ErrShortWrite
		return n, io.ErrShortWrite
	}
	return n, nil
}
