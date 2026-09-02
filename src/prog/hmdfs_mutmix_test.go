package prog

import "testing"

// TestMutateMixForSize verifies the size-banded mutation mix used by the
// inodeops/fileops DCT mutators: tiny programs are insertion-heavy (they must
// reach a size where modifier pairs exist), large ones removal-heavy (bounded
// execution cost and pair space). Band boundaries must stay in sync with
// RecommendedCalls (the size cap) and the docs on mutateMixForSize.
func TestMutateMixForSize(t *testing.T) {
	cases := []struct {
		size                              int
		ins, removeOne, removeGroup, mute int
	}{
		// Tiny band (1-3): insertion dominates, group removal disabled.
		{1, 85, 5, 0, 10},
		{3, 85, 5, 0, 10},
		// Grow band (4-10).
		{4, 60, 10, 5, 25},
		{10, 60, 10, 5, 25},
		// Peak band (11-15).
		{11, 35, 25, 10, 30},
		{15, 35, 25, 10, 30},
		// Shrink band (16+, up to the cap of 20).
		{16, 10, 40, 20, 30},
		{20, 10, 40, 20, 30},
		// Beyond the cap (defensive: callers clamp via fallback).
		{21, 10, 40, 20, 30},
	}
	for _, c := range cases {
		ins, removeOne, removeGroup, mute := mutateMixForSize(c.size)
		if ins != c.ins || removeOne != c.removeOne || removeGroup != c.removeGroup || mute != c.mute {
			t.Errorf("mutateMixForSize(size=%d) = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				c.size, ins, removeOne, removeGroup, mute, c.ins, c.removeOne, c.removeGroup, c.mute)
		}
		// Weights must always sum to 100 so the roll in the mutators spans
		// the full [0,100) range.
		if sum := ins + removeOne + removeGroup + mute; sum != 100 {
			t.Errorf("mutateMixForSize(size=%d) weights sum to %d, want 100", c.size, sum)
		}
	}
}
