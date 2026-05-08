package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/hashicorp/hcl/v2"
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

// Output format selectors for the --format flag. `flat` is the default
// (the human-readable Render output that shipped in tfperms-ftq.1);
// `role` emits a GCP custom-role YAML document via reporter.RenderRole.
// Constants live here rather than in the reporter package because they
// are CLI surface — adding a third format means adding a third case in
// runAnalyze, not a third value in a reporter-side enum.
const (
	formatFlat = "flat"
	formatRole = "role"
)

// roleNameRE is the regex a --role-name value must match before the
// `role` formatter will accept it. The pattern matches GCP's custom-
// role-ID constraint (alphanumeric and underscore, 3 to 64 chars
// inclusive) so a user copy-pasting the rendered file's `gcloud iam
// roles create <ID> --file=role.yaml` command cannot generate a name
// the API will reject. Compiled at package load (not per-invocation)
// because newRootCmd is called many times across the test suite and
// recompiling the regex per construction shows up in test timings.
var roleNameRE = regexp.MustCompile(`^[a-zA-Z0-9_]{3,64}$`)

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
	// Flag-backing variables are scoped to this constructor (closed
	// over by RunE / PreRunE) rather than declared at package level
	// so each newRootCmd call gets a fresh zero value — important
	// because the test suite builds a new root command per test and
	// would otherwise see cross-test bleed if a prior test left a
	// stale --role-name in package state.
	var (
		format   string
		roleName string
	)
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
		// SilenceErrors prevents cobra from printing the returned
		// error to stderr. main() is the single layer that prints
		// the error; without this, the message would appear twice.
		SilenceErrors: true,
		// PreRunE validates the --format / --role-name combination
		// before the pipeline runs. Putting the validation here (not
		// inside runAnalyze) means a misconfigured invocation fails
		// before parser.LoadRecursive walks the disk — quicker
		// feedback on a CI run that mistypes the role name.
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return validateFormatFlags(format, roleName)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := rootDefaultDir
			if len(args) == 1 {
				dir = args[0]
			}
			return runAnalyze(cmd.OutOrStdout(), dir, format, roleName)
		},
	}
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	cmd.Flags().StringVar(&format, "format", formatFlat, "output format: flat (default human-readable list) or role (GCP custom-role YAML)")
	cmd.Flags().StringVar(&roleName, "role-name", "", "GCP custom-role ID; whenever provided must match ^[a-zA-Z0-9_]{3,64}$, and is required with --format=role")
	cmd.AddCommand(newCatalogCmd())
	return cmd
}

// validateFormatFlags rejects --format / --role-name combinations the
// pipeline cannot satisfy. Split out from PreRunE so unit tests can
// drive the validation without constructing a cobra command.
//
// Rules:
//
//   - --format must be either formatFlat or formatRole. Any other value
//     is a user typo (e.g. --format=yaml) and rejected with an explicit
//     listing of the legal values rather than letting the dispatch
//     branch silently fall through to the default formatter.
//   - --format=role requires a non-empty --role-name. The role name is
//     used as both the YAML title and the gcloud command's role-ID
//     positional argument; a missing name would generate a file gcloud
//     cannot apply.
//   - When --role-name is provided (regardless of --format), it must
//     match roleNameRE. We validate the value even under --format=flat
//     so a user who sets the name first and forgets to flip --format
//     gets a clear error rather than having the name silently
//     ignored — the most user-hostile of the available behaviours.
//
// Every returned error names the offending flag (cobra's default
// error printing does not include the flag name): an unknown
// --format value, a missing --role-name when --format=role, or a
// --role-name that does not match the GCP custom-role-ID regex.
func validateFormatFlags(format, roleName string) error {
	switch format {
	case formatFlat, formatRole:
		// fine
	default:
		return fmt.Errorf("invalid --format %q: must be one of %q or %q", format, formatFlat, formatRole)
	}
	if format == formatRole && roleName == "" {
		return fmt.Errorf("--format=%s requires --role-name", formatRole)
	}
	if roleName != "" && !roleNameRE.MatchString(roleName) {
		return fmt.Errorf("invalid --role-name %q: must match %s", roleName, roleNameRE.String())
	}
	return nil
}

// runAnalyze executes the parser → catalog → resolver → reporter
// pipeline against dir and writes the requested report format to w.
// Split out from RunE so tests can drive the pipeline directly without
// constructing a cobra command.
//
// format selects the reporter:
//
//   - formatFlat → reporter.Render (the default human-readable list).
//   - formatRole → reporter.RenderRole (GCP custom-role YAML, using
//     roleName as the title and gcloud role-ID).
//
// Validation of the format / roleName combination is the caller's
// responsibility — newRootCmd's PreRunE handles it before runAnalyze
// is reached. A direct test caller passing an unknown format gets the
// formatFlat fallback rather than an error, because runAnalyze should
// never be the layer that rejects user input (that is what PreRunE is
// for, and a direct programmatic caller has no user-input surface).
//
// The resource count passed to reporter.Render is len(resources) —
// every distinct resource block produced by parser.LoadRecursive,
// counting each module-instance copy as its own resource. This is the
// definition tfperms-ftq.1 calls "distinct resources (counting module
// instances)". The role formatter does not consume the count
// because a custom-role YAML is purely a permission listing.
func runAnalyze(w io.Writer, dir, format, roleName string) error {
	resources, _, diags, err := parser.LoadRecursive(dir)
	if err != nil {
		return fmt.Errorf("load %q: %w", dir, err)
	}
	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	result := resolver.Resolve(resources, cat)
	result.Diagnostics = relativizeDiags(diags, dir)
	if format == formatRole {
		return reporter.RenderRole(w, result, roleName, version, date)
	}
	return reporter.Render(w, result, len(resources))
}

// relativizeDiags converts hcl.Diagnostics to a slice of
// resolver.Diagnostic, filtering for warnings and relativizing file
// paths against baseDir. Only warnings are converted; error-severity
// diagnostics are handled by the parser returning a non-nil error.
// If a diagnostic's Subject is nil, the filename falls back to
// "<unknown>". If filepath.Abs or filepath.Rel fails, the diagnostic
// retains its original Subject.Filename (typically an absolute path).
//
// baseDir is the user-supplied input directory and may be relative
// (e.g. "fixtures/warn") or even "."; parser.LoadRecursive normalizes
// it via filepath.Abs before populating diagnostic Subject filenames,
// so we apply the same normalization here. Otherwise filepath.Rel
// would compare a relative base against an absolute filename, fall
// into the error branch, and emit absolute paths to the user — the
// exact regression the prior review caught.
func relativizeDiags(diags hcl.Diagnostics, baseDir string) []resolver.Diagnostic {
	absBase, absErr := filepath.Abs(baseDir)
	out := make([]resolver.Diagnostic, 0)
	for _, d := range diags {
		if d.Severity != hcl.DiagWarning {
			continue
		}
		file := "<unknown>"
		line := 0
		if d.Subject != nil {
			line = d.Subject.Start.Line
			file = d.Subject.Filename
			if absErr == nil {
				if rel, err := filepath.Rel(absBase, d.Subject.Filename); err == nil {
					file = rel
				}
			}
		}
		out = append(out, resolver.Diagnostic{
			Summary: d.Summary,
			File:    file,
			Line:    line,
		})
	}
	return out
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
