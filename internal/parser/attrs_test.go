package parser

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// parseBlock parses a single resource/data block from src and returns the
// first *hcl.Block. The schema mirrors topLevelSchema so the block we hand
// extractAttrs is shaped identically to what Parse produces, except we
// skip MergeFiles — the type-assertion to *hclsyntax.Body works on either
// path because PartialContent returns the original block bodies.
func parseBlock(t *testing.T, src string) *hcl.Block {
	t.Helper()
	f, diags := hclsyntax.ParseConfig([]byte(src), "test.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parse fixture: %v", diags)
	}
	content, _, diags := f.Body.PartialContent(topLevelSchema)
	if diags.HasErrors() {
		t.Fatalf("partial content: %v", diags)
	}
	if len(content.Blocks) == 0 {
		t.Fatalf("fixture produced no blocks: %q", src)
	}
	return content.Blocks[0]
}

// emptyCtx returns an *hcl.EvalContext with empty Variables. Used by
// rows that exercise unresolved-reference behaviour.
func emptyCtx() *hcl.EvalContext {
	return &hcl.EvalContext{Variables: map[string]cty.Value{}}
}

// varLocalCtx returns an *hcl.EvalContext that resolves var.region and
// local.prefix. Used by the resolved-reference rows.
func varLocalCtx() *hcl.EvalContext {
	return &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{
				"region": cty.StringVal("us-east1"),
			}),
			"local": cty.ObjectVal(map[string]cty.Value{
				"prefix": cty.StringVal("prod"),
			}),
		},
	}
}

// TestExtractAttrs covers the resolution contract for extractAttrs as a
// single table. Each row supplies a single resource/data block fixture,
// the *hcl.EvalContext to evaluate against, the expected Attrs map, and
// the expected Reasons map.
//
// Coverage maps onto Requirement 7 of the .5 spec (literals, var/local
// resolution, unresolved references, function calls, interpolations,
// cross-resource refs) plus the meta-arg / nested-block exclusions
// required by Requirements 5, 6, and 8. The wantReasons column also
// pins the reason-classification contract: every cty.NilVal in
// wantAttrs must have a corresponding entry in wantReasons drawn from
// the {ReasonFunctionCall, ReasonDataSource, ReasonMissingVariable,
// ReasonOther} vocabulary, and every resolved attribute must NOT
// appear in wantReasons.
func TestExtractAttrs(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		ctx         *hcl.EvalContext
		want        map[string]cty.Value
		wantReasons map[string]string
	}{
		// --- literal scalars (Requirement 7) ---
		{
			name: "literal string",
			src:  `resource "x" "y" { bucket = "foo" }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"bucket": cty.StringVal("foo"),
			},
		},
		{
			name: "literal integer",
			src:  `resource "x" "y" { count_n = 42 }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"count_n": cty.NumberIntVal(42),
			},
		},
		{
			name: "literal float",
			src:  `resource "x" "y" { ratio = 3.14 }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"ratio": cty.NumberFloatVal(3.14),
			},
		},
		{
			name: "literal bool true",
			src:  `resource "x" "y" { enabled = true }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"enabled": cty.True,
			},
		},
		{
			name: "literal bool false",
			src:  `resource "x" "y" { enabled = false }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"enabled": cty.False,
			},
		},
		{
			name: "empty body",
			src:  `resource "x" "y" {}`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{},
		},
		{
			name: "multiple literals coexist",
			src: `resource "x" "y" {
				bucket  = "foo"
				count_n = 42
				enabled = true
			}`,
			ctx: emptyCtx(),
			want: map[string]cty.Value{
				"bucket":  cty.StringVal("foo"),
				"count_n": cty.NumberIntVal(42),
				"enabled": cty.True,
			},
		},

		// --- var / local resolution (Requirement 7) ---
		{
			name: "resolved var reference",
			src:  `resource "x" "y" { region = var.region }`,
			ctx:  varLocalCtx(),
			want: map[string]cty.Value{
				"region": cty.StringVal("us-east1"),
			},
		},
		{
			name: "resolved local reference",
			src:  `resource "x" "y" { name = local.prefix }`,
			ctx:  varLocalCtx(),
			want: map[string]cty.Value{
				"name": cty.StringVal("prod"),
			},
		},
		{
			name: "unresolved var reference yields NilVal but key present",
			src:  `resource "x" "y" { region = var.missing }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"region": cty.NilVal,
			},
			wantReasons: map[string]string{
				"region": ReasonMissingVariable,
			},
		},
		{
			name: "unresolved local reference yields NilVal but key present",
			src:  `resource "x" "y" { name = local.missing }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"name": cty.NilVal,
			},
			// local.missing is intentionally classified as "other": the
			// missing-variable category is reserved for var.* references
			// the user can fix by providing a default. Locals are not
			// user-supplied inputs.
			wantReasons: map[string]string{
				"name": ReasonOther,
			},
		},

		// --- complex expressions (Requirement 7) ---
		{
			name: "function call with no functions in ctx",
			src:  `resource "x" "y" { name = upper("foo") }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"name": cty.NilVal,
			},
			wantReasons: map[string]string{
				"name": ReasonFunctionCall,
			},
		},
		{
			name: "interpolation referencing unknown var",
			src:  `resource "x" "y" { name = "prefix-${var.missing}" }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"name": cty.NilVal,
			},
			wantReasons: map[string]string{
				"name": ReasonMissingVariable,
			},
		},
		{
			name: "cross-resource reference",
			src:  `resource "x" "y" { bucket = aws_s3_bucket.b.id }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"bucket": cty.NilVal,
			},
			wantReasons: map[string]string{
				"bucket": ReasonOther,
			},
		},
		{
			name: "data-source reference",
			src:  `resource "x" "y" { bucket = data.foo.bar.baz }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"bucket": cty.NilVal,
			},
			wantReasons: map[string]string{
				"bucket": ReasonDataSource,
			},
		},

		// --- priority hierarchy: function call > data source > missing variable > other ---
		{
			name: "function call wraps data-source ref - function_call wins",
			src:  `resource "x" "y" { name = upper(data.foo.bar.baz) }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"name": cty.NilVal,
			},
			wantReasons: map[string]string{
				"name": ReasonFunctionCall,
			},
		},
		{
			name: "data source mixed with missing var - data_source wins",
			src:  `resource "x" "y" { name = "${var.missing}-${data.foo.bar.baz}" }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"name": cty.NilVal,
			},
			wantReasons: map[string]string{
				"name": ReasonDataSource,
			},
		},
		{
			name: "function wraps everything - function_call wins regardless",
			src:  `resource "x" "y" { name = format("%s-%s", var.missing, data.foo.bar) }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"name": cty.NilVal,
			},
			wantReasons: map[string]string{
				"name": ReasonFunctionCall,
			},
		},

		// --- meta-arg skipping (Requirement 5) ---
		{
			name: "meta-args dropped, real attribute kept",
			src: `resource "x" "y" {
				provider   = google.us
				depends_on = [aws_iam_role.r]
				count      = 3
				for_each   = toset([])
				bucket     = "real"
			}`,
			ctx: emptyCtx(),
			want: map[string]cty.Value{
				"bucket": cty.StringVal("real"),
			},
		},

		// --- nested-block exclusion (Requirement 6) ---
		{
			name: "lifecycle nested block excluded",
			src: `resource "x" "y" {
				bucket = "real"
				lifecycle {
					prevent_destroy = true
				}
			}`,
			ctx: emptyCtx(),
			want: map[string]cty.Value{
				"bucket": cty.StringVal("real"),
			},
		},
		{
			name: "dynamic nested block excluded",
			src: `resource "x" "y" {
				bucket = "real"
				dynamic "rule" {
					for_each = []
					content {
						action = "allow"
					}
				}
			}`,
			ctx: emptyCtx(),
			want: map[string]cty.Value{
				"bucket": cty.StringVal("real"),
			},
		},

		// --- data block parity ---
		{
			name: "data block extracts attributes the same way",
			src:  `data "google_project" "p" { project_id = "my-project" }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"project_id": cty.StringVal("my-project"),
			},
		},

		// --- null literal: locks in Risk 3 (typed null is a resolved literal) ---
		{
			name: "literal null is resolved to a typed null cty.Value",
			src:  `resource "x" "y" { value = null }`,
			ctx:  emptyCtx(),
			want: map[string]cty.Value{
				"value": cty.NullVal(cty.DynamicPseudoType),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blk := parseBlock(t, tc.src)
			got, gotReasons := extractAttrs(blk, tc.ctx)
			if got == nil {
				t.Fatalf("extractAttrs attrs returned nil; must always be non-nil")
			}
			if gotReasons == nil {
				t.Fatalf("extractAttrs reasons returned nil; must always be non-nil")
			}
			if len(got) != len(tc.want) {
				t.Errorf("extracted %d attrs (%v), want %d (%v)", len(got), keysOfMap(got), len(tc.want), keysOfMap(tc.want))
			}
			for k, want := range tc.want {
				gotVal, ok := got[k]
				if !ok {
					t.Errorf("missing key %q (got keys: %v)", k, keysOfMap(got))
					continue
				}
				if !ctyValuesEqual(gotVal, want) {
					t.Errorf("attr %q = %#v, want %#v", k, gotVal, want)
				}
			}
			for k := range got {
				if _, ok := tc.want[k]; !ok {
					t.Errorf("unexpected extra key %q = %#v", k, got[k])
				}
			}
			// Reasons assertions: every cty.NilVal in want must have a
			// matching reason in wantReasons, and resolved attrs must
			// NOT appear in gotReasons. Also flags any unexpected extra
			// reason key.
			if len(gotReasons) != len(tc.wantReasons) {
				t.Errorf("extracted %d reasons (%v), want %d (%v)", len(gotReasons), keysOfReasonMap(gotReasons), len(tc.wantReasons), keysOfReasonMap(tc.wantReasons))
			}
			for k, want := range tc.wantReasons {
				gotReason, ok := gotReasons[k]
				if !ok {
					t.Errorf("missing reason for key %q (got reason keys: %v)", k, keysOfReasonMap(gotReasons))
					continue
				}
				if gotReason != want {
					t.Errorf("reason %q = %q, want %q", k, gotReason, want)
				}
			}
			for k, gotReason := range gotReasons {
				if _, ok := tc.wantReasons[k]; !ok {
					t.Errorf("unexpected extra reason key %q = %q", k, gotReason)
				}
				// Sanity: any reason entry must correspond to a
				// cty.NilVal attribute. This catches a class of
				// regression where a resolved attribute leaks a
				// reason — which would silently inflate
				// Result.Unresolved downstream.
				if v, ok := got[k]; ok && v != cty.NilVal {
					t.Errorf("reason %q recorded for resolved attribute (value=%#v)", k, v)
				}
			}
		})
	}
}

// TestExtractAttrs_NilBlockBody covers the defensive type-assertion
// fall-through in extractAttrs. It is exercised by handing the function a
// block whose Body is not a *hclsyntax.Body. We use the ext/dynblock-style
// surrogate body (the empty body produced by hcl.EmptyBody) as a stand-in
// for any non-hclsyntax body Parse might encounter through MergeFiles.
func TestExtractAttrs_NonHCLSyntaxBody(t *testing.T) {
	blk := &hcl.Block{
		Type:   "resource",
		Labels: []string{"x", "y"},
		Body:   hcl.EmptyBody(),
	}
	got, gotReasons := extractAttrs(blk, emptyCtx())
	if got == nil {
		t.Fatalf("extractAttrs attrs returned nil; must always be non-nil")
	}
	if gotReasons == nil {
		t.Fatalf("extractAttrs reasons returned nil; must always be non-nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty attrs map for non-hclsyntax body, got %v", keysOfMap(got))
	}
	if len(gotReasons) != 0 {
		t.Errorf("expected empty reasons map for non-hclsyntax body, got %v", keysOfReasonMap(gotReasons))
	}
}

// keysOfMap returns the keys of a map in an unspecified but stable-enough
// order for diagnostic messages. Tests that compare attribute keys use
// this helper rather than %v over the whole map because cty.Value's
// GoString is verbose.
func keysOfMap(m map[string]cty.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// keysOfReasonMap is the reasons-map sibling of keysOfMap: returns the
// keys of a map[string]string in an unspecified order for diagnostic
// messages.
func keysOfReasonMap(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ctyValuesEqual compares two cty.Value instances for test purposes. It
// treats cty.NilVal == cty.NilVal as equal and falls back to the cty
// package's own equality for known values.
//
// We need this helper because cty.Value.Equals panics on cty.NilVal (it
// is the zero value, not a real cty value), and reflect.DeepEqual is
// brittle across cty's internal representations of typed nulls.
func ctyValuesEqual(a, b cty.Value) bool {
	if a == cty.NilVal || b == cty.NilVal {
		return a == cty.NilVal && b == cty.NilVal
	}
	if !a.Type().Equals(b.Type()) {
		return false
	}
	if a.IsNull() || b.IsNull() {
		return a.IsNull() && b.IsNull()
	}
	if !a.IsKnown() || !b.IsKnown() {
		return a.IsKnown() == b.IsKnown()
	}
	return a.Equals(b).True()
}
