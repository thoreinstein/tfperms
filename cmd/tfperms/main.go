package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tfperms",
		Short:   "Static IAM permission analysis for Terraform GCP configs",
		Long:    "tfperms statically analyzes Terraform GCP configs and reports the minimum IAM permissions required for plan and apply, separately. It is not for runtime use.",
		Version: fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	cmd.AddCommand(newCatalogCmd())
	return cmd
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
