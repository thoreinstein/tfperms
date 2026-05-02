package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// writeFiles materialises a map of relative-path -> content under root and
// returns the absolute paths in the order their keys were given. Test cases
// pass a slice of names alongside the map so they can control the order
// Parse sees the files in (which is what the deterministic-sort case
// depends on); a plain range over the map would surrender that order to
// Go's randomised map iteration.
func writeFiles(t *testing.T, root string, files map[string]string, order []string) []string {
	t.Helper()
	paths := make([]string, 0, len(order))
	for _, name := range order {
		body, ok := files[name]
		if !ok {
			t.Fatalf("test bug: order references missing file %q", name)
		}
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %q: %v", full, err)
		}
		paths = append(paths, full)
	}
	return paths
}

// resKey collapses a Resource into the (Kind, Type, Name, ModulePath)
// tuple test cases assert against. File and Line are checked separately
// when relevant; this keeps the table rows compact. ModulePath is
// collapsed to a single "/"-joined string so resKey remains a comparable
// (==-friendly) value type — slices cannot appear in a struct key without
// breaking the equality used by equalResKeys.
type resKey struct {
	kind, typ, name string
	modulePath      string
}

// rk is a constructor for the common root-level case (no module path)
// so existing test rows stay compact. Tests asserting nested-module
// resources pass modulePath via a resKey literal directly.
func rk(kind, typ, name string) resKey {
	return resKey{kind: kind, typ: typ, name: name}
}

func keysOf(rs []Resource) []resKey {
	out := make([]resKey, 0, len(rs))
	for _, r := range rs {
		out = append(out, resKey{
			kind:       r.Kind,
			typ:        r.Type,
			name:       r.Name,
			modulePath: strings.Join(r.ModulePath, "/"),
		})
	}
	return out
}

// TestParse_Empty anchors the public contract: Parse(nil) and Parse([]) must
// return (nil, nil, nil, nil) — resources, modules, diagnostics, and error
// all nil — without touching the filesystem. cmd/tfperms relies on this so
// it can call Parse unconditionally.
func TestParse_Empty(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		got, mods, diags, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%v) error: %v", in, err)
		}
		if got != nil {
			t.Errorf("Parse(%v) = %v, want nil", in, got)
		}
		if mods != nil {
			t.Errorf("Parse(%v) modules = %v, want nil", in, mods)
		}
		if diags != nil {
			t.Errorf("Parse(%v) diags = %v, want nil", in, diags)
		}
	}
}

// TestParse covers the spec acceptance criteria as a single table. Each
// row builds its own t.TempDir so cases are mutually isolated. The "order"
// field controls the order Parse sees its input files in — important for
// the deterministic-sort row, which deliberately gives the input slice in
// reverse-name order to verify the result still sorts ascending.
func TestParse(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		order    []string
		wantKeys []resKey // expected (Kind, Type, Name) order
		wantErr  bool
		errFrag  string // when wantErr, this substring must appear in err.Error()
	}{
		{
			name: "single resource",
			files: map[string]string{
				"main.tf": `resource "google_storage_bucket" "b" {}`,
			},
			order:    []string{"main.tf"},
			wantKeys: []resKey{rk("resource", "google_storage_bucket", "b")},
		},
		{
			name: "multiple resources in one file",
			files: map[string]string{
				"main.tf": "" +
					"resource \"google_storage_bucket\" \"a\" {}\n" +
					"resource \"google_storage_bucket\" \"b\" {}\n" +
					"resource \"google_compute_instance\" \"c\" {}\n",
			},
			order: []string{"main.tf"},
			wantKeys: []resKey{
				rk("resource", "google_storage_bucket", "a"),
				rk("resource", "google_storage_bucket", "b"),
				rk("resource", "google_compute_instance", "c"),
			},
		},
		{
			name: "multi-file config merges uniformly",
			files: map[string]string{
				"a.tf": `resource "x" "a" {}`,
				"b.tf": `resource "x" "b" {}`,
			},
			order: []string{"a.tf", "b.tf"},
			wantKeys: []resKey{
				rk("resource", "x", "a"),
				rk("resource", "x", "b"),
			},
		},
		{
			name: "data block is extracted with Kind=data",
			files: map[string]string{
				"d.tf": `data "google_project" "p" {}`,
			},
			order:    []string{"d.tf"},
			wantKeys: []resKey{rk("data", "google_project", "p")},
		},
		{
			// Lists every top-level block kind the spec calls out as a
			// "silent skip" except `backend` (which is never legal at top
			// level — it only appears nested inside terraform { }) and
			// `check` (whose nested `assert { condition = ... error_message
			// = ... }` body is non-trivial to write inline; the
			// schema-driven skip path treats it identically to the others
			// shown here).
			name: "non-resource/data top-level blocks silently skipped",
			files: map[string]string{
				"m.tf": "" +
					"terraform { required_version = \">= 1.0\" }\n" +
					"provider \"google\" { project = \"x\" }\n" +
					"variable \"v\" { default = \"x\" }\n" +
					"locals { x = 1 }\n" +
					"module \"m\" { source = \"./mod\" }\n" +
					"output \"o\" { value = 1 }\n" +
					"resource \"google_storage_bucket\" \"b\" {}\n" +
					"data \"google_project\" \"p\" {}\n",
			},
			order: []string{"m.tf"},
			wantKeys: []resKey{
				rk("resource", "google_storage_bucket", "b"),
				rk("data", "google_project", "p"),
			},
		},
		{
			name: "deterministic sort across files even with reverse input order",
			// z.tf is given first in input order, but result must list a.tf first.
			files: map[string]string{
				"z.tf": "" +
					"resource \"x\" \"z\" {}\n" +
					"resource \"x\" \"z2\" {}\n",
				"a.tf": `resource "x" "a" {}`,
			},
			order: []string{"z.tf", "a.tf"},
			wantKeys: []resKey{
				rk("resource", "x", "a"),
				rk("resource", "x", "z"),
				rk("resource", "x", "z2"),
			},
		},
		{
			name: "empty file produces no resources, no error",
			files: map[string]string{
				"empty.tf": "",
			},
			order:    []string{"empty.tf"},
			wantKeys: nil,
		},
		{
			name: "duplicate resource across files is preserved (dedup is Epic 5's job)",
			files: map[string]string{
				"a.tf": `resource "x" "dup" {}`,
				"b.tf": `resource "x" "dup" {}`,
			},
			order: []string{"a.tf", "b.tf"},
			wantKeys: []resKey{
				rk("resource", "x", "dup"),
				rk("resource", "x", "dup"),
			},
		},
		{
			name: "malformed HCL surfaces single-line file:line: error",
			files: map[string]string{
				"bad.tf": `resource "x" "y" {`, // missing closing brace
			},
			order:   []string{"bad.tf"},
			wantErr: true,
			errFrag: "bad.tf:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := writeFiles(t, dir, tc.files, tc.order)

			got, _, diags, err := Parse(paths)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result: %v)", got)
				}
				msg := err.Error()
				if strings.Contains(msg, "\n") {
					t.Errorf("error message must be single-line; got %q", msg)
				}
				if strings.Contains(msg, "panic:") || strings.Contains(msg, "goroutine") {
					t.Errorf("error message must not contain stack-trace markers; got %q", msg)
				}
				if tc.errFrag != "" && !strings.Contains(msg, tc.errFrag) {
					t.Errorf("error %q does not contain %q", msg, tc.errFrag)
				}
				// Pattern check: must contain ":<digit>:" somewhere (file:line: shape).
				if !containsLineMarker(msg) {
					t.Errorf("error %q does not match <file>:<line>: pattern", msg)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// None of the existing rows declares cyclic locals, so the
			// diagnostics slice must be empty for every fixture in this
			// table. A future row that needs cycle warnings should split
			// out into its own test.
			if len(diags) != 0 {
				t.Errorf("unexpected diagnostics: %v", diags)
			}
			gotKeys := keysOf(got)
			if !equalResKeys(gotKeys, tc.wantKeys) {
				t.Errorf("result keys = %v, want %v (full: %+v)", gotKeys, tc.wantKeys, got)
			}
			// Attrs invariant: every Resource has a non-nil Attrs map.
			// Story .5 populated this with one entry per top-level
			// attribute (excluding meta-args); the empty-map half of the
			// pre-.5 assertion is gone, but the non-nil half stays so a
			// future regression that returns nil here is caught.
			for i, r := range got {
				if r.Attrs == nil {
					t.Errorf("got[%d].Attrs is nil; must be non-nil even when empty", i)
				}
			}
		})
	}
}

// TestParse_FileLineMetadata constructs a fixture with known line numbers
// and verifies File holds the absolute path the caller passed in and Line
// is the line of the block's `resource`/`data` keyword. Stories .5 and
// Epic 6 (which prints file:line in CLI output) depend on these invariants.
func TestParse_FileLineMetadata(t *testing.T) {
	dir := t.TempDir()
	src := "" +
		"# comment line 1\n" + // line 1
		"\n" + // line 2
		"resource \"google_storage_bucket\" \"b\" {}\n" + // line 3
		"\n" + // line 4
		"data \"google_project\" \"p\" {}\n" // line 5
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, _, diags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 2 {
		t.Fatalf("got %d resources, want 2: %+v", len(got), got)
	}
	want := []Resource{
		{Kind: "resource", Type: "google_storage_bucket", Name: "b", File: full, Line: 3},
		{Kind: "data", Type: "google_project", Name: "p", File: full, Line: 5},
	}
	for i, w := range want {
		g := got[i]
		if g.Kind != w.Kind || g.Type != w.Type || g.Name != w.Name || g.File != w.File || g.Line != w.Line {
			t.Errorf("got[%d]=%+v, want %+v", i, g, w)
		}
	}
}

// TestParse_AttrsPopulated proves the Parse → extractAttrs wire-up by
// running the full Parse pipeline on a block with a couple of literal
// attributes and asserting the resulting Resource.Attrs entries.
//
// Why end-to-end here when attrs_test.go already covers extractAttrs in
// isolation: this test specifically defends the call site in parse.go.
// A regression that drops the extractAttrs call (e.g. reverting to
// `make(map[string]cty.Value)`) keeps every attrs_test.go row passing
// but breaks production behaviour. Only literal attributes are used so
// the assertion does not couple to story .6's eval-context wiring.
func TestParse_AttrsPopulated(t *testing.T) {
	dir := t.TempDir()
	src := `resource "google_storage_bucket" "b" {
  bucket  = "my-bucket"
  enabled = true
}`
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, _, diags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(got), got)
	}
	r := got[0]
	if want := cty.StringVal("my-bucket"); !r.Attrs["bucket"].Equals(want).True() {
		t.Errorf("Attrs[bucket] = %#v, want %#v", r.Attrs["bucket"], want)
	}
	if want := cty.True; !r.Attrs["enabled"].Equals(want).True() {
		t.Errorf("Attrs[enabled] = %#v, want %#v", r.Attrs["enabled"], want)
	}
	if len(r.Attrs) != 2 {
		t.Errorf("Attrs has %d entries (%v), want 2", len(r.Attrs), r.Attrs)
	}
}

// TestParse_DeterministicAcrossRuns runs the same multi-file fixture many
// times and asserts the result order is identical each time. Go's sort is
// stable so this is paranoia, but it specifically defends against a future
// change reintroducing nondeterminism (e.g. switching to map-based block
// collection without a final sort).
func TestParse_DeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"z.tf": "resource \"x\" \"z1\" {}\nresource \"x\" \"z2\" {}\n",
		"m.tf": `resource "x" "m" {}`,
		"a.tf": `resource "x" "a" {}`,
	}
	paths := writeFiles(t, dir, files, []string{"z.tf", "m.tf", "a.tf"})

	first, _, _, err := Parse(paths)
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, _, _, err := Parse(paths)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !equalResKeys(keysOf(got), keysOf(first)) {
			t.Fatalf("iter %d order differs from first: %v vs %v", i, keysOf(got), keysOf(first))
		}
	}
}

// TestParse_VarLocalReferencesResolve proves the story .6 wire-up at the
// Parse boundary: a fixture with `variable`, `locals`, and a resource
// that references both must produce a Resource whose Attrs entries
// resolve to the literal values, not cty.NilVal. This complements
// evalctx_test.go (which tests buildEvalContext in isolation) by
// defending against a regression that drops the buildEvalContext call
// site or hands extractAttrs the empty context again.
func TestParse_VarLocalReferencesResolve(t *testing.T) {
	dir := t.TempDir()
	src := "" +
		"variable \"region\" { default = \"us-east1\" }\n" +
		"locals { region_copy = var.region }\n" +
		"resource \"google_storage_bucket\" \"b\" {\n" +
		"  region = var.region\n" +
		"  copy   = local.region_copy\n" +
		"}\n"
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, _, diags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(got), got)
	}
	r := got[0]
	want := cty.StringVal("us-east1")
	if r.Attrs["region"] == cty.NilVal || !r.Attrs["region"].Equals(want).True() {
		t.Errorf("Attrs[region] = %#v, want %#v", r.Attrs["region"], want)
	}
	if r.Attrs["copy"] == cty.NilVal || !r.Attrs["copy"].Equals(want).True() {
		t.Errorf("Attrs[copy] = %#v, want %#v", r.Attrs["copy"], want)
	}
}

// TestParse_DropsCountZero defends the Parse → evalMetaArgs wire-up:
// a literal `count = 0` resource must be dropped from the output and
// must NOT produce a warning (a clean drop is an answer, not an
// unknown). The companion resource without count survives.
func TestParse_DropsCountZero(t *testing.T) {
	dir := t.TempDir()
	src := "" +
		"resource \"x\" \"keep\" {}\n" +
		"resource \"x\" \"drop\" { count = 0 }\n" +
		"resource \"x\" \"keep_too\" { count = 1 }\n"
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, _, diags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("clean count=0 must produce no warnings; got %v", diags)
	}
	wantNames := []string{"keep", "keep_too"}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d resources, want %d: %+v", len(got), len(wantNames), got)
	}
	for i, w := range wantNames {
		if got[i].Name != w {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, w)
		}
	}
}

// TestParse_UnresolvedCountWarns defends the warning-emission wire-up:
// a `count = var.unknown` resource is kept (best-effort) and surfaces
// an "unresolved conditional" warning on the diagnostics return.
func TestParse_UnresolvedCountWarns(t *testing.T) {
	dir := t.TempDir()
	src := `resource "x" "y" { count = var.unknown }`
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, _, diags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resources, want 1 (unresolved must keep): %+v", len(got), got)
	}
	if len(diags) == 0 {
		t.Fatalf("expected at least one diagnostic, got none")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && d.Summary == "unresolved conditional" {
			found = true
		}
	}
	if !found {
		t.Errorf("missing 'unresolved conditional' warning; got %v", diags)
	}
}

// TestParse_DynamicAndPreventDestroy defends the structural-fields
// wire-up: dynamic-block labels and lifecycle.prevent_destroy must
// land on the Resource produced by Parse.
func TestParse_DynamicAndPreventDestroy(t *testing.T) {
	dir := t.TempDir()
	src := `resource "x" "y" {
  dynamic "rule" {
    for_each = []
    content {}
  }
  dynamic "tag" {
    for_each = []
    content {}
  }
  lifecycle {
    prevent_destroy = true
  }
}`
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, _, _, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(got), got)
	}
	r := got[0]
	if !r.PreventDestroy {
		t.Errorf("PreventDestroy = false, want true")
	}
	want := []string{"rule", "tag"}
	if len(r.DynamicBlocks) != len(want) {
		t.Fatalf("DynamicBlocks = %v, want %v", r.DynamicBlocks, want)
	}
	for i, w := range want {
		if r.DynamicBlocks[i] != w {
			t.Errorf("DynamicBlocks[%d] = %q, want %q", i, r.DynamicBlocks[i], w)
		}
	}
}

// TestParse_CycleEmitsWarning proves that locals dependency cycles
// surface as warning-severity diagnostics on Parse's middle return
// without escalating to an error. The Resource list is otherwise normal.
func TestParse_CycleEmitsWarning(t *testing.T) {
	dir := t.TempDir()
	src := "" +
		"locals {\n" +
		"  a = local.b\n" +
		"  b = local.a\n" +
		"}\n" +
		"resource \"x\" \"y\" {}\n"
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, _, diags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(got), got)
	}
	if len(diags) != 1 {
		t.Fatalf("expected exactly one diagnostic, got %d: %v", len(diags), diags)
	}
	d := diags[0]
	if d.Severity != hcl.DiagWarning {
		t.Errorf("severity = %v, want DiagWarning", d.Severity)
	}
	if !strings.Contains(d.Detail, "a") || !strings.Contains(d.Detail, "b") {
		t.Errorf("Detail %q must name both cycle members 'a' and 'b'", d.Detail)
	}
}

// TestParse_MetaArgWarningsDeterministic runs the same multi-resource
// fixture with multiple unresolved meta-args many times and asserts
// the warning order and content is byte-identical each iteration.
// Mirrors TestBuildEvalContext_Phase3_DeterministicWarningText for
// the meta-args side: Epic 6's reporter sorts on (file:line), so the
// upstream parser must already emit a stable order.
func TestParse_MetaArgWarningsDeterministic(t *testing.T) {
	dir := t.TempDir()
	src := "" +
		"resource \"x\" \"a\" { count = var.unknown_a }\n" +
		"resource \"x\" \"b\" { for_each = var.unknown_b }\n" +
		"resource \"x\" \"c\" { count = var.unknown_c }\n"
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, _, firstDiags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	first := summariseDiags(firstDiags)
	if first == "" {
		t.Fatalf("expected diagnostics, got empty summary (%d diags)", len(firstDiags))
	}
	for i := 0; i < 20; i++ {
		_, _, diags, err := Parse([]string{full})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		got := summariseDiags(diags)
		if got != first {
			t.Fatalf("iter %d diag summary differs:\n got: %q\nfirst: %q", i, got, first)
		}
	}
}

// containsLineMarker checks that msg has a ":<digit>" sequence — the
// minimum shape that proves a line number was interpolated. We avoid a
// regex to keep test-only dependencies out of the package.
func containsLineMarker(msg string) bool {
	for i := 0; i < len(msg)-1; i++ {
		if msg[i] != ':' {
			continue
		}
		c := msg[i+1]
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

func equalResKeys(a, b []resKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClassifySource pins down the source-kind classifier table. Cases are
// kept inline (not split into separate tests) so a future regression like
// "added a new prefix that swallows registry triplets" is visible as a row
// flip rather than a missing test.
func TestClassifySource(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"./child", SourceLocal},
		{"../sibling", SourceLocal},
		{"hashicorp/consul/aws", SourceRegistry},
		// Registry triplet retains its kind when followed by a
		// go-getter "//<subdir>" suffix or a "?query"/"#fragment"
		// modifier. Without the strip-before-split, these would
		// fall through to SourceUnknown.
		{"hashicorp/consul/aws//modules/vpc", SourceRegistry},
		{"hashicorp/consul/aws?ref=v1.0.0", SourceRegistry},
		{"hashicorp/consul/aws#main", SourceRegistry},
		// Private registry: <hostname>/<namespace>/<name>/<provider>.
		// First segment contains a "." → hostname → registry.
		{"app.terraform.io/example-corp/k8s-cluster/azurerm", SourceRegistry},
		// Regression: hostname embeds ".git" (e.g. registry.gitlab.*)
		// must not trip the git heuristic — a bare strings.Contains
		// check would misclassify these as SourceGit. The "." character
		// after .git is a hostname continuation, not a path boundary.
		{"registry.gitlab.example.com/ns/name/provider", SourceRegistry},
		{"app.gitsomething.io/ns/name/provider", SourceRegistry},
		// 4 segments but no hostname dot → not a registry path.
		{"a/b/c/d", SourceUnknown},
		{"git::https://example.com/repo.git", SourceGit},
		{"git@github.com:foo/bar.git", SourceGit},
		// .git as suffix, with subdir ("//"), with query, with fragment.
		{"github.com/hashicorp/terraform.git", SourceGit},
		{"github.com/hashicorp/terraform.git//modules/vpc", SourceGit},
		{"github.com/hashicorp/terraform.git?ref=v1.2.0", SourceGit},
		{"github.com/hashicorp/terraform.git#main", SourceGit},
		// Hostname contains ".git" but the actual repo path also ends
		// in ".git" — suffix check wins, classified as git.
		{"host.gitlab.com/user/repo.git", SourceGit},
		{"https://example.com/module.zip", SourceArchive},
		{"http://example.com/m.tar.gz", SourceArchive},
		{"just-a-string", SourceUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			if got := classifySource(tc.source); got != tc.want {
				t.Errorf("classifySource(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

// TestParse_Modules proves the Parse → extractModule wire-up: a module
// block with local and non-local sources must produce ModuleCall values
// with correctly classified SourceKind and Args.
func TestParse_Modules(t *testing.T) {
	dir := t.TempDir()
	src := `
module "local" {
  source = "./mod"
  input  = "foo"
}
module "registry" {
  source = "hashicorp/consul/aws"
}
`
	full := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, modules, diags, err := Parse([]string{full})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(modules) != 2 {
		t.Fatalf("got %d modules, want 2: %+v", len(modules), modules)
	}

	// First module: local
	m1 := modules[0]
	if m1.Name != "local" {
		t.Errorf("modules[0].Name = %q, want \"local\"", m1.Name)
	}
	if m1.Source != "./mod" {
		t.Errorf("modules[0].Source = %q, want \"./mod\"", m1.Source)
	}
	if m1.SourceKind != SourceLocal {
		t.Errorf("modules[0].SourceKind = %q, want %q", m1.SourceKind, SourceLocal)
	}
	got, ok := m1.Args["input"]
	if !ok {
		t.Fatalf("modules[0].Args missing key %q; got keys %v", "input", m1.Args)
	}
	if got == cty.NilVal {
		t.Fatalf("modules[0].Args[input] is cty.NilVal; expected resolved value")
	}
	if want := cty.StringVal("foo"); !got.Equals(want).True() {
		t.Errorf("modules[0].Args[input] = %#v, want %#v", got, want)
	}

	// Second module: registry
	m2 := modules[1]
	if m2.Name != "registry" {
		t.Errorf("modules[1].Name = %q, want \"registry\"", m2.Name)
	}
	if m2.SourceKind != SourceRegistry {
		t.Errorf("modules[1].SourceKind = %q, want %q", m2.SourceKind, SourceRegistry)
	}

	// Registry module should have triggered a warning diagnostic
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "non-local module source") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning for non-local module source, got none in %v", diags)
	}
}

// TestLoadRecursive_EmptyDirArg pins the public-boundary contract
// that LoadRecursive rejects an empty (or whitespace-only) dir
// argument with an error rather than silently resolving "" to the
// process working directory via filepath.Abs(""). The previous
// implementation accepted "" and made behaviour depend on ambient
// CWD state; this test guards against regressing back to that.
func TestLoadRecursive_EmptyDirArg(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{name: "empty", dir: ""},
		{name: "whitespace", dir: "   "},
		{name: "tab", dir: "\t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, mods, diags, err := LoadRecursive(tc.dir)
			if err == nil {
				t.Fatalf("LoadRecursive(%q): expected error, got nil (res=%v mods=%v diags=%v)", tc.dir, res, mods, diags)
			}
			if res != nil || mods != nil || diags != nil {
				t.Errorf("LoadRecursive(%q): expected nil slices on error, got res=%v mods=%v diags=%v", tc.dir, res, mods, diags)
			}
			if got := err.Error(); !strings.Contains(got, "LoadRecursive") || !strings.Contains(got, "empty") {
				t.Errorf("LoadRecursive(%q) error = %q; want error mentioning LoadRecursive and empty", tc.dir, got)
			}
		})
	}
}

// TestLoadRecursive_Empty proves the public contract on the trivial
// case: a directory with a single resource file and no modules
// produces the same Resource set Parse would, with each Resource
// carrying an empty ModulePath. This is the regression line that
// guards against LoadRecursive accidentally tagging root resources
// with a stale path.
func TestLoadRecursive_Empty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "x" "root" {}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, mods, diags, err := LoadRecursive(dir)
	if err != nil {
		t.Fatalf("LoadRecursive: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(mods) != 0 {
		t.Errorf("expected no modules, got %v", mods)
	}
	if len(res) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(res), res)
	}
	if got := res[0]; got.Name != "root" || len(got.ModulePath) != 0 {
		t.Errorf("got %+v, want Name=root with empty ModulePath", got)
	}
}

// TestLoadRecursive_LocalModule proves the basic recursion happy
// path: root calls one local module, the module's resource appears
// in the result with ModulePath equal to the call name. This is the
// path the local-module golden scenario also exercises; the unit
// test pins the contract at the API boundary so a future refactor
// that breaks the wiring fails here before the golden does.
func TestLoadRecursive_LocalModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`module "child" { source = "./mod" }`), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	modDir := filepath.Join(dir, "mod")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(`resource "x" "leaf" {}`), 0o644); err != nil {
		t.Fatalf("write mod: %v", err)
	}

	res, mods, diags, err := LoadRecursive(dir)
	if err != nil {
		t.Fatalf("LoadRecursive: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(mods) != 1 || mods[0].Name != "child" {
		t.Fatalf("expected 1 module call named child, got %v", mods)
	}
	if len(res) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(res), res)
	}
	if got := res[0]; got.Name != "leaf" {
		t.Errorf("got Name %q, want \"leaf\"", got.Name)
	}
	if got := res[0].ModulePath; len(got) != 1 || got[0] != "child" {
		t.Errorf("ModulePath = %v, want [child]", got)
	}
}

// TestLoadRecursive_DuplicateInstantiation proves the "duplicated
// resources, distinct ModulePaths" contract: a single module called
// from two different sites must contribute its resources twice in
// the output, each tagged with a distinct ModulePath. The deep-copy
// invariant on ModulePath is critical here — a shared underlying
// slice would let one instantiation overwrite the other.
func TestLoadRecursive_DuplicateInstantiation(t *testing.T) {
	dir := t.TempDir()
	root := `
module "x" { source = "./mod" }
module "y" { source = "./mod" }
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	modDir := filepath.Join(dir, "mod")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(`resource "x" "leaf" {}`), 0o644); err != nil {
		t.Fatalf("write mod: %v", err)
	}

	res, _, _, err := LoadRecursive(dir)
	if err != nil {
		t.Fatalf("LoadRecursive: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 instantiated resources, got %d: %+v", len(res), res)
	}
	got := []string{strings.Join(res[0].ModulePath, "/"), strings.Join(res[1].ModulePath, "/")}
	want := map[string]bool{"x": true, "y": true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected ModulePath %q in %v", p, got)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Errorf("missing ModulePaths %v in %v", want, got)
	}
	// Mutating one ModulePath must not affect the other (deep copy).
	res[0].ModulePath[0] = "MUTATED"
	if res[1].ModulePath[0] == "MUTATED" {
		t.Errorf("ModulePath slices share storage; mutation leaked across instantiations")
	}
}

// TestLoadRecursive_NestedModules proves multi-level path
// accumulation: root → A → B contributes B's resources tagged with
// ModulePath = ["a", "b"]. Order of accumulation matters; the path
// must run root-to-leaf, not leaf-to-root.
func TestLoadRecursive_NestedModules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`module "a" { source = "./a" }`), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	aDir := filepath.Join(dir, "a")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "main.tf"), []byte(`module "b" { source = "./b" }`), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	bDir := filepath.Join(aDir, "b")
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bDir, "main.tf"), []byte(`resource "x" "deep" {}`), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	res, _, diags, err := LoadRecursive(dir)
	if err != nil {
		t.Fatalf("LoadRecursive: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(res) != 1 {
		t.Fatalf("got %d resources, want 1: %+v", len(res), res)
	}
	want := []string{"a", "b"}
	got := res[0].ModulePath
	if len(got) != len(want) {
		t.Fatalf("ModulePath = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ModulePath[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestLoadRecursive_MissingLocalSource proves the graceful-degrade
// contract: a `module` block whose local source does not resolve to
// a directory must produce a warning diagnostic but not a hard
// error. The rest of the configuration loads as usual.
func TestLoadRecursive_MissingLocalSource(t *testing.T) {
	dir := t.TempDir()
	src := `
module "broken" { source = "./does-not-exist" }
resource "x" "kept" {}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, _, diags, err := LoadRecursive(dir)
	if err != nil {
		t.Fatalf("LoadRecursive: %v", err)
	}
	if len(res) != 1 || res[0].Name != "kept" {
		t.Fatalf("expected the sibling resource to survive, got %+v", res)
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && d.Summary == "could not load local module" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'could not load local module' warning, got %v", diags)
	}
}

// TestLoadRecursive_NonLocalModuleNotRecursed proves the SourceKind
// gate: non-local sources continue to emit the existing "non-local
// module source" warning from extractModule and are NOT walked.
// This is the regression line for the v1 non-goal of fetching
// remote modules.
func TestLoadRecursive_NonLocalModuleNotRecursed(t *testing.T) {
	dir := t.TempDir()
	src := `module "remote" { source = "hashicorp/consul/aws" }`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, _, diags, err := LoadRecursive(dir)
	if err != nil {
		t.Fatalf("LoadRecursive: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("expected no resources for a registry-only config, got %+v", res)
	}
	// Exactly one diagnostic: the existing "non-local module source"
	// warning. No "could not load" warning, because LoadRecursive
	// must short-circuit before the walker runs.
	if len(diags) != 1 {
		t.Fatalf("expected exactly 1 diagnostic, got %d: %v", len(diags), diags)
	}
	if diags[0].Summary != "non-local module source" {
		t.Errorf("diagnostic = %q, want %q", diags[0].Summary, "non-local module source")
	}
}

// TestLoadRecursive_Cycle proves the cycle-detection contract: a
// module that recurses into a directory currently up the call stack
// must produce a "module recursion cycle" warning rather than
// recursing indefinitely. The configuration loads as if the cycle
// edge had been pruned.
//
// We construct the cycle as root → a → root via a "../" back-reference
// because classifySource treats source = "." as SourceUnknown
// (only "./" and "../" prefixes count as local), so a self-cycle
// expressed as `source = "."` would be pruned at the SourceKind gate
// before the cycle detector ever ran.
func TestLoadRecursive_Cycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`module "a" { source = "./a" }`), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	aDir := filepath.Join(dir, "a")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "main.tf"), []byte(`module "back" { source = "../" }`), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	_, _, diags, err := LoadRecursive(dir)
	if err != nil {
		t.Fatalf("LoadRecursive: %v", err)
	}
	found := false
	for _, d := range diags {
		if d.Summary == "module recursion cycle" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected cycle warning, got %v", diags)
	}
}

// TestLoadRecursive_ArgumentPropagation exercises the literal-argument
// flow from a parent `module` call into the child module's eval
// context. Each subtest is a self-contained fixture so failures point
// at exactly one invariant.
//
// Invariants covered:
//   - A literal string arg overrides a child variable's default.
//   - A literal bool arg gates a child resource via `count = var.X ? 1
//     : 0`. With enabled = true the resource survives; with enabled =
//     false it is dropped (and produces no warning, since count = 0 is
//     a clean answer per evalMetaArgs).
//   - Two call sites with different literal args produce distinct
//     resource sets — the moduleTemplate cache must not collapse them.
//   - An unresolved argument expression (referring to a variable with
//     no default) does not propagate; the child's own default applies.
func TestLoadRecursive_ArgumentPropagation(t *testing.T) {
	t.Run("string override flows into child attrs", func(t *testing.T) {
		dir := t.TempDir()
		root := `module "child" {
  source = "./mod"
  region = "eu-west1"
}`
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o644); err != nil {
			t.Fatalf("write root: %v", err)
		}
		modDir := filepath.Join(dir, "mod")
		if err := os.MkdirAll(modDir, 0o755); err != nil {
			t.Fatalf("mkdir mod: %v", err)
		}
		modSrc := `variable "region" { default = "us-east1" }
resource "x" "leaf" {
  region = var.region
}`
		if err := os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(modSrc), 0o644); err != nil {
			t.Fatalf("write mod: %v", err)
		}

		res, _, _, err := LoadRecursive(dir)
		if err != nil {
			t.Fatalf("LoadRecursive: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("got %d resources, want 1: %+v", len(res), res)
		}
		want := cty.StringVal("eu-west1")
		got := res[0].Attrs["region"]
		if got == cty.NilVal || !got.Equals(want).True() {
			t.Errorf("Attrs[region] = %#v, want %#v (override must beat default)", got, want)
		}
	})

	t.Run("bool override gates child count conditional", func(t *testing.T) {
		// Same module called twice: once with enabled=true (resource
		// survives), once with enabled=false (resource dropped).
		dir := t.TempDir()
		root := `module "on" {
  source  = "./mod"
  enabled = true
}
module "off" {
  source  = "./mod"
  enabled = false
}`
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o644); err != nil {
			t.Fatalf("write root: %v", err)
		}
		modDir := filepath.Join(dir, "mod")
		if err := os.MkdirAll(modDir, 0o755); err != nil {
			t.Fatalf("mkdir mod: %v", err)
		}
		modSrc := `variable "enabled" { type = bool }
resource "x" "gated" {
  count = var.enabled ? 1 : 0
}`
		if err := os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(modSrc), 0o644); err != nil {
			t.Fatalf("write mod: %v", err)
		}

		res, _, diags, err := LoadRecursive(dir)
		if err != nil {
			t.Fatalf("LoadRecursive: %v", err)
		}
		// A clean count=0 must produce no warnings.
		for _, d := range diags {
			if d.Severity == hcl.DiagWarning && d.Summary == "unresolved conditional" {
				t.Errorf("unexpected unresolved-conditional warning; child should resolve via override: %v", diags)
			}
		}
		// Exactly one survivor: the "on" instantiation.
		if len(res) != 1 {
			t.Fatalf("got %d resources, want 1 (only the enabled call site survives): %+v", len(res), res)
		}
		got := res[0].ModulePath
		if len(got) != 1 || got[0] != "on" {
			t.Errorf("ModulePath = %v, want [on]", got)
		}
	})

	t.Run("distinct args produce distinct cached templates", func(t *testing.T) {
		// The same module is called with two different literal region
		// values. Each instantiation must reflect its own argument in
		// the resulting Resource's Attrs.
		dir := t.TempDir()
		root := `module "us" {
  source = "./mod"
  region = "us-east1"
}
module "eu" {
  source = "./mod"
  region = "eu-west1"
}`
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o644); err != nil {
			t.Fatalf("write root: %v", err)
		}
		modDir := filepath.Join(dir, "mod")
		if err := os.MkdirAll(modDir, 0o755); err != nil {
			t.Fatalf("mkdir mod: %v", err)
		}
		modSrc := `variable "region" { default = "default-region" }
resource "x" "leaf" {
  region = var.region
}`
		if err := os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(modSrc), 0o644); err != nil {
			t.Fatalf("write mod: %v", err)
		}

		res, _, _, err := LoadRecursive(dir)
		if err != nil {
			t.Fatalf("LoadRecursive: %v", err)
		}
		if len(res) != 2 {
			t.Fatalf("got %d resources, want 2: %+v", len(res), res)
		}
		byPath := map[string]cty.Value{}
		for _, r := range res {
			byPath[strings.Join(r.ModulePath, "/")] = r.Attrs["region"]
		}
		if got, want := byPath["us"], cty.StringVal("us-east1"); got == cty.NilVal || !got.Equals(want).True() {
			t.Errorf("region for module \"us\" = %#v, want %#v", got, want)
		}
		if got, want := byPath["eu"], cty.StringVal("eu-west1"); got == cty.NilVal || !got.Equals(want).True() {
			t.Errorf("region for module \"eu\" = %#v, want %#v", got, want)
		}
	})

	t.Run("unresolved arg does not override child default", func(t *testing.T) {
		// The argument expression references a variable with no
		// default and no override. extractAttrs collapses it to
		// cty.NilVal; literalOverrides drops it; the child sees only
		// its own default.
		dir := t.TempDir()
		root := `variable "missing" {}
module "child" {
  source = "./mod"
  region = var.missing
}`
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o644); err != nil {
			t.Fatalf("write root: %v", err)
		}
		modDir := filepath.Join(dir, "mod")
		if err := os.MkdirAll(modDir, 0o755); err != nil {
			t.Fatalf("mkdir mod: %v", err)
		}
		modSrc := `variable "region" { default = "child-default" }
resource "x" "leaf" {
  region = var.region
}`
		if err := os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(modSrc), 0o644); err != nil {
			t.Fatalf("write mod: %v", err)
		}

		res, _, _, err := LoadRecursive(dir)
		if err != nil {
			t.Fatalf("LoadRecursive: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("got %d resources, want 1: %+v", len(res), res)
		}
		want := cty.StringVal("child-default")
		got := res[0].Attrs["region"]
		if got == cty.NilVal || !got.Equals(want).True() {
			t.Errorf("Attrs[region] = %#v, want %#v (unresolved arg must not override default)", got, want)
		}
	})

	t.Run("override gates child for_each conditional", func(t *testing.T) {
		// The same module is called twice: once with an empty map for
		// for_each (resource drops cleanly with no warning), once with
		// a non-empty map (resource survives). Without the override
		// flow, var.instances would be cty.NilVal in the child context,
		// which evalForEach treats as "unresolved" — keep + warn — so
		// both call sites would survive and emit a warning. This test
		// is the regression bar for the for_each path that the doc
		// comment at parse.go:500 advertises but the original test
		// suite did not exercise.
		dir := t.TempDir()
		root := `module "on" {
  source    = "./mod"
  instances = { a = "x" }
}
module "off" {
  source    = "./mod"
  instances = {}
}`
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o644); err != nil {
			t.Fatalf("write root: %v", err)
		}
		modDir := filepath.Join(dir, "mod")
		if err := os.MkdirAll(modDir, 0o755); err != nil {
			t.Fatalf("mkdir mod: %v", err)
		}
		modSrc := `variable "instances" { type = map(string) }
resource "x" "fanout" {
  for_each = var.instances
}`
		if err := os.WriteFile(filepath.Join(modDir, "main.tf"), []byte(modSrc), 0o644); err != nil {
			t.Fatalf("write mod: %v", err)
		}

		res, _, diags, err := LoadRecursive(dir)
		if err != nil {
			t.Fatalf("LoadRecursive: %v", err)
		}
		// A resolved for_each (empty or non-empty) must not surface an
		// unresolved-conditional warning — the override fed into the
		// child eval context should make for_each fully known.
		for _, d := range diags {
			if d.Severity == hcl.DiagWarning && d.Summary == "unresolved conditional" {
				t.Errorf("unexpected unresolved-conditional warning; child for_each should resolve via override: %v", diags)
			}
		}
		// Exactly one survivor: the call site with the non-empty map.
		if len(res) != 1 {
			t.Fatalf("got %d resources, want 1 (only the non-empty for_each call site survives): %+v", len(res), res)
		}
		got := res[0].ModulePath
		if len(got) != 1 || got[0] != "on" {
			t.Errorf("ModulePath = %v, want [on]", got)
		}
	})

	t.Run("override flows transitively two levels deep", func(t *testing.T) {
		// root → a → b. Root supplies `enabled = true` to a; a's own
		// `module "b"` block forwards `enabled = var.enabled`. b's
		// resource is gated by `count = var.enabled ? 1 : 0`. The
		// transitive flow should keep the resource.
		dir := t.TempDir()
		rootSrc := `module "a" {
  source  = "./a"
  enabled = true
}`
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(rootSrc), 0o644); err != nil {
			t.Fatalf("write root: %v", err)
		}
		aDir := filepath.Join(dir, "a")
		if err := os.MkdirAll(aDir, 0o755); err != nil {
			t.Fatalf("mkdir a: %v", err)
		}
		aSrc := `variable "enabled" { type = bool }
module "b" {
  source  = "./b"
  enabled = var.enabled
}`
		if err := os.WriteFile(filepath.Join(aDir, "main.tf"), []byte(aSrc), 0o644); err != nil {
			t.Fatalf("write a: %v", err)
		}
		bDir := filepath.Join(aDir, "b")
		if err := os.MkdirAll(bDir, 0o755); err != nil {
			t.Fatalf("mkdir b: %v", err)
		}
		bSrc := `variable "enabled" { type = bool }
resource "x" "deep" {
  count = var.enabled ? 1 : 0
}`
		if err := os.WriteFile(filepath.Join(bDir, "main.tf"), []byte(bSrc), 0o644); err != nil {
			t.Fatalf("write b: %v", err)
		}

		res, _, _, err := LoadRecursive(dir)
		if err != nil {
			t.Fatalf("LoadRecursive: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("got %d resources, want 1 (transitive override should keep it): %+v", len(res), res)
		}
		want := []string{"a", "b"}
		got := res[0].ModulePath
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("ModulePath = %v, want %v", got, want)
		}
	})
}

// TestBuildCacheKey covers the deterministic-key invariant the cache
// relies on: same (absDir, overrides) → byte-identical key, and
// distinct override values → distinct keys. Without this, the cache
// would either collapse semantically distinct call sites or fail to
// reuse identical ones.
//
// The directory is sourced from t.TempDir() rather than a hard-coded
// "/tmp/mod" so the test stays hermetic and portable to OSes (Windows)
// where /tmp is not a meaningful path.
func TestBuildCacheKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mod")

	// Empty / nil overrides → bare absDir.
	if got := buildCacheKey(dir, nil); got != dir {
		t.Errorf("buildCacheKey(dir, nil) = %q, want %q", got, dir)
	}
	if got := buildCacheKey(dir, map[string]cty.Value{}); got != dir {
		t.Errorf("buildCacheKey(dir, empty) = %q, want %q", got, dir)
	}

	// Same overrides via two different insertion orders must hash to
	// the same key — this is the determinism invariant the sorted-key
	// code path defends.
	a := map[string]cty.Value{"region": cty.StringVal("us"), "enabled": cty.True}
	b := map[string]cty.Value{"enabled": cty.True, "region": cty.StringVal("us")}
	if buildCacheKey(dir, a) != buildCacheKey(dir, b) {
		t.Errorf("buildCacheKey not deterministic across insertion orders: %q vs %q",
			buildCacheKey(dir, a), buildCacheKey(dir, b))
	}

	// Distinct values → distinct keys.
	c := map[string]cty.Value{"region": cty.StringVal("us"), "enabled": cty.True}
	d := map[string]cty.Value{"region": cty.StringVal("eu"), "enabled": cty.True}
	if buildCacheKey(dir, c) == buildCacheKey(dir, d) {
		t.Errorf("buildCacheKey collapsed distinct values: both = %q", buildCacheKey(dir, c))
	}
}

// TestLiteralOverrides covers the filter that drops unresolved entries
// before they reach buildCacheKey / buildEvalContext. Unresolved entries
// would otherwise either be ignored downstream (wasted work) or, worse,
// pollute the cache key with non-deterministic GoString output.
func TestLiteralOverrides(t *testing.T) {
	if got := literalOverrides(nil); got != nil {
		t.Errorf("literalOverrides(nil) = %v, want nil", got)
	}
	if got := literalOverrides(map[string]cty.Value{}); got != nil {
		t.Errorf("literalOverrides(empty) = %v, want nil", got)
	}
	// All NilVal → nil (so the cache fast path stays active).
	if got := literalOverrides(map[string]cty.Value{"a": cty.NilVal, "b": cty.NilVal}); got != nil {
		t.Errorf("literalOverrides(all-nil) = %v, want nil", got)
	}
	// Mixed: only the resolved entries survive.
	got := literalOverrides(map[string]cty.Value{
		"resolved":   cty.StringVal("ok"),
		"unresolved": cty.NilVal,
	})
	if len(got) != 1 {
		t.Fatalf("literalOverrides mixed: got %d entries, want 1: %v", len(got), got)
	}
	if v, ok := got["resolved"]; !ok || !v.Equals(cty.StringVal("ok")).True() {
		t.Errorf("literalOverrides mixed: got[resolved] = %v, want StringVal(\"ok\")", v)
	}
	if _, ok := got["unresolved"]; ok {
		t.Errorf("literalOverrides mixed: cty.NilVal entry should have been dropped")
	}
}
