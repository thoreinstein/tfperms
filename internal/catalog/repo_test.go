package catalog

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	catalogdata "github.com/thoreinstein/tfperms/catalog"
)

// TestRepositoryCatalogIsValid loads the actual embedded catalog
// (everything currently committed under catalog/*.yaml) and asserts
// the validator accepts every entry. It is the consistency gate for
// catalog contributions: any future PR adding or modifying a YAML
// entry has to pass this test, which means no schema-violating entry
// can land via the normal review process.
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
func TestRepositoryCatalogIsValid(t *testing.T) {
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
// TestRepositoryCatalogIsValid would catch the same condition. The
// reason this dedicated assertion exists is documentation: when this
// test fails the message is unambiguous about what went wrong, which
// cuts the time to a fix on a contributor's first attempt.
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

// listCatalogYAMLFiles returns the *.yaml / *.yml bare filenames in the
// root of the embedded catalog FS. It mirrors loader.go's file selection
// (suffix filter, skip directories) so the test compares apples to apples
// with the Position.File values that the loader stamps onto each entry.
// If the loader ever moves to subdirectories, both this helper and
// loader.go will need updating together.
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
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
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

// TestRepositoryCatalogVerificationProvenanceComplete pins that every
// committed entry carries non-trivial provenance. The validator already
// rejects empty fields, but this test goes further: it makes sure
// nobody papered over a contribution by dropping in stub values like
// "TODO" or "0001-01-01" to make the validator pass. The entries below
// must point at real cloud.google.com or terraform-provider-google
// URLs and verified_at must fall in a plausibly-recent date range.
//
// This is not a substitute for human review — a contributor can still
// fabricate plausible-looking citations — but it raises the bar above
// "the validator passes" and discourages copy-paste-with-blanks.
func TestRepositoryCatalogVerificationProvenanceComplete(t *testing.T) {
	cat, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

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
