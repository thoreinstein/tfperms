package reporter

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

// TestRenderByResourceFull exercises the all-sections-populated path:
// two resource types, one with a firing conditional, plus warnings,
// unknowns, and unresolved conditionals. Asserts the format contract
// pins documented on RenderByResource — summary line, group header
// with instance count, indented instance rows, per-stage permission
// sections, and the `# from conditional` annotation for permissions
// sourced from a firing conditional.
func TestRenderByResourceFull(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"compute.disks.get", "storage.buckets.get", "storage.buckets.getIamPolicy"},
		ApplyOnlyPerms:  []string{"compute.disks.create", "storage.buckets.create", "storage.buckets.setIamPolicy"},
		TotalApplyPerms: []string{"compute.disks.create", "compute.disks.get", "storage.buckets.create", "storage.buckets.get", "storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy"},
		Resources: []resolver.ResourceResult{
			{
				Type:          "google_storage_bucket",
				Name:          "primary",
				File:          "main.tf",
				Line:          10,
				BasePlan:      []string{"storage.buckets.get"},
				BaseApplyOnly: []string{"storage.buckets.create"},
				Applied: []resolver.AppliedConditional{
					{
						When:      map[string]any{"uniform_bucket_level_access": true},
						Plan:      []string{"storage.buckets.getIamPolicy"},
						ApplyOnly: []string{"storage.buckets.setIamPolicy"},
					},
				},
			},
			{
				Type:          "google_storage_bucket",
				Name:          "lookup",
				File:          "main.tf",
				Line:          16,
				BasePlan:      []string{"storage.buckets.get"},
				BaseApplyOnly: []string{"storage.buckets.create"},
			},
			{
				Type:          "google_compute_disk",
				Name:          "data",
				File:          "compute.tf",
				Line:          5,
				BasePlan:      []string{"compute.disks.get"},
				BaseApplyOnly: []string{"compute.disks.create"},
			},
		},
		Diagnostics: []resolver.Diagnostic{
			{Summary: "non-local module source", File: "main.tf", Line: 4},
		},
		Unknowns: []resolver.UnknownResource{
			{Type: "google_dataplex_lake", Name: "primary", File: "main.tf", Line: 42},
		},
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "data",
				Attribute:    "versioning",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         14,
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderByResource(&buf, res, 3, false); err != nil {
		t.Fatalf("RenderByResource: %v", err)
	}
	got := buf.String()

	// Summary line: same shape as flat-list Render.
	if !strings.HasPrefix(got, "  6 permissions for 3 resources, 1 unknown, 1 unresolved conditional\n") {
		t.Errorf("summary line wrong; got:\n%s", got)
	}

	// Group headers: types alphabetised. compute_disk before storage_bucket.
	wantHeaders := []string{
		"  google_compute_disk (1 instance):",
		"  google_storage_bucket (2 instances):",
		"  warnings (1):",
		"  unknown resources (1):",
		"  unresolved conditionals (1):",
	}
	for _, h := range wantHeaders {
		if !strings.Contains(got, h) {
			t.Errorf("output missing %q header.\noutput:\n%s", h, got)
		}
	}

	// Instance rows: 4-space indent, type.name (file:line) form.
	wantInstances := []string{
		"    google_compute_disk.data (compute.tf:5)",
		"    google_storage_bucket.primary (main.tf:10)",
		"    google_storage_bucket.lookup (main.tf:16)",
	}
	for _, r := range wantInstances {
		if !strings.Contains(got, r) {
			t.Errorf("output missing instance row %q.\noutput:\n%s", r, got)
		}
	}

	// The base-only permission rows have no annotation. The
	// conditional-only permissions carry the `# from conditional` tag.
	wantUnannotated := []string{
		"    storage.buckets.get\n",
		"    storage.buckets.create\n",
		"    compute.disks.get\n",
		"    compute.disks.create\n",
	}
	for _, r := range wantUnannotated {
		if !strings.Contains(got, r) {
			t.Errorf("output missing un-annotated row %q.\noutput:\n%s", r, got)
		}
	}
	wantAnnotated := []string{
		"    storage.buckets.getIamPolicy  # from conditional uniform_bucket_level_access=true\n",
		"    storage.buckets.setIamPolicy  # from conditional uniform_bucket_level_access=true\n",
	}
	for _, r := range wantAnnotated {
		if !strings.Contains(got, r) {
			t.Errorf("output missing annotated row %q.\noutput:\n%s", r, got)
		}
	}

	// The instance-count noun must agree singular/plural per group.
	if strings.Contains(got, "google_compute_disk (1 instances)") {
		t.Errorf("singular case rendered as plural; output:\n%s", got)
	}
}

// TestRenderByResourceMinimal pins the empty-Result contract: one
// summary line, no sections at all. Mirrors TestRenderMinimal for the
// flat reporter.
func TestRenderByResourceMinimal(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderByResource(&buf, resolver.Result{}, 0, false); err != nil {
		t.Fatalf("RenderByResource: %v", err)
	}
	got := buf.String()
	want := "  0 permissions for 0 resources, 0 unknowns, 0 unresolved conditionals\n"
	if got != want {
		t.Errorf("minimal output mismatch\n--- want ---\n%q\n--- got ---\n%q", want, got)
	}
}

// TestRenderByResourceTypesSortedAlphabetically pins the group-order
// contract: groups are emitted in alphabetical order of Type, even
// when the resolver hands the resources out in a different order.
// Without this, two runs against the same input could surface groups
// in different positions, breaking the diff-stable contract.
func TestRenderByResourceTypesSortedAlphabetically(t *testing.T) {
	res := resolver.Result{
		Resources: []resolver.ResourceResult{
			{Type: "google_storage_bucket", Name: "x", File: "main.tf", Line: 10, BasePlan: []string{"storage.buckets.get"}},
			{Type: "google_compute_disk", Name: "y", File: "main.tf", Line: 5, BasePlan: []string{"compute.disks.get"}},
			{Type: "google_bigquery_dataset", Name: "z", File: "main.tf", Line: 20, BasePlan: []string{"bigquery.datasets.get"}},
		},
	}

	var buf bytes.Buffer
	if err := RenderByResource(&buf, res, 3, false); err != nil {
		t.Fatalf("RenderByResource: %v", err)
	}
	got := buf.String()

	bqIdx := strings.Index(got, "  google_bigquery_dataset (")
	cdIdx := strings.Index(got, "  google_compute_disk (")
	sbIdx := strings.Index(got, "  google_storage_bucket (")
	if bqIdx < 0 || cdIdx < 0 || sbIdx < 0 {
		t.Fatalf("output missing one or more group headers\noutput:\n%s", got)
	}
	if !(bqIdx < cdIdx && cdIdx < sbIdx) {
		t.Errorf("groups out of alphabetical order:\n bigquery=%d, compute=%d, storage=%d\noutput:\n%s",
			bqIdx, cdIdx, sbIdx, got)
	}
}

// TestRenderByResourceConditionalAnnotationOnlyForConditionalOnly
// pins the conditional-annotation rule: a permission appearing in
// both base AND a firing conditional is NOT annotated, because the
// permission is in the resource's catalog entry's base set
// regardless of the conditional. Only permissions exclusively
// contributed by a firing conditional carry the `# from conditional`
// tag.
//
// Without this rule, every base permission on a resource that fires
// any conditional sharing the same string would surface annotations,
// drowning out the actual signal (which permissions came from where).
func TestRenderByResourceConditionalAnnotationOnlyForConditionalOnly(t *testing.T) {
	res := resolver.Result{
		Resources: []resolver.ResourceResult{
			{
				Type:     "google_storage_bucket",
				Name:     "primary",
				File:     "main.tf",
				Line:     10,
				BasePlan: []string{"storage.buckets.get"},
				Applied: []resolver.AppliedConditional{
					{
						When: map[string]any{"versioning": true},
						// `storage.buckets.get` ALSO appears in base —
						// it must NOT be annotated. `storage.buckets.getVersioning`
						// is conditional-only — it MUST be annotated.
						Plan: []string{"storage.buckets.get", "storage.buckets.getVersioning"},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderByResource(&buf, res, 1, false); err != nil {
		t.Fatalf("RenderByResource: %v", err)
	}
	got := buf.String()

	// storage.buckets.get is in base — should render WITHOUT annotation.
	if !strings.Contains(got, "    storage.buckets.get\n") {
		t.Errorf("storage.buckets.get should render without annotation (it is in base):\n%s", got)
	}
	if strings.Contains(got, "storage.buckets.get  # from conditional") {
		t.Errorf("storage.buckets.get is in base; must not be annotated as from-conditional:\n%s", got)
	}
	// storage.buckets.getVersioning is conditional-only — must be annotated.
	wantAnnotated := "    storage.buckets.getVersioning  # from conditional versioning=true\n"
	if !strings.Contains(got, wantAnnotated) {
		t.Errorf("output missing annotated row %q.\noutput:\n%s", wantAnnotated, got)
	}
}

// TestRenderByResourcePropagatesWriteErrors pins the broken-pipe
// surfacing contract — same as Render. A failingWriter returning an
// error mid-render must produce a non-nil return wrapping the
// underlying error.
func TestRenderByResourcePropagatesWriteErrors(t *testing.T) {
	res := resolver.Result{
		Resources: []resolver.ResourceResult{
			{Type: "google_storage_bucket", Name: "x", File: "main.tf", Line: 1, BasePlan: []string{"storage.buckets.get"}},
		},
	}

	w := &failingWriter{byteBudget: 8}
	err := RenderByResource(w, res, 1, false)
	if err == nil {
		t.Fatal("RenderByResource with a failing writer returned nil; expected wrapped error")
	}
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("RenderByResource error chain does not wrap underlying writer error.\nerr: %v", err)
	}
}

// TestRenderByResourcePropagatesShortWrites pins the short-write
// detection contract from errWriter for the by-resource format.
func TestRenderByResourcePropagatesShortWrites(t *testing.T) {
	res := resolver.Result{
		Resources: []resolver.ResourceResult{
			{Type: "google_storage_bucket", Name: "x", File: "main.tf", Line: 1, BasePlan: []string{"storage.buckets.get"}},
		},
	}

	err := RenderByResource(shortWriter{}, res, 1, false)
	if err == nil {
		t.Fatal("RenderByResource with a short writer returned nil; expected io.ErrShortWrite to be latched and surfaced")
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("RenderByResource error chain does not wrap io.ErrShortWrite.\nerr: %v", err)
	}
}

// TestRenderByResourceDeterministic pins the determinism contract:
// two runs against the same Result produce byte-identical output.
// Builds on Canonicalize's idempotency to guarantee no map-iteration
// leaks into the rendered text.
func TestRenderByResourceDeterministic(t *testing.T) {
	res := shuffledFixture()

	var first, second bytes.Buffer
	if err := RenderByResource(&first, res, 5, false); err != nil {
		t.Fatalf("first RenderByResource: %v", err)
	}
	if err := RenderByResource(&second, res, 5, false); err != nil {
		t.Fatalf("second RenderByResource: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("two RenderByResource runs produced different output\n--- first ---\n%s\n--- second ---\n%s",
			first.String(), second.String())
	}
}

// TestRenderByResourceQuietSuppressesDiagnosticSections pins the
// quiet contract for the by-resource formatter: when quiet is true,
// the `unknown resources` and `unresolved conditionals` sections are
// suppressed entirely (no header, no body, no leading blank line).
// The summary line still reports accurate counts. Group bodies and
// the warnings section remain unaffected because quiet only targets
// catalog-gap diagnostics (Journey 3 noise), not the per-resource
// breakdown or parser warnings.
func TestRenderByResourceQuietSuppressesDiagnosticSections(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create"},
		TotalApplyPerms: []string{"storage.buckets.create", "storage.buckets.get"},
		Resources: []resolver.ResourceResult{
			{
				Type:          "google_storage_bucket",
				Name:          "primary",
				File:          "main.tf",
				Line:          10,
				BasePlan:      []string{"storage.buckets.get"},
				BaseApplyOnly: []string{"storage.buckets.create"},
			},
		},
		Diagnostics: []resolver.Diagnostic{
			{Summary: "non-local module source", File: "main.tf", Line: 4},
		},
		Unknowns: []resolver.UnknownResource{
			{Type: "google_dataplex_lake", Name: "primary", File: "main.tf", Line: 42},
		},
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_storage_bucket",
				ResourceName: "data",
				Attribute:    "versioning",
				Reason:       "missing_variable",
				File:         "main.tf",
				Line:         14,
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderByResource(&buf, res, 1, true); err != nil {
		t.Fatalf("RenderByResource: %v", err)
	}
	got := buf.String()

	// Summary line still reports the diagnostic counts even though
	// the detail rows are suppressed.
	if !strings.HasPrefix(got, "  2 permissions for 1 resource, 1 unknown, 1 unresolved conditional\n") {
		t.Errorf("summary line should retain accurate counts under quiet; got:\n%s", got)
	}

	// Suppressed sections — anchor on the header form to avoid
	// false-matching the summary line's bare phrase.
	if strings.Contains(got, "unknown resources (") {
		t.Errorf("quiet output should not contain 'unknown resources' header.\noutput:\n%s", got)
	}
	if strings.Contains(got, "unresolved conditionals (") {
		t.Errorf("quiet output should not contain 'unresolved conditionals' header.\noutput:\n%s", got)
	}
	if strings.Contains(got, "google_dataplex_lake.primary") {
		t.Errorf("quiet output should not contain unknown-resource detail row.\noutput:\n%s", got)
	}
	if strings.Contains(got, "versioning") {
		t.Errorf("quiet output should not contain unresolved-conditional detail row.\noutput:\n%s", got)
	}

	// Group, permission, and warning sections must still render —
	// quiet only targets the catalog-gap diagnostic sections.
	if !strings.Contains(got, "  google_storage_bucket (1 instance):") {
		t.Errorf("quiet output should still render group header.\noutput:\n%s", got)
	}
	if !strings.Contains(got, "  warnings (1):") {
		t.Errorf("quiet output should still render warnings.\noutput:\n%s", got)
	}
}

// TestRenderByResourceQuietWithEmptyDiagnosticsIsNoOp mirrors the flat
// formatter's noop-on-clean test: --quiet against a Result with no
// unknowns or unresolved conditionals must produce byte-identical
// output to the non-quiet path.
func TestRenderByResourceQuietWithEmptyDiagnosticsIsNoOp(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create"},
		TotalApplyPerms: []string{"storage.buckets.create", "storage.buckets.get"},
		Resources: []resolver.ResourceResult{
			{
				Type:          "google_storage_bucket",
				Name:          "primary",
				File:          "main.tf",
				Line:          10,
				BasePlan:      []string{"storage.buckets.get"},
				BaseApplyOnly: []string{"storage.buckets.create"},
			},
		},
	}

	var verbose, quiet bytes.Buffer
	if err := RenderByResource(&verbose, res, 1, false); err != nil {
		t.Fatalf("RenderByResource verbose: %v", err)
	}
	if err := RenderByResource(&quiet, res, 1, true); err != nil {
		t.Fatalf("RenderByResource quiet: %v", err)
	}
	if verbose.String() != quiet.String() {
		t.Errorf("quiet should be a noop on a clean Result\n--- verbose ---\n%s\n--- quiet ---\n%s",
			verbose.String(), quiet.String())
	}
}

// TestRenderByResourceModulePathRendered pins that the
// `module.<a>.<b>.` prefix surfaces on instance rows so reused-module
// instantiations are distinguishable in the by-resource view.
func TestRenderByResourceModulePathRendered(t *testing.T) {
	res := resolver.Result{
		Resources: []resolver.ResourceResult{
			{
				Type:       "google_storage_bucket",
				Name:       "shared",
				File:       "mod-c/main.tf",
				Line:       1,
				ModulePath: []string{"a", "shared"},
				BasePlan:   []string{"storage.buckets.get"},
			},
			{
				Type:       "google_storage_bucket",
				Name:       "shared",
				File:       "mod-c/main.tf",
				Line:       1,
				ModulePath: []string{"b", "shared"},
				BasePlan:   []string{"storage.buckets.get"},
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderByResource(&buf, res, 2, false); err != nil {
		t.Fatalf("RenderByResource: %v", err)
	}
	got := buf.String()

	wantRows := []string{
		"    module.a.module.shared.google_storage_bucket.shared (mod-c/main.tf:1)",
		"    module.b.module.shared.google_storage_bucket.shared (mod-c/main.tf:1)",
	}
	for _, r := range wantRows {
		if !strings.Contains(got, r) {
			t.Errorf("output missing module-prefixed row %q.\noutput:\n%s", r, got)
		}
	}
}
