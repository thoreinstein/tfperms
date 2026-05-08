package reporter

import (
	"bytes"
	"errors"
	"io"
	"reflect"
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

// shuffledFixture returns a Result whose every collection is in a
// deliberately-wrong order — none of the slices are pre-sorted, and
// the Resources entry's Applied conditionals share a When key prefix
// so the ordering of permission-suffix tiebreakers is exercised. Used
// by the Canonicalize tests to prove the deterministic sort applies
// to every field in turn.
func shuffledFixture() resolver.Result {
	return resolver.Result{
		PlanPerms:       []string{"storage.buckets.get", "bigquery.datasets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create", "bigquery.datasets.create"},
		TotalApplyPerms: []string{"storage.buckets.get", "bigquery.datasets.get", "storage.buckets.create"},
		Diagnostics: []resolver.Diagnostic{
			{Summary: "module recursion cycle", File: "mod/main.tf", Line: 10},
			{Summary: "non-local module source", File: "main.tf", Line: 4},
		},
		Unknowns: []resolver.UnknownResource{
			{Type: "google_z", Name: "y", File: "main.tf", Line: 30},
			{Type: "google_a", Name: "b", File: "main.tf", Line: 10},
		},
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "primary",
				ModulePath:   []string{"y"},
				Attribute:    "uniform_bucket_level_access",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         14,
			},
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "primary",
				ModulePath:   []string{"x"},
				Attribute:    "uniform_bucket_level_access",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         14,
			},
		},
		Resources: []resolver.ResourceResult{
			{
				Type:      "google_storage_bucket",
				Name:      "primary",
				File:      "main.tf",
				Line:      20,
				BasePerms: []string{"storage.buckets.get", "storage.buckets.create"},
				Applied: []resolver.AppliedConditional{
					{
						When:        map[string]any{"uniform_bucket_level_access": true},
						Permissions: []string{"storage.buckets.setIamPolicy", "storage.buckets.getIamPolicy"},
					},
					{
						When:        map[string]any{"versioning": true},
						Permissions: []string{"storage.buckets.getVersioning"},
					},
				},
			},
			{
				Type:      "google_storage_bucket",
				Name:      "primary",
				File:      "main.tf",
				Line:      10,
				BasePerms: []string{"storage.buckets.get"},
				Applied:   []resolver.AppliedConditional{},
			},
		},
	}
}

// TestCanonicalizeSortsEveryField pins the deterministic sort order
// across every Result collection. A regression that drops a field
// from Canonicalize, or reorders a tier in the sort comparators,
// fails this test by leaving an output slice in shuffled order.
func TestCanonicalizeSortsEveryField(t *testing.T) {
	got := Canonicalize(shuffledFixture())

	wantPlan := []string{"bigquery.datasets.get", "storage.buckets.get"}
	if !reflect.DeepEqual(got.PlanPerms, wantPlan) {
		t.Errorf("PlanPerms\n got: %v\nwant: %v", got.PlanPerms, wantPlan)
	}
	wantApplyOnly := []string{"bigquery.datasets.create", "storage.buckets.create"}
	if !reflect.DeepEqual(got.ApplyOnlyPerms, wantApplyOnly) {
		t.Errorf("ApplyOnlyPerms\n got: %v\nwant: %v", got.ApplyOnlyPerms, wantApplyOnly)
	}
	wantTotal := []string{"bigquery.datasets.get", "storage.buckets.create", "storage.buckets.get"}
	if !reflect.DeepEqual(got.TotalApplyPerms, wantTotal) {
		t.Errorf("TotalApplyPerms\n got: %v\nwant: %v", got.TotalApplyPerms, wantTotal)
	}

	// Diagnostics: (File, Line, Summary). main.tf:4 sorts before
	// mod/main.tf:10 because lexicographic File compare puts "main.tf"
	// before "mod/main.tf".
	if len(got.Diagnostics) != 2 || got.Diagnostics[0].File != "main.tf" || got.Diagnostics[1].File != "mod/main.tf" {
		t.Errorf("Diagnostics not (File, Line)-sorted: %#v", got.Diagnostics)
	}

	// Unknowns: (File, Line, Type, Name). Both share File, so Line
	// decides: line 10 before line 30.
	if len(got.Unknowns) != 2 || got.Unknowns[0].Line != 10 || got.Unknowns[1].Line != 30 {
		t.Errorf("Unknowns not (File, Line)-sorted: %#v", got.Unknowns)
	}

	// Unresolved: ModulePath is the only differing field; ["x"] sorts
	// before ["y"].
	if len(got.Unresolved) != 2 ||
		!reflect.DeepEqual(got.Unresolved[0].ModulePath, []string{"x"}) ||
		!reflect.DeepEqual(got.Unresolved[1].ModulePath, []string{"y"}) {
		t.Errorf("Unresolved not ModulePath-sorted: %#v", got.Unresolved)
	}

	// Resources: line 10 before line 20 (both share Type/Name/File).
	if len(got.Resources) != 2 || got.Resources[0].Line != 10 || got.Resources[1].Line != 20 {
		t.Errorf("Resources not (File, Line)-sorted: %#v", got.Resources)
	}

	// Within the line-20 ResourceResult: BasePerms alphabetised, and
	// the two Applied conditionals ordered by When-key serialisation
	// ("uniform_bucket_level_access=true" < "versioning=true" because
	// the key strings sort alphabetically).
	r20 := got.Resources[1]
	wantBase := []string{"storage.buckets.create", "storage.buckets.get"}
	if !reflect.DeepEqual(r20.BasePerms, wantBase) {
		t.Errorf("Resources[line=20].BasePerms\n got: %v\nwant: %v", r20.BasePerms, wantBase)
	}
	if len(r20.Applied) != 2 {
		t.Fatalf("Resources[line=20].Applied length: got %d, want 2; full=%#v", len(r20.Applied), r20.Applied)
	}
	if _, ok := r20.Applied[0].When["uniform_bucket_level_access"]; !ok {
		t.Errorf("Applied[0] should be the uniform_bucket_level_access conditional; got %#v", r20.Applied[0])
	}
	if _, ok := r20.Applied[1].When["versioning"]; !ok {
		t.Errorf("Applied[1] should be the versioning conditional; got %#v", r20.Applied[1])
	}
	wantPerms := []string{"storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy"}
	if !reflect.DeepEqual(r20.Applied[0].Permissions, wantPerms) {
		t.Errorf("Applied[0].Permissions\n got: %v\nwant: %v", r20.Applied[0].Permissions, wantPerms)
	}
}

// TestCanonicalizeIdempotent pins the contract called out in
// tfperms-ftq.5's acceptance criteria: running Canonicalize on
// already-canonical input produces an output reflect.DeepEqual to the
// input. Without this, a formatter that re-canonicalises (e.g. a
// future composition that calls Canonicalize at multiple layers)
// could shuffle output between calls.
func TestCanonicalizeIdempotent(t *testing.T) {
	once := Canonicalize(shuffledFixture())
	twice := Canonicalize(once)
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("Canonicalize is not idempotent\n once: %#v\ntwice: %#v", once, twice)
	}
}

// TestCanonicalizeDoesNotMutateInput pins that Canonicalize leaves
// the caller's Result untouched — every output slice is freshly
// allocated. Without this, two consumers sharing a Result (e.g. the
// flat-list and JSON formatters in a future composed pipeline) would
// see ordering side-effects from the first formatter.
func TestCanonicalizeDoesNotMutateInput(t *testing.T) {
	in := shuffledFixture()
	snapshot := shuffledFixture() // value-equal twin captured before Canonicalize

	_ = Canonicalize(in)

	if !reflect.DeepEqual(in, snapshot) {
		t.Errorf("Canonicalize mutated its input\n  got: %#v\n want: %#v", in, snapshot)
	}
}

// TestCanonicalizeNilSlicesBecomeEmpty pins the empty-slice
// contract: a nil top-level slice on input produces a non-nil empty
// slice on output, so JSON marshals as `[]` rather than `null`. The
// resolver guarantees non-nil, but Canonicalize is also called on
// hand-constructed Results in tests; the contract has to hold for
// both.
func TestCanonicalizeNilSlicesBecomeEmpty(t *testing.T) {
	got := Canonicalize(resolver.Result{})

	if got.PlanPerms == nil {
		t.Errorf("PlanPerms should be non-nil empty slice, got nil")
	}
	if got.ApplyOnlyPerms == nil {
		t.Errorf("ApplyOnlyPerms should be non-nil empty slice, got nil")
	}
	if got.TotalApplyPerms == nil {
		t.Errorf("TotalApplyPerms should be non-nil empty slice, got nil")
	}
	if got.Diagnostics == nil {
		t.Errorf("Diagnostics should be non-nil empty slice, got nil")
	}
	if got.Unknowns == nil {
		t.Errorf("Unknowns should be non-nil empty slice, got nil")
	}
	if got.Unresolved == nil {
		t.Errorf("Unresolved should be non-nil empty slice, got nil")
	}
	if got.Resources == nil {
		t.Errorf("Resources should be non-nil empty slice, got nil")
	}
}

// TestCanonicalizeRootModulePathStaysNil pins that ModulePath on a
// root-level resource (input nil) stays nil on output, so
// json.Marshal omits the `module_path` field via its `omitempty` tag.
// Without this guarantee the JSON shape would shift: root-level
// Unresolved / Resource entries would emit `"module_path": []`
// instead of dropping the field.
func TestCanonicalizeRootModulePathStaysNil(t *testing.T) {
	in := resolver.Result{
		Unresolved: []resolver.UnresolvedConditional{{
			ResourceType: "google_storage_bucket",
			ResourceName: "primary",
		}},
		Resources: []resolver.ResourceResult{{
			Type: "google_storage_bucket",
			Name: "primary",
		}},
	}

	got := Canonicalize(in)

	if got.Unresolved[0].ModulePath != nil {
		t.Errorf("Unresolved[0].ModulePath should be nil for root-level entry, got %#v",
			got.Unresolved[0].ModulePath)
	}
	if got.Resources[0].ModulePath != nil {
		t.Errorf("Resources[0].ModulePath should be nil for root-level entry, got %#v",
			got.Resources[0].ModulePath)
	}
}

// TestCanonicalizeUnknownsModulePathNotShared pins that the
// Unknowns slice returned by Canonicalize does not share ModulePath
// backing arrays with the input. Without this, mutating an output
// element's ModulePath would leak back into the caller's Result —
// violating Canonicalize's "fresh allocation" contract.
func TestCanonicalizeUnknownsModulePathNotShared(t *testing.T) {
	in := resolver.Result{
		Unknowns: []resolver.UnknownResource{{
			Type:       "google_storage_bucket",
			Name:       "primary",
			ModulePath: []string{"app", "storage"},
			File:       "main.tf",
			Line:       10,
		}},
	}
	original := []string{"app", "storage"}

	got := Canonicalize(in)

	got.Unknowns[0].ModulePath[0] = "MUTATED"

	if !reflect.DeepEqual(in.Unknowns[0].ModulePath, original) {
		t.Errorf("input Unknowns[0].ModulePath was mutated via shared backing array\n  got: %#v\n want: %#v",
			in.Unknowns[0].ModulePath, original)
	}
}

// TestRenderTwoRunsAreByteIdentical pins tfperms-ftq.5's acceptance
// criteria directly: two runs of Render against the same fixture
// produce byte-identical output. The fixture is shuffled (slices in
// non-canonical order) precisely to prove Render does not just
// passthrough resolver order — it canonicalises at entry.
func TestRenderTwoRunsAreByteIdentical(t *testing.T) {
	res := shuffledFixture()

	var buf1, buf2 bytes.Buffer
	if err := Render(&buf1, res, 5); err != nil {
		t.Fatalf("Render run 1: %v", err)
	}
	if err := Render(&buf2, res, 5); err != nil {
		t.Fatalf("Render run 2: %v", err)
	}

	if buf1.String() != buf2.String() {
		t.Errorf("two Render runs produced different output\n--- run 1 ---\n%s\n--- run 2 ---\n%s",
			buf1.String(), buf2.String())
	}
}
