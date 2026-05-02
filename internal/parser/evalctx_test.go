package parser

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// parseFiles parses each (name, body) pair via hclsyntax.ParseConfig and
// returns the resulting *hcl.File slice in declaration order. Test cases
// pass a slice of names alongside the map so they can control file order
// — Go's randomised map iteration would otherwise make ordering tests
// flaky.
func parseFiles(t *testing.T, files map[string]string, order []string) []*hcl.File {
	t.Helper()
	out := make([]*hcl.File, 0, len(order))
	for _, name := range order {
		body, ok := files[name]
		if !ok {
			t.Fatalf("test bug: order references missing file %q", name)
		}
		f, diags := hclsyntax.ParseConfig([]byte(body), name, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			t.Fatalf("parse fixture %q: %v", name, diags)
		}
		out = append(out, f)
	}
	return out
}

// varValue extracts ctx.Variables["var"][name]. Returns cty.NilVal when
// either the var object or the attribute is absent.
func varValue(ctx *hcl.EvalContext, name string) cty.Value {
	obj, ok := ctx.Variables["var"]
	if !ok {
		return cty.NilVal
	}
	if !obj.Type().HasAttribute(name) {
		return cty.NilVal
	}
	return obj.GetAttr(name)
}

// localValue extracts ctx.Variables["local"][name]. Returns cty.NilVal
// when either the local object or the attribute is absent.
func localValue(ctx *hcl.EvalContext, name string) cty.Value {
	obj, ok := ctx.Variables["local"]
	if !ok {
		return cty.NilVal
	}
	if !obj.Type().HasAttribute(name) {
		return cty.NilVal
	}
	return obj.GetAttr(name)
}

// hasVar reports whether ctx.Variables["var"][name] is present.
func hasVar(ctx *hcl.EvalContext, name string) bool {
	obj, ok := ctx.Variables["var"]
	if !ok {
		return false
	}
	return obj.Type().HasAttribute(name)
}

// hasLocal reports whether ctx.Variables["local"][name] is present.
func hasLocal(ctx *hcl.EvalContext, name string) bool {
	obj, ok := ctx.Variables["local"]
	if !ok {
		return false
	}
	return obj.Type().HasAttribute(name)
}

// TestBuildEvalContext_Phase1 covers the literal-only resolution surface:
// variable defaults that are literals are present, defaults that
// reference functions or other names are absent, and locals follow the
// same rules. Iterative resolution, var-ref locals, and cycle detection
// arrive in later phases.
func TestBuildEvalContext_Phase1(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		order     []string
		assertVar map[string]cty.Value // present + value
		absentVar []string             // must be absent
		assertLoc map[string]cty.Value
		absentLoc []string
	}{
		{
			name: "variable with literal default",
			files: map[string]string{
				"v.tf": `variable "region" { default = "us-east1" }`,
			},
			order:     []string{"v.tf"},
			assertVar: map[string]cty.Value{"region": cty.StringVal("us-east1")},
		},
		{
			name: "variable without default is absent",
			files: map[string]string{
				"v.tf": `variable "region" { type = string }`,
			},
			order:     []string{"v.tf"},
			absentVar: []string{"region"},
		},
		{
			name: "variable with function-call default is absent",
			files: map[string]string{
				"v.tf": `variable "region" { default = upper("foo") }`,
			},
			order:     []string{"v.tf"},
			absentVar: []string{"region"},
		},
		{
			name: "local with literal RHS",
			files: map[string]string{
				"l.tf": `locals { x = "hello" }`,
			},
			order:     []string{"l.tf"},
			assertLoc: map[string]cty.Value{"x": cty.StringVal("hello")},
		},
		{
			name: "local with function-call RHS is absent",
			files: map[string]string{
				"l.tf": `locals { x = upper("foo") }`,
			},
			order:     []string{"l.tf"},
			absentLoc: []string{"x"},
		},
		{
			name: "empty input has no var or local in context",
			files: map[string]string{
				"e.tf": `resource "x" "y" { bucket = "foo" }`,
			},
			order:     []string{"e.tf"},
			absentVar: []string{"any"},
			absentLoc: []string{"any"},
		},
		{
			name: "variable with literal number, bool, list",
			files: map[string]string{
				"v.tf": "" +
					"variable \"n\" { default = 42 }\n" +
					"variable \"b\" { default = true }\n" +
					"variable \"l\" { default = [\"a\", \"b\"] }\n",
			},
			order: []string{"v.tf"},
			assertVar: map[string]cty.Value{
				"n": cty.NumberIntVal(42),
				"b": cty.True,
				"l": cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := parseFiles(t, tc.files, tc.order)
			ctx, diags := buildEvalContext(files)
			if len(diags) != 0 {
				t.Errorf("phase 1 should produce no diagnostics; got %v", diags)
			}
			if ctx == nil {
				t.Fatal("buildEvalContext returned nil context; must always be non-nil")
			}
			if ctx.Variables == nil {
				t.Fatal("ctx.Variables is nil; must always be non-nil")
			}

			for name, want := range tc.assertVar {
				if !hasVar(ctx, name) {
					t.Errorf("var.%s missing; want %#v", name, want)
					continue
				}
				got := varValue(ctx, name)
				if !ctyValuesEqual(got, want) {
					t.Errorf("var.%s = %#v, want %#v", name, got, want)
				}
			}
			for _, name := range tc.absentVar {
				if hasVar(ctx, name) {
					t.Errorf("var.%s present (=%#v); want absent", name, varValue(ctx, name))
				}
			}
			for name, want := range tc.assertLoc {
				if !hasLocal(ctx, name) {
					t.Errorf("local.%s missing; want %#v", name, want)
					continue
				}
				got := localValue(ctx, name)
				if !ctyValuesEqual(got, want) {
					t.Errorf("local.%s = %#v, want %#v", name, got, want)
				}
			}
			for _, name := range tc.absentLoc {
				if hasLocal(ctx, name) {
					t.Errorf("local.%s present (=%#v); want absent", name, localValue(ctx, name))
				}
			}
		})
	}
}

// TestBuildEvalContext_Phase2 covers iterative local resolution: locals
// that reference resolved variables, locals that reference other locals
// (single hop, multi-hop, regardless of source order), and locals whose
// var dependency is missing (which must remain absent rather than
// erroring out).
func TestBuildEvalContext_Phase2(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		order     []string
		assertVar map[string]cty.Value
		assertLoc map[string]cty.Value
		absentLoc []string
	}{
		{
			name: "local references resolved var",
			files: map[string]string{
				"f.tf": "" +
					"variable \"region\" { default = \"us-east1\" }\n" +
					"locals { region_copy = var.region }\n",
			},
			order:     []string{"f.tf"},
			assertVar: map[string]cty.Value{"region": cty.StringVal("us-east1")},
			assertLoc: map[string]cty.Value{"region_copy": cty.StringVal("us-east1")},
		},
		{
			name: "local references another local (declared after)",
			files: map[string]string{
				"f.tf": "" +
					"locals {\n" +
					"  a = local.b\n" +
					"  b = \"leaf\"\n" +
					"}\n",
			},
			order: []string{"f.tf"},
			assertLoc: map[string]cty.Value{
				"a": cty.StringVal("leaf"),
				"b": cty.StringVal("leaf"),
			},
		},
		{
			name: "multi-hop local chain a -> b -> c -> leaf",
			files: map[string]string{
				"f.tf": "" +
					"locals {\n" +
					"  a = local.b\n" +
					"  b = local.c\n" +
					"  c = \"leaf\"\n" +
					"}\n",
			},
			order: []string{"f.tf"},
			assertLoc: map[string]cty.Value{
				"a": cty.StringVal("leaf"),
				"b": cty.StringVal("leaf"),
				"c": cty.StringVal("leaf"),
			},
		},
		{
			name: "multi-hop chain split across files in adversarial order",
			files: map[string]string{
				"a.tf": `locals { a = local.b }`,
				"b.tf": `locals { b = local.c }`,
				"c.tf": `locals { c = "leaf" }`,
			},
			// File order deliberately puts the leaf last so a single
			// pass cannot resolve everything.
			order: []string{"a.tf", "b.tf", "c.tf"},
			assertLoc: map[string]cty.Value{
				"a": cty.StringVal("leaf"),
				"b": cty.StringVal("leaf"),
				"c": cty.StringVal("leaf"),
			},
		},
		{
			name: "local references missing var stays absent",
			files: map[string]string{
				"f.tf": `locals { x = var.missing }`,
			},
			order:     []string{"f.tf"},
			absentLoc: []string{"x"},
		},
		{
			name: "interpolation referencing resolved local works",
			files: map[string]string{
				"f.tf": "" +
					"locals {\n" +
					"  name = \"alice\"\n" +
					"  greeting = \"hi ${local.name}\"\n" +
					"}\n",
			},
			order: []string{"f.tf"},
			assertLoc: map[string]cty.Value{
				"name":     cty.StringVal("alice"),
				"greeting": cty.StringVal("hi alice"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := parseFiles(t, tc.files, tc.order)
			ctx, diags := buildEvalContext(files)
			if len(diags) != 0 {
				t.Errorf("phase 2 fixture should produce no diagnostics; got %v", diags)
			}
			for name, want := range tc.assertVar {
				if !hasVar(ctx, name) {
					t.Errorf("var.%s missing; want %#v", name, want)
					continue
				}
				if got := varValue(ctx, name); !ctyValuesEqual(got, want) {
					t.Errorf("var.%s = %#v, want %#v", name, got, want)
				}
			}
			for name, want := range tc.assertLoc {
				if !hasLocal(ctx, name) {
					t.Errorf("local.%s missing; want %#v", name, want)
					continue
				}
				if got := localValue(ctx, name); !ctyValuesEqual(got, want) {
					t.Errorf("local.%s = %#v, want %#v", name, got, want)
				}
			}
			for _, name := range tc.absentLoc {
				if hasLocal(ctx, name) {
					t.Errorf("local.%s present (=%#v); want absent", name, localValue(ctx, name))
				}
			}
		})
	}
}

// TestBuildEvalContext_Phase3_Cycles covers cycle detection: each row
// supplies a fixture whose locals form a known dependency shape and
// asserts the warning surface (count, member sets, absence-of-resolved-
// values) plus that unrelated locals still resolve.
func TestBuildEvalContext_Phase3_Cycles(t *testing.T) {
	cases := []struct {
		name           string
		files          map[string]string
		order          []string
		wantCycles     [][]string // each inner slice = sorted member names of one cycle
		absentLoc      []string   // must be absent (cycle members + dependants thereof)
		assertResolved map[string]cty.Value
	}{
		{
			name: "two-node cycle",
			files: map[string]string{
				"f.tf": "" +
					"locals {\n" +
					"  a = local.b\n" +
					"  b = local.a\n" +
					"}\n",
			},
			order:      []string{"f.tf"},
			wantCycles: [][]string{{"a", "b"}},
			absentLoc:  []string{"a", "b"},
		},
		{
			name: "three-node cycle",
			files: map[string]string{
				"f.tf": "" +
					"locals {\n" +
					"  a = local.b\n" +
					"  b = local.c\n" +
					"  c = local.a\n" +
					"}\n",
			},
			order:      []string{"f.tf"},
			wantCycles: [][]string{{"a", "b", "c"}},
			absentLoc:  []string{"a", "b", "c"},
		},
		{
			name: "self-loop",
			files: map[string]string{
				"f.tf": `locals { a = local.a }`,
			},
			order:      []string{"f.tf"},
			wantCycles: [][]string{{"a"}},
			absentLoc:  []string{"a"},
		},
		{
			name: "cycle plus unrelated resolvable local",
			files: map[string]string{
				"f.tf": "" +
					"locals {\n" +
					"  a = local.b\n" +
					"  b = local.a\n" +
					"  c = \"ok\"\n" +
					"}\n",
			},
			order:          []string{"f.tf"},
			wantCycles:     [][]string{{"a", "b"}},
			absentLoc:      []string{"a", "b"},
			assertResolved: map[string]cty.Value{"c": cty.StringVal("ok")},
		},
		{
			name: "two independent cycles produce two warnings in deterministic order",
			files: map[string]string{
				"f.tf": "" +
					"locals {\n" +
					"  a = local.b\n" +
					"  b = local.a\n" +
					"  c = local.d\n" +
					"  d = local.c\n" +
					"}\n",
			},
			order:      []string{"f.tf"},
			wantCycles: [][]string{{"a", "b"}, {"c", "d"}},
			absentLoc:  []string{"a", "b", "c", "d"},
		},
		{
			name: "local depending on missing var is NOT a cycle (no warning)",
			files: map[string]string{
				"f.tf": `locals { x = upper(var.missing) }`,
			},
			order:      []string{"f.tf"},
			wantCycles: nil,
			absentLoc:  []string{"x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := parseFiles(t, tc.files, tc.order)
			ctx, diags := buildEvalContext(files)

			// Warning count must match wantCycles exactly.
			gotCycles := extractCycleMembers(diags)
			if !equalCycleSets(gotCycles, tc.wantCycles) {
				t.Errorf("cycle warnings = %v, want %v", gotCycles, tc.wantCycles)
			}
			// Every diagnostic must be warning-severity.
			for _, d := range diags {
				if d.Severity != hcl.DiagWarning {
					t.Errorf("diag %q severity = %v, want DiagWarning", d.Summary, d.Severity)
				}
			}

			for _, name := range tc.absentLoc {
				if hasLocal(ctx, name) {
					t.Errorf("local.%s present (=%#v); cycle members must be absent", name, localValue(ctx, name))
				}
			}
			for name, want := range tc.assertResolved {
				if !hasLocal(ctx, name) {
					t.Errorf("local.%s missing; want %#v", name, want)
					continue
				}
				if got := localValue(ctx, name); !ctyValuesEqual(got, want) {
					t.Errorf("local.%s = %#v, want %#v", name, got, want)
				}
			}
		})
	}
}

// TestBuildEvalContext_Phase3_PathologicalTermination combines literals,
// var-refs, multiple cycles, and function calls in one fixture and runs
// it under the package's default test timeout. The point is to confirm
// the resolver and SCC algorithm both terminate on adversarial input.
func TestBuildEvalContext_Phase3_PathologicalTermination(t *testing.T) {
	files := parseFiles(t, map[string]string{
		"big.tf": "" +
			"variable \"region\" { default = \"us-east1\" }\n" +
			"variable \"missing\" { type = string }\n" +
			"locals {\n" +
			"  literal = \"ok\"\n" +
			"  via_var = var.region\n" +
			"  via_chain = local.via_var\n" +
			"  cycle_a = local.cycle_b\n" +
			"  cycle_b = local.cycle_a\n" +
			"  selfloop = local.selfloop\n" +
			"  fn_call = upper(\"ABC\")\n" +
			"  bad_var = var.missing\n" +
			"}\n",
	}, []string{"big.tf"})

	ctx, diags := buildEvalContext(files)

	// Literals + var-refs + chain must resolve.
	for name, want := range map[string]cty.Value{
		"literal":   cty.StringVal("ok"),
		"via_var":   cty.StringVal("us-east1"),
		"via_chain": cty.StringVal("us-east1"),
	} {
		if !hasLocal(ctx, name) {
			t.Errorf("local.%s missing; want %#v", name, want)
			continue
		}
		if got := localValue(ctx, name); !ctyValuesEqual(got, want) {
			t.Errorf("local.%s = %#v, want %#v", name, got, want)
		}
	}

	// Cycle members and unresolvable locals must be absent.
	for _, name := range []string{"cycle_a", "cycle_b", "selfloop", "fn_call", "bad_var"} {
		if hasLocal(ctx, name) {
			t.Errorf("local.%s present; want absent", name)
		}
	}

	// Two cycles ⇒ exactly two warnings, deterministic order.
	want := [][]string{{"cycle_a", "cycle_b"}, {"selfloop"}}
	got := extractCycleMembers(diags)
	if !equalCycleSets(got, want) {
		t.Errorf("cycle warnings = %v, want %v", got, want)
	}
}

// TestBuildEvalContext_Phase3_DeterministicWarningText runs the same
// cycle fixture multiple times and asserts the warning Detail strings
// are byte-identical each time.
func TestBuildEvalContext_Phase3_DeterministicWarningText(t *testing.T) {
	files := parseFiles(t, map[string]string{
		"f.tf": "" +
			"locals {\n" +
			"  a = local.b\n" +
			"  b = local.a\n" +
			"  c = local.d\n" +
			"  d = local.c\n" +
			"}\n",
	}, []string{"f.tf"})

	_, firstDiags := buildEvalContext(files)
	first := summariseDiags(firstDiags)
	for i := 0; i < 20; i++ {
		_, diags := buildEvalContext(files)
		got := summariseDiags(diags)
		if got != first {
			t.Fatalf("iter %d diag summary differs: %q vs %q", i, got, first)
		}
	}
}

// extractCycleMembers parses the Detail of each cycle diagnostic into a
// sorted slice of member names. Returns one entry per diagnostic, in the
// order diagnostics were emitted.
func extractCycleMembers(diags hcl.Diagnostics) [][]string {
	out := make([][]string, 0, len(diags))
	for _, d := range diags {
		if d.Summary != "locals form a dependency cycle" {
			continue
		}
		parts := strings.Split(d.Detail, ", ")
		out = append(out, parts)
	}
	return out
}

// equalCycleSets compares two slices of cycles for set-of-sets equality.
// Inner slices are already sorted; outer slice order is treated as
// significant because cycle warnings are emitted in deterministic order.
func equalCycleSets(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// summariseDiags renders diags into a single string for byte-exact
// comparison across runs. Includes Severity, Summary, and Detail.
func summariseDiags(diags hcl.Diagnostics) string {
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		parts = append(parts, fmt.Sprintf("[%d] %s | %s", d.Severity, d.Summary, d.Detail))
	}
	return strings.Join(parts, "\n")
}

// TestBuildEvalContext_Phase2_Deterministic mirrors the parse_test.go
// determinism check: run the same fixture many times and confirm the
// returned context produces byte-identical assertions every iteration.
// Map iteration order in the resolver could otherwise leak through.
func TestBuildEvalContext_Phase2_Deterministic(t *testing.T) {
	files := parseFiles(t, map[string]string{
		"f.tf": "" +
			"variable \"region\" { default = \"us-east1\" }\n" +
			"locals {\n" +
			"  a = local.b\n" +
			"  b = local.c\n" +
			"  c = var.region\n" +
			"}\n",
	}, []string{"f.tf"})

	first, _ := buildEvalContext(files)
	for i := 0; i < 20; i++ {
		got, _ := buildEvalContext(files)
		for _, name := range []string{"a", "b", "c"} {
			f := localValue(first, name)
			g := localValue(got, name)
			if !ctyValuesEqual(f, g) {
				t.Fatalf("iter %d local.%s: got %#v, want %#v", i, name, g, f)
			}
		}
	}
}
