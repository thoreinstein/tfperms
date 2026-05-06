package catalog

// Catalog loader. Companion to catalog.go (data model) and validator.go
// (schema enforcement).
//
// The loader's job is to turn a directory of YAML files into a single
// merged Catalog. It is split out from catalog.go so the data model
// stays focused on shape, and from validator.go so the validator can be
// invoked on a Catalog assembled from arbitrary sources (tests, future
// dynamic sources) without re-reading any files.
//
// Two-phase decode:
//   - Phase 1: each YAML file unmarshals into a rawFile whose entry maps
//     are map[string]yaml.Node. A yaml.Node retains the line and column
//     of the underlying token, which is what we need for diagnostic
//     Positions.
//   - Phase 2: each yaml.Node is decoded into the typed entry struct
//     (ResourceEntry, DataSourceEntry, IAMBindingEntry). The decode
//     populates content fields; the loader manually copies Type and
//     Position back in because both are yaml:"-".
//
// Going through yaml.Node rather than decoding straight into the typed
// structs keeps line numbers attached to entries. yaml.v3 only attaches
// line/column information to yaml.Node values; once the data lands in a
// plain Go struct it is gone. annotateConditionals uses the same trick
// at the per-conditional level.
//
// Strict decoding: both phases reject unknown YAML keys. A misspelled
// top-level section (e.g. "resource:" instead of "resources:") or a
// misspelled per-entry field (e.g. "verifications:" instead of
// "verification:") is a hard load error rather than a silent drop —
// "strict schema validation" requires that contributor typos surface
// at CI rather than letting an entry quietly disappear from the merged
// catalog. yaml.v3 supports strict mode on a Decoder via KnownFields(true);
// (*yaml.Node).Decode does NOT honor it, so for per-entry decoding we
// round-trip through yaml.Marshal + yaml.NewDecoder. The round trip is
// negligible in cost and reuses yaml.v3's own struct-tag enforcement
// rather than re-implementing key validation by hand.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	catalogdata "github.com/thoreinstein/tfperms/catalog"
	"gopkg.in/yaml.v3"
)

// rawFile is the on-disk shape of a single catalog YAML file. It is
// intentionally separate from Catalog: the on-disk shape is per-file
// (one storage.yaml, one compute.yaml, ...) while Catalog is the merged
// runtime view. Decoding into rawFile first lets us preserve yaml.Node
// objects so the loader can attach Positions before flattening.
type rawFile struct {
	Resources   map[string]yaml.Node `yaml:"resources"`
	DataSources map[string]yaml.Node `yaml:"data_sources"`
	IAMBindings map[string]yaml.Node `yaml:"iam_bindings"`
}

// Load reads every embedded catalog file, merges them into a single
// Catalog, and runs strict schema validation. A non-nil error means the
// catalog is unusable — either a YAML file was malformed, two files
// declared the same Terraform type, or a required field was missing.
//
// On success the returned *Catalog is fully populated and validated;
// callers can read it concurrently. Load does not cache — each call
// re-reads and re-validates. In production the analyzer should call
// Load once during startup.
func Load() (*Catalog, error) {
	return LoadFS(catalogdata.FS, ".")
}

// LoadFS is the testable variant of Load. It accepts an fs.FS rooted at
// dir and reads every *.yaml file directly under dir (non-recursively).
// Tests use this entry point to inject malformed inputs without
// disturbing the embedded catalog.
//
// The .yml extension is intentionally rejected. catalog/embed.go's
// //go:embed pattern only matches *.yaml, so a contributor adding a
// `something.yml` file would see it work locally via the disk loader
// but vanish from the embedded binary. Restricting the loader to a
// single extension prevents that silent divergence.
//
// dir is interpreted relative to fsys; pass "." to read the root of fsys.
//
// Filenames are processed in lexicographic order so that any
// "duplicate type" error message reports the same "first" file across
// runs — this matters for CI log readability and for reproducible test
// failures.
func LoadFS(fsys fs.FS, dir string) (*Catalog, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		// Double %w: keep ErrCatalog discoverable via errors.Is and
		// preserve the underlying fs error (e.g. fs.ErrNotExist) in the
		// chain so callers can use errors.Is/errors.As on it. The
		// package doc on ErrCatalog promises this contract.
		return nil, fmt.Errorf("%w: read dir %q: %w", ErrCatalog, dir, err)
	}

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)

	cat := newCatalog()

	// firstSeen tracks where a Terraform type was first defined so the
	// duplicate-detection error can quote both locations. Keys are
	// "resources/<type>", "data_sources/<type>", "iam_bindings/<type>"
	// to keep the three sections in distinct namespaces — a resource
	// and an IAM binding may legitimately share a name (e.g. the IAM
	// binding `google_storage_bucket_iam_binding` references the
	// resource `google_storage_bucket`).
	firstSeen := make(map[string]Position)

	for _, name := range files {
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %w", ErrCatalog, name, err)
		}
		if err := mergeFile(cat, firstSeen, name, data); err != nil {
			return nil, err
		}
	}

	if err := validate(cat); err != nil {
		return nil, err
	}
	return cat, nil
}

// mergeFile decodes a single catalog YAML file and merges its entries
// into cat. firstSeen is updated for every entry inserted so subsequent
// files that redefine a type can report the conflicting source location.
//
// Decoding errors and duplicate-type errors are wrapped with ErrCatalog
// so callers using errors.Is can distinguish them from generic I/O
// failures returned by LoadFS.
func mergeFile(cat *Catalog, firstSeen map[string]Position, file string, data []byte) error {
	var raw rawFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown top-level keys (e.g. "resource:" instead of "resources:")
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("%w: parse %q: %w", ErrCatalog, file, err)
	}

	// resources
	for _, typ := range sortedKeys(raw.Resources) {
		node := raw.Resources[typ]
		key := "resources/" + typ
		pos := Position{File: file, Line: node.Line}
		if prev, dup := firstSeen[key]; dup {
			return fmt.Errorf(
				"%w: duplicate resource type %q: defined at %s and %s",
				ErrCatalog, typ, prev, pos,
			)
		}
		firstSeen[key] = pos

		entry := &ResourceEntry{}
		if err := strictDecodeNode(&node, entry); err != nil {
			return fmt.Errorf("%w: decode resources/%s: %w", ErrCatalog, typ, rewriteStrictDecodeErr(err, pos))
		}
		entry.Type = typ
		entry.Position = pos
		condLines := conditionalLines(&node)
		for i := range entry.Conditionals {
			if i < len(condLines) {
				entry.Conditionals[i].Position = Position{File: file, Line: condLines[i]}
			}
		}
		cat.Resources[typ] = entry
	}

	// data sources
	for _, typ := range sortedKeys(raw.DataSources) {
		node := raw.DataSources[typ]
		key := "data_sources/" + typ
		pos := Position{File: file, Line: node.Line}
		if prev, dup := firstSeen[key]; dup {
			return fmt.Errorf(
				"%w: duplicate data source type %q: defined at %s and %s",
				ErrCatalog, typ, prev, pos,
			)
		}
		firstSeen[key] = pos

		entry := &DataSourceEntry{}
		if err := strictDecodeNode(&node, entry); err != nil {
			return fmt.Errorf("%w: decode data_sources/%s: %w", ErrCatalog, typ, rewriteStrictDecodeErr(err, pos))
		}
		entry.Type = typ
		entry.Position = pos
		condLines := conditionalLines(&node)
		for i := range entry.Conditionals {
			if i < len(condLines) {
				entry.Conditionals[i].Position = Position{File: file, Line: condLines[i]}
			}
		}
		cat.DataSources[typ] = entry
	}

	// iam bindings
	for _, typ := range sortedKeys(raw.IAMBindings) {
		node := raw.IAMBindings[typ]
		key := "iam_bindings/" + typ
		pos := Position{File: file, Line: node.Line}
		if prev, dup := firstSeen[key]; dup {
			return fmt.Errorf(
				"%w: duplicate iam_binding type %q: defined at %s and %s",
				ErrCatalog, typ, prev, pos,
			)
		}
		firstSeen[key] = pos

		entry := &IAMBindingEntry{}
		if err := strictDecodeNode(&node, entry); err != nil {
			return fmt.Errorf("%w: decode iam_bindings/%s: %w", ErrCatalog, typ, rewriteStrictDecodeErr(err, pos))
		}
		entry.Type = typ
		entry.Position = pos
		condLines := conditionalLines(&node)
		for i := range entry.Conditionals {
			if i < len(condLines) {
				entry.Conditionals[i].Position = Position{File: file, Line: condLines[i]}
			}
		}
		cat.IAMBindings[typ] = entry
	}

	return nil
}

// strictDecodeNode decodes a yaml.Node into out with strict mode enabled
// (unknown struct fields produce an error). yaml.v3's (*Node).Decode
// uses an internal decoder that does NOT honor KnownFields, so this
// helper round-trips through yaml.Marshal + yaml.NewDecoder to engage
// strict-mode enforcement.
//
// The round trip preserves nesting, so unknown keys at any depth — for
// example a typo'd field inside a Conditional — are reported. The cost
// is one Marshal and one Decode per entry; for catalogs of tens to a
// few hundred entries this is negligible at startup.
//
// Error wrapping is left to the caller because the call sites already
// know which entry path the decode is for and run the result through
// rewriteStrictDecodeErr to anchor diagnostics at the catalog file and
// line — yaml.v3's TypeError on its own only reports a fragment-
// relative "line 1" which is misleading without the rewrite.
func strictDecodeNode(node *yaml.Node, out any) error {
	data, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// fragmentLineRE matches yaml.v3's "line N: " prefix on individual
// TypeError messages. The line number is relative to the marshaled
// fragment strictDecodeNode reparses, not to the original catalog file,
// so it is misleading on its own and must be replaced.
var fragmentLineRE = regexp.MustCompile(`^line \d+: `)

// rewriteStrictDecodeErr anchors a strict-decode error from
// strictDecodeNode at the catalog file and entry-level line captured
// during the rawFile pass. yaml.v3's KnownFields(true) reports unknown
// fields with a fragment-relative line ("line 1: field ... not found"),
// which is unhelpful — the offending field can sit on any line of the
// real file. Replacing the prefix with the entry's Position keeps the
// field-name detail (e.g. `field "verifcation" not found`) but points
// the contributor at a real source location.
//
// The yaml.TypeError stays in the chain so callers using errors.As keep
// programmatic access to the structured error; only the human-readable
// string is rewritten.
func rewriteStrictDecodeErr(err error, pos Position) error {
	if err == nil {
		return nil
	}
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		rewritten := &yaml.TypeError{Errors: make([]string, len(typeErr.Errors))}
		for i, msg := range typeErr.Errors {
			rewritten.Errors[i] = fmt.Sprintf("%s: %s", pos, fragmentLineRE.ReplaceAllString(msg, ""))
		}
		return rewritten
	}
	return fmt.Errorf("%s: %w", pos, err)
}

// sortedKeys returns m's keys in lexicographic order. yaml.Node values
// from a yaml.Unmarshal arrive via a Go map, whose iteration order is
// randomised. Sorting before walking keeps "first duplicate wins" stable
// across runs and keeps annotateConditionals's index alignment
// deterministic — both matter for reproducible diagnostics.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// conditionalLines walks the entry's parent yaml.Node looking for
// `conditionals:` and returns the line number of each list item, in the
// same order as yaml.v3 produced the decoded slice. Callers use the
// returned slice to set Position.Line on each Conditional /
// DataSourceConditional so validation errors can quote the line of the
// offending conditional rather than the line of the surrounding entry.
//
// Splitting position extraction from position assignment lets callers
// share this helper across the two conditional types (Conditional on
// resources / iam bindings, DataSourceConditional on data sources)
// without a generic constraint or interface — Go's type system would
// otherwise force a wrapper type, which adds noise without clarity for
// such a small helper.
//
// The walk is intentionally tolerant: if the conditionals subtree is
// missing or shaped unexpectedly the helper returns nil rather than
// failing the load. The validator already has its own checks; a missing
// line number degrades the error message but does not produce
// silently-wrong behaviour.
//
// Index alignment between the returned slice and the decoded conditionals
// slice depends on yaml.Node.Decode producing the slice in the same order
// as the document. yaml.v3 documents this guarantee for sequences.
func conditionalLines(parent *yaml.Node) []int {
	if parent == nil {
		return nil
	}
	mapNode := parent
	if mapNode.Kind == yaml.DocumentNode && len(mapNode.Content) > 0 {
		mapNode = mapNode.Content[0]
	}
	if mapNode.Kind != yaml.MappingNode {
		return nil
	}
	var seq *yaml.Node
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		key := mapNode.Content[i]
		if key.Value == "conditionals" {
			seq = mapNode.Content[i+1]
			break
		}
	}
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	lines := make([]int, len(seq.Content))
	for i, item := range seq.Content {
		lines[i] = item.Line
	}
	return lines
}
