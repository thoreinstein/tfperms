package reporter

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

// TestRender_HeaderAndPermissionSectionsAlwaysPresent locks in the
// invariant that the header line and the two permission sections are
// always rendered, even on an empty result set. A reader skimming the
// output should never have to wonder whether a missing section means
// "no data" or "renderer skipped it by accident".
func TestRender_HeaderAndPermissionSectionsAlwaysPresent(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, resolver.Result{}, nil, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"tfperms analyze",
		"0 permissions for 0 resources, 0 unknowns, 0 unresolved conditionals, 0 parse warnings",
		"plan permissions:",
		"  (none)",
		"additional apply permissions:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, got)
		}
	}
	for _, absent := range []string{
		"unresolved conditionals:",
		"parse warnings:",
		"unknown resources:",
	} {
		if strings.Contains(got, absent) {
			t.Errorf("output should not contain %q on empty result\n--- output ---\n%s", absent, got)
		}
	}
}

// TestRender_HeaderPluralization covers the grammatical edge case
// around "parse warning" vs "parse warnings". The header uses singular
// only when the count is exactly 1; zero and many both pluralize.
func TestRender_HeaderPluralization(t *testing.T) {
	cases := []struct {
		name     string
		diags    hcl.Diagnostics
		wantSubs string
	}{
		{
			name:     "zero warnings plural",
			diags:    nil,
			wantSubs: "0 parse warnings",
		},
		{
			name: "one warning singular",
			diags: hcl.Diagnostics{
				diag("warn1", "/a.tf", 1),
			},
			wantSubs: "1 parse warning",
		},
		{
			name: "two warnings plural",
			diags: hcl.Diagnostics{
				diag("warn1", "/a.tf", 1),
				diag("warn2", "/a.tf", 2),
			},
			wantSubs: "2 parse warnings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Render(&buf, resolver.Result{}, tc.diags, 0); err != nil {
				t.Fatalf("Render: %v", err)
			}
			line := firstHeaderLine(buf.String())
			if !strings.Contains(line, tc.wantSubs) {
				t.Errorf("header = %q, want substring %q", line, tc.wantSubs)
			}
		})
	}
}

// TestRender_PermissionsListed verifies that PlanPerms and
// ApplyOnlyPerms render in resolver-supplied order, one perm per line,
// indented two spaces. The reporter must not re-sort: the resolver's
// contract is the ordering source of truth.
func TestRender_PermissionsListed(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create", "storage.buckets.delete"},
		TotalApplyPerms: []string{"storage.buckets.create", "storage.buckets.delete", "storage.buckets.get"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, res, nil, 1); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"3 permissions for 1 resources",
		"plan permissions:\n  storage.buckets.get\n",
		"additional apply permissions:\n  storage.buckets.create\n  storage.buckets.delete\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestRender_UnresolvedConditionalsSection covers the unresolved-
// conditional row format and the module-path suffix. Root-level
// resources omit the "in module." suffix; nested resources render
// "in module.<a>.module.<b>" so a reader can paste the path into a
// `terraform state` query without translation.
func TestRender_UnresolvedConditionalsSection(t *testing.T) {
	res := resolver.Result{
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "primary",
				Attribute:    "uniform_bucket_level_access",
				Reason:       "missing_variable",
				File:         "/abs/main.tf",
				Line:         12,
			},
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "child",
				ModulePath:   []string{"a", "b"},
				Attribute:    "logging",
				Reason:       "function_call",
				File:         "/abs/mod/main.tf",
				Line:         5,
			},
		},
	}
	var buf bytes.Buffer
	if err := Render(&buf, res, nil, 2); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"unresolved conditionals:",
		"  google_storage_bucket.primary attr=uniform_bucket_level_access reason=missing_variable (/abs/main.tf:12)",
		"  google_storage_bucket.child in module.a.module.b attr=logging reason=function_call (/abs/mod/main.tf:5)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestRender_UnknownResourcesSection covers the unknown-resource row
// format. Section appears only when len(Unknowns) > 0.
func TestRender_UnknownResourcesSection(t *testing.T) {
	res := resolver.Result{
		Unknowns: []resolver.UnknownResource{
			{Type: "google_secret_manager_secret_iam_policy", File: "/abs/iam.tf", Line: 7},
		},
	}
	var buf bytes.Buffer
	if err := Render(&buf, res, nil, 1); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"unknown resources:",
		"  google_secret_manager_secret_iam_policy (/abs/iam.tf:7)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestRender_ParseWarningsSection covers the warning row format and
// the (file:line) suffix. Detail strings render as an indented sub-line
// when present.
func TestRender_ParseWarningsSection(t *testing.T) {
	diags := hcl.Diagnostics{
		diag("could not load local module", "/abs/main.tf", 3),
	}
	diags[0].Detail = `module "broken" at ./missing: stat ./missing: no such file or directory`

	var buf bytes.Buffer
	if err := Render(&buf, resolver.Result{}, diags, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"parse warnings:",
		"  could not load local module (/abs/main.tf:3)",
		`    module "broken" at ./missing: stat ./missing: no such file or directory`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestRender_ParseWarningsSortedByLocation locks in the deterministic
// ordering for parse warnings. The reporter sorts by (Filename, Line)
// regardless of input order so two runs over the same set produce
// identical output.
func TestRender_ParseWarningsSortedByLocation(t *testing.T) {
	diags := hcl.Diagnostics{
		diag("c-warning", "/abs/c.tf", 1),
		diag("a-warning-line-9", "/abs/a.tf", 9),
		diag("a-warning-line-2", "/abs/a.tf", 2),
		diag("b-warning", "/abs/b.tf", 1),
	}
	var buf bytes.Buffer
	if err := Render(&buf, resolver.Result{}, diags, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	// Locate each warning's index in the rendered output and assert
	// the order matches lexicographic-by-file then ascending-by-line.
	idx := func(s string) int { return strings.Index(out, s) }
	want := []string{
		"a-warning-line-2",
		"a-warning-line-9",
		"b-warning",
		"c-warning",
	}
	for i := 1; i < len(want); i++ {
		if idx(want[i-1]) >= idx(want[i]) {
			t.Errorf("expected %q before %q in:\n%s", want[i-1], want[i], out)
		}
	}
}

// TestRender_NilSubjectDiagnosticDoesNotPanic covers the defensive
// branch in diagLocation: a global / file-level synthetic diagnostic
// without a Subject must render in a stable position rather than
// crashing the reporter.
func TestRender_NilSubjectDiagnosticDoesNotPanic(t *testing.T) {
	diags := hcl.Diagnostics{
		{Severity: hcl.DiagWarning, Summary: "global", Subject: nil},
		diag("located", "/abs/a.tf", 1),
	}
	var buf bytes.Buffer
	if err := Render(&buf, resolver.Result{}, diags, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "global (:0)") {
		t.Errorf("expected nil-subject warning to render with empty location, got:\n%s", got)
	}
	if !strings.Contains(got, "located (/abs/a.tf:1)") {
		t.Errorf("expected located warning to retain its position, got:\n%s", got)
	}
}

// TestRender_PropagatesWriterError confirms the broken-pipe contract:
// a writer that fails on its first Write must surface as a non-nil
// error from Render rather than exiting zero with a half-written
// report. errWriter's latch is what enforces this — reusing the same
// pattern as cmd/tfperms/catalog.go.
func TestRender_PropagatesWriterError(t *testing.T) {
	want := errors.New("pipe closed")
	fw := &failingWriter{err: want}
	err := Render(fw, resolver.Result{}, nil, 0)
	if err == nil {
		t.Fatal("expected non-nil error from Render with failing writer")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want errors.Is(%v)", err, want)
	}
}

// TestRender_AllSectionsTogether is the end-to-end shape test: a
// non-empty Result combined with a non-empty diagnostics slice must
// render every section in the documented order (header, plan,
// additional, unresolved, parse warnings, unknown).
func TestRender_AllSectionsTogether(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create"},
		TotalApplyPerms: []string{"storage.buckets.create", "storage.buckets.get"},
		Unknowns: []resolver.UnknownResource{
			{Type: "google_unknown_thing", File: "/abs/u.tf", Line: 4},
		},
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "x",
				Attribute:    "logging",
				Reason:       "missing_variable",
				File:         "/abs/main.tf",
				Line:         11,
			},
		},
	}
	diags := hcl.Diagnostics{
		diag("could not load local module", "/abs/main.tf", 1),
	}

	var buf bytes.Buffer
	if err := Render(&buf, res, diags, 2); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()

	// Index-order assertion — sections must appear in the documented order.
	wantOrder := []string{
		"tfperms analyze",
		"plan permissions:",
		"additional apply permissions:",
		"unresolved conditionals:",
		"parse warnings:",
		"unknown resources:",
	}
	for i := 1; i < len(wantOrder); i++ {
		prev, cur := strings.Index(out, wantOrder[i-1]), strings.Index(out, wantOrder[i])
		if prev < 0 || cur < 0 || prev >= cur {
			t.Errorf("section %q must appear before %q\n--- output ---\n%s",
				wantOrder[i-1], wantOrder[i], out)
		}
	}
}

// diag is a small helper for building a warning-severity diagnostic
// pinned to (file, line). Tests use it instead of literal hcl.Diagnostic
// values to keep the table-driven cases readable.
func diag(summary, file string, line int) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagWarning,
		Summary:  summary,
		Subject: &hcl.Range{
			Filename: file,
			Start:    hcl.Pos{Line: line, Column: 1},
			End:      hcl.Pos{Line: line, Column: 1},
		},
	}
}

// firstHeaderLine returns the second non-empty line of out — the header
// banner is the first, and the count summary is the second. Pulling
// this out keeps TestRender_HeaderPluralization assertions scoped to
// the count line rather than the whole report.
func firstHeaderLine(out string) string {
	lines := strings.Split(out, "\n")
	count := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		count++
		if count == 2 {
			return l
		}
	}
	return ""
}

// failingWriter returns err on every Write. Used by
// TestRender_PropagatesWriterError to confirm the broken-pipe contract.
type failingWriter struct{ err error }

func (f *failingWriter) Write(p []byte) (int, error) { return 0, f.err }
