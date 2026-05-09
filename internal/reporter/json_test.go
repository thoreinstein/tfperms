package reporter

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

func TestRenderJSON(t *testing.T) {
	res := resolver.Result{
		PlanPerms:      []string{"storage.buckets.get"},
		ApplyOnlyPerms: []string{"storage.buckets.create"},
		TotalApplyPerms: []string{
			"storage.buckets.create",
			"storage.buckets.get",
		},
		Resources: []resolver.ResourceResult{
			{
				Type:     "google_storage_bucket",
				Name:     "data",
				File:     "main.tf",
				Line:     10,
				BasePlan: []string{"storage.buckets.get"},
				Applied: []resolver.AppliedConditional{
					{
						When:      map[string]any{"location": "US"},
						ApplyOnly: []string{"storage.buckets.create"},
					},
				},
			},
		},
		Unknowns: []resolver.UnknownResource{
			{Type: "unknown_type", Name: "u", File: "other.tf", Line: 20},
		},
		Unresolved: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_compute_instance",
				ResourceName: "web",
				Attribute:    "machine_type",
				Reason:       "missing_variable",
				File:         "compute.tf",
				Line:         30,
			},
		},
		Diagnostics: []resolver.Diagnostic{
			{Summary: "some warning", File: "main.tf", Line: 5},
		},
	}

	var buf bytes.Buffer
	err := RenderJSON(&buf, res, 1, "1.2.3")
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var got jsonOutput
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	want := jsonOutput{
		Version: "1.0",
		Summary: jsonSummary{
			PermissionCount: 2,
			ResourceCount:   1,
			UnknownCount:    1,
			UnresolvedCount: 1,
		},
		PlanPermissions:      []string{"storage.buckets.get"},
		ApplyOnlyPermissions: []string{"storage.buckets.create"},
		TotalApplyPermissions: []string{
			"storage.buckets.create",
			"storage.buckets.get",
		},
		Resources: []jsonResource{
			{
				Type: "google_storage_bucket",
				Name: "data",
				File: "main.tf",
				Line: 10,
				Permissions: []string{
					"storage.buckets.create",
					"storage.buckets.get",
				},
			},
		},
		Diagnostics: []resolver.Diagnostic{
			{Summary: "some warning", File: "main.tf", Line: 5},
		},
		Unknowns: []resolver.UnknownResource{
			{Type: "unknown_type", Name: "u", File: "other.tf", Line: 20},
		},
		UnresolvedConditionals: []resolver.UnresolvedConditional{
			{
				ResourceType: "google_compute_instance",
				ResourceName: "web",
				Attribute:    "machine_type",
				Reason:       "missing_variable",
				File:         "compute.tf",
				Line:         30,
			},
		},
		Metadata: jsonMetadata{
			TFPermsVersion: "1.2.3",
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatch\ngot: %+v\nwant: %+v", got, want)
	}
}

// TestRenderJSONDeterministic verifies that two RenderJSON calls on the
// same input produce byte-identical output. This is the v1.0 stability
// contract documented in docs/json-schema.md ("Identical inputs will
// produce bit-identical JSON output, making it safe for diff usage in
// CI/CD pipelines"). Regressions here — most plausibly someone adding a
// time.Now() back into the metadata block — must fail this test.
func TestRenderJSONDeterministic(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create"},
		TotalApplyPerms: []string{"storage.buckets.create", "storage.buckets.get"},
		Resources: []resolver.ResourceResult{
			{
				Type:     "google_storage_bucket",
				Name:     "data",
				File:     "main.tf",
				Line:     10,
				BasePlan: []string{"storage.buckets.get"},
			},
		},
	}

	var first, second bytes.Buffer
	if err := RenderJSON(&first, res, 1, "1.2.3"); err != nil {
		t.Fatalf("first RenderJSON: %v", err)
	}
	if err := RenderJSON(&second, res, 1, "1.2.3"); err != nil {
		t.Fatalf("second RenderJSON: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Errorf("RenderJSON not deterministic across runs.\n--- first ---\n%s\n--- second ---\n%s",
			first.String(), second.String())
	}
}

// TestRenderJSONEmptyDiagnostics pins the JSON v1.0 stability contract
// for empty top-level slices: when Result has nil (or empty) Unknowns,
// Unresolved, and Diagnostics fields, the rendered JSON MUST surface
// each key with an empty array `[]`, never `null` and never absent.
//
// Programmatic consumers (CI gates, dashboards) iterate over these
// arrays unconditionally; a `null` would force every consumer to
// special-case it, and an absent key would silently change shape under
// the v1.0 schema. Canonicalize is the single source of truth for the
// non-nil empty-slice invariant — every formatter calls it on entry,
// so a regression that drops Canonicalize from RenderJSON or that
// changes Canonicalize to return nil empties would fail this test
// rather than produce schema-breaking output downstream.
//
// Asserting on the raw JSON bytes (rather than just unmarshaling and
// checking len == 0) is deliberate: an unmarshal of `null` into a
// []T also yields a length-zero slice, so a structural-only assertion
// would not catch the regression. The literal `"unknowns": []` substring
// pins the on-the-wire shape.
func TestRenderJSONEmptyDiagnostics(t *testing.T) {
	// All diagnostic fields nil — the "clean run" shape resolver.Resolve
	// returns when no unknowns or unresolved conditionals were seen.
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		ApplyOnlyPerms:  []string{"storage.buckets.create"},
		TotalApplyPerms: []string{"storage.buckets.create", "storage.buckets.get"},
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, res, 1, "1.2.3"); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	got := buf.String()

	// Each diagnostic key must appear with an empty array literal. The
	// indented form (`": []`) matches the encoder.SetIndent("", "  ")
	// configuration; a regression that flips to `: null` or omits the
	// key entirely fails this substring check.
	wantSubstrings := []string{
		`"diagnostics": []`,
		`"unknowns": []`,
		`"unresolved_conditionals": []`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q — empty diagnostic slice rendered as null or omitted.\noutput:\n%s",
				want, got)
		}
	}

	// Defence in depth: parse the JSON back and confirm the slices are
	// non-nil. A `null` literal unmarshals to a nil []T, so this catches
	// any regression that the substring check might miss (e.g. a future
	// schema change that re-keys the field).
	var parsed jsonOutput
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if parsed.Diagnostics == nil {
		t.Errorf("Diagnostics unmarshaled to nil; expected non-nil empty slice")
	}
	if parsed.Unknowns == nil {
		t.Errorf("Unknowns unmarshaled to nil; expected non-nil empty slice")
	}
	if parsed.UnresolvedConditionals == nil {
		t.Errorf("UnresolvedConditionals unmarshaled to nil; expected non-nil empty slice")
	}
}

// TestRenderJSONWriterError confirms that a failing io.Writer surfaces as
// a wrapped error from RenderJSON. The errWriter adapter latches the
// underlying error and the trailing check returns it wrapped with the
// "write json" prefix, mirroring the role.go pattern. Reuses the
// failingWriter and errBrokenPipe defined in reporter_test.go.
func TestRenderJSONWriterError(t *testing.T) {
	res := resolver.Result{
		PlanPerms:       []string{"storage.buckets.get"},
		TotalApplyPerms: []string{"storage.buckets.get"},
	}

	w := &failingWriter{byteBudget: 0}
	err := RenderJSON(w, res, 0, "1.2.3")
	if err == nil {
		t.Fatal("RenderJSON returned nil error for failing writer; expected wrapped error")
	}
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("RenderJSON error does not wrap underlying writer error: got %v", err)
	}
	if !strings.Contains(err.Error(), "write json") {
		t.Errorf("RenderJSON error missing %q prefix: got %v", "write json", err)
	}
}
