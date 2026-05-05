package catalog

// stats.go owns the diagnostic aggregation that powers the
// `tfperms catalog stats` CLI command. Companion to catalog.go (data
// model), loader.go (decode + merge), and validator.go (schema gate).
//
// Layout responsibilities:
//   - This file defines CatalogStats and its sub-structures (the value
//     types every stats consumer reads), plus ComputeStats (the pure
//     aggregation entry point) and the small helpers that detect TODO
//     sentinels and compare best-effort version constraints.
//   - The cmd/tfperms layer is responsible for rendering CatalogStats
//     into a human-readable report. Keeping the two concerns split lets
//     unit tests assert counts and detection rules without parsing
//     formatted text, and lets future consumers (a JSON dump, a Web UI,
//     ...) reuse ComputeStats unchanged.
//
// Determinism:
//
// Every slice on CatalogStats is produced in a stable, fixture-friendly
// order. ComputeStats sorts by service name, by section + Terraform
// type, by verification date + Position — never by Go map iteration —
// so a golden-file test on the rendered output remains stable run to
// run. The sort keys are documented next to each builder helper.
//
// Drift detection is deliberately best-effort: tfperms does not depend
// on hashicorp/go-version (see validator.go's
// testedAgainstProviderConstraintPattern doc-comment), so this file
// implements a small numeric-component comparator that handles the
// operators the catalog actually uses (`>=`, `<=`, `>`, `<`, `=`, `!=`).
// Pessimistic constraints (`~>`) are partially analysed: the lower bound
// is enforced and the upper bound is intentionally skipped rather than
// guessed at — we would rather under-report drift than fabricate it.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultReferenceVersion is the provider version `tfperms catalog
// stats` treats as "current" when computing drift. It is a single
// pinned point — not a range — because drift detection compares each
// entry's tested_against_provider clauses to one concrete version. The
// constant is exported so the cmd layer (and tests) can document the
// value back to the user without re-deriving it.
//
// When the Google provider releases a new minor / major and the team
// wants drift reports to track it, bump this constant in the same diff
// that updates docs/expectations. Leaving the value implicit (computed
// from a release feed at runtime) would defeat the deterministic-output
// contract the stats command relies on for golden tests.
const DefaultReferenceVersion = "6.15.0"

// CatalogStats is the aggregated diagnostic snapshot ComputeStats
// returns. Every field is exported so the renderer can format it
// directly; consumers that only need a subset can inspect the relevant
// fields without going through the renderer.
type CatalogStats struct {
	// TotalResources / TotalDataSources / TotalIAMBindings are the raw
	// per-section counts. They are derived from the merged catalog, so
	// duplicate types (which Load rejects) cannot inflate them.
	TotalResources   int
	TotalDataSources int
	TotalIAMBindings int

	// Services lists per-service coverage in lexicographic order of
	// Service name. The slice is empty (length 0, never nil) when the
	// catalog has no entries, so callers can range over it without a
	// nil check.
	Services []ServiceStats

	// OldestVerified holds up to five entries with the oldest
	// VerifiedAt date, in ascending date order. Ties break on
	// (Section, Type, Position) so the order is stable.
	OldestVerified []AgingEntry

	// MissingProvenance lists every entry whose provenance fields
	// still carry a "TODO" sentinel (the marker emitted by
	// `tfperms catalog scaffold`). Sorted by (Section, Type, Field).
	MissingProvenance []ProvenanceIssue

	// Drifting lists entries whose tested_against_provider clauses
	// exclude ReferenceVersion. Sorted by (Section, Type).
	Drifting []DriftEntry

	// ReferenceVersion is the provider version drift was computed
	// against. Surfaced on CatalogStats so the renderer (or a JSON
	// consumer) can echo it back without reaching for the constant.
	ReferenceVersion string
}

// ServiceStats summarises one service file's entries. Service is the
// basename of the YAML file with the ".yaml" extension stripped — the
// same convention CONTRIBUTING.md uses for service grouping. Empirical
// + DocsSource sum to Total; an Unknown column is intentionally absent
// because the validator rejects any other VerificationMethod value.
type ServiceStats struct {
	Service    string
	Total      int
	Empirical  int
	DocsSource int
}

// AgingEntry identifies one catalog entry by section, Terraform type,
// and source position, plus the verification date that places it in
// the oldest-N ranking. Section is one of "resources", "data_sources",
// "iam_bindings".
type AgingEntry struct {
	Section    string
	Type       string
	VerifiedAt string
	Position   Position
}

// ProvenanceIssue points at a single field on a single entry that
// still carries a "TODO" sentinel. Field is a human-readable path
// such as "verification.source_urls[0]" so the renderer can echo it
// without inventing a separate description.
type ProvenanceIssue struct {
	Section  string
	Type     string
	Field    string
	Position Position
}

// DriftEntry identifies an entry whose tested_against_provider
// constraint excludes the reference version. The constraint string is
// included verbatim so the renderer can show the contributor what
// they declared without re-walking the catalog.
type DriftEntry struct {
	Section               string
	Type                  string
	TestedAgainstProvider string
	Position              Position
}

// ComputeStats aggregates the diagnostic snapshot for cat. A nil
// catalog is treated as an empty one: the function returns a
// zero-valued CatalogStats with non-nil empty slices and the supplied
// referenceVersion.
//
// referenceVersion controls drift detection. Callers that don't have
// strong opinions should pass DefaultReferenceVersion. An empty string
// disables drift detection — the Drifting slice is always empty in
// that case — which the cmd layer uses for fixtures that should not
// trigger drift reports regardless of constraint shape.
//
// ComputeStats is pure: no IO, no logging, no time.Now lookups. Every
// metric is derived from cat alone, which keeps unit tests
// deterministic and lets a future caller compute stats off a catalog
// loaded from somewhere other than the embedded FS without surprises.
func ComputeStats(cat *Catalog, referenceVersion string) CatalogStats {
	stats := CatalogStats{
		Services:          []ServiceStats{},
		OldestVerified:    []AgingEntry{},
		MissingProvenance: []ProvenanceIssue{},
		Drifting:          []DriftEntry{},
		ReferenceVersion:  referenceVersion,
	}
	if cat == nil {
		return stats
	}

	stats.TotalResources = len(cat.Resources)
	stats.TotalDataSources = len(cat.DataSources)
	stats.TotalIAMBindings = len(cat.IAMBindings)

	stats.Services = computeServiceStats(cat)
	stats.OldestVerified = computeOldestVerified(cat, oldestVerifiedLimit)
	stats.MissingProvenance = computeMissingProvenance(cat)
	if referenceVersion != "" {
		stats.Drifting = computeDrifting(cat, referenceVersion)
	}
	return stats
}

// oldestVerifiedLimit is the cap on CatalogStats.OldestVerified. Five
// is the same number documented in the Epic 4 plan; expressing it as
// a named constant keeps the cap discoverable from a single edit.
const oldestVerifiedLimit = 5

// computeServiceStats walks every entry across the three sections and
// groups them by service (Position.File minus ".yaml"). The returned
// slice is sorted by Service name so the renderer's golden output is
// stable.
//
// An entry whose Position.File is empty (a hand-rolled fixture that
// did not pass through the loader) is grouped under "unknown"; in
// production this branch is unreachable because the loader stamps a
// Position on every entry it merges.
func computeServiceStats(cat *Catalog) []ServiceStats {
	type bucket struct {
		total      int
		empirical  int
		docsSource int
	}
	buckets := make(map[string]*bucket)

	visit := func(file string, method VerificationMethod) {
		service := serviceFromFile(file)
		b, ok := buckets[service]
		if !ok {
			b = &bucket{}
			buckets[service] = b
		}
		b.total++
		switch method {
		case VerificationMethodEmpirical:
			b.empirical++
		case VerificationMethodDocsSource:
			b.docsSource++
		}
	}

	for _, e := range cat.Resources {
		visit(e.Position.File, e.Verification.Method)
	}
	for _, e := range cat.DataSources {
		visit(e.Position.File, e.Verification.Method)
	}
	for _, e := range cat.IAMBindings {
		visit(e.Position.File, e.Verification.Method)
	}

	out := make([]ServiceStats, 0, len(buckets))
	for service, b := range buckets {
		out = append(out, ServiceStats{
			Service:    service,
			Total:      b.total,
			Empirical:  b.empirical,
			DocsSource: b.docsSource,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Service < out[j].Service
	})
	return out
}

// serviceFromFile derives a service label from a Position.File value.
// The convention is "<service>.yaml" (storage.yaml → storage); files
// without the suffix or with no name fall back to "unknown" so the
// renderer never produces a blank service column.
func serviceFromFile(file string) string {
	if file == "" {
		return "unknown"
	}
	if strings.HasSuffix(file, ".yaml") {
		return strings.TrimSuffix(file, ".yaml")
	}
	return file
}

// computeOldestVerified returns up to limit entries with the oldest
// verified_at date, ascending. Ties break on (Section, Type, Position)
// to keep the order stable across runs — verified_at is per-day and
// many entries share a date in practice, so a deterministic tiebreak
// is non-optional for golden tests.
//
// Entries whose verified_at fails to parse are skipped entirely; the
// validator already rejects unparseable dates at load time, so this
// branch is defensive — a hand-rolled fixture that bypasses the
// validator would otherwise sort to "year zero" and dominate the
// output. Parsing is delegated to verifiedAtLayout (mirroring the
// validator's contract) so the two stay in lockstep: a date that
// passes Load() is guaranteed to sort here, and a hand-rolled fixture
// with a malformed date is silently dropped from this section rather
// than promoted to the front of the list.
func computeOldestVerified(cat *Catalog, limit int) []AgingEntry {
	all := make([]AgingEntry, 0, len(cat.Resources)+len(cat.DataSources)+len(cat.IAMBindings))
	add := func(section string, typ string, v Verification, pos Position) {
		// Skip entries whose verified_at does not match the layout
		// the validator pinned. A blank or malformed string would
		// otherwise sort to the head of the slice and dominate the
		// "oldest" report — exactly the false positive the doc-comment
		// promises to suppress.
		if _, err := time.Parse(verifiedAtLayout, v.VerifiedAt); err != nil {
			return
		}
		all = append(all, AgingEntry{
			Section:    section,
			Type:       typ,
			VerifiedAt: v.VerifiedAt,
			Position:   pos,
		})
	}
	for _, e := range cat.Resources {
		add("resources", e.Type, e.Verification, e.Position)
	}
	for _, e := range cat.DataSources {
		add("data_sources", e.Type, e.Verification, e.Position)
	}
	for _, e := range cat.IAMBindings {
		add("iam_bindings", e.Type, e.Verification, e.Position)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].VerifiedAt != all[j].VerifiedAt {
			return all[i].VerifiedAt < all[j].VerifiedAt
		}
		if all[i].Section != all[j].Section {
			return all[i].Section < all[j].Section
		}
		if all[i].Type != all[j].Type {
			return all[i].Type < all[j].Type
		}
		if all[i].Position.File != all[j].Position.File {
			return all[i].Position.File < all[j].Position.File
		}
		return all[i].Position.Line < all[j].Position.Line
	})

	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// todoSentinel is the case-sensitive marker emitted by
// `tfperms catalog scaffold` and the only marker computeMissingProvenance
// reports. Case matters: the validator rejects empty fields and the
// scaffold writes the literal "TODO", so a stray "todo" or "Todo" is
// either a bespoke comment from the contributor or an unrelated string
// — flagging those would produce false positives.
const todoSentinel = "TODO"

// computeMissingProvenance walks every entry and surfaces fields that
// still carry the "TODO" sentinel. The check is intentionally strict
// (exact-string equality, not substring contains) so a verified URL
// like "https://example.com/TODO-resolution" does not produce a false
// positive.
//
// A single entry can contribute multiple ProvenanceIssue rows — one
// per offending field. The renderer relies on this so a contributor
// who left several blanks sees them all rather than chasing them one
// at a time.
func computeMissingProvenance(cat *Catalog) []ProvenanceIssue {
	out := []ProvenanceIssue{}

	checkVerification := func(section, typ string, pos Position, v Verification) {
		if string(v.Method) == todoSentinel {
			out = append(out, ProvenanceIssue{Section: section, Type: typ, Field: "verification.method", Position: pos})
		}
		for i, u := range v.SourceURLs {
			if u == todoSentinel {
				out = append(out, ProvenanceIssue{
					Section: section, Type: typ,
					Field:    fmt.Sprintf("verification.source_urls[%d]", i),
					Position: pos,
				})
			}
		}
		if v.VerifiedAt == todoSentinel {
			out = append(out, ProvenanceIssue{Section: section, Type: typ, Field: "verification.verified_at", Position: pos})
		}
		if v.VerifiedProviderVersion == todoSentinel {
			out = append(out, ProvenanceIssue{Section: section, Type: typ, Field: "verification.verified_provider_version", Position: pos})
		}
	}

	checkPermissions := func(section, typ string, pos Position, p PermissionSet) {
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
				if perm == todoSentinel {
					out = append(out, ProvenanceIssue{
						Section: section, Type: typ,
						Field:    fmt.Sprintf("permissions.%s[%d]", s.name, i),
						Position: pos,
					})
				}
			}
		}
	}

	checkTested := func(section, typ string, pos Position, tested string) {
		if tested == todoSentinel {
			out = append(out, ProvenanceIssue{
				Section: section, Type: typ,
				Field:    "tested_against_provider",
				Position: pos,
			})
		}
	}

	for _, e := range cat.Resources {
		checkVerification("resources", e.Type, e.Position, e.Verification)
		checkTested("resources", e.Type, e.Position, e.TestedAgainstProvider)
		checkPermissions("resources", e.Type, e.Position, e.Permissions)
	}
	for _, e := range cat.DataSources {
		checkVerification("data_sources", e.Type, e.Position, e.Verification)
		checkTested("data_sources", e.Type, e.Position, e.TestedAgainstProvider)
		// DataSourcePermissions only carries Plan; reuse a synthetic
		// PermissionSet to avoid duplicating the "list of strings"
		// scan here.
		checkPermissions("data_sources", e.Type, e.Position, PermissionSet{Plan: e.Permissions.Plan})
	}
	for _, e := range cat.IAMBindings {
		checkVerification("iam_bindings", e.Type, e.Position, e.Verification)
		checkTested("iam_bindings", e.Type, e.Position, e.TestedAgainstProvider)
		checkPermissions("iam_bindings", e.Type, e.Position, e.Permissions)
		if e.ParentResource == todoSentinel {
			out = append(out, ProvenanceIssue{
				Section: "iam_bindings", Type: e.Type,
				Field:    "parent_resource",
				Position: e.Position,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// computeDrifting returns entries whose tested_against_provider
// constraint excludes referenceVersion. Sorted by (Section, Type).
//
// "Excludes" is decided by satisfiesConstraint, which parses each
// comma-separated clause and applies the operator to a numeric
// component comparison against referenceVersion. An entry whose
// constraint cannot be parsed at all is left out of the drift report
// — surfacing those is the validator's job, not the stats command's.
func computeDrifting(cat *Catalog, referenceVersion string) []DriftEntry {
	out := []DriftEntry{}
	add := func(section, typ, tested string, pos Position) {
		if tested == "" {
			return
		}
		ok, parsed := satisfiesConstraint(referenceVersion, tested)
		if !parsed {
			return
		}
		if !ok {
			out = append(out, DriftEntry{
				Section:               section,
				Type:                  typ,
				TestedAgainstProvider: tested,
				Position:              pos,
			})
		}
	}

	for _, e := range cat.Resources {
		add("resources", e.Type, e.TestedAgainstProvider, e.Position)
	}
	for _, e := range cat.DataSources {
		add("data_sources", e.Type, e.TestedAgainstProvider, e.Position)
	}
	for _, e := range cat.IAMBindings {
		add("iam_bindings", e.Type, e.TestedAgainstProvider, e.Position)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// constraintClausePattern parses one comma-separated piece of a
// tested_against_provider constraint. Captures: (1) operator, (2)
// version-like token. The operator alternation lists two-character
// operators ahead of one-character ones for the same reason the
// validator's regex does — see the doc-comment on
// testedAgainstProviderConstraintPattern. The version capture mirrors
// the validator's accepted shape (digit-led alphanumeric / dot /
// hyphen / plus).
var constraintClausePattern = regexp.MustCompile(
	`^\s*(>=|<=|!=|~>|>|<|=)?\s*(\d[0-9A-Za-z.\-+]*)\s*$`,
)

// satisfiesConstraint reports whether reference satisfies every
// comma-separated clause of constraint. The second return is true if
// the constraint was structurally parseable and false if any clause
// failed to match constraintClausePattern — callers use the parsed
// flag to decide whether to surface the entry.
//
// Clause semantics:
//
//   - `>=`, `<=`, `>`, `<`, `=`, `!=`: numeric component comparison via
//     compareVersions; a missing operator is treated as `=`.
//   - `~>`: pessimistic lower bound (>=) is enforced; the implicit
//     upper bound is intentionally not enforced because deciding the
//     ceiling without semver-aware parsing is guesswork. Treating an
//     "obviously out-of-range" version as in-range under-reports drift
//     rather than over-reports it, which is the safer failure mode for
//     a diagnostic command.
//   - Wildcard suffix (`6.x`, `6.0.x`): the validator accepts this
//     short form (see testedAgainstProviderConstraintPattern). With no
//     operator (or `=`), the clause matches when reference's leading
//     components equal the prefix; with `!=`, the inverse. For `>=`
//     and `~>` the wildcard is a lower bound at the prefix's leading
//     components — exactly what `>= 6.x` reads as. For `<=`, `>`, `<`
//     the prefix is treated as a point version (its highest in-range
//     value is unspecified without semver-aware parsing), which
//     under-reports drift — same trade-off as `~>`'s missing upper
//     bound.
func satisfiesConstraint(reference, constraint string) (bool, bool) {
	refV, _, ok := parseVersion(reference)
	if !ok {
		return false, false
	}

	for _, clause := range strings.Split(constraint, ",") {
		m := constraintClausePattern.FindStringSubmatch(clause)
		if m == nil {
			return false, false
		}
		op := m[1]
		if op == "" {
			op = "="
		}
		clauseV, clauseIsPrefix, ok := parseVersion(m[2])
		if !ok {
			return false, false
		}
		// Prefix constraints redefine equality: `6.x` means "any 6.y.z
		// matches" rather than "ref == [6]". Comparison-based operators
		// fall through to compareVersions, which already treats missing
		// trailing components as zero — so `>= 6.x` evaluates as
		// `>= 6.0.0…`, the natural reading.
		if clauseIsPrefix {
			switch op {
			case "=":
				if !versionHasPrefix(refV, clauseV) {
					return false, true
				}
				continue
			case "!=":
				if versionHasPrefix(refV, clauseV) {
					return false, true
				}
				continue
			}
		}
		cmp := compareVersions(refV, clauseV)
		switch op {
		case ">=":
			if cmp < 0 {
				return false, true
			}
		case "<=":
			if cmp > 0 {
				return false, true
			}
		case ">":
			if cmp <= 0 {
				return false, true
			}
		case "<":
			if cmp >= 0 {
				return false, true
			}
		case "=":
			if cmp != 0 {
				return false, true
			}
		case "!=":
			if cmp == 0 {
				return false, true
			}
		case "~>":
			// Lower bound only — see doc-comment.
			if cmp < 0 {
				return false, true
			}
		}
	}
	return true, true
}

// parseVersion splits a version string into numeric components. Only
// the leading dot-separated digit run is parsed; a trailing
// "-rc1+build.7" suffix is dropped because the catalog's drift report
// is concerned with major / minor / patch ordering, not pre-release
// metadata.
//
// The middle return is true when the string ends in `.x` or `.X` — the
// short-form wildcard the validator accepts (see
// testedAgainstProviderConstraintPattern). For `6.x` parseVersion
// returns ([6], true, true); satisfiesConstraint then redefines `=` to
// "ref's leading components match the prefix" rather than the strict
// numeric equality used for non-wildcard versions.
//
// Returns ok=false when the string is missing a leading digit or
// contains no parseable component.
func parseVersion(s string) ([]int, bool, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false, false
	}
	// Detect the `.x` / `.X` wildcard suffix and strip it before the
	// numeric parse. The validator only accepts a single trailing
	// wildcard segment (anything else fails the version-token regex
	// at load time), so a single HasSuffix check is sufficient — we
	// do not need to scan for `.x.0` or other malformed shapes.
	isPrefix := false
	if strings.HasSuffix(s, ".x") || strings.HasSuffix(s, ".X") {
		s = s[:len(s)-2]
		isPrefix = true
	}
	// Drop pre-release / build metadata; numeric components stop at
	// the first non-digit-dot character.
	end := len(s)
	for i, r := range s {
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '.' {
			continue
		}
		end = i
		break
	}
	core := s[:end]
	if core == "" {
		return nil, false, false
	}
	parts := strings.Split(core, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			// Trailing dot or doubled dot — treat as malformed.
			return nil, false, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false, false
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, false, false
	}
	return out, isPrefix, true
}

// versionHasPrefix reports whether version's leading components equal
// prefix. A prefix longer than the version is padded on the version
// side with zeros — same convention compareVersions uses — so the
// reference `6` matches the prefix `6.0` (both read as `6.0.0…`).
//
// This is the comparator that backs `=` and `!=` for wildcard
// constraints like `6.x`. Strict numeric equality would reject
// `parseVersion("6.15.0")` against `parseVersion("6.x") = [6]`; this
// helper accepts the match because the wildcard explicitly opts into
// "ignore trailing components".
func versionHasPrefix(version, prefix []int) bool {
	for i, p := range prefix {
		var v int
		if i < len(version) {
			v = version[i]
		}
		if v != p {
			return false
		}
	}
	return true
}

// compareVersions is a numeric component comparator. Components beyond
// the shorter slice's length are treated as zero, so "6.0" compares
// equal to "6.0.0" — the convention semver uses for unspecified
// trailing components.
func compareVersions(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
