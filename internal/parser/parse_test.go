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

// resKey collapses a Resource into the (Kind, Type, Name) triple test cases
// assert against. File and Line are checked separately when relevant; this
// keeps the table rows compact.
type resKey struct {
	kind, typ, name string
}

func keysOf(rs []Resource) []resKey {
	out := make([]resKey, 0, len(rs))
	for _, r := range rs {
		out = append(out, resKey{r.Kind, r.Type, r.Name})
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
			wantKeys: []resKey{{"resource", "google_storage_bucket", "b"}},
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
				{"resource", "google_storage_bucket", "a"},
				{"resource", "google_storage_bucket", "b"},
				{"resource", "google_compute_instance", "c"},
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
				{"resource", "x", "a"},
				{"resource", "x", "b"},
			},
		},
		{
			name: "data block is extracted with Kind=data",
			files: map[string]string{
				"d.tf": `data "google_project" "p" {}`,
			},
			order:    []string{"d.tf"},
			wantKeys: []resKey{{"data", "google_project", "p"}},
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
				{"resource", "google_storage_bucket", "b"},
				{"data", "google_project", "p"},
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
				{"resource", "x", "a"},
				{"resource", "x", "z"},
				{"resource", "x", "z2"},
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
				{"resource", "x", "dup"},
				{"resource", "x", "dup"},
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
