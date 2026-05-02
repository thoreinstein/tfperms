package parser

// Attribute extraction for resource/data blocks. Companion to parse.go.
//
// extractAttrs walks the top-level attributes of a parsed block body and
// returns a map keyed by attribute name. Resolution is best-effort and
// lazy: literal scalars resolve to typed cty.Value, var.X / local.X
// references resolve through the supplied *hcl.EvalContext when present,
// and everything else (function calls, interpolations referencing
// unknowns, cross-resource refs) yields cty.NilVal — the key is still
// present in the map.
//
// Meta-arguments listed in metaArgs are skipped. Story .7 owns count /
// for_each / depends_on / provider routing; they are dropped here so
// they do not leak into Attrs before .7 lands. If .7 later needs to
// read them it can re-walk the body.
//
// Nested blocks (lifecycle { ... }, dynamic { ... }, provisioner { ... },
// etc.) are excluded structurally: hclsyntax.Body keeps Attributes and
// Blocks in separate fields, and we only iterate Attributes.

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// metaArgs lists attribute names that Terraform treats as meta-arguments
// on resource and data blocks. Story .5 owns argument extraction; meta-
// argument routing belongs to story .7. Until that lands we skip these
// so they do not appear in Resource.Attrs.
//
// `count` and `for_each` are not strictly named in story .5's spec but
// are meta-arguments per Terraform's documentation; including them here
// avoids leaking them into Attrs prematurely.
var metaArgs = map[string]struct{}{
	"provider":   {},
	"depends_on": {},
	"count":      {},
	"for_each":   {},
}

// extractAttrs returns the top-level attributes of blk as a non-nil map
// keyed by attribute name.
//
// Resolution rules:
//   - If attr.Expr.Value(evalCtx) returns an error-severity diagnostic,
//     the value is cty.NilVal but the key is still inserted.
//   - If the resolved cty.Value is unknown (IsKnown() == false), the
//     value is cty.NilVal. A *known* null (e.g. cty.NullVal(cty.String)
//     from a literal `null`) is preserved as-is — that is a resolved
//     literal, not an unresolved expression.
//   - Names in metaArgs are skipped entirely.
//
// evalCtx may be nil; hclsyntax tolerates a nil context for fully literal
// expressions but yields diagnostics for any traversal, which this
// function maps to cty.NilVal as documented above.
//
// Defensive fall-through: if blk.Body is not a *hclsyntax.Body (which is
// theoretically possible for bodies returned through hcl.MergeFiles +
// PartialContent but does not occur in practice on parser-produced
// blocks), we return an empty non-nil map rather than panicking.
func extractAttrs(blk *hcl.Block, evalCtx *hcl.EvalContext) map[string]cty.Value {
	out := make(map[string]cty.Value)
	body, ok := blk.Body.(*hclsyntax.Body)
	if !ok {
		return out
	}
	for name, attr := range body.Attributes {
		if _, skip := metaArgs[name]; skip {
			continue
		}
		out[name] = resolveExpr(attr.Expr, evalCtx)
	}
	return out
}

// resolveExpr evaluates expr against evalCtx and collapses any failure
// (diagnostics or unknown value) to cty.NilVal. See extractAttrs's doc
// comment for the contract.
func resolveExpr(expr hcl.Expression, evalCtx *hcl.EvalContext) cty.Value {
	val, diags := expr.Value(evalCtx)
	if diags.HasErrors() {
		return cty.NilVal
	}
	if !val.IsKnown() {
		return cty.NilVal
	}
	return val
}
