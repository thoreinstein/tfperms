package parser

// HCL parsing layer. Companion to walker.go: walker produces the file list,
// Parse turns those files into structured Resource values that downstream
// stages (.5 attribute extraction, .6 variable/locals expansion, Epic 3
// module resolution, Epic 5 dedup) iterate over.

import (
	"github.com/zclconf/go-cty/cty"
)

// Resource is a single resource or data block extracted from a Terraform
// configuration.
//
// Field contracts:
//   - Kind  is exactly "resource" or "data" — the HCL block type. Future
//     top-level block types (provider, module, output, ...) are silently
//     skipped by Parse and never produce a Resource.
//   - Type  is the first label of the block, e.g. "google_storage_bucket".
//   - Name  is the second label — the local name as written in HCL.
//   - File  is the source file path. It is whatever was passed to Parse,
//     so when the walker hands Parse absolute paths, this field is an
//     absolute path; the package does not normalise.
//   - Line  is reported via DefRange.Start.Line, which is the line of the
//     `resource`/`data` keyword. For conventionally-formatted Terraform
//     (`resource "x" "y" {` on one line) this is the same line as the
//     opening brace; for pathologically multi-line block headers it is
//     the keyword line, not the brace line.
//   - Attrs is always non-nil but always empty at this stage. Story .5
//     populates it with the block's argument values; downstream code
//     should be written to handle either an empty or a populated map so
//     it does not need to change when .5 lands.
type Resource struct {
	Kind  string
	Type  string
	Name  string
	File  string
	Line  int
	Attrs map[string]cty.Value
}

// Parse reads each .tf file, merges them into a single configuration, and
// returns the resource and data blocks as Resource values sorted by
// (File, Line). Skeleton at this stage; the parsing/extraction body is
// added in the next phase.
func Parse(files []string) ([]Resource, error) {
	if len(files) == 0 {
		return nil, nil
	}
	return nil, nil
}
