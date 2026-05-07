package parser

// HCL parsing layer. Companion to walker.go: walker produces the file list,
// Parse turns those files into structured Resource values that downstream
// stages (.5 attribute extraction, .6 variable/locals expansion, Epic 3
// module resolution, Epic 5 dedup) iterate over.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Resource is a single resource or data block extracted from a Terraform
// configuration.
//
// Field contracts:
//   - Kind  is exactly "resource" or "data" — the HCL block type. Future
//     top-level block types (provider, module, output, ...) are silently
//     skipped by Parse and never produce a Resource.
//   - Type  is the first label of the block, e.g. "google_storage_bucket".
//   - Name  is the second label — the local name as written in HCL.
//   - File  is the source file path. It is whatever was passed to Parse,
//     so when the walker hands Parse absolute paths, this field is an
//     absolute path; the package does not normalise.
//   - Line  is reported via DefRange.Start.Line, which is the line of the
//     `resource`/`data` keyword. For conventionally-formatted Terraform
//     (`resource "x" "y" {` on one line) this is the same line as the
//     opening brace; for pathologically multi-line block headers it is
//     the keyword line, not the brace line.
//   - Attrs is always non-nil. It contains exactly one entry per
//     top-level attribute on the block, excluding Terraform meta-
//     arguments (provider, depends_on, count, for_each — count /
//     for_each are routed through metaargs.go's evalMetaArgs and
//     surface via Parse's keep/drop decision and warning diagnostics).
//     Nested blocks (lifecycle, dynamic, provisioner, ...) do not
//     contribute keys. Each value is either a fully-resolved cty.Value
//     (literals; var.X / local.X resolved through the eval context
//     built from the config's `variable` and `locals` blocks — see
//     evalctx.go) or cty.NilVal when the right-hand side could not be
//     evaluated at this stage (function calls, interpolations
//     referencing unknowns, cross-resource references,
//     missing/unresolved variables/locals). Callers that need richer
//     resolution should treat cty.NilVal as "deferred / unknown"
//     rather than "absent".
//   - AttrReasons is always non-nil. For every Attrs entry whose value
//     is cty.NilVal, AttrReasons holds a stable classification string
//     describing *why* the expression could not be resolved. Resolved
//     attributes have no AttrReasons entry — callers can therefore use
//     the reasons map to enumerate the unresolved subset without
//     re-checking each cty.Value. The classification strings are
//     defined in attrs.go (ReasonFunctionCall, ReasonDataSource,
//     ReasonMissingVariable, ReasonOther) and are exposed downstream
//     via resolver.UnresolvedConditional.Reason; they are part of the
//     public API and must not change without coordinating with the
//     resolver's JSON contract.
//   - DynamicBlocks holds the labels of `dynamic "<label>" { ... }`
//     blocks declared *directly* on this resource's body, in source
//     order. Empty slice (or nil; both are valid zero values, callers
//     must not rely on the distinction) when there are no dynamic
//     blocks. Only top-level dynamic blocks are captured — dynamic
//     blocks nested inside another block (e.g. inside a `content`
//     body) are deliberately ignored at this layer. v1 non-goal #1.
//   - PreventDestroy is true iff the resource declares a `lifecycle {
//     prevent_destroy = true }` block whose RHS is a literal boolean
//     `true`. Anything else — `prevent_destroy = false`, no lifecycle
//     block, no prevent_destroy attribute, or a non-literal expression
//     such as `var.lock` — leaves this `false`. v1 non-goal #5.
//   - ModulePath is the chain of module call names that traverse from
//     the root configuration down to this resource. Root-level
//     resources (those declared in the directory passed to
//     LoadRecursive) have an empty/nil ModulePath. A resource declared
//     inside `module "foo"` called from the root has
//     ModulePath = ["foo"]. A resource declared inside `module "bar"`
//     called from inside `module "foo"` has
//     ModulePath = ["foo", "bar"]. Plain Parse() never sets this field
//     — it is populated only by LoadRecursive when it instantiates a
//     copy of a module's resources at each call site.
type Resource struct {
	Kind           string
	Type           string
	Name           string
	File           string
	Line           int
	Attrs          map[string]cty.Value
	AttrReasons    map[string]string
	DynamicBlocks  []string
	PreventDestroy bool
	ModulePath     []string
}

// ModuleCall is a single module block extracted from a Terraform configuration.
//
// Field contracts:
//   - Name is the local HCL name of the module, e.g. "foo" in `module "foo"`.
//   - Source is the literal value of the `source` attribute.
//   - SourceKind is the classified type of the source: "local", "registry",
//     "git", "archive", or "unknown".
//   - Args contains the arguments passed to the module. Expressions that
//     cannot be resolved statically result in cty.NilVal.
//   - File is the source file path.
//   - Line is the line number where the `module` block begins.
//   - ModulePath is the instantiation path from the root configuration to
//     this module call, expressed as the sequence of parent module names.
//     It is nil/empty for root-level module blocks.
type ModuleCall struct {
	Name       string
	Source     string
	SourceKind string
	Args       map[string]cty.Value
	File       string
	Line       int
	ModulePath []string
}

const (
	SourceLocal    = "local"
	SourceRegistry = "registry"
	SourceGit      = "git"
	SourceArchive  = "archive"
	SourceUnknown  = "unknown"
)

// classifySource determines the SourceKind from the source string based on
// standard Terraform module source patterns.
//
// Registry sources cover both:
//   - Public registry: <namespace>/<name>/<provider>           (3 parts)
//   - Private registry: <hostname>/<namespace>/<name>/<provider> (4 parts,
//     hostname identified by a "." in the first segment)
//
// The hostname dot test discriminates private-registry calls from a stray
// 4-segment local-style path like "a/b/c/d" (which has no hostname) — those
// fall through to SourceUnknown so downstream consumers do not silently
// treat unrelated strings as registry references.
func classifySource(source string) string {
	if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") {
		return SourceLocal
	}
	// Git check comes before archive/http because git sources can use
	// https:// prefixes. Detection is boundary-aware: a bare ".git"
	// substring would also match hostnames like
	// "registry.gitlab.example.com", so we only treat ".git" as a git
	// signal when it terminates the path component (suffix, "/", "?",
	// or "#") — the standard Terraform / go-getter conventions.
	if isGitSource(source) {
		return SourceGit
	}
	if strings.HasSuffix(source, ".zip") || strings.HasSuffix(source, ".tar.gz") || strings.HasSuffix(source, ".tar") ||
		strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return SourceArchive
	}
	// Registry sources may carry go-getter modifiers — a "//<subdir>"
	// suffix and/or a "?query"/"#fragment" — that the segment-count
	// below would otherwise mistake for extra path components. Strip
	// those before splitting. Order matters: this runs after the git
	// and archive checks, so ".git//" still classifies as git and
	// "http(s)://" still classifies as archive.
	registrySrc := source
	if i := strings.Index(registrySrc, "//"); i >= 0 {
		registrySrc = registrySrc[:i]
	}
	if i := strings.IndexAny(registrySrc, "?#"); i >= 0 {
		registrySrc = registrySrc[:i]
	}
	parts := strings.Split(registrySrc, "/")
	switch len(parts) {
	case 3:
		// Public Terraform registry: namespace/name/provider.
		return SourceRegistry
	case 4:
		// Private registry: hostname/namespace/name/provider. The
		// presence of a "." in the leading segment is what
		// distinguishes a hostname from an unrelated 4-segment path.
		if strings.Contains(parts[0], ".") {
			return SourceRegistry
		}
	}
	return SourceUnknown
}

// isGitSource reports whether source matches one of the recognised git
// module-source forms:
//   - "git::<url>"            (explicit forced-getter prefix)
//   - "git@host:path"         (SSH shorthand)
//   - "<...>.git"             (trailing repo suffix)
//   - "<...>.git//<subdir>"   (go-getter "//" subdir, splits to ".git/")
//   - "<...>.git?<query>"     (go-getter ?ref= etc.)
//   - "<...>.git#<fragment>"  (rare, but symmetric with "?")
//
// A bare strings.Contains(source, ".git") would misfire on hostnames like
// "registry.gitlab.example.com" or "app.gitsomething.io" — those segments
// embed ".git" but the next character is a letter, not a path boundary.
// Restricting matches to component boundaries keeps that class of
// host-qualified registry source classified as SourceRegistry rather than
// SourceGit.
func isGitSource(source string) bool {
	if strings.HasPrefix(source, "git::") || strings.HasPrefix(source, "git@") {
		return true
	}
	if strings.HasSuffix(source, ".git") {
		return true
	}
	for _, boundary := range []string{".git/", ".git?", ".git#"} {
		if strings.Contains(source, boundary) {
			return true
		}
	}
	return false
}

// topLevelSchema enumerates the top-level blocks Parse extracts. Anything
// not listed here falls through PartialContent's "remaining body" silently
// — that is how provider/terraform/output/variable/locals/module/check/
// moved/import/removed blocks are skipped without diagnostics. The
// `backend` block is not listed because it is never legal at top level
// (it only appears nested inside `terraform { backend "s3" {} }`); we
// skip it transitively by skipping `terraform`. New top-level block
// types added by future Terraform versions also fall through harmlessly,
// so this stage does not need to be revisited each Terraform release.
//
// PartialContent is preferred over Content here precisely because Content
// raises a diagnostic for any unrecognised block; that would force this
// schema to enumerate every Terraform top-level type and stay in sync
// with each release. The downside of PartialContent is that we lose the
// ability to flag typos like `resoruce` — but HCL's own parse-time
// validation catches structural malformation, and downstream stages are
// not harmed by an unknown top-level block at this layer.
var topLevelSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "resource", LabelNames: []string{"type", "name"}},
		{Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "module", LabelNames: []string{"name"}},
	},
}

// Parse reads each file, merges them into a single configuration, and
// returns its `resource`, `data`, and `module` blocks as structured values
// sorted by (File, Line). Other top-level block types are silently skipped
// (see topLevelSchema for the rationale).
//
// Returns:
//   - The Resource slice (sorted, deterministic).
//   - The ModuleCall slice (sorted, deterministic).
//   - hcl.Diagnostics carrying warning-severity entries only — currently
//     used by buildEvalContext to surface locals dependency cycles and by
//     extractModule to warn about non-local module sources.
//     Callers may log/format these but Parse itself never escalates a
//     warning to an error. The slice is nil when there is nothing to
//     report.
//   - error for hard failures only.
//
// Errors:
//   - I/O failures reading any file are wrapped via fmt.Errorf("read %s: %w", ...)
//     so callers can match against fs.ErrNotExist and friends.
//   - Parse-time HCL diagnostics are flattened to a single line of the
//     form "<file>:<line>: <message>" via formatDiag. Only the first
//     error-severity diagnostic is surfaced; warning-severity diagnostics
//     are ignored. The diagnostic's Summary (not Detail) is used so the
//     message stays single-line — Detail tends to span multiple lines and
//     "must not leak through" per the parser error contract.
//
// The empty-input case (nil or empty slice) returns (nil, nil, nil, nil)
// without touching the filesystem; the walker errors out earlier when a
// directory has no .tf files, so this code path is mostly defensive.
func Parse(files []string) ([]Resource, []ModuleCall, hcl.Diagnostics, error) {
	return parseConfig(files, nil)
}

// parseConfig is the override-aware sibling of Parse. It is called by
// LoadRecursive's buildModuleTemplate with the literal arguments
// propagated from a parent module call; Parse itself wraps it with a
// nil override map so the public API surface stays stable.
//
// The override map is threaded straight through to buildEvalContext —
// see that function for the priority rules (override > default >
// absent) and the undeclared-name filter.
func parseConfig(files []string, overrides map[string]cty.Value) ([]Resource, []ModuleCall, hcl.Diagnostics, error) {
	if len(files) == 0 {
		return nil, nil, nil, nil
	}

	parsed := make([]*hcl.File, 0, len(files))
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read %q: %w", path, err)
		}
		f, diags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, nil, nil, formatDiag(diags)
		}
		parsed = append(parsed, f)
	}

	// Build the static eval context BEFORE MergeFiles so buildEvalContext
	// can iterate per-file *hclsyntax.Body — MergeFiles returns an opaque
	// merged body that cannot be cast back, and we want per-file Subject
	// ranges on cycle warnings.
	evalCtx, evalDiags := buildEvalContext(parsed, overrides)

	// MergeFiles unifies the bodies into a single hcl.Body the schema-based
	// extractor can iterate once. The returned body is an internal
	// merged-body type that cannot be cast back to *hclsyntax.Body, so all
	// downstream traversal in this package is schema-based
	// (PartialContent). This is the trade-off for treating multi-file
	// configs uniformly rather than re-implementing the merge in every
	// consuming stage.
	mergedBody := hcl.MergeFiles(parsed)

	// PartialContent's second return is the "remaining body" of unmatched
	// blocks/attributes; we discard it because skipping unknown top-level
	// blocks (provider/terraform/output/...) is precisely the desired
	// behaviour. The Diagnostics return surfaces only schema-level errors
	// (e.g. wrong label count on a matched block), not unknown-block
	// warnings — those would have come from Content.
	content, _, diags := mergedBody.PartialContent(topLevelSchema)
	if diags.HasErrors() {
		return nil, nil, nil, formatDiag(diags)
	}

	resources := make([]Resource, 0, len(content.Blocks))
	modules := make([]ModuleCall, 0)
	for _, blk := range content.Blocks {
		if blk.Type == "module" {
			m, mDiags := extractModule(blk, evalCtx)
			evalDiags = append(evalDiags, mDiags...)
			modules = append(modules, m)
			continue
		}

		// The schema declares two labels for both registered block types,
		// so HCL guarantees Labels has length 2 by the time we get here;
		// no bounds check is needed.
		meta, metaDiags := evalMetaArgs(blk, evalCtx)
		// Diagnostics are surfaced regardless of keep/drop. In practice
		// evalMetaArgs only produces warnings on the keep path (a clean
		// `count = 0` is an answer, not an unknown), but appending
		// unconditionally keeps the call site honest if that contract
		// changes.
		evalDiags = append(evalDiags, metaDiags...)
		if !meta.keep {
			continue
		}
		attrs, reasons := extractAttrs(blk, evalCtx)
		resources = append(resources, Resource{
			Kind:           blk.Type,
			Type:           blk.Labels[0],
			Name:           blk.Labels[1],
			File:           blk.DefRange.Filename,
			Line:           blk.DefRange.Start.Line,
			Attrs:          attrs,
			AttrReasons:    reasons,
			DynamicBlocks:  meta.dynamicLabels,
			PreventDestroy: meta.preventDestroy,
		})
	}

	// Stable sort by (File, Line) so consumers and golden-file tests see
	// a deterministic order regardless of input ordering. Within a single
	// file HCL preserves source order; this comparator preserves that
	// (Line is monotonically increasing for blocks in one file) and only
	// reorders across files.
	sort.SliceStable(resources, func(i, j int) bool {
		if resources[i].File != resources[j].File {
			return resources[i].File < resources[j].File
		}
		return resources[i].Line < resources[j].Line
	})

	sort.SliceStable(modules, func(i, j int) bool {
		if modules[i].File != modules[j].File {
			return modules[i].File < modules[j].File
		}
		return modules[i].Line < modules[j].Line
	})

	return resources, modules, evalDiags, nil
}

// extractModule extracts a module block into a ModuleCall.
func extractModule(blk *hcl.Block, evalCtx *hcl.EvalContext) (ModuleCall, hcl.Diagnostics) {
	name := blk.Labels[0]
	// Module call sites do not surface AttrReasons — the reason map is
	// only consumed by the resolver for resource/data conditional
	// gating. Discard it here to keep ModuleCall.Args's contract narrow.
	attrs, _ := extractAttrs(blk, evalCtx)

	sourceVal := attrs["source"]
	source := ""
	if sourceVal != cty.NilVal && !sourceVal.IsNull() && sourceVal.Type().Equals(cty.String) {
		source = sourceVal.AsString()
	}
	// Remove source from Args as it is already captured in its own field.
	delete(attrs, "source")

	kind := classifySource(source)

	var diags hcl.Diagnostics
	if kind != SourceLocal {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "non-local module source",
			Detail:   fmt.Sprintf("module %q: tfperms does not parse non-local source %q", name, source),
			Subject:  &blk.DefRange,
		})
	}

	return ModuleCall{
		Name:       name,
		Source:     source,
		SourceKind: kind,
		Args:       attrs,
		File:       blk.DefRange.Filename,
		Line:       blk.DefRange.Start.Line,
	}, diags
}

// moduleCycleError is returned by buildModuleTemplate when a recursive
// module load re-enters a directory that is already being expanded up
// the call stack. The path field captures the cycle sequence — the
// portion of the recursion stack starting at the first occurrence of
// the offending directory, with that directory appended again so the
// closing edge is explicit. It is intercepted at the call site, where
// it surfaces as a warning diagnostic rather than a hard failure.
//
// Example shapes:
//   - Self-cycle (a module whose source resolves to its own directory):
//     path = ["/root", "/root"].
//   - Two-node cycle (root -> a -> root):
//     path = ["/root", "/root/a", "/root"].
//   - Three-node cycle (root -> a -> b -> root):
//     path = ["/root", "/root/a", "/root/a/b", "/root"].
//
// The first and last entries are always equal — that equality is what
// makes a cycle a cycle. Callers that render the path do not need to
// special-case the duplicate; rendering as "A -> B -> A" already shows
// the closing edge naturally.
type moduleCycleError struct {
	path []string
}

func (e *moduleCycleError) Error() string {
	return "module recursion cycle: " + strings.Join(e.path, " -> ")
}

// moduleTemplate is the parsed-and-recursively-expanded "blueprint" of
// a single Terraform module directory. Cached entries in LoadRecursive
// store the result as if the module were the root config — root-level
// resources have an empty ModulePath, resources from a nested
// `module "x"` block carry ModulePath = ["x", ...]. Each call site
// instantiates the template by prepending the caller's module name to
// every resource's ModulePath.
type moduleTemplate struct {
	resources []Resource
	modules   []ModuleCall
	diags     hcl.Diagnostics
}

// LoadRecursive parses dir as a Terraform configuration, then walks
// every `module "name" { source = "./..." }` call site whose source is
// classified as SourceLocal, returning the flat union of every
// resource encountered tagged with the chain of module names from the
// root down to the resource.
//
// Resources are duplicated for each call site of a module: if
// `module "x"` and `module "y"` both reference `./mod`, the resources
// inside `./mod` appear twice in the result, once with
// ModulePath = ["x", ...] and once with ModulePath = ["y", ...].
//
// Returns:
//   - []Resource: the flattened recursive resource set described above.
//     Order is determined by a depth-first walk: root resources first
//     (in Parse's File:Line order), then for each local module call in
//     source order, the recursively-loaded resources of that module.
//   - []ModuleCall: every module block encountered during the walk,
//     deduped by instantiation path and (File, Line) source position.
//     The result is deterministically sorted by File, Line, and path.
//   - hcl.Diagnostics: warning-severity entries from Parse plus
//     warnings emitted at call sites that could not be loaded — missing
//     directory, no .tf files, parse failure inside the child module,
//     or a recursion cycle. Non-local module sources continue to
//     produce the same "non-local module source" warning extractModule
//     emits in plain Parse; LoadRecursive does not attempt to fetch
//     them.
//   - error: hard failure on the root directory only — a missing /
//     unreadable root, an HCL parse error in a root file, or
//     filepath.Abs failure on the root path. Failures inside nested
//     modules surface as warning diagnostics so a single broken module
//     does not abort the whole configuration.
//
// Local module sources are resolved relative to the file declaring the
// `module` block (filepath.Dir(m.File) + m.Source), not the process
// CWD or the root directory — this matches Terraform's own module
// resolution semantics for relative sources.
//
// dir must be a non-empty path. An empty (or all-whitespace) dir is
// rejected with an error rather than being silently resolved to the
// process working directory via filepath.Abs(""), which would make
// behaviour depend on ambient CWD state and is almost always a caller
// bug. Pass "." explicitly if the current directory is genuinely
// intended.
func LoadRecursive(dir string) ([]Resource, []ModuleCall, hcl.Diagnostics, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil, nil, fmt.Errorf("LoadRecursive: dir is empty; pass %q explicitly to load the current directory", ".")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("abs %q: %w", dir, err)
	}
	cache := map[string]*moduleTemplate{}
	// Root configurations have no propagated arguments; overrides start
	// at nil and only become non-empty when buildModuleTemplate recurses
	// into a `module` call site whose Args resolved to literal values.
	// The recursion stack starts empty; buildModuleTemplate appends
	// absDir to it before walking into nested module calls.
	template, walkErr, parseErr := buildModuleTemplate(absDir, nil, cache, nil)
	if walkErr != nil {
		return nil, nil, nil, walkErr
	}
	if parseErr != nil {
		return nil, nil, nil, parseErr
	}
	return template.resources, template.modules, template.diags, nil
}

// buildModuleTemplate parses absDir, recursively expands every local
// `module` call site, and returns a *moduleTemplate whose resources
// carry the ModulePath chain rooted *at absDir*. Callers prepend their
// own module name to each resource's ModulePath when instantiating the
// returned template at a parent call site.
//
// overrides carries the literal arguments propagated from the parent
// module call (or nil at the root). They flow through parseConfig into
// buildEvalContext so the child's `count = var.X ? 1 : 0`,
// `for_each = var.Y`, attribute references, and the resolution of its
// own nested `module` blocks' arguments all see the parent-supplied
// values.
//
// The function returns three values to disambiguate the failure modes:
//   - (template, nil, nil) — success.
//   - (nil, walkErr, nil)  — the walker rejected absDir (missing dir,
//     no .tf files, permission denied). Callers above the root translate
//     this to a warning; LoadRecursive lets it bubble out as a hard error
//     for the root.
//   - (nil, nil, parseErr) — Parse rejected one of the .tf files inside
//     absDir. Same treatment as walkErr at child / root levels.
//
// Cycle detection: re-entering a directory currently up the recursion
// stack returns (nil, nil, *moduleCycleError) carrying the full path
// sequence — the slice of stack entries from the first occurrence of
// absDir through the bottom of the stack, with absDir appended again
// so the closing edge is visible (e.g. ["/root", "/root/a", "/root"]).
// The cycle key is the directory path alone — recursing back into a
// dir with different overrides is still a cycle from a static-
// evaluation standpoint, and pretending otherwise would let
// pathological fixtures recurse indefinitely as long as some argument
// keeps changing.
//
// The stack is passed by value via a full-slice expression
// (stack[:len:len]) when descending, so sibling branches each see the
// stack that prevailed at this level — no shared backing array means a
// later sibling cannot observe a directory left over from an earlier
// sibling's recursion. This is what keeps diamond dependency graphs
// (root -> a -> c, root -> b -> c) from being misclassified as cycles.
//
// The cache, however, is keyed by (absDir, overrides) via
// buildCacheKey: the same module instantiated with two different
// argument sets must produce two distinct templates so that a
// `count = var.enabled ? 1 : 0` block resolves to different keep/drop
// outcomes per call site.
func buildModuleTemplate(absDir string, overrides map[string]cty.Value, cache map[string]*moduleTemplate, stack []string) (*moduleTemplate, error, error) {
	cacheKey := buildCacheKey(absDir, overrides)
	if t, ok := cache[cacheKey]; ok {
		return t, nil, nil
	}
	for i, p := range stack {
		if p == absDir {
			cycle := make([]string, 0, len(stack)-i+1)
			cycle = append(cycle, stack[i:]...)
			cycle = append(cycle, absDir)
			return nil, nil, &moduleCycleError{path: cycle}
		}
	}
	// Full-slice expression caps the new slice's capacity at its
	// length so the append always allocates a fresh backing array.
	// Without this, two sibling module calls at the same recursion
	// depth could each append into the parent's spare capacity and
	// silently overwrite each other's stack entry.
	childStack := append(stack[:len(stack):len(stack)], absDir)

	files, walkErr := FindTerraformFiles(absDir)
	if walkErr != nil {
		return nil, walkErr, nil
	}
	resources, modules, diags, parseErr := parseConfig(files, overrides)
	if parseErr != nil {
		return nil, nil, parseErr
	}

	// Start the template with this dir's own resources/modules/diags.
	// Modules and diagnostics are copied into fresh slices so later
	// appends from child templates do not splice into the slice
	// returned by Parse (paranoia — Parse already returns fresh
	// slices, but the cache shares the *moduleTemplate across call
	// sites and we want to be defensive against future code that
	// mutates the cached entries).
	tmpl := &moduleTemplate{
		resources: resources,
		modules:   append([]ModuleCall(nil), modules...),
		diags:     append(hcl.Diagnostics(nil), diags...),
	}

	// Track which (ModulePath, File, Line) source positions we have
	// already emitted a ModuleCall for, so cache reuse across
	// multiple call sites of the same nested module does not produce
	// duplicate entries in the final modules slice.
	seenSites := make(map[string]bool, len(modules))
	for _, m := range modules {
		site := fmt.Sprintf("%s:%s:%d", strings.Join(m.ModulePath, "/"), m.File, m.Line)
		seenSites[site] = true
	}

	for _, m := range modules {
		if m.SourceKind != SourceLocal {
			continue
		}
		// Resolve the local source relative to the file containing
		// the module block, NOT the process CWD or the root dir.
		// Terraform's own module resolver works the same way, so a
		// relative source like "../shared" is rooted at the
		// declaring file's directory.
		baseDir := filepath.Dir(m.File)
		childAbs, absErr := filepath.Abs(filepath.Join(baseDir, m.Source))
		if absErr != nil {
			tmpl.diags = append(tmpl.diags, moduleLoadWarning(m, fmt.Errorf("resolve path: %w", absErr)))
			continue
		}
		// Filter the call-site arguments down to entries that resolved
		// to a known cty.Value. Unresolved args (cty.NilVal — e.g. an
		// expression that references something this stage cannot
		// evaluate) cannot be folded into the child's eval context and
		// would surface as an unknown anyway; dropping them keeps the
		// override map a strict literal-only set.
		childOverrides := literalOverrides(m.Args)
		childTmpl, childWalkErr, childParseErr := buildModuleTemplate(childAbs, childOverrides, cache, childStack)
		var cycleErr *moduleCycleError
		switch {
		case errors.As(childParseErr, &cycleErr):
			tmpl.diags = append(tmpl.diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  "module recursion cycle",
				Detail: fmt.Sprintf(
					"module %q at %s would re-enter a directory already being expanded; skipping. cycle: %s",
					m.Name, m.Source, strings.Join(cycleErr.path, " -> "),
				),
				Subject: moduleSubject(m),
			})
			continue
		case childWalkErr != nil:
			tmpl.diags = append(tmpl.diags, moduleLoadWarning(m, childWalkErr))
			continue
		case childParseErr != nil:
			tmpl.diags = append(tmpl.diags, moduleLoadWarning(m, childParseErr))
			continue
		}

		// Instantiate the child template at this call site by deep-
		// copying each resource and prepending m.Name to its
		// ModulePath. The deep copy is mandatory: cached templates
		// are shared between call sites, so reusing the same slice
		// would let a later instantiation mutate an earlier one's
		// path.
		for _, r := range childTmpl.resources {
			instanced := r
			newPath := make([]string, 0, len(r.ModulePath)+1)
			newPath = append(newPath, m.Name)
			newPath = append(newPath, r.ModulePath...)
			instanced.ModulePath = newPath
			tmpl.resources = append(tmpl.resources, instanced)
		}

		// Pull up the child's module call sites (deduped) and
		// diagnostics so the root caller sees the union of every
		// level's reporting.
		for _, cm := range childTmpl.modules {
			instanced := cm
			newPath := make([]string, 0, len(cm.ModulePath)+1)
			newPath = append(newPath, m.Name)
			newPath = append(newPath, cm.ModulePath...)
			instanced.ModulePath = newPath

			site := fmt.Sprintf("%s:%s:%d", strings.Join(instanced.ModulePath, "/"), instanced.File, instanced.Line)
			if seenSites[site] {
				continue
			}
			seenSites[site] = true
			tmpl.modules = append(tmpl.modules, instanced)
		}
		tmpl.diags = append(tmpl.diags, childTmpl.diags...)
	}

	// Maintain deterministic output sorted by File, then Line, then
	// ModulePath (joined as the instantiation path).
	decorated := make([]struct {
		mc      ModuleCall
		pathKey string
	}, len(tmpl.modules))
	for i, mc := range tmpl.modules {
		decorated[i].mc = mc
		decorated[i].pathKey = strings.Join(mc.ModulePath, "/")
	}
	sort.SliceStable(decorated, func(i, j int) bool {
		if decorated[i].mc.File != decorated[j].mc.File {
			return decorated[i].mc.File < decorated[j].mc.File
		}
		if decorated[i].mc.Line != decorated[j].mc.Line {
			return decorated[i].mc.Line < decorated[j].mc.Line
		}
		return decorated[i].pathKey < decorated[j].pathKey
	})
	for i := range decorated {
		tmpl.modules[i] = decorated[i].mc
	}

	cache[cacheKey] = tmpl
	return tmpl, nil, nil
}

// buildCacheKey returns the moduleTemplate cache key for (absDir,
// overrides). When overrides is empty, the key is just absDir — the
// pre-arg-propagation behaviour, so existing callers see no extra
// allocation. Non-empty override sets are appended in sorted-name
// order with each value rendered via cty.Value.GoString(), which is
// deterministic for the literal scalars we propagate (strings, numbers,
// bools, and tuples/objects of the same).
//
// Why include overrides in the key at all: a `count = var.enabled ?
// 1 : 0` block inside the same module directory must produce a "kept"
// resource for one call site (enabled = true) and a dropped resource
// for another (enabled = false). Reusing a cached template across
// distinct argument sets would collapse those into one outcome.
//
// Why length-prefix each component when overrides are present: raw
// concatenation with `|` and `=` separators is ambiguous because a
// directory path on POSIX may legally contain those bytes. Without
// length prefixes, ".../mod|enabled=cty.True" with no overrides would
// produce the same key as ".../mod" plus an `enabled = cty.True`
// override, and the loader would hand back the wrong cached template.
// Length-prefixing every component makes the encoding unambiguous.
func buildCacheKey(absDir string, overrides map[string]cty.Value) string {
	if len(overrides) == 0 {
		return absDir
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d:%s", len(absDir), absDir)
	for _, k := range keys {
		vStr := overrides[k].GoString()
		fmt.Fprintf(&b, "|%d:%s=%d:%s", len(k), k, len(vStr), vStr)
	}
	return b.String()
}

// literalOverrides filters a module call's resolved Args down to the
// subset that can be propagated as overrides — i.e. entries whose
// cty.Value is non-Nil and known. Unknown values (cty.NilVal, an
// IsKnown() == false placeholder) cannot meaningfully override a
// child's variable resolution and are dropped.
//
// Returns nil when the resulting set is empty so buildCacheKey's
// "no overrides → bare absDir key" fast path stays active.
func literalOverrides(args map[string]cty.Value) map[string]cty.Value {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]cty.Value, len(args))
	for k, v := range args {
		if v == cty.NilVal {
			continue
		}
		if !v.IsKnown() {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// moduleSubject returns a pointer to a synthetic hcl.Range pinned to
// the module block's declaration site, suitable for the Subject field
// of a hcl.Diagnostic. Pulling this out of the call sites keeps the
// Diagnostic literals readable.
func moduleSubject(m ModuleCall) *hcl.Range {
	return &hcl.Range{
		Filename: m.File,
		Start:    hcl.Pos{Line: m.Line, Column: 1},
		End:      hcl.Pos{Line: m.Line, Column: 1},
	}
}

// moduleLoadWarning packages a child-load failure into the standard
// "could not load local module" warning diagnostic. The Detail string
// is single-line so callers that flatten diagnostics for terminal
// output do not have to wrap.
func moduleLoadWarning(m ModuleCall, err error) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  "could not load local module",
		Detail:   fmt.Sprintf("module %q at %s: %v", m.Name, m.Source, err),
		Subject:  moduleSubject(m),
	}
}

// formatDiag flattens HCL diagnostics into a single-line
// "<file>:<line>: <summary>" error.
//
// Only the first error-severity diagnostic is reported; warning-severity
// diagnostics are skipped because they would never be the cause of a
// HasErrors() check returning true and are not actionable in a single-line
// error contract. If the first error has no Subject (rare; happens for
// file-level synthetic diagnostics), we synthesize "<unknown>:0".
//
// Summary is used in preference to Detail because Summary is one-line by
// convention while Detail commonly contains multiple lines including code
// snippets — those would violate the single-line error contract that the
// CLI reporter and tests assert.
func formatDiag(diags hcl.Diagnostics) error {
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		file, line := "<unknown>", 0
		if d.Subject != nil {
			file = d.Subject.Filename
			line = d.Subject.Start.Line
		}
		return fmt.Errorf("%s:%d: %s", file, line, d.Summary)
	}
	// Defensive: HasErrors() returned true but the loop found no
	// error-severity entry. This should be unreachable, but emitting a
	// generic message is safer than returning nil and pretending the
	// parse succeeded.
	return fmt.Errorf("hcl: unknown diagnostic error")
}
