package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/thoreinstein/tfperms/internal/catalog"
	"github.com/thoreinstein/tfperms/internal/parser"
	"github.com/thoreinstein/tfperms/internal/reporter"
	"github.com/thoreinstein/tfperms/internal/resolver"
)

// Build metadata. These are intentionally `var` (not `const`) so the linker
// can override them via `-ldflags "-X main.version=... -X main.commit=... -X main.date=..."`
// during release builds. A `const` cannot be overridden by ldflags and would
// silently no-op, leaving every release reporting the dev defaults below.
var (
	version = "0.0.0-dev"
	commit  = "none"
	date    = "unknown"
)

// rootDefaultDir is the path used when the user invokes `tfperms` with
// no positional argument. It maps to the current working directory. We
// pass "." rather than calling os.Getwd ourselves because parser.
// LoadRecursive performs the filepath.Abs resolution and rejects an
// empty string explicitly — letting it own the lookup keeps the
// "what does no-arg mean" rule in one place.
const rootDefaultDir = "."

// newRootCmd returns the `tfperms` cobra command tree.
//
// The root command analyses a Terraform configuration and prints the
// flat-list permission report (Epic 6 / tfperms-ftq.1):
//
//   - Optional positional argument: a directory path. Omitted means
//     `.` (the current working directory). More than one argument is
//     rejected by cobra at parse time so the help text stays
//     consistent with the implemented surface.
//   - Pipeline: parser.LoadRecursive(dir) → catalog.Load() →
//     resolver.Resolve(...) → reporter.Render(stdout, ...).
//
// Errors at any stage propagate to cobra. SilenceUsage: true
// suppresses the trailing usage block on every error path — including
// cobra's own Args validation errors (e.g. too many positional
// arguments). The trade-off is deliberate: the help text is one
// `tfperms --help` away, and printing it after every parse failure
// or broken-pipe error buries the actual cause. Tests pin the
// behaviour separately (TestRootCommandRejectsExtraArgs).
//
// The catalog and `version` subcommands remain as-is; this RunE
// addition is the first time the root command itself is runnable.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tfperms [path]",
		Short:   "Static IAM permission analysis for Terraform GCP configs",
		Long:    "tfperms statically analyzes Terraform GCP configs and reports the minimum IAM permissions required for plan and apply, separately. It is not for runtime use.",
		Version: fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
		// MaximumNArgs(1) — zero or one positional. Zero means "use
		// rootDefaultDir"; one means "this is the directory to analyse".
		Args: cobra.MaximumNArgs(1),
		// SilenceUsage matches the catalog subcommand tree:
		// suppressing the trailing usage block on every error
		// (including cobra Args validation) keeps the user's eye on
		// the actual error. See newRootCmd's doc comment for the
		// trade-off.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := rootDefaultDir
			if len(args) == 1 {
				dir = args[0]
			}
			return runAnalyze(cmd.OutOrStdout(), dir)
		},
	}
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	cmd.AddCommand(newCatalogCmd())
	return cmd
}

// runAnalyze executes the parser → catalog → resolver → reporter
// pipeline against dir and writes the flat-list report to w. Split out
// from RunE so tests can drive the pipeline directly without
// constructing a cobra command, and so a future `tfperms <path>
// --format=...` flag has an obvious place to dispatch on format
// without bloating the cobra wiring.
//
// The resource count passed to reporter.Render is len(resources) —
// every distinct resource block produced by parser.LoadRecursive,
// counting each module-instance copy as its own resource. This is the
// definition tfperms-ftq.1 calls "distinct resources (counting module
// instances)".
//
// hcl.Diagnostics returned from parser.LoadRecursive are intentionally
// dropped here. The parser surfaces nested-module parse failures as
// warnings; rendering them belongs alongside the unresolved /
// unknowns sections of the report and is tracked by tfperms-ftq.6
// (render unknowns and unresolved per format). Wiring them in this
// commit would expand the surface beyond the plan; flag in Issues so
// the reviewer sees the deferred work.
func runAnalyze(w io.Writer, dir string) error {
	resources, _, _, err := parser.LoadRecursive(dir)
	if err != nil {
		return fmt.Errorf("load %q: %w", dir, err)
	}
	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	result := resolver.Resolve(resources, cat)
	return reporter.Render(w, result, len(resources))
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
