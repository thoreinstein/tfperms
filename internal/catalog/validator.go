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
//	storage.yaml:42: iam_bindings/google_storage_bucket_iam_binding:
//	  parent_resource "google_storage_bucket_does_not_exist" is not a
//	  declared resource type
//
// Errors are wrapped with ErrCatalog so callers can use errors.Is to
// distinguish a schema failure from an underlying I/O error.
//
// The full required schema is documented on catalog.go's package
// doc-comment. The summary the validator enforces:
//
//   - verification.method ∈ {empirical, docs+source}
//   - verification.source_urls is non-empty
//   - verification.verified_at parses as YYYY-MM-DD
//   - verification.verified_provider_version is non-empty
//   - tested_against_provider is non-empty
//   - permissions.plan is non-empty (resources / data sources / iam bindings)
//   - all permission strings are non-empty after trimming
//   - data_sources permissions only carry plan (the type system enforces
//     this — DataSourcePermissions has only a Plan field)
//   - iam_bindings.parent_resource refers to a declared resource type

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// validVerificationMethods is the canonical lookup of accepted
// VerificationMethod values. Keep in sync with the VerificationMethod*
// consts in catalog.go.
var validVerificationMethods = map[VerificationMethod]struct{}{
	VerificationMethodEmpirical:  {},
	VerificationMethodDocsSource: {},
}

// verifiedAtLayout is the date format expected for Verification.VerifiedAt.
// We pin to a single representation rather than time.RFC3339 because
// catalog entries record verification on a date granularity (the
// contributor ran terraform apply on day X) and a wall-clock timestamp
// would imply a precision the entry does not actually have.
const verifiedAtLayout = "2006-01-02"

// validate runs strict schema checks on a fully-merged Catalog and
// returns the first error encountered, wrapped with ErrCatalog so
// callers can use errors.Is(err, ErrCatalog) to recognise validation
// failures.
//
// Iteration order is deterministic — entries are visited in
// lexicographic key order — so a deterministic-test asserting the
// "first error" reported is stable across runs.
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
	if err := validateTestedAgainstProvider(e.TestedAgainstProvider, loc); err != nil {
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
	if err := validateTestedAgainstProvider(e.TestedAgainstProvider, loc); err != nil {
		return err
	}
	if err := validateDataSourcePermissions(e.Permissions, loc); err != nil {
		return err
	}
	for i, c := range e.Conditionals {
		condLoc := fmt.Sprintf("%s: data_sources/%s/conditionals[%d]", c.Position, e.Type, i)
		if err := validateDataSourceConditional(c, condLoc); err != nil {
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
	if err := validateTestedAgainstProvider(e.TestedAgainstProvider, loc); err != nil {
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
	if len(v.SourceURLs) == 0 {
		return fmt.Errorf("%w: %s: verification.source_urls must contain at least one citation", ErrCatalog, loc)
	}
	for i, u := range v.SourceURLs {
		if strings.TrimSpace(u) == "" {
			return fmt.Errorf("%w: %s: verification.source_urls[%d] is empty", ErrCatalog, loc, i)
		}
	}
	if strings.TrimSpace(v.VerifiedAt) == "" {
		return fmt.Errorf("%w: %s: verification.verified_at is required (YYYY-MM-DD)", ErrCatalog, loc)
	}
	if _, err := time.Parse(verifiedAtLayout, v.VerifiedAt); err != nil {
		return fmt.Errorf(
			"%w: %s: verification.verified_at %q is not a valid YYYY-MM-DD date: %w",
			ErrCatalog, loc, v.VerifiedAt, err,
		)
	}
	if strings.TrimSpace(v.VerifiedProviderVersion) == "" {
		return fmt.Errorf("%w: %s: verification.verified_provider_version is required", ErrCatalog, loc)
	}
	return nil
}

// validateTestedAgainstProvider enforces that an entry declares the
// provider version range it was verified against. The format is
// intentionally not parsed — the catalog accepts any non-empty
// constraint expression (e.g. ">=5.0.0,<7.0.0", "~> 6.0", "6.x") so the
// catalog can match whatever convention the surrounding tooling is
// using. The diagnostics command will surface the raw string to the
// user; pinning a single grammar here would force a contributor whose
// project uses a different idiom to translate.
func validateTestedAgainstProvider(v string, loc string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%w: %s: tested_against_provider is required", ErrCatalog, loc)
	}
	return nil
}

// validatePermissionSet enforces that all permission identifiers are
// non-empty post-trim. When planRequired is true (the default for
// resource / iam binding entries) Plan must contain at least one
// permission; passing planRequired=false is reserved for conditionals,
// where an empty Plan means "no extra plan permissions".
//
// All four stages (Plan / Create / Update / Delete) are checked for
// blank entries so a typo or stray empty list element is rejected up
// front rather than silently propagating into the resolver's union.
func validatePermissionSet(p PermissionSet, loc string, planRequired bool) error {
	if planRequired && len(p.Plan) == 0 {
		return fmt.Errorf("%w: %s: permissions.plan must contain at least one permission", ErrCatalog, loc)
	}
	stages := []struct {
		name string
		list []string
	}{
		{"plan", p.Plan},
		{"create", p.Create},
		{"update", p.Update},
		{"delete", p.Delete},
	}
	for _, s := range stages {
		for i, perm := range s.list {
			if strings.TrimSpace(perm) == "" {
				return fmt.Errorf("%w: %s: permissions.%s[%d] is empty", ErrCatalog, loc, s.name, i)
			}
		}
	}
	return nil
}

// validateDataSourcePermissions enforces the read-only invariant on data
// sources: Plan is required and non-empty. The struct shape itself
// already prevents create / update / delete from being expressed at all
// — strict YAML decoding rejects those fields on a data source — so the
// validator only checks Plan.
func validateDataSourcePermissions(p DataSourcePermissions, loc string) error {
	if len(p.Plan) == 0 {
		return fmt.Errorf("%w: %s: permissions.plan must contain at least one permission", ErrCatalog, loc)
	}
	for i, perm := range p.Plan {
		if strings.TrimSpace(perm) == "" {
			return fmt.Errorf("%w: %s: permissions.plan[%d] is empty", ErrCatalog, loc, i)
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
// an additive conditional may legitimately add only update-time
// permissions (e.g. a flag that triggers an update call but not a read).
// At least one stage must contribute a permission though — a
// conditional that adds nothing is a footgun.
func validateConditional(c Conditional, loc string) error {
	if err := validateWhen(c.When, loc); err != nil {
		return err
	}
	if err := validatePermissionSet(c.Permissions, loc, false /*planRequired*/); err != nil {
		return err
	}
	if len(c.Permissions.Plan) == 0 &&
		len(c.Permissions.Create) == 0 &&
		len(c.Permissions.Update) == 0 &&
		len(c.Permissions.Delete) == 0 {
		return fmt.Errorf("%w: %s: conditional must add at least one permission", ErrCatalog, loc)
	}
	return nil
}

// validateDataSourceConditional applies the same rules as
// validateConditional but on the read-only DataSourcePermissions shape.
func validateDataSourceConditional(c DataSourceConditional, loc string) error {
	if err := validateWhen(c.When, loc); err != nil {
		return err
	}
	for i, perm := range c.Permissions.Plan {
		if strings.TrimSpace(perm) == "" {
			return fmt.Errorf("%w: %s: permissions.plan[%d] is empty", ErrCatalog, loc, i)
		}
	}
	if len(c.Permissions.Plan) == 0 {
		return fmt.Errorf("%w: %s: conditional must add at least one permission", ErrCatalog, loc)
	}
	return nil
}

// validateWhen is the shared predicate check used by both conditional
// types: a non-empty map with no blank keys.
func validateWhen(when map[string]any, loc string) error {
	if len(when) == 0 {
		return fmt.Errorf("%w: %s: when clause must have at least one predicate", ErrCatalog, loc)
	}
	for k := range when {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: %s: when clause has empty key", ErrCatalog, loc)
		}
	}
	return nil
}
