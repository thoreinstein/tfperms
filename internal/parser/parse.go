package parser

// HCL parsing layer. Companion to walker.go: walker produces the file list,
// Parse turns those files into structured Resource values that downstream
// stages (.5 attribute extraction, .6 variable/locals expansion, Epic 3
// module resolution, Epic 5 dedup) iterate over.

import (
	"fmt"
	"os"
	"sort"

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
//     arguments (provider, depends_on, count, for_each — the latter two
//     belong to story .7's count/for_each routing). Nested blocks
//     (lifecycle, dynamic, provisioner, ...) do not contribute keys.
//     Each value is either a fully-resolved cty.Value (literals;
//     var.X / local.X resolved through the eval context) or cty.NilVal
//     when the right-hand side could not be evaluated at this stage
//     (function calls, interpolations referencing unknowns, cross-
//     resource references, missing variables/locals). Until story .6
//     wires a populated *hcl.EvalContext into Parse, var.X and local.X
//     references resolve to cty.NilVal end-to-end; literal-only blocks
//     are unaffected. Callers that need richer resolution should treat
//     cty.NilVal as "deferred / unknown" rather than "absent".
type Resource struct {
	Kind  string
	Type  string
	Name  string
	File  string
	Line  int
	Attrs map[string]cty.Value
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
	},
}

// Parse reads each file, merges them into a single configuration, and
// returns its `resource` and `data` blocks as Resource values sorted by
// (File, Line). Other top-level block types are silently skipped (see
// topLevelSchema for the rationale).
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
// The empty-input case (nil or empty slice) returns (nil, nil) without
// touching the filesystem; the walker errors out earlier when a directory
// has no .tf files, so this code path is mostly defensive.
func Parse(files []string) ([]Resource, error) {
	if len(files) == 0 {
		return nil, nil
	}

	parsed := make([]*hcl.File, 0, len(files))
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", path, err)
		}
		f, diags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, formatDiag(diags)
		}
		parsed = append(parsed, f)
	}

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
		return nil, formatDiag(diags)
	}

	// evalCtx is the context attribute resolution evaluates against. Story
	// .5 deliberately leaves Variables empty: literals resolve fine
	// without it, and var.X / local.X collapse to cty.NilVal — exactly
	// the lazy-resolution contract documented on Resource.Attrs. Story .6
	// will populate Variables (and possibly Functions) here without any
	// further surgery to extractAttrs.
	evalCtx := &hcl.EvalContext{Variables: map[string]cty.Value{}}

	out := make([]Resource, 0, len(content.Blocks))
	for _, blk := range content.Blocks {
		// The schema declares two labels for both registered block types,
		// so HCL guarantees Labels has length 2 by the time we get here;
		// no bounds check is needed.
		out = append(out, Resource{
			Kind:  blk.Type,
			Type:  blk.Labels[0],
			Name:  blk.Labels[1],
			File:  blk.DefRange.Filename,
			Line:  blk.DefRange.Start.Line,
			Attrs: extractAttrs(blk, evalCtx),
		})
	}

	// Stable sort by (File, Line) so consumers and golden-file tests see
	// a deterministic order regardless of input ordering. Within a single
	// file HCL preserves source order; this comparator preserves that
	// (Line is monotonically increasing for blocks in one file) and only
	// reorders across files.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})

	return out, nil
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
