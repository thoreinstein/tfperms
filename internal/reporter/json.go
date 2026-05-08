package reporter

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/thoreinstein/tfperms/internal/resolver"
)

// jsonOutput is the stable, versioned JSON representation of a
// tfperms analysis result. See docs/schema/tfperms-output-v1.json
// for the formal JSON Schema.
type jsonOutput struct {
	Version                string                           `json:"version"`
	Summary                jsonSummary                      `json:"summary"`
	PlanPermissions        []string                         `json:"plan_permissions"`
	ApplyOnlyPermissions   []string                         `json:"apply_only_permissions"`
	TotalApplyPermissions  []string                         `json:"total_apply_permissions"`
	Resources              []jsonResource                   `json:"resources"`
	Diagnostics            []resolver.Diagnostic            `json:"diagnostics"`
	Unknowns               []resolver.UnknownResource       `json:"unknowns"`
	UnresolvedConditionals []resolver.UnresolvedConditional `json:"unresolved_conditionals"`
	Metadata               jsonMetadata                     `json:"metadata"`
}

type jsonSummary struct {
	PermissionCount int `json:"permission_count"`
	ResourceCount   int `json:"resource_count"`
	UnknownCount    int `json:"unknown_count"`
	UnresolvedCount int `json:"unresolved_count"`
}

type jsonResource struct {
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	ModulePath []string `json:"module_path,omitempty"`
	// Permissions is the sorted, deduplicated union of all permissions
	// required by this specific resource (Base + Applied).
	Permissions []string `json:"permissions"`
}

type jsonMetadata struct {
	TFPermsVersion string `json:"tfperms_version"`
}

// RenderJSON writes the stable JSON representation of res to w.
//
// resourceCount is the number of distinct Terraform resources analysed,
// matching Render's definition.
//
// version is the tfperms build version (main.version) and flows into
// the `metadata` block.
//
// Canonicalize is called on entry so output is deterministic, including
// all keys and array elements. The v1.0 contract guarantees bit-identical
// output for identical inputs (see docs/json-schema.md), which is why
// this signature deliberately takes no wall-clock timestamp: a
// generated_at field would defeat the determinism guarantee.
func RenderJSON(w io.Writer, res resolver.Result, resourceCount int, version string) error {
	res = Canonicalize(res)

	output := jsonOutput{
		Version: "1.0",
		Summary: jsonSummary{
			PermissionCount: len(res.TotalApplyPerms),
			ResourceCount:   resourceCount,
			UnknownCount:    len(res.Unknowns),
			UnresolvedCount: len(res.Unresolved),
		},
		PlanPermissions:        res.PlanPerms,
		ApplyOnlyPermissions:   res.ApplyOnlyPerms,
		TotalApplyPermissions:  res.TotalApplyPerms,
		Resources:              make([]jsonResource, len(res.Resources)),
		Diagnostics:            res.Diagnostics,
		Unknowns:               res.Unknowns,
		UnresolvedConditionals: res.Unresolved,
		Metadata: jsonMetadata{
			TFPermsVersion: version,
		},
	}

	for i, r := range res.Resources {
		output.Resources[i] = jsonResource{
			Type:       r.Type,
			Name:       r.Name,
			File:       r.File,
			Line:       r.Line,
			ModulePath: r.ModulePath,
			// Requirement 5: sorted, deduplicated union of all permissions.
			// ResourceResult.BasePerms and Applied[].Permissions are
			// already sorted/deduped by Canonicalize; we just need to
			// union them.
			Permissions: unionResourcePerms(r),
		}
	}

	ew := &errWriter{w: w}
	encoder := json.NewEncoder(ew)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		// Flush any pending writer error first: a broken pipe latched
		// during the encode would otherwise be masked by the encoder
		// surfacing its own write error from the same source. Either
		// way, returning a wrapped error preserves the underlying
		// cause for errors.Is comparisons. Mirrors role.go.
		if ew.err != nil {
			return fmt.Errorf("write json: %w", ew.err)
		}
		return fmt.Errorf("encode json: %w", err)
	}
	if ew.err != nil {
		return fmt.Errorf("write json: %w", ew.err)
	}
	return nil
}

// unionResourcePerms computes the sorted, deduplicated union of
// r.BasePerms and every permission list in r.Applied.
func unionResourcePerms(r resolver.ResourceResult) []string {
	set := make(map[string]struct{})
	for _, p := range r.BasePerms {
		set[p] = struct{}{}
	}
	for _, a := range r.Applied {
		for _, p := range a.Permissions {
			set[p] = struct{}{}
		}
	}
	// Re-use sortedSet from resolver? No, it's internal.
	// But sortedStrings from reporter.go is available.
	perms := make([]string, 0, len(set))
	for p := range set {
		perms = append(perms, p)
	}
	return sortedStrings(perms)
}
