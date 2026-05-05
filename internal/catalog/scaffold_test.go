package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestInferServicePath pins the service-routing rule documented in the
// PR description: google_<service>_* → <service>.yaml, with a misc.yaml
// fallback for anything we cannot decompose. The cases include the
// failure modes (non-google prefix, bare prefix, unrecognised
// vocabulary) because the routing decision is silent — a wrong route
// would land an entry in the wrong file without a clear signal.
func TestInferServicePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "single-segment service", in: "google_storage_bucket", want: "storage.yaml"},
		{name: "multi-segment after service", in: "google_sql_database_instance", want: "sql.yaml"},
		{name: "service with underscore in prefix only", in: "google_dataplex_lake", want: "dataplex.yaml"},
		{name: "iam binding type", in: "google_storage_bucket_iam_binding", want: "storage.yaml"},
		{name: "non-google prefix falls back to misc", in: "aws_s3_bucket", want: "misc.yaml"},
		{name: "bare google prefix falls back to misc", in: "google_", want: "misc.yaml"},
		{name: "no underscore after prefix", in: "google_compute", want: "compute.yaml"},
		{name: "empty input falls back to misc", in: "", want: "misc.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InferServicePath(tc.in); got != tc.want {
				t.Errorf("InferServicePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCheckDuplicateExistingEntry confirms CheckDuplicate sees an entry
// the loader would see. The fixture is a real catalog YAML body so the
// test fails the same way the loader would on a malformed file — keeping
// the two views aligned protects against drift between scaffold's
// idempotency check and the loader's notion of "defined".
func TestCheckDuplicateExistingEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.yaml")
	body := `resources:
  google_storage_bucket:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [storage.buckets.get]
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
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cases := []struct {
		name        string
		section     Section
		typ         string
		wantErrIs   error
		wantNoError bool
	}{
		{name: "resource duplicate", section: SectionResources, typ: "google_storage_bucket", wantErrIs: ErrDuplicateEntry},
		{name: "data source duplicate", section: SectionDataSources, typ: "google_storage_bucket", wantErrIs: ErrDuplicateEntry},
		{name: "resource not declared", section: SectionResources, typ: "google_storage_object", wantNoError: true},
		{name: "iam binding section absent in file", section: SectionIAMBindings, typ: "google_storage_bucket_iam_binding", wantNoError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckDuplicate(path, tc.typ, tc.section)
			if tc.wantNoError {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("err = %v, want errors.Is(%v) == true", err, tc.wantErrIs)
			}
		})
	}
}

// TestCheckDuplicateMissingFile confirms a non-existent path is treated
// as "no duplicates possible" rather than an error, so the cobra layer
// can call CheckDuplicate before deciding whether to create or append.
func TestCheckDuplicateMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.yaml")
	if err := CheckDuplicate(path, "google_storage_bucket", SectionResources); err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
}

// TestCheckDuplicateUnknownSection guards the switch statement: a future
// fourth Section value would silently skip the duplicate check on the
// default branch, and that would be a correctness regression. An
// explicit error keeps the failure loud.
func TestCheckDuplicateUnknownSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.yaml")
	if err := os.WriteFile(path, []byte("resources: {}\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	err := CheckDuplicate(path, "google_storage_bucket", Section("unknown"))
	if err == nil || !strings.Contains(err.Error(), "unknown section") {
		t.Fatalf("expected 'unknown section' error, got %v", err)
	}
}

// TestGenerateStubResourceShape confirms the resource fragment has all
// the required top-level keys and that they match the schema's struct
// tags. Re-decoding into the loader's rawFile shape catches indentation
// or section-name regressions that a string match would miss.
func TestGenerateStubResourceShape(t *testing.T) {
	stub, err := GenerateStub("google_dataplex_lake", SectionResources)
	if err != nil {
		t.Fatalf("GenerateStub: %v", err)
	}

	var raw rawFile
	if err := yaml.Unmarshal(stub, &raw); err != nil {
		t.Fatalf("stub does not parse as catalog YAML: %v\n%s", err, stub)
	}
	node, ok := raw.Resources["google_dataplex_lake"]
	if !ok {
		t.Fatalf("expected google_dataplex_lake under resources, stub:\n%s", stub)
	}
	// Decode into the typed entry to confirm every required field is
	// present and that the TODO sentinels live in fields whose YAML
	// tag matches the schema.
	var entry ResourceEntry
	if err := node.Decode(&entry); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if entry.Verification.Method != "TODO" {
		t.Errorf("verification.method = %q, want TODO", entry.Verification.Method)
	}
	if len(entry.Verification.SourceURLs) != 1 || entry.Verification.SourceURLs[0] != "TODO" {
		t.Errorf("verification.source_urls = %v, want [TODO]", entry.Verification.SourceURLs)
	}
	if entry.Verification.VerifiedAt != "TODO" {
		t.Errorf("verification.verified_at = %q, want TODO", entry.Verification.VerifiedAt)
	}
	if entry.Verification.VerifiedProviderVersion != "TODO" {
		t.Errorf("verification.verified_provider_version = %q, want TODO", entry.Verification.VerifiedProviderVersion)
	}
	if entry.TestedAgainstProvider != "TODO" {
		t.Errorf("tested_against_provider = %q, want TODO", entry.TestedAgainstProvider)
	}
	for stage, got := range map[string][]string{
		"plan":   entry.Permissions.Plan,
		"create": entry.Permissions.Create,
		"update": entry.Permissions.Update,
		"delete": entry.Permissions.Delete,
	} {
		if len(got) != 1 || got[0] != "TODO" {
			t.Errorf("permissions.%s = %v, want [TODO]", stage, got)
		}
	}
}

// TestGenerateStubDataSourceShape confirms the data-source fragment
// emits only the plan permission list. Emitting create/update/delete
// here would break strict decoding at load time because
// DataSourcePermissions has no fields for those stages.
func TestGenerateStubDataSourceShape(t *testing.T) {
	stub, err := GenerateStub("google_storage_bucket", SectionDataSources)
	if err != nil {
		t.Fatalf("GenerateStub: %v", err)
	}
	var raw rawFile
	if err := yaml.Unmarshal(stub, &raw); err != nil {
		t.Fatalf("parse: %v\n%s", err, stub)
	}
	node, ok := raw.DataSources["google_storage_bucket"]
	if !ok {
		t.Fatalf("expected google_storage_bucket under data_sources, stub:\n%s", stub)
	}
	var entry DataSourceEntry
	// Strict decode — any create/update/delete keys here would fail.
	if err := strictDecodeNode(&node, &entry); err != nil {
		t.Fatalf("data source stub fails strict decode: %v", err)
	}
	if len(entry.Permissions.Plan) != 1 || entry.Permissions.Plan[0] != "TODO" {
		t.Errorf("permissions.plan = %v, want [TODO]", entry.Permissions.Plan)
	}
}

// TestGenerateStubIAMBindingShape confirms the iam_bindings fragment
// includes a parent_resource: TODO field. Without it the contributor
// would leave the binding pointing at nothing and the validator's
// cross-reference check would be unable to pinpoint what was missing.
func TestGenerateStubIAMBindingShape(t *testing.T) {
	stub, err := GenerateStub("google_storage_bucket_iam_binding", SectionIAMBindings)
	if err != nil {
		t.Fatalf("GenerateStub: %v", err)
	}
	var raw rawFile
	if err := yaml.Unmarshal(stub, &raw); err != nil {
		t.Fatalf("parse: %v\n%s", err, stub)
	}
	node, ok := raw.IAMBindings["google_storage_bucket_iam_binding"]
	if !ok {
		t.Fatalf("expected entry under iam_bindings, stub:\n%s", stub)
	}
	var entry IAMBindingEntry
	if err := node.Decode(&entry); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if entry.ParentResource != "TODO" {
		t.Errorf("parent_resource = %q, want TODO", entry.ParentResource)
	}
}

// TestGenerateStubUnknownSection mirrors the CheckDuplicate guard — a
// new Section value must update GenerateStub or return an explicit
// error rather than silently producing an empty document.
func TestGenerateStubUnknownSection(t *testing.T) {
	if _, err := GenerateStub("google_storage_bucket", Section("unknown")); err == nil {
		t.Fatalf("expected error for unknown section, got nil")
	}
}

// TestScaffoldCreatesNewFile is the create-path end-to-end test: no
// pre-existing file, single Scaffold call, the file appears with a
// valid stub. It also pins the result message format because the
// cobra layer prints it directly to stdout.
func TestScaffoldCreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dataplex.yaml")
	res, err := Scaffold(ScaffoldRequest{
		ResourceType: "google_dataplex_lake",
		Section:      SectionResources,
		TargetPath:   path,
	})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if !res.Created {
		t.Errorf("Created = false, want true")
	}
	if !strings.Contains(res.Message, "google_dataplex_lake") || !strings.Contains(res.Message, path) {
		t.Errorf("Message = %q, expected to mention type and path", res.Message)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "google_dataplex_lake:") {
		t.Errorf("file body missing entry key:\n%s", body)
	}
}

// TestScaffoldMergesIntoExistingSection covers the merge path: a file
// with one entry plus a Scaffold call for a second entry must produce
// a single YAML document containing both entries under one resources:
// heading. This is the loader-compatibility contract — the loader
// only calls Decode once, so two top-level mappings would silently
// drop one entry.
func TestScaffoldMergesIntoExistingSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compute.yaml")
	seed := `resources:
  google_compute_instance:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [compute.instances.get]
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := Scaffold(ScaffoldRequest{
		ResourceType: "google_compute_disk",
		Section:      SectionResources,
		TargetPath:   path,
	})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if res.Created {
		t.Errorf("Created = true, want false (merge path)")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The merged file must be a single YAML document under the
	// loader's strict decoder — a double `resources:` heading would
	// either fail to parse or cause the loader to drop the first
	// entry.
	var raw rawFile
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("merged file does not parse: %v\n%s", err, body)
	}
	if _, ok := raw.Resources["google_compute_instance"]; !ok {
		t.Errorf("merged file missing google_compute_instance:\n%s", body)
	}
	if _, ok := raw.Resources["google_compute_disk"]; !ok {
		t.Errorf("merged file missing google_compute_disk:\n%s", body)
	}
	// Scanning twice would drop entries silently; the strict decoder
	// rejects the case explicitly.
	if err := dec.Decode(&raw); err == nil {
		t.Errorf("expected single-document file, got more than one")
	}
}

// TestScaffoldAppendsNewSection covers the case where the existing
// file has only `resources:` and the user runs scaffold with
// --data-source: the data_sources: heading must be added at the
// top level, not nested under resources.
func TestScaffoldAppendsNewSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compute.yaml")
	seed := `resources:
  google_compute_instance:
    verification:
      method: docs+source
      source_urls: [https://example.test/iam]
      verified_at: "2025-12-15"
      verified_provider_version: "6.12.0"
    tested_against_provider: ">=5.0.0,<7.0.0"
    permissions:
      plan: [compute.instances.get]
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Scaffold(ScaffoldRequest{
		ResourceType: "google_compute_instance",
		Section:      SectionDataSources,
		TargetPath:   path,
	}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw rawFile
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("merged file does not parse: %v\n%s", err, body)
	}
	if _, ok := raw.Resources["google_compute_instance"]; !ok {
		t.Errorf("expected resources entry to survive, got:\n%s", body)
	}
	if _, ok := raw.DataSources["google_compute_instance"]; !ok {
		t.Errorf("expected data_sources entry to be added, got:\n%s", body)
	}
}

// TestScaffoldRefusesDuplicate confirms re-scaffolding the same entry
// surfaces ErrDuplicateEntry rather than overwriting the existing file.
// The cobra layer maps this to a non-zero exit status, which is the
// acceptance criterion in the issue.
func TestScaffoldRefusesDuplicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dataplex.yaml")
	if _, err := Scaffold(ScaffoldRequest{
		ResourceType: "google_dataplex_lake",
		Section:      SectionResources,
		TargetPath:   path,
	}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, err = Scaffold(ScaffoldRequest{
		ResourceType: "google_dataplex_lake",
		Section:      SectionResources,
		TargetPath:   path,
	})
	if !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("err = %v, want errors.Is(ErrDuplicateEntry) == true", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("file content changed after duplicate scaffold; original:\n%s\nafter:\n%s", original, after)
	}
}

// TestScaffoldEmptyResourceType guards the input-validation contract:
// empty input is rejected with a clear error rather than producing a
// stub keyed on the empty string.
func TestScaffoldEmptyResourceType(t *testing.T) {
	dir := t.TempDir()
	if _, err := Scaffold(ScaffoldRequest{
		ResourceType: "   ",
		Section:      SectionResources,
		TargetPath:   filepath.Join(dir, "x.yaml"),
	}); err == nil {
		t.Fatal("expected error for empty resource type, got nil")
	}
}

// TestScaffoldCreatesParentDir confirms Scaffold creates the catalog
// directory if it does not exist. The cobra layer composes paths like
// "catalog/dataplex.yaml" without ensuring the directory; the helper
// must MkdirAll on its behalf.
func TestScaffoldCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh", "subdir", "dataplex.yaml")
	if _, err := Scaffold(ScaffoldRequest{
		ResourceType: "google_dataplex_lake",
		Section:      SectionResources,
		TargetPath:   path,
	}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s, stat: %v", path, err)
	}
}
