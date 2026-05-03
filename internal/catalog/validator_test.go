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
//
// Each fixture provides ONLY enough valid baseline schema to isolate
// the rule under test. Cases that cover a single missing field omit
// that field while keeping the rest of the entry valid; that way a
// regression in another rule does not mask the rule actually being
// tested.
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
    verification:
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
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
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"verification.method", "telepathy", "empirical", "docs+source"},
		},
		{
			name: "missing verification source_urls",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"verification.source_urls", "at least one citation"},
		},
		{
			name: "blank verification source_urls entry",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls:
        - "   "
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"verification.source_urls[0] is empty"},
		},
		{
			name: "missing verified_at",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"verification.verified_at is required"},
		},
		{
			name: "malformed verified_at",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "yesterday"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"verification.verified_at", "yesterday", "YYYY-MM-DD"},
		},
		{
			name: "missing verified_provider_version",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"verification.verified_provider_version is required"},
		},
		{
			name: "missing tested_against_provider",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"tested_against_provider is required"},
		},
		{
			name: "missing permissions.plan",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
`,
			wantSubs: []string{"permissions.plan must contain at least one permission"},
		},
		{
			name: "empty permissions.plan",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
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
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan:
        - "   "
`,
			wantSubs: []string{"permissions.plan[0] is empty"},
		},
		{
			name: "blank create permission",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
      create:
        - storage.buckets.create
        - ""
`,
			wantSubs: []string{"permissions.create[1] is empty"},
		},
		{
			name: "blank update permission",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
      update:
        - "   "
`,
			wantSubs: []string{"permissions.update[0] is empty"},
		},
		{
			name: "blank delete permission",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
      delete:
        - ""
`,
			wantSubs: []string{"permissions.delete[0] is empty"},
		},
		{
			name: "data source missing plan",
			yaml: `
data_sources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
`,
			wantSubs: []string{"data_sources/google_storage_bucket", "permissions.plan must contain at least one permission"},
		},
		{
			name: "iam binding missing parent_resource",
			yaml: `
iam_bindings:
  google_storage_bucket_iam_binding:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.getIamPolicy]
      create: [storage.buckets.setIamPolicy]
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
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.getIamPolicy]
      create: [storage.buckets.setIamPolicy]
`,
			wantSubs: []string{"parent_resource", "google_storage_bucket", "is not a declared resource type"},
		},
		{
			name: "conditional with empty when",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when: {}
        permissions:
          update: [storage.buckets.update]
`,
			wantSubs: []string{"conditionals[0]", "when clause must have at least one predicate"},
		},
		{
			name: "conditional adds no permissions",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when:
          uniform_bucket_level_access: true
        permissions:
          plan: []
`,
			wantSubs: []string{"conditionals[0]", "must add at least one permission"},
		},
		{
			name: "data source conditional adds no permissions",
			yaml: `
data_sources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when:
          include_iam: true
        permissions:
          plan: []
`,
			wantSubs: []string{"conditionals[0]", "must add at least one permission"},
		},
		{
			// A "TODO" or other prose value passes the empty check but is
			// not a recognisable Terraform version constraint. The
			// best-effort regex must reject it so a contributor can't
			// merge a stub.
			name: "tested_against_provider placeholder string",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: "TODO"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"tested_against_provider", "TODO", "not a recognised version constraint"},
		},
		{
			// "latest" is the most common drive-by mistake — it looks
			// plausible to a human but does not parse as a constraint
			// expression and would be silently accepted by the previous
			// non-empty-only check.
			name: "tested_against_provider non-version word",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: "latest"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"tested_against_provider", "latest", "not a recognised version constraint"},
		},
		{
			// Doubled operators like `!!1.2.3` would slip past a
			// character-class regex (`[<>=!~]*`) because the class
			// accepts any number of those symbols in any order. The
			// alternation form rejects them: `!!` is not one of the
			// seven Terraform-supported operators.
			name: "tested_against_provider doubled bang operator",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: "!!1.2.3"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"tested_against_provider", "!!1.2.3", "not a recognised version constraint"},
		},
		{
			// `><6.0` is the same kind of character-class hole: two
			// valid operator characters in an order Terraform never
			// produces. The alternation regex requires one specific
			// operator token and rejects the combination.
			name: "tested_against_provider mixed operator characters",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: "><6.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"tested_against_provider", "><6.0", "not a recognised version constraint"},
		},
		{
			// Terraform supports `~>` (pessimistic constraint) but not
			// `=~`. A character-class regex would not distinguish the
			// two; the alternation form does.
			name: "tested_against_provider reversed pessimistic operator",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: "=~ 5.0"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"tested_against_provider", "=~ 5.0", "not a recognised version constraint"},
		},
		{
			// Multi-clause constraints are common (`>=5.0.0,<7.0.0`); a
			// trailing comma yields an empty clause that should be
			// rejected as a typo.
			name: "tested_against_provider trailing empty clause",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,"
    permissions:
      plan: [storage.buckets.get]
`,
			wantSubs: []string{"tested_against_provider clause", "is empty"},
		},
		{
			// A `when` key containing whitespace or punctuation cannot be
			// a real Terraform attribute and is almost always a YAML
			// indentation typo. Reject it lexically.
			name: "when clause key with invalid identifier",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when:
          "uniform bucket level access": true
        permissions:
          update: [storage.buckets.update]
`,
			wantSubs: []string{"conditionals[0]", "uniform bucket level access", "not a valid HCL identifier"},
		},
		{
			// Branching on a meta-argument like `count` is conceptually
			// nonsensical: count governs whether the resource exists at
			// all, not which permissions it requires once it does.
			name: "when clause uses meta-argument",
			yaml: `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
    conditionals:
      - when:
          count: 1
        permissions:
          update: [storage.buckets.update]
`,
			wantSubs: []string{"conditionals[0]", `"count"`, "meta-argument"},
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
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
      create: [storage.buckets.create]
      update: [storage.buckets.update]
      delete: [storage.buckets.delete]
    conditionals:
      - when:
          uniform_bucket_level_access: true
        permissions:
          create: [storage.buckets.setIamPolicy]
          update: [storage.buckets.setIamPolicy]
data_sources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
iam_bindings:
  google_storage_bucket_iam_binding:
    parent_resource: google_storage_bucket
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.getIamPolicy]
      create: [storage.buckets.setIamPolicy]
      update: [storage.buckets.setIamPolicy]
      delete: [storage.buckets.setIamPolicy]
`
	fs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}
	if _, err := LoadFS(fs, "."); err != nil {
		t.Fatalf("LoadFS rejected canonical catalog: %v", err)
	}
}

// TestValidateAcceptsTestedAgainstProviderForms is the positive-path
// counterpart to the new tested_against_provider negative cases. The
// validator accepts every constraint idiom the catalog has historically
// allowed, so a regression that tightened the regex too far (e.g.
// requiring a fully-qualified semver, or rejecting the pessimistic
// `~>` operator) would surface here.
//
// The non-canonical forms — `~> 6.0` (pessimistic with a partial
// version) and `6.x` (the .x short-form) — are the cases most likely
// to break under a stricter regex, so they get explicit coverage in
// addition to the canonical multi-clause `>=5.0.0,<7.0.0` form already
// exercised by TestValidateAcceptsCanonicalCatalog.
func TestValidateAcceptsTestedAgainstProviderForms(t *testing.T) {
	cases := []struct {
		name       string
		constraint string
	}{
		{name: "canonical multi-clause", constraint: ">=5.0.0,<7.0.0"},
		{name: "pessimistic with partial version", constraint: "~> 6.0"},
		{name: "pessimistic without space", constraint: "~>6.0"},
		{name: "x short-form", constraint: "6.x"},
		{name: "exact equals", constraint: "= 6.12.0"},
		{name: "not equals", constraint: "!=5.0.0"},
		{name: "bare version no operator", constraint: "6.12.0"},
		{name: "semver pre-release and build", constraint: ">=5.0.0-rc1+build.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: "` + tc.constraint + `"
    permissions:
      plan: [storage.buckets.get]
`
			fs := fstest.MapFS{
				"x.yaml": &fstest.MapFile{Data: []byte(yaml)},
			}
			if _, err := LoadFS(fs, "."); err != nil {
				t.Fatalf("LoadFS rejected valid constraint %q: %v", tc.constraint, err)
			}
		})
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
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
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
