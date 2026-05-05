package catalog

import (
	"testing"
	"testing/fstest"
)

// TestComputeStatsNilCatalog confirms ComputeStats handles a nil
// catalog safely. The renderer in cmd/tfperms calls ComputeStats with
// the result of catalog.Load() which is non-nil on success, but an
// in-memory caller (a future API consumer, a fuzz test) might pass nil
// — and a nil-deref here would be a much worse failure mode than the
// empty-stats fallback.
func TestComputeStatsNilCatalog(t *testing.T) {
	stats := ComputeStats(nil, DefaultReferenceVersion)
	if stats.TotalResources != 0 || stats.TotalDataSources != 0 || stats.TotalIAMBindings != 0 {
		t.Errorf("expected zero totals, got %+v", stats)
	}
	if stats.Services == nil || len(stats.Services) != 0 {
		t.Errorf("expected empty (non-nil) Services, got %#v", stats.Services)
	}
	if stats.OldestVerified == nil || len(stats.OldestVerified) != 0 {
		t.Errorf("expected empty (non-nil) OldestVerified, got %#v", stats.OldestVerified)
	}
	if stats.MissingProvenance == nil || len(stats.MissingProvenance) != 0 {
		t.Errorf("expected empty (non-nil) MissingProvenance, got %#v", stats.MissingProvenance)
	}
	if stats.Drifting == nil || len(stats.Drifting) != 0 {
		t.Errorf("expected empty (non-nil) Drifting, got %#v", stats.Drifting)
	}
	if stats.ReferenceVersion != DefaultReferenceVersion {
		t.Errorf("ReferenceVersion = %q, want %q", stats.ReferenceVersion, DefaultReferenceVersion)
	}
}

// TestComputeStatsServiceGrouping uses a multi-file fstest fixture to
// verify per-service grouping splits empirical / docs+source counts
// correctly and orders services lexicographically.
func TestComputeStatsServiceGrouping(t *testing.T) {
	mfs := fstest.MapFS{
		"storage.yaml": &fstest.MapFile{Data: []byte(`
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
`)},
		"compute.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_compute_instance:
    verification:
      method: empirical
      source_urls: [https://example.test/iam]
      verified_at: "2026-01-10"
      verified_provider_version: "6.20.0"
    tested_against_provider: ">=6.0.0,<7.0.0"
    permissions:
      plan: [compute.instances.get]
      create: [compute.instances.create]
      update: [compute.instances.update]
      delete: [compute.instances.delete]
`)},
	}
	cat, err := LoadFS(mfs, ".")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}

	stats := ComputeStats(cat, DefaultReferenceVersion)

	if stats.TotalResources != 2 {
		t.Errorf("TotalResources = %d, want 2", stats.TotalResources)
	}
	if stats.TotalDataSources != 1 {
		t.Errorf("TotalDataSources = %d, want 1", stats.TotalDataSources)
	}
	if stats.TotalIAMBindings != 0 {
		t.Errorf("TotalIAMBindings = %d, want 0", stats.TotalIAMBindings)
	}

	if len(stats.Services) != 2 {
		t.Fatalf("Services len = %d, want 2: %+v", len(stats.Services), stats.Services)
	}
	// Sorted lexicographically: compute < storage.
	if stats.Services[0].Service != "compute" {
		t.Errorf("Services[0].Service = %q, want compute", stats.Services[0].Service)
	}
	if stats.Services[0].Empirical != 1 || stats.Services[0].DocsSource != 0 {
		t.Errorf("compute counts = empirical=%d, docs+source=%d, want 1/0",
			stats.Services[0].Empirical, stats.Services[0].DocsSource)
	}
	if stats.Services[1].Service != "storage" {
		t.Errorf("Services[1].Service = %q, want storage", stats.Services[1].Service)
	}
	if stats.Services[1].Total != 2 {
		t.Errorf("storage Total = %d, want 2", stats.Services[1].Total)
	}
	if stats.Services[1].Empirical != 0 || stats.Services[1].DocsSource != 2 {
		t.Errorf("storage counts = empirical=%d, docs+source=%d, want 0/2",
			stats.Services[1].Empirical, stats.Services[1].DocsSource)
	}
}

// TestComputeStatsOldestVerified pins the oldest-N selection logic:
// ascending date order, capped at five entries, ties broken
// deterministically.
func TestComputeStatsOldestVerified(t *testing.T) {
	mfs := fstest.MapFS{
		"alpha.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_alpha_one:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2024-06-01"
      verified_provider_version: "5.0.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [alpha.one.get]
      create: [alpha.one.create]
      update: [alpha.one.update]
      delete: [alpha.one.delete]
  google_alpha_two:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-01-01"
      verified_provider_version: "5.5.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [alpha.two.get]
      create: [alpha.two.create]
      update: [alpha.two.update]
      delete: [alpha.two.delete]
`)},
		"beta.yaml": &fstest.MapFile{Data: []byte(`
resources:
  google_beta_one:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-06-01"
      verified_provider_version: "6.0.0"
    tested_against_provider: ">=6.0.0,<7.0.0"
    permissions:
      plan: [beta.one.get]
      create: [beta.one.create]
      update: [beta.one.update]
      delete: [beta.one.delete]
  google_beta_two:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2024-06-01"
      verified_provider_version: "5.0.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [beta.two.get]
      create: [beta.two.create]
      update: [beta.two.update]
      delete: [beta.two.delete]
`)},
	}
	cat, err := LoadFS(mfs, ".")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}

	stats := ComputeStats(cat, DefaultReferenceVersion)

	if len(stats.OldestVerified) != 4 {
		t.Fatalf("OldestVerified len = %d, want 4: %+v", len(stats.OldestVerified), stats.OldestVerified)
	}
	// Two entries share 2024-06-01 — the tiebreak (Section, Type)
	// puts google_alpha_one before google_beta_two.
	if stats.OldestVerified[0].Type != "google_alpha_one" {
		t.Errorf("OldestVerified[0].Type = %q, want google_alpha_one", stats.OldestVerified[0].Type)
	}
	if stats.OldestVerified[1].Type != "google_beta_two" {
		t.Errorf("OldestVerified[1].Type = %q, want google_beta_two", stats.OldestVerified[1].Type)
	}
	if stats.OldestVerified[2].VerifiedAt != "2025-01-01" {
		t.Errorf("OldestVerified[2].VerifiedAt = %q, want 2025-01-01", stats.OldestVerified[2].VerifiedAt)
	}
	if stats.OldestVerified[3].VerifiedAt != "2025-06-01" {
		t.Errorf("OldestVerified[3].VerifiedAt = %q, want 2025-06-01", stats.OldestVerified[3].VerifiedAt)
	}
}

// TestComputeStatsOldestVerifiedCapsAtLimit confirms that more than
// `oldestVerifiedLimit` entries are truncated to exactly the limit —
// the renderer's "Oldest 5" promise is structural, not advisory.
func TestComputeStatsOldestVerifiedCapsAtLimit(t *testing.T) {
	c := newCatalog()
	for i := 0; i < oldestVerifiedLimit+3; i++ {
		typ := "google_test_resource_" + string(rune('a'+i))
		c.Resources[typ] = &ResourceEntry{
			Type:     typ,
			Position: Position{File: "test.yaml", Line: i + 1},
			Verification: Verification{
				VerifiedAt: "2024-01-01",
			},
		}
	}
	stats := ComputeStats(c, "")
	if got := len(stats.OldestVerified); got != oldestVerifiedLimit {
		t.Errorf("OldestVerified len = %d, want %d", got, oldestVerifiedLimit)
	}
}

// TestComputeStatsMissingProvenance flags TODO sentinels in
// permission lists and source URL slices. A hand-rolled catalog is
// used because the validator would reject these values at load time
// otherwise — and we want the diagnostic to work on raw catalogs the
// user might be in the middle of editing.
func TestComputeStatsMissingProvenance(t *testing.T) {
	c := newCatalog()
	c.Resources["google_partial_resource"] = &ResourceEntry{
		Type:     "google_partial_resource",
		Position: Position{File: "partial.yaml", Line: 1},
		Verification: Verification{
			Method:                  VerificationMethodDocsSource,
			SourceURLs:              []string{"TODO", "https://example.test/iam"},
			VerifiedAt:              "2026-01-01",
			VerifiedProviderVersion: "6.0.0",
		},
		TestedAgainstProvider: ">=6.0.0,<7.0.0",
		Permissions: PermissionSet{
			Plan:   []string{"TODO"},
			Create: []string{"partial.create"},
			Update: []string{"partial.update", "TODO"},
			Delete: []string{"partial.delete"},
		},
	}
	c.IAMBindings["google_partial_iam"] = &IAMBindingEntry{
		Type:           "google_partial_iam",
		ParentResource: "TODO",
		Position:       Position{File: "partial.yaml", Line: 50},
		Verification: Verification{
			Method:                  VerificationMethodDocsSource,
			SourceURLs:              []string{"https://example.test/iam"},
			VerifiedAt:              "2026-01-01",
			VerifiedProviderVersion: "6.0.0",
		},
		TestedAgainstProvider: ">=6.0.0,<7.0.0",
		Permissions: PermissionSet{
			Plan:   []string{"partial.iam.get"},
			Create: []string{"partial.iam.set"},
			Update: []string{"partial.iam.set"},
			Delete: []string{"partial.iam.set"},
		},
	}

	stats := ComputeStats(c, "")
	wantFields := []struct {
		section string
		typ     string
		field   string
	}{
		{"iam_bindings", "google_partial_iam", "parent_resource"},
		{"resources", "google_partial_resource", "permissions.plan[0]"},
		{"resources", "google_partial_resource", "permissions.update[1]"},
		{"resources", "google_partial_resource", "verification.source_urls[0]"},
	}
	if len(stats.MissingProvenance) != len(wantFields) {
		t.Fatalf("MissingProvenance len = %d, want %d: %+v",
			len(stats.MissingProvenance), len(wantFields), stats.MissingProvenance)
	}
	for i, want := range wantFields {
		got := stats.MissingProvenance[i]
		if got.Section != want.section || got.Type != want.typ || got.Field != want.field {
			t.Errorf("MissingProvenance[%d] = {%s/%s/%s}, want {%s/%s/%s}",
				i, got.Section, got.Type, got.Field,
				want.section, want.typ, want.field)
		}
	}
}

// TestComputeStatsDrifting exercises the constraint-vs-reference
// satisfiability check across the operators the catalog actually uses.
// "satisfies" means "no drift"; "does not satisfy" means the entry is
// reported.
func TestComputeStatsDrifting(t *testing.T) {
	c := newCatalog()
	add := func(typ, tested string) {
		c.Resources[typ] = &ResourceEntry{
			Type:                  typ,
			Position:              Position{File: "test.yaml", Line: 1},
			TestedAgainstProvider: tested,
			Verification: Verification{
				Method:                  VerificationMethodDocsSource,
				SourceURLs:              []string{"https://example.test/iam"},
				VerifiedAt:              "2026-01-01",
				VerifiedProviderVersion: "6.0.0",
			},
		}
	}
	// in range
	add("google_in_range", ">=6.0.0,<7.0.0")
	// out of range — too low
	add("google_too_low", ">=7.0.0,<8.0.0")
	// out of range — too high
	add("google_too_high", ">=4.0.0,<5.0.0")
	// not equal — exact 5.0.0 excludes 6.15.0
	add("google_neq", "=5.0.0")
	// pessimistic lower-bound only — 6.15.0 satisfies >=6.0
	add("google_pessimistic_lower", "~> 6.0")
	// pessimistic lower-bound — 6.15.0 does not satisfy >=7.0
	add("google_pessimistic_high", "~> 7.0")

	stats := ComputeStats(c, "6.15.0")
	wantTypes := []string{
		"google_neq",
		"google_pessimistic_high",
		"google_too_high",
		"google_too_low",
	}
	if len(stats.Drifting) != len(wantTypes) {
		t.Fatalf("Drifting len = %d, want %d: %+v",
			len(stats.Drifting), len(wantTypes), stats.Drifting)
	}
	for i, want := range wantTypes {
		if stats.Drifting[i].Type != want {
			t.Errorf("Drifting[%d].Type = %q, want %q",
				i, stats.Drifting[i].Type, want)
		}
	}
}

// TestComputeStatsDriftingWildcardShortForm pins the behaviour around
// the `6.x` short form that validator.go documents as accepted (see
// testedAgainstProviderConstraintPattern at validator.go:58-66). The
// previous parseVersion implementation truncated `6.x` to a malformed
// `6.` core and bailed out; computeDrifting then silently dropped the
// entry from the drift report. This test pins the corrected reading:
// `6.x` matches any 6.y.z reference, `7.x` does not, and the explicit
// `>=` / `~>` operators behave as a lower bound at the prefix.
func TestComputeStatsDriftingWildcardShortForm(t *testing.T) {
	c := newCatalog()
	add := func(typ, tested string) {
		c.Resources[typ] = &ResourceEntry{
			Type:                  typ,
			Position:              Position{File: "test.yaml", Line: 1},
			TestedAgainstProvider: tested,
			Verification: Verification{
				Method:                  VerificationMethodDocsSource,
				SourceURLs:              []string{"https://example.test/iam"},
				VerifiedAt:              "2026-01-01",
				VerifiedProviderVersion: "6.0.0",
			},
		}
	}
	// `6.x` (bare → defaults to `=`) matches reference 6.15.0.
	add("google_wildcard_in_range", "6.x")
	// `6.X` (uppercase wildcard) matches identically.
	add("google_wildcard_uppercase", "6.X")
	// `6.0.x` — leading prefix [6,0] does not match reference [6,15,0]
	// in the first two components, so this should drift.
	add("google_wildcard_minor_pin", "6.0.x")
	// `7.x` — major mismatch, drifts.
	add("google_wildcard_too_high", "7.x")
	// `>= 6.x` — lower bound at 6.0.0, satisfied.
	add("google_wildcard_lower_bound", ">= 6.x")
	// `>= 7.x` — lower bound at 7.0.0, drifts.
	add("google_wildcard_lower_too_high", ">= 7.x")
	// `~> 6.x` — pessimistic lower bound, satisfied.
	add("google_wildcard_pessimistic_ok", "~> 6.x")
	// `!= 7.x` — reference does not start with [7], so satisfied.
	add("google_wildcard_neq_other_major", "!= 7.x")
	// `!= 6.x` — reference starts with [6], so drifts.
	add("google_wildcard_neq_self_major", "!= 6.x")

	stats := ComputeStats(c, "6.15.0")
	wantTypes := []string{
		"google_wildcard_lower_too_high",
		"google_wildcard_minor_pin",
		"google_wildcard_neq_self_major",
		"google_wildcard_too_high",
	}
	if len(stats.Drifting) != len(wantTypes) {
		t.Fatalf("Drifting len = %d, want %d: %+v",
			len(stats.Drifting), len(wantTypes), stats.Drifting)
	}
	for i, want := range wantTypes {
		if stats.Drifting[i].Type != want {
			t.Errorf("Drifting[%d].Type = %q, want %q",
				i, stats.Drifting[i].Type, want)
		}
	}
}

// TestComputeStatsOldestVerifiedSkipsInvalidDates pins the contract
// the doc-comment on computeOldestVerified promises: an entry whose
// verified_at fails to parse against verifiedAtLayout is dropped from
// the slice rather than sorted to the front. Load() rejects malformed
// dates at validation time, so this exercises the defensive branch a
// hand-rolled fixture would otherwise hit — and prevents a regression
// where the renderer "oldest" section is dominated by a typo.
func TestComputeStatsOldestVerifiedSkipsInvalidDates(t *testing.T) {
	c := newCatalog()
	c.Resources["google_blank_date"] = &ResourceEntry{
		Type:     "google_blank_date",
		Position: Position{File: "test.yaml", Line: 1},
		Verification: Verification{
			VerifiedAt: "",
		},
	}
	c.Resources["google_garbage_date"] = &ResourceEntry{
		Type:     "google_garbage_date",
		Position: Position{File: "test.yaml", Line: 2},
		Verification: Verification{
			VerifiedAt: "yesterday",
		},
	}
	c.Resources["google_almost_date"] = &ResourceEntry{
		Type:     "google_almost_date",
		Position: Position{File: "test.yaml", Line: 3},
		Verification: Verification{
			// Wrong layout — DD-MM-YYYY rather than YYYY-MM-DD.
			VerifiedAt: "01-06-2024",
		},
	}
	c.Resources["google_real_date"] = &ResourceEntry{
		Type:     "google_real_date",
		Position: Position{File: "test.yaml", Line: 4},
		Verification: Verification{
			VerifiedAt: "2024-06-01",
		},
	}

	stats := ComputeStats(c, "")
	if len(stats.OldestVerified) != 1 {
		t.Fatalf("OldestVerified len = %d, want 1 (only the well-formed date should appear): %+v",
			len(stats.OldestVerified), stats.OldestVerified)
	}
	if stats.OldestVerified[0].Type != "google_real_date" {
		t.Errorf("OldestVerified[0].Type = %q, want google_real_date",
			stats.OldestVerified[0].Type)
	}
}

// TestComputeStatsDriftingDisabled confirms that an empty
// referenceVersion turns off drift detection entirely. The cmd layer
// uses this in fixture-driven golden tests to keep the drift section
// stable regardless of constraint evolution.
func TestComputeStatsDriftingDisabled(t *testing.T) {
	c := newCatalog()
	c.Resources["google_test"] = &ResourceEntry{
		Type:                  "google_test",
		Position:              Position{File: "test.yaml", Line: 1},
		TestedAgainstProvider: ">=99.0.0,<100.0.0",
	}
	stats := ComputeStats(c, "")
	if len(stats.Drifting) != 0 {
		t.Errorf("Drifting len = %d with empty referenceVersion, want 0", len(stats.Drifting))
	}
}

// TestParseVersion exercises the small numeric-component parser. The
// satisfiesConstraint flow depends on this, so a regression here
// would silently flip drift reports. The wildcard cases (`6.x`,
// `6.0.x`) are pinned because the validator at validator.go:58-66
// documents them as accepted constraint shapes — a regression that
// dropped them would silently omit valid catalog entries from the
// drift report (the bug review #1 caught).
func TestParseVersion(t *testing.T) {
	cases := []struct {
		in           string
		want         []int
		wantIsPrefix bool
		wantOK       bool
	}{
		{"6.15.0", []int{6, 15, 0}, false, true},
		{"6.0", []int{6, 0}, false, true},
		{"6", []int{6}, false, true},
		{"5.0.0-rc1+build.7", []int{5, 0, 0}, false, true},
		{"  6.15.0  ", []int{6, 15, 0}, false, true},
		// Wildcard short forms — the contract documented at
		// validator.go's testedAgainstProviderConstraintPattern.
		{"6.x", []int{6}, true, true},
		{"6.X", []int{6}, true, true},
		{"6.0.x", []int{6, 0}, true, true},
		{"  6.x  ", []int{6}, true, true},
		{"", nil, false, false},
		{"abc", nil, false, false},
		{"6..0", nil, false, false},
		{"6.", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, isPrefix, ok := parseVersion(tc.in)
			if ok != tc.wantOK {
				t.Errorf("parseVersion(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if isPrefix != tc.wantIsPrefix {
				t.Errorf("parseVersion(%q) isPrefix = %v, want %v",
					tc.in, isPrefix, tc.wantIsPrefix)
			}
			if len(got) != len(tc.want) {
				t.Errorf("parseVersion(%q) = %v, want %v", tc.in, got, tc.want)
				return
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseVersion(%q) = %v, want %v", tc.in, got, tc.want)
					return
				}
			}
		})
	}
}

// TestCompareVersions covers the numeric-component compare. The
// "trailing zero treated as equal" branch is locked because semver
// callers expect "6.0" == "6.0.0".
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b []int
		want int
	}{
		{[]int{6, 15, 0}, []int{6, 0, 0}, 1},
		{[]int{6, 0, 0}, []int{6, 15, 0}, -1},
		{[]int{6, 0}, []int{6, 0, 0}, 0},
		{[]int{6, 0, 0}, []int{6}, 0},
		{[]int{5, 9, 9}, []int{6, 0, 0}, -1},
	}
	for _, tc := range cases {
		got := compareVersions(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("compareVersions(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
