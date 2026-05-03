// Package catalogdata is a thin embed-only wrapper around the catalog
// YAML files at the repository root.
//
// The wrapper exists because the loader package (internal/catalog) cannot
// use //go:embed to reach files outside its own directory tree. Putting
// the YAML files at the repo root keeps them discoverable for
// contributors (the directory CONTRIBUTING.md points contributors at)
// while still letting the loader consume them through a standard fs.FS
// handle.
//
// This package is intentionally tiny — exactly one exported symbol, the
// embed.FS — so contributors editing or adding YAML files do not need to
// think about Go code. The loader at internal/catalog imports FS via an
// alias because both packages are spelled "catalog" in different
// directories of the repo.
package catalogdata

import "embed"

// FS holds every *.yaml file in this directory at compile time. The
// embed pattern is intentionally non-recursive: contributors add new
// service files at the top level (storage.yaml, compute.yaml, ...).
// Subdirectories are not part of the catalog layout.
//
//go:embed *.yaml
var FS embed.FS
