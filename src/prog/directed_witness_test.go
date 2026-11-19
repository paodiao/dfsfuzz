package prog

import (
	"math/rand"
	"testing"
)

// TestDirectedWitnessUsesProfileTargetConstraint pins P2-B7: witness selection
// must use the active profile's TargetConstraint. GlusterFS symlink/readlink
// targets may be directories, while the legacy global callNameConstraint table
// classifies them as file-only.
func TestDirectedWitnessUsesProfileTargetConstraint(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	cands := &witnessCands{dir: []string{"dirA"}}
	for _, call := range []string{"symlink", "readlink"} {
		got, ok := pickWitness(glusterfsProfile, cands, call, r)
		if !ok || got != "dirA" {
			t.Fatalf("pickWitness(%q) = %q (ok=%v), want dirA", call, got, ok)
		}
	}

	bn := directedBNode{
		directedNode: directedNode{path: "dirA", isDir: true},
		sibDir:       []string{"dirA"},
		sibFileIdx:   -1,
		sibDirIdx:    -1,
		sibFileIdx2:  -1,
		sibDirIdx2:   -1,
	}
	for _, call := range []string{"symlink", "readlink"} {
		got, ok := bn.pickSibling(glusterfsProfile, call, r)
		if !ok || got != "dirA" {
			t.Fatalf("pickSibling(%q) = %q (ok=%v), want dirA", call, got, ok)
		}
	}
}
