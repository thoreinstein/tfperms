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
				Type:      "google_storage_bucket",
				Name:      "data",
				File:      "main.tf",
				Line:      10,
				BasePerms: []string{"storage.buckets.get"},
				Applied: []resolver.AppliedConditional{
					{
						When:        map[string]any{"location": "US"},
						Permissions: []string{"storage.buckets.create"},
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
				Type:      "google_storage_bucket",
				Name:      "data",
				File:      "main.tf",
				Line:      10,
				BasePerms: []string{"storage.buckets.get"},
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
