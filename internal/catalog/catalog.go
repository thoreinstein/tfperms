// Package catalog defines the YAML-backed permission catalog that maps
// Terraform resource / data / IAM-binding types onto the GCP IAM
// permissions required to plan and apply them.
//
// The catalog is the source of truth that the analyzer consults for every
// resource it observes in a parsed Terraform configuration. Each YAML file
// in the embedded `catalog/` directory is partitioned by GCP service
// family (storage.yaml, compute.yaml, ...). All files are merged at
// load time into a single in-memory lookup keyed by Terraform resource
// type.
//
// Layout responsibilities:
//   - This file (catalog.go) defines the data model: Catalog,
//     ResourceEntry, DataSourceEntry, IAMBindingEntry, PermissionSet,
//     Verification, Conditional, Position, and the VerificationMethod
//     enum.
//   - loader.go owns reading YAML files off an fs.FS, decoding them
//     through yaml.v3, and merging multiple files into a single
//     Catalog while preserving source-line information for diagnostics.
//   - validator.go owns strict schema validation: required fields, enum
//     constraints, and cross-entry references such as iam_bindings'
//     parent_resource pointing at a known resource type.
//
// Source-position tracking:
//   - Each entry carries a Position (file + line) populated by the loader
//     so validation diagnostics can quote the offending YAML location.
//     The Position is intentionally NOT part of the YAML schema — it is
//     derived from yaml.Node line numbers.
package catalog

import (
	"errors"
	"fmt"
)

// VerificationMethod enumerates the supported strategies for verifying
// that a resource described by the catalog exists in GCP. The enum is
// intentionally string-typed so YAML decodes naturally into the same
// representation used in the schema.
type VerificationMethod string

// Recognised VerificationMethod values. Anything else is rejected by the
// validator with a "unknown verification.method" error.
const (
	VerificationMethodGcloud    VerificationMethod = "gcloud"
	VerificationMethodREST      VerificationMethod = "rest"
	VerificationMethodTerraform VerificationMethod = "terraform"
)

// Position locates a YAML node in its source file. File is the relative
// path of the YAML file inside the embedded catalog (e.g. "storage.yaml"),
// not the absolute disk path. Line is 1-indexed and matches yaml.Node.Line.
//
// Position is zero-value-safe: a freshly constructed entry that has not
// been populated by the loader has File == "" and Line == 0, which the
// validator reports as "<unknown>:0" rather than crashing.
type Position struct {
	File string
	Line int
}

// String renders Position as "file:line" for inclusion in error messages.
// Unknown positions render as "<unknown>:0" so test assertions can match
// the literal even when the loader did not record a position.
func (p Position) String() string {
	if p.File == "" {
		return "<unknown>:0"
	}
	return fmt.Sprintf("%s:%d", p.File, p.Line)
}

// PermissionSet is the pair of permission lists consumed by the analyzer:
// Plan permissions are required during `terraform plan`, Apply permissions
// during `terraform apply`. The two are tracked separately so a consumer
// can report a least-privilege role for a read-only plan run distinct
// from a writing apply run.
//
// A nil/empty Apply slice is permitted (read-only data sources), but Plan
// must be present and non-empty for resource and data source entries —
// the validator enforces this.
type PermissionSet struct {
	Plan  []string `yaml:"plan"`
	Apply []string `yaml:"apply"`
}

// Verification describes how to confirm a resource declared in the
// catalog actually exists in GCP. Method is required; Command is an
// optional human-oriented hint the analyzer can surface to operators —
// the analyzer itself never executes it.
type Verification struct {
	Method  VerificationMethod `yaml:"method"`
	Command string             `yaml:"command,omitempty"`
}

// Conditional adjusts the base permission set when an attribute on the
// underlying Terraform block matches a particular value. Multiple
// Conditionals on the same entry are evaluated independently and ALL
// matching conditionals contribute their permissions on top of the base
// PermissionSet.
//
// When is a map of attribute name -> expected value. The analyzer
// compares the resolved cty.Value of the attribute against the value
// here using cty equality semantics. A nil/empty When is rejected by
// the validator — a conditional with no predicate would always fire
// and is better expressed as base permissions.
//
// Permissions are *additive* — the analyzer unions them with the base
// PermissionSet rather than replacing it. The validator does not require
// every PermissionSet field to be populated for a Conditional; an empty
// Plan or Apply means "no extra permissions on this side".
type Conditional struct {
	When        map[string]any `yaml:"when"`
	Permissions PermissionSet  `yaml:"permissions"`
	// Position is set by the loader from the conditional node's line.
	Position Position `yaml:"-"`
}

// ResourceEntry is the catalog entry for a Terraform `resource` block
// type (e.g. google_storage_bucket).
//
// Type is the Terraform resource type — populated by the loader from the
// map key under `resources:` so callers reading a ResourceEntry
// independently still know which type it describes.
type ResourceEntry struct {
	Type         string        `yaml:"-"`
	Verification Verification  `yaml:"verification"`
	Permissions  PermissionSet `yaml:"permissions"`
	Conditionals []Conditional `yaml:"conditionals,omitempty"`
	Position     Position      `yaml:"-"`
}

// DataSourceEntry is the catalog entry for a Terraform `data` block
// type. The structure mirrors ResourceEntry; data sources almost
// always have an empty Apply slice (data sources don't write) but the
// schema does not enforce that — a data source that triggers a
// side-effecting list call could legitimately need apply-time
// permissions.
type DataSourceEntry struct {
	Type         string        `yaml:"-"`
	Verification Verification  `yaml:"verification"`
	Permissions  PermissionSet `yaml:"permissions"`
	Conditionals []Conditional `yaml:"conditionals,omitempty"`
	Position     Position      `yaml:"-"`
}

// IAMBindingEntry is the catalog entry for an IAM binding/member/policy
// resource type (e.g. google_storage_bucket_iam_binding). ParentResource
// names the resource type the binding applies to and MUST refer to a
// resource type defined in the same merged catalog — the validator
// enforces the cross-reference.
//
// Verification on an IAM binding means "how do I verify the binding
// applied", not "does the parent exist"; that is the parent's
// responsibility.
type IAMBindingEntry struct {
	Type           string        `yaml:"-"`
	ParentResource string        `yaml:"parent_resource"`
	Verification   Verification  `yaml:"verification"`
	Permissions    PermissionSet `yaml:"permissions"`
	Position       Position      `yaml:"-"`
}

// Catalog is the merged in-memory view of every catalog file. Maps are
// keyed by Terraform type — the same string a user would write in HCL.
// All maps are non-nil after a successful Load even when a particular
// section is empty in the YAML, so callers can range over them without
// nil-checking.
type Catalog struct {
	Resources   map[string]*ResourceEntry
	DataSources map[string]*DataSourceEntry
	IAMBindings map[string]*IAMBindingEntry
}

// newCatalog returns a Catalog with all three maps initialised to non-nil
// empties. Used by the loader and by tests that build catalogs in memory.
func newCatalog() *Catalog {
	return &Catalog{
		Resources:   make(map[string]*ResourceEntry),
		DataSources: make(map[string]*DataSourceEntry),
		IAMBindings: make(map[string]*IAMBindingEntry),
	}
}

// ErrCatalog is the sentinel error category returned by Load / LoadFS for
// schema and validation failures. Callers can use errors.Is to distinguish
// a catalog schema error from an underlying I/O error. Wrapping is
// performed by the loader and validator using fmt.Errorf("...: %w", ...)
// so the chain remains intact.
var ErrCatalog = errors.New("catalog")
