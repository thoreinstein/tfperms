// Package parser walks Terraform configurations and extracts structural
// information about resource blocks. This file owns file discovery; HCL
// parsing lives in parse.go (added in a later story).
package parser

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FindTerraformFiles returns the .tf files in dir without recursion.
//
// It mirrors Terraform's own configuration-loading semantics: only the
// immediate directory is treated as the configuration; subdirectories are
// separate modules and are not traversed here (module recursion is owned
// by the resolver in Epic 3).
//
// Skipped: any entry whose name begins with '.' (this subsumes .terraform
// and .terragrunt-cache — both well-known cache dirs that should never be
// parsed — as well as .git, .idea, and editor dotfiles). Symlinks (file or
// directory) are NOT followed; we use os.DirEntry.Type rather than
// os.DirEntry.Info specifically to read the link's own mode bits without
// dereferencing.
//
// Returns a single-line, absolute-path-bearing error when dir contains no
// .tf files. Underlying I/O errors (missing dir, permission denied, dir is
// actually a file) are wrapped via %w so callers can use errors.Is against
// fs.ErrNotExist and friends.
func FindTerraformFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("tfperms: read directory %q: %w", dir, err)
	}

	var found []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // skips .terraform, .terragrunt-cache, .git, dotfiles
		}
		// Type() reads the entry's own mode without following symlinks.
		// Do NOT replace with Info() — that would dereference the link
		// and break the symlink-non-following test.
		mode := e.Type()
		if mode&fs.ModeSymlink != 0 {
			continue
		}
		if mode.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".tf") {
			continue
		}
		found = append(found, filepath.Join(dir, name))
	}

	if len(found) == 0 {
		abs, absErr := filepath.Abs(dir)
		if absErr != nil {
			// filepath.Abs only fails if os.Getwd fails; surface both so
			// the user can diagnose the rare case it happens.
			return nil, fmt.Errorf("tfperms: no .tf files found in %s (could not resolve absolute path: %v)", dir, absErr)
		}
		return nil, fmt.Errorf("tfperms: no .tf files found in %s", abs)
	}
	return found, nil
}
