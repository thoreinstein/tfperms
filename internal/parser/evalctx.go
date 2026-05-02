package parser

// Static evaluation context construction. Companion to parse.go.
//
// buildEvalContext walks the parsed *hcl.File slice produced by Parse,
// collects every `variable "name" {}` and `locals {}` block, and returns a
// populated *hcl.EvalContext suitable for resolving `var.X` and `local.X`
// references in resource/data attribute expressions.
//
// Phase 1 (this file) only handles literal-only resolution: variable
// defaults and locals expressions are each evaluated against a nil
// context, so literals succeed and any reference (var.X / local.X) or
// function call fails and is absent from the resulting context. Later
// phases extend this with iterative resolution and cycle detection.
//
// Type fidelity is *not* preserved: a `variable "x" { type = number;
// default = "5" }` block is captured as cty.StringVal("5"), not coerced
// to a number. tfperms downstream rarely consumes attribute values for
// arithmetic, so this is acceptable; callers needing coercion must do
// it themselves.

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// evalCtxSchema enumerates the top-level blocks buildEvalContext extracts
// from each parsed file. PartialContent is used so other top-level block
// types (resource/data/provider/...) fall through silently — the same
// rationale as topLevelSchema in parse.go.
var evalCtxSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "variable", LabelNames: []string{"name"}},
		{Type: "locals"},
	},
}

// variableSchema is a sub-schema applied to each `variable` block's body
// to extract just the `default` attribute. PartialContent silently skips
// `type`, `description`, `validation`, `sensitive`, `nullable`, and any
// other variable meta-attributes — same rationale as evalCtxSchema.
var variableSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "default"},
	},
}

// localDecl bundles a local's source expression with its declaration
// range so cycle diagnostics (added in phase 3) can carry a useful
// Subject. The first declaration of a given name wins (deterministic
// via file iteration order, which Parse controls); subsequent
// redeclarations are silently dropped to stay best-effort.
type localDecl struct {
	expr hcl.Expression
	rng  hcl.Range
}

// buildEvalContext walks files, collects variable defaults and locals
// bindings, and returns an *hcl.EvalContext populated with `var` and
// `local` object values plus any warning-severity diagnostics. The
// returned context is always non-nil; the diagnostics slice may be
// empty (and is always empty in this phase).
func buildEvalContext(files []*hcl.File) (*hcl.EvalContext, hcl.Diagnostics) {
	varExprs, localDecls := collectDecls(files)

	// Resolve variable defaults against a nil context. Literals succeed;
	// anything referencing functions or other names fails and is absent.
	varValues := make(map[string]cty.Value, len(varExprs))
	for name, expr := range varExprs {
		val, diags := expr.Value(nil)
		if diags.HasErrors() || !val.IsKnown() {
			continue
		}
		varValues[name] = val
	}

	// Resolve locals against a nil context too. Phase 1 only catches
	// literal locals; phase 2 extends this with iterative resolution
	// against the populated context.
	localValues := make(map[string]cty.Value, len(localDecls))
	for name, d := range localDecls {
		val, diags := d.expr.Value(nil)
		if diags.HasErrors() || !val.IsKnown() {
			continue
		}
		localValues[name] = val
	}

	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{}}
	// cty.ObjectVal panics on an empty map, so guard with len(...) > 0.
	if len(varValues) > 0 {
		ctx.Variables["var"] = cty.ObjectVal(varValues)
	}
	if len(localValues) > 0 {
		ctx.Variables["local"] = cty.ObjectVal(localValues)
	}
	return ctx, nil
}

// collectDecls walks every parsed file's *hclsyntax.Body and extracts
// variable defaults and locals bindings. Files whose bodies are not
// *hclsyntax.Body (theoretically possible through some HCL extensions
// but not produced by ParseConfig) are skipped defensively.
//
// First-declaration-wins semantics: if a name is declared in multiple
// files (a variable redeclaration, or two `locals { x = ... }` blocks
// across files), the first occurrence in file iteration order is kept.
// Terraform itself errors on duplicates; we silently keep the first to
// stay best-effort and not duplicate Terraform's validation here.
func collectDecls(files []*hcl.File) (map[string]hcl.Expression, map[string]localDecl) {
	varExprs := make(map[string]hcl.Expression)
	localDecls := make(map[string]localDecl)

	for _, f := range files {
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		// PartialContent's diagnostics here would surface things like
		// wrong label counts on `variable` blocks. The rest of Parse
		// has already validated structural HCL via ParseConfig; if a
		// label count is wrong the rest of the pipeline will likely
		// fail elsewhere too. We skip diagnostics rather than surface
		// them — the contract for buildEvalContext is "warnings only".
		content, _, _ := body.PartialContent(evalCtxSchema)

		for _, blk := range content.Blocks {
			switch blk.Type {
			case "variable":
				name := blk.Labels[0]
				if _, exists := varExprs[name]; exists {
					continue
				}
				varBody, ok := blk.Body.(*hclsyntax.Body)
				if !ok {
					continue
				}
				varContent, _, _ := varBody.PartialContent(variableSchema)
				if attr, ok := varContent.Attributes["default"]; ok {
					varExprs[name] = attr.Expr
				}
			case "locals":
				localsBody, ok := blk.Body.(*hclsyntax.Body)
				if !ok {
					continue
				}
				attrs, _ := localsBody.JustAttributes()
				// JustAttributes returns the attributes regardless of
				// diagnostics; diagnostics here would flag nested blocks
				// inside `locals { }` which is not valid Terraform anyway.
				for name, attr := range attrs {
					if _, exists := localDecls[name]; exists {
						continue
					}
					localDecls[name] = localDecl{expr: attr.Expr, rng: attr.Range}
				}
			}
		}
	}

	return varExprs, localDecls
}
