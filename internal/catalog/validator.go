package catalog

// Catalog schema validator. Companion to catalog.go (data model) and
// loader.go (decoding + merge).
//
// validate runs after all YAML files have been merged into a single
// Catalog so cross-entry rules (e.g. iam_bindings.parent_resource
// referencing a resources entry) can be checked uniformly regardless
// of which file declared which entry.
//
// Error format: every diagnostic begins with the entry's Position so a
// contributor can locate the issue without grep:
//
//   storage.yaml:42: iam_bindings/google_storage_bucket_iam_binding:
//     parent_resource "google_storage_bucket_does_not_exist" is not a
//     declared resource type
//
// Errors are wrapped with ErrCatalog so callers can use errors.Is to
// distinguish a schema failure from an underlying I/O error.

import (
	"fmt"
	"sort"
	"strings"
)

// validVerificationMethods is the canonical lookup of accepted
// VerificationMethod values. Keep in sync with the VerificationMethod*
// consts in catalog.go.
var validVerificationMethods = map[VerificationMethod]struct{}{
	VerificationMethodGcloud:    {},
	VerificationMethodREST:      {},
	VerificationMethodTerraform: {},
}

// validate runs strict schema checks on a fully-merged Catalog and
// returns the first error encountered, wrapped with ErrCatalog so
// callers can use errors.Is(err, ErrCatalog) to recognise validation
// failures.
//
// Iteration order is deterministic — entries are visited in
// lexicographic key order — so a deterministic-test asserting the
// "first error" reported is stable across runs.
//
// Validation rules per entry kind:
//
//	resources:
//	  - verification.method must be a recognised value
//	  - permissions.plan must be present and non-empty
//	  - each conditional must have a non-empty `when` map
//	  - permission strings must be non-empty after trimming
//
//	data_sources: same rules as resources.
//
//	iam_bindings:
//	  - parent_resource must be set and refer to a declared resource
//	  - verification.method must be recognised
//	  - permissions.plan must be present and non-empty
//
// The validator stops at the first error rather than aggregating; the
// loader is normally consumed by a CI step that fails fast and a single
// actionable diagnostic is more useful than a wall of stacked errors.
func validate(cat *Catalog) error {
	for _, typ := range sortedKeys(cat.Resources) {
		entry := cat.Resources[typ]
		if err := validateResourceEntry(entry); err != nil {
			return err
		}
	}
	for _, typ := range sortedKeys(cat.DataSources) {
		entry := cat.DataSources[typ]
		if err := validateDataSourceEntry(entry); err != nil {
			return err
		}
	}
	for _, typ := range sortedKeys(cat.IAMBindings) {
		entry := cat.IAMBindings[typ]
		if err := validateIAMBindingEntry(entry, cat); err != nil {
			return err
		}
	}
	return nil
}

func validateResourceEntry(e *ResourceEntry) error {
	loc := fmt.Sprintf("%s: resources/%s", e.Position, e.Type)
	if err := validateVerification(e.Verification, loc); err != nil {
		return err
	}
	if err := validatePermissionSet(e.Permissions, loc, true /*planRequired*/); err != nil {
		return err
	}
	for i, c := range e.Conditionals {
		condLoc := fmt.Sprintf("%s: resources/%s/conditionals[%d]", c.Position, e.Type, i)
		if err := validateConditional(c, condLoc); err != nil {
			return err
		}
	}
	return nil
}

func validateDataSourceEntry(e *DataSourceEntry) error {
	loc := fmt.Sprintf("%s: data_sources/%s", e.Position, e.Type)
	if err := validateVerification(e.Verification, loc); err != nil {
		return err
	}
	if err := validatePermissionSet(e.Permissions, loc, true /*planRequired*/); err != nil {
		return err
	}
	for i, c := range e.Conditionals {
		condLoc := fmt.Sprintf("%s: data_sources/%s/conditionals[%d]", c.Position, e.Type, i)
		if err := validateConditional(c, condLoc); err != nil {
			return err
		}
	}
	return nil
}

func validateIAMBindingEntry(e *IAMBindingEntry, cat *Catalog) error {
	loc := fmt.Sprintf("%s: iam_bindings/%s", e.Position, e.Type)
	if err := validateVerification(e.Verification, loc); err != nil {
		return err
	}
	if err := validatePermissionSet(e.Permissions, loc, true /*planRequired*/); err != nil {
		return err
	}
	if strings.TrimSpace(e.ParentResource) == "" {
		return fmt.Errorf("%w: %s: parent_resource is required", ErrCatalog, loc)
	}
	if _, ok := cat.Resources[e.ParentResource]; !ok {
		// Build a helpful "did you mean?" hint by listing the closest
		// known types alphabetically. Cheap to compute and worth the
		// few-line cost — most parent_resource typos are typos, and
		// without the hint a contributor has to grep the catalog.
		known := sortedKeys(cat.Resources)
		return fmt.Errorf(
			"%w: %s: parent_resource %q is not a declared resource type (known: %v)",
			ErrCatalog, loc, e.ParentResource, known,
		)
	}
	return nil
}

func validateVerification(v Verification, loc string) error {
	if v.Method == "" {
		return fmt.Errorf("%w: %s: verification.method is required", ErrCatalog, loc)
	}
	if _, ok := validVerificationMethods[v.Method]; !ok {
		known := make([]string, 0, len(validVerificationMethods))
		for m := range validVerificationMethods {
			known = append(known, string(m))
		}
		sort.Strings(known)
		return fmt.Errorf(
			"%w: %s: verification.method %q is not one of %v",
			ErrCatalog, loc, v.Method, known,
		)
	}
	return nil
}

// validatePermissionSet enforces that all permission identifiers are
// non-empty post-trim. When planRequired is true (the default for
// resource / data source / iam binding entries) Plan must contain at
// least one permission; passing planRequired=false is reserved for
// conditionals, where an empty Plan means "no extra plan permissions".
func validatePermissionSet(p PermissionSet, loc string, planRequired bool) error {
	if planRequired && len(p.Plan) == 0 {
		return fmt.Errorf("%w: %s: permissions.plan must contain at least one permission", ErrCatalog, loc)
	}
	for i, perm := range p.Plan {
		if strings.TrimSpace(perm) == "" {
			return fmt.Errorf("%w: %s: permissions.plan[%d] is empty", ErrCatalog, loc, i)
		}
	}
	for i, perm := range p.Apply {
		if strings.TrimSpace(perm) == "" {
			return fmt.Errorf("%w: %s: permissions.apply[%d] is empty", ErrCatalog, loc, i)
		}
	}
	return nil
}

// validateConditional enforces conditional-specific rules. The base
// rule is that `when` must contain at least one predicate; an empty
// `when` would always match and is better expressed as base
// permissions on the parent entry.
//
// `permissions` on a conditional is checked with planRequired=false:
// an additive conditional may legitimately add only Apply-time
// permissions (e.g. a flag that triggers an update call but not a
// read).
func validateConditional(c Conditional, loc string) error {
	if len(c.When) == 0 {
		return fmt.Errorf("%w: %s: when clause must have at least one predicate", ErrCatalog, loc)
	}
	for k := range c.When {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: %s: when clause has empty key", ErrCatalog, loc)
		}
	}
	if err := validatePermissionSet(c.Permissions, loc, false /*planRequired*/); err != nil {
		return err
	}
	if len(c.Permissions.Plan) == 0 && len(c.Permissions.Apply) == 0 {
		return fmt.Errorf("%w: %s: conditional must add at least one permission", ErrCatalog, loc)
	}
	return nil
}
