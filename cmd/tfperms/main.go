package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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

// rootLongDescription is the advisory framing surfaced by `tfperms
// --help` (cobra's Long). Per Epic 7 (tfperms-a6t.1), the long help is
// the place users learn the v1 limitations and the "output is
// advisory" framing — without that framing a reader of the rendered
// permission set might assume tfperms validates against a live
// project, gates CI, or compares required-vs-granted, none of which
// it does.
//
// The text is a literal block rather than fmt.Sprintf'd so a
// `tfperms --help` diff stays byte-stable across builds; future edits
// to the wording are deliberate (and reviewable) rather than
// emergent from a template variable change.
const rootLongDescription = `tfperms statically analyzes a Terraform Google Cloud Platform configuration
and reports the minimum IAM permissions required for 'terraform plan' and
'terraform apply', separately.

Output is advisory. Use the result as input when defining a custom IAM role
for a service account running terraform automation.

v1 limitations:
- Reads HCL directly. Module sources must be local (./, ../); registry,
  git, and archive modules are listed but not analyzed.
- Catalog covers ~30-50 most-common google_* resource types. Unknown
  resources are reported alongside, never silently omitted.
- tfperms does not validate against a live GCP project, does not gate CI,
  and does not compare required vs granted permissions.

See https://github.com/thoreinstein/tfperms for the full PRD and
contributing guide.`

// includeDeleteFlagHelp is the --help text for --include-delete. The
// trade-off explanation is part of the flag description per
// tfperms-a6t.2 — users seeing only `--help` (not the PRD) need to
// understand why they would flip it. Hoisted to a const so the long
// multi-line block does not crowd newRootCmd, keeping the cobra
// constructor scannable.
const includeDeleteFlagHelp = `include catalog Delete permissions in the apply set (default true).
Pass --include-delete=false (or --exclude-delete) for the strictly
smaller permission set valid for the next apply assuming no destroys.
Trade-off:
  include (default) — sufficient for any apply, including ones that destroy.
  exclude          — strictly smaller; will fail if terraform actually destroys.
The default 'include' is correct unless you know your apply will never destroy.`

// Output format selectors for the --format flag. `flat` is the default
// (the human-readable Render output that shipped in tfperms-ftq.1);
// `role` emits a GCP custom-role YAML document via reporter.RenderRole.
// Constants live here rather than in the reporter package because they
// are CLI surface — adding a third format means adding a third case in
// runAnalyze, not a third value in a reporter-side enum.
const (
	formatFlat       = "flat"
	formatRole       = "role"
	formatJSON       = "json"
	formatByResource = "by-resource"
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
//   - One positional argument: a directory path. Zero arguments prints
//     the long-form help (the advisory framing in rootLongDescription)
//     and exits 0 — there is no implicit "use cwd" behaviour because
//     a silent run against the current directory hid the v1 limitations
//     and the `--help` advisory framing from users invoking tfperms
//     for the first time. More than one argument is rejected at parse
//     time so the help text stays consistent with the implemented
//     surface.
//   - The positional argument must point at a directory; passing a
//     file produces a multi-line, "tfperms:"-prefixed error: a primary
//     line "tfperms: expected a directory, got a file: <path>" followed
//     by an indented hint line "(run 'tfperms <directory>' instead)".
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
		format        string
		roleName      string
		byResource    bool
		quiet         bool
		includeDelete bool
		excludeDelete bool
	)
	cmd := &cobra.Command{
		Use:     "tfperms [path]",
		Short:   "Static IAM permission analysis for Terraform GCP configs",
		Long:    rootLongDescription,
		Version: fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
		// MaximumNArgs(1) — zero or one positional. Zero is handled
		// in RunE (prints help and exits 0); one means "this is the
		// directory to analyse".
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
		//
		// Order matters: validateFormatFlags runs first so a typo like
		// `--by-resource --format=jsno` surfaces as the actionable
		// "invalid --format" error rather than the --by-resource /
		// --format conflict error. Only after the format value is
		// known to be one of the legal values does resolveFormat
		// reconcile --by-resource against an explicit --format.
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := validateFormatFlags(format, roleName); err != nil {
				return err
			}
			// cmd.Flags().Changed("format") distinguishes "user passed
			// --format=X" from "format holds its zero/default value
			// because the user did not pass --format". Without that
			// signal, resolveFormat cannot tell whether `--by-resource
			// --format=flat` is a real conflict (explicit clash) or a
			// no-op (default value sitting unset alongside the
			// shorthand). The conflict-detection branch below depends
			// on it.
			effective, err := resolveFormat(format, cmd.Flags().Changed("format"), byResource)
			if err != nil {
				return err
			}
			format = effective
			// Spec contract: --role-name without --format=role produces
			// a warning, not an error. The role name is unused outside
			// the role formatter, so silently accepting it is a UX trap
			// (the user typed something they expected to take effect).
			// We surface the warning to stderr, mention the offending
			// flag pair, and continue — the rest of the pipeline runs.
			// Validation of the regex is deferred to validateFormatFlags
			// (which now only enforces it under --format=role); here we
			// notice that the user passed --role-name at all without the
			// format that would consume it.
			if cmd.Flags().Changed("role-name") && format != formatRole {
				cmd.PrintErrf("warning: --role-name=%q is ignored without --format=role\n", roleName)
			}
			// --exclude-delete is sugar for --include-delete=false,
			// so it only overrides when the user actually asked to
			// exclude (the truthy form). An explicit
			// --exclude-delete=false matches the default and must NOT
			// flip --include-delete back on: a command like
			// `--include-delete=false --exclude-delete=false` clearly
			// asks for Delete permissions to be suppressed via the
			// canonical flag, and treating `--exclude-delete=false` as
			// an override would silently re-enable them. We therefore
			// gate the override on excludeDelete itself rather than on
			// "was the flag changed at all".
			if excludeDelete {
				includeDelete = false
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Zero positional args: print the long-form help (which
			// includes the advisory framing and v1 caveats) and exit
			// successfully. cmd.Help() writes to cmd.OutOrStderr by
			// default; tests redirect that into their capture buffer
			// via SetOut/SetErr so the assertion sees the body.
			if len(args) == 0 {
				return cmd.Help()
			}
			// Path validation runs against the raw argument so the
			// error message echoes what the user typed (not the
			// absolutized form). A file argument is rejected with a
			// pointer to the containing directory — a common new-user
			// mistake (`tfperms main.tf`) for which the abstract
			// "must be a directory" message is unhelpful.
			//
			// os.Stat can fail for reasons other than "not found"
			// (permission denied, ENOTDIR for a path component that
			// is a file, broken symlink loop, etc.). Reporting every
			// stat failure as "directory not found" misdiagnoses the
			// real problem — "tfperms: directory not found: /etc/p/q"
			// is actively misleading when the failure is "permission
			// denied on /etc/p". errors.Is(..., fs.ErrNotExist) is
			// the canonical Go check for the ENOENT-equivalent case;
			// everything else surfaces the underlying os.PathError
			// (already a single-line "stat <path>: <op error>") with
			// the tfperms: prefix prepended via %w.
			path := args[0]
			info, err := os.Stat(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("tfperms: directory not found: %s", path)
				}
				return fmt.Errorf("tfperms: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("tfperms: expected a directory, got a file: %s\n  (run 'tfperms <directory>' instead)", path)
			}
			return runAnalyze(cmd.OutOrStdout(), path, format, roleName, quiet, includeDelete)
		},
	}
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	cmd.Flags().StringVar(&format, "format", formatFlat, "output format: flat (default human-readable list), by-resource (grouped by resource type), role (GCP custom-role YAML), or json")
	cmd.Flags().StringVar(&roleName, "role-name", "", "GCP custom-role ID; required with --format=role and must match ^[a-zA-Z0-9_]{3,64}$ there. Without --format=role the value is ignored (warning emitted) rather than validated")
	cmd.Flags().BoolVar(&byResource, "by-resource", false, "shorthand for --format=by-resource; mutually exclusive with --format")
	// --quiet only suppresses display sections in the flat / by-resource
	// formats; the role and json formats are unaffected (the JSON
	// schema is a stability surface, and the role formatter does not
	// emit unknowns / unresolved sections at all). The summary line
	// keeps the accurate counts so downstream tooling that greps the
	// first line still detects diagnostic findings under --quiet.
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress unknown-resource and unresolved-conditional sections in flat and by-resource output (no effect on role/json)")
	// --include-delete defaults to true (the safe, "any apply" set);
	// --exclude-delete is sugar for --include-delete=false. Both are
	// declared so users discovering the flag via `tfperms --help` see
	// either name; PreRunE reconciles the two if both are set.
	cmd.Flags().BoolVar(&includeDelete, "include-delete", true, includeDeleteFlagHelp)
	cmd.Flags().BoolVar(&excludeDelete, "exclude-delete", false, "shorthand for --include-delete=false; suppress catalog Delete permissions from the apply set (truthy form overrides --include-delete; --exclude-delete=false is a no-op and never re-enables Delete)")
	cmd.AddCommand(newCatalogCmd())
	return cmd
}

// resolveFormat reconciles the --format and --by-resource flags. The
// --by-resource flag is sugar over --format=by-resource — it is
// idiomatic for an investigative-mode invocation (tfperms ./infra
// --by-resource) and matches the documented Journey 4 ergonomics. The
// two flags can disagree: a user passing both --format=<anything-but-
// by-resource> and --by-resource has expressed conflicting intent and
// we surface the conflict explicitly rather than silently picking one.
//
// formatExplicit signals whether the caller actually passed --format
// on the command line (versus format holding the default formatFlat
// because cobra populated the zero value). Without this signal we
// cannot distinguish `--by-resource` (the user wants by-resource;
// format defaults to "flat") from `--format=flat --by-resource` (the
// user explicitly contradicted themselves). The PreRunE wires this in
// from cmd.Flags().Changed("format"); direct test callers pass it
// explicitly.
//
// Returns the effective format value to thread through validation and
// dispatch:
//
//   - --by-resource alone (formatExplicit=false) returns
//     formatByResource (overriding the default formatFlat).
//   - --format=X without --by-resource returns X verbatim.
//   - --format=by-resource and --by-resource together is permitted
//     (they agree); --format=<anything-else> with --by-resource —
//     including --format=flat, the otherwise-default value — is a
//     conflict.
//
// The conflict error names both flags so the user knows exactly which
// pair to reconcile. validateFormatFlags handles the rest of the
// format-value validation downstream.
func resolveFormat(format string, formatExplicit, byResource bool) (string, error) {
	if !byResource {
		return format, nil
	}
	if !formatExplicit {
		// --by-resource alone (format holds its default value because
		// the user did not pass --format). Promote to by-resource.
		return formatByResource, nil
	}
	if format == formatByResource {
		// Both flags set, both agree — permit and proceed.
		return formatByResource, nil
	}
	return "", fmt.Errorf("tfperms: --by-resource conflicts with --format=%s; pass either --by-resource or --format=by-resource, not both", format)
}

// validateFormatFlags rejects --format / --role-name combinations the
// pipeline cannot satisfy. Split out from PreRunE so unit tests can
// drive the validation without constructing a cobra command.
//
// Rules:
//
//   - --format must be one of formatFlat, formatByResource, formatRole,
//     or formatJSON. Any other value is a user typo (e.g. --format=yaml)
//     and rejected with an explicit listing of the legal values rather
//     than letting the dispatch branch silently fall through to the
//     default formatter.
//   - --format=role requires a non-empty --role-name. The role name is
//     used as both the YAML title and the gcloud command's role-ID
//     positional argument; a missing name would generate a file gcloud
//     cannot apply.
//   - When --format=role and --role-name is provided, it must match
//     roleNameRE — the YAML's title and the gcloud role-ID positional
//     are derived from this value, and a non-conforming name produces
//     a file gcloud will reject. Validation is gated on --format=role
//     because the spec frames --role-name as "ignored otherwise" and
//     mandates a warning (not a hard error) when the user passes it
//     without the role formatter — the warning is emitted by PreRunE,
//     and we deliberately do not error here on a malformed value
//     outside the role formatter.
//
// Every returned error names the offending flag (cobra's default
// error printing does not include the flag name): an unknown
// --format value, a missing --role-name when --format=role, or a
// --role-name that does not match the GCP custom-role-ID regex
// under --format=role.
func validateFormatFlags(format, roleName string) error {
	switch format {
	case formatFlat, formatByResource, formatRole, formatJSON:
		// fine
	default:
		return fmt.Errorf("tfperms: invalid --format value %q (must be one of flat, by-resource, role, json)", format)
	}
	if format == formatRole {
		if roleName == "" {
			return fmt.Errorf("tfperms: --role-name is required when --format=role")
		}
		if !roleNameRE.MatchString(roleName) {
			return fmt.Errorf("tfperms: --role-name must match %s; got %q\n  (note: dashes are not allowed; use underscores)", roleNameRE.String(), roleName)
		}
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
//   - formatByResource → reporter.RenderByResource (grouped output,
//     Journey 4).
//   - formatRole → reporter.RenderRole (GCP custom-role YAML, using
//     roleName as the title and gcloud role-ID).
//   - formatJSON → reporter.RenderJSON (stable, versioned JSON output).
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
//
// Path relativisation applies to every File field on the resolver
// Result: parser.LoadRecursive emits absolute paths (it absolutises
// dir before walking), and surfacing those to a user inspecting their
// own configuration is noise — they want module-relative paths like
// `modules/api/main.tf`. relativizeResult does that rewrite once,
// after Resolve, so every formatter sees the same root-relative
// shape.
//
// quiet suppresses the `unknown resources` and `unresolved
// conditionals` sections in the flat and by-resource formats only.
// The role and json formats ignore the flag: role output is a YAML
// custom-role document that has no diagnostic sections to suppress,
// and the JSON v1.0 schema is a stability surface — silently dropping
// the `unknowns` / `unresolved` arrays under --quiet would break
// integration consumers that always expect those keys to be present.
//
// includeDelete maps onto resolver.ResolveOptions.ExcludeDelete with
// inverted polarity: true (the CLI default) leaves ExcludeDelete at its
// zero value so catalog Delete permissions flow through to the apply
// set; false sets ExcludeDelete: true to suppress them. The CLI
// surface keeps the positive --include-delete framing because that
// matches the safe default users expect, while the resolver API is
// intentionally negative so a programmatic caller passing
// ResolveOptions{} does not silently shrink the apply set. PreRunE
// reconciles the --include-delete / --exclude-delete pair before the
// value reaches here, so runAnalyze sees a single, already-decided
// boolean.
func runAnalyze(w io.Writer, dir, format, roleName string, quiet, includeDelete bool) error {
	resources, _, diags, err := parser.LoadRecursive(dir)
	if err != nil {
		return err
	}
	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("tfperms: catalog corrupt — please file an issue: %w", err)
	}
	result := resolver.Resolve(resources, cat, resolver.ResolveOptions{ExcludeDelete: !includeDelete})
	result.Diagnostics = relativizeDiags(diags, dir)
	relativizeResult(&result, dir)
	switch format {
	case formatByResource:
		return reporter.RenderByResource(w, result, len(resources), quiet)
	case formatRole:
		return reporter.RenderRole(w, result, roleName, version, date)
	case formatJSON:
		return reporter.RenderJSON(w, result, len(resources), version)
	default:
		return reporter.Render(w, result, len(resources), quiet)
	}
}

// relativizeResult rewrites every File field on res so the path is
// relative to baseDir and uses forward slashes regardless of the host
// OS. parser.LoadRecursive absolutises baseDir before walking, so
// every File we see at this layer is an absolute path; surfacing
// those to the user (in flat-list output, by-resource output, or
// JSON) leaks developer-machine paths into reports that should read
// the same on every host. Mirrors the relativisation the catalog
// regression harness applies to its goldens — same problem, same
// solution.
//
// A path that does not lie under baseDir is left unchanged. That
// should not happen for fixtures rooted at baseDir, but emitting the
// absolute path verbatim is a louder failure than a silent rewrite if
// the parser ever surprises us.
//
// Diagnostics are not rewritten here because relativizeDiags handles
// them at the conversion boundary (they originate from hcl.Diagnostic
// and undergo a separate filter / convert pass).
func relativizeResult(res *resolver.Result, baseDir string) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return
	}
	for i := range res.Unknowns {
		res.Unknowns[i].File = relativiseAgainst(res.Unknowns[i].File, absBase)
	}
	for i := range res.Unresolved {
		res.Unresolved[i].File = relativiseAgainst(res.Unresolved[i].File, absBase)
	}
	for i := range res.Resources {
		res.Resources[i].File = relativiseAgainst(res.Resources[i].File, absBase)
	}
}

// relativiseAgainst returns file as a forward-slashed path relative
// to absBase, or file unchanged if filepath.Rel fails or the result
// escapes absBase (a literal ".." segment, not just a ".." prefix —
// "..foo" is a valid child name). Empty paths pass through.
func relativiseAgainst(file, absBase string) string {
	if file == "" {
		return file
	}
	rel, err := filepath.Rel(absBase, file)
	if err != nil {
		return file
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return file
	}
	return filepath.ToSlash(rel)
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

// run is the testable entry point for the binary. It wraps
// cmd.Execute() with a panic recovery boundary so an unexpected
// panic anywhere in the pipeline (parser, catalog, resolver, reporter)
// surfaces as a single-line `tfperms: internal error (panic): <message>`
// on stderr rather than dumping a Go stack trace at the user. Stack
// traces leak implementation details and are illegible to a CLI
// operator who is debugging a Terraform configuration, not the
// internals of tfperms.
//
// The cobra command is passed in (rather than built inside run) so a
// test caller can inject a command whose RunE deliberately panics and
// observe the recovery path. main() supplies newRootCmd(); production
// behaviour is unchanged. Returned int is the exit code so the test
// can drive run without invoking os.Exit. Splitting run from main is
// the standard Go pattern for this — main does I/O setup and process
// exit, run does the work.
//
// Error reporting policy: every error that reaches stderr is prefixed
// with `tfperms: ` exactly once. Errors built inside the codebase
// already carry the prefix (validateFormatFlags, resolveFormat, the
// path-validation branches in RunE, the parser/walker layers); errors
// that originate elsewhere (cobra's own argument parsing, third-party
// libraries) are prefixed here so the user sees one consistent shape
// regardless of which layer rejected the input. The `if !strings.HasPrefix`
// guard prevents the prefix from being doubled when the error already
// carries it.
func run(cmd *cobra.Command, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			// A panic during Execute is by definition an internal bug,
			// not user error. Render a single-line message that names
			// the panic value (which may be an error, a string, or any
			// arbitrary value) and exit non-zero. We deliberately do
			// NOT print the stack trace — the user is a Terraform
			// operator, not a tfperms developer, and the trace would
			// bury the actionable summary. A developer reproducing the
			// bug locally can re-run without the recover for a full
			// trace.
			//
			// `code` is a named return so the deferred recover can set
			// the exit status without re-entering the normal return
			// path (Go's deferred functions can mutate named returns).
			fmt.Fprintf(stderr, "tfperms: internal error (panic): %v\n", r)
			code = 1
		}
	}()
	if err := cmd.Execute(); err != nil {
		msg := err.Error()
		// Prefix-once policy: errors constructed inside tfperms
		// (validateFormatFlags, runAnalyze, parser, walker, ...)
		// already carry the `tfperms: ` prefix because the message
		// shape is part of their contract — those errors are surfaced
		// verbatim. Errors that originate elsewhere (cobra's argument
		// validation, an unwrapped third-party error) do not, and
		// prefixing them here keeps the user-visible shape uniform
		// across every error path.
		if !strings.HasPrefix(msg, "tfperms: ") {
			msg = "tfperms: " + msg
		}
		fmt.Fprintln(stderr, msg)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(newRootCmd(), os.Stderr))
}
