package parser

import "testing"

// TestParse_Empty anchors the package layout: Parse(nil) and Parse([]string{})
// must return (nil, nil) without touching the filesystem. This is the contract
// that lets cmd/tfperms call Parse unconditionally even when the walker
// returned no files (although in practice the walker errors first).
func TestParse_Empty(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%v) error: %v", in, err)
		}
		if got != nil {
			t.Errorf("Parse(%v) = %v, want nil", in, got)
		}
	}
}
