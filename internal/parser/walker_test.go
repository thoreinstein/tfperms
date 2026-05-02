package parser

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeFile creates a file with the given relative path under root, creating
// parent directories as needed. Helper used by the table-driven cases below
// to keep fixture construction concise.
func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte{}, 0o644); err != nil {
		t.Fatalf("write %q: %v", full, err)
	}
}

// baseNames returns the basenames of paths, sorted, so result-set assertions
// are independent of the order os.ReadDir surfaces entries in.
func baseNames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFindTerraformFiles_HappyAndEdge covers the cases whose fixtures are
// just files and directories (no symlinks). Each subtest builds its own
// t.TempDir so the cases never observe each other's state.
func TestFindTerraformFiles_HappyAndEdge(t *testing.T) {
	cases := []struct {
		name    string
		files   []string // relative paths (files; dirs are implied by parent components)
		want    []string // expected basenames in result, sorted; nil means expect error
		wantErr bool
	}{
		{
			name:  "happy path returns immediate .tf files and ignores non-tf",
			files: []string{"main.tf", "variables.tf", "README.md"},
			want:  []string{"main.tf", "variables.tf"},
		},
		{
			name:  "no recursion into subdirectories",
			files: []string{"main.tf", "submodule/inner.tf"},
			want:  []string{"main.tf"},
		},
		{
			name: "skip rules: dot-prefixed dirs are not traversed",
			files: []string{
				"main.tf",
				".terraform/x.tf",
				".terragrunt-cache/y.tf",
				".git/HEAD",
				".idea/foo",
			},
			want: []string{"main.tf"},
		},
		{
			name:    "empty directory returns error",
			files:   nil,
			wantErr: true,
		},
		{
			name:    "directory with no .tf files returns error",
			files:   []string{"README.md", "Makefile"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				writeFile(t, dir, f)
			}

			got, err := FindTerraformFiles(dir)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result: %v)", got)
				}
				msg := err.Error()
				if strings.Contains(msg, "\n") {
					t.Errorf("error message must be single-line; got %q", msg)
				}
				absDir, absErr := filepath.Abs(dir)
				if absErr != nil {
					t.Fatalf("filepath.Abs(%q): %v", dir, absErr)
				}
				if !strings.Contains(msg, absDir) {
					t.Errorf("error message %q does not contain absolute path %q", msg, absDir)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotNames := baseNames(got)
			if !equalStringSlices(gotNames, tc.want) {
				t.Errorf("result basenames = %v, want %v (full: %v)", gotNames, tc.want, got)
			}
		})
	}
}

// TestFindTerraformFiles_NonExistentPath checks that I/O errors from a
// missing directory wrap fs.ErrNotExist. Asserting via errors.Is keeps the
// test portable across OSes whose ReadDir messages differ.
func TestFindTerraformFiles_NonExistentPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := FindTerraformFiles(missing)
	if err == nil {
		t.Fatalf("expected error for missing path %q, got nil", missing)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %v does not wrap fs.ErrNotExist", err)
	}
}

// TestFindTerraformFiles_FilePathAsInput verifies we return an error when
// the caller passes a file path instead of a directory. The message text is
// OS-dependent (os.ReadDir surfaces "not a directory" on Linux and a
// different phrasing on Windows), so we only assert that an error occurs.
func TestFindTerraformFiles_FilePathAsInput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf")
	filePath := filepath.Join(dir, "main.tf")

	if _, err := FindTerraformFiles(filePath); err == nil {
		t.Fatalf("expected error when input is a file path, got nil")
	}
}

// TestFindTerraformFiles_DoesNotFollowSymlinks confirms the implementation
// uses os.DirEntry.Type (which reads the link's own mode) and not Info
// (which would dereference). On unprivileged Windows runs os.Symlink fails;
// the test skips cleanly there.
func TestFindTerraformFiles_DoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()

	writeFile(t, dir, "real.tf")
	writeFile(t, otherDir, "target.tf")

	// Symlinked file: link.tf -> otherDir/target.tf
	linkFile := filepath.Join(dir, "link.tf")
	if err := os.Symlink(filepath.Join(otherDir, "target.tf"), linkFile); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Symlinked directory: linkdir -> otherDir
	linkDir := filepath.Join(dir, "linkdir")
	if err := os.Symlink(otherDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := FindTerraformFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotNames := baseNames(got)
	want := []string{"real.tf"}
	if !equalStringSlices(gotNames, want) {
		t.Errorf("result basenames = %v, want %v (full: %v); symlinks must not be followed",
			gotNames, want, got)
	}
}
