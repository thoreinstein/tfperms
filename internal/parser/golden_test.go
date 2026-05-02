package parser

// Golden-file regression harness for the parser. Companion to walker.go,
// parse.go, attrs.go, evalctx.go, and metaargs.go: each scenario in
// testdata/parser/<name>/ exercises the full pipeline (FindTerraformFiles
// → Parse → Resource serialisation) and pins its output to an
// expected.json file checked into the repo.
//
// Re-generate the goldens after an intentional behaviour change:
//
//	go test ./internal/parser -run TestGolden -update
//
// The harness covers three failure modes uniformly, all of which surface
// as ordinary string fields on the JSON document:
//
//   - Walker errors (e.g. directory has no .tf files). Captured via
//     `walker_error`. Parse is never invoked in this branch.
//   - Parse errors (malformed HCL). Captured via `parse_error`;
//     diagnostics are omitted (Parse returned nil) and `resources` is
//     rendered as an empty array because the field is intentionally
//     non-omitempty for shape stability.
//   - Warning-severity diagnostics (cycles, unresolved meta-args). These
//     coexist with the resources slice on the same document.
//
// All filesystem-bearing strings are relativised to <SCENARIO_DIR>/
// before serialisation so the goldens are byte-identical across
// machines, working trees, and OS path separators (the harness
// normalises filepath.Separator to '/' as a final pass).

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// updateGolden controls whether TestGolden writes generated output to
// expected.json (true) or compares against it (false). Wired to the
// `-update` flag so callers run `go test -update` to regenerate after an
// intentional behaviour change.
var updateGolden = flag.Bool("update", false, "rewrite testdata/parser/*/expected.json from current parser output")

// goldenDoc is the JSON shape every expected.json file conforms to. The
// struct ordering matches the JSON field order so a hand-edited golden
// remains diff-friendly. Optional fields use `omitempty` so simple
// scenarios produce minimal documents.
type goldenDoc struct {
	WalkerError string        `json:"walker_error,omitempty"`
	ParseError  string        `json:"parse_error,omitempty"`
	Diagnostics []goldenDiag  `json:"diagnostics,omitempty"`
	Resources   []goldenResrc `json:"resources"`
	Modules     []goldenMod   `json:"modules"`
}

// goldenMod is the serialised projection of a parser.ModuleCall. Args are
// rendered identically to Resource.Attrs.
type goldenMod struct {
	Name       string       `json:"name"`
	Source     string       `json:"source"`
	SourceKind string       `json:"source_kind"`
	File       string       `json:"file"`
	Line       int          `json:"line"`
	Args       []goldenAttr `json:"args"`
}

// goldenDiag is the serialised projection of a single hcl.Diagnostic.
// Severity is rendered as "warning" / "error" / "unknown" so a future
// hcl release adding a new severity does not silently match the wrong
// branch on the consumer side.
type goldenDiag struct {
	Severity string       `json:"severity"`
	Summary  string       `json:"summary"`
	Detail   string       `json:"detail,omitempty"`
	Subject  *goldenRange `json:"subject,omitempty"`
}

// goldenRange is the serialised projection of an hcl.Range, narrowed to
// just the start line/column. End position is intentionally dropped:
// the parser's contract guarantees Line, not the full span, and the
// extra precision would make goldens noisier without buying coverage.
type goldenRange struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// goldenResrc is the serialised projection of a parser.Resource. Attrs
// is rendered as a sorted slice of {name, value} pairs rather than a
// JSON object so iteration order is deterministic regardless of Go's
// randomised map traversal — the top-level JSON encoder sorts string
// keys but we go through the same pipeline twice for nested object
// values, and a sorted slice is a single source of truth.
type goldenResrc struct {
	Kind           string       `json:"kind"`
	Type           string       `json:"type"`
	Name           string       `json:"name"`
	File           string       `json:"file"`
	Line           int          `json:"line"`
	Attrs          []goldenAttr `json:"attrs"`
	DynamicBlocks  []string     `json:"dynamic_blocks,omitempty"`
	PreventDestroy bool         `json:"prevent_destroy,omitempty"`
}

// goldenAttr pairs an attribute name with its serialised cty.Value. The
// Value field is `any` so a single document can carry strings, bools,
// numbers, nested maps, and the explicit nil that represents both
// cty.NilVal (unresolved) and known-null literals. The two are
// indistinguishable in JSON; tests that need to distinguish them must
// assert in Go rather than against the golden.
type goldenAttr struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// TestGolden discovers every immediate subdirectory of testdata/parser
// and runs each as a sub-test. A subdirectory is a "scenario": its .tf
// files (if any) are fed through FindTerraformFiles → Parse, and the
// result is compared against (or written to) the scenario's
// expected.json.
//
// The harness intentionally accepts an empty testdata/parser/ tree —
// the assertion is "every scenario directory matches its golden", so
// zero scenarios is a vacuously-true pass. This lets phase 1 land the
// driver before any scenarios are populated.
func TestGolden(t *testing.T) {
	root := filepath.Join("testdata", "parser")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("no scenarios at %s", root)
			return
		}
		t.Fatalf("read %s: %v", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			runGoldenScenario(t, filepath.Join(root, name))
		})
	}
}

// runGoldenScenario executes one scenario directory: drives the parser,
// serialises the result, then either writes (in -update mode) or
// compares against expected.json.
//
// The compare path emits a side-by-side diff with both `--- want ---`
// and `--- got ---` blocks so a CI failure is actionable without
// re-running locally. We deliberately do not minimise the diff output:
// when goldens are small (tens of lines) the full payload is more
// useful than a line-by-line patch.
func runGoldenScenario(t *testing.T, scenarioDir string) {
	t.Helper()

	doc := buildGoldenDoc(t, scenarioDir)
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	expectedPath := filepath.Join(scenarioDir, "expected.json")

	if *updateGolden {
		if err := os.WriteFile(expectedPath, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", expectedPath, err)
		}
		return
	}

	want, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `go test -update` to create)", expectedPath, err)
	}
	if string(want) != string(got) {
		t.Errorf("golden mismatch for %s\n--- want ---\n%s--- got ---\n%s", scenarioDir, string(want), string(got))
	}
}

// buildGoldenDoc runs the parser pipeline over scenarioDir and projects
// the result into the JSON-friendly goldenDoc shape.
//
// All paths and path-bearing strings (walker error message, parse error
// message, diagnostic Subject filenames, Resource.File) are relativised
// against the scenario directory. We feed FindTerraformFiles an
// absolute path so every downstream string is also absolute and can be
// stripped uniformly — relative-path inputs would split the
// relativisation into two cases.
func buildGoldenDoc(t *testing.T, scenarioDir string) goldenDoc {
	t.Helper()
	doc := goldenDoc{
		Resources: []goldenResrc{},
		Modules:   []goldenMod{},
	}

	absDir, err := filepath.Abs(scenarioDir)
	if err != nil {
		t.Fatalf("abs %q: %v", scenarioDir, err)
	}

	files, walkerErr := FindTerraformFiles(absDir)
	if walkerErr != nil {
		doc.WalkerError = relativise(walkerErr.Error(), absDir)
		return doc
	}

	resources, modules, diags, parseErr := Parse(files)
	if parseErr != nil {
		doc.ParseError = relativise(parseErr.Error(), absDir)
		return doc
	}

	for _, d := range diags {
		doc.Diagnostics = append(doc.Diagnostics, projectDiag(d, absDir))
	}

	for _, r := range resources {
		doc.Resources = append(doc.Resources, projectResource(r, absDir))
	}

	for _, m := range modules {
		doc.Modules = append(doc.Modules, projectModule(m, absDir))
	}
	return doc
}

// projectModule projects a parser.ModuleCall into the goldenMod shape.
func projectModule(m ModuleCall, absDir string) goldenMod {
	return goldenMod{
		Name:       m.Name,
		Source:     m.Source,
		SourceKind: m.SourceKind,
		File:       relativisePath(m.File, absDir),
		Line:       m.Line,
		Args:       projectAttrs(m.Args),
	}
}

// relativise replaces occurrences of absDir in s with the literal token
// "<SCENARIO_DIR>" and normalises path separators to '/'. The token
// (rather than the empty string) makes it visible in goldens that the
// scenario directory was here without leaking an OS-specific absolute
// path.
//
// We do path-separator normalisation on the *whole* output so a Windows
// run produces the same goldens as macOS/Linux. The replacement is safe
// because the parser never embeds backslashes in user-facing strings
// for any reason other than path separation.
func relativise(s, absDir string) string {
	s = strings.ReplaceAll(s, absDir, "<SCENARIO_DIR>")
	if filepath.Separator != '/' {
		s = strings.ReplaceAll(s, string(filepath.Separator), "/")
	}
	return s
}

// relativisePath converts a single absolute filesystem path to a
// scenario-relative one. The returned string never starts with
// "../" — if filepath.Rel would produce one (because p is outside
// absDir, which should not happen but is possible if a fixture symlinks
// elsewhere) we fall through to the prefix-replacement form via
// relativise.
func relativisePath(p, absDir string) string {
	if p == "" {
		return p
	}
	if rel, err := filepath.Rel(absDir, p); err == nil && !strings.HasPrefix(rel, "..") {
		if filepath.Separator != '/' {
			rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")
		}
		return rel
	}
	return relativise(p, absDir)
}

// projectDiag projects an *hcl.Diagnostic into the goldenDiag shape,
// relativising the Subject's filename. Diagnostics with a nil Subject
// (rare; surfaces for synthetic diagnostics that have no source range)
// produce a goldenDiag with Subject left nil so `omitempty` drops the
// field from the document.
func projectDiag(d *hcl.Diagnostic, absDir string) goldenDiag {
	out := goldenDiag{
		Severity: severityName(d.Severity),
		Summary:  d.Summary,
		Detail:   d.Detail,
	}
	if d.Subject != nil {
		out.Subject = &goldenRange{
			File: relativisePath(d.Subject.Filename, absDir),
			Line: d.Subject.Start.Line,
			Col:  d.Subject.Start.Column,
		}
	}
	return out
}

// severityName maps an hcl.DiagnosticSeverity to a stable string. We
// emit "unknown" rather than panicking on an unrecognised value so a
// future hcl release adding a severity surfaces visibly in the diff
// instead of corrupting the run.
func severityName(s hcl.DiagnosticSeverity) string {
	switch s {
	case hcl.DiagError:
		return "error"
	case hcl.DiagWarning:
		return "warning"
	default:
		return "unknown"
	}
}

// projectResource projects a parser.Resource into the goldenResrc
// shape. Attrs are routed through projectAttrs which sorts by name; the
// remaining fields are copied as-is except for File which is
// relativised.
func projectResource(r Resource, absDir string) goldenResrc {
	return goldenResrc{
		Kind:           r.Kind,
		Type:           r.Type,
		Name:           r.Name,
		File:           relativisePath(r.File, absDir),
		Line:           r.Line,
		Attrs:          projectAttrs(r.Attrs),
		DynamicBlocks:  r.DynamicBlocks,
		PreventDestroy: r.PreventDestroy,
	}
}

// projectAttrs sorts the attribute map by key and projects each value
// through projectCtyValue. An empty map produces a non-nil empty slice
// so the JSON document always emits `"attrs": []` for resources with
// no attributes — that distinguishes "block has no attributes" from
// "field absent" in the golden.
func projectAttrs(attrs map[string]cty.Value) []goldenAttr {
	out := make([]goldenAttr, 0, len(attrs))
	if len(attrs) == 0 {
		return out
	}
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, goldenAttr{Name: n, Value: projectCtyValue(attrs[n])})
	}
	return out
}

// projectCtyValue converts a cty.Value to a JSON-friendly Go primitive.
// The mapping is:
//
//	cty.NilVal      → nil   (unresolved expression)
//	known null      → nil   (e.g. literal `null`; type info is dropped)
//	unknown value   → nil   (defensive — Parse already collapses these)
//	cty.String      → string
//	cty.Bool        → bool
//	cty.Number      → float64 (best-effort; loses precision for
//	                  numbers outside float64 range, which the parser
//	                  fixtures do not exercise)
//	list/set/tuple  → []any (recursive; element order preserved)
//	map/object      → map[string]any (recursive; encoding/json sorts
//	                  string keys at marshal time)
//
// An unsupported type (one we have not enumerated) produces a sentinel
// "<unsupported:<type>>" string rather than panicking, so a future
// scenario that exercises a new cty kind surfaces visibly in the
// diff and can be wired up explicitly.
func projectCtyValue(v cty.Value) any {
	if v == cty.NilVal {
		return nil
	}
	if !v.IsKnown() {
		return nil
	}
	if v.IsNull() {
		return nil
	}
	t := v.Type()
	switch {
	case t.Equals(cty.String):
		return v.AsString()
	case t.Equals(cty.Bool):
		return v.True()
	case t.Equals(cty.Number):
		f, _ := v.AsBigFloat().Float64()
		return f
	case t.IsListType(), t.IsSetType(), t.IsTupleType():
		out := make([]any, 0, v.LengthInt())
		it := v.ElementIterator()
		for it.Next() {
			_, ev := it.Element()
			out = append(out, projectCtyValue(ev))
		}
		return out
	case t.IsMapType(), t.IsObjectType():
		obj := make(map[string]any)
		it := v.ElementIterator()
		for it.Next() {
			k, ev := it.Element()
			obj[k.AsString()] = projectCtyValue(ev)
		}
		return obj
	}
	return fmt.Sprintf("<unsupported:%s>", t.FriendlyName())
}
