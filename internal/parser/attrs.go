package parser

// Attribute extraction for resource/data blocks. Companion to parse.go.
//
// extractAttrs walks the top-level attributes of a parsed block body and
// returns two parallel maps keyed by attribute name:
//
//   - Attrs:   resolved cty.Value or cty.NilVal per the rules below.
//   - Reasons: a stable classification string for each attribute whose
//     Attrs entry is cty.NilVal. Resolved attributes do not appear in
//     the reasons map. Callers reading "is this attribute unresolved?"
//     can use either `attrs[k] == cty.NilVal` or `_, ok := reasons[k]`
//     interchangeably; both views are populated together.
//
// Resolution is best-effort and lazy: literal scalars resolve to typed
// cty.Value, var.X / local.X references resolve through the supplied
// *hcl.EvalContext when present, and everything else (function calls,
// interpolations referencing unknowns, cross-resource refs) yields
// cty.NilVal — the key is still present in the Attrs map and gets a
// classified Reasons entry.
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

// Stable classification strings for unresolved-attribute reasons. These
// strings are part of the public API: they propagate through
// resolver.UnresolvedConditional.Reason into Result JSON output so
// downstream consumers (CLI, future automation) can branch on a known
// vocabulary. Adding a new value is forward-compatible; renaming an
// existing one is a breaking change.
//
// Priority hierarchy when an expression contains multiple unresolved
// reference kinds (e.g. `"${var.x}-${upper(data.a.b)}"`):
//
//	ReasonFunctionCall > ReasonDataSource > ReasonMissingVariable > ReasonOther
//
// The hierarchy is enforced by classifyReason and is documented here so
// callers reading downstream JSON do not have to reverse-engineer the
// ordering from observed output.
const (
	ReasonFunctionCall    = "function_call"
	ReasonDataSource      = "data_source"
	ReasonMissingVariable = "missing_variable"
	ReasonOther           = "other"
)

// metaArgs lists attribute names that Terraform treats as meta-arguments
// on resource and data blocks. They are dropped here so they do not
// appear in Resource.Attrs.
//
// `count` and `for_each` are owned by metaargs.go (evalMetaArgs); that
// routine reads them directly off the block body and decides whether
// to keep, drop, or warn on the resource. A future refactor that
// re-routes them through Attrs must coordinate with metaargs.go to
// avoid double-emitting the same expression.
var metaArgs = map[string]struct{}{
	"provider":   {},
	"depends_on": {},
	"count":      {},
	"for_each":   {},
}

// extractAttrs returns the top-level attributes of blk as two non-nil
// maps keyed by attribute name: the resolved Attrs map and the parallel
// Reasons map (populated only for entries that resolved to cty.NilVal).
//
// Resolution rules:
//   - If attr.Expr.Value(evalCtx) returns an error-severity diagnostic,
//     the value is cty.NilVal and a reason is recorded.
//   - If the resolved cty.Value is unknown (IsKnown() == false), the
//     value is cty.NilVal and a reason is recorded. A *known* null (e.g.
//     cty.NullVal(cty.String) from a literal `null`) is preserved as-is
//     — that is a resolved literal, not an unresolved expression.
//   - Names in metaArgs are skipped entirely.
//
// evalCtx may be nil; hclsyntax tolerates a nil context for fully literal
// expressions but yields diagnostics for any traversal, which this
// function maps to cty.NilVal as documented above.
//
// Defensive fall-through: if blk.Body is not a *hclsyntax.Body (which is
// theoretically possible for bodies returned through hcl.MergeFiles +
// PartialContent but does not occur in practice on parser-produced
// blocks), we return two empty non-nil maps rather than panicking.
func extractAttrs(blk *hcl.Block, evalCtx *hcl.EvalContext) (map[string]cty.Value, map[string]string) {
	attrs := make(map[string]cty.Value)
	reasons := make(map[string]string)
	body, ok := blk.Body.(*hclsyntax.Body)
	if !ok {
		return attrs, reasons
	}
	for name, attr := range body.Attributes {
		if _, skip := metaArgs[name]; skip {
			continue
		}
		val := resolveExpr(attr.Expr, evalCtx)
		attrs[name] = val
		if val == cty.NilVal {
			reasons[name] = classifyReason(attr.Expr, evalCtx)
		}
	}
	return attrs, reasons
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

// classifyReason inspects an unresolved expression and returns the most
// informative classification string from the priority hierarchy:
//
//	ReasonFunctionCall > ReasonDataSource > ReasonMissingVariable > ReasonOther
//
// Function calls win because tfperms cannot evaluate functions at
// static-analysis time at all — the user cannot fix this by adding a
// variable default, only by removing the function call from the gating
// expression or rewriting the catalog conditional. Data-source
// references are next because they require provider API calls that
// are not available at static-analysis time. Missing variables are
// the most user-actionable case ("provide a default for `var.X`").
// Anything else (cross-resource references, traversals into resolved
// objects that landed on missing keys, complex constructs) falls
// through to ReasonOther.
//
// This function is only meaningful for expressions that already
// failed to resolve (resolveExpr returned cty.NilVal); callers should
// not invoke it for resolved expressions. It still returns a string
// in that case but the value is not contractual.
//
// evalCtx may be nil; the missing-variable check treats every var.*
// reference as missing in that case, which matches the resolver
// behaviour (a nil eval context cannot resolve any variable).
func classifyReason(expr hcl.Expression, evalCtx *hcl.EvalContext) string {
	// Function-call detection requires walking the expression AST; the
	// hcl.Expression interface does not expose function calls through
	// Variables(). The hclsyntax.Walk helper iterates every node in
	// the parsed expression tree.
	if syntaxExpr, ok := expr.(hclsyntax.Expression); ok {
		if containsFunctionCall(syntaxExpr) {
			return ReasonFunctionCall
		}
	}

	// Variables() returns the traversals referenced by the expression.
	// Each traversal starts with a hcl.TraverseRoot whose Name is the
	// top-level identifier (var, local, data, or a resource type).
	hasDataSource := false
	hasMissingVar := false
	for _, traversal := range expr.Variables() {
		if len(traversal) == 0 {
			continue
		}
		root, ok := traversal[0].(hcl.TraverseRoot)
		if !ok {
			continue
		}
		switch root.Name {
		case "data":
			hasDataSource = true
		case "var":
			if isMissingTraversal(traversal, evalCtx, "var") {
				hasMissingVar = true
			}
		}
	}

	if hasDataSource {
		return ReasonDataSource
	}
	if hasMissingVar {
		return ReasonMissingVariable
	}
	return ReasonOther
}

// containsFunctionCall walks expr's AST and reports whether any node is
// a function-call expression. Wraps hclsyntax.Walk with a tiny visitor
// type — using Walk (rather than a manual recursive switch) keeps the
// check correct as new hclsyntax expression types are introduced.
func containsFunctionCall(expr hclsyntax.Expression) bool {
	v := &funcCallVisitor{}
	// Walk returns hcl.Diagnostics which are unused here — the walker
	// does not produce diagnostics, the visitor only inspects nodes.
	_ = hclsyntax.Walk(expr, v)
	return v.found
}

type funcCallVisitor struct {
	found bool
}

func (v *funcCallVisitor) Enter(node hclsyntax.Node) hcl.Diagnostics {
	if _, ok := node.(*hclsyntax.FunctionCallExpr); ok {
		v.found = true
	}
	return nil
}

func (v *funcCallVisitor) Exit(_ hclsyntax.Node) hcl.Diagnostics { return nil }

// isMissingTraversal reports whether a `var.X` (or analogous root.X)
// traversal references a name absent from evalCtx.Variables[root].
//
// A traversal of length < 2 cannot be classified — `var` on its own is
// a syntax error caught by hcl earlier — and returns false rather than
// guessing.
//
// A nil evalCtx, a missing root object, or a non-object value at the
// root all collapse to "missing" because none of those states can
// resolve any name under the root.
func isMissingTraversal(traversal hcl.Traversal, evalCtx *hcl.EvalContext, root string) bool {
	if len(traversal) < 2 {
		return false
	}
	attr, ok := traversal[1].(hcl.TraverseAttr)
	if !ok {
		return false
	}
	if evalCtx == nil {
		return true
	}
	rootVal, ok := evalCtx.Variables[root]
	if !ok {
		return true
	}
	if rootVal.IsNull() || !rootVal.Type().IsObjectType() {
		return true
	}
	return !rootVal.Type().HasAttribute(attr.Name)
}
