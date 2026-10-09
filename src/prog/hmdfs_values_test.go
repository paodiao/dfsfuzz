package prog

import (
	"math/rand"
	"testing"
)

func valueRand(seed int64) *randGen {
	return &randGen{Rand: rand.New(rand.NewSource(seed))}
}

// TestRandHmdfsOffsetBuckets checks that the bucket-first offset chooser
// covers the offsetBucketOf classes and that the tail/mid length coupling
// keeps the intended bucket semantics.
func TestRandHmdfsOffsetBuckets(t *testing.T) {
	r := valueRand(7)
	const size = 10000 // 10000 % 4096 = 1808 -> tail = [8192, 10000)
	seen := map[uint64]int{}
	for i := 0; i < 3000; i++ {
		off := r.randHmdfsOffset(size)
		b := bucketOfOffset(size, off)
		seen[b]++
		length := r.pickHmdfsLenAt(size, off, 8192)
		if length == 0 {
			t.Fatalf("zero length at offset %v", off)
		}
		switch b {
		case offsetBucketTail:
			if off+length <= size {
				t.Fatalf("tail write does not cross EOF: off=%v len=%v size=%v", off, length, size)
			}
		case offsetBucketMid:
			if off+length > size {
				t.Fatalf("mid write crosses EOF: off=%v len=%v size=%v", off, length, size)
			}
		}
	}
	for _, b := range []uint64{offsetBucketZero, offsetBucketMid, offsetBucketTail, offsetBucketBeyond} {
		if seen[b] == 0 {
			t.Fatalf("bucket %v never produced (seen=%v)", b, seen)
		}
	}
}

// TestRandHmdfsOffsetNoTailOnPageMultiple: a page-multiple file has no tail
// region, so the tail layer must never be produced.
func TestRandHmdfsOffsetNoTailOnPageMultiple(t *testing.T) {
	r := valueRand(11)
	const size = 8192
	for i := 0; i < 2000; i++ {
		if b := bucketOfOffset(size, r.randHmdfsOffset(size)); b == offsetBucketTail {
			t.Fatalf("tail produced for page-multiple size %v", size)
		}
	}
}

// TestRandHmdfsOffsetDifferent: the three-way mix keeps overlapping picks
// around the anchor, different-bucket picks, and free picks.
func TestRandHmdfsOffsetDifferent(t *testing.T) {
	r := valueRand(13)
	const size = 65536
	anchor := uint64(30000)
	sameBucket, diffBucket, overlap := 0, 0, 0
	for i := 0; i < 3000; i++ {
		off := r.randHmdfsOffsetDifferent(size, anchor, 4096)
		if bucketOfOffset(size, off) == bucketOfOffset(size, anchor) {
			sameBucket++
		} else {
			diffBucket++
		}
		if off < anchor+4096 && anchor < off+4096 {
			overlap++
		}
	}
	if sameBucket == 0 || diffBucket == 0 || overlap == 0 {
		t.Fatalf("mix arms missing: same=%d diff=%d overlap=%d", sameBucket, diffBucket, overlap)
	}
	if diffBucket < 300 {
		t.Fatalf("different-bucket picks too rare: %d/3000", diffBucket)
	}
}

// TestPickMutationLengthSizeRelative: a known size must produce size-relative
// boundary values; size 0 keeps the legacy table.
func TestPickMutationLengthSizeRelative(t *testing.T) {
	r := valueRand(17)
	const size = 12345
	sawRelative := false
	for i := 0; i < 2000; i++ {
		v := pickMutationLength(r, size)
		if v == 0 || v > lenBoundaryCap {
			t.Fatalf("length out of range: %v", v)
		}
		switch v {
		case size - 1, size, size + 1, size / 2, 2 * size:
			sawRelative = true
		}
	}
	if !sawRelative {
		t.Fatalf("no size-relative length produced")
	}
	for i := 0; i < 200; i++ {
		if v := pickMutationLength(r, 0); v == 0 {
			t.Fatalf("legacy length must be non-zero")
		}
	}
}

// TestClampLenToBucket pins the tail/mid coupling.
func TestClampLenToBucket(t *testing.T) {
	const size = 10000
	if got := clampLenToBucket(size, 9900, 1, offsetBucketTail); got < 101 {
		t.Fatalf("tail length not lifted to cross EOF: %v", got)
	}
	if got := clampLenToBucket(size, 5000, 8192, offsetBucketMid); got != 5000 {
		t.Fatalf("mid length not clamped to remaining bytes: %v", got)
	}
	if got := clampLenToBucket(size, 12000, 777, offsetBucketBeyond); got != 777 {
		t.Fatalf("beyond length must stay unchanged: %v", got)
	}
}

func mkOpenWithFlags(target *Target, path string, flags uint64) *Call {
	meta := target.SyscallMap["open"]
	ptrType := meta.Args[0].Type.(*PtrType)
	flagsType := meta.Args[1].Type.(*FlagsType)
	modeType := meta.Args[2].Type.(*FlagsType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr, MakeConstArg(flagsType, DirIn, flags), MakeConstArg(modeType, DirIn, 0o666)}
	target.assignSizesCall(c)
	return c
}

func mkCreat(target *Target, path string) *Call {
	meta := target.SyscallMap["creat"]
	ptrType := meta.Args[0].Type.(*PtrType)
	modeType := meta.Args[1].Type.(*FlagsType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr, MakeConstArg(modeType, DirIn, 0o666)}
	target.assignSizesCall(c)
	return c
}

// TestFdCapabilityModel checks the fd capability decoding, the call
// requirement mapping and the capability-aware fd reuse.
func TestFdCapabilityModel(t *testing.T) {
	target := smokeTarget(t)

	rdonly := mkOpenWithFlags(target, "merge_view/a", 0)
	if got := fdCapOf(target, rdonly); got != fdCapRead {
		t.Fatalf("O_RDONLY cap = %v, want read", got)
	}
	rdwr := mkOpenWithFlags(target, "merge_view/b", 2)
	if got := fdCapOf(target, rdwr); got != fdCapRead|fdCapWrite {
		t.Fatalf("O_RDWR cap = %v, want read|write", got)
	}
	cr := mkCreat(target, "merge_view/c")
	if got := fdCapOf(target, cr); got != fdCapWrite {
		t.Fatalf("creat cap = %v, want write", got)
	}
	if got := capNeeded("fsync"); got != 0 {
		t.Fatalf("capNeeded(fsync) = %v, want 0 (any file fd)", got)
	}
	if got := capNeeded("getdents64"); got != fdCapDir {
		t.Fatalf("capNeeded(getdents64) = %v", got)
	}

	p := &Prog{Target: target, Calls: []*Call{rdonly}}
	if fd := findOpenFdForPath(p, "merge_view/a", 1, fdCapWrite); fd != nil {
		t.Fatalf("read-only fd reused for a write")
	}
	if fd := findOpenFdForPath(p, "merge_view/a", 1, fdCapRead); fd == nil {
		t.Fatalf("read-only fd not offered for a read")
	}
	// fsync-style lookups (want 0) accept any file fd, including O_RDONLY.
	if fd := findOpenFdForPath(p, "merge_view/a", 1, 0); fd == nil {
		t.Fatalf("any-capability lookup rejected a read-only fd")
	}
	// A conjunctive read+write requirement must reject O_RDONLY and accept O_RDWR.
	if fd := findOpenFdForPath(p, "merge_view/a", 1, fdCapRead|fdCapWrite); fd != nil {
		t.Fatalf("read+write lookup accepted a read-only fd")
	}
	prdwr := &Prog{Target: target, Calls: []*Call{rdwr}}
	if fd := findOpenFdForPath(prdwr, "merge_view/b", 1, fdCapRead|fdCapWrite); fd == nil {
		t.Fatalf("read+write lookup rejected an O_RDWR fd")
	}

	pc := &Prog{Target: target, Calls: []*Call{cr}}
	if fd := findOpenFdForPath(pc, "merge_view/c", 1, fdCapRead); fd != nil {
		t.Fatalf("creat fd reused for a read")
	}
	if fd := findOpenFdForPath(pc, "merge_view/c", 1, fdCapWrite); fd == nil {
		t.Fatalf("creat fd not offered for a write")
	}
}

// TestRandHmdfsOffsetBucketDistribution pins the layer->bucket mapping that a
// single shifted constant would silently break.
func TestRandHmdfsOffsetBucketDistribution(t *testing.T) {
	r := valueRand(23)
	const size = 10000 // tail = [8192, 10000), mid = [1, 8191]
	counts := map[uint64]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		counts[bucketOfOffset(size, r.randHmdfsOffset(size))]++
	}
	check := func(bucket uint64, lo, hi float64) {
		t.Helper()
		pct := 100 * float64(counts[bucket]) / float64(n)
		if pct < lo || pct > hi {
			t.Fatalf("bucket %v share %.1f%% out of [%.1f,%.1f] (counts=%v)", bucket, pct, lo, hi, counts)
		}
	}
	// free (5%) spreads over mid/tail/beyond: ~15 / 35.5 / 25.1 / 24.3.
	check(offsetBucketZero, 11, 19)
	check(offsetBucketMid, 31, 40)
	check(offsetBucketTail, 21, 29)
	check(offsetBucketBeyond, 20, 29)
}

func TestRandHmdfsOffsetSmallFiles(t *testing.T) {
	// size < 2 pages: mid is empty, its weight folds into tail.
	r := valueRand(29)
	const size = 1000
	counts := map[uint64]int{}
	for i := 0; i < 10000; i++ {
		counts[bucketOfOffset(size, r.randHmdfsOffset(size))]++
	}
	if counts[offsetBucketMid] != 0 {
		t.Fatalf("mid bucket produced for size %d: %v", size, counts)
	}
	if counts[offsetBucketTail] < 5000 {
		t.Fatalf("tail weight (mid folded in) too low: %v", counts)
	}
	// size == 1: only zero/beyond exist.
	r2 := valueRand(31)
	counts2 := map[uint64]int{}
	for i := 0; i < 5000; i++ {
		counts2[bucketOfOffset(1, r2.randHmdfsOffset(1))]++
	}
	if counts2[offsetBucketMid] != 0 || counts2[offsetBucketTail] != 0 {
		t.Fatalf("degenerate size 1 produced mid/tail: %v", counts2)
	}
	if pct := 100 * float64(counts2[offsetBucketZero]) / 5000; pct < 68 || pct > 82 {
		t.Fatalf("size 1 zero share %.1f%% out of [68,82]", pct)
	}
}

func TestRandHmdfsOffsetPageMultiple(t *testing.T) {
	r := valueRand(37)
	const size = 8192 // no tail region
	counts := map[uint64]int{}
	for i := 0; i < 10000; i++ {
		counts[bucketOfOffset(size, r.randHmdfsOffset(size))]++
	}
	if counts[offsetBucketTail] != 0 {
		t.Fatalf("tail produced for page-multiple size: %v", counts)
	}
	if pct := 100 * float64(counts[offsetBucketMid]) / 10000; pct < 55 || pct > 66 {
		t.Fatalf("mid share %.1f%% (tail weight folded in) out of [55,66]", pct)
	}
}

// TestOffsetProbWeightsSum guards the three-way OffsetDifferent split and the
// bucket weight table against silent weight changes.
func TestOffsetProbWeightsSum(t *testing.T) {
	if got := diffOffsetOverlapProb + diffOffsetBucketBias + diffOffsetFreeProb; got != 100 {
		t.Fatalf("OffsetDifferent weights sum to %d, want 100", got)
	}
	sum := 0
	for _, p := range hmdfsOffBucketProbs {
		sum += p
	}
	if sum != 100 {
		t.Fatalf("offset bucket weights sum to %d, want 100", sum)
	}
}

// TestFindInsertPositionPathClass checks the tree-type classification of open
// fds: a plain open of a directory (no O_DIRECTORY) must land in the dir
// ranges, and a file open in the file ranges.
func TestFindInsertPositionPathClass(t *testing.T) {
	target := smokeTarget(t)
	ft := smokeFileTree()

	dirOpen := mkOpen(target, "merge_view/dirA")
	pDir := &Prog{Target: target, Calls: []*Call{dirOpen}}
	if _, fd := findInsertPosition(pDir, valueRand(3), 1, nil, -1, 0, 0, "", ft); fd != nil {
		t.Fatalf("directory open classified as file fd")
	}
	if _, fd := findInsertPosition(pDir, valueRand(3), 2, nil, -1, 0, 0, "", ft); fd == nil {
		t.Fatalf("directory open not offered for dir lookups")
	}
	// Without the tree the classification falls back to the flags.
	if _, fd := findInsertPosition(pDir, valueRand(3), 1, nil, -1, 0, 0, "", nil); fd == nil {
		t.Fatalf("nil-tree fallback should classify by flags")
	}

	fileOpen := mkOpen(target, "merge_view/dirA/a")
	pFile := &Prog{Target: target, Calls: []*Call{fileOpen}}
	if _, fd := findInsertPosition(pFile, valueRand(3), 1, nil, -1, 0, 0, "", ft); fd == nil {
		t.Fatalf("file open not offered for file lookups")
	}
	if _, fd := findInsertPosition(pFile, valueRand(3), 2, nil, -1, 0, 0, "", ft); fd != nil {
		t.Fatalf("file open classified as dir fd")
	}
}
