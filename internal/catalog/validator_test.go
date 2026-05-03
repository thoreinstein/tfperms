package catalog

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// TestValidate is the table-driven negative-path test for the validator.
// Each case provides a YAML payload that violates exactly one rule and
// asserts that LoadFS returns an ErrCatalog-wrapped error whose message
// contains the substrings unique to that violation.
//
// The test goes through LoadFS rather than calling validate() directly
// so it doubles as an integration check: a regression in the loader
// that drops Position or Type before validate() runs would surface
// here, since the asserted substrings include the entry path.
func TestValidate(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantSubs []string // all required to appear in err.Error()
	}{
		{
			name: "missing verification method",
			yaml: `
resources:
  google_storage_bucket:
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"resources/google_storage_bucket", "verification.method is required"},
		},
		{
			name: "unknown verification method",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: telepathy
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"verification.method", "telepathy", "gcloud", "rest", "terraform"},
		},
		{
			name: "missing permissions.plan",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: gcloud
`,
			wantSubs: []string{"permissions.plan must contain at least one permission"},
		},
		{
			name: "empty permissions.plan",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: []
`,
			wantSubs: []string{"permissions.plan must contain at least one permission"},
		},
		{
			name: "blank permission string",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan:
        - "   "
`,
			wantSubs: []string{"permissions.plan[0] is empty"},
		},
		{
			name: "blank apply permission",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: [storage.buckets.get]
      apply:
        - storage.buckets.create
        - ""
`,
			wantSubs: []string{"permissions.apply[1] is empty"},
		},
		{
			name: "data source missing plan",
			yaml: `
data_sources:
  google_storage_bucket:
    verification:
      method: gcloud
`,
			wantSubs: []string{"data_sources/google_storage_bucket", "permissions.plan must contain at least one permission"},
		},
		{
			name: "iam binding missing parent_resource",
			yaml: `
iam_bindings:
  google_storage_bucket_iam_binding:
    verification:
      method: rest
    permissions:
      plan: [storage.buckets.getIamPolicy]
      apply: [storage.buckets.setIamPolicy]
`,
			wantSubs: []string{"iam_bindings/google_storage_bucket_iam_binding", "parent_resource is required"},
		},
		{
			name: "iam binding parent_resource not declared",
			yaml: `
iam_bindings:
  google_storage_bucket_iam_binding:
    parent_resource: google_storage_bucket
    verification:
      method: rest
    permissions:
      plan: [storage.buckets.getIamPolicy]
      apply: [storage.buckets.setIamPolicy]
`,
			wantSubs: []string{"parent_resource", "google_storage_bucket", "is not a declared resource type"},
		},
		{
			name: "conditional with empty when",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when: {}
        permissions:
          apply: [storage.buckets.update]
`,
			wantSubs: []string{"conditionals[0]", "when clause must have at least one predicate"},
		},
		{
			name: "conditional adds no permissions",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when:
          uniform_bucket_level_access: true
        permissions:
          plan: []
          apply: []
`,
			wantSubs: []string{"conditionals[0]", "must add at least one permission"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := fstest.MapFS{
				"x.yaml": &fstest.MapFile{Data: []byte(tc.yaml)},
			}
			_, err := LoadFS(fs, ".")
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !errors.Is(err, ErrCatalog) {
				t.Errorf("error not wrapped with ErrCatalog: %v", err)
			}
			msg := err.Error()
			for _, want := range tc.wantSubs {
				if !strings.Contains(msg, want) {
					t.Errorf("error message missing %q: %v", want, err)
				}
			}
		})
	}
}

// TestValidateAcceptsCanonicalCatalog asserts the validator does NOT
// reject a representative valid YAML payload. It is the positive-path
// counterpart to TestValidate's negative table — without this check, a
// regression that turned every entry into an error would silently
// pass the negative tests.
func TestValidateAcceptsCanonicalCatalog(t *testing.T) {
	yaml := `
resources:
  google_storage_bucket:
    verification:
      method: gcloud
      command: "gcloud storage buckets describe gs://{name}"
    permissions:
      plan: [storage.buckets.get]
      apply: [storage.buckets.create, storage.buckets.update, storage.buckets.delete]
    conditionals:
      - when:
          uniform_bucket_level_access: true
        permissions:
          plan: []
          apply: [storage.buckets.update]
data_sources:
  google_storage_bucket:
    verification:
      method: gcloud
    permissions:
      plan: [storage.buckets.get]
iam_bindings:
  google_storage_bucket_iam_binding:
    parent_resource: google_storage_bucket
    verification:
      method: rest
    permissions:
      plan: [storage.buckets.getIamPolicy]
      apply: [storage.buckets.setIamPolicy]
`
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}
	if _, err := LoadFS(fs, "."); err != nil {
		t.Fatalf("LoadFS rejected canonical catalog: %v", err)
	}
}

// TestValidatePositionInError pins that validation errors include the
// source position of the offending entry. Without this, error messages
// would force contributors to grep the catalog file for the failing
// type — the whole point of the loader's two-phase decode is to avoid
// that.
func TestValidatePositionInError(t *testing.T) {
	yaml := `
resources:
  google_storage_bucket:
    permissions:
      plan: [storage.buckets.get]
`
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}
	_, err := LoadFS(fs, ".")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "storage.yaml:") {
		t.Errorf("error missing source position: %v", err)
	}
}
