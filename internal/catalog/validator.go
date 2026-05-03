package catalog

// Catalog schema validator. Phase 2 stub — real checks land in Phase 3.
//
// Even at the stub stage, validate is wired through the loader so that
// the loader-vs-validator boundary is fixed before validation logic is
// added. Phase 3 replaces the body of this function with the real
// checks (required fields, enum constraints, iam_bindings parent
// reference) without touching loader.go.

// validate runs strict schema checks on a fully-merged Catalog and
// returns the first error encountered, wrapped with ErrCatalog so callers
// can use errors.Is(err, ErrCatalog) to recognise validation failures.
//
// In Phase 2 this is a no-op: the loader is testable end-to-end without
// validator behaviour and the schema-validation tests are added with the
// real implementation in Phase 3.
func validate(_ *Catalog) error {
	return nil
}
