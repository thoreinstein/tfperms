package main

// catalog.go owns the `tfperms catalog ...` command tree. It is split out
// from main.go so the root-level command stays focused on version / help
// concerns. Subcommands under `catalog` operate on the YAML files under
// the repository's `catalog/` directory: `scaffold` writes a stub for a
// new entry, `stats` summarises the merged in-memory catalog.
//
// All filesystem and aggregation work lives under internal/catalog/ —
// this file is the cobra wiring only. Keeping the wiring thin lets the
// underlying logic be unit-tested without spinning up a cobra command,
// and it keeps all "where on disk does this go" / "what counts as
// drift" decisions in one place.

import (
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/thoreinstein/tfperms/internal/catalog"
)

// newCatalogCmd returns the parent `tfperms catalog` command. It has no
// runnable behaviour of its own — Cobra prints help when invoked
// without a subcommand. Subcommands are registered on the returned
// command rather than free functions so that tests can build an
// isolated command tree without mutating package-level state.
func newCatalogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Manage and inspect the tfperms permission catalog",
		Long: "catalog provides subcommands for working with the YAML-backed " +
			"permission catalog under catalog/. Use `catalog scaffold` to " +
			"emit a stub for a new entry, `catalog stats` to inspect " +
			"catalog health (verification age, missing provenance, drift), " +
			"or `catalog versions` to count entries grouped by their " +
			"literal tested_against_provider clause.",
	}
	cmd.AddCommand(newCatalogScaffoldCmd())
	cmd.AddCommand(newCatalogStatsCmd())
	cmd.AddCommand(newCatalogVersionsCmd())
	return cmd
}

// newCatalogVersionsCmd returns `tfperms catalog versions`.
//
// The command loads the embedded catalog through catalog.Load() (the
// same strict-validation entry point used at production startup),
// computes the per-constraint census via catalog.AggregateVersions, and
// renders the result to stdout in a deterministic, golden-file-friendly
// layout. There are no flags today — the report shape is fixed by the
// underlying VersionGroup type, and a future --json variant belongs on
// this command rather than as a new top-level subcommand.
//
// Errors from catalog.Load() (a malformed YAML, a schema violation)
// are surfaced verbatim, matching the diagnostic vocabulary of
// `catalog stats` so contributors see one consistent shape across the
// catalog command tree.
func newCatalogVersionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "versions",
		Short: "Print a census of tested_against_provider clauses",
		Long: "Group catalog entries (resources, data sources, IAM bindings) " +
			"by their literal tested_against_provider clause and print the " +
			"counts. Grouping is by exact string match — semantically " +
			"equivalent clauses with different whitespace appear as " +
			"separate groups so formatting drift stays visible. Use " +
			"`catalog stats` for semantic drift detection against a " +
			"reference provider version.",
		Args: cobra.NoArgs,
		// SilenceUsage matches catalog stats / scaffold: once we are
		// past argument parsing, the trailing usage block is irrelevant
		// and suppressing it keeps the user's eye on the actual error.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := catalog.Load()
			if err != nil {
				return err
			}
			groups := catalog.AggregateVersions(cat)
			return renderCatalogVersions(cmd.OutOrStdout(), groups)
		},
	}
	return cmd
}

// renderCatalogVersions writes a deterministic, single-section report
// derived from groups to w. The format is fixed (no flags adjust it)
// so a golden-file test can pin it: deviations from the layout below —
// header text, column ordering, padding rules, footer text — are
// user-visible changes and require updating the golden fixture in the
// same diff.
//
// Layout:
//
//	tfperms catalog versions
//
//	tested_against_provider census:
//	  count  tested_against_provider
//	  N      <constraint>
//	  ...
//
//	Note: groups are matched by literal string. Whitespace differences
//	between otherwise-equivalent clauses appear as distinct rows. Use
//	`tfperms catalog stats` for semantic drift detection.
//
// An empty census (no entries) prints "(none)" under the header so a
// reader skimming the output cannot mistake "no constraints declared"
// for "the section was omitted by accident". The trailing footer is
// always printed — it is contributor-facing context, not a data row,
// and its presence anchors the format for the golden test.
//
// All writes go through an errWriter that latches the first underlying
// io.Writer error (the same helper renderCatalogStats uses). The
// per-section tabwriter flush wraps its failure with a section-
// specific %w so the cobra "Error: ..." line identifies where the pipe
// broke; the trailing errWriter check catches errors that only surface
// in plain Fprintln calls (the empty-census branch and the footer).
func renderCatalogVersions(w io.Writer, groups []catalog.VersionGroup) error {
	ew := &errWriter{w: w}

	fmt.Fprintln(ew, "tfperms catalog versions")
	fmt.Fprintln(ew)

	fmt.Fprintln(ew, "tested_against_provider census:")
	if len(groups) == 0 {
		fmt.Fprintln(ew, "  (none)")
	} else {
		tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  count\ttested_against_provider")
		for _, g := range groups {
			// AggregateVersions preserves empty
			// `tested_against_provider` values for in-memory callers
			// (a hand-rolled catalog mid-edit, a downstream JSON
			// consumer) rather than dropping them. A blank table cell
			// would make such a row effectively invisible and
			// indistinguishable from formatting damage, so substitute
			// an explicit placeholder to keep the contract visible at
			// the CLI surface.
			constraint := g.TestedAgainstProvider
			if constraint == "" {
				constraint = "(empty)"
			}
			fmt.Fprintf(tw, "  %d\t%s\n", g.Count, constraint)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("flush versions: %w", err)
		}
	}
	fmt.Fprintln(ew)

	// Footer is part of the format contract — golden test pins it.
	// Phrased as a "Note:" so it reads as auxiliary guidance rather
	// than a data row, and references `catalog stats` so a contributor
	// surprised by a duplicate-with-whitespace row knows where to go
	// for semantic analysis.
	fmt.Fprintln(ew, "Note: groups are matched by literal string. Whitespace differences")
	fmt.Fprintln(ew, "between otherwise-equivalent clauses appear as distinct rows. Use")
	fmt.Fprintln(ew, "`tfperms catalog stats` for semantic drift detection.")

	if ew.err != nil {
		return fmt.Errorf("write catalog versions: %w", ew.err)
	}
	return nil
}

// newCatalogStatsCmd returns `tfperms catalog stats`.
//
// The command loads the embedded catalog (going through catalog.Load()
// to share the strict validation pipeline used at production startup),
// computes the diagnostic snapshot via catalog.ComputeStats, and
// renders the snapshot to stdout in a deterministic, golden-file-
// friendly layout. There are no flags today; the report shape is
// fixed by the underlying CatalogStats type, and any future variants
// (--json, --service=storage) belong on this command rather than as
// new top-level subcommands.
//
// Errors from catalog.Load() (a malformed YAML, a schema violation)
// are surfaced verbatim — the same as `tfperms` startup would surface
// them — so the user has one consistent diagnostic vocabulary across
// commands.
func newCatalogStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Print health statistics for the embedded catalog",
		Long: "Aggregate and print catalog-level diagnostics: per-section totals, " +
			"per-service coverage (empirical vs docs+source), the five oldest " +
			"verifications, missing provenance markers (TODO sentinels), and " +
			"entries whose tested_against_provider clauses exclude the current " +
			"reference provider version.",
		Args: cobra.NoArgs,
		// SilenceUsage matches catalog scaffold: the trailing usage
		// block is irrelevant once we are past argument parsing, and
		// suppressing it keeps the user's eye on the actual error.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := catalog.Load()
			if err != nil {
				return err
			}
			stats := catalog.ComputeStats(cat, catalog.DefaultReferenceVersion)
			return renderCatalogStats(cmd.OutOrStdout(), stats)
		},
	}
	return cmd
}

// renderCatalogStats writes a deterministic multi-section report
// derived from stats to w. The format is fixed (no flags adjust it)
// so a golden-file test can pin it: deviations from the layout below
// — header text, section ordering, column ordering, padding rules —
// are user-visible changes and require updating the golden fixture in
// the same diff.
//
// The five sections are printed in the order:
//
//  1. Totals
//  2. Coverage by service
//  3. Oldest verifications
//  4. Missing provenance
//  5. Drift
//
// Each section ends with a single blank line so the overall report
// reads as a sequence of paragraphs. Empty sections still print a
// header followed by "(none)" so a reader skimming the output cannot
// mistake "no drift" for "drift section was omitted by accident".
//
// All writes — both direct fmt.Fprint calls and tabwriter.Flush calls
// — go through an errWriter that latches the first underlying
// io.Writer error. Per-section flushes still wrap their failure with a
// section-specific %w so the cobra "Error: ..." line identifies where
// the pipe broke; the trailing errWriter check catches errors that
// only surface in plain Fprintln calls (e.g. the empty-Drifting branch
// that has no flush after it). The contract: if any write fails, the
// command must exit non-zero rather than emit a truncated report under
// a successful exit code — that would let a CI consumer of
// `tfperms catalog stats` silently treat partial output as authoritative.
func renderCatalogStats(w io.Writer, stats catalog.CatalogStats) error {
	ew := &errWriter{w: w}

	fmt.Fprintln(ew, "tfperms catalog stats")
	fmt.Fprintln(ew)

	fmt.Fprintln(ew, "Totals:")
	tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  resources\t%d\n", stats.TotalResources)
	fmt.Fprintf(tw, "  data_sources\t%d\n", stats.TotalDataSources)
	fmt.Fprintf(tw, "  iam_bindings\t%d\n", stats.TotalIAMBindings)
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush totals: %w", err)
	}
	fmt.Fprintln(ew)

	fmt.Fprintln(ew, "Coverage by service:")
	if len(stats.Services) == 0 {
		fmt.Fprintln(ew, "  (none)")
	} else {
		tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  service\ttotal\tempirical\tdocs+source")
		for _, s := range stats.Services {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\n",
				s.Service, s.Total, s.Empirical, s.DocsSource)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("flush coverage: %w", err)
		}
	}
	fmt.Fprintln(ew)

	fmt.Fprintf(ew, "Oldest verifications (up to %d):\n", catalogOldestLimit)
	if len(stats.OldestVerified) == 0 {
		fmt.Fprintln(ew, "  (none)")
	} else {
		tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
		for _, e := range stats.OldestVerified {
			fmt.Fprintf(tw, "  %s\t%s/%s\t%s\n",
				e.Position, e.Section, e.Type, e.VerifiedAt)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("flush oldest verifications: %w", err)
		}
	}
	fmt.Fprintln(ew)

	fmt.Fprintln(ew, "Missing provenance (TODO sentinels):")
	if len(stats.MissingProvenance) == 0 {
		fmt.Fprintln(ew, "  (none)")
	} else {
		tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
		for _, p := range stats.MissingProvenance {
			fmt.Fprintf(tw, "  %s\t%s/%s\t%s\n",
				p.Position, p.Section, p.Type, p.Field)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("flush missing provenance: %w", err)
		}
	}
	fmt.Fprintln(ew)

	// ComputeStats trims ReferenceVersion at its entrance, so a plain
	// equality check is sufficient here — a whitespace-only value cannot
	// reach the renderer.
	driftHeader := "Drift from provider " + stats.ReferenceVersion + ":"
	if stats.ReferenceVersion == "" {
		driftHeader = "Drift (disabled — no reference version):"
	}
	fmt.Fprintln(ew, driftHeader)
	if len(stats.Drifting) == 0 {
		fmt.Fprintln(ew, "  (none)")
	} else {
		tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
		for _, d := range stats.Drifting {
			fmt.Fprintf(tw, "  %s\t%s/%s\t%s\n",
				d.Position, d.Section, d.Type, d.TestedAgainstProvider)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("flush drift: %w", err)
		}
	}
	if ew.err != nil {
		return fmt.Errorf("write catalog stats: %w", ew.err)
	}
	return nil
}

// errWriter is an io.Writer adapter that latches the first underlying
// write error. It exists so renderCatalogStats can fan io.Writer
// calls — both direct fmt.Fprint{ln,f} on the writer and tabwriter
// instances built on top of it — through a single point that records
// the first failure. The renderer drops fmt's own returned error
// (consistent with idiomatic Go fmt usage), but errWriter preserves
// it so the post-flush check at the end of renderCatalogStats can
// surface it. Once an error is latched, subsequent Writes short-
// circuit with (0, err); this is consistent with io.Writer's
// "non-nil error if n < len(p)" contract and is what tabwriter
// expects to bubble up through Flush.
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
	// contract but is observable in the wild (a misbehaving Stdout
	// pipe, a custom test double). Latch io.ErrShortWrite so the
	// trailing ew.err check still catches the truncation rather than
	// letting renderCatalogStats exit zero with a half-written report.
	if n < len(p) {
		ew.err = io.ErrShortWrite
		return n, io.ErrShortWrite
	}
	return n, nil
}

// catalogOldestLimit mirrors the cap inside ComputeStats (its
// oldestVerifiedLimit constant is package-private). Repeating the
// number locally keeps the renderer self-contained while the doc-
// comment ties them together; if either is bumped, the other must be
// updated in the same diff so the header text matches the data.
const catalogOldestLimit = 5

// newCatalogScaffoldCmd returns `tfperms catalog scaffold <resource-type>`.
//
// The command is intentionally narrow: it derives the target file from the
// resource type's service prefix, generates a YAML stub with TODO
// sentinels, and writes/appends it. It does NOT pre-populate real
// permission lists, contact GCP, or reach out to terraform-provider-google
// — those decisions are the contributor's job and the TODO sentinels
// signpost where they need to fill values in before the entry will pass
// `make catalog-validate`.
//
// The two mutually-exclusive flags pick the schema section for the stub:
//   - --data-source emits under data_sources: (read-only, plan-only)
//   - --iam-binding emits under iam_bindings: (with parent_resource: TODO)
//
// Without either flag the stub is a regular resource entry under
// resources:.
func newCatalogScaffoldCmd() *cobra.Command {
	var (
		dataSource bool
		iamBinding bool
		catalogDir string
	)

	cmd := &cobra.Command{
		Use:   "scaffold <resource-type>",
		Short: "Emit a YAML stub for a new catalog entry",
		Long: "Generate a placeholder catalog entry for the given Terraform " +
			"resource type and write it to the appropriate service file under " +
			"catalog/. If the file does not exist it is created; if the entry " +
			"already exists, scaffold exits non-zero without modifying the file. " +
			"All required schema fields are pre-filled with TODO sentinels for " +
			"the contributor to replace.",
		Args: cobra.ExactArgs(1),
		// SilenceUsage keeps the cobra usage block out of stderr when
		// RunE returns an error. Usage is only relevant for flag /
		// argument mistakes, which cobra surfaces before RunE runs;
		// duplicate-entry / I/O failures are runtime conditions and
		// the trailing usage block makes the actual error easy to
		// miss. SilenceErrors is left at its default so cobra still
		// prints the "Error: ..." line.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dataSource && iamBinding {
				return fmt.Errorf("tfperms: --data-source and --iam-binding are mutually exclusive")
			}

			resourceType := args[0]
			section := catalog.SectionResources
			switch {
			case dataSource:
				section = catalog.SectionDataSources
			case iamBinding:
				section = catalog.SectionIAMBindings
			}

			dir := catalogDir
			if dir == "" {
				dir = "catalog"
			}

			servicePath := catalog.InferServicePath(resourceType)
			targetPath := filepath.Join(dir, servicePath)

			result, err := catalog.Scaffold(catalog.ScaffoldRequest{
				ResourceType: resourceType,
				Section:      section,
				TargetPath:   targetPath,
			})
			if err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}

	cmd.Flags().BoolVar(&dataSource, "data-source", false,
		"Emit a data_sources stub instead of a resources stub")
	cmd.Flags().BoolVar(&iamBinding, "iam-binding", false,
		"Emit an iam_bindings stub instead of a resources stub")
	cmd.Flags().StringVar(&catalogDir, "catalog-dir", "",
		"Root directory containing service YAML files (defaults to ./catalog)")

	return cmd
}
