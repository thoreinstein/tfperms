package catalog

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSchemaUnmarshal confirms the data model round-trips a representative
// YAML document. It is intentionally narrow: it exercises decoding only,
// not the loader (loader_test.go) or validator (validator_test.go).
//
// The YAML payload below is the canonical shape of a catalog file: each
// section (resources, data_sources, iam_bindings) decodes through the
// loader's two-phase rawFile -> typed-entry path. Keeping the test here
// rather than in loader_test catches regressions that affect only the
// struct tags or the enum types — those would surface here even if the
// loader is removed or refactored.
func TestSchemaUnmarshal(t *testing.T) {
	const src = `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls:
        - https://cloud.google.com/storage/docs/access-control/iam-permissions
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan:
        - storage.buckets.get
      create:
        - storage.buckets.create
      update:
        - storage.buckets.update
      delete:
        - storage.buckets.delete
    conditionals:
      - when:
          uniform_bucket_level_access: true
        permissions:
          create:
            - storage.buckets.setIamPolicy
data_sources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls:
        - https://cloud.google.com/storage/docs/access-control/iam-permissions
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan:
        - storage.buckets.get
iam_bindings:
  google_storage_bucket_iam_binding:
    parent_resource: google_storage_bucket
    verification:
      method: docs+source
      source_urls:
        - https://cloud.google.com/storage/docs/access-control/iam-permissions
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan:
        - storage.buckets.getIamPolicy
      create:
        - storage.buckets.setIamPolicy
      update:
        - storage.buckets.setIamPolicy
      delete:
        - storage.buckets.setIamPolicy
`

	var raw struct {
		Resources   map[string]ResourceEntry   `yaml:"resources"`
		DataSources map[string]DataSourceEntry `yaml:"data_sources"`
		IAMBindings map[string]IAMBindingEntry `yaml:"iam_bindings"`
	}
	if err := yaml.Unmarshal([]byte(src), &raw); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	bucket, ok := raw.Resources["google_storage_bucket"]
	if !ok {
		t.Fatal("expected google_storage_bucket in resources")
	}
	if bucket.Verification.Method != VerificationMethodDocsSource {
		t.Errorf("verification.method = %q, want %q", bucket.Verification.Method, VerificationMethodDocsSource)
	}
	if len(bucket.Verification.SourceURLs) != 1 {
		t.Errorf("verification.source_urls len = %d, want 1", len(bucket.Verification.SourceURLs))
	}
	if bucket.Verification.VerifiedAt != "2025-12-15" {
		t.Errorf("verified_at = %q, want 2025-12-15", bucket.Verification.VerifiedAt)
	}
	if bucket.Verification.VerifiedProviderVersion != "6.12.0" {
		t.Errorf("verified_provider_version = %q, want 6.12.0", bucket.Verification.VerifiedProviderVersion)
	}
	if bucket.TestedAgainstProvider != ">=5.0.0,<7.0.0" {
		t.Errorf("tested_against_provider = %q", bucket.TestedAgainstProvider)
	}
	if len(bucket.Permissions.Plan) != 1 || bucket.Permissions.Plan[0] != "storage.buckets.get" {
		t.Errorf("permissions.plan = %v, want [storage.buckets.get]", bucket.Permissions.Plan)
	}
	if len(bucket.Permissions.Create) != 1 {
		t.Errorf("permissions.create len = %d, want 1", len(bucket.Permissions.Create))
	}
	if len(bucket.Permissions.Update) != 1 {
		t.Errorf("permissions.update len = %d, want 1", len(bucket.Permissions.Update))
	}
	if len(bucket.Permissions.Delete) != 1 {
		t.Errorf("permissions.delete len = %d, want 1", len(bucket.Permissions.Delete))
	}
	if len(bucket.Conditionals) != 1 {
		t.Fatalf("conditionals len = %d, want 1", len(bucket.Conditionals))
	}
	cond := bucket.Conditionals[0]
	if got, want := cond.When["uniform_bucket_level_access"], true; got != want {
		t.Errorf("conditional.when[uniform_bucket_level_access] = %v, want %v", got, want)
	}
	if len(cond.Permissions.Create) != 1 {
		t.Errorf("conditional.create len = %d, want 1", len(cond.Permissions.Create))
	}

	binding, ok := raw.IAMBindings["google_storage_bucket_iam_binding"]
	if !ok {
		t.Fatal("expected google_storage_bucket_iam_binding in iam_bindings")
	}
	if binding.ParentResource != "google_storage_bucket" {
		t.Errorf("iam binding parent_resource = %q, want google_storage_bucket", binding.ParentResource)
	}
	if binding.Verification.Method != VerificationMethodDocsSource {
		t.Errorf("iam binding verification.method = %q, want %q", binding.Verification.Method, VerificationMethodDocsSource)
	}
	if len(binding.Permissions.Create) != 1 || binding.Permissions.Create[0] != "storage.buckets.setIamPolicy" {
		t.Errorf("iam binding create = %v", binding.Permissions.Create)
	}

	ds, ok := raw.DataSources["google_storage_bucket"]
	if !ok {
		t.Fatal("expected google_storage_bucket in data_sources")
	}
	if len(ds.Permissions.Plan) != 1 {
		t.Errorf("data source plan len = %d, want 1", len(ds.Permissions.Plan))
	}
}

// TestPositionString covers the two human-visible branches of Position's
// String method. The "<unknown>:0" output is what tests will see when an
// entry was constructed in code (e.g. a hand-rolled fixture) rather than
// loaded from YAML, and we want a stable, asserted format.
func TestPositionString(t *testing.T) {
	cases := []struct {
		name string
		pos  Position
		want string
	}{
		{name: "populated", pos: Position{File: "storage.yaml", Line: 12}, want: "storage.yaml:12"},
		{name: "zero", pos: Position{}, want: "<unknown>:0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pos.String(); got != tc.want {
				t.Errorf("Position.String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewCatalog confirms newCatalog returns non-nil maps so range loops
// in callers do not need defensive nil checks. It is a one-line guarantee
// but worth pinning because a future "optimisation" that lazily initialises
// the maps would silently break callers.
func TestNewCatalog(t *testing.T) {
	c := newCatalog()
	if c.Resources == nil || c.DataSources == nil || c.IAMBindings == nil {
		t.Fatalf("newCatalog returned nil map(s): %+v", c)
	}
}
