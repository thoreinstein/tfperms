package parser

import (
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
