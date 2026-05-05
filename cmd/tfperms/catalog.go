package main

// catalog.go owns the `tfperms catalog ...` command tree. It is split out
// from main.go so the root-level command stays focused on version / help
// concerns. Subcommands under `catalog` operate on the YAML files under
// the repository's `catalog/` directory: `scaffold` writes a stub for a
// new entry, future siblings (stats, versions) will read existing
// entries.
//
// All filesystem work lives in internal/catalog/scaffold.go — this file
// is the cobra wiring only. Keeping the wiring thin lets the scaffold
// logic be unit-tested without spinning up a cobra command, and it keeps
// all "where on disk does this go" decisions in one place.

import (
	"fmt"
	"path/filepath"

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
			"emit a stub for a new entry.",
	}
	cmd.AddCommand(newCatalogScaffoldCmd())
	return cmd
}

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
				return fmt.Errorf("--data-source and --iam-binding are mutually exclusive")
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
