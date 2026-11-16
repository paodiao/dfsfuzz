package prog

import (
	"math/rand"
	"testing"
)

// coverBoardCellsExcept marks every uncovered producible cell of the global
// board as discovered except the ones selected by keep, mirroring the
// recordPairs accounting (found counts a cell on first sight only). It is the
// test-side lever for forcing the directed mutation arms onto one target.
func coverBoardCellsExcept(t *testing.T, keep func(ck comboKey, c cellKey) bool) {
	t.Helper()
	covBoard.mu.Lock()
	defer covBoard.mu.Unlock()
	for ck := range covBoard.producible {
		for _, rb := range producibleRets(ck.Variant) {
			for _, off := range producibleOffBucketsForRet(ck.Variant, rb) {
				for _, isDir := range producibleTypes(ck.Variant) {
					for d := 0; d < 4; d++ {
						if !covBoard.depths[d] {
							continue
						}
						persists := [2]bool{false}
						np := 1
						if covBoard.persistDir != "" && uint64(d) >= covBoard.persistBucket {
							persists[1] = true
							np = 2
						}
						for pi := 0; pi < np; pi++ {
							for tm := 0; tm < 2; tm++ {
								c := cellKey{
									Root: ck.Root, Variant: ck.Variant, Rel: ck.Rel,
									Temporal: TemporalRel(tm), RetA: RetSuccess, RetB: rb,
									OffB: off, PersistB: persists[pi], DepthB: uint64(d), TypeB: isDir,
								}
								if keep(ck, c) {
									continue
								}
								if covBoard.discovered[c] == 0 {
									f := covBoard.found[ck]
									f[tm]++
									covBoard.found[ck] = f
									// HMDFS plannable == model; keep both ledgers in
									// sync with what recordPairs would do.
									pf := covBoard.plannableFound[ck]
									pf[tm]++
									covBoard.plannableFound[ck] = pf
								}
								covBoard.discovered[c]++
							}
						}
					}
				}
			}
		}
	}
}

// TestRealizeDirectedTruncateLenDegenerate pins the degenerate-size handling
// of the truncate length realizer: size 0 must not underflow (the old
// size-1 wrap reached r.Intn(-1) and panicked) and sizes without a tail page
// must decline the Mid bucket.
func TestRealizeDirectedTruncateLenDegenerate(t *testing.T) {
	r := &randGen{Rand: rand.New(rand.NewSource(1))}
	check := func(size, bucket uint64, wantOK bool) uint64 {
		t.Helper()
		l, ok := realizeDirectedTruncateLen(r, size, bucket)
		if ok != wantOK {
			t.Fatalf("realizeDirectedTruncateLen(size=%d, bucket=%d) ok=%v, want %v", size, bucket, ok, wantOK)
		}
		if ok && bucketOfOffset(size, l) != bucket {
			t.Fatalf("size=%d bucket=%d realized len=%d -> bucket %d", size, bucket, l, bucketOfOffset(size, l))
		}
		return l
	}
	check(0, offsetBucketZero, true)
	if l := check(0, offsetBucketBeyond, true); l == 0 {
		t.Fatalf("size 0 beyond length must be positive")
	}
	check(0, offsetBucketMid, false) // regression: used to panic
	check(0, offsetBucketTail, false)
	check(1, offsetBucketMid, false)
	check(1000, offsetBucketMid, false) // below one page: no mid region
	check(8192, offsetBucketMid, true)  // page multiple: mid is [1, size-1]
	check(10000, offsetBucketMid, true)
	check(10000, offsetBucketTail, true)
	check(8192, offsetBucketTail, false)
}

// TestMutateGroupDataDirectedZeroSizeMid forces the Mid bucket on a zero-size
// truncate group: the directed arm must decline (fall back) instead of
// panicking in the length realizer.
func TestMutateGroupDataDirectedZeroSizeMid(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Variant == "truncate" && c.OffB == offsetBucketMid
	})
	lcs := &LayeredChoiceStrategy{FileTree: directedTestTree(0)}
	for seed := int64(0); seed < 20; seed++ {
		tr0 := mkTruncate(target, "merge_view/dirA/a", 0)
		tr0.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		tr1 := mkTruncate(target, "merge_view/dirA/a", 0)
		tr1.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
		ps := []*Prog{
			{Target: target, Calls: []*Call{tr0}},
			{Target: target, Calls: []*Call{tr1}},
		}
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if mutateGroupDataDirected(ps, lcs, r) {
			t.Fatalf("seed %d: unrealizable Mid bucket at size 0 must decline", seed)
		}
	}
}

// TestUncoveredDimPots pins the per-dimension accessors: recording one pair
// decrements exactly its offset bucket and depth bucket, and combos outside
// the producible grid stay zero.
func TestUncoveredDimPots(t *testing.T) {
	newTestCoverageBoard(t)
	beforeOff := covBoard.uncoveredOffPots([]string{"pwrite64"})
	if beforeOff[offsetBucketMid] == 0 {
		t.Fatalf("fixture has no uncovered Mid cells: %v", beforeOff)
	}
	beforeDepth := covBoard.uncoveredDepthPots("open", "pwrite64", PathSame)
	beforePot := CoveragePotential("open", "pwrite64", PathSame)

	p := DAGPair{
		A: &DAGVertex{CallName: "open", Path: "merge_view/dirA/a",
			RetBucket: RetSuccess, FuncID: FuncFileOpen},
		B: &DAGVertex{CallName: "pwrite64", Path: "merge_view/dirA/a",
			RetBucket: RetSuccess, FuncID: FuncWrite, Size: 10000, Off: 1000},
		Temporal: TemporalConcurrent,
		PathRel:  PathSamePath,
	}
	RecordCoveragePairs([]DAGPair{p})

	afterOff := covBoard.uncoveredOffPots([]string{"pwrite64"})
	if got, want := afterOff[offsetBucketMid], beforeOff[offsetBucketMid]-1; got != want {
		t.Fatalf("Mid pot = %d, want %d (before %v)", got, want, beforeOff)
	}
	for b := offsetBucketZero; b <= offsetBucketBeyond; b++ {
		if b == offsetBucketMid {
			continue
		}
		if afterOff[b] != beforeOff[b] {
			t.Fatalf("bucket %d changed: %d -> %d", b, beforeOff[b], afterOff[b])
		}
	}
	// The recorded cell's path sits at depth bucket 2.
	afterDepth := covBoard.uncoveredDepthPots("open", "pwrite64", PathSame)
	if got, want := afterDepth[2], beforeDepth[2]-1; got != want {
		t.Fatalf("depth-2 pot = %d, want %d (before %v)", got, want, beforeDepth)
	}
	if afterDepth[1] != beforeDepth[1] || afterDepth[3] != beforeDepth[3] {
		t.Fatalf("other depth pots changed: %v -> %v", beforeDepth, afterDepth)
	}
	if got := CoveragePotential("open", "pwrite64", PathSame); got != beforePot-1 {
		t.Fatalf("combo potential = %d, want %d", got, beforePot-1)
	}
	if pots := covBoard.uncoveredDepthPots("open", "pwrite64", PathNoRel); pots != [4]int{} {
		t.Fatalf("NoRel pots = %v, want zero (outside the producible grid)", pots)
	}
}

// TestUncoveredOffPotsSkipENOENT pins the ENOENT filter of the offset
// aggregation: with every Success truncate cell covered and only the ENOENT
// cells left, the offset pots must be all zero (a failed lookup never carries
// an offset bucket), while the depth margin still counts those cells (the
// lookup path is real).
func TestUncoveredOffPotsSkipENOENT(t *testing.T) {
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Variant == "truncate" && c.RetB == RetENOENT
	})
	if pots := covBoard.uncoveredOffPots([]string{"truncate"}); pots != [5]int{} {
		t.Fatalf("ENOENT cells leaked into the offset pots: %v", pots)
	}
	if pots := covBoard.uncoveredDepthPots("open", "truncate", PathSame); pots == [4]int{} {
		t.Fatal("ENOENT cells must still count toward the depth margin")
	}
}

// directedTestTree is the smoke tree with a nonzero file size, so Mid/Tail
// offset buckets are realizable.
func directedTestTree(size uint64) *FileTree {
	ft := smokeFileTree()
	if n := ft.FindNode("merge_view/dirA/a"); n != nil {
		n.Size = size
	}
	return ft
}

// TestMutateGroupDataDirected forces the data arm onto the Mid bucket (all
// other pwrite64 cells covered) and checks the shared realized offset.
func TestMutateGroupDataDirected(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Variant == "pwrite64" && c.OffB == offsetBucketMid
	})
	lcs := &LayeredChoiceStrategy{FileTree: directedTestTree(10000)}

	done := false
	for seed := int64(0); seed < 50 && !done; seed++ {
		ps := smokePs(target, true)
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if !mutateGroupDataDirected(ps, lcs, r) {
			continue
		}
		done = true
		off0 := ps[0].Calls[1].Args[3].(*ConstArg).Val
		off1 := ps[1].Calls[1].Args[3].(*ConstArg).Val
		if off0 != off1 {
			t.Fatalf("shared offsets differ: %v vs %v", off0, off1)
		}
		if b := bucketOfOffset(10000, off0); b != offsetBucketMid {
			t.Fatalf("offset bucket = %d, want Mid (off=%v)", b, off0)
		}
	}
	if !done {
		t.Fatal("no seed produced a directed data mutation")
	}
}

// TestMutateGroupDataDirectedFallback checks that a fully covered board makes
// the directed arm decline (the random arm stays available).
func TestMutateGroupDataDirectedFallback(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(comboKey, cellKey) bool { return false })
	lcs := &LayeredChoiceStrategy{FileTree: directedTestTree(10000)}
	for seed := int64(0); seed < 20; seed++ {
		ps := smokePs(target, true)
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if mutateGroupDataDirected(ps, lcs, r) {
			t.Fatal("directed data mutation succeeded with no uncovered cells")
		}
	}
}

// TestMutateGroupDataDirectedTruncate forces the Beyond bucket on a
// truncate-only group (no explicit offset argument; the length realizes it).
func TestMutateGroupDataDirectedTruncate(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Variant == "truncate" && c.OffB == offsetBucketBeyond
	})
	lcs := &LayeredChoiceStrategy{FileTree: directedTestTree(10000)}

	tr0 := mkTruncate(target, "merge_view/dirA/a", 100000)
	tr0.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	p0 := &Prog{Target: target, Calls: []*Call{tr0}}
	tr1 := mkTruncate(target, "merge_view/dirA/a", 100000)
	tr1.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	p1 := &Prog{Target: target, Calls: []*Call{tr1}}
	ps := []*Prog{p0, p1}

	if !mutateGroupDataDirected(ps, lcs, smokeRand(target)) {
		t.Fatal("directed truncate mutation declined")
	}
	len0 := ps[0].Calls[0].Args[1].(*ConstArg).Val
	len1 := ps[1].Calls[0].Args[1].(*ConstArg).Val
	if len0 != len1 {
		t.Fatalf("shared truncate lengths differ: %v vs %v", len0, len1)
	}
	if b := bucketOfOffset(10000, len0); b != offsetBucketBeyond {
		t.Fatalf("truncate length bucket = %d, want Beyond (len=%v)", b, len0)
	}
}

// TestMutateGroupPathDirected forces one (relation, depth) target: the member
// truncate must be re-derived as a depth-2 child of the moved anchor, while
// the group's other member keeps its previous relation. The target combo needs
// a modifier side for the producible model (truncate is one; open is not).
func TestMutateGroupPathDirected(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Root == "open" && ck.Variant == "truncate" &&
			ck.Rel == PathChild && c.DepthB == 2
	})
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	done := false
	for seed := int64(0); seed < 50 && !done; seed++ {
		openA := mkOpen(target, "merge_view/dirA/a")
		// O_RDONLY: a read-only open may legally anchor a directory Child
		// target (an O_RDWR open cannot open directories).
		openA.Args[1].(*ConstArg).Val = 0
		openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		p0 := &Prog{Target: target, Calls: []*Call{openA, mkClose(target, openA)}}

		openB := mkOpen(target, "merge_view/dirA/a")
		openB.Args[1].(*ConstArg).Val = 0
		openB.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
		trC := mkTruncate(target, "merge_view/x/y/z", 1000)
		trC.CheckInfo = &FileMetadata{Stime: 300, Etime: 400}
		p1 := &Prog{Target: target, Calls: []*Call{openB, trC}}

		ps := []*Prog{p0, p1}
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if !mutateGroupPathDirected(ps, lcs, r) {
			continue
		}
		done = true
		anchorPath := callPath(ps[0].Calls[0])
		otherPath := callPath(ps[1].Calls[0])
		memberPath := callPath(ps[1].Calls[1])
		if DepthBucketOf(anchorPath) != 1 {
			t.Fatalf("anchor path %q not at depth bucket 1", anchorPath)
		}
		if otherPath != anchorPath {
			t.Fatalf("unselected member (Same rel) path %q != anchor %q", otherPath, anchorPath)
		}
		if GetParentDir(memberPath) != anchorPath {
			t.Fatalf("member path %q is not a direct child of anchor %q", memberPath, anchorPath)
		}
		if DepthBucketOf(memberPath) != 2 {
			t.Fatalf("member path %q not at depth bucket 2", memberPath)
		}
		for _, p := range ps {
			if p.HasBrokenRefs() {
				t.Fatalf("broken refs after directed path mutation")
			}
		}
	}
	if !done {
		t.Fatal("no seed produced a directed path mutation")
	}
}

// TestMutateGroupPathDirectedFallback checks the fully covered board declines.
func TestMutateGroupPathDirectedFallback(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(comboKey, cellKey) bool { return false })
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	for seed := int64(0); seed < 20; seed++ {
		openA := mkOpen(target, "merge_view/dirA/a")
		openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		openB := mkOpen(target, "merge_view/dirA/a")
		openB.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
		ps := []*Prog{
			{Target: target, Calls: []*Call{openA}},
			{Target: target, Calls: []*Call{openB}},
		}
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if mutateGroupPathDirected(ps, lcs, r) {
			t.Fatal("directed path mutation succeeded with no uncovered cells")
		}
	}
}

// TestPathAnchorBucket pins the anchor-bucket arithmetic, including the
// Parent x bucket-3 regression (it used to compute 4 and reject a reachable
// target).
func TestPathAnchorBucket(t *testing.T) {
	cases := []struct {
		rel  PathRelation
		t    int
		want int
	}{
		{PathParent, 3, 3}, // regression: used to be rejected (4)
		{PathParent, 1, 2},
		{PathParent, 2, 2},
		{PathChild, 1, 0},
		{PathChild, 2, 1},
		{PathChild, 3, 3},
		{PathSame, 2, 2},
		{PathSibling, 3, 3},
	}
	for _, c := range cases {
		if got := pathAnchorBucket(c.rel, c.t); got != c.want {
			t.Errorf("pathAnchorBucket(%v, %d) = %d, want %d", c.rel, c.t, got, c.want)
		}
	}
}

// mkMkdir builds mkdir(path, mode).
func mkMkdir(target *Target, path string) *Call {
	meta := target.SyscallMap["mkdir"]
	ptrType := meta.Args[0].Type.(*PtrType)
	modeType := meta.Args[1].Type.(*FlagsType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr, MakeConstArg(modeType, DirIn, 0o755)}
	target.assignSizesCall(c)
	return c
}

// TestMutateGroupPathDirectedSameTypeFilter checks that the arm declines when
// a dir-only anchor (mkdir) shares a Same path with a file-only member
// (creat): no single path type can satisfy both sides. The pair is guarded
// twice -- the Same type pre-filter (mutation.go) drops the candidate up
// front, and the downstream pathSatisfiesType re-check would reject the
// realized path anyway -- so this test pins the combined behavior, not the
// pre-filter in isolation.
func TestMutateGroupPathDirectedSameTypeFilter(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Root == "mkdir" && ck.Variant == "creat" &&
			ck.Rel == PathSame && c.DepthB == 2
	})
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	for seed := int64(0); seed < 20; seed++ {
		mk := mkMkdir(target, "merge_view/dirA")
		mk.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		cr := mkPathCall(t, target, "creat", "merge_view/dirA", 150, 250)
		ps := []*Prog{
			{Target: target, Calls: []*Call{mk}},
			{Target: target, Calls: []*Call{cr}},
		}
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if mutateGroupPathDirected(ps, lcs, r) {
			t.Fatal("Same relation with incompatible types must be filtered out")
		}
	}
}

// TestMutateGroupPathDirectedSameType checks the Any-anchor refinement: for
// Same with a file-only member the anchor must be pinned to a file path.
func TestMutateGroupPathDirectedSameType(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Root == "open" && ck.Variant == "truncate" &&
			ck.Rel == PathSame && c.DepthB == 2
	})
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	done := false
	for seed := int64(0); seed < 50 && !done; seed++ {
		openA := mkOpen(target, "merge_view/dirA/a")
		// O_RDONLY: exercise the Any-anchor Same refinement (pinning the
		// anchor to the file-only member's type).
		openA.Args[1].(*ConstArg).Val = 0
		openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		p0 := &Prog{Target: target, Calls: []*Call{openA}}
		tr := mkTruncate(target, "merge_view/dirA/a", 1000)
		tr.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
		p1 := &Prog{Target: target, Calls: []*Call{tr}}

		ps := []*Prog{p0, p1}
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if !mutateGroupPathDirected(ps, lcs, r) {
			continue
		}
		done = true
		anchorPath := callPath(ps[0].Calls[0])
		memberPath := callPath(ps[1].Calls[0])
		if memberPath != anchorPath {
			t.Fatalf("Same target must share the path: anchor=%q member=%q", anchorPath, memberPath)
		}
		if n := lcs.FileTree.FindNode(anchorPath); n == nil || n.Type != NodeTypeFile {
			t.Fatalf("Any anchor pinned to a file member must land on a file: %q", anchorPath)
		}
		if DepthBucketOf(anchorPath) != 2 {
			t.Fatalf("path %q not at depth bucket 2", anchorPath)
		}
		for _, p := range ps {
			if p.HasBrokenRefs() {
				t.Fatalf("broken refs after directed path mutation")
			}
		}
	}
	if !done {
		t.Fatal("no seed produced a directed Same mutation")
	}
}

// TestResolveFdTargetGetdents64 pins the fd resolution of getdents64: its
// arg0 is an fd, so a group path mutation must move the embedded open (same
// as read/write) instead of silently no-op'ing on the getdents call.
func TestResolveFdTargetGetdents64(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	sCalls := hmdfsSmokeSpecialCalls(t, target)
	r := newRand(target, rand.NewSource(1))
	s := newState(target, nil, nil)
	calls := r.generateGetdents64Calls(s, sCalls, "merge_view/dirA")
	if len(calls) < 2 {
		t.Fatalf("getdents64 sequence too short: %d", len(calls))
	}
	ps := []*Prog{{Target: target, Calls: calls}}
	idx := -1
	for i, c := range calls {
		if c != nil && c.Meta.CallName == "getdents64" {
			idx = i
			break
		}
	}
	if idx <= 0 {
		t.Fatalf("getdents64 call not found or first: idx=%d", idx)
	}
	got := resolveFdTarget(ps, GroupPosition{ProgIdx: 0, CallIdx: idx})
	if got.ProgIdx != 0 || got.CallIdx != 0 {
		t.Fatalf("resolveFdTarget(getdents64) = %+v, want the embedded open at index 0", got)
	}
}

// TestGetFdFilePathForCallGetdents64 pins the path resolution of getdents64:
// its arg0 is an fd, so getFdFilePathForCall/pathOfCall must resolve the
// embedded open (extractPathFromCall returns "" for the fd arg) and the group
// classifier must see the real relation (Same), not NoRel.
func TestGetFdFilePathForCallGetdents64(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	sCalls := hmdfsSmokeSpecialCalls(t, target)
	r := newRand(target, rand.NewSource(1))
	s := newState(target, nil, nil)
	calls := r.generateGetdents64Calls(s, sCalls, "merge_view/dirA")
	if len(calls) < 2 {
		t.Fatalf("getdents64 sequence too short: %d", len(calls))
	}
	idx := -1
	for i, c := range calls {
		if c != nil && c.Meta.CallName == "getdents64" {
			idx = i
			break
		}
	}
	if idx <= 0 {
		t.Fatalf("getdents64 call not found or first: idx=%d", idx)
	}

	ps := []*Prog{{Target: target, Calls: calls}}
	pos := GroupPosition{ProgIdx: 0, CallIdx: idx}
	if got := getFdFilePathForCall(ps, pos); got != "merge_view/dirA" {
		t.Fatalf("getFdFilePathForCall(getdents64) = %q, want %q", got, "merge_view/dirA")
	}
	if got := pathOfCall(ps, pos); got != "merge_view/dirA" {
		t.Fatalf("pathOfCall(getdents64) = %q, want %q", got, "merge_view/dirA")
	}
	if rel := pathRelBetween("merge_view/dirA", pathOfCall(ps, pos)); rel != PathSame {
		t.Fatalf("pathRelBetween with resolved getdents64 path = %v, want PathSame", rel)
	}

	// Integration: the group classifier must attach the real path/relation to
	// the getdents64 member instead of PathNoRel.
	anchor := mkOpen(target, "merge_view/dirA")
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	for _, c := range calls {
		c.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	}
	ps2 := []*Prog{{Target: target, Calls: []*Call{anchor}}, {Target: target, Calls: calls}}
	members := findConcurrentCalls(ps2, GroupPosition{}, "merge_view/dirA", nil)
	found := false
	for _, m := range members {
		if m.Pos.ProgIdx == 1 && m.Pos.CallIdx == idx {
			found = true
			if m.Path != "merge_view/dirA" || m.Rel != PathSame {
				t.Fatalf("getdents64 member = {Path:%q Rel:%v}, want {merge_view/dirA Same}", m.Path, m.Rel)
			}
		}
	}
	if !found {
		t.Fatal("getdents64 call not classified as a concurrent member")
	}
}
