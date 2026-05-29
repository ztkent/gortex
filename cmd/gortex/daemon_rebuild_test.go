package main

import "testing"

type fakeRebuildYes struct{}

func (fakeRebuildYes) NeedsRebuild() bool { return true }

type fakeRebuildNo struct{}

func (fakeRebuildNo) NeedsRebuild() bool { return false }

// storeNeedsRebuild must detect the optional NeedsRebuild capability and
// default to false for backends that don't implement it (the in-memory
// store), so the warm-restart fast path is bypassed only on an explicit
// rebuild signal.
func TestStoreNeedsRebuild(t *testing.T) {
	cases := []struct {
		name string
		g    any
		want bool
	}{
		{"implements true", fakeRebuildYes{}, true},
		{"implements false", fakeRebuildNo{}, false},
		{"no capability", struct{}{}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := storeNeedsRebuild(c.g); got != c.want {
			t.Errorf("%s: storeNeedsRebuild = %v, want %v", c.name, got, c.want)
		}
	}
}
