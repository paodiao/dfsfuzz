package prog

import "testing"

func TestLayeredChoiceStrategyClientTscOffsets(t *testing.T) {
	lcs := &LayeredChoiceStrategy{}
	lcs.SetTscOffsets([]int64{400, 500, 600})
	if got := lcs.tscoffFor(0); got != 400 {
		t.Fatalf("client root tscoff = %d, want 400", got)
	}
	if got := lcs.tscoffFor(2); got != 600 {
		t.Fatalf("client node 2 tscoff = %d, want 600", got)
	}
	if got := lcs.tscoffFor(3); got != 600 {
		t.Fatalf("out-of-range tscoff = %d, want last 600", got)
	}
}
