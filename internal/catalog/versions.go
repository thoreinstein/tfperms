package catalog

// versions.go owns the aggregation that powers the
// `tfperms catalog versions` CLI command. Companion to stats.go (the
// other diagnostic aggregator) and a sibling of the data model in
// catalog.go.
//
// Layout responsibilities:
//   - This file defines VersionGroup (the value type the renderer
//     consumes) and AggregateVersions (the pure aggregation entry point).
//   - The cmd/tfperms layer is responsible for rendering []VersionGroup
//     into a human-readable report. Splitting the concerns means the
//     aggregation can be unit-tested without parsing formatted text and
//     a future caller (a JSON dump, a Web UI) can reuse the slice
//     unchanged.
//
// Grouping policy:
//
// AggregateVersions groups by the literal `tested_against_provider`
// string. Two clauses that are semantically equivalent but textually
// different (`">= 5.0.0"` vs `">=5.0.0"`) are reported as distinct
// groups. This is deliberate — implementing a full constraint
// normaliser would duplicate logic the validator already declines to
// own (see testedAgainstProviderConstraintPattern in validator.go) and
// would hide formatting drift the catalog wants visible. The
// `catalog stats` drift report is the right place for semantic
// constraint analysis; this command is a literal census.
//
// Determinism:
//
// The returned slice is sorted by Count descending and ties broken by
// the literal constraint string ascending, so a golden-file test on
// the rendered output remains stable across runs and across map-
// iteration orders.

import "sort"

// VersionGroup is one row of the version census: a verbatim
// `tested_against_provider` string and the number of catalog entries
// (across resources, data sources, and IAM bindings) that declared it.
//
// The struct is exported so the cmd-layer renderer can format it
// directly and a future JSON consumer can serialise it without going
// through the renderer.
type VersionGroup struct {
	// TestedAgainstProvider is the literal constraint string from the
	// YAML, preserved verbatim. Whitespace differences between
	// otherwise-equivalent clauses produce distinct groups (see the
	// file-level grouping policy comment for the rationale).
	TestedAgainstProvider string
	// Count is the number of catalog entries that declared this exact
	// constraint string. The sum of Count across all VersionGroups
	// equals TotalResources + TotalDataSources + TotalIAMBindings on
	// CatalogStats — every entry contributes exactly once.
	Count int
}

// AggregateVersions walks every entry in cat (resources, data sources,
// and IAM bindings) and groups them by the literal
// `tested_against_provider` string. The returned slice is sorted by
// Count descending; ties break on TestedAgainstProvider ascending so
// the order is deterministic across runs.
//
// A nil catalog is treated as an empty one: the function returns an
// empty (non-nil) slice. Empty constraint strings — which the
// validator rejects at load time but a hand-rolled in-memory catalog
// might carry — are grouped under the literal empty string rather
// than dropped. Surfacing them lets a contributor mid-edit see the
// blank entry in their report rather than wonder why the totals are
// off; the validator is the right place to refuse unsaved YAML.
//
// AggregateVersions is pure: no IO, no logging. Every metric is
// derived from cat alone, which keeps unit tests deterministic and
// lets a future caller compute the census off a catalog loaded from
// somewhere other than the embedded FS without surprises.
func AggregateVersions(cat *Catalog) []VersionGroup {
	out := []VersionGroup{}
	if cat == nil {
		return out
	}

	counts := make(map[string]int)
	for _, e := range cat.Resources {
		counts[e.TestedAgainstProvider]++
	}
	for _, e := range cat.DataSources {
		counts[e.TestedAgainstProvider]++
	}
	for _, e := range cat.IAMBindings {
		counts[e.TestedAgainstProvider]++
	}

	out = make([]VersionGroup, 0, len(counts))
	for constraint, n := range counts {
		out = append(out, VersionGroup{
			TestedAgainstProvider: constraint,
			Count:                 n,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// Primary: Count descending — busiest constraints lead.
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		// Secondary: literal constraint ascending — stable tiebreak
		// so golden-file tests do not flake on map iteration order.
		return out[i].TestedAgainstProvider < out[j].TestedAgainstProvider
	})
	return out
}
