package prog

import (
	"math/rand"
	"strings"
	"testing"
)

// Integration smoke tests for the dynamic group mutations. They build real
// syscall calls via the linux/amd64 target (target data is checked in under
// src/sys/linux), assign last-execution windows by hand, and verify the four
// mutators behave on a concrete concurrent layout. Skipped when the target
// data is unavailable (e.g. cross-compiling on Windows).

func smokeTarget(t *testing.T) *Target {
	target, err := GetTarget("linux", "amd64")
	if err != nil {
		t.Skipf("no linux/amd64 target data: %v", err)
	}
	return target
}

func smokeFileTree() *FileTree {
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir:  map[string][]string{"c1": {"merge_view/dirA", "merge_view/dirB"}},
		Init_file: map[string][]string{"c1": {"merge_view/dirA/a", "merge_view/dirB/b"}},
	})
	return ft
}

func mkOpen(target *Target, path string) *Call {
	meta := target.SyscallMap["open"]
	ptrType := meta.Args[0].Type.(*PtrType)
	flagsType := meta.Args[1].Type.(*FlagsType)
	modeType := meta.Args[2].Type.(*FlagsType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr, MakeConstArg(flagsType, DirIn, 2), MakeConstArg(modeType, DirIn, 0o666)}
	target.assignSizesCall(c)
	return c
}

func mkPwrite64(target *Target, openCall *Call, count, offset uint64) *Call {
	meta := target.SyscallMap["pwrite64"]
	fdType := meta.Args[0].Type.(*ResourceType)
	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)
	fd := MakeResultArg(fdType, DirIn, openCall.Ret, 0)
	buf := MakeDataArg(bufPtrType.Elem, DirIn, make([]byte, count))
	bufPtr := MakePointerArg(bufPtrType, DirIn, 0, buf)
	c := MakeCall(meta, nil)
	c.Args = []Arg{fd, bufPtr, MakeConstArg(countType, DirIn, count), MakeConstArg(posType, DirIn, offset)}
	target.assignSizesCall(c)
	return c
}

func mkClose(target *Target, openCall *Call) *Call {
	meta := target.SyscallMap["close"]
	fdType := meta.Args[0].Type.(*ResourceType)
	c := MakeCall(meta, nil)
	c.Args = []Arg{MakeResultArg(fdType, DirIn, openCall.Ret, 0)}
	target.assignSizesCall(c)
	return c
}

func callPath(c *Call) string {
	if len(c.Args) == 0 {
		return ""
	}
	ptr, ok := c.Args[0].(*PointerArg)
	if !ok || ptr.Res == nil {
		return ""
	}
	d, ok := ptr.Res.(*DataArg)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(string(d.Data()), "\x00")
}

func smokeRand(target *Target) *randGen {
	return newRand(target, rand.NewSource(1))
}

// Layout: p0 anchor open [100,200]; p1 open [150,250] + pwrite64 [160,260]
// (both concurrent, both on the same path -> PathSame) + a late stat
// [500,600] (backbone); p2 open with no timing info (excluded). close calls
// carry no timing so the anchor is unique.
func smokePs(target *Target, withP0Write bool) []*Prog {
	openA := mkOpen(target, "merge_view/dirA/a")
	openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	p0calls := []*Call{openA, mkClose(target, openA)}
	if withP0Write {
		pw0 := mkPwrite64(target, openA, 8, 0)
		pw0.CheckInfo = &FileMetadata{Stime: 110, Etime: 210}
		p0calls = []*Call{openA, pw0, mkClose(target, openA)}
	}
	p0 := &Prog{Target: target, Calls: p0calls}

	openB := mkOpen(target, "merge_view/dirA/a")
	openB.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	pw1 := mkPwrite64(target, openB, 8, 0)
	pw1.CheckInfo = &FileMetadata{Stime: 160, Etime: 260}
	stat0 := mkOpen(target, "merge_view/x/y/z")
	p1 := &Prog{Target: target, Calls: []*Call{openB, pw1, stat0}}

	p2 := &Prog{Target: target, Calls: []*Call{mkOpen(target, "merge_view/dirB/b")}}
	return []*Prog{p0, p1, p2}
}

func TestMutateGroupPathDynamicSmoke(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	ps := smokePs(target, false)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	if !MutateGroupPathDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("MutateGroupPathDynamic returned false")
	}
	newPath := callPath(ps[0].Calls[0])
	if newPath == "" || newPath == "merge_view/dirA/a" {
		t.Fatalf("anchor path not migrated: %q", newPath)
	}
	// The concurrent p1 open follows with PathSame -> the new base path.
	if got := callPath(ps[1].Calls[0]); got != newPath {
		t.Fatalf("concurrent open path = %q, want %q", got, newPath)
	}
	// The non-concurrent backbone call stays put.
	if got := callPath(ps[1].Calls[2]); got != "merge_view/x/y/z" {
		t.Fatalf("backbone path changed: %q", got)
	}
}

func TestRemoveGroupDynamicSmoke(t *testing.T) {
	target := smokeTarget(t)
	ps := smokePs(target, false)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	openA := ps[0].Calls[0]
	closeA := ps[0].Calls[1]
	if !RemoveGroupDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("RemoveGroupDynamic returned false")
	}
	// openA stays: its fd user closeA is outside the concurrent group.
	// openB and pw1 form a self-contained fd chain inside the group and go.
	if len(ps[0].Calls) != 2 || len(ps[1].Calls) != 1 || len(ps[2].Calls) != 1 {
		t.Fatalf("unexpected call counts: p0=%v p1=%v p2=%v (want 2/1/1)",
			len(ps[0].Calls), len(ps[1].Calls), len(ps[2].Calls))
	}
	if ra, ok := closeA.Args[0].(*ResultArg); !ok || ra.Res != openA.Ret {
		t.Fatal("close user lost its open provider after group removal")
	}
	if got := callPath(ps[1].Calls[0]); got != "merge_view/x/y/z" {
		t.Fatalf("p1 backbone changed: %q", got)
	}
	for _, p := range ps {
		if p.HasBrokenRefs() {
			t.Fatal("broken refs after RemoveGroupDynamic")
		}
	}
}

// TestRemoveGroupDynamicFdSafe pins the removal-set-aware fd rule: a provider
// whose user survives outside the group must be kept instead of having the
// user rewritten to a constant fd 0.
func TestRemoveGroupDynamicFdSafe(t *testing.T) {
	target := smokeTarget(t)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	// Nothing removable: both grouped opens are only used by closes outside
	// the group, so the mutation must decline without touching ps.
	openA := mkOpen(target, "merge_view/dirA/a")
	openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	closeA := mkClose(target, openA)
	openB := mkOpen(target, "merge_view/dirA/a")
	openB.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	closeB := mkClose(target, openB)
	ps := []*Prog{
		{Target: target, Calls: []*Call{openA, closeA}},
		{Target: target, Calls: []*Call{openB, closeB}},
	}
	if RemoveGroupDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("RemoveGroupDynamic removed fd-in-use providers")
	}
	if len(ps[0].Calls) != 2 || len(ps[1].Calls) != 2 {
		t.Fatalf("ps changed on declined removal: p0=%v p1=%v", len(ps[0].Calls), len(ps[1].Calls))
	}
	if ra, ok := closeB.Args[0].(*ResultArg); !ok || ra.Res != openB.Ret {
		t.Fatal("closeB lost its open provider")
	}

	// Mixed group: the self-contained fd chain (openB2+pw1) goes, while openC
	// is kept because its closeC survives outside the group.
	openA2 := mkOpen(target, "merge_view/dirA/a")
	openA2.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	closeA2 := mkClose(target, openA2)
	openB2 := mkOpen(target, "merge_view/dirA/a")
	openB2.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	pw1 := mkPwrite64(target, openB2, 8, 0)
	pw1.CheckInfo = &FileMetadata{Stime: 160, Etime: 260}
	openC := mkOpen(target, "merge_view/dirB/b")
	openC.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	closeC := mkClose(target, openC)
	ps2 := []*Prog{
		{Target: target, Calls: []*Call{openA2, closeA2}},
		{Target: target, Calls: []*Call{openB2, pw1}},
		{Target: target, Calls: []*Call{openC, closeC}},
	}
	if !RemoveGroupDynamic(ps2, lcs, smokeRand(target)) {
		t.Fatal("RemoveGroupDynamic removed nothing in the mixed group")
	}
	if len(ps2[0].Calls) != 2 || len(ps2[1].Calls) != 0 || len(ps2[2].Calls) != 2 {
		t.Fatalf("unexpected call counts: p0=%v p1=%v p2=%v (want 2/0/2)",
			len(ps2[0].Calls), len(ps2[1].Calls), len(ps2[2].Calls))
	}
	if ra, ok := closeC.Args[0].(*ResultArg); !ok || ra.Res != openC.Ret {
		t.Fatal("closeC lost its open provider")
	}
	for _, p := range ps2 {
		if p.HasBrokenRefs() {
			t.Fatal("broken refs after mixed group removal")
		}
	}
}

func TestRemoveReservoirDynamicSmoke(t *testing.T) {
	target := smokeTarget(t)
	ps := smokePs(target, false)
	if !RemoveReservoirDynamic(ps, smokeRand(target)) {
		t.Fatal("RemoveReservoirDynamic returned false")
	}
	// The opens that were fd-in-use at removal time (p0's and p1's) must
	// survive; their fd users may themselves be removed, which clears
	// Ret.uses, so check for existence only.
	hasOpen := func(p *Prog) bool {
		for _, c := range p.Calls {
			if strings.Contains(c.Meta.Name, "open") {
				return true
			}
		}
		return false
	}
	if !hasOpen(ps[0]) || !hasOpen(ps[1]) {
		t.Fatal("an open call that was in use at removal time was removed")
	}
	if total := len(ps[0].Calls) + len(ps[1].Calls) + len(ps[2].Calls); total >= 6 {
		t.Fatalf("nothing removed (total=%v, want < 6)", total)
	}
}

func TestMutateGroupDataDynamicSmoke(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	ps := smokePs(target, true)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	if !MutateGroupDataDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("MutateGroupDataDynamic returned false")
	}
	// The anchor's pwrite64 and the concurrent pwrite64 must share offset
	// and length.
	pw0 := ps[0].Calls[1]
	pw1 := ps[1].Calls[1]
	off0 := pw0.Args[3].(*ConstArg).Val
	off1 := pw1.Args[3].(*ConstArg).Val
	if off0 != off1 {
		t.Fatalf("shared offsets differ: %v vs %v", off0, off1)
	}
	len0 := pw0.Args[2].(*ConstArg).Val
	len1 := pw1.Args[2].(*ConstArg).Val
	if len0 != len1 {
		t.Fatalf("shared lengths differ: %v vs %v", len0, len1)
	}
}

// TestFindHBCalls covers the causal-successor detection: per prog, the
// earliest call starting after the anchor finishes; overlapping and earlier
// calls are excluded.
func TestFindHBCalls(t *testing.T) {
	mk := func(stime, etime uint64) *Call {
		return &Call{CheckInfo: &FileMetadata{Stime: stime, Etime: etime}}
	}
	p0 := &Prog{Calls: []*Call{mk(100, 200)}}
	p1 := &Prog{Calls: []*Call{mk(250, 350), mk(500, 600)}}
	p2 := &Prog{Calls: []*Call{mk(180, 220)}}
	p3 := &Prog{Calls: []*Call{mk(50, 90)}}
	ps := []*Prog{p0, p1, p2, p3}
	anchor := GroupPosition{ProgIdx: 0, CallIdx: 0}

	hb := findHBCalls(ps, anchor, "merge_view/a", []int64{0, 0, 0, 0})
	if len(hb) != 1 || hb[0].Pos.ProgIdx != 1 || hb[0].Pos.CallIdx != 0 {
		t.Fatalf("findHBCalls = %+v, want p1[0] only", hb)
	}
	// No timing info: empty result.
	ps2 := []*Prog{&Prog{Calls: []*Call{mk(100, 200)}}, &Prog{Calls: []*Call{{}}}}
	if hb := findHBCalls(ps2, anchor, "merge_view/a", nil); len(hb) != 0 {
		t.Fatalf("findHBCalls with no timing = %+v, want empty", hb)
	}
}

// TestMutateGroupDataDynamicCausal verifies that a causal successor
// (starting after the anchor finishes) participates in the shared-offset
// mutation: sequential write->read on the same offset.
func TestMutateGroupDataDynamicCausal(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	openA := mkOpen(target, "merge_view/dirA/a")
	openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	pw0 := mkPwrite64(target, openA, 8, 0)
	pw0.CheckInfo = &FileMetadata{Stime: 110, Etime: 210}
	p0 := &Prog{Target: target, Calls: []*Call{openA, pw0, mkClose(target, openA)}}

	openB := mkOpen(target, "merge_view/dirA/a")
	openB.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	pw1 := mkPwrite64(target, openB, 8, 0)
	pw1.CheckInfo = &FileMetadata{Stime: 160, Etime: 260}
	pwc := mkPwrite64(target, openB, 8, 0)
	pwc.CheckInfo = &FileMetadata{Stime: 300, Etime: 400}
	p1 := &Prog{Target: target, Calls: []*Call{openB, pw1, pwc}}

	ps := []*Prog{p0, p1}
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	if !MutateGroupDataDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("MutateGroupDataDynamic returned false")
	}
	off0 := ps[0].Calls[1].Args[3].(*ConstArg).Val
	off1 := ps[1].Calls[1].Args[3].(*ConstArg).Val
	offC := ps[1].Calls[2].Args[3].(*ConstArg).Val
	if off0 != off1 || off0 != offC {
		t.Fatalf("causal successor not sharing offset: %v/%v/%v", off0, off1, offC)
	}
}

// TestMutateGroupPathDynamicCausal verifies that a causal successor is
// migrated together with the anchor (same pattern, new location).
func TestMutateGroupPathDynamicCausal(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	openA := mkOpen(target, "merge_view/dirA/a")
	openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	p0 := &Prog{Target: target, Calls: []*Call{openA, mkClose(target, openA)}}

	openB := mkOpen(target, "merge_view/dirA/a")
	openB.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	statC := mkOpen(target, "merge_view/x/y/z")
	statC.CheckInfo = &FileMetadata{Stime: 300, Etime: 400}
	p1 := &Prog{Target: target, Calls: []*Call{openB, statC}}

	ps := []*Prog{p0, p1}
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	if !MutateGroupPathDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("MutateGroupPathDynamic returned false")
	}
	if got := callPath(ps[1].Calls[1]); got == "merge_view/x/y/z" {
		t.Fatalf("causal successor not migrated: %q", got)
	}
}

// TestMutateGroupPathDynamicOwnerUsers pins the owner-user constraint: an
// O_RDONLY open used by pread64 is file-only, so neither the anchor nor the
// shared-owner members may migrate to a directory (a constraint that looked
// only at the op could install a dir and void the read).
func TestMutateGroupPathDynamicOwnerUsers(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	okCount := 0
	for seed := int64(0); seed < 30; seed++ {
		openA := mkOpen(target, "merge_view/dirA/a")
		openA.Args[1].(*ConstArg).Val = 0 // O_RDONLY
		openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		preadA := mkPread64At(target, openA, 16, 0)
		preadA.CheckInfo = &FileMetadata{Stime: 110, Etime: 210}
		p0 := &Prog{Target: target, Calls: []*Call{openA, preadA}}

		openB := mkOpen(target, "merge_view/dirA/a")
		openB.Args[1].(*ConstArg).Val = 0
		openB.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
		preadB := mkPread64At(target, openB, 16, 0)
		preadB.CheckInfo = &FileMetadata{Stime: 160, Etime: 260}
		closeB := mkClose(target, openB)
		closeB.CheckInfo = &FileMetadata{Stime: 300, Etime: 400}
		p1 := &Prog{Target: target, Calls: []*Call{openB, preadB, closeB}}

		ps := []*Prog{p0, p1}
		r := newRand(target, rand.NewSource(seed))
		if !MutateGroupPathDynamic(ps, lcs, r) {
			continue
		}
		okCount++
		for _, pi := range []int{0, 1} {
			path := callPath(ps[pi].Calls[0])
			node := lcs.FileTree.FindNode(path)
			if node == nil || node.Type != NodeTypeFile {
				t.Fatalf("seed %d: owner path %q is not a file (node=%v)", seed, path, node)
			}
		}
		for _, p := range ps {
			if p.HasBrokenRefs() {
				t.Fatalf("seed %d: broken refs after owner-user path mutation", seed)
			}
		}
	}
	if okCount == 0 {
		t.Fatal("no seed produced a path mutation")
	}
}

func mkPread64At(target *Target, openCall *Call, count, offset uint64) *Call {
	meta := target.SyscallMap["pread64"]
	fdType := meta.Args[0].Type.(*ResourceType)
	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)
	fd := MakeResultArg(fdType, DirIn, openCall.Ret, 0)
	buf := MakeOutDataArg(bufPtrType.Elem, DirOut, count)
	bufPtr := MakePointerArg(bufPtrType, DirIn, 0, buf)
	c := MakeCall(meta, nil)
	c.Args = []Arg{fd, bufPtr, MakeConstArg(countType, DirIn, count), MakeConstArg(posType, DirIn, offset)}
	target.assignSizesCall(c)
	return c
}

// TestSyncStashReadVerification covers the read-verification sync helper: only
// pread64 calls reading exactly the old (offset, length) region are moved.
func TestSyncStashReadVerification(t *testing.T) {
	target := smokeTarget(t)
	openA := mkOpen(target, "merge_view/dirA/a")
	openB := mkOpen(target, "merge_view/dirA/a")
	p0 := &Prog{Target: target, Calls: []*Call{
		mkPread64At(target, openA, 50, 100), // matches: moved
		mkPread64At(target, openA, 30, 100), // same offset, other length: kept
		mkPread64At(target, openA, 50, 200), // other offset: kept
	}}
	p1 := &Prog{Target: target, Calls: []*Call{
		mkPread64At(target, openB, 50, 100), // matches in another prog: moved
	}}
	ps := []*Prog{p0, p1}

	syncStashReadVerification(ps, "", 100, 50, 300, 70)

	got0 := p0.Calls[0].Args[3].(*ConstArg).Val
	got1 := p1.Calls[0].Args[3].(*ConstArg).Val
	if got0 != 300 || got1 != 300 {
		t.Fatalf("matching reads not moved: %v/%v (want 300/300)", got0, got1)
	}
	// The out-buffer size must follow the synced count.
	if ptr, ok := p0.Calls[0].Args[1].(*PointerArg); ok {
		if d, ok := ptr.Res.(*DataArg); ok && d.size != 70 {
			t.Fatalf("buf size not synced: %v (want 70)", d.size)
		}
	}
	if len0 := p0.Calls[1].Args[2].(*ConstArg).Val; len0 != 30 {
		t.Fatalf("partially matching read changed: %v", len0)
	}
	if off2 := p0.Calls[2].Args[3].(*ConstArg).Val; off2 != 200 {
		t.Fatalf("unrelated read changed: %v", off2)
	}
}

// TestInsertStashCallAddsReadVerification verifies that a freshly inserted
// pwrite64 gets a matching pread64 in another stash read prog.
func TestInsertStashCallAddsReadVerification(t *testing.T) {
	target := smokeTarget(t)
	openW := mkOpen(target, "merge_view/dirA/a")
	pw := mkPwrite64(target, openW, 8, 0)
	writer := &Prog{Target: target, IsStash: true, Calls: []*Call{openW, pw, mkClose(target, openW)}}

	openR := mkOpen(target, "merge_view/dirA/a")
	pr := mkPread64At(target, openR, 8, 0)
	reader := &Prog{Target: target, IsStash: true, Calls: []*Call{openR, pr, mkClose(target, openR)}}

	ps := []*Prog{writer, reader}
	r := &randGen{Rand: rand.New(rand.NewSource(1))}

	// The special calls needed by insertStashCall (same construction pattern
	// as the fuzzer's, fuzzer.go).
	sc := &SpecialCalls{}
	for id, syscall := range target.Syscalls {
		switch syscall.Name {
		case "pwrite64":
			sc.Pwrite64Id = id
		case "pread64":
			sc.Pread64Id = id
		case "fsync":
			sc.FsyncId = id
		case "fdatasync":
			sc.FdatasyncId = id
		case "close":
			sc.CloseId = id
		}
	}

	inserted := false
	for i := 0; i < 50 && !inserted; i++ {
		pwBefore := 0
		for _, c := range writer.Calls {
			if strings.Contains(c.Meta.Name, "pwrite64") {
				pwBefore++
			}
		}
		if !insertStashCall(ps, writer, r, sc, nil) {
			continue
		}
		pwAfter := 0
		var newOff uint64
		for _, c := range writer.Calls {
			if strings.Contains(c.Meta.Name, "pwrite64") {
				pwAfter++
				if len(c.Args) >= 4 {
					newOff = c.Args[3].(*ConstArg).Val
				}
			}
		}
		if pwAfter > pwBefore {
			inserted = true
			// The reader must have gained a pread64 reading the same offset.
			found := false
			for _, c := range reader.Calls {
				if strings.Contains(c.Meta.Name, "pread64") && len(c.Args) >= 4 &&
					c.Args[3].(*ConstArg).Val == newOff {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no matching read verification for inserted pwrite64 at offset %v", newOff)
			}
		}
	}
	if !inserted {
		t.Fatal("no pwrite64 insertion happened in 50 attempts")
	}
}

// TestInsertDcacheCallGetdents64 verifies the self-contained getdents64
// insertion: open(O_DIRECTORY) + getdents64(count=4096) + close, no longer
// depending on the seed carrying a directory fd.
func TestInsertDcacheCallGetdents64(t *testing.T) {
	target := smokeTarget(t)
	// A dcache-like prog with directory-path calls (mkdir/stat style) but no
	// directory fd at all.
	mkdirCall := mkOpen(target, "merge_view/dirA")
	mkdirCall.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	p := &Prog{Target: target, IsDCache: true, Calls: []*Call{mkdirCall}}

	sc := &SpecialCalls{}
	for id, syscall := range target.Syscalls {
		switch syscall.Name {
		case "open":
			sc.OpenId = id
		case "close":
			sc.CloseId = id
		case "getdents64":
			sc.Getdents64Id = id
		case "mkdir":
			sc.MkdirId = id
		case "rmdir":
			sc.RmdirId = id
		case "creat":
			sc.CreatId = id
		case "unlink":
			sc.UnlinkId = id
		case "rename":
			sc.RenameId = id
		}
	}

	r := &randGen{Rand: rand.New(rand.NewSource(1))}
	// Non-nil hmcfg: the mkdir/creat branches of insertDcacheCall read
	// hmcfg.FileTree (nil-pointer safe only when hmcfg itself is non-nil).
	hmcfg := &Hmdfs_config{}
	inserted := false
	for i := 0; i < 60 && !inserted; i++ {
		if !insertDcacheCall([]*Prog{p}, 0, p, r, sc, hmcfg) {
			continue
		}
		for _, c := range p.Calls {
			if strings.Contains(c.Meta.Name, "getdents64") {
				inserted = true
				if len(c.Args) < 3 {
					t.Fatalf("getdents64 with too few args")
				}
				if cnt := c.Args[2].(*ConstArg).Val; cnt != 4096 {
					t.Fatalf("getdents64 count = %v, want 4096", cnt)
				}
			}
		}
	}
	if !inserted {
		t.Fatal("no getdents64 insertion happened in 60 attempts")
	}
	// The insertion must be self-contained: an O_DIRECTORY open present.
	foundOpenDir := false
	for _, c := range p.Calls {
		if strings.Contains(c.Meta.Name, "open") && len(c.Args) >= 2 {
			if flags, ok := c.Args[1].(*ConstArg); ok && (flags.Val&target.GetConst("O_DIRECTORY")) != 0 {
				foundOpenDir = true
			}
		}
	}
	if !foundOpenDir {
		t.Fatal("no O_DIRECTORY open inserted")
	}
}

// TestFindInsertPositionTmpDirFd pins the fd classification of a TmpDir open:
// the tree node type (TmpDir is a real directory) must win over the raw flag
// check, so the fd lands in the dir ranges and is never reused as a file fd
// (a read/write inserted after it would fail EISDIR before the trace).
func TestFindInsertPositionTmpDirFd(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	sc := hmdfsSmokeSpecialCalls(t, target)
	r := newRand(target, rand.NewSource(1))
	s := newState(target, nil, nil)
	calls := r.generateGetdents64Calls(s, sc, "merge_view/tmpdir")
	if len(calls) < 3 {
		t.Fatalf("getdents64 sequence too short: %d", len(calls))
	}
	p := &Prog{Target: target, Calls: calls}
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_tmpdir: map[string][]string{"c1": {"merge_view/tmpdir"}},
	})

	if _, fd := findInsertPosition(p, r, 2, sc, -1, 0, 0, "", ft); fd != calls[0].Ret {
		t.Fatalf("TmpDir open not classified as a directory fd: %v", fd)
	}
	for seed := int64(0); seed < 20; seed++ {
		rr := newRand(target, rand.NewSource(seed))
		if _, fd := findInsertPosition(p, rr, 1, sc, -1, 0, 0, "", ft); fd != nil {
			t.Fatalf("seed %d: TmpDir fd reused as a file fd", seed)
		}
	}
}

// TestRefreshGeneralFailPos 验证通用突变后的 GeneralFailPos 重定位：sync 调用
// 的实际位置变化应被同步写回 ps[0] 的条目。
func TestRefreshGeneralFailPos(t *testing.T) {
	target := smokeTarget(t)
	sCalls := hmdfsSmokeSpecialCalls(t, target)
	r := newRand(target, rand.NewSource(1))

	var syncIdx uint64
	s0 := r.genSyncCall(sCalls, &syncIdx, 1) // syncId 0 -> key 101
	s1 := r.genSyncCall(sCalls, &syncIdx, 1) // syncId 1 -> key 102

	p0 := &Prog{Target: target, GeneralFailPos: [][]int{{101, 0, 102, 0}}}
	p1 := &Prog{Target: target, Calls: []*Call{
		mkOpen(target, "merge_view/dirA/a"), s0,
		mkOpen(target, "merge_view/dirB/b"), s1,
	}}
	ps := []*Prog{p0, p1}

	RefreshGeneralFailPos(ps, 1)
	if got := p0.GeneralFailPos[0][1]; got != 1 {
		t.Fatalf("start entry = %v, want 1", got)
	}
	if got := p0.GeneralFailPos[0][3]; got != 3 {
		t.Fatalf("end entry = %v, want 3", got)
	}

	// 前插一个调用再刷新：条目应跟着 sync 调用平移。
	p1.Calls = append([]*Call{mkOpen(target, "merge_view/dirA/a")}, p1.Calls...)
	RefreshGeneralFailPos(ps, 1)
	if got := p0.GeneralFailPos[0][1]; got != 2 {
		t.Fatalf("after insert: start entry = %v, want 2", got)
	}
	if got := p0.GeneralFailPos[0][3]; got != 4 {
		t.Fatalf("after insert: end entry = %v, want 4", got)
	}

	// progIdx==0（窗口条目）与空表：不应 panic、不应改动。
	RefreshGeneralFailPos(ps, 0)
	RefreshGeneralFailPos([]*Prog{{Target: target}}, 1)
}

// TestMutateSrvFailPosConsistency 不变量：通用 Mutate（hasFail=true）后，
// SrvFailPos 条目与 syz_failure_sync 调用一一对应（条目位置即 sync 调用索引）。
// 通用突变只会平移/搬移调用（generateCall 不会生成 syz_failure*，removeCall
// 跳过 syz_failure*），故对应关系必须保持。
func TestMutateSrvFailPosConsistency(t *testing.T) {
	target := smokeTarget(t)
	sCalls := hmdfsSmokeSpecialCalls(t, target)
	ct := target.DefaultChoiceTable()

	for iter := 0; iter < 200; iter++ {
		r := newRand(target, rand.NewSource(int64(iter+1)))
		var syncIdx uint64
		s0 := r.genSyncCall(sCalls, &syncIdx, 1)
		s1 := r.genSyncCall(sCalls, &syncIdx, 1)
		p := &Prog{
			Target:     target,
			HasNetFail: true,
			Calls: []*Call{
				mkOpen(target, "merge_view/dirA/a"), s0,
				mkOpen(target, "merge_view/dirB/b"), s1,
			},
			SrvFailPos: [][]int{{1*100 + 1, 1}, {1*100 + 2, 3}},
		}
		p.Mutate(rand.NewSource(int64(1000+iter)), 20, ct, nil, sCalls, 0, true, false, &Hmdfs_config{})

		syncPos := make(map[int]bool)
		for i, c := range p.Calls {
			if strings.Contains(c.Meta.Name, "syz_failure_sync") {
				syncPos[i] = true
			}
		}
		if len(syncPos) != len(p.SrvFailPos) {
			t.Fatalf("iter %d: %d sync calls vs %d SrvFailPos entries", iter, len(syncPos), len(p.SrvFailPos))
		}
		used := make(map[int]bool)
		for _, item := range p.SrvFailPos {
			if !syncPos[item[1]] {
				t.Fatalf("iter %d: SrvFailPos %v does not point at a sync call (sync calls at %v)",
					iter, item, syncPos)
			}
			if used[item[1]] {
				t.Fatalf("iter %d: duplicate SrvFailPos position %v", iter, item[1])
			}
			used[item[1]] = true
		}
	}
}

func srvPositions(p *Prog) []int {
	out := make([]int, 0, len(p.SrvFailPos))
	for _, item := range p.SrvFailPos {
		out = append(out, item[1])
	}
	return out
}

func checkSrvPositions(t *testing.T, p *Prog, want ...int) {
	t.Helper()
	got := srvPositions(p)
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("positions = %v, want %v", got, want)
		}
	}
}

// TestShiftSrvFailPos 验证 SrvFailPos 平移边界（含位置 0 与各类 no-op）。
func TestShiftSrvFailPos(t *testing.T) {
	p := &Prog{SrvFailPos: [][]int{{101, 2}, {102, 5}}}
	shiftSrvFailPos(p, 3, 2)
	checkSrvPositions(t, p, 2, 7)
	shiftSrvFailPos(p, 2, -1)
	checkSrvPositions(t, p, 1, 6)
	shiftSrvFailPos(p, 0, 1)
	checkSrvPositions(t, p, 2, 7)
	if p.SrvFailPos[0][0] != 101 || p.SrvFailPos[1][0] != 102 {
		t.Fatalf("keys changed: %v", p.SrvFailPos)
	}

	p0 := &Prog{SrvFailPos: [][]int{{101, 0}}}
	shiftSrvFailPos(p0, 0, 1)
	checkSrvPositions(t, p0, 1)

	shiftSrvFailPos(p, -1, 1)
	checkSrvPositions(t, p, 2, 7)
	shiftSrvFailPos(&Prog{}, 0, 1)
	shiftSrvFailPos(nil, 0, 1)
}

// TestMoveSrvFailPosEntry 验证搬移操作的区间平移（左右两向与边界）。
func TestMoveSrvFailPosEntry(t *testing.T) {
	left := &Prog{SrvFailPos: [][]int{{101, 2}, {102, 5}, {103, 7}}}
	moveSrvFailPosEntry(left, 5, 1)
	checkSrvPositions(t, left, 3, 1, 7)

	right := &Prog{SrvFailPos: [][]int{{101, 2}, {102, 4}, {103, 7}}}
	moveSrvFailPosEntry(right, 2, 6)
	checkSrvPositions(t, right, 6, 3, 7)

	down := &Prog{SrvFailPos: [][]int{{101, 0}, {102, 1}, {103, 2}, {104, 4}}}
	moveSrvFailPosEntry(down, 0, 3)
	checkSrvPositions(t, down, 3, 0, 1, 4)

	up := &Prog{SrvFailPos: [][]int{{101, 0}, {102, 1}, {103, 3}, {104, 4}}}
	moveSrvFailPosEntry(up, 3, 0)
	checkSrvPositions(t, up, 1, 2, 0, 4)

	noop := &Prog{SrvFailPos: [][]int{{101, 3}}}
	moveSrvFailPosEntry(noop, 3, 3)
	checkSrvPositions(t, noop, 3)
	moveSrvFailPosEntry(&Prog{}, 0, 1)
	moveSrvFailPosEntry(nil, 0, 1)
}

// TestGeneralFailPosSyncKeyMapping 端到端校验生成器与 RefreshGeneralFailPos 共同
// 依赖的映射约定：prog 与 syz_failure_sync 的 id 0/1 对应于 progIdx*100+1/+2。
func TestGeneralFailPosSyncKeyMapping(t *testing.T) {
	target := smokeTarget(t)
	sCalls := hmdfsSmokeSpecialCalls(t, target)
	hmcfg := &Hmdfs_config{}
	hmdfsSmokeConfig(hmcfg)

	findEntry := func(table [][]int, key int) (int, bool) {
		for _, row := range table {
			for i := 0; i < len(row)-1; i++ {
				if row[i] == key {
					return row[i+1], true
				}
			}
		}
		return 0, false
	}
	assertMapping := func(t *testing.T, ps []*Prog) {
		t.Helper()
		if len(ps) < 2 {
			return
		}
		for progIdx := 1; progIdx < len(ps); progIdx++ {
			syncSeen := false
			for callIdx, c := range ps[progIdx].Calls {
				if !strings.Contains(c.Meta.Name, "syz_failure_sync") {
					continue
				}
				syncSeen = true
				id := extractSyncId(c)
				if id != 0 && id != 1 {
					t.Fatalf("prog %d call %d: unexpected sync id %d", progIdx, callIdx, id)
				}
				key := progIdx*100 + id + 1
				pos, ok := findEntry(ps[0].GeneralFailPos, key)
				if !ok {
					t.Fatalf("prog %d: sync id %d has no table entry for key %d", progIdx, id, key)
				}
				if pos != callIdx {
					t.Fatalf("prog %d call %d (id %d): entry %d = %d, want %d",
						progIdx, callIdx, id, key, pos, callIdx)
				}
			}
			if progIdx == 1 && !syncSeen {
				t.Fatalf("prog 1 has no syz_failure_sync calls (generator changed?)")
			}
		}
	}

	for seed := int64(0); seed < 30; seed++ {
		ps := target.GenerateProgsForHmdfsStash(rand.New(rand.NewSource(seed)), sCalls, hmcfg)
		assertMapping(t, ps)

		if len(ps) < 2 {
			continue
		}
		// p1 在三种 mode 下都有 sync 对；做一次通用突变 + 重定位后再验证。
		ps2 := Clones(ps)
		ps2[1].Mutate(rand.NewSource(seed+500), RecommendedCalls, target.DefaultChoiceTable(),
			nil, sCalls, 0, true, false, hmcfg)
		RefreshGeneralFailPos(ps2, 1)
		assertMapping(t, ps2)
	}
}

// mkTruncate builds truncate(path, len) with a length independent of any
// buffer, mirroring the data-mutator support for length-only calls.
func mkTruncate(target *Target, path string, length uint64) *Call {
	meta := target.SyscallMap["truncate"]
	ptrType := meta.Args[0].Type.(*PtrType)
	lenType := meta.Args[1].Type.(*IntType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr, MakeConstArg(lenType, DirIn, length)}
	target.assignSizesCall(c)
	return c
}

// mkRead builds read(fd, buf buffer[out], count len[buf]) on an open call's
// fd: the receive buffer carries no input payload, matching the read support
// in the data mutator.
func mkRead(target *Target, openCall *Call, count uint64) *Call {
	meta := target.SyscallMap["read"]
	fdType := meta.Args[0].Type.(*ResourceType)
	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	fd := MakeResultArg(fdType, DirIn, openCall.Ret, 0)
	buf := MakeOutDataArg(bufPtrType.Elem, DirOut, count)
	bufPtr := MakePointerArg(bufPtrType, DirIn, 0, buf)
	c := MakeCall(meta, nil)
	c.Args = []Arg{fd, bufPtr, MakeConstArg(countType, DirIn, count)}
	target.assignSizesCall(c)
	return c
}

// mkPread64 is the offset-carrying read variant used by the mixed-kind test.
func mkPread64(target *Target, openCall *Call, count, offset uint64) *Call {
	meta := target.SyscallMap["pread64"]
	fdType := meta.Args[0].Type.(*ResourceType)
	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)
	fd := MakeResultArg(fdType, DirIn, openCall.Ret, 0)
	buf := MakeOutDataArg(bufPtrType.Elem, DirOut, count)
	bufPtr := MakePointerArg(bufPtrType, DirIn, 0, buf)
	c := MakeCall(meta, nil)
	c.Args = []Arg{fd, bufPtr, MakeConstArg(countType, DirIn, count), MakeConstArg(posType, DirIn, offset)}
	target.assignSizesCall(c)
	return c
}

// isPickableLength reports whether v can be returned by the legacy (size==0)
// pickMutationLength table: the fixed boundary buckets plus the random ranges
// (512/4096/8192 are fixed bucket values that fall outside the ranges). With a
// known size the function additionally emits size-relative values, which the
// size-aware test asserts by range instead.
func isPickableLength(v uint64) bool {
	switch v {
	case 512, 4096, 8192:
		return true
	}
	for _, rr := range lenBucketRanges {
		if v >= uint64(rr[0]) && v <= uint64(rr[1]) {
			return true
		}
	}
	return false
}

func bufDataLen(c *Call) uint64 {
	ptr, ok := c.Args[1].(*PointerArg)
	if !ok || ptr.Res == nil {
		return 0
	}
	data, ok := ptr.Res.(*DataArg)
	if !ok {
		return 0
	}
	// Size() works for both directions; Data() panics on buffer[out].
	return data.Size()
}

// TestMutateGroupDataTruncateOnly covers length-only calls: truncate has no
// buffer, so the data mutator must rewrite its len argument (arg[1]) to the
// shared length.
func TestMutateGroupDataTruncateOnly(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	const origLen = 100000

	tr0 := mkTruncate(target, "merge_view/dirA/a", origLen)
	tr0.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	p0 := &Prog{Target: target, Calls: []*Call{tr0}}

	tr1 := mkTruncate(target, "merge_view/dirA/a", origLen)
	tr1.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
	p1 := &Prog{Target: target, Calls: []*Call{tr1}}

	ps := []*Prog{p0, p1}
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	if !MutateGroupDataDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("MutateGroupDataDynamic returned false on truncate-only group")
	}
	len0 := ps[0].Calls[0].Args[1].(*ConstArg).Val
	len1 := ps[1].Calls[0].Args[1].(*ConstArg).Val
	if len0 != len1 {
		t.Fatalf("shared truncate lengths differ: %v vs %v", len0, len1)
	}
	if len0 == origLen || !isPickableLength(len0) {
		t.Fatalf("truncate length not mutated to a pickable value: %v (orig %v)", len0, origLen)
	}
}

// TestMutateGroupDataReadOnly covers receive-buffer-only calls: read has no
// input payload, so the data mutator must resize the out buffer (arg[1]) and
// let assignSizesCall recompute count len[buf].
func TestMutateGroupDataReadOnly(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	const origSize = 70000

	// Opens carry no CheckInfo: pickAnchor's fd backtracking does not need it,
	// and leaving it out makes the data call the only eligible anchor.
	openA := mkOpen(target, "merge_view/dirA/a")
	rd0 := mkRead(target, openA, origSize)
	rd0.CheckInfo = &FileMetadata{Stime: 110, Etime: 210}
	p0 := &Prog{Target: target, Calls: []*Call{openA, rd0}}

	openB := mkOpen(target, "merge_view/dirA/a")
	rd1 := mkRead(target, openB, origSize)
	rd1.CheckInfo = &FileMetadata{Stime: 160, Etime: 260}
	p1 := &Prog{Target: target, Calls: []*Call{openB, rd1}}

	ps := []*Prog{p0, p1}
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	if !MutateGroupDataDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("MutateGroupDataDynamic returned false on read-only group")
	}
	for i, p := range ps {
		rd := p.Calls[1]
		count := rd.Args[2].(*ConstArg).Val
		if got := bufDataLen(rd); count != got {
			t.Fatalf("p%d count=%v != buffer len=%v", i, count, got)
		}
	}
	size0 := bufDataLen(ps[0].Calls[1])
	size1 := bufDataLen(ps[1].Calls[1])
	if size0 != size1 {
		t.Fatalf("shared read sizes differ: %v vs %v", size0, size1)
	}
	if size0 == origSize || !isPickableLength(size0) {
		t.Fatalf("read buffer not resized to a pickable value: %v (orig %v)", size0, origSize)
	}
}

// TestMutateGroupDataSharedAcrossKinds covers a mixed group: the write's
// count, the read's count/buffer and the shared offset must all align, with
// the write buffer resized to the same length.
func TestMutateGroupDataSharedAcrossKinds(t *testing.T) {
	target := smokeTarget(t)
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)

	openA := mkOpen(target, "merge_view/dirA/a")
	pw := mkPwrite64(target, openA, 8, 0)
	pw.CheckInfo = &FileMetadata{Stime: 110, Etime: 210}
	p0 := &Prog{Target: target, Calls: []*Call{openA, pw}}

	openB := mkOpen(target, "merge_view/dirA/a")
	pr := mkPread64(target, openB, 8, 0)
	pr.CheckInfo = &FileMetadata{Stime: 160, Etime: 260}
	p1 := &Prog{Target: target, Calls: []*Call{openB, pr}}

	ps := []*Prog{p0, p1}
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	if !MutateGroupDataDynamic(ps, lcs, smokeRand(target)) {
		t.Fatal("MutateGroupDataDynamic returned false on mixed group")
	}
	w := ps[0].Calls[1]
	r := ps[1].Calls[1]
	wCount := w.Args[2].(*ConstArg).Val
	rCount := r.Args[2].(*ConstArg).Val
	wBuf := bufDataLen(w)
	rBuf := bufDataLen(r)
	if wCount != rCount || wCount != wBuf || rCount != rBuf {
		t.Fatalf("lengths not shared: wCount=%v rCount=%v wBuf=%v rBuf=%v", wCount, rCount, wBuf, rBuf)
	}
	if !isPickableLength(wBuf) {
		t.Fatalf("shared length not pickable: %v", wBuf)
	}
	wOff := w.Args[3].(*ConstArg).Val
	rOff := r.Args[3].(*ConstArg).Val
	if wOff != rOff {
		t.Fatalf("offsets not shared: %v vs %v", wOff, rOff)
	}
}

// TestReassignDataArgKeepsFlatPointer pins the L7 fix: resizing a buffer must
// replace the pointer's fields (a fresh address + a flat DataArg pointee), not
// nest a second PointerArg under the old pointer. A nested pointer breaks
// validation (pointee type must be ptrType.Elem) and, in production, makes the
// syscall read the old address while the copyin lands on the new one.
func TestReassignDataArgKeepsFlatPointer(t *testing.T) {
	target := smokeTarget(t)
	openA := mkOpen(target, "merge_view/dirA/a")
	pw := mkPwrite64(target, openA, 8, 0)
	p := &Prog{Target: target, Calls: []*Call{openA, pw}}
	r := newRand(target, rand.NewSource(1))

	oldPtr := pw.Args[1].(*PointerArg)
	oldAddr := oldPtr.Address
	newData := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if !reassignDataArg(p, pw, r, newData) {
		t.Fatal("reassignDataArg returned false")
	}
	ptr := pw.Args[1].(*PointerArg)
	data, ok := ptr.Res.(*DataArg)
	if !ok {
		t.Fatalf("pointee is %T, want *DataArg (flat)", ptr.Res)
	}
	if data.Size() != uint64(len(newData)) || string(data.Data()) != string(newData) {
		t.Fatalf("pointee size/data = %v/%q, want %v/%q",
			data.Size(), data.Data(), len(newData), newData)
	}
	// The old allocation (address 0, size 8 in this fixture) is noted by
	// stateFromProg, so the reallocation must move to a free granule.
	if ptr.Address == oldAddr {
		t.Errorf("address not reallocated: 0x%x", ptr.Address)
	}
	target.assignSizesCall(pw)
	// debugValidate (debug=true in tests) runs inside Serialize.
	if out := p.Serialize(); len(out) == 0 {
		t.Fatal("serialize returned empty")
	}
}

// TestMutateWriteResizeKeepsFlatPointer exercises the stash paths that resize
// the write buffer: the count and the flat buffer size must stay aligned and
// the resized program must still validate/serialize.
func TestMutateWriteResizeKeepsFlatPointer(t *testing.T) {
	target := smokeTarget(t)
	openA := mkOpen(target, "merge_view/dirA/a")
	pw := mkPwrite64(target, openA, 8, 0)
	pw.CheckInfo = &FileMetadata{Stime: 110, Etime: 210}
	p := &Prog{Target: target, Calls: []*Call{openA, pw}, IsStash: true}
	ps := []*Prog{p}

	r := newRand(target, rand.NewSource(1))
	if !mutateWriteLength(ps, p, pw, r) {
		t.Fatal("mutateWriteLength returned false")
	}
	if got, count := bufDataLen(pw), pw.Args[2].(*ConstArg).Val; got != count {
		t.Fatalf("after length mutation count=%v != buffer size=%v", count, got)
	}
	if !mutateWriteData(ps, p, pw, r) {
		t.Fatal("mutateWriteData returned false")
	}
	if got, count := bufDataLen(pw), pw.Args[2].(*ConstArg).Val; got != count {
		t.Fatalf("after data mutation count=%v != buffer size=%v", count, got)
	}
	if out := p.Serialize(); len(out) == 0 {
		t.Fatal("serialize returned empty")
	}
}

// TestStashFdConstructorsLinkOpenRet pins the fd linkage of the five stash
// mutation constructors: args[0] must be a ResultArg referencing the open's
// Ret, not the Ret object itself (which serializes as the constant 0).
func TestStashFdConstructorsLinkOpenRet(t *testing.T) {
	target := smokeTarget(t)
	sc := hmdfsSmokeSpecialCalls(t, target)
	openA := mkOpen(target, "merge_view/dirA/a")
	p := &Prog{Target: target, Calls: []*Call{openA}}
	r := newRand(target, rand.NewSource(1))

	check := func(name string, c *Call) *Call {
		t.Helper()
		if c == nil || len(c.Args) == 0 {
			t.Fatalf("%s: no call", name)
		}
		ra, ok := c.Args[0].(*ResultArg)
		if !ok || ra.Res != openA.Ret {
			t.Fatalf("%s: arg0 = %#v, want a ResultArg referencing open.Ret", name, c.Args[0])
		}
		if !openA.Ret.uses[ra] {
			t.Fatalf("%s: open.Ret.uses does not record the consumer", name)
		}
		return c
	}
	check("pwrite64", genPwriteCallWithFd(p, r, sc, openA.Ret))
	check("pread64", genPreadCallWithFd(p, r, sc, openA.Ret))
	check("pread64At", genPreadCallWithFdAt(p, r, sc, openA.Ret, 0, 16))
	check("fsync", genFsyncCallWithFd(p, r, sc, openA.Ret))
	check("fdatasync", genFdatasyncCallWithFd(p, r, sc, openA.Ret))

	for _, c := range []*Call{
		genPwriteCallWithFd(p, r, sc, openA.Ret),
		genPreadCallWithFd(p, r, sc, openA.Ret),
	} {
		q := &Prog{Target: target, Calls: []*Call{openA, c}}
		if q.HasBrokenRefs() {
			t.Fatalf("%s: broken refs after fd wrapping", c.Meta.CallName)
		}
		if out := q.Serialize(); len(out) == 0 {
			t.Fatalf("%s: serialize returned empty", c.Meta.CallName)
		}
	}
}

// TestStashFdInsertCallLinkage sweeps insertStashCall and asserts every
// inserted fd call references one of the program's opens.
func TestStashFdInsertCallLinkage(t *testing.T) {
	target := smokeTarget(t)
	sc := hmdfsSmokeSpecialCalls(t, target)
	openA := mkOpen(target, "merge_view/dirA/a")
	openB := mkOpen(target, "merge_view/dirB/b")
	closeA := mkClose(target, openA)
	base := []*Call{openA, openB, closeA}

	p := &Prog{Target: target, Calls: append([]*Call{}, base...), IsStash: true}
	ps := []*Prog{p}
	seen := map[string]bool{}
	for seed := int64(0); seed < 300; seed++ {
		p.Calls = append([]*Call{}, base...)
		r := newRand(target, rand.NewSource(seed))
		if !insertStashCall(ps, p, r, sc, nil) {
			continue
		}
		openRets := map[*ResultArg]bool{}
		for _, c := range p.Calls {
			if strings.Contains(c.Meta.Name, "open") {
				openRets[c.Ret] = true
			}
		}
		for _, c := range p.Calls {
			if c == openA || c == openB || c == closeA {
				continue
			}
			if c.Args[0] == nil {
				t.Fatalf("seed %d: inserted %s has no args", seed, c.Meta.CallName)
			}
			ra, ok := c.Args[0].(*ResultArg)
			if !ok || ra.Res == nil || !openRets[ra.Res] {
				t.Fatalf("seed %d: inserted %s fd = %#v, want an open.Ret reference", seed, c.Meta.CallName, c.Args[0])
			}
			seen[c.Meta.CallName] = true
		}
		if p.HasBrokenRefs() {
			t.Fatalf("seed %d: broken refs after insertStashCall", seed)
		}
	}
	for _, name := range []string{"pwrite64", "pread64", "fsync", "fdatasync"} {
		if !seen[name] {
			t.Errorf("no %s inserted across 300 seeds", name)
		}
	}
}

// TestStashFdInsertCallCloseBoundary pins the insertion window: a call that
// references an open's fd must land before that fd's close, also when the
// program interleaves several open/close blocks. (Historical bug: the close
// lookup compared against a fresh wrapper, so the window always extended to
// the program end and fd calls could land after their close -> EBADF.)
func TestStashFdInsertCallCloseBoundary(t *testing.T) {
	target := smokeTarget(t)
	sc := hmdfsSmokeSpecialCalls(t, target)
	openA := mkOpen(target, "merge_view/dirA/a")
	pwA := mkPwrite64(target, openA, 8, 0)
	closeA := mkClose(target, openA)
	openB := mkOpen(target, "merge_view/dirB/b")
	closeB := mkClose(target, openB)
	base := []*Call{openA, pwA, closeA, openB, closeB}

	inserted := 0
	for seed := int64(0); seed < 300; seed++ {
		p := &Prog{Target: target, Calls: append([]*Call{}, base...), IsStash: true}
		ps := []*Prog{p}
		r := newRand(target, rand.NewSource(seed))
		if !insertStashCall(ps, p, r, sc, nil) {
			continue
		}
		inserted++
		// First close after each open, keyed by the original return object.
		closeIdx := map[*ResultArg]int{}
		for i, c := range p.Calls {
			if c == nil || !strings.Contains(c.Meta.Name, "close") || len(c.Args) == 0 {
				continue
			}
			if ra, ok := c.Args[0].(*ResultArg); ok && ra.Res != nil {
				if _, seen := closeIdx[ra.Res]; !seen {
					closeIdx[ra.Res] = i
				}
			}
		}
		for i, c := range p.Calls {
			if c == openA || c == pwA || c == closeA || c == openB || c == closeB {
				continue
			}
			if c == nil || len(c.Args) == 0 {
				continue
			}
			ra, ok := c.Args[0].(*ResultArg)
			if !ok || ra.Res == nil {
				continue
			}
			if ci, ok := closeIdx[ra.Res]; ok && i > ci {
				t.Fatalf("seed %d: inserted %s at %d lands after its close at %d",
					seed, c.Meta.CallName, i, ci)
			}
		}
		if p.HasBrokenRefs() {
			t.Fatalf("seed %d: broken refs after insertStashCall", seed)
		}
	}
	if inserted == 0 {
		t.Fatal("no insertion across 300 seeds: test is vacuous")
	}
}

// TestStashFdAddReadVerificationLinkage pins the added verification read's fd:
// it must reference the reader's own open, not the raw Ret.
func TestStashFdAddReadVerificationLinkage(t *testing.T) {
	target := smokeTarget(t)
	sc := hmdfsSmokeSpecialCalls(t, target)

	writerOpen := mkOpen(target, "merge_view/dirA/a")
	writer := &Prog{Target: target, Calls: []*Call{writerOpen, mkPwrite64(target, writerOpen, 8, 0)}, IsStash: true}
	readerOpen := mkOpen(target, "merge_view/dirA/a")
	reader := &Prog{Target: target, Calls: []*Call{readerOpen, mkPread64(target, readerOpen, 8, 0)}, IsStash: true}
	ps := []*Prog{writer, reader}

	before := len(reader.Calls)
	r := newRand(target, rand.NewSource(1))
	addStashReadVerification(ps, writer, "merge_view/dirA/a", r, sc, 0, 8)
	if len(reader.Calls) != before+1 {
		t.Fatalf("reader calls = %d, want %d", len(reader.Calls), before+1)
	}
	ins := reader.Calls[before]
	ra, ok := ins.Args[0].(*ResultArg)
	if !ok || ra.Res != readerOpen.Ret {
		t.Fatalf("inserted %s fd = %#v, want reader open.Ret", ins.Meta.CallName, ins.Args[0])
	}
	if !readerOpen.Ret.uses[ra] {
		t.Fatal("reader open.Ret.uses does not record the inserted read")
	}
	if reader.HasBrokenRefs() {
		t.Fatal("broken refs after addStashReadVerification")
	}
	if out := reader.Serialize(); len(out) == 0 {
		t.Fatal("serialize returned empty")
	}
}
