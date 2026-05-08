package reporter

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

// TestRenderFull exercises the "all sections populated" path. The
// fixture is hand-crafted (not the output of resolver.Resolve) because
// the reporter's contract is "given this Result, produce these bytes",
// independent of how the Result was produced. Using a synthetic Result
// keeps the test sensitive to renderer drift while immune to upstream
// resolver changes.
//
// Each assertion is a substring match rather than a byte-equal compare
// because the goal here is structural: every section header is present,
// every input row appears, the summary numbers track the inputs, and
// the inter-section blank lines exist. A dedicated golden-file test on
// a richer fixture would belong in the cmd-level integration tests
// where the format contract is the user-observable surface.
func TestRenderFull(t *testing.T) {
	res := resolver.Result{
		PlanPerms: []string{
			"bigquery.datasets.get",
			"storage.buckets.get",
		},
		ApplyOnlyPerms: []string{
			"bigquery.datasets.create",
			"storage.buckets.create",
			"storage.buckets.delete",
		},
		// TotalApplyPerms drives the summary number. Setting it
		// independently of Plan / ApplyOnly proves the renderer reads
		// it directly and does not synthesise the count from the other
		// two slices — a regression that would silently desynchronise
		// the summary on a future resolver change.
		TotalApplyPerms: []string{
			"bigquery.datasets.create",
			"bigquery.datasets.get",
			"storage.buckets.create",
			"storage.buckets.delete",
			"storage.buckets.get",
		},
		Unknowns: []resolver.UnknownResource{
			{Type: "google_dataplex_lake", Name: "primary", File: "main.tf", Line: 42},
		},
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "data",
				Attribute:    "uniform_bucket_level_access",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         14,
			},
		},
	}

	var buf bytes.Buffer
	if err := Render(&buf, res, 17); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := buf.String()

	// Summary line: must be the first line. Anchor on a leading prefix
	// + count so a regression that flips the format ("17 resources for
	// 5 permissions, ...") fails loudly.
	if !strings.HasPrefix(got, "  5 permissions for 17 resources, 1 unknown, 1 unresolved conditional\n") {
		t.Errorf("summary line wrong; got:\n%s", got)
	}

	// Each section header carries its count.
	wantHeaders := []string{
		"  plan permissions (2):",
		"  apply-only permissions (3):",
		"  unknown resources (1):",
		"  unresolved conditionals (1):",
	}
	for _, h := range wantHeaders {
		if !strings.Contains(got, h) {
			t.Errorf("output missing %q header.\noutput:\n%s", h, got)
		}
	}

	// Each input row must appear verbatim. The four-space indent is
	// part of the format contract.
	wantRows := []string{
		"    bigquery.datasets.get",
		"    storage.buckets.get",
		"    bigquery.datasets.create",
		"    storage.buckets.create",
		"    storage.buckets.delete",
		"    google_dataplex_lake.primary (main.tf:42)",
		"    google_storage_bucket.data: uniform_bucket_level_access (main.tf:14) — missing_variable",
	}
	for _, r := range wantRows {
		if !strings.Contains(got, r) {
			t.Errorf("output missing row %q.\noutput:\n%s", r, got)
		}
	}
}

// TestRenderCollapsed proves the empty-section collapse contract: a
// Result with non-empty permission sets and empty diagnostic sets must
// produce no `unknown resources` or `unresolved conditionals` blocks at
// all (no header, no body, no leading blank). This is the common case
// for a cleanly catalogued configuration and the format is what users
// will see most often.
func TestRenderCollapsed(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create"},
		TotalApplyPerms: []string{"storage.buckets.create", "storage.buckets.get"},
		// Unknowns and Unresolved intentionally nil — Result's
		// contract says non-nil empty slices are also valid; both
		// must collapse identically. We test the nil path here and
		// the empty-slice path implicitly via TestRenderMinimal.
	}

	var buf bytes.Buffer
	if err := Render(&buf, res, 1); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := buf.String()

	// Diagnostic sections must be entirely absent — no header, no
	// "(none)" stub. This is the contract that distinguishes the flat
	// reporter from the catalog stats renderer (which prints "(none)"
	// for empty sections by design).
	//
	// Anchor on the section-header form ("unknown resources (" /
	// "unresolved conditionals (") rather than the bare phrase: the
	// summary line legitimately contains the words "unresolved
	// conditionals" with a count prefix, and a naive substring check
	// would false-positive against it.
	if strings.Contains(got, "unknown resources (") {
		t.Errorf("collapsed output should not contain 'unknown resources' header.\noutput:\n%s", got)
	}
	if strings.Contains(got, "unresolved conditionals (") {
		t.Errorf("collapsed output should not contain 'unresolved conditionals' header.\noutput:\n%s", got)
	}

	// The summary line carries exactly four counts: permissions,
	// resources, unknowns, and unresolved conditionals. Diagnostics
	// (warnings) are rendered only in the optional warnings section
	// and intentionally do not appear in the summary line.
	if !strings.HasPrefix(got, "  2 permissions for 1 resource, 0 unknowns, 0 unresolved conditionals\n") {
		t.Errorf("summary line wrong; got:\n%s", got)
	}

	// Permission sections still render.
	if !strings.Contains(got, "  plan permissions (1):") {
		t.Errorf("output missing plan section.\noutput:\n%s", got)
	}
	if !strings.Contains(got, "  apply-only permissions (1):") {
		t.Errorf("output missing apply-only section.\noutput:\n%s", got)
	}
}

// TestRenderMinimal exercises the fully-empty Result path: no
// permissions and no diagnostics. The reporter must still emit the
// summary line — that line is the stable anchor downstream `diff`
// consumers rely on, and omitting it on an empty Result would mean a
// successful run produced zero bytes (indistinguishable from a broken
// pipe). All four sections are collapsed; output is exactly one line.
func TestRenderMinimal(t *testing.T) {
	// All slices explicitly empty (not nil) to prove the renderer
	// treats len()==0 identically regardless of allocation state.
	// resolver.Resolve returns non-nil empty slices on a no-resource
	// configuration, so this is the production-realistic shape.
	res := resolver.Result{
		PlanPerms:       []string{},
		ApplyOnlyPerms:  []string{},
		TotalApplyPerms: []string{},
		Unknowns:        []resolver.UnknownResource{},
		Unresolved:      []resolver.UnresolvedConditional{},
	}

	var buf bytes.Buffer
	if err := Render(&buf, res, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := buf.String()
	want := "  0 permissions for 0 resources, 0 unknowns, 0 unresolved conditionals\n"
	if got != want {
		t.Errorf("minimal output mismatch\n--- want ---\n%q\n--- got ---\n%q", want, got)
	}
}

// TestRenderUnresolvedWithModulePath pins the contract that
// UnresolvedConditional.ModulePath is rendered as the
// `module.<a>.module.<b>.` prefix used elsewhere to disambiguate
// reused-module instantiations. Without the prefix, two unresolved
// conditionals with the same ResourceType.ResourceName but differing
// module paths would render as identical lines, leaving the user
// unable to locate the right call site.
func TestRenderUnresolvedWithModulePath(t *testing.T) {
	res := resolver.Result{
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "x",
				ModulePath:   []string{"a", "b"},
				Attribute:    "uniform_bucket_level_access",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         14,
			},
			// Same ResourceType.ResourceName as the first entry but
			// a different ModulePath — the rendered lines must
			// differ so users can tell the two call sites apart.
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "x",
				ModulePath:   []string{"c"},
				Attribute:    "uniform_bucket_level_access",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         14,
			},
			// Empty ModulePath — must render without the
			// `module.` prefix so root-level resources keep their
			// existing format.
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "y",
				Attribute:    "uniform_bucket_level_access",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         20,
			},
		},
	}

	var buf bytes.Buffer
	if err := Render(&buf, res, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := buf.String()

	wantRows := []string{
		"    module.a.module.b.google_storage_bucket.x: uniform_bucket_level_access (main.tf:14) — missing_variable",
		"    module.c.google_storage_bucket.x: uniform_bucket_level_access (main.tf:14) — missing_variable",
		"    google_storage_bucket.y: uniform_bucket_level_access (main.tf:20) — missing_variable",
	}
	for _, row := range wantRows {
		if !strings.Contains(got, row) {
			t.Errorf("output missing row %q.\noutput:\n%s", row, got)
		}
	}
}

// TestRenderSummarySingular pins the singular noun form for the
// 1-of-each case. The plural helper picks "permission"/"resource"/
// "unknown"/"unresolved conditional" when the count is exactly 1, so
// the default CLI output reads correctly in the common 1-resource case.
func TestRenderSummarySingular(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{},
		TotalApplyPerms: []string{"storage.buckets.get"},
	}

	var buf bytes.Buffer
	if err := Render(&buf, res, 1); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := buf.String()
	want := "  1 permission for 1 resource, 0 unknowns, 0 unresolved conditionals\n"
	if !strings.HasPrefix(got, want) {
		t.Errorf("singular summary line wrong\n--- want prefix ---\n%q\n--- got ---\n%q", want, got)
	}
}

// TestRenderDiagnostics verifies that parse-level warnings are rendered
// under the `warnings:` header with their summary and file:line context.
func TestRenderDiagnostics(t *testing.T) {
	res := resolver.Result{
		Diagnostics: []resolver.Diagnostic{
			{Summary: "non-local module source", File: "main.tf", Line: 4},
			{Summary: "module recursion cycle", File: "mod/main.tf", Line: 10},
		},
	}

	var buf bytes.Buffer
	if err := Render(&buf, res, 1); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := buf.String()

	if !strings.HasPrefix(got, "  0 permissions for 1 resource, 0 unknowns, 0 unresolved conditionals\n") {
		t.Errorf("summary line wrong; got:\n%s", got)
	}

	if !strings.Contains(got, "  warnings (2):") {
		t.Errorf("output missing 'warnings' header.\noutput:\n%s", got)
	}

	wantRows := []string{
		"    non-local module source (main.tf:4)",
		"    module recursion cycle (mod/main.tf:10)",
	}
	for _, r := range wantRows {
		if !strings.Contains(got, r) {
			t.Errorf("output missing row %q.\noutput:\n%s", r, got)
		}
	}
}

// failingWriter accepts the first byteBudget bytes and then returns
// errBrokenPipe on every subsequent Write. It models a stdout pipe that
// the consumer closes mid-render: Render must surface that error rather
// than swallow it and exit zero with truncated output. The pattern
// mirrors the failingWriter in cmd/tfperms/catalog_stats_test.go.
type failingWriter struct {
	byteBudget int
	written    int
}

var errBrokenPipe = errors.New("simulated broken pipe")

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.written >= f.byteBudget {
		return 0, errBrokenPipe
	}
	remaining := f.byteBudget - f.written
	if len(p) <= remaining {
		f.written += len(p)
		return len(p), nil
	}
	f.written = f.byteBudget
	return remaining, errBrokenPipe
}

// TestRenderPropagatesWriteErrors guards against silent truncation: if
// the underlying writer returns an error mid-render, Render must
// surface that error rather than return nil with a half-written report.
// This is the same regression class catalog_stats_test.go pins for the
// catalog renderer; the reporter inherits the contract because both
// writers feed the same stdout pipe at the CLI boundary.
func TestRenderPropagatesWriteErrors(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"a.b.c"},
		ApplyOnlyPerms:  []string{"d.e.f"},
		TotalApplyPerms: []string{"a.b.c", "d.e.f"},
	}

	w := &failingWriter{byteBudget: 8}
	err := Render(w, res, 1)
	if err == nil {
		t.Fatal("Render with a failing writer returned nil; expected the broken-pipe error to be propagated")
	}
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("Render error chain does not wrap the underlying writer error.\nerr: %v", err)
	}
}

// shortWriter accepts every Write but reports back only the first byte
// as written, with a nil error. This violates io.Writer's contract — a
// well-behaved writer that writes fewer bytes than requested must
// return a non-nil error — but real-world misbehaving writers exist
// (custom test doubles, broken stdout wrappers), so errWriter is
// expected to detect the short write and latch io.ErrShortWrite.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 1, nil
}

// TestRenderPropagatesShortWrites pins the contract that errWriter
// detects writers returning (n < len(p), nil). Without the short-write
// latch, every Fprintln/Fprintf would silently lose bytes and the
// trailing ew.err check would return nil — exactly the silent
// truncation the renderer is supposed to prevent.
func TestRenderPropagatesShortWrites(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"a.b.c"},
		TotalApplyPerms: []string{"a.b.c"},
	}

	err := Render(shortWriter{}, res, 1)
	if err == nil {
		t.Fatal("Render with a short writer returned nil; expected io.ErrShortWrite to be latched and surfaced")
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("Render error chain does not wrap io.ErrShortWrite.\nerr: %v", err)
	}
}
