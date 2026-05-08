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

// rootArgsError wraps cobra.MaximumNArgs(1) with a friendlier error
// vocabulary. The default cobra message ("accepts at most 1 arg(s),
// received 2") is correct but ugly; callers see the directory-name
// language they typed.
func rootArgsError(cmd *cobra.Command, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("accepts at most one directory argument, got %d", len(args))
	}
	return nil
}

// newRootCmd wires the root analyze command. The single optional
// positional argument is the directory to analyze; it defaults to "."
// so a user can `cd` into a Terraform module and run `tfperms` with no
// arguments. Pass "." explicitly through to parser.LoadRecursive
// rather than relying on its empty-string rejection — the contract is
// "current working directory" and this is the way to ask for it.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tfperms [directory]",
		Short:   "Static IAM permission analysis for Terraform GCP configs",
		Long:    "tfperms statically analyzes Terraform GCP configs and reports the minimum IAM permissions required for plan and apply, separately. It is not for runtime use.",
		Version: fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
		Args:    rootArgsError,
		// SilenceUsage matches the catalog subcommands: once we are past
		// argument parsing, the trailing usage block is irrelevant and
		// suppressing it keeps the user's eye on the actual error.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
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

// runAnalyze orchestrates the analysis pipeline:
//
//	catalog.Load() → parser.LoadRecursive(dir) → resolver.Resolve()
//	→ reporter.Render()
//
// Hard errors short-circuit before reaching the reporter:
//   - catalog.Load() failures (malformed YAML, schema violation) — the
//     analyzer is unusable without its catalog; surface verbatim so
//     contributors see the same diagnostic vocabulary as `catalog stats`.
//   - parser.LoadRecursive() failures (missing dir, no .tf files,
//     permission denied at the root) — the analyzer cannot proceed
//     without a parseable root configuration.
//
// Warning-severity diagnostics (e.g. "could not load local module") are
// non-fatal and pass through to reporter.Render via the diags slice so
// the user sees them in the parse-warnings section of the report.
func runAnalyze(w io.Writer, dir string) error {
	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	resources, _, diags, err := parser.LoadRecursive(dir)
	if err != nil {
		return err
	}
	res := resolver.Resolve(resources, cat)
	return reporter.Render(w, res, diags, len(resources))
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
