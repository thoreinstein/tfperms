package catalog

import "testing"

// TestAggregateVersionsNilCatalog confirms AggregateVersions handles a
// nil catalog the same way ComputeStats does — by returning an empty
// non-nil slice rather than panicking. The cmd layer always calls it
// with the result of catalog.Load() (non-nil on success), but an
// in-memory caller might pass nil, and a nil-deref would be a worse
// failure mode than the empty-slice fallback.
func TestAggregateVersionsNilCatalog(t *testing.T) {
	got := AggregateVersions(nil)
	if got == nil {
		t.Fatalf("AggregateVersions(nil) = nil, want empty (non-nil) slice")
	}
	if len(got) != 0 {
		t.Errorf("AggregateVersions(nil) len = %d, want 0: %+v", len(got), got)
	}
}

// TestAggregateVersionsEmptyCatalog confirms an empty (but non-nil)
// catalog yields an empty slice. Mirrors the nil case but exercises the
// builder's loop bodies — important because a future change that
// pre-seeds the map with a "default" entry would silently break this
// invariant otherwise.
func TestAggregateVersionsEmptyCatalog(t *testing.T) {
	got := AggregateVersions(newCatalog())
	if got == nil {
		t.Fatalf("AggregateVersions(empty) = nil, want empty (non-nil) slice")
	}
	if len(got) != 0 {
		t.Errorf("AggregateVersions(empty) len = %d, want 0: %+v", len(got), got)
	}
}

// TestAggregateVersionsCountsAcrossSections confirms entries from all
// three sections (resources, data sources, IAM bindings) contribute to
// the same constraint group. A regression here would produce undercounts
// that look correct on a resources-only catalog but silently drop data
// sources / iam bindings from the census.
func TestAggregateVersionsCountsAcrossSections(t *testing.T) {
	c := newCatalog()
	c.Resources["google_a"] = &ResourceEntry{
		Type:                  "google_a",
		TestedAgainstProvider: ">=6.0.0,<7.0.0",
	}
	c.Resources["google_b"] = &ResourceEntry{
		Type:                  "google_b",
		TestedAgainstProvider: ">=6.0.0,<7.0.0",
	}
	c.DataSources["google_c"] = &DataSourceEntry{
		Type:                  "google_c",
		TestedAgainstProvider: ">=6.0.0,<7.0.0",
	}
	c.IAMBindings["google_d_iam"] = &IAMBindingEntry{
		Type:                  "google_d_iam",
		TestedAgainstProvider: ">=6.0.0,<7.0.0",
	}

	got := AggregateVersions(c)
	if len(got) != 1 {
		t.Fatalf("AggregateVersions len = %d, want 1: %+v", len(got), got)
	}
	if got[0].TestedAgainstProvider != ">=6.0.0,<7.0.0" {
		t.Errorf("got[0].TestedAgainstProvider = %q, want %q",
			got[0].TestedAgainstProvider, ">=6.0.0,<7.0.0")
	}
	if got[0].Count != 4 {
		t.Errorf("got[0].Count = %d, want 4", got[0].Count)
	}
}

// TestAggregateVersionsLiteralGrouping pins the grouping policy
// documented at the top of versions.go: two textually different
// constraints (different whitespace) are NOT collapsed into one group.
// A regression that normalised whitespace would silently hide
// formatting drift the catalog wants visible.
func TestAggregateVersionsLiteralGrouping(t *testing.T) {
	c := newCatalog()
	c.Resources["google_tight"] = &ResourceEntry{
		Type:                  "google_tight",
		TestedAgainstProvider: ">=5.0.0",
	}
	c.Resources["google_loose"] = &ResourceEntry{
		Type:                  "google_loose",
		TestedAgainstProvider: ">= 5.0.0",
	}

	got := AggregateVersions(c)
	if len(got) != 2 {
		t.Fatalf("AggregateVersions len = %d, want 2 (whitespace-different "+
			"constraints must group separately): %+v", len(got), got)
	}
}

// TestAggregateVersionsSortOrder pins the sort contract: Count
// descending, with TestedAgainstProvider ascending as the tiebreak. A
// golden-file test on the rendered output relies on this — without a
// deterministic order, the report would flake across runs whenever the
// runtime's map-iteration order shifted.
func TestAggregateVersionsSortOrder(t *testing.T) {
	c := newCatalog()
	// Three entries on ">=6.0.0,<7.0.0" — most popular.
	for _, name := range []string{"google_a", "google_b", "google_c"} {
		c.Resources[name] = &ResourceEntry{
			Type:                  name,
			TestedAgainstProvider: ">=6.0.0,<7.0.0",
		}
	}
	// Two entries on ">=5.0.0,<6.0.0" — second place.
	for _, name := range []string{"google_d", "google_e"} {
		c.Resources[name] = &ResourceEntry{
			Type:                  name,
			TestedAgainstProvider: ">=5.0.0,<6.0.0",
		}
	}
	// Two entries on ">=4.0.0,<5.0.0" — same count as second place, must
	// sort lexicographically before ">=5.0.0,<6.0.0" by tiebreak.
	for _, name := range []string{"google_f", "google_g"} {
		c.Resources[name] = &ResourceEntry{
			Type:                  name,
			TestedAgainstProvider: ">=4.0.0,<5.0.0",
		}
	}
	// One entry on ">=7.0.0" — least popular.
	c.Resources["google_h"] = &ResourceEntry{
		Type:                  "google_h",
		TestedAgainstProvider: ">=7.0.0",
	}

	got := AggregateVersions(c)
	want := []VersionGroup{
		{TestedAgainstProvider: ">=6.0.0,<7.0.0", Count: 3},
		{TestedAgainstProvider: ">=4.0.0,<5.0.0", Count: 2},
		{TestedAgainstProvider: ">=5.0.0,<6.0.0", Count: 2},
		{TestedAgainstProvider: ">=7.0.0", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("AggregateVersions len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("AggregateVersions[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestAggregateVersionsEmptyConstraintGrouped pins the contract from
// the doc-comment: an empty `tested_against_provider` string is grouped
// under the empty literal rather than dropped. The validator forbids
// blank values at load time, but a hand-rolled in-memory catalog can
// carry one — and surfacing it in the census is the right behaviour
// for a contributor inspecting a partial edit.
func TestAggregateVersionsEmptyConstraintGrouped(t *testing.T) {
	c := newCatalog()
	c.Resources["google_blank"] = &ResourceEntry{
		Type:                  "google_blank",
		TestedAgainstProvider: "",
	}
	c.Resources["google_set"] = &ResourceEntry{
		Type:                  "google_set",
		TestedAgainstProvider: ">=6.0.0",
	}

	got := AggregateVersions(c)
	if len(got) != 2 {
		t.Fatalf("AggregateVersions len = %d, want 2 (empty constraint must "+
			"appear as its own group): %+v", len(got), got)
	}
	// Tie on Count=1; lexicographic tiebreak puts "" before ">=6.0.0".
	if got[0].TestedAgainstProvider != "" || got[0].Count != 1 {
		t.Errorf("got[0] = %+v, want {\"\" 1}", got[0])
	}
	if got[1].TestedAgainstProvider != ">=6.0.0" || got[1].Count != 1 {
		t.Errorf("got[1] = %+v, want {\">=6.0.0\" 1}", got[1])
	}
}
