package reporter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

func TestRenderJSON(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
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
	err := RenderJSON(&buf, res, 1, "1.2.3", now)
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
			GeneratedAt:    now,
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatch\ngot: %+v\nwant: %+v", got, want)
	}
}
