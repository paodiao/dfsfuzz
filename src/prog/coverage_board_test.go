package prog

import (
	"math/rand"
	"sync"
	"testing"
)

// newTestCoverageBoard resets and initializes the global board with a fake
// config (no tree -> default depth buckets; a persistence dir), and restores
// the uninitialized state when the test ends so other tests see no bias.
func newTestCoverageBoard(t *testing.T) {
	t.Helper()
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{Persistence_dir: "merge_view/persist"})
	t.Cleanup(ResetCoverageBoard)
}

// mkCoveragePair builds a pair whose features are set directly (call names
// are enough for DagPairToVariant; the relation comes from PathRel).
// mkCoveragePair builds one concrete synthesized pair. A call with several
// traced funcs (creat) contributes its first candidate: the synthetic pair
// models a single observed event, not the whole shape set.
func mkCoveragePair(rootCall, variantCall string, rel PathRel, temporal TemporalRel,
	retA, retB RetBucket, bPath string) DAGPair {
	aFunc, bFunc := uint32(0), uint32(0)
	if fs, ok := callFuncClasses(hmdfsProfile, rootCall, false); ok && len(fs) > 0 {
		aFunc = fs[0]
	}
	if fs, ok := callFuncClasses(hmdfsProfile, variantCall, false); ok && len(fs) > 0 {
		bFunc = fs[0]
	}
	return DAGPair{
		A:        &DAGVertex{CallName: rootCall, Path: bPath, RetBucket: retA, FuncID: aFunc},
		B:        &DAGVertex{CallName: variantCall, Path: bPath, RetBucket: retB, FuncID: bFunc, Size: 1000, Off: 0},
		Temporal: temporal,
		PathRel:  rel,
	}
}

// TestDirectedPairPotentialArithmetic pins the arithmetic producible model for a
// representative combo: write x write @ Same with persist=on and depths
// {1,2,3} has retA pinned to Success, so 1x2x4x2x3x1 = 48 cells per temporal
// form; there is no target subset (full scoring), so both forms count:
// 48+48 = 96.
func TestDirectedPairPotentialArithmetic(t *testing.T) {
	newTestCoverageBoard(t)
	if pot := DirectedPairPotential("write", "write", PathSame); pot != 96 {
		t.Fatalf("potential = %d, want 96", pot)
	}
}

// TestCoverageModifierGates pins the ExtractPairs-based producible gates:
// HB needs a modifier root, CC needs at least one modifier side, and a combo
// with two non-modifier sides produces nothing. Numbers use the same fake
// board as TestDirectedPairPotentialArithmetic (persist on, depths {1,2,3});
// read/write/mkdir variants are single-type (file/dir).
func TestCoverageModifierGates(t *testing.T) {
	newTestCoverageBoard(t)
	cases := []struct {
		root, variant string
		want          int
	}{
		{"read", "stat", 0},    // both non-modifier: no pair can form
		{"read", "write", 48},  // CC only: write is a modifier (2x4x2x3x1)
		{"stat", "mkdir", 12},  // CC only: mkdir is a modifier (2x1x2x3x1)
		{"write", "read", 48},  // HB+CC: read has 1x4x2x3x1 cells per form
		{"write", "write", 96}, // HB+CC regression anchor
	}
	for _, c := range cases {
		if pot := DirectedPairPotential(c.root, c.variant, PathSame); pot != c.want {
			t.Fatalf("DirectedPairPotential(%s,%s,Same) = %d, want %d", c.root, c.variant, pot, c.want)
		}
	}
}

// TestCoveragePersistDepth pins the depth-conditional persist multiplicity:
// persist paths are the persist dir itself or a descendant (prefix match), so
// only domain buckets >= the dir's bucket can hold persist cells. The other
// tests use a shallow persist dir ("merge_view/persist", bucket 1), where
// every domain bucket qualifies (depthMult 6); a deep dir (bucket 3) narrows
// it to that single bucket (depthMult 4), and a dir outside the modeled depth
// domain contributes nothing.
func TestCoveragePersistDepth(t *testing.T) {
	// Deep persist dir: bucket 3 -> depthMult 3 + 1 = 4.
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	InitCoverageBoard(&Hmdfs_config{Persistence_dir: "merge_view/a/b/c/d/e/f"})
	if got := DepthBucketOf("merge_view/a/b/c/d/e/f"); got != 3 {
		t.Fatalf("persist dir bucket = %d, want 3", got)
	}
	cases := []struct {
		root, variant string
		want          int
	}{
		{"write", "write", 64}, // 2x4x1x4 per temporal form, HB+CC
		{"read", "write", 32},  // CC only: 2x4x1x4
		{"stat", "mkdir", 8},   // CC only: 2x1x1x4
		{"write", "read", 32},  // 1x4x1x4 per form, HB+CC
	}
	for _, c := range cases {
		if pot := DirectedPairPotential(c.root, c.variant, PathSame); pot != c.want {
			t.Fatalf("deep persist: DirectedPairPotential(%s,%s,Same) = %d, want %d",
				c.root, c.variant, pot, c.want)
		}
	}

	// Persist dir outside the modeled depth domain (shallow tree: buckets
	// {1,2}): no persist multiplicity is counted.
	ResetCoverageBoard()
	tree := NewFileTree()
	tree.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir:  map[string][]string{"c1": {"merge_view/dirA"}},
		Init_file: map[string][]string{"c1": {"merge_view/dirA/a"}},
	})
	InitCoverageBoard(&Hmdfs_config{Persistence_dir: "merge_view/a/b/c/d/e/f", FileTree: tree})
	if covBoard.persistBucket != 3 {
		t.Fatalf("persistBucket = %d, want 3", covBoard.persistBucket)
	}
	if covBoard.depths[3] {
		t.Fatal("bucket 3 unexpectedly modeled by the shallow test tree")
	}
	if pot := DirectedPairPotential("write", "write", PathSame); pot != 32 {
		t.Fatalf("out-of-domain persist: potential = %d, want 32", pot)
	}
}

// TestCoverageRecordPair checks that recording a novel target pair lowers the
// combo's uncovered potential by exactly one, and that re-recording the same
// cell is idempotent.
func TestCoverageRecordPair(t *testing.T) {
	newTestCoverageBoard(t)
	pot0 := DirectedPairPotential("write", "write", PathSame)
	p := mkCoveragePair("write", "write", PathSamePath, TemporalHB,
		RetSuccess, RetFailure, "merge_view/persist/f")
	RecordCoveragePairs([]DAGPair{p})
	if pot := DirectedPairPotential("write", "write", PathSame); pot != pot0-1 {
		t.Fatalf("potential after first record = %d, want %d", pot, pot0-1)
	}
	RecordCoveragePairs([]DAGPair{p})
	if pot := DirectedPairPotential("write", "write", PathSame); pot != pot0-1 {
		t.Fatalf("potential after duplicate record = %d, want %d", pot, pot0-1)
	}
	found, target := CoverageStats()
	if found != 1 || target <= 0 {
		t.Fatalf("cover stats = %d/%d after one record", found, target)
	}
}

// TestPickWeightedIndex covers the potential-roulette index picker: all-zero
// returns -1, a single positive entry always wins, and zero entries never win.
func TestPickWeightedIndex(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	if i := pickWeightedIndex([]int{0, 0, 0}, r); i != -1 {
		t.Fatalf("all-zero = %d, want -1", i)
	}
	for n := 0; n < 20; n++ {
		if i := pickWeightedIndex([]int{0, 7, 0}, r); i != 1 {
			t.Fatalf("single-positive = %d, want 1", i)
		}
	}
	for n := 0; n < 100; n++ {
		if i := pickWeightedIndex([]int{3, 0, 3}, r); i == 1 {
			t.Fatal("zero-weight index picked")
		}
	}
}

// TestCoverageBiasPickRoulette checks the directed pick returns a candidate
// whose potential matches the board (roulette over uncovered target cells).
func TestCoverageBiasPickRoulette(t *testing.T) {
	newTestCoverageBoard(t)
	r := rand.New(rand.NewSource(1))
	cand := []CallVariant{
		{CallName: "mkdir", PathRelation: PathSame},
		{CallName: "stat", PathRelation: PathSame},
		{CallName: "getdents64", PathRelation: PathSame},
	}
	pots := covBoard.directedPairPotentials("mkdir", cand)
	v, pot := pickByDirectedPairPotential(cand, "mkdir", r)
	if v == nil || pot <= 0 {
		t.Fatalf("roulette pick = %v pot=%d, want non-nil >0", v, pot)
	}
	ok := false
	for i := range cand {
		if cand[i] == *v && pots[i] == pot {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("picked %v pot=%d inconsistent with %v", *v, pot, pots)
	}
}

// TestCoverageUnreachableRetirement drives a combo past the unreachable
// threshold without production: it drops out of the directed scope, and a
// later discovery lifts the suppression and re-baselines the window.
func TestCoverageUnreachableRetirement(t *testing.T) {
	newTestCoverageBoard(t)
	ck := comboKey{Root: "write", Variant: "truncate", Rel: PathSame}
	pot0 := DirectedPairPotential(ck.Root, ck.Variant, ck.Rel)
	if pot0 <= 0 {
		t.Fatalf("combo has no potential: %d", pot0)
	}
	for i := 0; i < unreachableThreshold; i++ {
		covBoard.noteDirectedPick(ck, pot0)
	}
	if !covBoard.unreachable[ck] {
		t.Fatal("combo not marked unreachable after threshold misses")
	}
	if pot := DirectedPairPotential(ck.Root, ck.Variant, ck.Rel); pot != 0 {
		t.Fatalf("directed-scope potential = %d, want 0 for a suppressed combo", pot)
	}
	// A discovery lifts the suppression.
	f0 := covBoard.found[ck]
	RecordCoveragePairs([]DAGPair{mkCoveragePair("write", "truncate", PathSamePath, TemporalHB,
		RetSuccess, RetENOENT, "merge_view/d/f")})
	got := covBoard.found[ck].total()
	want := f0.total()
	if got[0]+got[1] != want[0]+want[1]+1 {
		t.Fatal("found not incremented")
	}
	if covBoard.unreachable[ck] {
		t.Fatal("suppression not lifted by a discovery")
	}
	if pot := DirectedPairPotential(ck.Root, ck.Variant, ck.Rel); pot != pot0-1 {
		t.Fatalf("directed potential after discovery = %d, want %d", pot, pot0-1)
	}
}

// TestDirectedCellMissAccounting covers the Phase 2b cell ledger: consecutive
// misses suppress the cell, a hit (or a fresh discovery) clears it, and the
// directCell counters track plans/hits/misses/plan-fails.
func TestDirectedCellMissAccounting(t *testing.T) {
	newTestCoverageBoard(t)
	p := mkCoveragePair("write", "truncate", PathSamePath, TemporalHB,
		RetSuccess, RetENOENT, "merge_view/d/f")
	ck, ok := cellKeyOf(&p, covBoard.persistDir)
	if !ok {
		t.Fatal("cellKeyOf failed")
	}
	if !covBoard.cellUncovered(ck) {
		t.Fatal("fresh cell should be uncovered")
	}
	for i := 0; i < cellMissThreshold; i++ {
		covBoard.noteDirectedMiss(ck)
	}
	if !covBoard.cellUnreachable[ck] {
		t.Fatal("cell not suppressed after threshold misses")
	}
	if covBoard.cellUncovered(ck) {
		t.Fatal("suppressed cell still uncovered")
	}
	if got := CellUnreachableCount(); got != 1 {
		t.Fatalf("cellUnreachable count = %d, want 1", got)
	}
	covBoard.noteDirectedHit(ck)
	if covBoard.cellUnreachable[ck] || covBoard.cellAttempts[ck] != 0 {
		t.Fatal("hit did not clear the cell baseline")
	}
	// A fresh discovery of a suppressed cell also clears it.
	for i := 0; i < cellMissThreshold; i++ {
		covBoard.noteDirectedMiss(ck)
	}
	if !covBoard.cellUnreachable[ck] {
		t.Fatal("cell not re-suppressed")
	}
	RecordCoveragePairs([]DAGPair{p})
	if covBoard.cellUnreachable[ck] {
		t.Fatal("discovery did not clear the cell suppression")
	}
	// directCell counters: plans/hits/misses/noPlan/insertFail, plus cfgErr.
	covBoard.noteDirectedPlans(1)
	NoteDirectedInsertFail()
	NoteDirectedNoPlan()
	NoteDirectedCfgErr()
	plans, hits, misses, noPlans, insertFails := DirectedCellStats()
	if plans != 1 || hits != 1 || misses != 2*cellMissThreshold || noPlans != 1 || insertFails != 1 {
		t.Fatalf("directCell stats = %d/%d/%d/%d/%d, want 1/1/%d/1/1",
			plans, hits, misses, noPlans, insertFails, 2*cellMissThreshold)
	}
	if batches := DirectedBatchStats(); batches != 1 {
		t.Fatalf("directCell batches = %d, want 1", batches)
	}
	if got := DirectedCfgErrCount(); got != 1 {
		t.Fatalf("cfgErr = %d, want 1", got)
	}
}

// TestDirectedCellBreakdownCounters pins the diagnostic splits: noPlan reasons
// are attributed once per attempt, cfgErr/found are not noPlan events, and
// FreshSame misses are separated from ordinary misses without changing the
// aggregate counters or the suppression ledger.
func TestDirectedCellBreakdownCounters(t *testing.T) {
	newTestCoverageBoard(t)
	NoteDirectedNoPlanReason(directedPlanNoneNoCombo)
	NoteDirectedNoPlanReason(directedPlanNoneNoCell)
	NoteDirectedNoPlanReason(directedPlanNoneEnumEmpty)
	NoteDirectedNoPlanReason(directedPlanNoneTree)
	NoteDirectedNoPlan() // generic: aggregate only
	NoteDirectedNoPlanReason(directedPlanFound)
	NoteDirectedNoPlanReason(directedPlanCfgErr)
	ck := cellKey{
		Root: "write", Variant: "truncate", Rel: PathSame, Temporal: TemporalHB,
		RetA: RetSuccess, RetB: RetENOENT,
	}
	covBoard.noteDirectedMissKind(ck, true)
	covBoard.noteDirectedMissKind(ck, false)
	_, _, misses, noPlans, _ := DirectedCellStats()
	if misses != 2 {
		t.Fatalf("misses = %d, want 2", misses)
	}
	if noPlans != 5 {
		t.Fatalf("noPlans = %d, want 5", noPlans)
	}
	bd := DirectedCellBreakdownStats()
	if bd.NoPlanNoCombo != 1 || bd.NoPlanNoCell != 1 ||
		bd.NoPlanEnumEmpty != 1 || bd.NoPlanTree != 1 {
		t.Fatalf("noPlan breakdown = %+v, want 1/1/1/1", bd)
	}
	if bd.MissFreshSame != 1 || bd.MissOther != 1 {
		t.Fatalf("miss breakdown = fresh:%d other:%d, want 1/1",
			bd.MissFreshSame, bd.MissOther)
	}
	if covBoard.cellUnreachable[ck] {
		t.Fatal("diagnostic misses must not suppress the cell below threshold")
	}
}

// TestCellAdmissionSplitsSaturationFromMismatch pins the noPlan classification
// input: a covered/suppressed cell is saturated, while a cell outside the
// plannable layer is an enumeration/plannable mismatch (neither uncovered nor
// saturated).
func TestCellAdmissionSplitsSaturationFromMismatch(t *testing.T) {
	glusterfsCoverageConfig(t, true)
	ck := glusterfsCoverageCell("symlink", "readlink", PathSame, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	if uncovered, saturated := covBoard.cellAdmission(ck); !uncovered || saturated {
		t.Fatalf("plannable fresh cell: uncovered=%v saturated=%v, want true/false",
			uncovered, saturated)
	}
	covBoard.mu.Lock()
	covBoard.discovered[ck] = 1
	covBoard.mu.Unlock()
	if uncovered, saturated := covBoard.cellAdmission(ck); uncovered || !saturated {
		t.Fatalf("covered cell: uncovered=%v saturated=%v, want false/true",
			uncovered, saturated)
	}
	bad := glusterfsCoverageCell("symlink", "readlink", PathSibling, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	if uncovered, saturated := covBoard.cellAdmission(bad); uncovered || saturated {
		t.Fatalf("out-of-plannable cell: uncovered=%v saturated=%v, want false/false",
			uncovered, saturated)
	}
}

// TestCoverageUnreachableRebaseline checks that a combo producing a new cell
// within every window shorter than the threshold is never marked unreachable:
// recordPairs re-baselines the window immediately at production, so the
// effective rule is "threshold consecutive directed picks since the last
// production". One distinct target cell per 7 picks (paths differ in depth
// bucket; bucket 1 = 1 slash, bucket 2 = 2-4, bucket 3 = 5+).
func TestCoverageUnreachableRebaseline(t *testing.T) {
	newTestCoverageBoard(t)
	ck := comboKey{Root: "write", Variant: "truncate", Rel: PathSame}
	const cadence = unreachableThreshold - 1 // production strictly before the window closes
	paths := []string{"merge_view/d", "merge_view/d/e", "merge_view/a/b/c/d/e"}
	for i := 0; i < cadence*len(paths); i++ {
		if i%cadence == 0 {
			p := mkCoveragePair("write", "truncate", PathSamePath, TemporalHB,
				RetSuccess, RetENOENT, paths[i/cadence])
			RecordCoveragePairs([]DAGPair{p})
		}
		covBoard.noteDirectedPick(ck, 1)
		if covBoard.unreachable[ck] {
			t.Fatalf("combo marked unreachable despite production in window %d", i/cadence)
		}
	}
	if pot := DirectedPairPotential(ck.Root, ck.Variant, ck.Rel); pot <= 0 {
		t.Fatalf("productive combo potential = %d, want > 0", pot)
	}
}

// TestCoverageChooseVariantDirected is a smoke test for the chooseVariant
// integration: with an initialized board and the `directedBiasProb` bias
// (0.75), some picks must be directed (and every pick must stay a valid
// variant).
func TestCoverageChooseVariantDirected(t *testing.T) {
	newTestCoverageBoard(t)
	dct := NewDistributedChoiceTable()
	r := rand.New(rand.NewSource(1))
	before, _ := DirectedPickStats()
	for i := 0; i < 50; i++ {
		if cv := dct.ChooseVariant("mkdir", r); cv == nil {
			t.Fatal("ChooseVariant returned nil")
		}
	}
	after, _ := DirectedPickStats()
	if d := after - before; d == 0 || d > 50 {
		t.Fatalf("directed picks delta = %d out of 50, want in (0,50]", d)
	}
}

// TestCoverageBoardNotReady checks the uninitialized board is inert: no
// potential, no records, and the directed pick helper returns nil.
func TestCoverageBoardNotReady(t *testing.T) {
	ResetCoverageBoard()
	defer ResetCoverageBoard()
	if CoverageBoardReady() {
		t.Fatal("board reported ready before init")
	}
	if pot := DirectedPairPotential("mkdir", "write", PathSame); pot != 0 {
		t.Fatalf("potential before init = %d, want 0", pot)
	}
	RecordCoveragePairs([]DAGPair{mkCoveragePair("mkdir", "write", PathSamePath, TemporalHB,
		RetSuccess, RetFailure, "merge_view/persist/f")})
	if found, _ := CoverageStats(); found != 0 {
		t.Fatalf("records before init counted: %d", found)
	}
	cand := []CallVariant{{CallName: "write", PathRelation: PathSame}}
	if v, pot := pickByDirectedPairPotential(cand, "mkdir", rand.New(rand.NewSource(1))); v != nil || pot != 0 {
		t.Fatalf("directed pick before init = %v/%d, want nil/0", v, pot)
	}
}

// TestCellKeyOffNormalization pins the call-name-level offset normalization:
// chmod (FuncSetattr without ia_size) must key as NA even though the raw
// feature bucket would be Zero, while real offset calls keep their bucket.
func TestCellKeyOffNormalization(t *testing.T) {
	mk := func(call string, fid uint32, off, size uint64) cellKey {
		t.Helper()
		p := DAGPair{
			A:        &DAGVertex{CallName: "stat", Path: "merge_view/d/a", RetBucket: RetSuccess, FuncID: FuncGetattr},
			B:        &DAGVertex{CallName: call, Path: "merge_view/d/a", RetBucket: RetSuccess, FuncID: fid, Off: off, Size: size},
			Temporal: TemporalHB,
			PathRel:  PathSamePath,
		}
		ck, ok := cellKeyOf(&p, "")
		if !ok {
			t.Fatalf("%s: cellKeyOf failed", call)
		}
		return ck
	}
	if ck := mk("chmod", FuncSetattr, 0, 100); ck.OffB != offsetBucketNA {
		t.Fatalf("chmod OffB = %d, want NA(%d)", ck.OffB, offsetBucketNA)
	}
	if ck := mk("write", FuncWrite, 0, 100); ck.OffB != offsetBucketZero {
		t.Fatalf("write OffB = %d, want zero(%d)", ck.OffB, offsetBucketZero)
	}
	if ck := mk("pwrite64", FuncWrite, 5000, 4096); ck.OffB != offsetBucketBeyond {
		t.Fatalf("pwrite64 OffB = %d, want beyond(%d)", ck.OffB, offsetBucketBeyond)
	}
}

// TestCellKeyRenamePrePostAnchor pins the cell anchor of a rename pair whose
// relation is established through the rename source (pre path): the mapping
// uses the pre/post basis, while the B-side dims come from the post path
// (sound because generated rename destinations are same-parent siblings).
func TestCellKeyRenamePrePostAnchor(t *testing.T) {
	p := DAGPair{
		A: &DAGVertex{CallName: "stat", Path: "merge_view/d/a",
			RetBucket: RetSuccess, FuncID: FuncGetattr},
		B: &DAGVertex{CallName: "rename", Path: "merge_view/d/sib._renamed_x",
			OldPath: "merge_view/d", RetBucket: RetSuccess, FuncID: FuncRename},
		Temporal: TemporalConcurrent,
		PathRel:  PathParentChild,
	}
	ck, ok := cellKeyOf(&p, "merge_view/persist")
	if !ok {
		t.Fatal("cellKeyOf failed for the rename Parent pair")
	}
	if ck.Root != "stat" || ck.Variant != "rename" || ck.Rel != PathParent {
		t.Fatalf("combo = %s/%s/%v, want stat/rename/Parent", ck.Root, ck.Variant, ck.Rel)
	}
	if ck.DepthB != 2 || ck.PersistB || ck.TypeB || ck.OffB != offsetBucketNA {
		t.Fatalf("dims = depth %d persist %v dir %v off %d, want 2/false/false/NA",
			ck.DepthB, ck.PersistB, ck.TypeB, ck.OffB)
	}
}

// TestCoverageDepthDomain pins the found/producible domain invariant: with a
// shallow initial tree (depth buckets {1,2} modeled, bucket 3 absent), a cell
// discovered at bucket 3 must not count towards found/cover nor be eligible as
// a 2b directed target — otherwise prod−found distorts potential/retirement.
func TestCoverageDepthDomain(t *testing.T) {
	const cid = "c1"
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	tree := NewFileTree()
	tree.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir:  map[string][]string{cid: {"merge_view/dirA"}},
		Init_file: map[string][]string{cid: {"merge_view/dirA/a"}},
	})
	InitCoverageBoard(&Hmdfs_config{Persistence_dir: "merge_view/persist", FileTree: tree})
	if covBoard.depths[3] {
		t.Fatal("bucket 3 unexpectedly modeled by the shallow test tree")
	}

	combo := comboKey{Root: "write", Variant: "write", Rel: PathSame}
	deep := mkCoveragePair("write", "write", PathSamePath, TemporalHB,
		RetSuccess, RetFailure, "merge_view/a/b/c/d/e/f")
	if got := DepthBucketOf(deep.B.Path); got != 3 {
		t.Fatalf("deep path bucket = %d, want 3", got)
	}
	ck, ok := cellKeyOf(&deep, covBoard.persistDir)
	if !ok {
		t.Fatal("cellKeyOf failed for the deep pair")
	}
	if covBoard.cellInDomain(ck) {
		t.Fatal("unmodeled-depth cell accepted by the domain gate")
	}
	if covBoard.cellUncovered(ck) {
		t.Fatal("unmodeled-depth cell accepted as a directed target")
	}
	pot0 := DirectedPairPotential(combo.Root, combo.Variant, combo.Rel)
	f0 := covBoard.found[combo]
	RecordCoveragePairs([]DAGPair{deep})
	f := covBoard.found[combo].total()
	base := f0.total()
	if f[0]+f[1] != base[0]+base[1] {
		t.Fatalf("unmodeled-depth discovery counted: found=%v, want %v", f, base)
	}
	if pot := DirectedPairPotential(combo.Root, combo.Variant, combo.Rel); pot != pot0 {
		t.Fatalf("potential changed on unmodeled-depth discovery: %d, want %d", pot, pot0)
	}

	// Control: a modeled depth (bucket 2) still counts and is targetable.
	shallow := mkCoveragePair("write", "write", PathSamePath, TemporalHB,
		RetSuccess, RetFailure, "merge_view/dirA/a")
	if got := DepthBucketOf(shallow.B.Path); got != 2 {
		t.Fatalf("control path bucket = %d, want 2", got)
	}
	ck2, ok := cellKeyOf(&shallow, covBoard.persistDir)
	if !ok {
		t.Fatal("cellKeyOf failed for the control pair")
	}
	if !covBoard.cellUncovered(ck2) {
		t.Fatal("modeled-depth cell not usable as a directed target")
	}
	RecordCoveragePairs([]DAGPair{shallow})
	f = covBoard.found[combo].total()
	if f[0]+f[1] != base[0]+base[1]+1 {
		t.Fatalf("modeled-depth discovery not counted: found=%v, want %v", f, base[0]+base[1]+1)
	}
	if pot := DirectedPairPotential(combo.Root, combo.Variant, combo.Rel); pot != pot0-1 {
		t.Fatalf("potential after modeled discovery = %d, want %d", pot, pot0-1)
	}
}

// TestCoverageNoRelExcluded pins the NoRel accounting: unrelated pairs are
// dropped by ExtractPairs (PathNone), so NoRel combos must not exist in the
// producible grid (cover= denominator / Direction-3 directed bias). NoRel
// generation itself remains on the DCT weighted path.
func TestCoverageNoRelExcluded(t *testing.T) {
	newTestCoverageBoard(t)
	ck := comboKey{Root: "write", Variant: "truncate", Rel: PathNoRel}
	if _, ok := covBoard.producible[ck]; ok {
		t.Fatal("NoRel combo present in the producible grid")
	}
	if pot := DirectedPairPotential("write", "truncate", PathNoRel); pot != 0 {
		t.Fatalf("directed potential for NoRel = %d, want 0", pot)
	}
}

// TestCoverageEventShapeIgnoresCallNames pins the event-level shape unit: the
// abstract shape hash must not depend on the call name (write/pwrite64 are the
// same low-level func) while margins still separate shapes.
func TestCoverageEventShapeIgnoresCallNames(t *testing.T) {
	shape := func(name string, ret RetBucket, off uint64) uint64 {
		p := DAGPair{
			A: &DAGVertex{CallName: "open", Path: "merge_view/dirA/a",
				RetBucket: RetSuccess, FuncID: FuncFileOpen},
			B: &DAGVertex{CallName: name, Path: "merge_view/dirA/a",
				RetBucket: ret, FuncID: FuncWrite, Size: 10000, Off: off},
			Temporal: TemporalConcurrent,
			PathRel:  PathSamePath,
		}
		return hashPair(p, nil)
	}
	if shape("write", RetSuccess, 0) != shape("pwrite64", RetSuccess, 0) {
		t.Fatal("write/pwrite64 must share the event shape")
	}
	if shape("write", RetSuccess, 0) == shape("write", RetFailure, 0) {
		t.Fatal("different ret buckets must separate shapes")
	}
	if shape("write", RetSuccess, 0) == shape("write", RetSuccess, 10) {
		t.Fatal("different offset buckets must separate shapes")
	}
}

// TestCoverageFoundModelGate pins the found gate: observations outside the
// producible dimension tables still mark the exact cell discovered, but must
// not count towards found (that would deflate the combo's potential below its
// undiscovered model cells).
func TestCoverageFoundModelGate(t *testing.T) {
	newTestCoverageBoard(t)
	found0, _ := CoverageStats()
	pot0 := DirectedPairPotential("mkdir", "stat", PathSame)
	if pot0 <= 0 {
		t.Fatalf("fixture combo has no potential: %d", pot0)
	}
	mkPair := func(ret RetBucket) DAGPair {
		return DAGPair{
			A: &DAGVertex{CallName: "mkdir", Path: "merge_view/dirA",
				RetBucket: RetSuccess, FuncID: FuncMkdir, IsDir: true},
			B: &DAGVertex{CallName: "stat", Path: "merge_view/dirA/a",
				RetBucket: ret, FuncID: FuncGetattr},
			Temporal: TemporalConcurrent,
			PathRel:  PathSamePath,
		}
	}
	// stat has no RetFailure in the producible table: out of model.
	RecordCoveragePairs([]DAGPair{mkPair(RetFailure)})
	if found1, _ := CoverageStats(); found1 != found0 {
		t.Fatalf("out-of-model observation moved found: %d -> %d", found0, found1)
	}
	if pot1 := DirectedPairPotential("mkdir", "stat", PathSame); pot1 != pot0 {
		t.Fatalf("out-of-model observation moved potential: %d -> %d", pot0, pot1)
	}
	// The in-model Success form still counts.
	RecordCoveragePairs([]DAGPair{mkPair(RetSuccess)})
	if pot2 := DirectedPairPotential("mkdir", "stat", PathSame); pot2 != pot0-1 {
		t.Fatalf("in-model observation must decrement potential: %d -> %d", pot0, pot2)
	}
	if found2, _ := CoverageStats(); found2 != found0+1 {
		t.Fatalf("in-model observation must increment found: %d -> %d", found0, found2)
	}
}

// TestCoverageVariantNameOutOfDomain pins the P2-#7 closure: call names with a
// syzkaller variant suffix (open$dir etc.) are outside the base-name grid, so
// their observations must not create found entries (which used to push cover=
// beyond target) while the exact cell stays discovered.
func TestCoverageVariantNameOutOfDomain(t *testing.T) {
	newTestCoverageBoard(t)
	found0, _ := CoverageStats()
	pot0 := DirectedPairPotential("open", "pwrite64", PathSame)
	if pot0 <= 0 {
		t.Fatalf("fixture combo has no potential: %d", pot0)
	}
	p := DAGPair{
		A: &DAGVertex{CallName: "open$dir", Path: "merge_view/dirA/a",
			RetBucket: RetSuccess, FuncID: FuncDirOpen, IsDir: true},
		B: &DAGVertex{CallName: "pwrite64", Path: "merge_view/dirA/a",
			RetBucket: RetSuccess, FuncID: FuncWrite, Size: 1000, Off: 0},
		Temporal: TemporalConcurrent,
		PathRel:  PathSamePath,
	}
	RecordCoveragePairs([]DAGPair{p})
	if found1, _ := CoverageStats(); found1 != found0 {
		t.Fatalf("variant-name observation moved found: %d -> %d", found0, found1)
	}
	if pot := DirectedPairPotential("open", "pwrite64", PathSame); pot != pot0 {
		t.Fatalf("variant-name observation moved the base combo potential: %d -> %d", pot0, pot)
	}
	if ck, ok := cellKeyOf(&p, covBoard.persistDir); !ok || covBoard.discovered[ck] == 0 {
		t.Fatal("out-of-domain cell must still be marked discovered")
	}
}

// TestCoverageTruncateENOENTModel pins the ret-conditional offset model
// (P2-#8): a failed truncate lookup is a FuncLookup event with OffB=NA, so
// truncate contributes 4 Success x bucket cells plus a single ENOENT x NA
// cell, and the previously counted unobservable ENOENT x bucket cells are
// gone from the grid.
func TestCoverageTruncateENOENTModel(t *testing.T) {
	newTestCoverageBoard(t)
	// open root is a non-modifier; truncate is a modifier -> CC only.
	// typeList {file}, depthMult 6, offCount 4 (Success) + 1 (ENOENT) = 5.
	if pot := DirectedPairPotential("open", "truncate", PathSame); pot != 30 {
		t.Fatalf("open/truncate potential = %d, want 30", pot)
	}
	if covBoard.cellInModel(cellKey{
		Root: "open", Variant: "truncate", Rel: PathSame,
		Temporal: TemporalConcurrent, RetA: RetSuccess, RetB: RetENOENT,
		OffB: offsetBucketZero, DepthB: 2, TypeB: false,
	}) {
		t.Fatal("ENOENT x bucket cell must not be in the model")
	}
	found0, _ := CoverageStats()
	p := DAGPair{
		A: &DAGVertex{CallName: "open", Path: "merge_view/dirA/a",
			RetBucket: RetSuccess, FuncID: FuncFileOpen},
		// Failed lookup: the event is FuncLookup, so offsetBucketOf is NA even
		// though the call name carries offset semantics.
		B: &DAGVertex{CallName: "truncate", Path: "merge_view/dirA/a",
			RetBucket: RetENOENT, FuncID: FuncLookup},
		Temporal: TemporalConcurrent,
		PathRel:  PathSamePath,
	}
	RecordCoveragePairs([]DAGPair{p})
	if found1, _ := CoverageStats(); found1 != found0+1 {
		t.Fatalf("truncate ENOENT observation must count: found %d -> %d", found0, found1)
	}
	if pot := DirectedPairPotential("open", "truncate", PathSame); pot != 29 {
		t.Fatalf("potential after truncate ENOENT = %d, want 29", pot)
	}
}

// TestCoverageRootDirectedPotentialsCellSuppressed pins the 2b-available
// potential: rootDirectedPotentials subtracts the cells the 2b miss ledger
// currently suppresses
// (a fully cell-suppressed combo reads 0 immediately), while the generic
// DirectedPairPotential view keeps the raw directed-pair potential.
func TestCoverageRootDirectedPotentialsCellSuppressed(t *testing.T) {
	newTestCoverageBoard(t)
	ck := comboKey{Root: "write", Variant: "write", Rel: PathSame}
	pot0 := DirectedPairPotential(ck.Root, ck.Variant, ck.Rel)
	if pot0 <= 0 {
		t.Fatalf("fixture combo has no potential: %d", pot0)
	}
	if got := covBoard.rootDirectedPotentials([]comboKey{ck})[0]; got != pot0 {
		t.Fatalf("available potential = %d, want %d", got, pot0)
	}
	var cells []cellKey
	covBoard.forEachUncoveredCellKey(ck, func(c cellKey) { cells = append(cells, c) })
	if len(cells) == 0 {
		t.Fatal("fixture combo has no uncovered cells")
	}
	for _, c := range cells {
		for i := 0; i < cellMissThreshold; i++ {
			covBoard.noteDirectedMiss(c)
		}
	}
	if got := covBoard.cellSuppressed[ck]; got != len(cells) {
		t.Fatalf("suppressed counter = %d, want %d", got, len(cells))
	}
	if got := covBoard.rootDirectedPotentials([]comboKey{ck})[0]; got != 0 {
		t.Fatalf("fully suppressed combo available potential = %d, want 0", got)
	}
	if got := DirectedPairPotential(ck.Root, ck.Variant, ck.Rel); got != pot0 {
		t.Fatalf("2a model potential moved: %d -> %d", pot0, got)
	}
	// A hit on a suppressed cell discovers it (found+1) and clears its
	// suppression (suppressed-1): the two cancel, so the cell remains
	// unavailable as a plan target; the raw 2a model potential drops by the
	// one now-discovered cell (it ignores suppression but subtracts found).
	covBoard.noteDirectedHit(cells[0])
	if got := covBoard.rootDirectedPotentials([]comboKey{ck})[0]; got != 0 {
		t.Fatalf("available potential after one hit = %d, want 0", got)
	}
	if got := covBoard.cellSuppressed[ck]; got != len(cells)-1 {
		t.Fatalf("suppressed counter after hit = %d, want %d", got, len(cells)-1)
	}
	if got := DirectedPairPotential(ck.Root, ck.Variant, ck.Rel); got != pot0-1 {
		t.Fatalf("2a model potential after hit: %d -> %d, want %d", pot0, got, pot0-1)
	}
	// A discovered cell can no longer be re-suppressed (noteDirectedMiss
	// guards on discovered != 0), so availability stays 0.
	for i := 0; i < cellMissThreshold; i++ {
		covBoard.noteDirectedMiss(cells[0])
	}
	if got := covBoard.rootDirectedPotentials([]comboKey{ck})[0]; got != 0 {
		t.Fatalf("available potential after re-suppression = %d, want 0", got)
	}
}

// TestDirectedMissSkipsDiscoveredCell pins the discovered-cell guard: a cell
// observed before its pending batch resolved (a cross-proc race) must not be
// charged as a miss nor suppressed.
func TestDirectedMissSkipsDiscoveredCell(t *testing.T) {
	newTestCoverageBoard(t)
	p := mkCoveragePair("mkdir", "write", PathSamePath, TemporalHB,
		RetSuccess, RetFailure, "merge_view/persist/f")
	RecordCoveragePairs([]DAGPair{p})
	ck, ok := cellKeyOf(&p, covBoard.persistDir)
	if !ok || covBoard.discovered[ck] == 0 {
		t.Fatal("fixture cell not discovered")
	}
	_, misses0, _, _, _ := DirectedCellStats()
	for i := 0; i < cellMissThreshold+2; i++ {
		covBoard.noteDirectedMiss(ck)
	}
	if _, misses, _, _, _ := DirectedCellStats(); misses != misses0 {
		t.Fatalf("discovered cell charged misses: %d -> %d", misses0, misses)
	}
	if covBoard.cellUnreachable[ck] || covBoard.cellAttempts[ck] != 0 {
		t.Fatal("discovered cell must not be suppressed nor keep attempts")
	}
}

// TestCoverageFoundTemporalGate pins the temporal-form gate: an open(O_CREAT)
// event carries FuncCreate, so ExtractPairs (func-level modifiers) can form
// HB/CC pairs whose call name "open" has no modifier role in the grid
// (prod[form]=0). Such observations must not count towards found, or cover=
// can exceed target; the exact cell is still marked discovered.
func TestCoverageFoundTemporalGate(t *testing.T) {
	newTestCoverageBoard(t)
	found0, _ := CoverageStats()
	pair := func(aName string, aFunc uint32, bName string, bFunc uint32, temporal TemporalRel) DAGPair {
		return DAGPair{
			A:        &DAGVertex{CallName: aName, Path: "merge_view/dirA/a", RetBucket: RetSuccess, FuncID: aFunc},
			B:        &DAGVertex{CallName: bName, Path: "merge_view/dirA/a", RetBucket: RetSuccess, FuncID: bFunc},
			Temporal: temporal,
			PathRel:  PathSamePath,
		}
	}
	// open(O_CREAT) root: extractor sees a succeeded modifier (FuncCreate)
	// while the grid combo (open, read) has prod=[0,0] for both forms.
	leakHB := pair("open", FuncCreate, "read", FuncRead, TemporalHB)
	leakCC := pair("open", FuncCreate, "read", FuncRead, TemporalConcurrent)
	RecordCoveragePairs([]DAGPair{leakHB, leakCC})
	if found1, _ := CoverageStats(); found1 != found0 {
		t.Fatalf("temporal-out-of-model observations moved found: %d -> %d", found0, found1)
	}
	if ck, ok := cellKeyOf(&leakHB, covBoard.persistDir); !ok || covBoard.discovered[ck] == 0 {
		t.Fatal("out-of-model cell must still be marked discovered")
	}
	// Controls: call-level modifier sides count in their producible form.
	RecordCoveragePairs([]DAGPair{pair("write", FuncWrite, "read", FuncRead, TemporalHB)})
	RecordCoveragePairs([]DAGPair{pair("read", FuncRead, "write", FuncWrite, TemporalConcurrent)})
	if found2, _ := CoverageStats(); found2 != found0+2 {
		t.Fatalf("in-model temporal observations must count: %d -> %d", found0, found2)
	}
}

// addKnownCellShapes white-box-registers every candidate shape of one cell
// under an A context (the test mirror of cellShapeKnown's enumeration).
func addKnownCellShapes(cell cellKey, aFunc uint32, aDepth uint64, aIsDir, aPersist bool, rel PathRel) {
	bFuncs, ok := predictedEventFuncs(hmdfsProfile, cell.Variant, cell.TypeB, cell.RetB, cell.Temporal)
	if !ok {
		return
	}
	for _, bFunc := range bFuncs {
		bf := packFeatures(bFunc, cell.RetB, cell.DepthB, cell.TypeB, cell.PersistB, cell.OffB)
		for _, aOff := range rootOffsetBuckets(cell.Root, false) {
			af := packFeatures(aFunc, RetSuccess, aDepth, aIsDir, aPersist, aOff)
			covBoard.knownShapes[hashShape(cell.Temporal, rel, af, bf)] = true
		}
	}
}

// TestCoverageShapeDrainCell pins the Direction 3 drain predicate: a combo is
// drained only when every uncovered cell maps to an already-known shape for
// the given A context; the memo follows the board's shape epoch (a late shape
// invalidates a cached "not drained"), twins (write/pwrite64) drain together,
// and a different A context is never drained by another context's shapes.
func TestCoverageShapeDrainCell(t *testing.T) {
	newTestCoverageBoard(t)
	combo := comboKey{Root: "open", Variant: "pwrite64", Rel: PathSame}
	const aDepth = 2
	// Populate every candidate shape but the first enumerated cell's.
	var missing cellKey
	first := true
	covBoard.forEachUncoveredCellKey(combo, func(cell cellKey) {
		if first {
			first = false
			missing = cell
			return
		}
		addKnownCellShapes(cell, FuncFileOpen, aDepth, false, false, PathSamePath)
	})
	if first {
		t.Fatal("fixture combo has no uncovered cells")
	}
	if missing.RetB != RetSuccess || missing.OffB != offsetBucketZero || missing.DepthB != 1 ||
		missing.PersistB || missing.TypeB || missing.Temporal != TemporalConcurrent {
		t.Fatalf("first enumerated cell = %+v, want (Success, Zero, depth 1, file, non-persist, CC)", missing)
	}
	if covBoard.shapeDrained(combo, FuncFileOpen, aDepth, false, false, PathSamePath) {
		t.Fatal("combo with a missing shape reported drained")
	}
	// Deliver the last shape through the production path: the new shape bumps
	// the epoch, so the memoized "not drained" must be recomputed.
	last := DAGPair{
		A:        &DAGVertex{CallName: "open", Path: "merge_view/d/a", RetBucket: RetSuccess, FuncID: FuncFileOpen},
		B:        &DAGVertex{CallName: "pwrite64", Path: "merge_view/d", RetBucket: missing.RetB, FuncID: FuncWrite, Size: 1000, Off: 0},
		Temporal: missing.Temporal,
		PathRel:  PathSamePath,
	}
	RecordCoveragePairs([]DAGPair{last})
	if !covBoard.shapeDrained(combo, FuncFileOpen, aDepth, false, false, PathSamePath) {
		t.Fatal("combo not drained after the last shape arrived")
	}
	if !covBoard.shapeDrained(comboKey{Root: "open", Variant: "write", Rel: PathSame},
		FuncFileOpen, aDepth, false, false, PathSamePath) {
		t.Fatal("write twin combo must share the drain state")
	}
	if covBoard.shapeDrained(combo, FuncFileOpen, aDepth+1, false, false, PathSamePath) {
		t.Fatal("different A depth must not be drained by another context's shapes")
	}
}

// TestCoverageShapeDrainFallback pins the generation-side Direction 3 guard:
// once every non-"open" candidate combo is shape-drained for the base path,
// directed picks fall back to the static sampler; the flags-ambiguous "open"
// variant stays directed (conservative), so only open picks may be counted.
func TestCoverageShapeDrainFallback(t *testing.T) {
	newTestCoverageBoard(t)
	dct := NewDistributedChoiceTable()
	r := rand.New(rand.NewSource(1))
	basePath := "merge_view/dirA"
	relOK := map[int]map[PathRelation]bool{
		relTargetAny:  {PathSame: true},
		relTargetFile: {PathSame: true},
		relTargetDir:  {PathSame: true},
	}
	// Control: unknown shapes on a fresh board keep the directed behavior.
	before, _ := DirectedPickStats()
	for i := 0; i < 50; i++ {
		if v := dct.ChooseVariantFiltered("mkdir", r, false, basePath, relOK, false, true); v == nil {
			t.Fatal("ChooseVariantFiltered returned nil")
		}
	}
	if after, _ := DirectedPickStats(); after == before {
		t.Fatal("fixture: no directed picks on a fresh board")
	}
	// Clear the control phase's pick-time accounting: no execution ran, so
	// hot combos may have been marked unreachable, which would let the second
	// phase pass vacuously.
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{Persistence_dir: "merge_view/persist"})
	aDepth := DepthBucketOf(basePath)
	for _, cv := range dct.Variants["mkdir"] {
		rel := dagRelOfPathRelation(cv.PathRelation)
		if rel == PathNone {
			continue
		}
		ck := comboKey{Root: "mkdir", Variant: cv.CallName, Rel: cv.PathRelation}
		covBoard.forEachUncoveredCellKey(ck, func(cell cellKey) {
			addKnownCellShapes(cell, FuncMkdir, aDepth, true, false, rel)
		})
	}
	// Cross-check the guard, not saturation: a non-open combo still has cell
	// potential, yet its candidate shapes are all known, so the skipped
	// directed budget is the guard's doing.
	if pot := DirectedPairPotential("mkdir", "stat", PathSame); pot == 0 {
		t.Fatal("fixture: drained combo has no cell potential")
	}
	if !covBoard.shapeDrained(comboKey{Root: "mkdir", Variant: "stat", Rel: PathSame},
		FuncMkdir, aDepth, true, false, PathSamePath) {
		t.Fatal("non-open combo with potential must report drained")
	}
	// Drained combos must never spend a directed pick.
	before, _ = DirectedPickStats()
	for i := 0; i < 200; i++ {
		v := dct.ChooseVariantFiltered("mkdir", r, false, basePath, relOK, false, true)
		if v == nil {
			t.Fatal("ChooseVariantFiltered returned nil")
		}
		after, _ := DirectedPickStats()
		if after != before {
			before = after
			if v.CallName != "open" {
				t.Fatalf("directed pick on a drained combo: %q/%v", v.CallName, v.PathRelation)
			}
		}
	}
}

// TestCoverageShapeDrainConcurrent pins the drain memo's board-level locking:
// several proc-like goroutines each own a DCT but share the global board
// (the fuzzer generates per VM in parallel), so a read-locked cache write is
// caught by -race / "concurrent map read and map write".
func TestCoverageShapeDrainConcurrent(t *testing.T) {
	newTestCoverageBoard(t)
	relOK := map[int]map[PathRelation]bool{
		relTargetAny:  {PathSame: true},
		relTargetFile: {PathSame: true},
		relTargetDir:  {PathSame: true},
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			dct := NewDistributedChoiceTable()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 200; i++ {
				if v := dct.ChooseVariantFiltered("mkdir", r, false, "merge_view/dirA", relOK, false, true); v == nil {
					t.Errorf("ChooseVariantFiltered returned nil")
					return
				}
			}
		}(int64(g + 1))
	}
	wg.Wait()
}

// TestRecordCoveragePairsTwinCombos pins the board-learning granularity: two
// pairs sharing the abstract shape/novelty bit (write and pwrite64 both map to
// FuncWrite) but mapping to different cells must both be recorded by one
// observation pass, and a repeated pass must not double-count the ledgers.
func TestRecordCoveragePairsTwinCombos(t *testing.T) {
	newTestCoverageBoard(t)
	writePair := mkCoveragePair("write", "stat", PathSamePath, TemporalHB, RetSuccess, RetSuccess, "merge_view/d/a")
	pwritePair := mkCoveragePair("pwrite64", "stat", PathSamePath, TemporalHB, RetSuccess, RetSuccess, "merge_view/d/a")

	// Premise: one shared shape/novelty bit, two different cells.
	if h1, h2 := hashPair(writePair, nil), hashPair(pwritePair, nil); h1 != h2 {
		t.Fatalf("twin pairs do not share a shape/novelty bit: %#x vs %#x", h1, h2)
	}
	wck, ok := cellKeyOf(&writePair, covBoard.persistDir)
	if !ok {
		t.Fatal("write cell key not resolvable")
	}
	pck, ok := cellKeyOf(&pwritePair, covBoard.persistDir)
	if !ok {
		t.Fatal("pwrite64 cell key not resolvable")
	}
	if wck == pck {
		t.Fatal("twin pairs unexpectedly share a cell")
	}

	RecordCoveragePairs([]DAGPair{writePair, pwritePair})
	covBoard.mu.RLock()
	wd, pd := covBoard.discovered[wck], covBoard.discovered[pck]
	wf := covBoard.found[comboKey{Root: "write", Variant: "stat", Rel: PathSame}]
	pf := covBoard.found[comboKey{Root: "pwrite64", Variant: "stat", Rel: PathSame}]
	shapes := len(covBoard.knownShapes)
	covBoard.mu.RUnlock()
	if wd == 0 || pd == 0 {
		t.Fatalf("twin cells not both discovered: write=%d pwrite64=%d", wd, pd)
	}
	if shapes != 1 {
		t.Fatalf("twin shapes = %d, want 1 (same abstract shape)", shapes)
	}
	wft, pft := wf.total(), pf.total()
	if wft[0]+wft[1] == 0 || pft[0]+pft[1] == 0 {
		t.Fatalf("twin combos not both found: write=%v pwrite64=%v", wf, pf)
	}

	// A repeated observation must not double-count found.
	RecordCoveragePairs([]DAGPair{writePair, pwritePair})
	covBoard.mu.RLock()
	wf2 := covBoard.found[comboKey{Root: "write", Variant: "stat", Rel: PathSame}]
	pf2 := covBoard.found[comboKey{Root: "pwrite64", Variant: "stat", Rel: PathSame}]
	covBoard.mu.RUnlock()
	if wf2 != wf || pf2 != pf {
		t.Fatalf("repeat observation double-counted found: write %v->%v pwrite %v->%v", wf, wf2, pf, pf2)
	}
}

// TestNoteDirectedHitMarksDiscovered pins the 2b safety net: a hit on a cell
// whose pair never reached recordPairs still retires it from plan enumeration.
func TestNoteDirectedHitMarksDiscovered(t *testing.T) {
	newTestCoverageBoard(t)
	// In-model combo: the former mkdir x write Same pair is A1-masked, so
	// cellUncovered now rejects it before the hit is ever recorded.
	p := mkCoveragePair("write", "truncate", PathSamePath, TemporalHB, RetSuccess, RetENOENT, "merge_view/d/f")
	ck, ok := cellKeyOf(&p, covBoard.persistDir)
	if !ok {
		t.Fatal("cell key not resolvable")
	}
	if !covBoard.cellUncovered(ck) {
		t.Fatal("cell should start uncovered")
	}
	covBoard.noteDirectedHit(ck)
	if covBoard.cellUncovered(ck) {
		t.Fatal("cell still uncovered after a hit")
	}
}
