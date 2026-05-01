package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tfperms",
		Short:   "Static IAM permission analysis for Terraform GCP configs",
		Long:    "tfperms statically analyzes Terraform GCP configs and reports the minimum IAM permissions required for plan and apply, separately. It is not for runtime use.",
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	return cmd
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
