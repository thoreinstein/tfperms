package catalog

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	catalogdata "github.com/thoreinstein/tfperms/catalog"
)

// TestCatalogValid loads the actual embedded catalog (everything
// currently committed under catalog/*.yaml) and asserts the validator
// accepts every entry. It is the consistency gate for catalog
// contributions: any future PR adding or modifying a YAML entry has to
// pass this test, which means no schema-violating entry can land via
// the normal review process.
//
// This is the single test exposed by `make catalog-validate`. Naming
// it TestCatalogValid (rather than the older TestRepositoryCatalogIsValid)
// matches the make target's `-run` filter so the developer-facing
// command and the test it runs use the same vocabulary. The other
// TestRepositoryCatalog* tests in this file remain repository-level
// invariants and run as part of the normal `go test ./...` suite.
//
// This test is intentionally separate from TestLoadProductionEmbed
// (in loader_test.go) which is a smoke test — that test only checks
// "Load() returns a non-nil catalog without an error". The two could
// be consolidated, but keeping them split makes the intent clear:
// loader_test.go pins loader behaviour, repo_test.go pins repository
// content.
//
// Failure mode: if this test fails on a commit that did not modify a
// YAML file, the validator itself was tightened and existing entries
// no longer satisfy it. The fix is to either relax the validator (if
// the new rule was wrong) or update the offending YAML entry to
// satisfy it (if the rule was right).
func TestCatalogValid(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() rejected committed catalog: %v", err)
	}

	// Sanity floor: the catalog should never go empty. If a refactor
	// accidentally points the embed FS at a directory with no YAML
	// files, Load() would still succeed with an empty catalog and the
	// rest of the suite would not catch it. This explicit check makes
	// that misconfiguration fail loudly.
	totalEntries := len(cat.Resources) + len(cat.DataSources) + len(cat.IAMBindings)
	if totalEntries == 0 {
		t.Fatal("repository catalog is empty — embed FS likely misconfigured")
	}
}

// TestRepositoryCatalogIAMBindingsResolve confirms every IAM binding
// in the committed catalog points at a parent_resource that is also
// committed. The validator already enforces this at load time, so
// TestCatalogValid would catch the same condition. The reason this
// dedicated assertion exists is documentation: when this test fails
// the message is unambiguous about what went wrong, which cuts the
// time to a fix on a contributor's first attempt.
func TestRepositoryCatalogIAMBindingsResolve(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	for _, typ := range sortedKeys(cat.IAMBindings) {
		binding := cat.IAMBindings[typ]
		if _, ok := cat.Resources[binding.ParentResource]; !ok {
			t.Errorf(
				"iam binding %q at %s references parent_resource %q which is not declared in the catalog",
				typ, binding.Position, binding.ParentResource,
			)
		}
	}
}

// TestRepositoryCatalogEveryFileContributes is a defense-in-depth
// assertion that every committed catalog YAML file produces at least
// one merged entry. Strict decoding (loader.go's KnownFields(true)) is
// the primary mechanism that prevents a misspelled top-level key from
// silently emptying a file — a typo there is now a hard parse error.
// This test is the second line of defense: if a future change relaxed
// strict mode, an empty file would still be caught here.
//
// The previous review specifically flagged that asserting only
// "merged catalog is non-empty in aggregate" allowed a misspelled file
// to coast through CI as long as some other file kept the total count
// above zero. By indexing entries back to their source file via
// Position.File and counting per file, this test makes that scenario
// fail loudly with a message that names the offending filename.
func TestRepositoryCatalogEveryFileContributes(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Enumerate every YAML file in the embedded catalog directly so
	// the test sees files even if the loader silently skipped them.
	files, err := listCatalogYAMLFiles()
	if err != nil {
		t.Fatalf("list catalog files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("embedded catalog has no YAML files — repo is misconfigured")
	}

	// Count entries by source file. Position.File is the per-file
	// path that loader.go assigned to each entry, so a file that
	// contributed nothing has zero entries here even though its
	// filename appears in `files`.
	perFile := make(map[string]int, len(files))
	for _, f := range files {
		perFile[f] = 0
	}
	for _, e := range cat.Resources {
		perFile[e.Position.File]++
	}
	for _, e := range cat.DataSources {
		perFile[e.Position.File]++
	}
	for _, e := range cat.IAMBindings {
		perFile[e.Position.File]++
	}

	for _, f := range files {
		if perFile[f] == 0 {
			t.Errorf(
				"catalog file %q contributed zero entries — likely a misspelled top-level section "+
					"(expected one of: resources, data_sources, iam_bindings)",
				f,
			)
		}
	}
}

// listCatalogYAMLFiles returns the *.yaml bare filenames in the root of
// the embedded catalog FS. It mirrors loader.go's file selection (suffix
// filter, skip directories) so the test compares apples to apples with
// the Position.File values that the loader stamps onto each entry. The
// .yml extension is rejected by both this helper and loader.go to match
// catalog/embed.go's //go:embed *.yaml pattern; if the loader ever moves
// to subdirectories or accepts new extensions, all three must change
// together.
func listCatalogYAMLFiles() ([]string, error) {
	entries, err := fs.ReadDir(catalogdata.FS, ".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		// Bare name matches the Position.File the loader records for
		// each entry from this file (loader.go calls
		// mergeFile(cat, firstSeen, name, data) with the bare name).
		out = append(out, name)
	}
	return out, nil
}

// expectedResource captures the permission lists this repo asserts for
// a single resource entry. Tests use this rather than constructing a
// full ResourceEntry because the surrounding metadata (Position,
// Verification provenance, tested_against_provider) is exercised by
// other tests in this file and would just be noise here.
type expectedResource struct {
	plan   []string
	create []string
	update []string
	delete []string
}

// expectedIAMBinding is the IAM-binding analogue of expectedResource.
// IAM bindings carry a parent_resource cross-reference; the structural
// IAMBindingsResolve test already locks the parent string, so the
// permission-locking test only needs the four lists.
type expectedIAMBinding struct {
	parent string
	plan   []string
	create []string
	update []string
	delete []string
}

// expectedDataSource is the read-only counterpart to expectedResource.
// Data sources only carry Plan permissions in the schema, so locking
// just that list is sufficient.
type expectedDataSource struct {
	plan []string
}

// TestRepositoryCatalogPermissionsAreLocked is the per-resource lock
// table the Epic 4 ticket explicitly calls for. The previous review
// flagged the absence of this test as the central correctness gap:
// "Add per-resource catalog tests that lock the expected permission
// sets" (docs/tfperms_pdr.md, Epic 4).
//
// Each entry below is a contract: the YAML committed in catalog/ MUST
// produce exactly these permission lists for this resource type. A
// drift in either direction — the YAML adding or removing a permission
// without updating this table, or this table being edited without a
// corresponding YAML change — fails the test with a diff that names
// the resource and the stage that drifted.
//
// Editing a permission mapping is a deliberate change. Update the
// expected values here in the same diff that changes the YAML so the
// reviewer sees both sides of the change at once.
//
// The conditional block on google_storage_bucket is locked separately
// (TestRepositoryCatalogConditionalsAreLocked) so a regression that
// removes the conditional surfaces with a clearer error than a generic
// diff in this larger table.
func TestRepositoryCatalogPermissionsAreLocked(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	expectedResources := map[string]expectedResource{
		"google_storage_bucket": {
			plan:   []string{"storage.buckets.get"},
			create: []string{"storage.buckets.create"},
			update: []string{"storage.buckets.update"},
			delete: []string{"storage.buckets.delete"},
		},
		"google_storage_bucket_object": {
			plan:   []string{"storage.objects.get"},
			create: []string{"storage.objects.create"},
			update: []string{"storage.objects.update"},
			delete: []string{"storage.objects.delete"},
		},
	}

	expectedDataSources := map[string]expectedDataSource{
		"google_storage_bucket": {
			plan: []string{"storage.buckets.get"},
		},
	}

	// All three IAM-binding flavours (binding, member, policy) on a
	// bucket use the same plan / write split. They are listed as
	// independent entries to keep the lock granular: if a future
	// catalog change distinguishes them (because the provider treats
	// them differently), the per-flavour expectation here makes the
	// drift visible per-entry rather than disguised by reuse.
	expectedIAMBindings := map[string]expectedIAMBinding{
		"google_storage_bucket_iam_binding": {
			parent: "google_storage_bucket",
			plan:   []string{"storage.buckets.getIamPolicy"},
			create: []string{"storage.buckets.setIamPolicy"},
			update: []string{"storage.buckets.setIamPolicy"},
			delete: []string{"storage.buckets.setIamPolicy"},
		},
		"google_storage_bucket_iam_member": {
			parent: "google_storage_bucket",
			plan:   []string{"storage.buckets.getIamPolicy"},
			create: []string{"storage.buckets.setIamPolicy"},
			update: []string{"storage.buckets.setIamPolicy"},
			delete: []string{"storage.buckets.setIamPolicy"},
		},
		"google_storage_bucket_iam_policy": {
			parent: "google_storage_bucket",
			plan:   []string{"storage.buckets.getIamPolicy"},
			create: []string{"storage.buckets.setIamPolicy"},
			update: []string{"storage.buckets.setIamPolicy"},
			delete: []string{"storage.buckets.setIamPolicy"},
		},
	}

	// Resources: every committed entry must appear in the expected
	// table; every entry in the expected table must appear in the
	// catalog. Both directions are checked so adding a YAML entry
	// without updating the table fails too.
	checkSetEquality(t, "resources", keysOf(cat.Resources), keysOf(expectedResources))
	for typ, want := range expectedResources {
		got, ok := cat.Resources[typ]
		if !ok {
			continue // already reported by checkSetEquality
		}
		assertList(t, "resources/"+typ+"/plan", got.Permissions.Plan, want.plan)
		assertList(t, "resources/"+typ+"/create", got.Permissions.Create, want.create)
		assertList(t, "resources/"+typ+"/update", got.Permissions.Update, want.update)
		assertList(t, "resources/"+typ+"/delete", got.Permissions.Delete, want.delete)
	}

	// Data sources: same shape, plan-only.
	checkSetEquality(t, "data_sources", keysOf(cat.DataSources), keysOf(expectedDataSources))
	for typ, want := range expectedDataSources {
		got, ok := cat.DataSources[typ]
		if !ok {
			continue
		}
		assertList(t, "data_sources/"+typ+"/plan", got.Permissions.Plan, want.plan)
	}

	// IAM bindings: lock parent and all four lists.
	checkSetEquality(t, "iam_bindings", keysOf(cat.IAMBindings), keysOf(expectedIAMBindings))
	for typ, want := range expectedIAMBindings {
		got, ok := cat.IAMBindings[typ]
		if !ok {
			continue
		}
		if got.ParentResource != want.parent {
			t.Errorf("iam_bindings/%s parent_resource = %q, want %q", typ, got.ParentResource, want.parent)
		}
		assertList(t, "iam_bindings/"+typ+"/plan", got.Permissions.Plan, want.plan)
		assertList(t, "iam_bindings/"+typ+"/create", got.Permissions.Create, want.create)
		assertList(t, "iam_bindings/"+typ+"/update", got.Permissions.Update, want.update)
		assertList(t, "iam_bindings/"+typ+"/delete", got.Permissions.Delete, want.delete)
	}
}

// TestRepositoryCatalogConditionalsAreLocked locks the conditional
// permission rules on resources that declare them. Conditionals are
// tested separately from base permissions because a regression that
// drops a conditional entirely (rather than changing a permission
// inside one) is a distinct kind of bug: the base entry still
// validates, but a downstream conditional path silently disappears.
// Pinning the count and contents of the conditional list makes that
// failure surface here with a clearer error than the larger
// permissions table would.
//
// Currently only google_storage_bucket carries a conditional. Adding
// a new conditional anywhere requires extending this table.
func TestRepositoryCatalogConditionalsAreLocked(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	bucket, ok := cat.Resources["google_storage_bucket"]
	if !ok {
		t.Fatal("google_storage_bucket missing — base permission test would have caught this too")
	}
	if len(bucket.Conditionals) != 1 {
		t.Fatalf("google_storage_bucket conditionals len = %d, want 1", len(bucket.Conditionals))
	}
	cond := bucket.Conditionals[0]

	// Lock the predicate. The conditional fires only when
	// uniform_bucket_level_access is true; flipping the value or the
	// attribute name silently changes the resolver's behaviour, so we
	// pin both.
	wantWhen := map[string]any{"uniform_bucket_level_access": true}
	if !reflect.DeepEqual(cond.When, wantWhen) {
		t.Errorf("conditional.when = %v, want %v", cond.When, wantWhen)
	}

	// Lock the additive permissions. Plan reads getIamPolicy; create
	// and update each call setIamPolicy. Delete does not appear here:
	// destroying the bucket destroys the policy alongside it, no
	// extra setIamPolicy is issued.
	assertList(t, "google_storage_bucket/conditional[0]/plan", cond.Permissions.Plan,
		[]string{"storage.buckets.getIamPolicy"})
	assertList(t, "google_storage_bucket/conditional[0]/create", cond.Permissions.Create,
		[]string{"storage.buckets.setIamPolicy"})
	assertList(t, "google_storage_bucket/conditional[0]/update", cond.Permissions.Update,
		[]string{"storage.buckets.setIamPolicy"})
	if len(cond.Permissions.Delete) != 0 {
		t.Errorf("conditional.delete = %v, want empty", cond.Permissions.Delete)
	}

	// Other resources currently have no conditionals. Pin that
	// negative fact so a future addition requires extending this
	// table rather than slipping in unnoticed.
	for typ, entry := range cat.Resources {
		if typ == "google_storage_bucket" {
			continue
		}
		if len(entry.Conditionals) != 0 {
			t.Errorf("resources/%s has %d conditionals, want 0 (not yet locked)",
				typ, len(entry.Conditionals))
		}
	}
	for typ, entry := range cat.DataSources {
		if len(entry.Conditionals) != 0 {
			t.Errorf("data_sources/%s has %d conditionals, want 0 (not yet locked)",
				typ, len(entry.Conditionals))
		}
	}
}

// provenanceFloorDate is the earliest verified_at the repository will
// accept. The catalog work formally started in 2024, so any earlier
// date is necessarily a placeholder. The test uses a fixed floor rather
// than "N years ago" so the assertion is deterministic across CI runs
// regardless of the wall clock — a year-relative window would silently
// accept stale dates as the floor advanced, which is the exact failure
// mode this test exists to prevent.
var provenanceFloorDate = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// provenanceCeilingSlack caps how far in the future a verified_at may
// be. Some slack accommodates timezone differences between the
// contributor's machine and CI; more than a few days indicates a typo
// or fabricated date. 7 days is generous and still catches "year 2099"
// stubs.
const provenanceCeilingSlack = 7 * 24 * time.Hour

// obviouslyStubVerifiedAt enumerates date strings that are formally
// well-formed YYYY-MM-DD but are recognisable placeholders. Go's
// time.Time zero value formats as "0001-01-01" so it appears here, as
// does the Unix epoch and a handful of common copy-paste defaults.
//
// This list is not exhaustive — a sufficiently determined contributor
// can write a plausible-looking arbitrary date — but combined with the
// floor / ceiling window it forces a contributor to commit to a real,
// recent date rather than a stub.
var obviouslyStubVerifiedAt = map[string]struct{}{
	"0001-01-01": {},
	"1970-01-01": {},
	"2000-01-01": {},
	"2099-01-01": {},
	"9999-12-31": {},
}

// TestRepositoryCatalogVerificationProvenanceComplete pins that every
// committed entry carries non-trivial provenance. The validator already
// rejects empty fields and unparseable dates, but this test goes
// further: it makes sure nobody papered over a contribution by dropping
// in stub values like "TODO" or "0001-01-01" to make the validator
// pass. The entries below must point at real cloud.google.com or
// terraform-provider-google URLs, must have a verified_at that parses
// as YYYY-MM-DD, must avoid the obvious-stub date list, and must fall
// inside [provenanceFloorDate, today + provenanceCeilingSlack].
//
// This is not a substitute for human review — a contributor can still
// fabricate plausible-looking citations — but it raises the bar above
// "the validator passes" and discourages copy-paste-with-blanks.
//
// Previous review feedback specifically called out that an earlier
// version of this test claimed to verify verified_at but never
// inspected the field. The verified_at block below is the fix: parse
// it explicitly, reject the obvious-stub list, and bound it inside a
// fixed window. If the assertion regresses, the implementation gap the
// reviewer originally flagged would silently return.
func TestRepositoryCatalogVerificationProvenanceComplete(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	ceiling := time.Now().UTC().Add(provenanceCeilingSlack)

	check := func(loc string, v Verification, tested string) {
		t.Helper()
		// At least one source URL must be a real GCP or provider-source
		// citation. We accept either domain so a contributor can choose
		// whichever is more authoritative for the resource.
		sawCitation := false
		for _, u := range v.SourceURLs {
			if strings.Contains(u, "cloud.google.com") ||
				strings.Contains(u, "github.com/hashicorp/terraform-provider-google") {
				sawCitation = true
				break
			}
		}
		if !sawCitation {
			t.Errorf("%s: source_urls %v contains no recognised citation domain", loc, v.SourceURLs)
		}

		// verified_at is parsed, then range-checked. The validator
		// already rejects unparseable dates, but the validator does
		// not check for plausibility, so a contributor can pass it by
		// writing "0001-01-01". The range check below blocks that
		// hatch.
		if _, isStub := obviouslyStubVerifiedAt[v.VerifiedAt]; isStub {
			t.Errorf("%s: verification.verified_at %q is on the obvious-stub list — write the actual date the verification ran", loc, v.VerifiedAt)
		}
		parsedDate, parseErr := time.Parse(verifiedAtLayout, v.VerifiedAt)
		if parseErr != nil {
			// The validator already enforces YYYY-MM-DD parseability,
			// so this branch should be unreachable in CI. We still
			// report it explicitly so a future loosening of the
			// validator does not silently bypass the range check
			// below.
			t.Errorf("%s: verification.verified_at %q failed YYYY-MM-DD parse: %v", loc, v.VerifiedAt, parseErr)
		} else {
			if parsedDate.Before(provenanceFloorDate) {
				t.Errorf("%s: verification.verified_at %q is before the project floor (%s)", loc, v.VerifiedAt, provenanceFloorDate.Format(verifiedAtLayout))
			}
			if parsedDate.After(ceiling) {
				t.Errorf("%s: verification.verified_at %q is more than %s in the future (now=%s)", loc, v.VerifiedAt, provenanceCeilingSlack, ceiling.Format(verifiedAtLayout))
			}
		}

		// verified_provider_version should look like a semver-ish
		// release, not a placeholder. We do not parse it strictly —
		// the catalog accepts any non-empty string — but flag obvious
		// placeholders.
		if v.VerifiedProviderVersion == "TODO" || v.VerifiedProviderVersion == "0.0.0" {
			t.Errorf("%s: verified_provider_version %q looks like a placeholder", loc, v.VerifiedProviderVersion)
		}
		if tested == "TODO" || tested == "" {
			t.Errorf("%s: tested_against_provider %q looks like a placeholder", loc, tested)
		}
	}
	for typ, e := range cat.Resources {
		check("resources/"+typ, e.Verification, e.TestedAgainstProvider)
	}
	for typ, e := range cat.DataSources {
		check("data_sources/"+typ, e.Verification, e.TestedAgainstProvider)
	}
	for typ, e := range cat.IAMBindings {
		check("iam_bindings/"+typ, e.Verification, e.TestedAgainstProvider)
	}
}

// topTierEmpiricalResources is the canonical Epic 4 list of resource
// types that MUST be empirically verified before they may be marked
// empirical in the catalog YAML. The list maps a Terraform resource
// type to the rationale for why it is on the tier-1 list; the
// rationale string is surfaced in test failures so a future maintainer
// reading a CI log understands why a particular resource is on the
// list rather than having to grep the PDR.
//
// Membership rules:
//
//   - A resource is added to this map only after a human contributor
//     performed empirical verification (see CONTRIBUTING.md) and
//     captured the run in the entry's verification block. Adding to
//     this list without changing the YAML to method: empirical fails
//     TestRepositoryCatalogTopTierResourcesAreEmpirical.
//   - A resource may be marked method: empirical in YAML only if it
//     also appears in this map. The reverse check
//     (TestRepositoryCatalogEmpiricalEntriesAreOnTopTierList) blocks
//     unauthorised empirical claims.
//   - The PDR (docs/tfperms_pdr.md, Epic 4) calls out approximately
//     10-15 top-tier resources and gives examples
//     (google_compute_instance, google_storage_bucket,
//     google_bigquery_dataset, google_pubsub_topic,
//     google_cloud_run_service, google_sql_database_instance, ...).
//     Those examples are tracked as the population this list will
//     eventually grow to cover; the actual membership is gated on a
//     human running the verification in a real GCP project.
//
// The list is empty in this initial Epic 4 schema-and-loader branch
// because no empirical verification has been performed yet. The
// adjacent two tests (TopTierResourcesAreEmpirical /
// EmpiricalEntriesAreOnTopTierList) enforce the contract regardless of
// list size, so future PRs that do perform empirical verification can
// add an entry here AND flip the YAML method to empirical with the
// machine-checkable assurance that the two stay in lockstep.
var topTierEmpiricalResources = map[string]string{}

// TestRepositoryCatalogTopTierResourcesAreEmpirical enforces the Epic
// 4 contract that every resource the project has designated as
// "top-tier" (empirically verified by a human) carries
// method: empirical in the catalog YAML. The previous review flagged
// the absence of this gate as a correctness issue: without it, a
// contributor can claim a resource is top-tier in code review prose
// while shipping a docs+source mapping, and CI does not notice.
//
// Test mechanics:
//
//   - Iterate topTierEmpiricalResources in lexicographic key order so
//     a multi-failure run produces a stable diagnostic.
//   - For each declared top-tier resource, look it up in the merged
//     catalog. A missing entry is a hard failure — the list cannot
//     contain ghost types.
//   - Assert the entry's Verification.Method is exactly
//     VerificationMethodEmpirical. Anything else means the contributor
//     promoted the type without performing the verification.
//
// The test's pass-by-default behaviour with an empty top-tier list is
// intentional: shipping the rule machinery before any entry actually
// satisfies it lets a future "empirically verify google_storage_bucket"
// PR be a single-file YAML change plus a single-line list addition,
// gated entirely by the existing test instead of needing fresh test
// scaffolding.
func TestRepositoryCatalogTopTierResourcesAreEmpirical(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Sort for deterministic diagnostics. sortedKeys is the same
	// helper the loader uses, which keeps the per-key error order
	// consistent with everything else this test file emits.
	for _, typ := range sortedKeys(topTierEmpiricalResources) {
		rationale := topTierEmpiricalResources[typ]
		entry, ok := cat.Resources[typ]
		if !ok {
			t.Errorf("topTierEmpiricalResources lists %q (%s) but it is not in the catalog — either add the YAML entry or remove from the top-tier list",
				typ, rationale)
			continue
		}
		if entry.Verification.Method != VerificationMethodEmpirical {
			t.Errorf("resources/%s is on the top-tier list (%s) but verification.method = %q, want %q — perform the empirical verification or remove from the top-tier list",
				typ, rationale, entry.Verification.Method, VerificationMethodEmpirical)
		}
	}
}

// TestRepositoryCatalogEmpiricalEntriesAreOnTopTierList is the inverse
// of TestRepositoryCatalogTopTierResourcesAreEmpirical: every entry
// that claims method: empirical in YAML must appear on the top-tier
// list. This guards against a contributor stamping empirical on a
// resource without going through the topTierEmpiricalResources gate
// (where the rationale and review trail live).
//
// The pair of tests together implements a bidirectional contract:
//
//	YAML method == empirical  ⇔  type ∈ topTierEmpiricalResources
//
// Any drift between the two surfaces in CI rather than in user-visible
// permission output. A contributor moving a resource into the empirical
// tier must update both sides in the same diff; a reviewer can confirm
// that by reading the diff alone.
func TestRepositoryCatalogEmpiricalEntriesAreOnTopTierList(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	for _, typ := range sortedKeys(cat.Resources) {
		entry := cat.Resources[typ]
		if entry.Verification.Method != VerificationMethodEmpirical {
			continue
		}
		if _, ok := topTierEmpiricalResources[typ]; !ok {
			t.Errorf("resources/%s is marked method: empirical but is not on the topTierEmpiricalResources list — add it (with a rationale) or change the YAML method back to docs+source",
				typ)
		}
	}

	// Data sources and IAM bindings are read-only / boilerplate and
	// are not part of the Epic 4 top-tier scope; the PDR only calls
	// out core resources for empirical verification. We still surface
	// any empirical claims on those shapes so a future spec change
	// does not silently accept claims this test currently does not
	// model.
	for _, typ := range sortedKeys(cat.DataSources) {
		entry := cat.DataSources[typ]
		if entry.Verification.Method == VerificationMethodEmpirical {
			t.Errorf("data_sources/%s is marked method: empirical — empirical verification is currently scoped to resources by Epic 4; revisit the contract before adding empirical claims to data sources",
				typ)
		}
	}
	for _, typ := range sortedKeys(cat.IAMBindings) {
		entry := cat.IAMBindings[typ]
		if entry.Verification.Method == VerificationMethodEmpirical {
			t.Errorf("iam_bindings/%s is marked method: empirical — empirical verification is currently scoped to resources by Epic 4; revisit the contract before adding empirical claims to IAM bindings",
				typ)
		}
	}
}

// keysOf returns the keys of m as a slice so callers can compare
// map-key sets without writing a loop at every call site.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// checkSetEquality reports any keys present in got but missing from
// want, and vice versa, with the kind label embedded in the error
// message ("resources", "data_sources", "iam_bindings"). It is the
// "did this YAML add or remove a top-level entry without updating the
// expected table" check.
func checkSetEquality(t *testing.T, kind string, got, want []string) {
	t.Helper()
	gotSet := make(map[string]struct{}, len(got))
	for _, k := range got {
		gotSet[k] = struct{}{}
	}
	wantSet := make(map[string]struct{}, len(want))
	for _, k := range want {
		wantSet[k] = struct{}{}
	}
	for k := range gotSet {
		if _, ok := wantSet[k]; !ok {
			t.Errorf("%s/%s present in catalog but not locked in expected table — add it to TestRepositoryCatalogPermissionsAreLocked",
				kind, k)
		}
	}
	for k := range wantSet {
		if _, ok := gotSet[k]; !ok {
			t.Errorf("%s/%s expected by lock table but missing from catalog — was it removed without updating the test?",
				kind, k)
		}
	}
}

// assertList compares two string slices for exact equality (order
// included). The catalog style guide requires alphabetic sorting per
// list, so a list ordering difference is a real bug, not test noise.
func assertList(t *testing.T, loc string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %v, want %v", loc, got, want)
	}
}
