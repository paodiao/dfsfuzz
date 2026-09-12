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

func smokeRand() *randGen {
	return &randGen{Rand: rand.New(rand.NewSource(1))}
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
	ps := smokePs(target, false)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	if !MutateGroupPathDynamic(ps, lcs, smokeRand()) {
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

	if !RemoveGroupDynamic(ps, lcs, smokeRand()) {
		t.Fatal("RemoveGroupDynamic returned false")
	}
	if len(ps[0].Calls) != 1 || len(ps[1].Calls) != 1 {
		t.Fatalf("unexpected call counts: p0=%v p1=%v (want 1/1)", len(ps[0].Calls), len(ps[1].Calls))
	}
}

func TestRemoveReservoirDynamicSmoke(t *testing.T) {
	target := smokeTarget(t)
	ps := smokePs(target, false)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	if !RemoveReservoirDynamic(ps, lcs, smokeRand()) {
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
	ps := smokePs(target, true)
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}

	if !MutateGroupDataDynamic(ps, lcs, smokeRand()) {
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
	if !MutateGroupDataDynamic(ps, lcs, smokeRand()) {
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
	if !MutateGroupPathDynamic(ps, lcs, smokeRand()) {
		t.Fatal("MutateGroupPathDynamic returned false")
	}
	if got := callPath(ps[1].Calls[1]); got == "merge_view/x/y/z" {
		t.Fatalf("causal successor not migrated: %q", got)
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

	syncStashReadVerification(ps, 100, 50, 300, 70)

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

// patternSingleOpenBase builds the base shape that produced the broken-ref
// programs intercepted in hmdfsfuzz3: every node holds exactly one timed open
// (no close), so an fd-using pattern op can only reference that open.
func patternSingleOpenBase(target *Target) []*Prog {
	paths := []string{"merge_view/dirA/a", "merge_view/dirA/a", "merge_view/dirB/b"}
	ps := make([]*Prog, 0, len(paths))
	for i, pth := range paths {
		o := mkOpen(target, pth)
		o.CheckInfo = &FileMetadata{Stime: uint64(1000 + i*500), Etime: uint64(1100 + i*500)}
		ps = append(ps, &Prog{Target: target, Calls: []*Call{o}})
	}
	return ps
}

// patternBrokenSweep runs insertCallFromPattern over deterministic seeds on
// the given base shape and reports how many resulting programs hold
// dangling/forward references (the panic: no result root cause). A hit pins
// the exact seed for single-step reproduction.
func patternBrokenSweep(t *testing.T, ps []*Prog, seedType string, sCalls *SpecialCalls,
	cfg *Hmdfs_config, lcs *LayeredChoiceStrategy, seeds int64) (hits int, firstSeed int64, firstDiag string) {
	firstSeed = -1
	for seed := int64(0); seed < seeds; seed++ {
		ps := Clones(ps)
		r := newRand(ps[0].Target, rand.NewSource(seed))
		if !insertCallFromPattern(ps, r, sCalls, cfg, lcs, seedType) {
			continue
		}
		for _, p := range ps {
			if p.HasBrokenRefs() {
				hits++
				if firstSeed < 0 {
					firstSeed = seed
					firstDiag = p.DumpRefDiagnosis()
				}
				break
			}
		}
	}
	return
}

func TestInsertCallFromPatternBrokenRefs(t *testing.T) {
	target := smokeTarget(t)
	sCalls := hmdfsSmokeSpecialCalls(t, target)
	cfg := &Hmdfs_config{
		Cids:     []string{"c1", "c2", "c3"},
		Node_num: 3,
		Serv_num: 0,
		Init_dir: map[string][]string{
			"c1": {"merge_view/dirA", "merge_view/dirB"},
		},
		Init_file: map[string][]string{
			"c1": {"merge_view/dirA/a", "merge_view/dirB/b"},
		},
	}
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(cfg)
	cfg.FileTree = ft

	const seeds = 30000
	for _, seedType := range []string{"fileops", "inodeops"} {
		lcs := NewLayeredChoiceStrategy(seedType, cfg, target)
		hits, firstSeed, firstDiag := patternBrokenSweep(t, patternSingleOpenBase(target),
			seedType, sCalls, cfg, lcs, seeds)
		t.Logf("seedType=%s base=single-open: %d broken-ref hits / %d seeds (first at seed %d)",
			seedType, hits, seeds, firstSeed)
		if hits > 0 && firstDiag != "" {
			t.Logf("first hit diagnosis:\n%s", firstDiag)
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
		p.Mutate(rand.NewSource(int64(1000+iter)), 20, ct, nil, sCalls, 0, true, false, &Hmdfs_config{}, 0)

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
// 依赖的映射约定：prog 内 syz_failure_sync 的 id 0/1 对应键 progIdx*100+1/+2。
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
		// p1 在三种 mode 下都带 sync 对；做一次通用突变 + 重定位后再验。
		ps2 := Clones(ps)
		ps2[1].Mutate(rand.NewSource(seed+500), RecommendedCalls, target.DefaultChoiceTable(),
			nil, sCalls, 0, true, false, hmcfg, 1)
		RefreshGeneralFailPos(ps2, 1)
		assertMapping(t, ps2)
	}
}
