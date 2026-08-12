package prog

import (
	"math/rand"
	"testing"
)

// TestPathRelBetween covers the geometric path relation classifier used by
// the dynamic group mutations.
func TestPathRelBetween(t *testing.T) {
	cases := []struct {
		anchor, conc string
		want         PathRelation
	}{
		{"merge_view/a", "merge_view/a", PathSame},
		{"merge_view/a", "merge_view/a/f", PathChild},
		{"merge_view/a/f", "merge_view/a", PathParent},
		{"merge_view/a", "merge_view/b", PathSibling},
		{"merge_view/a", "merge_view/x/y/z", PathNoRel},
		{"merge_view/a", "", PathNoRel},
		{"", "merge_view/a", PathNoRel},
	}
	for _, c := range cases {
		if got := pathRelBetween(c.anchor, c.conc); got != c.want {
			t.Fatalf("pathRelBetween(%q, %q) = %v, want %v", c.anchor, c.conc, got, c.want)
		}
	}
}

// TestFindConcurrentCalls covers the execution-window overlap detection
// (windows normalized to the global TSC domain via tscoffs).
func TestFindConcurrentCalls(t *testing.T) {
	mk := func(stime, etime uint64) *Call {
		return &Call{CheckInfo: &FileMetadata{Stime: stime, Etime: etime}}
	}
	p0 := &Prog{Calls: []*Call{mk(100, 200)}}
	p1 := &Prog{Calls: []*Call{mk(150, 250), mk(300, 400)}}
	p2 := &Prog{Calls: []*Call{{}, mk(50, 90)}}
	ps := []*Prog{p0, p1, p2}
	anchor := GroupPosition{ProgIdx: 0, CallIdx: 0}

	// tscoffs shift p2 by +100: its window [50,90] -> [150,190] overlaps the
	// anchor [100,200], so it must be found too.
	conc := findConcurrentCalls(ps, anchor, "merge_view/a", []int64{0, 0, -100})
	if len(conc) != 2 {
		t.Fatalf("findConcurrentCalls found %v calls, want 2", len(conc))
	}
	// Overlap requires s1 < e2 && s2 < e1: p1[0] [150,250] overlaps
	// (150<200 && 100<250), p1[1] [300,400] does not (300<200 false).
	// p2[0] has no timing (skipped), p2[1] [50,90]-(-100)=[150,190] overlaps.
}

// TestPickAnchor requires a path-carrying call in ps[0] with timing info and
// restricts to read/write calls when asked.
func TestPickAnchor(t *testing.T) {
	r := &randGen{Rand: rand.New(rand.NewSource(1))}
	// No timing info anywhere: must fail.
	p0 := &Prog{Calls: []*Call{{}}}
	_, _, ok := pickAnchor([]*Prog{p0}, r, false)
	if ok {
		t.Fatal("pickAnchor succeeded without any timing info")
	}
	// With timing info but no path: must fail.
	p0.Calls[0] = &Call{CheckInfo: &FileMetadata{Stime: 1, Etime: 2}}
	_, _, ok = pickAnchor([]*Prog{p0}, r, false)
	if ok {
		t.Fatal("pickAnchor succeeded without any path")
	}
}

// TestChooseTemporal covers the temporal-form selection: new combos start at
// 50/50 (both forms appear), and feedback shifts the choice.
func TestChooseTemporal(t *testing.T) {
	dct := NewDistributedChoiceTable("fileops")
	r := rand.New(rand.NewSource(1))
	root := "write"
	variant := CallVariant{CallName: "read", PathRelation: PathSame}

	seen := map[TemporalRel]int{}
	for i := 0; i < 40; i++ {
		seen[dct.ChooseTemporal(root, variant, r)]++
	}
	if seen[TemporalConcurrent] == 0 || seen[TemporalHB] == 0 {
		t.Fatalf("50/50 start not observed: %v", seen)
	}

	// Reward the HB form 8 times: the choice must shift toward HB.
	for i := 0; i < 8; i++ {
		dct.UpdateTemporalWeight(root, variant, TemporalHB)
	}
	hb := 0
	for i := 0; i < 40; i++ {
		if dct.ChooseTemporal(root, variant, r) == TemporalHB {
			hb++
		}
	}
	if hb < 30 {
		t.Fatalf("HB form not preferred after rewards: %v/40", hb)
	}
}

// TestFirstBoundaryAfter covers the causal-insertion position: the first
// boundary at or after refTime. Boundaries of the three calls (windows
// [100,200] [300,400] [500,600]) are 0 / 200 / 400 / 600.
func TestFirstBoundaryAfter(t *testing.T) {
	mk := func(stime, etime uint64) *Call {
		return &Call{CheckInfo: &FileMetadata{Stime: stime, Etime: etime}}
	}
	p := &Prog{Calls: []*Call{mk(100, 200), mk(300, 400), mk(500, 600)}}
	if pos := firstBoundaryAfter(p, 250, 0); pos != 2 {
		t.Fatalf("firstBoundaryAfter(250) = %v, want 2", pos)
	}
	if pos := firstBoundaryAfter(p, 100, 0); pos != 1 {
		t.Fatalf("firstBoundaryAfter(100) = %v, want 1", pos)
	}
	if pos := firstBoundaryAfter(p, 650, 0); pos != 3 {
		t.Fatalf("firstBoundaryAfter(650) = %v, want 3", pos)
	}
	if pos := firstBoundaryAfter(p, -1, 0); pos != -1 {
		t.Fatalf("firstBoundaryAfter(-1) = %v, want -1", pos)
	}
	np := &Prog{Calls: []*Call{{}, {}}}
	if pos := firstBoundaryAfter(np, 100, 0); pos != -1 {
		t.Fatalf("firstBoundaryAfter without timing = %v, want -1", pos)
	}
}

// TestDagPairToVariantTemporal covers the temporal dimension of the mapping:
// concurrent and HB pairs map to the same combo but carry their temporal
// relation through.
func TestDagPairToVariantTemporal(t *testing.T) {
	mk := &DAGVertex{FuncID: FuncMkdir, Path: "merge_view/100/a"}
	cr := &DAGVertex{FuncID: FuncCreate, Path: "merge_view/100/a/f"}
	cc := &DAGPair{A: mk, B: cr, PathRel: PathParentChild, Temporal: TemporalConcurrent}
	root, variant, temporal, ok := DagPairToVariant(cc)
	if !ok || root != "mkdir" || variant.CallName != "creat" || temporal != TemporalConcurrent {
		t.Fatalf("concurrent mapping wrong: root=%v variant=%v temporal=%v ok=%v", root, variant, temporal, ok)
	}
	hb := &DAGPair{A: mk, B: cr, PathRel: PathParentChild, Temporal: TemporalHB}
	root, variant, temporal, ok = DagPairToVariant(hb)
	if !ok || root != "mkdir" || temporal != TemporalHB {
		t.Fatalf("HB mapping wrong: root=%v variant=%v temporal=%v ok=%v", root, variant, temporal, ok)
	}
}

// TestMarkYieldRewardsWeight verifies that a yield (novel concurrent pair)
// rewards the combo: weight +1 per yield, capped at maxComboWeight.
func TestMarkYieldRewardsWeight(t *testing.T) {
	dct := NewDistributedChoiceTable("fileops")
	root := "write"
	variant := CallVariant{CallName: "read", PathRelation: PathSame}
	w0 := dct.WeightOf(root, variant)
	dct.MarkYield(root, variant)
	if w := dct.WeightOf(root, variant); w != w0+1 {
		t.Fatalf("weight after yield = %v, want %v", w, w0+1)
	}
	// Explored set and NoYield reset alongside the reward.
	dct.MarkYield(root, variant)
	if !dct.Explored[root][variant] {
		t.Fatal("MarkYield did not set Explored")
	}
	if dct.NoYield[root][variant] != 0 {
		t.Fatalf("MarkYield did not reset NoYield: %v", dct.NoYield[root][variant])
	}
	// Reward is capped at maxComboWeight.
	for i := 0; i < 200; i++ {
		dct.MarkYield(root, variant)
	}
	if w := dct.WeightOf(root, variant); w > maxComboWeight {
		t.Fatalf("weight %v exceeds cap %v", w, maxComboWeight)
	}
}

// TestNoYieldDownweightFloor verifies the down-weight floor semantics: combos
// above noYieldDelta decay by noYieldDelta; combos in (1, noYieldDelta] drop
// straight to 1 on the next trigger; weight 1 is the floor.
func TestNoYieldDownweightFloor(t *testing.T) {
	dct := NewDistributedChoiceTable("fileops")
	root := "write"
	variant := CallVariant{CallName: "read", PathRelation: PathSame}
	// Pre-fill the no-yield counter so the next selection triggers a decay.
	if dct.NoYield[root] == nil {
		dct.NoYield[root] = make(map[CallVariant]int)
	}

	// w = 5 (in (1, noYieldDelta]): one trigger drops it to 1.
	dct.Weights[root][variant] = 5
	dct.NoYield[root][variant] = noYieldThreshold - 1
	dct.noYieldTick(root, variant)
	if w := dct.WeightOf(root, variant); w != 1 {
		t.Fatalf("w=5 after trigger = %v, want 1", w)
	}

	// w = 1 (floor): trigger does not drop further.
	dct.Weights[root][variant] = 1
	dct.NoYield[root][variant] = noYieldThreshold - 1
	dct.noYieldTick(root, variant)
	if w := dct.WeightOf(root, variant); w != 1 {
		t.Fatalf("w=1 after trigger = %v, want 1", w)
	}

	// w = 30 (above noYieldDelta): decays by noYieldDelta (25).
	dct.Weights[root][variant] = 30
	dct.NoYield[root][variant] = noYieldThreshold - 1
	dct.noYieldTick(root, variant)
	if w := dct.WeightOf(root, variant); w != 30-noYieldDelta {
		t.Fatalf("w=30 after trigger = %v, want %v", w, 30-noYieldDelta)
	}
}

// TestOffsetBucketOf covers the offset bucket boundaries, mirroring the HMDFS
// writeback behavior: beyond (pos >= size), tail (last partial page,
// size-r <= pos < size with r = size % 4096), zero, mid, and NA.
func TestOffsetBucketOf(t *testing.T) {
	mkV := func(fid uint32, off, size uint64) *DAGVertex {
		return &DAGVertex{FuncID: fid, Off: off, Size: size}
	}
	// Non-offset funcs are NA regardless of off/size.
	if b := offsetBucketOf(mkV(FuncMkdir, 100, 1000)); b != offsetBucketNA {
		t.Fatalf("mkdir bucket = %v, want NA", b)
	}
	// pos == 0 -> Zero (even for size 0).
	if b := offsetBucketOf(mkV(FuncWrite, 0, 1000)); b != offsetBucketZero {
		t.Fatalf("write@0 bucket = %v, want Zero", b)
	}
	// pos >= size -> Beyond.
	if b := offsetBucketOf(mkV(FuncWrite, 1000, 1000)); b != offsetBucketBeyond {
		t.Fatalf("write@size bucket = %v, want Beyond", b)
	}
	if b := offsetBucketOf(mkV(FuncWrite, 5000, 1000)); b != offsetBucketBeyond {
		t.Fatalf("write@beyond bucket = %v, want Beyond", b)
	}
	// size = 5000, r = 5000%4096 = 904, tail = [4096, 5000).
	if b := offsetBucketOf(mkV(FuncWrite, 4096, 5000)); b != offsetBucketTail {
		t.Fatalf("write@4096/5000 bucket = %v, want Tail", b)
	}
	if b := offsetBucketOf(mkV(FuncWrite, 4095, 5000)); b != offsetBucketMid {
		t.Fatalf("write@4095/5000 bucket = %v, want Mid", b)
	}
	if b := offsetBucketOf(mkV(FuncWrite, 4999, 5000)); b != offsetBucketTail {
		t.Fatalf("write@4999/5000 bucket = %v, want Tail", b)
	}
	// size page-aligned (8192, r = 0): no tail region; pos < size is Mid.
	if b := offsetBucketOf(mkV(FuncWrite, 8191, 8192)); b != offsetBucketMid {
		t.Fatalf("write@8191/8192 bucket = %v, want Mid", b)
	}
	// truncate (FuncSetattr) uses the same buckets.
	if b := offsetBucketOf(mkV(FuncSetattr, 500, 1000)); b != offsetBucketMid {
		t.Fatalf("truncate@500/1000 bucket = %v, want Mid", b)
	}
	if b := offsetBucketOf(mkV(FuncSetattr, 2000, 1000)); b != offsetBucketBeyond {
		t.Fatalf("truncate@2000/1000 bucket = %v, want Beyond", b)
	}
}

// TestFeaturesOfOffsetLayout asserts the offset bucket sits at bits 12-14 of
// the feature vector and distinct buckets produce distinct feature values.
func TestFeaturesOfOffsetLayout(t *testing.T) {
	mkV := func(fid uint32, off, size uint64) *DAGVertex {
		return &DAGVertex{FuncID: fid, Path: "merge_view/a", RetBucket: RetSuccess, Off: off, Size: size}
	}
	f0 := featuresOf(mkV(FuncWrite, 0, 1000), nil)     // Zero
	f1 := featuresOf(mkV(FuncWrite, 500, 1000), nil)   // Mid
	f2 := featuresOf(mkV(FuncWrite, 995, 1000), nil)   // Beyond? 995<1000 -> 995>=1000-1000%4096? size=1000 r=1000 -> size-r=0 -> Tail
	if f0 == f1 || f1 == f2 || f0 == f2 {
		t.Fatalf("offset buckets collide: %v %v %v", f0, f1, f2)
	}
	if got := offsetBucketOf(mkV(FuncWrite, 995, 1000)); got != offsetBucketTail {
		t.Fatalf("write@995/1000 bucket = %v, want Tail", got)
	}
	if f0>>12&7 != offsetBucketZero || f1>>12&7 != offsetBucketMid || f2>>12&7 != offsetBucketTail {
		t.Fatalf("offset bits wrong: %v %v %v", f0>>12&7, f1>>12&7, f2>>12&7)
	}
}
