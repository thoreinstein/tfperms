package resolver

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/thoreinstein/tfperms/internal/catalog"
	"github.com/thoreinstein/tfperms/internal/parser"
)

// TestResolveDeduplicatesIdenticalResources verifies that multiple instances
// of the same resource type with identical attributes produce the same
// permission set as a single instance (full deduplication).
func TestResolveDeduplicatesIdenticalResources(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", nil)

	// Two identical buckets.
	resources := []parser.Resource{
		{Kind: "resource", Type: "google_storage_bucket", Name: "bucket_1"},
		{Kind: "resource", Type: "google_storage_bucket", Name: "bucket_2"},
	}

	res := Resolve(resources, cat, ResolveOptions{})

	// Should be identical to singleResourceCatalog base perms.
	wantPlan := []string{"storage.buckets.get"}
	wantApplyOnly := []string{
		"storage.buckets.create",
		"storage.buckets.delete",
		"storage.buckets.update",
	}
	assertSliceEqual(t, "plan_perms", res.PlanPerms, wantPlan)
	assertSliceEqual(t, "apply_only_perms", res.ApplyOnlyPerms, wantApplyOnly)
}

// TestResolveUnionsVaryingConditionals verifies that if multiple instances
// of the same resource type have different attribute values, the resolver
// unions all resulting permissions.
func TestResolveUnionsVaryingConditionals(t *testing.T) {
	cat := singleResourceCatalog(t, "google_storage_bucket", []catalog.Conditional{
		{
			When: map[string]any{"uniform_bucket_level_access": true},
			Permissions: catalog.PermissionSet{
				Plan: []string{"storage.buckets.getIamPolicy"},
			},
		},
		{
			When: map[string]any{"versioning": true},
			Permissions: catalog.PermissionSet{
				Plan: []string{"storage.buckets.getVersioning"},
			},
		},
	})

	// Instance 1 triggers first conditional.
	// Instance 2 triggers second conditional.
	resources := []parser.Resource{
		{
			Kind:  "resource",
			Type:  "google_storage_bucket",
			Name:  "bucket_1",
			Attrs: map[string]cty.Value{"uniform_bucket_level_access": cty.True},
		},
		{
			Kind:  "resource",
			Type:  "google_storage_bucket",
			Name:  "bucket_2",
			Attrs: map[string]cty.Value{"versioning": cty.True},
		},
	}

	res := Resolve(resources, cat, ResolveOptions{})

	// Plan should include BOTH conditional permissions.
	wantPlan := []string{
		"storage.buckets.get",
		"storage.buckets.getIamPolicy",
		"storage.buckets.getVersioning",
	}
	assertSliceEqual(t, "plan_perms", res.PlanPerms, wantPlan)
}

// TestResolveDeduplicatesOverlappingPermissions verifies that distinct
// resource types with overlapping permissions result in the permission
// appearing only once in the output.
func TestResolveDeduplicatesOverlappingPermissions(t *testing.T) {
	cat := &catalog.Catalog{
		Resources: map[string]*catalog.ResourceEntry{
			"google_storage_bucket": {
				Type: "google_storage_bucket",
				Permissions: catalog.PermissionSet{
					Plan: []string{"storage.buckets.get", "common.permission"},
				},
			},
			"google_compute_instance": {
				Type: "google_compute_instance",
				Permissions: catalog.PermissionSet{
					Plan: []string{"compute.instances.get", "common.permission"},
				},
			},
		},
		DataSources: map[string]*catalog.DataSourceEntry{},
		IAMBindings: map[string]*catalog.IAMBindingEntry{},
	}

	resources := []parser.Resource{
		{Kind: "resource", Type: "google_storage_bucket", Name: "b"},
		{Kind: "resource", Type: "google_compute_instance", Name: "i"},
	}

	res := Resolve(resources, cat, ResolveOptions{})

	// "common.permission" should appear only once.
	wantPlan := []string{
		"common.permission",
		"compute.instances.get",
		"storage.buckets.get",
	}
	assertSliceEqual(t, "plan_perms", res.PlanPerms, wantPlan)
}
