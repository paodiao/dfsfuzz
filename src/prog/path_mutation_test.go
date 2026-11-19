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

// TestChooseTemporal covers the Phase 3 temporal-form selection: the learned
// weights are gone; modifier roots pick 50/50, non-modifier roots always pick
// the concurrent form.
func TestChooseTemporal(t *testing.T) {
	dct := NewDistributedChoiceTable()
	r := rand.New(rand.NewSource(1))
	variant := CallVariant{CallName: "read", PathRelation: PathSame}

	seen := map[TemporalRel]int{}
	for i := 0; i < 40; i++ {
		seen[dct.ChooseTemporal("write", variant, r)]++
	}
	if seen[TemporalConcurrent] == 0 || seen[TemporalHB] == 0 {
		t.Fatalf("modifier root 50/50 not observed: %v", seen)
	}

	// Non-modifier roots (open/read/pread64/stat/getdents64): an HB placement
	// after such a root cannot produce an HB pair of this combo.
	for i := 0; i < 40; i++ {
		if got := dct.ChooseTemporal("read", variant, r); got != TemporalConcurrent {
			t.Fatalf("non-modifier root chose %v, want concurrent", got)
		}
	}
}

// TestChooseTemporalIndependentOfVariant pins the contract the DCT insertion
// path relies on: temporal is chosen before the variant, so the draw must not
// depend on the variant argument.
func TestChooseTemporalIndependentOfVariant(t *testing.T) {
	dct := NewDistributedChoiceTable()
	readRand := rand.New(rand.NewSource(17))
	writeRand := rand.New(rand.NewSource(17))
	readVariant := CallVariant{CallName: "read", PathRelation: PathSame}
	writeVariant := CallVariant{CallName: "write", PathRelation: PathSibling}
	for i := 0; i < 40; i++ {
		gotRead := dct.ChooseTemporal("open", readVariant, readRand)
		gotWrite := dct.ChooseTemporal("open", writeVariant, writeRand)
		if gotRead != gotWrite {
			t.Fatalf("draw %d depends on variant: read=%v write=%v", i, gotRead, gotWrite)
		}
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

// TestDagRelToDctRelRenamePrePost pins the direction mapping on the same
// pre/post path basis as determinePathRel: a rename vertex's relation is
// established through its pre path (source), so a post-only check would drop
// the variant-rename Parent and root-rename Child forms (and the
// rename-rename combinations).
func TestDagRelToDctRelRenamePrePost(t *testing.T) {
	mkRename := func(dst, src string) *DAGVertex {
		return &DAGVertex{FuncID: FuncRename, CallName: "rename", Path: dst, OldPath: src}
	}
	mkCall := func(name string, fid uint32, path string) *DAGVertex {
		return &DAGVertex{CallName: name, FuncID: fid, Path: path}
	}
	cases := []struct {
		name string
		pair DAGPair
		want PathRelation
		ok   bool
	}{
		{
			// Variant rename with Parent: B src = parent of A, B dst = a
			// same-parent sibling of that parent.
			name: "variant rename Parent",
			pair: DAGPair{
				A:       mkCall("stat", FuncGetattr, "merge_view/d/a"),
				B:       mkRename("merge_view/d/sib._renamed_x", "merge_view/d"),
				PathRel: PathParentChild, Temporal: TemporalConcurrent,
			},
			want: PathParent, ok: true,
		},
		{
			// Root rename with Child: B is a child of the root's source; the
			// root destination is a fresh sibling of the source.
			name: "root rename Child",
			pair: DAGPair{
				A:       mkRename("merge_view/d/a._renamed_x", "merge_view/d/a"),
				B:       mkCall("creat", FuncCreate, "merge_view/d/a/c"),
				PathRel: PathParentChild, Temporal: TemporalConcurrent,
			},
			want: PathChild, ok: true,
		},
		{
			// Rename x rename: B src = parent of A src, the two destinations
			// are unrelated; the relation only holds through the pre paths.
			name: "rename rename Parent",
			pair: DAGPair{
				A:       mkRename("merge_view/d/a._renamed_x", "merge_view/d/a"),
				B:       mkRename("merge_view/d/._renamed_y", "merge_view/d"),
				PathRel: PathParentChild, Temporal: TemporalConcurrent,
			},
			want: PathParent, ok: true,
		},
		{
			// Unrelated rename paths stay unmapped. Defensive only: the
			// extractor never classifies unrelated paths as PathParentChild
			// (it requires an ancestry relation), so this pins the fallback
			// branch against hand-built/inconsistent inputs.
			name: "unrelated rename",
			pair: DAGPair{
				A:       mkCall("stat", FuncGetattr, "merge_view/d/a"),
				B:       mkRename("merge_view/x/b._renamed_x", "merge_view/x/b"),
				PathRel: PathParentChild, Temporal: TemporalConcurrent,
			},
			want: 0, ok: false,
		},
		{
			// Without rename the post-only and pre/post bases coincide.
			name: "plain child",
			pair: DAGPair{
				A:       mkCall("stat", FuncGetattr, "merge_view/d"),
				B:       mkCall("creat", FuncCreate, "merge_view/d/a"),
				PathRel: PathParentChild, Temporal: TemporalConcurrent,
			},
			want: PathChild, ok: true,
		},
	}
	for _, tc := range cases {
		rel, ok := dagRelToDctRel(&tc.pair)
		if ok != tc.ok || (ok && rel != tc.want) {
			t.Fatalf("%s: rel=%v ok=%v, want rel=%v ok=%v", tc.name, rel, ok, tc.want, tc.ok)
		}
	}
	// DagPairToVariant surfaces the mapped relation end to end.
	pair := cases[0].pair
	root, variant, _, ok := DagPairToVariant(&pair)
	if !ok || root != "stat" || variant.CallName != "rename" || variant.PathRelation != PathParent {
		t.Fatalf("variant rename Parent mapping: root=%v variant=%+v ok=%v", root, variant, ok)
	}
}

// TestOffsetBucketOf covers the offset bucket boundaries, mirroring the HMDFS
// writeback behavior: beyond (pos >= size), tail (last partial page,
// size-r <= pos < size with r = size % 4096), zero, mid, and NA.
func TestOffsetBucketOf(t *testing.T) {
	mkV := func(fid uint32, off, size uint64) *DAGVertex {
		name := ""
		switch fid {
		case FuncWrite:
			name = "write"
		case FuncRead:
			name = "read"
		case FuncSetattr:
			name = "truncate"
		case FuncMkdir:
			name = "mkdir"
		}
		return &DAGVertex{FuncID: fid, CallName: name, Off: off, Size: size}
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
	// chmod/utime share FuncSetattr but carry no ia_size: the name-level gate
	// must classify their off=0 as NA, not Zero (same as truncate@0).
	if b := offsetBucketOf(&DAGVertex{FuncID: FuncSetattr, CallName: "chmod", Off: 0, Size: 1000}); b != offsetBucketNA {
		t.Fatalf("chmod bucket = %v, want NA", b)
	}
	if b := offsetBucketOf(&DAGVertex{FuncID: FuncSetattr, CallName: "utime", Off: 0, Size: 1000}); b != offsetBucketNA {
		t.Fatalf("utime bucket = %v, want NA", b)
	}
}

// TestFeaturesOfOffsetLayout asserts the offset bucket sits at bits 12-14 of
// the feature vector and distinct buckets produce distinct feature values.
func TestFeaturesOfOffsetLayout(t *testing.T) {
	mkV := func(fid uint32, off, size uint64) *DAGVertex {
		return &DAGVertex{FuncID: fid, CallName: "write", Path: "merge_view/a", RetBucket: RetSuccess, Off: off, Size: size}
	}
	f0 := featuresOf(mkV(FuncWrite, 0, 1000), nil)   // Zero
	f1 := featuresOf(mkV(FuncWrite, 500, 1000), nil) // Mid
	f2 := featuresOf(mkV(FuncWrite, 995, 1000), nil) // Beyond? 995<1000 -> 995>=1000-1000%4096? size=1000 r=1000 -> size-r=0 -> Tail
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

// TestProgTimingsRoundTrip covers the corpus timeline transfer added for
// fuzzer restarts: ProgTimings extracts per-call [Stime, Etime, Retv] triples
// (0/0/0 for calls without CheckInfo) and ApplyProgTimings restores them on a
// fresh copy, leaving missing calls nil, never overwriting an existing
// CheckInfo and ignoring shape mismatches.
func TestProgTimingsRoundTrip(t *testing.T) {
	target := smokeTarget(t)
	mk := func(stime, etime uint64, retv int64) *Call {
		return &Call{CheckInfo: &FileMetadata{Stime: stime, Etime: etime, Retv: retv}}
	}
	ps := []*Prog{{
		Target: target,
		Calls:  []*Call{mk(100, 200, 0), {}, mk(300, 400, 0)},
	}}
	timings := ProgTimings(ps)
	if len(timings) != 1 || len(timings[0]) != 9 {
		t.Fatalf("ProgTimings shape = %v, want 1x9", timings)
	}
	want := []uint64{100, 200, 0, 0, 0, 0, 300, 400, 0}
	for i, v := range want {
		if timings[0][i] != v {
			t.Fatalf("timings[0][%d] = %d, want %d", i, timings[0][i], v)
		}
	}

	fresh := &Prog{Target: target, Calls: []*Call{{}, {}, {}}}
	if n := ApplyProgTimings([]*Prog{fresh}, timings); n != 2 {
		t.Fatalf("ApplyProgTimings restored %d calls, want 2", n)
	}
	if ci := fresh.Calls[0].CheckInfo; ci == nil || ci.Stime != 100 || ci.Etime != 200 || ci.Retv != 0 {
		t.Fatalf("call0 CheckInfo not restored: %+v", ci)
	}
	if fresh.Calls[1].CheckInfo != nil {
		t.Fatal("0/0/0 triple must leave CheckInfo nil")
	}
	if ci := fresh.Calls[2].CheckInfo; ci == nil || ci.Stime != 300 || ci.Etime != 400 || ci.Retv != 0 {
		t.Fatalf("call2 CheckInfo not restored: %+v", ci)
	}

	// Existing CheckInfo is never overwritten: the prog matches the payload
	// shape, so the per-call guard (not the shape guard) is what skips call 0.
	keep := &Prog{Target: target, Calls: []*Call{mk(1, 2, 0), {}, {}}}
	if n := ApplyProgTimings([]*Prog{keep}, timings); n != 1 {
		t.Fatalf("existing CheckInfo restored count = %d, want 1", n)
	}
	if ci := keep.Calls[0].CheckInfo; ci == nil || ci.Stime != 1 || ci.Etime != 2 {
		t.Fatalf("ApplyProgTimings overwrote an existing CheckInfo: %+v", ci)
	}
	if ci := keep.Calls[2].CheckInfo; ci == nil || ci.Stime != 300 || ci.Etime != 400 {
		t.Fatalf("call2 CheckInfo not restored: %+v", ci)
	}
	// A short (mismatched) payload is ignored.
	if n := ApplyProgTimings([]*Prog{fresh}, [][]uint64{{1, 2}}); n != 0 {
		t.Fatalf("short payload restored count = %d, want 0", n)
	}
	if fresh.Calls[0].CheckInfo.Stime != 100 || fresh.Calls[0].CheckInfo.Etime != 200 {
		t.Fatal("short timing payload must be ignored")
	}
	// A longer payload must not restore by index (misaligned layout).
	long := &Prog{Target: target, Calls: []*Call{{}}}
	if n := ApplyProgTimings([]*Prog{long}, [][]uint64{{5, 6, 7, 8}}); n != 0 {
		t.Fatalf("long payload restored count = %d, want 0", n)
	}
	if long.Calls[0].CheckInfo != nil {
		t.Fatal("long payload must not restore a misaligned CheckInfo")
	}

	// Failed calls keep their negative Retv across the transfer.
	neg := int64(-17)
	failPs := []*Prog{{Target: target, Calls: []*Call{mk(500, 600, neg)}}}
	failTimings := ProgTimings(failPs)
	if len(failTimings) != 1 || len(failTimings[0]) != 3 || failTimings[0][2] != uint64(neg) {
		t.Fatalf("failed-call timings = %v, want [500 600 %d]", failTimings, uint64(neg))
	}
	restoredFail := &Prog{Target: target, Calls: []*Call{{}}}
	if n := ApplyProgTimings([]*Prog{restoredFail}, failTimings); n != 1 {
		t.Fatalf("failed-call restored count = %d, want 1", n)
	}
	if ci := restoredFail.Calls[0].CheckInfo; ci == nil || ci.Retv != neg {
		t.Fatalf("failed-call Retv not restored: %+v", ci)
	}
}
