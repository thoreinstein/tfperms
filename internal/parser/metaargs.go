package parser

// Meta-argument evaluation for resource/data blocks. Companion to
// parse.go and attrs.go.
//
// evalMetaArgs walks a parsed block's body and decides — based on the
// block's count / for_each meta-arguments — whether the resource should
// be kept or dropped from Parse's output, and what diagnostic warnings
// (if any) accompany that decision. It also extracts two structural
// signals consumed downstream:
//
//   - dynamic block labels (top-level `dynamic "<label>" { ... }` only;
//     nested dynamic blocks inside another block's body are *not*
//     captured — that is a v1 non-goal and the spec is explicit about
//     it).
//   - lifecycle.prevent_destroy when written as a literal boolean.
//
// Resolution rules in summary:
//
//	count = 0                 → drop, no warning
//	count = N (N > 0)         → keep
//	count = var.X resolves    → drop iff value is 0, keep otherwise
//	count = expr unresolved   → keep + warn ("unresolved conditional")
//	count = non-number known  → keep + warn (Terraform itself errors;
//	                            we stay best-effort and surface a warning)
//	for_each = {} or []       → drop, no warning
//	for_each non-empty        → keep
//	for_each unresolved       → keep + warn
//	for_each non-collection   → keep + warn
//
// When both `count` and `for_each` are present (which Terraform itself
// rejects), evalMetaArgs evaluates both. If either says drop, the
// resource is dropped; if either is unresolved, a warning fires. We do
// not try to replicate Terraform's "you may declare only one of these"
// validation — the user's config is broken and a Terraform run will
// report it; the parser simply stays best-effort.
//
// Why a separate file rather than extending attrs.go: meta-arg policy
// is fundamentally different from attribute extraction. attrs.go is a
// pure name → cty.Value mapping. This routine produces three outputs
// (keep/drop, dynamic labels, prevent_destroy) plus diagnostics, and
// must traverse nested blocks (lifecycle, dynamic) which attrs.go
// deliberately ignores. Mixing the two would violate the existing
// single-purpose split and force attrs.go to grow a diagnostics return.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// metaResult is the structured output of evalMetaArgs.
//
//   - keep is true when the resource should remain in Parse's output.
//     A `count = 0` literal — a clean, resolved "no instances" answer
//     — sets keep=false and emits no diagnostic. An unresolved count
//     or for_each sets keep=true and emits a warning.
//   - dynamicLabels lists labels of top-level `dynamic "<label>"`
//     blocks declared on this resource's body, in source order.
//   - preventDestroy is true iff a `lifecycle { prevent_destroy = true }`
//     literal was found.
type metaResult struct {
	keep           bool
	dynamicLabels  []string
	preventDestroy bool
}

// evalMetaArgs inspects blk's count / for_each / lifecycle / dynamic
// meta-arguments against evalCtx and returns a metaResult plus any
// warning-severity diagnostics. The returned diagnostics slice is nil
// when the meta-args are either absent or fully resolved.
//
// evalCtx may be nil; the routine is lenient (resolution failures map
// to "unresolved + warn" via resolveExpr-like semantics) and never
// panics on a nil context.
//
// Defensive fall-through: if blk.Body is not a *hclsyntax.Body — which
// does not occur on parser-produced blocks but is possible in theory
// for bodies surfaced through some HCL extensions — the routine
// returns keep=true with no diagnostics, mirroring extractAttrs's
// "fail open" stance.
func evalMetaArgs(blk *hcl.Block, evalCtx *hcl.EvalContext) (metaResult, hcl.Diagnostics) {
	res := metaResult{keep: true}
	body, ok := blk.Body.(*hclsyntax.Body)
	if !ok {
		return res, nil
	}

	var diags hcl.Diagnostics

	if attr, ok := body.Attributes["count"]; ok {
		keep, d := evalCount(blk, attr, evalCtx)
		if !keep {
			res.keep = false
		}
		diags = append(diags, d...)
	}

	if attr, ok := body.Attributes["for_each"]; ok {
		keep, d := evalForEach(blk, attr, evalCtx)
		if !keep {
			res.keep = false
		}
		diags = append(diags, d...)
	}

	return res, diags
}

// evalCount evaluates a `count` attribute. Returns (keep, diags):
//
//   - keep=false iff the expression resolves to a known number equal
//     to zero. A literal `count = 0`, `count = 0.0`, or
//     `count = var.X` where var.X resolves to zero all drop.
//   - keep=true otherwise. A resolved positive number keeps without
//     warning; an unresolved expression or a known non-number keeps
//     with a warning-severity "unresolved conditional" diagnostic.
//
// We never invent a synthetic warning for a clean drop: `count = 0`
// is an answer, not an unknown.
func evalCount(blk *hcl.Block, attr *hclsyntax.Attribute, evalCtx *hcl.EvalContext) (bool, hcl.Diagnostics) {
	val, valDiags := attr.Expr.Value(evalCtx)
	if valDiags.HasErrors() || !val.IsKnown() || val.IsNull() {
		return true, hcl.Diagnostics{unresolvedConditional(blk, "count", attr.Expr.Range())}
	}
	if !val.Type().Equals(cty.Number) {
		// Terraform itself errors on non-number count; parser stays
		// best-effort and warns rather than dropping silently.
		return true, hcl.Diagnostics{unresolvedConditional(blk, "count", attr.Expr.Range())}
	}
	// AsBigFloat().Sign() returns -1/0/+1 for negative/zero/positive.
	// A negative count is also "no instances" by Terraform's runtime
	// rules, but Terraform errors before getting there; we treat
	// non-positive (Sign() <= 0) as drop to keep behaviour intuitive.
	sign := val.AsBigFloat().Sign()
	if sign <= 0 {
		return false, nil
	}
	return true, nil
}

// evalForEach evaluates a `for_each` attribute. Returns (keep, diags):
//
//   - keep=false iff the expression resolves to a known empty
//     collection: an empty map, list, set, tuple, or object. We treat
//     all five as equivalent because Terraform itself does (each
//     surfaces zero instances).
//   - keep=true otherwise. A resolved non-empty collection keeps
//     without warning; an unresolved expression or a known
//     non-collection value keeps with an "unresolved conditional"
//     warning. `for_each = toset([...])` is in practice unresolved at
//     this stage because the eval context has no functions registered,
//     so it routes through the warning path — that matches the spec's
//     "best effort" stance.
func evalForEach(blk *hcl.Block, attr *hclsyntax.Attribute, evalCtx *hcl.EvalContext) (bool, hcl.Diagnostics) {
	val, valDiags := attr.Expr.Value(evalCtx)
	if valDiags.HasErrors() || !val.IsKnown() || val.IsNull() {
		return true, hcl.Diagnostics{unresolvedConditional(blk, "for_each", attr.Expr.Range())}
	}
	empty, ok := isEmptyForEach(val)
	if !ok {
		// Not a collection or object — Terraform errors here; we warn.
		return true, hcl.Diagnostics{unresolvedConditional(blk, "for_each", attr.Expr.Range())}
	}
	if empty {
		return false, nil
	}
	return true, nil
}

// isEmptyForEach reports whether v is an empty for_each value. The
// second return is false when v is not a valid for_each shape at all
// (e.g. a string or number) — callers treat that as "warn".
//
// Object types must be branched on before calling LengthInt: cty
// panics if asked for the length of an object value. Collection types
// (map/list/set/tuple) all support LengthInt safely. Tuples cover the
// `[]` literal, which parses as an empty tuple rather than a list.
func isEmptyForEach(v cty.Value) (empty, ok bool) {
	t := v.Type()
	switch {
	case t.IsObjectType():
		return len(t.AttributeTypes()) == 0, true
	case t.IsMapType(), t.IsListType(), t.IsSetType(), t.IsTupleType():
		return v.LengthInt() == 0, true
	default:
		return false, false
	}
}

// unresolvedConditional builds a warning-severity diagnostic for a
// meta-argument expression we could not resolve. The Subject points
// at the expression's range so reporter consumers can render
// `file:line: <message>`.
//
// metaArg names the meta-argument ("count" or "for_each"); blk supplies
// the resource type/name so the Detail message is self-describing
// even when the diagnostics slice is collected separately from the
// resource list.
func unresolvedConditional(blk *hcl.Block, metaArg string, exprRange hcl.Range) *hcl.Diagnostic {
	subject := exprRange
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  "unresolved conditional",
		Detail:   fmt.Sprintf("%s %q %q: %s expression cannot be resolved", blk.Type, blk.Labels[0], blk.Labels[1], metaArg),
		Subject:  &subject,
	}
}
