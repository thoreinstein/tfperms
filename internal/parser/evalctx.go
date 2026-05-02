package parser

// Static evaluation context construction. Companion to parse.go.
//
// buildEvalContext walks the parsed *hcl.File slice produced by Parse,
// collects every `variable "name" {}` and `locals {}` block, and returns a
// populated *hcl.EvalContext suitable for resolving `var.X` and `local.X`
// references in resource/data attribute expressions.
//
// Resolution is iterative and best-effort:
//   - Variable defaults are evaluated against a nil context. Literal
//     defaults succeed; defaults that reference functions or other
//     variables fail and are absent from the resulting context. This
//     mirrors Terraform's own contract: variable defaults may not refer
//     to other variables or to locals.
//   - Locals are then resolved in passes over the collected expressions.
//     Each pass evaluates any local whose dependencies are satisfied by
//     the growing context. The loop terminates when a pass adds no new
//     resolutions.
//   - Locals still unresolved after fixed-point either belong to a
//     dependency cycle (flagged by phase 3) or depend on something we
//     cannot resolve (functions, missing variables, cross-resource
//     references). Both cases are silently absent from the context.
//
// Type fidelity is *not* preserved: a `variable "x" { type = number;
// default = "5" }` block is captured as cty.StringVal("5"), not coerced
// to a number. tfperms downstream rarely consumes attribute values for
// arithmetic, so this is acceptable; callers needing coercion must do
// it themselves.

import (
	"sort"
	"strings"

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
// empty.
//
// overrides may be nil. When non-nil, each entry binds a declared
// variable name to a propagated value (typically a literal module
// argument flowing in from a parent caller via LoadRecursive). An
// override takes priority over the same variable's `default = ...`
// expression. Override entries whose name is not declared by any
// `variable` block in `files` are silently dropped — that mirrors the
// rest of buildEvalContext's best-effort contract: undeclared names
// never appear in the resulting `var` object.
func buildEvalContext(files []*hcl.File, overrides map[string]cty.Value) (*hcl.EvalContext, hcl.Diagnostics) {
	varExprs, localDecls := collectDecls(files)

	// Resolve each declared variable in priority order:
	//   1. an override (propagated from a parent module call);
	//   2. the variable's literal default expression, evaluated against
	//      a nil context per the variable-default contract;
	//   3. otherwise, absent.
	// Overrides are filtered to declared names so propagating
	// `module "m" { foo = "x" }` into a child that never declared
	// `variable "foo"` does not pollute the child's `var` namespace.
	varValues := make(map[string]cty.Value, len(varExprs))
	for name, expr := range varExprs {
		if v, ok := overrides[name]; ok && v != cty.NilVal && v.IsKnown() {
			varValues[name] = v
			continue
		}
		// expr == nil means the variable is declared without a default.
		// With no override, it stays absent — same outcome the previous
		// implementation produced by simply not inserting the key.
		if expr == nil {
			continue
		}
		val, diags := expr.Value(nil)
		if diags.HasErrors() || !val.IsKnown() {
			continue
		}
		varValues[name] = val
	}

	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{}}
	// cty.ObjectVal panics on an empty map, so guard with len(...) > 0.
	if len(varValues) > 0 {
		ctx.Variables["var"] = cty.ObjectVal(varValues)
	}

	// Iteratively resolve locals against the growing context. Each pass
	// tries every still-unresolved local in alphabetical order; on
	// success the result is folded into ctx.Variables["local"] for the
	// next pass. The loop terminates when a pass makes no progress.
	resolved := make(map[string]cty.Value, len(localDecls))
	unresolved := make(map[string]hcl.Expression, len(localDecls))
	for name, d := range localDecls {
		unresolved[name] = d.expr
	}

	for {
		progress := false
		// Sort names per pass so iteration is deterministic regardless
		// of map order. cty.ObjectVal itself does not depend on insertion
		// order, but resolving in a stable order keeps the unresolved
		// set deterministic too.
		names := make([]string, 0, len(unresolved))
		for n := range unresolved {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, name := range names {
			val, diags := unresolved[name].Value(ctx)
			if diags.HasErrors() || !val.IsKnown() {
				continue
			}
			resolved[name] = val
			delete(unresolved, name)
			progress = true
		}

		// Rebuild the local object exposed to the next pass. Rebuilding
		// each iteration (not just at exit) lets multi-hop dependency
		// chains converge in O(depth) passes instead of O(depth^2).
		if len(resolved) > 0 {
			ctx.Variables["local"] = cty.ObjectVal(resolved)
		}

		if !progress {
			break
		}
	}

	// Anything still in `unresolved` is either part of a cycle or
	// depends on something outside our reach (a function call, an
	// undefined var, a cross-resource reference). Cycle detection only
	// flags the former; the latter are silently absent.
	diags := findCycleDiagnostics(unresolved, localDecls)
	return ctx, diags
}

// collectDecls walks every parsed file's *hclsyntax.Body and extracts
// variable declarations and locals bindings. Files whose bodies are
// not *hclsyntax.Body (theoretically possible through some HCL
// extensions but not produced by ParseConfig) are skipped defensively.
//
// varExprs has one entry per declared variable. The value is the
// variable's `default` expression when one is present; nil when the
// variable is declared without a default. Distinguishing "declared
// without default" from "not declared" matters for buildEvalContext's
// override path: an undeclared name never enters the `var` object,
// even when a caller propagates an argument under that name.
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
					// Body unparseable → record as declared-without-
					// default so an override can still resolve it.
					varExprs[name] = nil
					continue
				}
				varContent, _, _ := varBody.PartialContent(variableSchema)
				if attr, ok := varContent.Attributes["default"]; ok {
					varExprs[name] = attr.Expr
				} else {
					// Declared but no default. nil signals "no default
					// expression to evaluate"; an override (if any)
					// still applies in buildEvalContext.
					varExprs[name] = nil
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

// findCycleDiagnostics inspects the set of unresolved locals and
// returns one warning per dependency cycle in the local→local subgraph.
//
// Locals whose only unresolved dependencies are non-local (a missing
// `var.X`, a function call, a reference to a cross-resource value) are
// not part of any cycle and produce no diagnostic. That is deliberate:
// the contract for buildEvalContext is to flag only true cycles. As a
// concrete consequence, `local.x = upper(var.region)` with `var.region`
// missing will sit unresolved at fixed-point but emit no warning,
// because its only unresolved dependency is `var.region`, not another
// local.
//
// Cycles are detected via iterative Tarjan SCC on the subgraph induced
// by unresolved locals. A cycle is any SCC of size ≥ 2, or a single-
// node SCC that contains a self-loop (`local.a = local.a`). Members of
// each cycle are sorted alphabetically; cycles themselves are sorted by
// their first member, so the warning order is deterministic across runs.
func findCycleDiagnostics(unresolved map[string]hcl.Expression, decls map[string]localDecl) hcl.Diagnostics {
	if len(unresolved) == 0 {
		return nil
	}

	// adj[A] = sorted list of B such that A's expression contains a
	// `local.B` traversal AND B is also unresolved. Sorted purely for
	// determinism of the Tarjan traversal.
	adj := make(map[string][]string, len(unresolved))
	selfLoop := make(map[string]bool)
	for name, expr := range unresolved {
		seen := make(map[string]struct{})
		for _, trav := range expr.Variables() {
			// A traversal with only one step (`local`) is malformed
			// Terraform — defensive skip rather than panicking on the
			// step-1 type assertion below.
			if len(trav) < 2 {
				continue
			}
			root, ok := trav[0].(hcl.TraverseRoot)
			if !ok || root.Name != "local" {
				continue
			}
			attr, ok := trav[1].(hcl.TraverseAttr)
			if !ok {
				continue
			}
			if _, isUnresolved := unresolved[attr.Name]; !isUnresolved {
				continue
			}
			if attr.Name == name {
				selfLoop[name] = true
				continue
			}
			seen[attr.Name] = struct{}{}
		}
		deps := make([]string, 0, len(seen))
		for d := range seen {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		adj[name] = deps
	}

	// Iterate Tarjan over node names in alphabetical order so SCC
	// discovery is deterministic.
	nodes := make([]string, 0, len(unresolved))
	for n := range unresolved {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	sccs := tarjanSCC(nodes, adj)

	cycles := make([][]string, 0)
	for _, scc := range sccs {
		if len(scc) >= 2 {
			members := append([]string(nil), scc...)
			sort.Strings(members)
			cycles = append(cycles, members)
			continue
		}
		// Single-node SCC: only a cycle if it has a self-loop.
		if selfLoop[scc[0]] {
			cycles = append(cycles, []string{scc[0]})
		}
	}
	sort.Slice(cycles, func(i, j int) bool {
		return cycles[i][0] < cycles[j][0]
	})

	if len(cycles) == 0 {
		return nil
	}

	diags := make(hcl.Diagnostics, 0, len(cycles))
	for _, members := range cycles {
		var subject *hcl.Range
		if d, ok := decls[members[0]]; ok {
			r := d.rng
			subject = &r
		}
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "locals form a dependency cycle",
			Detail:   joinCommaSeparated(members),
			Subject:  subject,
		})
	}
	return diags
}

// joinCommaSeparated joins names with ", ". Thin wrapper over
// strings.Join so the call sites in findCycleDiagnostics read as
// "render the cycle members" rather than as a generic string operation.
func joinCommaSeparated(names []string) string {
	return strings.Join(names, ", ")
}

// tarjanSCC runs an iterative Tarjan strongly-connected-components
// algorithm over the graph defined by `nodes` (in deterministic order)
// and `adj` (each adjacency list also in deterministic order).
//
// Iterative — not recursive — because pathological local-graph fixtures
// could otherwise blow the goroutine stack. Returns SCCs in discovery
// order; caller is responsible for any further sorting.
func tarjanSCC(nodes []string, adj map[string][]string) [][]string {
	index := 0
	indices := make(map[string]int, len(nodes))
	lowlink := make(map[string]int, len(nodes))
	onStack := make(map[string]bool, len(nodes))
	stack := make([]string, 0, len(nodes))
	var sccs [][]string

	// frame represents one node's traversal state on the explicit DFS
	// stack: which adjacency-list index we are about to recurse into.
	type frame struct {
		node string
		i    int
	}

	for _, root := range nodes {
		if _, visited := indices[root]; visited {
			continue
		}

		dfs := []frame{{node: root, i: 0}}
		indices[root] = index
		lowlink[root] = index
		index++
		stack = append(stack, root)
		onStack[root] = true

		for len(dfs) > 0 {
			top := &dfs[len(dfs)-1]
			neighbors := adj[top.node]
			if top.i < len(neighbors) {
				w := neighbors[top.i]
				top.i++
				if _, seen := indices[w]; !seen {
					indices[w] = index
					lowlink[w] = index
					index++
					stack = append(stack, w)
					onStack[w] = true
					dfs = append(dfs, frame{node: w, i: 0})
					continue
				}
				if onStack[w] {
					if indices[w] < lowlink[top.node] {
						lowlink[top.node] = indices[w]
					}
				}
				continue
			}

			// Finished exploring all neighbors. If this is an SCC root,
			// pop one component.
			if lowlink[top.node] == indices[top.node] {
				var comp []string
				for {
					n := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[n] = false
					comp = append(comp, n)
					if n == top.node {
						break
					}
				}
				sccs = append(sccs, comp)
			}

			// Pop this frame and propagate lowlink to parent.
			finished := top.node
			dfs = dfs[:len(dfs)-1]
			if len(dfs) > 0 {
				parent := &dfs[len(dfs)-1]
				if lowlink[finished] < lowlink[parent.node] {
					lowlink[parent.node] = lowlink[finished]
				}
			}
		}
	}

	return sccs
}
