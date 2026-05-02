package parser

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// metaCtxResolved returns an *hcl.EvalContext that resolves
// var.zero=0, var.two=2, var.flag=true, var.empty_map={}, var.empty_list=[],
// var.nonempty_map, var.nonempty_list, and var.lock=true. Used by rows
// that exercise resolved-reference behaviour for count and for_each.
func metaCtxResolved() *hcl.EvalContext {
	return &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{
				"zero":         cty.NumberIntVal(0),
				"two":          cty.NumberIntVal(2),
				"flag":         cty.True,
				"empty_map":    cty.MapValEmpty(cty.String),
				"empty_list":   cty.ListValEmpty(cty.String),
				"nonempty_map": cty.MapVal(map[string]cty.Value{"a": cty.StringVal("x")}),
				"nonempty_lst": cty.ListVal([]cty.Value{cty.StringVal("a")}),
				"lock":         cty.True,
			}),
		},
	}
}

// TestEvalMetaArgs_Count covers the count-only acceptance criteria:
// literal zero/positive, var-resolved zero/positive, unresolved, and
// ternary expressions resolved both ways. Each row supplies a single
// resource block fixture and the *hcl.EvalContext to evaluate against.
func TestEvalMetaArgs_Count(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		ctx      *hcl.EvalContext
		wantKeep bool
		wantWarn bool
	}{
		{
			name:     "count_zero_literal",
			src:      `resource "x" "y" { count = 0 }`,
			ctx:      emptyCtx(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "count_zero_float_literal",
			src:      `resource "x" "y" { count = 0.0 }`,
			ctx:      emptyCtx(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "count_positive_literal",
			src:      `resource "x" "y" { count = 3 }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "count_via_var_drops",
			src:      `resource "x" "y" { count = var.zero }`,
			ctx:      metaCtxResolved(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "count_via_var_keeps",
			src:      `resource "x" "y" { count = var.two }`,
			ctx:      metaCtxResolved(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "count_unresolved_warns",
			src:      `resource "x" "y" { count = var.unknown }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: true,
		},
		{
			name:     "count_ternary_resolves_to_one",
			src:      `resource "x" "y" { count = var.flag ? 1 : 0 }`,
			ctx:      metaCtxResolved(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "count_ternary_resolves_to_zero",
			src:      `resource "x" "y" { count = var.flag ? 0 : 1 }`,
			ctx:      metaCtxResolved(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "count_non_number_warns",
			src:      `resource "x" "y" { count = "three" }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: true,
		},
		{
			name:     "no_count_attribute_keeps",
			src:      `resource "x" "y" { bucket = "foo" }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "count_zero_negative_drops",
			src:      `resource "x" "y" { count = -1 }`,
			ctx:      emptyCtx(),
			wantKeep: false,
			wantWarn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blk := parseBlock(t, tc.src)
			res, diags := evalMetaArgs(blk, tc.ctx)
			if res.keep != tc.wantKeep {
				t.Errorf("keep = %v, want %v", res.keep, tc.wantKeep)
			}
			gotWarn := false
			for _, d := range diags {
				if d.Severity == hcl.DiagWarning && d.Summary == "unresolved conditional" {
					gotWarn = true
				}
			}
			if gotWarn != tc.wantWarn {
				t.Errorf("warn = %v, want %v (diags=%v)", gotWarn, tc.wantWarn, diags)
			}
			if tc.wantWarn {
				// Detail must mention which meta-arg was unresolved.
				if !strings.Contains(diags[0].Detail, "count") {
					t.Errorf("diag Detail %q must mention 'count'", diags[0].Detail)
				}
				// Subject must point at the count expression's range.
				if diags[0].Subject == nil {
					t.Errorf("diag Subject must be non-nil for unresolved count")
				}
			}
		})
	}
}

// TestEvalMetaArgs_ForEach covers the for_each acceptance criteria:
// empty map/list/set/tuple drop; non-empty keep; unresolved or
// non-collection expressions warn.
func TestEvalMetaArgs_ForEach(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		ctx      *hcl.EvalContext
		wantKeep bool
		wantWarn bool
	}{
		{
			name:     "foreach_empty_object_literal",
			src:      `resource "x" "y" { for_each = {} }`,
			ctx:      emptyCtx(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "foreach_empty_tuple_literal",
			src:      `resource "x" "y" { for_each = [] }`,
			ctx:      emptyCtx(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "foreach_nonempty_object",
			src:      `resource "x" "y" { for_each = { a = "x" } }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "foreach_nonempty_tuple",
			src:      `resource "x" "y" { for_each = ["a"] }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "foreach_via_var_empty_map",
			src:      `resource "x" "y" { for_each = var.empty_map }`,
			ctx:      metaCtxResolved(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "foreach_via_var_empty_list",
			src:      `resource "x" "y" { for_each = var.empty_list }`,
			ctx:      metaCtxResolved(),
			wantKeep: false,
			wantWarn: false,
		},
		{
			name:     "foreach_via_var_nonempty_map",
			src:      `resource "x" "y" { for_each = var.nonempty_map }`,
			ctx:      metaCtxResolved(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "foreach_via_var_nonempty_list",
			src:      `resource "x" "y" { for_each = var.nonempty_lst }`,
			ctx:      metaCtxResolved(),
			wantKeep: true,
			wantWarn: false,
		},
		{
			name:     "foreach_unresolved_warns",
			src:      `resource "x" "y" { for_each = var.unknown }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: true,
		},
		{
			name: "foreach_toset_unresolved_warns",
			// toset is a Terraform function; emptyCtx has no functions
			// registered, so this expression cannot be resolved here.
			// Per the spec's best-effort stance we keep + warn.
			src:      `resource "x" "y" { for_each = toset([]) }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: true,
		},
		{
			name:     "foreach_non_collection_warns",
			src:      `resource "x" "y" { for_each = "not a collection" }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: true,
		},
		{
			name:     "no_foreach_attribute_keeps",
			src:      `resource "x" "y" { bucket = "foo" }`,
			ctx:      emptyCtx(),
			wantKeep: true,
			wantWarn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blk := parseBlock(t, tc.src)
			res, diags := evalMetaArgs(blk, tc.ctx)
			if res.keep != tc.wantKeep {
				t.Errorf("keep = %v, want %v", res.keep, tc.wantKeep)
			}
			gotWarn := false
			for _, d := range diags {
				if d.Severity == hcl.DiagWarning && d.Summary == "unresolved conditional" {
					gotWarn = true
				}
			}
			if gotWarn != tc.wantWarn {
				t.Errorf("warn = %v, want %v (diags=%v)", gotWarn, tc.wantWarn, diags)
			}
			if tc.wantWarn {
				if !strings.Contains(diags[0].Detail, "for_each") {
					t.Errorf("diag Detail %q must mention 'for_each'", diags[0].Detail)
				}
				if diags[0].Subject == nil {
					t.Errorf("diag Subject must be non-nil for unresolved for_each")
				}
			}
		})
	}
}
