package catalog

// scaffold.go implements `tfperms catalog scaffold`. The command writes
// a stub YAML entry for a Terraform resource / data source / IAM binding
// to the appropriate service file under the repository's catalog/
// directory.
//
// Layout:
//   - Section + InferServicePath: pure helpers used by the cobra layer
//     and the unit tests. These never touch the filesystem.
//   - Scaffold + ScaffoldRequest: the high-level entry point used by the
//     cobra layer. It composes the helpers and the file-management logic
//     into one call.
//   - GenerateStub: produces a YAML fragment with TODO sentinels for the
//     given resource type and section. Goes through yaml.Marshal so the
//     output is guaranteed syntactically valid even if the schema gains
//     new required fields later — a field-by-field string template would
//     drift.
//   - CheckDuplicate: parses an existing file via the loader's rawFile
//     shape and reports whether the resource type is already declared in
//     the requested section. Reusing rawFile is deliberate; a separate
//     "is this key present" implementation would silently drift from the
//     loader's view of what counts as a defined entry.
//
// The file-management policy preserves the existing file as a single
// YAML document. When the target file does not exist, scaffold writes
// the stub as the entire body. When it exists, scaffold parses it as a
// yaml.Node tree, splices the new entry into the appropriate top-level
// section (creating the section heading if necessary), and re-serialises
// the tree. Going through yaml.Node — rather than appending raw text —
// guarantees the result is one well-formed YAML document that the
// loader's single-document Decode call can consume; a naive
// concatenation produces two top-level mappings which yaml.v3 silently
// drops one of.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Section names a top-level section in a catalog YAML file. Exported so
// the cobra layer can pick a section from --data-source / --iam-binding
// flags without re-deriving the YAML key from a string.
type Section string

// Recognised Section values. These mirror the rawFile struct tags in
// loader.go; if a fourth section is ever added there, this enum and
// every switch over it must be extended in lockstep.
const (
	SectionResources   Section = "resources"
	SectionDataSources Section = "data_sources"
	SectionIAMBindings Section = "iam_bindings"
)

// ScaffoldRequest carries the parameters for one Scaffold call.
//
// ResourceType is the Terraform type the user passed on the CLI
// (e.g. "google_dataplex_lake"). It is used both as the YAML map key
// for the new entry and as the input to InferServicePath.
//
// Section selects which top-level YAML section the new entry lands in.
// It is kept distinct from a boolean flag pair on the CLI because three
// modes (resources / data_sources / iam_bindings) do not map cleanly to
// two booleans.
//
// TargetPath is the relative or absolute path to the YAML file the
// scaffold will create or append to. The cobra layer is responsible for
// composing this from a --catalog-dir flag and InferServicePath; keeping
// the composition in the cobra layer means tests of Scaffold can place
// fixtures in t.TempDir() without a separate "where is catalog/" hook.
type ScaffoldRequest struct {
	ResourceType string
	Section      Section
	TargetPath   string
}

// ScaffoldResult is the success return from Scaffold. Message is a
// single human-readable line ready to print to stdout (e.g.
// "wrote stub for google_dataplex_lake to catalog/dataplex.yaml").
// Created is true when the target file did not exist before the call,
// false when the call appended to an existing file. Tests use Created
// to assert the create-vs-append branch chose the right path.
type ScaffoldResult struct {
	Message string
	Created bool
}

// ErrDuplicateEntry is returned by Scaffold when the requested resource
// type already appears in the requested section of the target file.
// Wrapped with errors.Is so callers (including the cobra RunE) can map
// it to a specific exit code without parsing error strings. The cobra
// layer relies on errors.Is, not on string matching.
var ErrDuplicateEntry = errors.New("catalog scaffold: entry already exists")

// InferServicePath maps a Terraform resource type to the basename of
// the catalog YAML file it should land in.
//
// The rule is the project convention "google_<service>_*" → "<service>.yaml".
// Anything that does not start with "google_" or that has no segment
// after the prefix falls back to "misc.yaml". A bare "google_" with no
// suffix also routes to misc.yaml — the inference cannot recover a
// service name from an empty token.
//
// The function is pure: no IO, no logging, no filesystem access. The
// CLI layer composes its result with a directory prefix to produce the
// final path.
func InferServicePath(resourceType string) string {
	const prefix = "google_"
	if !strings.HasPrefix(resourceType, prefix) {
		return "misc.yaml"
	}
	rest := strings.TrimPrefix(resourceType, prefix)
	if rest == "" {
		return "misc.yaml"
	}
	// Service is the first underscore-delimited segment after the
	// prefix. "google_storage_bucket" → "storage";
	// "google_sql_database_instance" → "sql".
	service, _, _ := strings.Cut(rest, "_")
	if service == "" {
		return "misc.yaml"
	}
	return service + ".yaml"
}

// CheckDuplicate reports whether resourceType is already declared in
// section of the YAML file at path. A nil error means the entry can be
// safely added; ErrDuplicateEntry signals a clash; any other error
// means the file could not be parsed.
//
// The check intentionally uses the loader's rawFile shape so it sees
// exactly the same set of "defined" entries that Load would. A
// separate string-search implementation would miss commented-out
// entries (correctly) but also miss entries declared under a YAML
// anchor or alias — and silently disagreeing with the loader is a
// landmine for users.
//
// A non-existent path is not an error: the file simply has no entries
// yet, so no duplicate is possible. This lets callers use
// CheckDuplicate before deciding whether to create-or-append without a
// separate os.Stat round trip.
func CheckDuplicate(path, resourceType string, section Section) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("catalog scaffold: read %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}

	var raw rawFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Strict mode would reject any unknown top-level key in the
	// existing file, even though we only care about three sections.
	// Existing catalog files only declare the three known sections,
	// so strict mode is safe and prevents a stale typo'd section
	// from masking a duplicate.
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("catalog scaffold: parse %q: %w", path, err)
	}

	var entries map[string]yaml.Node
	switch section {
	case SectionResources:
		entries = raw.Resources
	case SectionDataSources:
		entries = raw.DataSources
	case SectionIAMBindings:
		entries = raw.IAMBindings
	default:
		return fmt.Errorf("catalog scaffold: unknown section %q", section)
	}
	if _, ok := entries[resourceType]; ok {
		return fmt.Errorf("%w: %s already declared in %s of %s",
			ErrDuplicateEntry, resourceType, section, path)
	}
	return nil
}

// GenerateStub returns a YAML fragment for the given resource type in
// the given section. The fragment is a complete top-level document with
// exactly one section heading — callers append it to an existing
// catalog file or write it as a new file.
//
// The output is produced by yaml.Marshal of a typed value, not by
// string templating. This guarantees the fragment is syntactically
// valid YAML even if the schema gains new required fields and an old
// scaffold path is missed; the worst case becomes "missing field"
// at validate time rather than "syntactically broken file" at parse
// time.
//
// All required schema fields are populated with TODO sentinels. The
// sentinels are syntactically valid YAML (strings or 1-element lists of
// strings) so the file parses; they will fail catalog-validate, which
// is the point: the contributor must replace each one with a real
// value.
func GenerateStub(resourceType string, section Section) ([]byte, error) {
	stub := stubEntry()

	var doc map[string]map[string]any
	switch section {
	case SectionResources:
		doc = map[string]map[string]any{
			"resources": {resourceType: stub},
		}
	case SectionDataSources:
		// Data sources are read-only: only a plan permission list,
		// and no create/update/delete keys.
		doc = map[string]map[string]any{
			"data_sources": {resourceType: dataSourceStub()},
		}
	case SectionIAMBindings:
		// IAM bindings additionally carry a parent_resource pointer
		// at the resource the binding applies to.
		entry := stubEntry()
		entry["parent_resource"] = "TODO"
		doc = map[string]map[string]any{
			"iam_bindings": {resourceType: entry},
		}
	default:
		return nil, fmt.Errorf("catalog scaffold: unknown section %q", section)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("catalog scaffold: encode stub: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("catalog scaffold: close encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// stubEntry builds the resource-shaped portion of the stub: a full
// PermissionSet (plan/create/update/delete), verification block, and
// tested_against_provider field, all populated with TODO sentinels.
//
// Returned as map[string]any rather than a typed struct because the
// stub deliberately uses string TODO sentinels in places the typed
// schema requires non-string types (e.g. verified_at is a date string,
// method is an enum). yaml.Marshal of a typed struct with TODO in those
// fields would either need extra omitempty handling or duplicate the
// schema's struct tags here. Map-shaped output keeps the stub format
// readable and the schema tags as the single source of truth for
// production decode paths.
func stubEntry() map[string]any {
	return map[string]any{
		"verification": map[string]any{
			"method":                    "TODO",
			"source_urls":               []string{"TODO"},
			"verified_at":               "TODO",
			"verified_provider_version": "TODO",
		},
		"tested_against_provider": "TODO",
		"permissions": map[string]any{
			"plan":   []string{"TODO"},
			"create": []string{"TODO"},
			"update": []string{"TODO"},
			"delete": []string{"TODO"},
		},
	}
}

// dataSourceStub is the read-only counterpart of stubEntry. Data
// sources only carry a plan permission list (see DataSourcePermissions
// in catalog.go) so emitting create/update/delete here would produce a
// fragment that fails strict decode at load time.
func dataSourceStub() map[string]any {
	return map[string]any{
		"verification": map[string]any{
			"method":                    "TODO",
			"source_urls":               []string{"TODO"},
			"verified_at":               "TODO",
			"verified_provider_version": "TODO",
		},
		"tested_against_provider": "TODO",
		"permissions": map[string]any{
			"plan": []string{"TODO"},
		},
	}
}

// Scaffold is the high-level entry point invoked by the cobra command.
// It checks for a duplicate, generates the stub, ensures the target
// directory exists, and writes the file (creating or merging).
//
// Concurrency: Scaffold is not safe for concurrent calls against the
// same TargetPath. Two concurrent invocations could both observe
// "no duplicate" and then both write, producing a doubled entry or a
// torn file. The CLI is single-shot and this is fine; if the function
// is ever wired into a long-running daemon, a per-file mutex (or fcntl
// lock) is the natural place to add coordination.
func Scaffold(req ScaffoldRequest) (ScaffoldResult, error) {
	if strings.TrimSpace(req.ResourceType) == "" {
		return ScaffoldResult{}, fmt.Errorf("catalog scaffold: resource type is required")
	}
	if req.TargetPath == "" {
		return ScaffoldResult{}, fmt.Errorf("catalog scaffold: target path is required")
	}
	if _, err := sectionKey(req.Section); err != nil {
		return ScaffoldResult{}, err
	}

	if err := CheckDuplicate(req.TargetPath, req.ResourceType, req.Section); err != nil {
		return ScaffoldResult{}, err
	}

	if err := os.MkdirAll(filepath.Dir(req.TargetPath), 0o755); err != nil {
		return ScaffoldResult{}, fmt.Errorf("catalog scaffold: ensure dir: %w", err)
	}

	created, err := writeOrMerge(req)
	if err != nil {
		return ScaffoldResult{}, err
	}

	verb := "merged stub for"
	if created {
		verb = "created stub for"
	}
	return ScaffoldResult{
		Message: fmt.Sprintf("%s %s in %s", verb, req.ResourceType, req.TargetPath),
		Created: created,
	}, nil
}

// sectionKey converts a Section to its YAML top-level key. A central
// helper keeps the section-name strings in one place; misspelling one
// here would silently drop scaffolded entries onto the wrong heading.
func sectionKey(s Section) (string, error) {
	switch s {
	case SectionResources:
		return "resources", nil
	case SectionDataSources:
		return "data_sources", nil
	case SectionIAMBindings:
		return "iam_bindings", nil
	default:
		return "", fmt.Errorf("catalog scaffold: unknown section %q", s)
	}
}

// writeOrMerge is the file-management half of Scaffold. When the target
// file does not exist it is created from a freshly generated stub. When
// it exists, the file is parsed as a yaml.Node tree, the new entry is
// spliced into the appropriate section (creating the section heading
// if missing), and the tree is re-serialised.
//
// The yaml.Node round trip preserves comments and key ordering for
// existing entries, which matters because catalog files routinely
// carry inline rationale comments next to non-obvious permissions
// (see storage.yaml). A simple "append text" approach would either
// produce two top-level mappings (which yaml.v3's single-document
// decoder silently drops) or wipe the comments.
func writeOrMerge(req ScaffoldRequest) (created bool, err error) {
	existing, readErr := os.ReadFile(req.TargetPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, fmt.Errorf("catalog scaffold: read %q: %w", req.TargetPath, readErr)
	}

	if errors.Is(readErr, os.ErrNotExist) || len(bytes.TrimSpace(existing)) == 0 {
		stub, err := GenerateStub(req.ResourceType, req.Section)
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(req.TargetPath, stub, 0o644); err != nil {
			return false, fmt.Errorf("catalog scaffold: write %q: %w", req.TargetPath, err)
		}
		return true, nil
	}

	merged, err := mergeIntoDocument(existing, req.ResourceType, req.Section)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(req.TargetPath, merged, 0o644); err != nil {
		return false, fmt.Errorf("catalog scaffold: write %q: %w", req.TargetPath, err)
	}
	return false, nil
}

// mergeIntoDocument parses existing as a yaml.Node tree, splices a stub
// entry for resourceType into section, and returns the re-serialised
// document. The returned bytes always end in a single trailing newline
// (yaml.v3's Encoder convention) so concatenated git diffs do not have
// a "no newline at end of file" marker.
//
// Errors fall into three categories:
//   - existing is not parseable YAML (returned with the underlying
//     yaml.Decoder error wrapped),
//   - existing's root is not a mapping (catalog files must be a mapping
//     of section name → entries; a sequence or scalar is rejected),
//   - the section heading exists but its value is not a mapping
//     (e.g. someone wrote "resources: []" — also rejected, because the
//     loader would reject it too).
func mergeIntoDocument(existing []byte, resourceType string, section Section) ([]byte, error) {
	key, err := sectionKey(section)
	if err != nil {
		return nil, err
	}
	stubVal, err := buildStubNode(section)
	if err != nil {
		return nil, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(existing, &root); err != nil {
		return nil, fmt.Errorf("catalog scaffold: parse existing: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		// Empty/whitespace-only files are handled by the caller; any
		// other shape that produces a non-document root is unexpected.
		return nil, fmt.Errorf("catalog scaffold: existing file has no document root")
	}
	mapNode := root.Content[0]
	if mapNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("catalog scaffold: existing file root is not a mapping")
	}

	sectionNode := findMappingValue(mapNode, key)
	if sectionNode == nil {
		// Section heading is missing — append it as a new top-level key.
		mapNode.Content = append(mapNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: resourceType},
				stubVal,
			}},
		)
	} else {
		if sectionNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("catalog scaffold: section %q is not a mapping", key)
		}
		sectionNode.Content = append(sectionNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: resourceType},
			stubVal,
		)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("catalog scaffold: encode merged document: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("catalog scaffold: close encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// findMappingValue returns the value node for key in a MappingNode, or
// nil if the key is absent. yaml.MappingNode stores key/value pairs as
// flat content slots: Content[2*i] is the key, Content[2*i+1] is the
// value. Higher-level libraries hide this layout, but we are walking
// the tree directly to preserve comments and order, so the helper has
// to know.
func findMappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// buildStubNode produces the yaml.Node for the per-entry stub body
// (everything under the resourceType key) for the given section. It
// reuses GenerateStub to avoid duplicating the field shape between two
// code paths — if the schema gains a new field, GenerateStub is the one
// place to update.
func buildStubNode(section Section) (*yaml.Node, error) {
	stubBytes, err := GenerateStub("__placeholder__", section)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(stubBytes, &root); err != nil {
		return nil, fmt.Errorf("catalog scaffold: re-parse stub: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("catalog scaffold: stub has no document root")
	}
	mapNode := root.Content[0]
	if mapNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("catalog scaffold: stub root is not a mapping")
	}
	// stub structure: {section: {placeholder: <entry>}}
	sectionVal := findMappingValue(mapNode, mustSectionKey(section))
	if sectionVal == nil || sectionVal.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("catalog scaffold: stub section node missing")
	}
	entryVal := findMappingValue(sectionVal, "__placeholder__")
	if entryVal == nil {
		return nil, fmt.Errorf("catalog scaffold: stub entry node missing")
	}
	return entryVal, nil
}

// mustSectionKey is the panic-on-error helper variant of sectionKey.
// Used from buildStubNode where the section value has already been
// validated by the caller — a panic here would indicate a programming
// error inside this file, not a user-input issue.
func mustSectionKey(s Section) string {
	k, err := sectionKey(s)
	if err != nil {
		panic(err)
	}
	return k
}
