package prog

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// directedPlanFixture prepares the global board with the smoke config and
// returns the target/hmcfg/sCalls/LCS for the directed-plan tests.
func directedPlanFixture(t *testing.T) (*Target, *Hmdfs_config, *SpecialCalls, *LayeredChoiceStrategy) {
	t.Helper()
	target := hmdfsSmokeTarget(t)
	hmcfg := &Hmdfs_config{}
	hmdfsSmokeConfig(hmcfg)
	sc := hmdfsSmokeSpecialCalls(t, target)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ResetCoverageBoard()
	InitCoverageBoard(hmcfg)
	t.Cleanup(ResetCoverageBoard)
	return target, hmcfg, sc, lcs
}

func directedPlanBaseProgs(target *Target) []*Prog {
	ps := make([]*Prog, 0, 3)
	for i := 0; i < 3; i++ {
		o := mkOpen(target, "merge_view/dirA/a")
		c := mkClose(target, o)
		// Timing anchors: directed insertion aligns variants to the root
		// reference, which needs at least one executed call per prog.
		o.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		c.CheckInfo = &FileMetadata{Stime: 210, Etime: 260}
		ps = append(ps, &Prog{Target: target, Calls: []*Call{o, c}})
	}
	return ps
}

func directedCellCounts() (hits, misses uint64) {
	_, h, m, _, _ := DirectedCellStats()
	return h, m
}

// TestDirectedTemplateScope pins the planned template boundaries.
func TestDirectedTemplateScope(t *testing.T) {
	if _, ok := directedVariantKinds("mkdir"); !ok {
		t.Fatal("mkdir must be in scope (fresh-name variant)")
	}
	kinds, _ := directedVariantKinds("mkdir")
	if len(kinds) != 2 || kinds[1].retB != RetEEXIST {
		t.Fatalf("mkdir kinds must be Success/EEXIST, got %v", kinds)
	}
	if _, ok := directedVariantKinds("rmdir"); !ok {
		t.Fatal("rmdir must be in scope")
	}
	if _, ok := directedVariantKinds("unlink"); !ok {
		t.Fatal("unlink must be in scope")
	}
	if _, ok := directedVariantKinds("creat"); !ok {
		t.Fatal("creat must be in scope (fresh-name variant)")
	}
	if _, ok := directedVariantKinds("pwrite64"); !ok {
		t.Fatal("pwrite64 must be in scope")
	}
	renameKinds, ok := directedVariantKinds("rename")
	if !ok || len(renameKinds) != 2 {
		t.Fatalf("rename must expose exactly its two dual-path kinds, got %v/%v", renameKinds, ok)
	}
	if renameKinds[0].retB != RetSuccess || renameKinds[1].retB != RetFailure {
		t.Fatalf("rename kinds must be Success/Failure, got %v", renameKinds)
	}
	if !isModifierCall("chmod") || !isModifierCall("write") ||
		isModifierCall("read") || isModifierCall("stat") {
		t.Fatal("modifier mapping wrong")
	}
}

// TestRealizeDirectedParams pins the bucket recomputation rule: every realized
// offset/length maps back to the requested bucket via bucketOfOffset.
func TestRealizeDirectedParams(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	r := newRand(target, rand.NewSource(1))
	node := func(size uint64) directedNode {
		return directedNode{path: "merge_view/dirA/a", size: size}
	}
	sizes := []uint64{0, 1, 100, 4095, 4096, 5000, 8192, 12345}
	buckets := []uint64{offsetBucketZero, offsetBucketMid, offsetBucketTail, offsetBucketBeyond}
	for _, variant := range []string{"pread64", "pwrite64", "truncate"} {
		for _, size := range sizes {
			for _, bucket := range buckets {
				off, blen, ok := realizeDirectedParams(variant, bucket, node(size), r)
				if !ok {
					continue
				}
				pos := off
				if variant == "truncate" {
					pos = blen
				}
				if got := bucketOfOffset(size, pos); got != bucket {
					t.Fatalf("%s size=%d bucket=%d realized pos=%d -> bucket %d",
						variant, size, bucket, pos, got)
				}
			}
		}
	}
	for _, variant := range []string{"read", "write"} {
		if _, _, ok := realizeDirectedParams(variant, offsetBucketMid, node(5000), r); ok {
			t.Fatalf("%s must not realize a non-zero bucket", variant)
		}
		if _, _, ok := realizeDirectedParams(variant, offsetBucketZero, node(5000), r); !ok {
			t.Fatalf("%s must realize zero", variant)
		}
	}
	for _, variant := range []string{"open", "stat", "chmod", "fsync", "fdatasync", "getdents64", "rmdir"} {
		if _, _, ok := realizeDirectedParams(variant, offsetBucketNA, node(0), r); !ok {
			t.Fatalf("%s must realize NA", variant)
		}
	}
	// Sub-page files have no separate tail page: any length in [1, size-1]
	// classifies as Tail, and the shared truncate realizer must accept it (the
	// data-mutation arm previously rejected the whole bucket).
	for _, size := range []uint64{2, 10, 4095} {
		l, ok := realizeDirectedTruncateLen(r, size, offsetBucketTail)
		if !ok {
			t.Fatalf("sub-page size=%d: Tail truncate not realizable", size)
		}
		if got := bucketOfOffset(size, l); got != offsetBucketTail {
			t.Fatalf("sub-page size=%d: realized len=%d -> bucket %d", size, l, got)
		}
	}
	// The shared offset realizer is deterministic at size 0: Beyond is the
	// only realizable non-zero bucket (the sampler itself can draw EOF, which
	// bucketOfOffset classifies as Zero).
	if off, ok := realizeDirectedOffset(r, 0, offsetBucketBeyond); !ok || off != 4096 {
		t.Fatalf("size-0 Beyond = (%d,%v), want (4096,true)", off, ok)
	}
	if _, ok := realizeDirectedOffset(r, 0, offsetBucketMid); ok {
		t.Fatal("size-0 Mid must not be realizable")
	}
	// Tail at size 0 must not underflow (it used to return MaxUint64 via the
	// size-1 degenerate path).
	if off := r.randOffsetInBucket(0, offsetBucketTail); off != 0 {
		t.Fatalf("size-0 Tail sampler = %d, want 0", off)
	}
	if _, ok := realizeDirectedOffset(r, 0, offsetBucketTail); ok {
		t.Fatal("size-0 Tail must not be realizable")
	}
}

// TestPickDirectedBatchBuildsConstructiblePlan checks the batch picker with
// the smoke tree: every plan shares the batch root call and base path, NodeX
// is 1..M, the batch never exceeds the node budget, and each plan is
// uncovered, geometrically consistent, with realized parameters matching the
// predicted cell.
func TestPickDirectedBatchBuildsConstructiblePlan(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	found, cfgErrs := 0, 0
	for seed := int64(0); seed < 50 && found < 5; seed++ {
		r := newRand(target, rand.NewSource(seed))
		batch, status := PickDirectedBatch(ps, lcs, hmcfg, r)
		if status == directedPlanCfgErr {
			cfgErrs++
		}
		if batch == nil {
			continue
		}
		found++
		if len(batch.Plans) == 0 || len(batch.Plans) > len(ps)-1 {
			t.Fatalf("batch size %d out of range 1..%d", len(batch.Plans), len(ps)-1)
		}
		seen := make(map[cellKey]bool, len(batch.Plans))
		for i := range batch.Plans {
			plan := &batch.Plans[i]
			if plan.NodeX != i+1 {
				t.Fatalf("plan %d NodeX = %d, want %d", i, plan.NodeX, i+1)
			}
			if plan.Cell.Combo.Root != batch.Root || plan.BaseA != batch.BaseA {
				t.Fatalf("plan %d does not share the batch root (%s/%s)", i, batch.Root, batch.BaseA)
			}
			if ck := plan.Cell.toKey(); seen[ck] {
				t.Fatalf("duplicate cell in batch: %+v", plan.Cell)
			} else {
				seen[ck] = true
			}
			if !covBoard.cellUncovered(plan.Cell.toKey()) {
				t.Fatalf("plan targets a covered/suppressed cell: %+v", plan.Cell)
			}
			if plan.FreshA || plan.FreshA2 || plan.FreshB || plan.FreshSame {
				// Fresh-path plans are covered by their own tests; the shared
				// batch invariants above already apply.
				continue
			}
			a := hmcfg.FileTree.FindNode(plan.BaseA)
			if a == nil {
				t.Fatalf("plan BaseA missing from tree: %q", plan.BaseA)
			}
			if DepthBucketOf(plan.BaseB) != plan.Cell.DepthB {
				t.Fatalf("depth mismatch for %q", plan.BaseB)
			}
			if plan.Cell.PersistB != (hmcfg.Persistence_dir != "" && strings.HasPrefix(plan.BaseB, hmcfg.Persistence_dir)) {
				t.Fatalf("persist mismatch for %q", plan.BaseB)
			}
			if plan.Cell.Combo.Variant == "rename" {
				src := hmcfg.FileTree.FindNode(plan.BaseB2)
				if src == nil {
					t.Fatalf("rename source missing from tree: %q", plan.BaseB2)
				}
				if GetParentDir(plan.BaseB2) != GetParentDir(plan.BaseB) {
					t.Fatalf("rename pair not same-parent: %q %q", plan.BaseB2, plan.BaseB)
				}
				if plan.Cell.RetB == RetSuccess {
					if hmcfg.FileTree.FindNode(plan.BaseB) != nil {
						t.Fatalf("fresh rename dst must not exist: %q", plan.BaseB)
					}
					if plan.Cell.TypeB != (src.Type != NodeTypeFile) {
						t.Fatalf("fresh rename TypeB = %v, src type %v", plan.Cell.TypeB, src.Type)
					}
				} else if plan.Cell.RetB == RetFailure {
					dst := hmcfg.FileTree.FindNode(plan.BaseB)
					if dst == nil || dst.Type != NodeTypeDir || len(dst.Children) == 0 || !plan.Cell.TypeB {
						t.Fatalf("enotempty rename dst must be a non-empty dir: %q", plan.BaseB)
					}
				} else {
					t.Fatalf("unexpected rename RetB %v", plan.Cell.RetB)
				}
				if plan.Cell.Combo.Rel == PathSame {
					if plan.BaseA != plan.BaseB2 {
						t.Fatalf("rename Same must anchor A on the source: %q/%q", plan.BaseA, plan.BaseB2)
					}
				} else {
					if plan.BaseA == plan.BaseB || plan.BaseA == plan.BaseB2 {
						t.Fatalf("rename non-Same A must differ from src/dst: %q", plan.BaseA)
					}
					if got := pathRelBetween(plan.BaseA, plan.BaseB); got != plan.Cell.Combo.Rel {
						t.Fatalf("relation mismatch: %v vs %v", got, plan.Cell.Combo.Rel)
					}
				}
				continue
			}
			b := hmcfg.FileTree.FindNode(plan.BaseB)
			if b == nil {
				t.Fatalf("plan BaseB missing from tree: %q", plan.BaseB)
			}
			if got := pathRelBetween(plan.BaseA, plan.BaseB); got != plan.Cell.Combo.Rel {
				t.Fatalf("relation mismatch: %v vs %v", got, plan.Cell.Combo.Rel)
			}
			if (b.Type != NodeTypeFile) != plan.Cell.TypeB {
				t.Fatalf("type mismatch: node=%v cell dir=%v", b.Type, plan.Cell.TypeB)
			}
			if offsetVariants[plan.Cell.Combo.Variant] {
				pos := plan.BOffset
				if plan.Cell.Combo.Variant == "truncate" {
					pos = plan.BLen
				}
				if got := bucketOfOffset(b.Size, pos); got != plan.Cell.OffB {
					t.Fatalf("%s realized bucket %d, want %d", plan.Cell.Combo.Variant, got, plan.Cell.OffB)
				}
			}
		}
	}
	if found == 0 {
		t.Fatalf("no constructible batch sampled in 50 seeds (cfgErr=%d)", cfgErrs)
	}
}

// TestPickDirectedBatchCfgErr checks the planner classifies precondition/
// config anomalies separately from "nothing to target": an empty or
// length-mismatched hmcfg.Cids is cfgErr, while the healthy smoke fixture is
// never classified as cfgErr.
func TestPickDirectedBatchCfgErr(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	r := newRand(target, rand.NewSource(1))

	badEmpty := *hmcfg
	badEmpty.Cids = nil
	if batch, st := PickDirectedBatch(ps, lcs, &badEmpty, r); batch != nil || st != directedPlanCfgErr {
		t.Fatalf("empty Cids: batch=%v status=%v, want nil/cfgErr", batch, st)
	}
	badShort := *hmcfg
	badShort.Cids = hmcfg.Cids[:len(hmcfg.Cids)-1]
	if batch, st := PickDirectedBatch(ps, lcs, &badShort, r); batch != nil || st != directedPlanCfgErr {
		t.Fatalf("short Cids: batch=%v status=%v, want nil/cfgErr", batch, st)
	}
	if _, st := PickDirectedBatch(ps, lcs, hmcfg, r); st == directedPlanCfgErr {
		t.Fatal("healthy fixture classified as cfgErr")
	}
}

// TestInsertDirectedBatchCommitsAndPends checks that a successful insertion
// commits every node and records the exact pending cells, and that an invalid
// plan aborts the whole batch without touching ps.
func TestInsertDirectedBatchCommitsAndPends(t *testing.T) {
	target, hmcfg, sc, lcs := directedPlanFixture(t)
	var (
		batch *directedBatch
		ps    []*Prog
	)
	for seed := int64(0); seed < 200 && batch == nil; seed++ {
		ps = directedPlanBaseProgs(target)
		r := newRand(target, rand.NewSource(seed))
		cand, _ := PickDirectedBatch(ps, lcs, hmcfg, r)
		if cand == nil {
			continue
		}
		r2 := newRand(target, rand.NewSource(seed+1000))
		if !insertDirectedBatch(ps, r2, sc, hmcfg, lcs, cand) {
			continue
		}
		batch = cand
	}
	if batch == nil {
		t.Fatal("no batch could be inserted across 200 seeds with the smoke tree")
	}
	if len(lcs.PendingDirected) != len(batch.Plans) {
		t.Fatalf("pending batch size = %d, want %d", len(lcs.PendingDirected), len(batch.Plans))
	}
	for i := range lcs.PendingDirected {
		if lcs.PendingDirected[i].Cell.Combo != batch.Plans[i].Cell.Combo {
			t.Fatalf("pending %d does not match the batch plan", i)
		}
	}
	if plans, _, _, _, _ := DirectedCellStats(); plans != uint64(len(batch.Plans)) {
		t.Fatalf("directed cell counter = %d, want %d", plans, len(batch.Plans))
	}
	if batches := DirectedBatchStats(); batches != 1 {
		t.Fatalf("directed batch counter = %d, want 1", batches)
	}
	// Invalid batch (bad NodeX): no mutation and the pending batch is left
	// exactly as the successful insertion recorded it.
	saved := make([]int, len(ps))
	for i := range ps {
		saved[i] = len(ps[i].Calls)
	}
	pendingBefore := append([]DirectedPending(nil), lcs.PendingDirected...)
	bad := *batch
	bad.Plans = append([]directedPlan(nil), batch.Plans...)
	bad.Plans[0].NodeX = 0
	if insertDirectedBatch(ps, newRand(target, rand.NewSource(8)), sc, hmcfg, lcs, &bad) {
		t.Fatal("invalid nodeX accepted")
	}
	for i := range ps {
		if len(ps[i].Calls) != saved[i] {
			t.Fatalf("ps[%d] mutated on failed insertion", i)
		}
	}
	if len(lcs.PendingDirected) != len(pendingBefore) {
		t.Fatal("failed insertion modified the pending batch")
	}
	for i := range pendingBefore {
		if lcs.PendingDirected[i].Cell != pendingBefore[i].Cell {
			t.Fatalf("pending cell %d changed on failed insertion", i)
		}
	}
}

// TestResolveDirectedPending covers hit/miss/non-executed settlement of a
// pending batch.
func TestResolveDirectedPending(t *testing.T) {
	_, _, _, lcs := directedPlanFixture(t)
	pair := DAGPair{
		A:        &DAGVertex{CallName: "stat", Path: "merge_view/dirA/a", RetBucket: RetSuccess, FuncID: FuncGetattr},
		B:        &DAGVertex{CallName: "stat", Path: "merge_view/dirA/a", RetBucket: RetSuccess, FuncID: FuncGetattr},
		Temporal: TemporalConcurrent,
		PathRel:  PathSamePath,
	}
	ck, ok := cellKeyOf(&pair, "")
	if !ok {
		t.Fatal("cellKeyOf failed")
	}
	cell := directedCell{
		Combo:    comboKey{Root: "stat", Variant: "stat", Rel: PathSame},
		Temporal: ck.Temporal, RetB: ck.RetB, OffB: ck.OffB,
		PersistB: ck.PersistB, DepthB: ck.DepthB, TypeB: ck.TypeB,
	}
	other := cell
	other.Combo.Variant = "read"
	if other.toKey() == ck {
		t.Fatal("test cells must differ")
	}
	hit0, miss0 := directedCellCounts()

	// Batch of two: the observed pair hits one cell, the other misses.
	lcs.PendingDirected = []DirectedPending{{Cell: cell}, {Cell: other}}
	lcs.ResolveDirectedPending([]DAGPair{pair}, "", true)
	if len(lcs.PendingDirected) != 0 {
		t.Fatal("pending not cleared after resolution")
	}
	if h, m := directedCellCounts(); h != hit0+1 || m != miss0+1 {
		t.Fatalf("hit/miss counters = %d/%d, want %d/%d", h, m, hit0+1, miss0+1)
	}
	if covBoard.cellAttempts[other.toKey()] != 1 {
		t.Fatalf("missed cell attempts = %d, want 1", covBoard.cellAttempts[other.toKey()])
	}
	if covBoard.cellAttempts[ck] != 0 {
		t.Fatalf("hit cell attempts = %d, want 0", covBoard.cellAttempts[ck])
	}

	// Executed empty round: the remaining cell is a miss.
	lcs.PendingDirected = []DirectedPending{{Cell: other}}
	lcs.ResolveDirectedPending(nil, "", true)
	if len(lcs.PendingDirected) != 0 {
		t.Fatal("pending not cleared on miss")
	}
	if h, m := directedCellCounts(); h != hit0+1 || m != miss0+2 {
		t.Fatalf("miss counters = %d/%d, want %d/%d", h, m, hit0+1, miss0+2)
	}
	if covBoard.cellAttempts[other.toKey()] != 2 {
		t.Fatalf("cell attempts = %d, want 2", covBoard.cellAttempts[other.toKey()])
	}

	// Non-executed round: clears without counting.
	lcs.PendingDirected = []DirectedPending{{Cell: other}}
	lcs.ResolveDirectedPending(nil, "", false)
	if len(lcs.PendingDirected) != 0 {
		t.Fatal("pending not cleared on non-executed round")
	}
	if h, m := directedCellCounts(); h != hit0+1 || m != miss0+2 {
		t.Fatalf("non-executed counted as a miss: %d/%d", h, m)
	}
}

// TestValidateDirectedPlanLive pins the insert-time recheck: every drifted tree
// assumption (missing base, size change moving the offset bucket, empty rmdir
// target) rejects the plan before any mutation, so a drifted plan is charged as
// insertFail instead of a spurious cell miss.
func TestValidateDirectedPlanLive(t *testing.T) {
	const (
		filePath = "merge_view/dirA/a"
		dirPath  = "merge_view/dirA"
	)
	filePlan := func() *directedPlan {
		const size, off = uint64(5000), uint64(4500)
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: "write", Variant: "pwrite64", Rel: PathSame},
				RetB:     RetSuccess,
				OffB:     bucketOfOffset(size, off),
				PersistB: false,
				DepthB:   DepthBucketOf(filePath),
				TypeB:    false,
			},
			NodeX: 1, BaseA: filePath, BaseB: filePath,
			BOffset: off, BLen: 1,
		}
	}
	dirPlan := func() *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:  comboKey{Root: "write", Variant: "rmdir", Rel: PathSame},
				RetB:   RetFailure,
				OffB:   offsetBucketNA,
				DepthB: DepthBucketOf(dirPath),
				TypeB:  true,
			},
			NodeX: 1, BaseA: dirPath, BaseB: dirPath,
		}
	}

	// Healthy file plan.
	_, hmcfg, _, lcs := directedPlanFixture(t)
	lcs.FileTree.UpdateNodeSize(filePath, 5000)
	if !validateDirectedPlanLive(hmcfg, lcs, filePlan()) {
		t.Fatal("valid plan rejected")
	}

	// Missing BaseB.
	_, hmcfg2, _, lcs2 := directedPlanFixture(t)
	lcs2.FileTree.UpdateNodeSize(filePath, 5000)
	p2 := filePlan()
	lcs2.FileTree.RemoveNode(filePath)
	if validateDirectedPlanLive(hmcfg2, lcs2, p2) {
		t.Fatal("missing BaseB accepted")
	}

	// Size change moves the offset bucket.
	_, hmcfg3, _, lcs3 := directedPlanFixture(t)
	lcs3.FileTree.UpdateNodeSize(filePath, 5000)
	p3 := filePlan()
	lcs3.FileTree.UpdateNodeSize(filePath, 100)
	if validateDirectedPlanLive(hmcfg3, lcs3, p3) {
		t.Fatal("size-drifted bucket accepted")
	}

	// rmdir target is no longer a non-empty directory.
	_, hmcfg4, _, lcs4 := directedPlanFixture(t)
	p4 := dirPlan()
	if !validateDirectedPlanLive(hmcfg4, lcs4, p4) {
		t.Fatal("valid non-empty rmdir plan rejected")
	}
	lcs4.FileTree.RemoveNode("merge_view/dirA/a")
	lcs4.FileTree.RemoveNode("merge_view/dirA/b")
	if validateDirectedPlanLive(hmcfg4, lcs4, p4) {
		t.Fatal("empty rmdir target accepted")
	}
}

// TestInsertDirectedBatchRecheckLeavesPsUntouched checks the recheck runs
// before any mutation: a batch with one drifted variant fails as a whole,
// every prog keeps its call counts, and no pending is recorded.
func TestInsertDirectedBatchRecheckLeavesPsUntouched(t *testing.T) {
	target, hmcfg, sc, lcs := directedPlanFixture(t)
	var (
		batch        *directedBatch
		ps           []*Prog
		cands, multi int
	)
	for seed := int64(0); seed < 200 && batch == nil; seed++ {
		ps = directedPlanBaseProgs(target)
		r := newRand(target, rand.NewSource(seed))
		cand, _ := PickDirectedBatch(ps, lcs, hmcfg, r)
		if cand == nil {
			continue
		}
		cands++
		if len(cand.Plans) < 2 {
			continue
		}
		multi++
		batch = cand
	}
	if batch == nil {
		t.Fatalf("no multi-variant batch sampled in 200 seeds (batches=%d multi=%d)", cands, multi)
	}
	bad := *batch
	bad.Plans = append([]directedPlan(nil), batch.Plans...)
	if bad.Plans[1].Cell.Combo.Variant == "rename" {
		// A fresh rename destination is legitimately absent; corrupt the
		// source instead so the live recheck still trips.
		bad.Plans[1].BaseB2 = "merge_view/__missing__"
	} else {
		bad.Plans[1].BaseB = "merge_view/__missing__"
	}
	saved := make([]int, len(ps))
	for i := range ps {
		saved[i] = len(ps[i].Calls)
	}
	if insertDirectedBatch(ps, newRand(target, rand.NewSource(99)), sc, hmcfg, lcs, &bad) {
		t.Fatal("insert succeeded despite a drifted variant")
	}
	for i := range ps {
		if len(ps[i].Calls) != saved[i] {
			t.Fatalf("ps[%d] mutated on recheck failure", i)
		}
	}
	if len(lcs.PendingDirected) != 0 {
		t.Fatal("pending recorded on recheck failure")
	}
}

// hasDirectedPlanBases reports whether the plan set contains any plan whose
// relation and base paths match (root/variant are intentionally unconstrained).
func hasDirectedPlanBases(plans map[cellKey]*directedPlan, rel PathRelation, baseA, baseB string) bool {
	for _, p := range plans {
		if p.Cell.Combo.Rel == rel && p.BaseA == baseA && p.BaseB == baseB {
			return true
		}
	}
	return false
}

// hasDirectedPlanRelForB reports whether any plan targets base B under rel.
func hasDirectedPlanRelForB(plans map[cellKey]*directedPlan, rel PathRelation, baseB string) bool {
	for _, p := range plans {
		if p.Cell.Combo.Rel == rel && p.BaseB == baseB {
			return true
		}
	}
	return false
}

// hasDirectedPlanBaseBIn reports whether some plan targets baseB under rel
// with a BaseA drawn from allowed (witness sampling is random, so tests assert
// membership in the candidate set rather than one exact path).
func hasDirectedPlanBaseBIn(plans map[cellKey]*directedPlan, rel PathRelation, baseB string, allowed ...string) bool {
	for _, p := range plans {
		if p.Cell.Combo.Rel != rel || p.BaseB != baseB {
			continue
		}
		for _, a := range allowed {
			if p.BaseA == a {
				return true
			}
		}
	}
	return false
}

// TestDirectedPlanWitnessRelations pins the B-driven witness derivation on the
// smoke tree: every relation is derived from B in O(1), the Sibling candidates
// exclude B itself, and impossible witnesses (Parent of a leaf, only-child
// Sibling) produce no plan. Witness sampling is random, so multi-candidate
// relations assert membership in the candidate set.
func TestDirectedPlanWitnessRelations(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans with the smoke tree")
	}
	const (
		fileA = "merge_view/dirA/a"
		fileB = "merge_view/dirA/b"
		dirA  = "merge_view/dirA"
		dirB  = "merge_view/dirB"
		fileC = "merge_view/dirB/c"
	)
	cases := []struct {
		rel          PathRelation
		baseA, baseB string
		want         bool
	}{
		{PathSame, fileA, fileA, true},    // A = B
		{PathChild, dirA, fileA, true},    // A = parent(B)
		{PathSibling, fileB, fileA, true}, // A = the only sibling of B
		{PathSibling, dirB, dirA, true},   // A = the only dir sibling of B
	}
	for _, c := range cases {
		if got := hasDirectedPlanBases(plans, c.rel, c.baseA, c.baseB); got != c.want {
			t.Fatalf("witness(%v, %s -> %s) = %v, want %v", c.rel, c.baseA, c.baseB, got, c.want)
		}
	}
	// Parent candidates of dirA are {a, b}; the sampled representative must be
	// one of them.
	if !hasDirectedPlanBaseBIn(plans, PathParent, dirA, fileA, fileB) {
		t.Fatalf("Parent witness for %s not drawn from {a, b}", dirA)
	}
	// Negative witnesses: a leaf has no children (no Parent target) and an
	// only child has no sibling (no Sibling target).
	if hasDirectedPlanRelForB(plans, PathParent, fileA) {
		t.Fatalf("leaf %s must have no Parent witness", fileA)
	}
	if hasDirectedPlanRelForB(plans, PathSibling, fileC) {
		t.Fatalf("only child %s must have no Sibling witness", fileC)
	}
}

// TestDirectedPlanWitnessDiversity pins that the witness is sampled, not fixed
// to the min path: the same (B, relation, root) queried under different RNG
// seeds yields different concrete A paths, while the candidate set stays
// deterministic (TestDirectedPlanDeterministic).
func TestDirectedPlanWitnessDiversity(t *testing.T) {
	target, hmcfg, _, _ := directedPlanFixture(t)
	view := buildDirectedView(hmcfg.FileTree, hmcfg.Persistence_dir)
	var dirA *directedBNode
	for i := range view.bNodes {
		if view.bNodes[i].path == "merge_view/dirA" {
			dirA = &view.bNodes[i]
			break
		}
	}
	if dirA == nil {
		t.Fatal("dirA not present in the planning view")
	}
	seen := map[string]bool{}
	for seed := int64(0); seed < 20; seed++ {
		r := newRand(target, rand.NewSource(seed))
		p, ok := dirA.witnessFor(PathParent, "open", r.Rand)
		if !ok {
			t.Fatal("dirA must have a Parent witness")
		}
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Fatalf("witness sampling never varied across seeds: %v", seen)
	}
}

// TestDirectedPlanCoversFullTree pins the removed 48-node window: with 60 files
// whose names sort before the target, the target must still be planned (the old
// per-cid lexicographic cap would have excluded it). The target sits under the
// persistence dir so its cell marginals are unique: cells with identical
// marginals dedup to the first B in path order, so a shared-marginal target
// would not appear as a representative plan.
func TestDirectedPlanCoversFullTree(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()[:2]
	files := make([]string, 0, 61)
	for i := 0; i < 60; i++ {
		files = append(files, fmt.Sprintf("merge_view/dirA/f%02d", i))
	}
	files = append(files, "merge_view/pdir/zz")
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{
			cids[0]: {"merge_view/dirA"},
			cids[1]: {"merge_view/dirA", "merge_view/pdir"},
		},
		Init_file: map[string][]string{cids[1]: files},
	})
	if n := len(ft.NodesByCid[cids[1]]); n <= 48 {
		t.Fatalf("fixture too small: %d nodes in cid1", n)
	}
	hmcfg := &Hmdfs_config{Cids: cids, Node_num: 2, FileTree: ft,
		Persistence_dir: "merge_view/pdir"}
	// The board is intentionally initialized without the persist dir — this
	// test asserts planDirectedCells output directly (no uncoveredCellFlags /
	// PickDirectedBatch), so the persist cell margin needs no board domain.
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{})
	t.Cleanup(ResetCoverageBoard)
	ps := make([]*Prog, 2)
	for i := range ps {
		ps[i] = &Prog{Target: target}
	}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	for _, p := range plans {
		if p.BaseB == "merge_view/pdir/zz" {
			return
		}
	}
	t.Fatal("target beyond the old 48-node window is not planned")
}

// TestDirectedPlanDeterministic pins reproducibility: the candidate cell order
// and every representative plan must be identical for the same RNG seed (the
// view follows slice order, witnesses are sampled with the plan RNG, no map
// iteration leaks into the result).
func TestDirectedPlanDeterministic(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	collect := func(seed int64) ([]cellKey, map[cellKey]directedPlan) {
		plans, cells := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		out := make(map[cellKey]directedPlan, len(plans))
		for k, v := range plans {
			out[k] = *v
		}
		return cells, out
	}
	cells1, plans1 := collect(7)
	cells2, plans2 := collect(7)
	if len(cells1) != len(cells2) {
		t.Fatalf("candidate cell count differs: %d vs %d", len(cells1), len(cells2))
	}
	for i := range cells1 {
		if cells1[i] != cells2[i] {
			t.Fatalf("candidate cell order differs at %d: %v vs %v", i, cells1[i], cells2[i])
		}
		if p1, p2 := plans1[cells1[i]], plans2[cells2[i]]; p1 != p2 {
			t.Fatalf("plan differs for %v: %+v vs %+v", cells1[i], p1, p2)
		}
	}
}

// TestDirectedPlanDirectWitnesses pins the generation-side direct-adjacency
// rule over the global merged view: Child uses the direct A-eligible parent and
// Parent the direct A-eligible children (mirroring GetPathByRelation's 1-hop
// semantics). A TmpDir intermediate works as B (its direct parent dirA is a
// valid A) but is never an A itself, so chains through it produce no plan.
func TestDirectedPlanDirectWitnesses(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()[:2]
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{
			cids[0]: {"merge_view/dirA"},
			cids[1]: {"merge_view/dirA"},
		},
		Init_file: map[string][]string{
			cids[0]: {"merge_view/dirA/sub/deep"},
			cids[1]: {"merge_view/dirA/sub/deep", "merge_view/dirA/sub/other", "merge_view/topB"},
		},
	})
	// The intermediate "merge_view/dirA/sub" is an auto-created TmpDir and is
	// not registered under any cid; the global tree walk still sees it.
	for _, n := range ft.NodesByCid[cids[0]] {
		if n.FullPath == "merge_view/dirA/sub" {
			t.Fatal("fixture invalid: intermediate dir registered in cid0")
		}
	}
	hmcfg := &Hmdfs_config{Cids: cids, Node_num: 2, FileTree: ft}
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{})
	t.Cleanup(ResetCoverageBoard)
	ps := make([]*Prog, 2)
	for i := range ps {
		ps[i] = &Prog{Target: target}
	}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	const (
		dirA  = "merge_view/dirA"
		sub   = "merge_view/dirA/sub"
		deep  = "merge_view/dirA/sub/deep"
		other = "merge_view/dirA/sub/other"
	)
	// Positive: sub's direct parent dirA is A-eligible, so the TmpDir B keeps a
	// Child plan; deep/other are direct siblings.
	if !hasDirectedPlanBases(plans, PathChild, dirA, sub) {
		t.Fatal("direct Child witness for the TmpDir B missing")
	}
	if !hasDirectedPlanBases(plans, PathSibling, other, deep) {
		t.Fatal("direct Sibling witness missing")
	}
	// Negative: deep's direct parent is the TmpDir sub (not A-eligible), so the
	// direct-adjacency rule yields no Child plan; dirA's only direct child is
	// the TmpDir sub, so no Parent plan either.
	if hasDirectedPlanRelForB(plans, PathChild, deep) {
		t.Fatal("deep B must have no Child plan (direct parent is a TmpDir)")
	}
	if hasDirectedPlanRelForB(plans, PathParent, dirA) {
		t.Fatal("dirA must have no Parent plan (its only direct child is a TmpDir)")
	}
	// A B whose only parent is the merge_view root (excluded) has no Child
	// witness at all.
	if hasDirectedPlanRelForB(plans, PathChild, "merge_view/topB") {
		t.Fatal("top-level B without an A-eligible direct parent must have no Child witness")
	}
	if hasDirectedPlanRelForB(plans, PathSame, sub) {
		t.Fatal("TmpDir B must have no Same witness (not A-eligible)")
	}
}

// TestDirectedPlanGlobalPool pins that the planner pools are the global merged
// view, not the creation-ownership cid lists: node0 may target a path owned by
// another node, and a variant node may target a path owned by node0.
func TestDirectedPlanGlobalPool(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()[:2]
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{
			cids[0]: {"merge_view/dirA"},
			cids[1]: {"merge_view/dirB"},
		},
		Init_file: map[string][]string{
			cids[0]: {"merge_view/dirA/zero"},
			cids[1]: {"merge_view/dirB/one"},
		},
	})
	// The persistence margin makes B=one's cells unique: with shared marginals
	// the earlier path (dirA/zero) would be the representative and the remote
	// A/B combination would not appear as a plan.
	hmcfg := &Hmdfs_config{Cids: cids, Node_num: 2, FileTree: ft,
		Persistence_dir: "merge_view/dirB"}
	// The board is intentionally initialized without the persist dir — this
	// test asserts planDirectedCells output directly (no uncoveredCellFlags /
	// PickDirectedBatch), so the persist cell margin needs no board domain.
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{})
	t.Cleanup(ResetCoverageBoard)
	ps := make([]*Prog, 2)
	for i := range ps {
		ps[i] = &Prog{Target: target}
	}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	const (
		dirA = "merge_view/dirA"
		zero = "merge_view/dirA/zero"
		dirB = "merge_view/dirB"
		one  = "merge_view/dirB/one"
	)
	// A (root base on node0) may be a cid1-owned path (remote operation).
	if !hasDirectedPlanBases(plans, PathChild, dirB, one) {
		t.Fatal("A from another node's ownership list missing")
	}
	// B (variant base on nodeX) may be a cid0-owned path (remote operation).
	if !hasDirectedPlanBases(plans, PathChild, dirA, zero) {
		t.Fatal("B from node0's ownership list missing")
	}
	// Raw enumerator output leaves node assignment to batch assembly.
	for _, p := range plans {
		if p.NodeX != 0 {
			t.Fatalf("raw plan NodeX = %d, want 0 (assigned by batch assembly)", p.NodeX)
		}
	}
}

// TestDirectedPlanSiblingExcludesB pins the Sibling candidate pool on a wide
// directory: B is never its own A, the pool is exactly the other A-eligible
// siblings, and sampling varies across seeds.
func TestDirectedPlanSiblingExcludesB(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()[:2]
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{
			cids[0]: {"merge_view/dirW"},
			cids[1]: {"merge_view/dirW"},
		},
		Init_file: map[string][]string{
			cids[0]: {"merge_view/dirW/x"},
			cids[1]: {"merge_view/dirW/y", "merge_view/dirW/z"},
		},
	})
	view := buildDirectedView(ft, "")
	var nodeY *directedBNode
	for i := range view.bNodes {
		if view.bNodes[i].path == "merge_view/dirW/y" {
			nodeY = &view.bNodes[i]
			break
		}
	}
	if nodeY == nil {
		t.Fatal("merge_view/dirW/y not present in the planning view")
	}
	seen := map[string]bool{}
	for seed := int64(0); seed < 20; seed++ {
		r := newRand(target, rand.NewSource(seed))
		p, ok := nodeY.witnessFor(PathSibling, "open", r.Rand)
		if !ok {
			t.Fatal("dirW/y must have a Sibling witness")
		}
		if p == "merge_view/dirW/y" {
			t.Fatal("Sibling witness must exclude B itself")
		}
		if p != "merge_view/dirW/x" && p != "merge_view/dirW/z" {
			t.Fatalf("unexpected Sibling witness %q", p)
		}
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Fatalf("Sibling witness sampling never varied: %v", seen)
	}
}

// TestPickDirectedBatchSameRoot pins the batch grouping and fill on a minimal
// tree where every seed's root has at least as many pickable cells as the node
// budget: the batch fills len(ps)-1, all plans share the root call and base
// path, cells are distinct, and NodeX is 1..M.
func TestPickDirectedBatchSameRoot(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{
			cids[0]: {"merge_view/dirW"},
			cids[1]: {"merge_view/dirW"},
			cids[2]: {"merge_view/dirW"},
		},
		Init_file: map[string][]string{cids[1]: {"merge_view/dirW/x"}},
	})
	hmcfg := &Hmdfs_config{Cids: cids, Node_num: 3, FileTree: ft}
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{})
	t.Cleanup(ResetCoverageBoard)
	ps := make([]*Prog, 3)
	for i := range ps {
		ps[i] = &Prog{Target: target}
	}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	for seed := int64(0); seed < 20; seed++ {
		r := newRand(target, rand.NewSource(seed))
		batch, status := PickDirectedBatch(ps, lcs, hmcfg, r)
		if batch == nil {
			t.Fatalf("seed %d: no batch (status %v)", seed, status)
		}
		// The batch is capped by the node budget, and the one-state-authority
		// filter may shrink it further when several plans share a BaseB with a
		// writer (the seed is always kept).
		if len(batch.Plans) == 0 || len(batch.Plans) > len(ps)-1 {
			t.Fatalf("seed %d: batch size %d out of range 1..%d", seed, len(batch.Plans), len(ps)-1)
		}
		seen := make(map[cellKey]bool, len(batch.Plans))
		for i := range batch.Plans {
			p := &batch.Plans[i]
			if p.NodeX != i+1 {
				t.Fatalf("seed %d: NodeX = %d, want %d", seed, p.NodeX, i+1)
			}
			if p.Cell.Combo.Root != batch.Root || p.BaseA != batch.BaseA {
				t.Fatalf("seed %d: plan %d does not share the batch root (%s/%s)", seed, i, batch.Root, batch.BaseA)
			}
			if ck := p.Cell.toKey(); seen[ck] {
				t.Fatalf("seed %d: duplicate cell in batch", seed)
			} else {
				seen[ck] = true
			}
		}
	}
}

// TestPickDirectedBatchBaseBStateAuthority pins the intra-batch state
// authority: on every BaseB the batch keeps either all observers or exactly
// one plan when a writer is involved (FreshSame entries are the intended
// same-fresh-path race and are exempt), and the seed always survives.
func TestPickDirectedBatchBaseBStateAuthority(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	sawWriterBatch := false
	batches, writerBatches := 0, 0
	for seed := int64(0); seed < 300; seed++ {
		b, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if b == nil || len(b.Plans) == 0 {
			continue
		}
		batches++
		byB := make(map[string][]*directedPlan)
		for i := range b.Plans {
			p := &b.Plans[i]
			if p.NodeX != i+1 {
				t.Fatalf("seed %d: NodeX = %d, want %d", seed, p.NodeX, i+1)
			}
			byB[p.BaseB] = append(byB[p.BaseB], p)
		}
		for base, grp := range byB {
			writers := 0
			for _, p := range grp {
				if p.FreshSame {
					continue
				}
				if planStateEffect(p) != planEffectObserver {
					writers++
				}
			}
			if writers > 1 || (writers == 1 && len(grp) > 1) {
				t.Fatalf("seed %d: BaseB %q kept %d plans with %d writer(s)", seed, base, len(grp), writers)
			}
			if writers == 1 {
				sawWriterBatch = true
				writerBatches++
			}
		}
	}
	if !sawWriterBatch {
		t.Fatalf("no writer batch sampled in 300 seeds (batches=%d writer=%d)", batches, writerBatches)
	}
}

// TestPickDirectedBatchEUniqueDst pins the E destination reservations: an
// overwrite destination never lands on another plan's anchor/source/target,
// and two E plans never share a destination.
func TestPickDirectedBatchEUniqueDst(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	sawReplace := false
	batches, eBatches := 0, 0
	for seed := int64(0); seed < 300; seed++ {
		b, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if b == nil {
			continue
		}
		batches++
		anchors := make(map[string]bool, 2*len(b.Plans))
		for i := range b.Plans {
			anchors[b.Plans[i].BaseA] = true
		}
		seenDst := make(map[string]bool)
		for i := range b.Plans {
			p := &b.Plans[i]
			if !p.RenameReplace {
				continue
			}
			sawReplace = true
			eBatches++
			if seenDst[p.BaseB] {
				t.Fatalf("seed %d: duplicate E destination %q", seed, p.BaseB)
			}
			seenDst[p.BaseB] = true
			if anchors[p.BaseB] {
				t.Fatalf("seed %d: E destination hit an anchor: %+v", seed, p)
			}
			for j := range b.Plans {
				q := &b.Plans[j]
				if q == p {
					continue
				}
				if q.BaseB2 != "" && q.BaseB2 == p.BaseB {
					t.Fatalf("seed %d: E destination hit a source: %+v", seed, p)
				}
				if q.BaseB != "" && q.BaseB == p.BaseB {
					t.Fatalf("seed %d: E destination hit a target: %+v", seed, p)
				}
			}
		}
	}
	if !sawReplace {
		t.Fatalf("no E conversion sampled in 300 seeds (batches=%d eBatches=%d)", batches, eBatches)
	}
}

// TestDirectedFreshRenameNoSuffixAccumulation pins the suffix convention:
// materializing a fresh rename destination strips one previous "._renamed_*"
// level before appending, so repeated planning cannot grow the name by one
// suffix per round.
func TestDirectedFreshRenameNoSuffixAccumulation(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, src := range []string{
		"merge_view/dirW/a",
		"merge_view/dirW/a._renamed_aaaaaa",
		"merge_view/dirW/a._renamed_aaaaaa._renamed_bbbbbb",
	} {
		got := freshRenameDst(src, r)
		want := strings.Count(src, "._renamed_")
		if want < 1 {
			want = 1
		}
		if n := strings.Count(got, "._renamed_"); n > want {
			t.Fatalf("suffix grew: %q -> %q (%d > %d)", src, got, n, want)
		}
		if !strings.Contains(got, "._renamed_") {
			t.Fatalf("no rename suffix appended: %q -> %q", src, got)
		}
	}
}

// TestPickDirectedBatchDeterministic pins reproducibility: the same seed and
// state produce an identical batch (root, base path, order, NodeX, cells).
func TestPickDirectedBatchDeterministic(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	pick := func(seed int64) *directedBatch {
		b, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		return b
	}
	b1, b2 := pick(7), pick(7)
	if b1 == nil || b2 == nil {
		t.Fatal("no batch with the smoke tree")
	}
	if b1.Root != b2.Root || b1.BaseA != b2.BaseA || len(b1.Plans) != len(b2.Plans) {
		t.Fatalf("batch header differs: %+v vs %+v", b1, b2)
	}
	for i := range b1.Plans {
		if b1.Plans[i] != b2.Plans[i] {
			t.Fatalf("plan %d differs: %+v vs %+v", i, b1.Plans[i], b2.Plans[i])
		}
	}
}

// TestInsertDirectedBatchAbort checks that a variant which cannot be timing-
// anchored aborts the whole batch: every prog keeps its call counts, no
// pending is recorded, and nothing is charged as a planned cell.
func TestInsertDirectedBatchAbort(t *testing.T) {
	target, hmcfg, sc, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	// A variant node without calls has no timing anchor at all, so the batch
	// build aborts after the root and the first variant were generated.
	ps[2] = &Prog{Target: target}
	mk := func(node int, temporal TemporalRel) directedPlan {
		return directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: "open", Variant: "read", Rel: PathChild},
				Temporal: temporal, RetB: RetSuccess, OffB: offsetBucketZero,
				DepthB: DepthBucketOf("merge_view/dirA/a"),
			},
			NodeX: node, BaseA: "merge_view/dirA", BaseB: "merge_view/dirA/a",
		}
	}
	batch := &directedBatch{Root: "open", BaseA: "merge_view/dirA",
		Plans: []directedPlan{mk(1, TemporalConcurrent), mk(2, TemporalHB)}}
	saved := make([]int, len(ps))
	for i := range ps {
		saved[i] = len(ps[i].Calls)
	}
	// ps[2] has no call timing, so the second variant cannot be anchored to
	// the root reference: the build aborts before the commit.
	if insertDirectedBatch(ps, newRand(target, rand.NewSource(3)), sc, hmcfg, lcs, batch) {
		t.Fatal("unanchorable variants must abort the batch")
	}
	for i := range ps {
		if len(ps[i].Calls) != saved[i] {
			t.Fatalf("ps[%d] mutated on aborted batch", i)
		}
	}
	if len(lcs.PendingDirected) != 0 {
		t.Fatal("pending recorded on aborted batch")
	}
	if plans, _, _, _, _ := DirectedCellStats(); plans != 0 {
		t.Fatalf("aborted batch counted %d planned cells", plans)
	}
}

// renameFixture builds a small merged view for the dual-path rename tests:
// one parent directory holding a file source, a sibling file, a non-empty
// destination directory, a directory source, and an unregistered intermediate
// (TmpDir) with a child.
func renameFixture(t *testing.T) (*Target, *Hmdfs_config, *SpecialCalls, *LayeredChoiceStrategy) {
	t.Helper()
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{
			cids[0]: {"merge_view/dirW", "merge_view/dirW/dstDir", "merge_view/dirW/srcDir"},
		},
		Init_file: map[string][]string{
			cids[0]: {
				"merge_view/dirW/src", "merge_view/dirW/sib",
				"merge_view/dirW/dstDir/inside", "merge_view/dirW/sub/kid",
			},
		},
	})
	hmcfg := &Hmdfs_config{Cids: cids, Node_num: 3, FileTree: ft}
	sc := hmdfsSmokeSpecialCalls(t, target)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{})
	t.Cleanup(ResetCoverageBoard)
	return target, hmcfg, sc, lcs
}

// TestDirectedPlanRenameFreshWitnesses pins the fresh-destination rename form:
// a synthesized B node per source (TmpDir sources included) with plan.BaseB
// deferred to batch assembly, Same anchored on the source (prePath) and
// suppressed for TmpDir sources, and Sibling/Child witnesses excluding the
// source.
func TestDirectedPlanRenameFreshWitnesses(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	const sub = "merge_view/dirW/sub"
	var (
		checked                      int
		seenSame, seenSib, seenChild bool
		seenTmpSrc                   bool
	)
	for _, p := range plans {
		if p.Cell.Combo.Variant != "rename" || p.Cell.RetB != RetSuccess {
			continue
		}
		if p.RenameReplace {
			continue // E is covered by TestDirectedPlanRenameReplace
		}
		checked++
		if p.BaseB2 == sub {
			seenTmpSrc = true
		}
		if p.BaseB != "" {
			t.Fatalf("fresh rename plan must defer BaseB to batch assembly: %+v", p)
		}
		if p.BaseB2 == "" {
			t.Fatalf("bad rename source %q", p.BaseB2)
		}
		srcNode := hmcfg.FileTree.FindNode(p.BaseB2)
		if srcNode == nil || p.Cell.TypeB != (srcNode.Type != NodeTypeFile) {
			t.Fatalf("fresh rename TypeB does not match the source: %+v", p)
		}
		if p.BaseA == p.BaseB2 && p.Cell.Combo.Rel != PathSame {
			t.Fatalf("non-Same relation must exclude the source: %+v", p)
		}
		switch p.Cell.Combo.Rel {
		case PathSame:
			if p.BaseB2 == sub {
				t.Fatalf("TmpDir rename source must not carry a Same plan (it would be the root base)")
			}
			if p.BaseA != p.BaseB2 {
				t.Fatalf("Same must anchor A on the source: A=%q src=%q", p.BaseA, p.BaseB2)
			}
			seenSame = true
		case PathSibling:
			seenSib = true
		case PathChild:
			if GetParentDir(p.BaseB2) != p.BaseA {
				t.Fatalf("Child witness %q is not the direct parent of %q", p.BaseA, p.BaseB2)
			}
			seenChild = true
		}
	}
	if checked == 0 {
		t.Fatal("no fresh rename plans")
	}
	if !seenSame {
		t.Fatal("missing Same witness with A=source")
	}
	if !seenSib {
		t.Fatal("missing Sibling witness for a source with siblings")
	}
	if !seenChild {
		t.Fatal("missing Child witness through the parent")
	}
	if !seenTmpSrc {
		t.Fatal("no plan with a TmpDir rename source (the variant side must accept TmpDir)")
	}
}

// TestDirectedPlanRenameENOTEMPTY pins the dual-path failure form: the only
// non-empty plain dir destination pairs with the first non-file sibling
// directory (a TmpDir source is allowed: it is a variant-side path), and all
// four relations are constructible (Same via the source prePath, Sibling/Child
// through the parent, Parent through dst's children).
func TestDirectedPlanRenameENOTEMPTY(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	const (
		dirW   = "merge_view/dirW"
		src    = "merge_view/dirW/src"
		sib    = "merge_view/dirW/sib"
		dstDir = "merge_view/dirW/dstDir"
		srcDir = "merge_view/dirW/srcDir"
		inside = "merge_view/dirW/dstDir/inside"
	)
	var (
		checked                                  int
		seenSame, seenSib, seenChild, seenParent bool
	)
	for _, p := range plans {
		if p.Cell.Combo.Variant != "rename" || p.Cell.RetB != RetFailure {
			continue
		}
		checked++
		if p.BaseB != dstDir || p.BaseB2 != srcDir || !p.Cell.TypeB {
			t.Fatalf("unexpected enotempty plan: %+v", p)
		}
		if p.BaseA == p.BaseB || p.BaseA == p.BaseB2 {
			t.Fatalf("enotempty A must differ from src/dst: %+v", p)
		}
		switch p.Cell.Combo.Rel {
		case PathSame:
			if p.BaseA != srcDir {
				t.Fatalf("enotempty Same must anchor A on the source")
			}
			seenSame = true
		case PathSibling:
			if p.BaseA != sib && p.BaseA != src {
				t.Fatalf("enotempty Sibling A = %q, want an eligible file sibling (%q or %q)", p.BaseA, sib, src)
			}
			seenSib = true
		case PathChild:
			if p.BaseA != dirW {
				t.Fatalf("enotempty Child A = %q, want the parent %q", p.BaseA, dirW)
			}
			seenChild = true
		case PathParent:
			if p.BaseA != inside {
				t.Fatalf("enotempty Parent A = %q, want the destination child %q", p.BaseA, inside)
			}
			seenParent = true
		}
	}
	if checked == 0 {
		t.Fatal("no enotempty rename plans")
	}
	if !seenSame || !seenSib || !seenChild || !seenParent {
		t.Fatalf("missing relation coverage: same=%v sibling=%v child=%v parent=%v",
			seenSame, seenSib, seenChild, seenParent)
	}
}

// TestValidateRenamePlanLive pins the dual-path recheck: same-parent sources,
// fresh destinations, non-empty directory destinations, the source/destination
// A guard and the prePath-based Same relation.
func TestValidateRenamePlanLive(t *testing.T) {
	_, hmcfg, _, lcs := renameFixture(t)
	const (
		dirW   = "merge_view/dirW"
		src    = "merge_view/dirW/src"
		sib    = "merge_view/dirW/sib"
		dstDir = "merge_view/dirW/dstDir"
		srcDir = "merge_view/dirW/srcDir"
		inside = "merge_view/dirW/dstDir/inside"
		sub    = "merge_view/dirW/sub"
	)
	fresh := dirW + "/src._renamed_test"
	plan := func(rel PathRelation, ret RetBucket, a, b, b2 string, typeB bool) directedPlan {
		return directedPlan{
			Cell: directedCell{
				Combo: comboKey{Root: "open", Variant: "rename", Rel: rel},
				RetB:  ret, OffB: offsetBucketNA, TypeB: typeB,
				DepthB: DepthBucketOf(b),
			},
			BaseA: a, BaseB: b, BaseB2: b2,
		}
	}
	valid := []directedPlan{
		plan(PathSame, RetSuccess, src, fresh, src, false),
		plan(PathChild, RetSuccess, dirW, fresh, src, false),
		plan(PathParent, RetFailure, inside, dstDir, srcDir, true),
		plan(PathSibling, RetFailure, sib, dstDir, srcDir, true),
	}
	for i, p := range valid {
		if !validateDirectedPlanLive(hmcfg, lcs, &p) {
			t.Fatalf("valid rename plan %d rejected: %+v", i, p)
		}
	}
	invalid := []directedPlan{
		plan(PathSame, RetSuccess, src, fresh, "merge_view/missing", false),
		plan(PathChild, RetSuccess, dirW, dstDir+"/x_fresh", src, false),      // cross-parent
		plan(PathChild, RetSuccess, dirW, dstDir, src, false),                 // fresh dst exists
		plan(PathParent, RetFailure, inside, src, srcDir, false),              // dst is a file
		plan(PathParent, RetFailure, inside, srcDir, srcDir, true),            // dst dir empty
		plan(PathSibling, RetFailure, dstDir, dstDir, srcDir, true),           // A == dst
		plan(PathSibling, RetFailure, srcDir, dstDir, srcDir, true),           // A == src
		plan(PathSame, RetSuccess, sib, fresh, src, false),                    // Same A != src
		plan(PathChild, RetENOENT, dirW, fresh, src, false),                   // bad ret
		plan(PathSame, RetSuccess, sub, dirW+"/sub._renamed_test", sub, true), // Same would root on a TmpDir
	}
	for i, p := range invalid {
		if validateDirectedPlanLive(hmcfg, lcs, &p) {
			t.Fatalf("invalid rename plan %d accepted: %+v", i, p)
		}
	}
	// A TmpDir is a valid rename-variant source (the variant side is
	// relation-derived); only the rename root excludes TmpDir.
	tmp := plan(PathChild, RetSuccess, dirW, dirW+"/sub._renamed_test", sub, true)
	if !validateDirectedPlanLive(hmcfg, lcs, &tmp) {
		t.Fatal("TmpDir rename variant source rejected")
	}
	bad := plan(PathSame, RetSuccess, src, src, src, false)
	bad.Cell.Combo.Variant = "read"
	if validateDirectedPlanLive(hmcfg, lcs, &bad) {
		t.Fatal("non-rename plan with BaseB2 accepted")
	}
	// E overwrite forms: existing same-class same-parent destination.
	replaceSame := plan(PathSame, RetSuccess, src, sib, src, false)
	replaceSame.RenameReplace = true
	replaceChild := plan(PathChild, RetSuccess, dirW, sib, src, false)
	replaceChild.RenameReplace = true
	replaceDir := plan(PathSame, RetSuccess, dstDir, srcDir, dstDir, true)
	replaceDir.RenameReplace = true
	for i, p := range []directedPlan{replaceSame, replaceChild, replaceDir} {
		if !validateDirectedPlanLive(hmcfg, lcs, &p) {
			t.Fatalf("valid E plan %d rejected: %+v", i, p)
		}
	}
	replaceMissing := plan(PathSame, RetSuccess, src, fresh, src, false)
	replaceMissing.RenameReplace = true
	replaceClass := plan(PathChild, RetSuccess, dirW, srcDir, src, false)
	replaceClass.RenameReplace = true
	replaceNonEmpty := plan(PathChild, RetSuccess, dirW, dstDir, srcDir, true)
	replaceNonEmpty.RenameReplace = true
	replaceFailure := plan(PathParent, RetFailure, inside, dstDir, srcDir, true)
	replaceFailure.RenameReplace = true
	for i, p := range []directedPlan{replaceMissing, replaceClass, replaceNonEmpty, replaceFailure} {
		if validateDirectedPlanLive(hmcfg, lcs, &p) {
			t.Fatalf("invalid E plan %d accepted: %+v", i, p)
		}
	}
}

// TestPickDirectedBatchMaterializesRenameDst pins the assembly-time
// materialization of a fresh rename destination: unique sibling name with the
// documented prefix, absent from the tree, and reproducible for a fixed seed.
func TestPickDirectedBatchMaterializesRenameDst(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	pick := func(seed int64) *directedBatch {
		b, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		return b
	}
	batches, freshRename := 0, 0
	for seed := int64(0); seed < 200; seed++ {
		b1, b2 := pick(seed), pick(seed)
		if b1 == nil {
			continue
		}
		batches++
		if b2 == nil || len(b1.Plans) != len(b2.Plans) {
			t.Fatalf("seed %d: batch not reproducible", seed)
		}
		for i := range b1.Plans {
			if b1.Plans[i] != b2.Plans[i] {
				t.Fatalf("seed %d: plan %d differs: %+v vs %+v", seed, i, b1.Plans[i], b2.Plans[i])
			}
		}
		for i := range b1.Plans {
			p := &b1.Plans[i]
			if p.Cell.Combo.Variant != "rename" || p.Cell.RetB != RetSuccess {
				continue
			}
			if p.RenameReplace {
				continue // E destinations are existing paths, not materialized
			}
			if !strings.HasPrefix(p.BaseB, p.BaseB2+"._renamed_") {
				t.Fatalf("fresh rename dst %q lacks the source prefix %q", p.BaseB, p.BaseB2)
			}
			if hmcfg.FileTree.FindNode(p.BaseB) != nil {
				t.Fatalf("fresh rename dst %q must not exist", p.BaseB)
			}
			freshRename++
			return
		}
	}
	t.Fatalf("no fresh rename batch sampled in 200 seeds (batches=%d freshRename=%d)", batches, freshRename)
}

// TestDirectedPlanRenameReplace pins the E overwrite shape sampled at batch
// assembly: a fresh rename Success plan may instead carry an existing
// same-class same-parent destination (plan.RenameReplace), both shapes appear
// across seeds, and a fixed seed reproduces the batch.
func TestDirectedPlanRenameReplace(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	sawFresh, sawReplace := false, false
	for seed := int64(0); seed < 200; seed++ {
		b1, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if b1 == nil {
			continue
		}
		b2, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if b2 == nil || len(b1.Plans) != len(b2.Plans) {
			t.Fatalf("seed %d: batch not reproducible", seed)
		}
		for i := range b1.Plans {
			if b1.Plans[i] != b2.Plans[i] {
				t.Fatalf("seed %d: plan %d differs: %+v vs %+v", seed, i, b1.Plans[i], b2.Plans[i])
			}
			p := &b1.Plans[i]
			if p.Cell.Combo.Variant != "rename" || p.Cell.RetB != RetSuccess {
				continue
			}
			if !p.RenameReplace {
				sawFresh = true
				continue
			}
			sawReplace = true
			src := hmcfg.FileTree.FindNode(p.BaseB2)
			dst := hmcfg.FileTree.FindNode(p.BaseB)
			if src == nil || dst == nil || dst == src {
				t.Fatalf("E plan with invalid src/dst: %+v", p)
			}
			if p.BaseB == p.BaseA || p.BaseB == p.BaseB2 {
				t.Fatalf("E destination collides with a plan anchor: %+v", p)
			}
			if (src.Type == NodeTypeFile) != (dst.Type == NodeTypeFile) {
				t.Fatalf("E destination class must match the source: %+v", p)
			}
			if GetParentDir(p.BaseB2) != GetParentDir(p.BaseB) {
				t.Fatalf("E destination must be a same-parent sibling: %+v", p)
			}
			if src.Type != NodeTypeFile && len(dst.Children) > 0 {
				t.Fatalf("E dir destination must be empty: %+v", p)
			}
			if !validateDirectedPlanLive(hmcfg, lcs, p) {
				t.Fatalf("E plan fails the live recheck: %+v", p)
			}
		}
	}
	if !sawReplace {
		t.Fatal("no E overwrite plan sampled across seeds")
	}
	if !sawFresh {
		t.Fatal("no fresh rename plan sampled across seeds")
	}
}

// TestRenameReplaceConversionPersist pins the persist-margin guard of the E
// conversion: when persistDir is a file, an existing sibling carrying the
// persist flag must never replace the fresh destination (its observed cell
// would have PersistB=true while the planned cell pins false).
func TestRenameReplaceConversionPersist(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{cids[0]: {"merge_view/dirW"}},
		Init_file: map[string][]string{
			cids[0]: {"merge_view/dirW/src", "merge_view/dirW/mid", "merge_view/dirW/sib"},
		},
	})
	hmcfg := &Hmdfs_config{Cids: cids, Node_num: 3, FileTree: ft,
		Persistence_dir: "merge_view/dirW/mid"}
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{})
	t.Cleanup(ResetCoverageBoard)
	ps := directedPlanBaseProgs(target)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	sawReplace := false
	batches, eBatches := 0, 0
	for seed := int64(0); seed < 200; seed++ {
		b, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if b == nil {
			continue
		}
		batches++
		for i := range b.Plans {
			p := &b.Plans[i]
			if !p.RenameReplace {
				continue
			}
			sawReplace = true
			eBatches++
			if p.BaseB == "merge_view/dirW/mid" {
				t.Fatalf("E destination flipped the persist margin: %+v", p)
			}
			if !validateDirectedPlanLive(hmcfg, lcs, p) {
				t.Fatalf("E plan fails the live recheck: %+v", p)
			}
		}
	}
	if !sawReplace {
		t.Fatalf("no E conversion sampled in 200 seeds (batches=%d eBatches=%d)", batches, eBatches)
	}
}

// TestPickDirectedBatchRenameSrcDedup pins the same-source rename guard: one
// rename per source per batch, while distinct-source rename plans still share
// a batch.
func TestPickDirectedBatchRenameSrcDedup(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	sawRename, sawMultiSrc := false, false
	batches, renameBatches, multiSrcBatches := 0, 0, 0
	for seed := int64(0); seed < 500; seed++ {
		b, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if b == nil {
			continue
		}
		batches++
		seen := make(map[string]bool)
		srcs := 0
		for i := range b.Plans {
			p := &b.Plans[i]
			if p.Cell.Combo.Variant != "rename" {
				continue
			}
			sawRename = true
			if seen[p.BaseB2] {
				t.Fatalf("seed %d: duplicate rename source %q in one batch", seed, p.BaseB2)
			}
			seen[p.BaseB2] = true
			srcs++
		}
		if srcs > 0 {
			renameBatches++
		}
		if srcs >= 2 {
			sawMultiSrc = true
			multiSrcBatches++
		}
	}
	if !sawRename {
		t.Fatalf("no rename batch sampled in 500 seeds (batches=%d renameBatches=%d)", batches, renameBatches)
	}
	if !sawMultiSrc {
		t.Fatalf("no distinct-source rename batch sampled in 500 seeds (renameBatches=%d multiSrc=%d)", renameBatches, multiSrcBatches)
	}
}

// TestPickRenameDst pins the DCT rename destination choice: existing picks are
// same-parent same-class (non-empty dirs only for the ENOTEMPTY form, which
// the DCT allows), the fresh fallback follows the "._renamed_<suffix>"
// convention, and a fixed seed reproduces the sequence.
func TestPickRenameDst(t *testing.T) {
	_, hmcfg, _, lcs := renameFixture(t)
	const src = "merge_view/dirW/src"
	pick := func(seed int64) []string {
		r := rand.New(rand.NewSource(seed))
		out := make([]string, 40)
		for i := range out {
			out[i] = lcs.pickRenameDst(src, r)
		}
		return out
	}
	seq1, seq2 := pick(1), pick(1)
	for i := range seq1 {
		if seq1[i] != seq2[i] {
			t.Fatalf("pick %d not reproducible: %q vs %q", i, seq1[i], seq2[i])
		}
	}
	sawFresh, sawExisting := false, false
	for _, dst := range pick(7) {
		if strings.HasPrefix(dst, src+"._renamed_") {
			sawFresh = true
			continue
		}
		sawExisting = true
		n := hmcfg.FileTree.FindNode(dst)
		if n == nil {
			t.Fatalf("existing destination %q not in the tree", dst)
		}
		if n.Type != NodeTypeFile {
			t.Fatalf("file source must pick a file destination: %q", dst)
		}
		if dst == src || GetParentDir(dst) != GetParentDir(src) {
			t.Fatalf("destination %q must be a distinct same-parent sibling", dst)
		}
	}
	if !sawFresh || !sawExisting {
		t.Fatalf("expected both shapes over 40 picks (fresh=%v existing=%v)", sawFresh, sawExisting)
	}
	// A directory source picks a same-parent directory destination.
	const dirSrc = "merge_view/dirW/dstDir"
	for i := 0; i < 40; i++ {
		r := rand.New(rand.NewSource(int64(100 + i)))
		dst := lcs.pickRenameDst(dirSrc, r)
		if strings.HasPrefix(dst, dirSrc+"._renamed_") {
			continue
		}
		n := hmcfg.FileTree.FindNode(dst)
		if n == nil || n.Type == NodeTypeFile || n.Type == NodeTypeTmpDir {
			t.Fatalf("dir source picked a non-dir destination: %q", dst)
		}
		if GetParentDir(dst) != GetParentDir(dirSrc) {
			t.Fatalf("destination %q is not a same-parent sibling", dst)
		}
	}
}

// TestInsertRenamePlanCommits pins the dual-path variant generation: the
// inserted rename call carries old=source (BaseB2) and new=destination
// (BaseB), and the batch records its pending cells.
func TestInsertRenamePlanCommits(t *testing.T) {
	target, hmcfg, sc, lcs := renameFixture(t)
	var ps []*Prog
	batches, freshBatches, insertFails := 0, 0, 0
	for seed := int64(0); seed < 200; seed++ {
		ps = directedPlanBaseProgs(target)
		batch, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if batch == nil {
			continue
		}
		batches++
		var fresh *directedPlan
		for i := range batch.Plans {
			p := &batch.Plans[i]
			if p.Cell.Combo.Variant == "rename" && p.Cell.RetB == RetSuccess {
				fresh = p
				break
			}
		}
		if fresh == nil {
			continue
		}
		freshBatches++
		if !insertDirectedBatch(ps, newRand(target, rand.NewSource(seed+1000)), sc, hmcfg, lcs, batch) {
			insertFails++
			continue
		}
		if len(lcs.PendingDirected) != len(batch.Plans) {
			t.Fatalf("pending batch size = %d, want %d", len(lcs.PendingDirected), len(batch.Plans))
		}
		node := fresh.NodeX
		for _, c := range ps[node].Calls {
			if c == nil || c.Meta == nil || c.Meta.Name != "rename" {
				continue
			}
			oldPath, newPath := extractRenamePaths(c)
			if oldPath != fresh.BaseB2 || newPath != fresh.BaseB {
				t.Fatalf("rename args = (%q, %q), want (%q, %q)",
					oldPath, newPath, fresh.BaseB2, fresh.BaseB)
			}
			return
		}
		t.Fatalf("no rename call found on node %d", node)
	}
	t.Fatalf("no fresh rename plan inserted in 200 seeds (batches=%d fresh=%d insertFail=%d)", batches, freshBatches, insertFails)
}

// TestDirectedPlanFreshRootWitnesses pins the A4 root-side fresh forms:
// mkdir/creat roots materialize a fresh child of an existing dir (Parent) or a
// fresh sibling (Sibling), and the rename root moves an existing source to a
// fresh same-parent destination (Same/Sibling/Parent).
func TestDirectedPlanFreshRootWitnesses(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	var mkdirParent, mkdirSibling, renameSame, renameSibling, renameParent bool
	for _, p := range plans {
		switch {
		case p.FreshA && p.Cell.Combo.Root == "mkdir" && p.Cell.Combo.Rel == PathParent:
			if p.BaseA != "" || p.BaseA2 == "" || p.BaseB != p.BaseA2 {
				t.Fatalf("fresh mkdir Parent plan malformed: %+v", p)
			}
			mkdirParent = true
		case p.FreshA && p.Cell.Combo.Root == "mkdir" && p.Cell.Combo.Rel == PathSibling:
			if p.BaseA != "" || p.BaseA2 == "" || GetParentDir(p.BaseB) != p.BaseA2 {
				t.Fatalf("fresh mkdir Sibling plan malformed: %+v", p)
			}
			mkdirSibling = true
		case p.FreshA2 && p.Cell.Combo.Root == "rename" && p.Cell.Combo.Rel == PathSame:
			if p.BaseA == "" || p.BaseA != p.BaseB {
				t.Fatalf("rename root Same plan malformed: %+v", p)
			}
			renameSame = true
		case p.FreshA2 && p.Cell.Combo.Root == "rename" && p.Cell.Combo.Rel == PathSibling:
			if GetParentDir(p.BaseA) != GetParentDir(p.BaseB) {
				t.Fatalf("rename root Sibling plan not same-parent: %+v", p)
			}
			renameSibling = true
		case p.FreshA2 && p.Cell.Combo.Root == "rename" && p.Cell.Combo.Rel == PathParent:
			if GetParentDir(p.BaseA) != p.BaseB {
				t.Fatalf("rename root Parent plan must use a direct child of B: %+v", p)
			}
			renameParent = true
		}
	}
	if !mkdirParent || !mkdirSibling || !renameSame || !renameSibling || !renameParent {
		t.Fatalf("missing fresh-root forms: mkdirP=%v mkdirS=%v renSame=%v renSib=%v renPar=%v",
			mkdirParent, mkdirSibling, renameSame, renameSibling, renameParent)
	}
}

// TestDirectedPlanFreshBKindsAndGates pins the A1 synthesized fresh B forms,
// the fresh-fresh ret table and the unlink ENOENT reachability gate.
func TestDirectedPlanFreshBKindsAndGates(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	var mkdirChild, mkdirSibling, freshSame, freshENOENT bool
	for _, p := range plans {
		switch {
		case p.FreshB && p.Cell.Combo.Variant == "mkdir" && p.Cell.Combo.Rel == PathChild:
			if p.Cell.RetB != RetSuccess || !p.Cell.TypeB || p.BaseB != "" {
				t.Fatalf("fresh mkdir Child plan malformed: %+v", p)
			}
			if p.BaseA != p.BaseB2 {
				t.Fatalf("fresh mkdir Child must use the direct parent: %+v", p)
			}
			mkdirChild = true
		case p.FreshB && p.Cell.Combo.Variant == "mkdir" && p.Cell.Combo.Rel == PathSibling:
			if p.Cell.RetB != RetSuccess || !p.Cell.TypeB || p.BaseB2 == "" {
				t.Fatalf("fresh mkdir Sibling plan malformed: %+v", p)
			}
			mkdirSibling = true
		case p.FreshSame && p.Cell.Combo.Root == "mkdir" && p.Cell.Combo.Variant == "mkdir":
			if p.Cell.RetB != RetEEXIST || !p.Cell.TypeB || p.BaseA != "" || p.BaseA2 == "" {
				t.Fatalf("fresh-fresh mkdir/mkdir plan malformed: %+v", p)
			}
			freshSame = true
		}
		if p.Cell.Combo.Variant == "unlink" && p.Cell.RetB == RetENOENT && !p.FreshSame {
			if p.Cell.Combo.Root != "unlink" || p.Cell.Combo.Rel != PathSame {
				t.Fatalf("unlink ENOENT planned outside root-unlink Same: %+v", p)
			}
		}
		if p.FreshSame && p.Cell.RetB == RetENOENT {
			// Both temporal forms stay planned: an HB hit is a legitimate
			// double-delete (another variant removed the path first) or an
			// anomaly signal (the root's effect not visible to the variant).
			freshENOENT = true
		}
	}
	if !mkdirChild || !mkdirSibling || !freshSame {
		t.Fatalf("missing fresh-B forms: child=%v sibling=%v freshSame=%v",
			mkdirChild, mkdirSibling, freshSame)
	}
	if !freshENOENT {
		t.Fatal("missing fresh-fresh CC ENOENT plan")
	}
}

// TestDirectedPlanFreshSameRegularVariants pins the G-c scope-S fresh-fresh
// forms: lookup/metadata/open races against the create (stat/chmod/open), the
// newly created inode's size-0 data path (read/write at offset Zero) and the
// just-created directory listing (getdents64), plus the table invariant that
// every generated fresh-fresh plan matches a pinned form.
func TestDirectedPlanFreshSameRegularVariants(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	type form struct {
		root, variant string
		ret           RetBucket
		off           uint64
		isDir         bool
	}
	want := []form{
		{"creat", "stat", RetSuccess, offsetBucketNA, false},
		{"creat", "stat", RetENOENT, offsetBucketNA, false},
		{"creat", "open", RetENOENT, offsetBucketNA, false},
		{"creat", "read", RetSuccess, offsetBucketZero, false},
		{"mkdir", "stat", RetSuccess, offsetBucketNA, true},
		{"mkdir", "open", RetSuccess, offsetBucketNA, true},
		{"mkdir", "getdents64", RetSuccess, offsetBucketNA, true},
	}
	seen := make(map[form]bool)
	var hb, cc bool
	for _, p := range plans {
		if !p.FreshSame {
			continue
		}
		f := form{p.Cell.Combo.Root, p.Cell.Combo.Variant, p.Cell.RetB, p.Cell.OffB, p.Cell.TypeB}
		if !freshSameFormAllowed(f.root, f.variant, f.ret, f.off, f.isDir) {
			t.Fatalf("fresh-fresh plan outside the pinned form table: %+v", p)
		}
		seen[f] = true
		if p.Cell.Temporal == TemporalHB {
			hb = true
		} else {
			cc = true
		}
	}
	for _, f := range want {
		if !seen[f] {
			t.Fatalf("missing fresh-fresh form %+v", f)
		}
	}
	if !hb || !cc {
		t.Fatalf("fresh-fresh forms must carry both temporal shapes (hb=%v cc=%v)", hb, cc)
	}
}

// TestValidateFreshPlanLive pins the live recheck of every fresh-path slot.
func TestValidateFreshPlanLive(t *testing.T) {
	_, hmcfg, _, lcs := renameFixture(t)
	const (
		dirW = "merge_view/dirW"
		src  = "merge_view/dirW/src"
		sub  = "merge_view/dirW/sub"
	)
	freshChild := dirW + "/mut_dir_test"
	freshFile := dirW + "/._creat_test.txt"
	plan := func(root, variant string, rel PathRelation, ret RetBucket, typeB bool,
		a, b, a2, b2 string, freshA, freshA2, freshB, freshSame bool) directedPlan {
		depthB := DepthBucketOf(b)
		depthA := DepthBucketOf(a)
		if freshSame {
			depthB = depthA
		}
		return directedPlan{
			Cell: directedCell{
				Combo: comboKey{Root: root, Variant: variant, Rel: rel},
				RetB:  ret, OffB: offsetBucketNA,
				TypeB: typeB, DepthB: depthB,
			},
			BaseA: a, BaseB: b, BaseA2: a2, BaseB2: b2,
			FreshA: freshA, FreshA2: freshA2, FreshB: freshB, FreshSame: freshSame,
		}
	}
	valid := []directedPlan{
		plan("mkdir", "stat", PathParent, RetSuccess, true, freshChild, dirW, dirW, "", true, false, false, false),
		plan("creat", "open", PathSibling, RetSuccess, false, freshFile, src, dirW, "", true, false, false, false),
		plan("open", "mkdir", PathChild, RetSuccess, true, dirW, freshChild, "", dirW, false, false, true, false),
		plan("open", "creat", PathSibling, RetSuccess, false, src, freshFile, "", dirW, false, false, true, false),
		plan("mkdir", "mkdir", PathSame, RetEEXIST, true, freshChild, freshChild, dirW, "", false, false, false, true),
		plan("creat", "unlink", PathSame, RetENOENT, false, freshFile, freshFile, dirW, "", false, false, false, true),
		plan("rename", "stat", PathSame, RetSuccess, false, src, src, src+"._renamed_test", "", false, true, false, false),
		plan("rename", "open", PathSibling, RetSuccess, false, src, "merge_view/dirW/sib", src+"._renamed_test", "", false, true, false, false),
		// G-c scope S: lookup/metadata/open races and dir-root forms.
		plan("creat", "stat", PathSame, RetENOENT, false, freshFile, freshFile, dirW, "", false, false, false, true),
		plan("mkdir", "open", PathSame, RetSuccess, true, freshChild, freshChild, dirW, "", false, false, false, true),
		plan("mkdir", "getdents64", PathSame, RetSuccess, true, freshChild, freshChild, dirW, "", false, false, false, true),
		// P2-#8: destructive Same-root ENOENT forms — rename via the main
		// loop (root unlink removes the source) and truncate via the FreshA2
		// rename root.
		plan("unlink", "rename", PathSame, RetENOENT, false, src, dirW+"/dst._renamed_x", "", src, false, false, false, false),
		plan("rename", "truncate", PathSame, RetENOENT, false, src, src, src+"._renamed_test", "", false, true, false, false),
	}
	// The size-0 data-path form carries the pinned offset bucket (Zero), which
	// the helper's default NA cannot express.
	readPlan := plan("creat", "read", PathSame, RetSuccess, false, freshFile, freshFile, dirW, "", false, false, false, true)
	readPlan.Cell.OffB = offsetBucketZero
	valid = append(valid, readPlan)
	for i, p := range valid {
		if !validateDirectedPlanLive(hmcfg, lcs, &p) {
			t.Fatalf("valid fresh plan %d rejected: %+v", i, p)
		}
	}
	invalid := []directedPlan{
		plan("open", "stat", PathParent, RetSuccess, true, freshChild, dirW, dirW, "", true, false, false, false), // bad root
		plan("mkdir", "stat", PathParent, RetSuccess, true, freshChild, dirW, "merge_view/missing", "", true, false, false, false),
		plan("mkdir", "stat", PathParent, RetSuccess, true, src, dirW, dirW, "", true, false, false, false),                             // A exists
		plan("open", "unlink", PathChild, RetSuccess, false, dirW, freshFile, "", dirW, false, false, true, false),                      // bad FreshB call
		plan("open", "mkdir", PathChild, RetSuccess, true, src, freshChild, "", src, false, false, true, false),                         // parent is a file
		plan("mkdir", "mkdir", PathSame, RetSuccess, true, freshChild, freshChild, dirW, "", false, false, false, true),                 // bad ret
		plan("rename", "stat", PathSame, RetSuccess, false, sub, sub, sub+"._renamed_test", "", false, true, false, false),              // TmpDir root src
		plan("rename", "stat", PathSame, RetSuccess, false, src, src, "merge_view/dirW/dstDir/x", "", false, true, false, false),        // cross-parent
		plan("mkdir", "stat", PathParent, RetSuccess, true, sub+"/mut_dir_test", sub, sub, "", true, false, false, false),               // TmpDir root container
		plan("mkdir", "mkdir", PathSame, RetEEXIST, true, sub+"/mut_dir_test", sub+"/mut_dir_test", sub, "", false, false, false, true), // TmpDir host
		// G-c scope S negatives: outside the pinned table or with wrong margins.
		plan("mkdir", "read", PathSame, RetSuccess, false, freshChild, freshChild, dirW, "", false, false, false, true), // not a pinned form
		plan("creat", "stat", PathSame, RetENOENT, true, freshFile, freshFile, dirW, "", false, false, false, true),     // wrong TypeB
		plan("creat", "read", PathSame, RetSuccess, false, freshFile, freshFile, dirW, "", false, false, false, true),   // wrong OffB (NA)
		plan("mkdir", "open", PathSame, RetENOENT, false, freshChild, freshChild, dirW, "", false, false, false, true),  // wrong TypeB
		// P2-#8 negatives: the rename ENOENT form requires the unlink root and
		// the root base to be the rename source.
		plan("unlink", "rename", PathSame, RetENOENT, false, src, dirW+"/dst._renamed_x", "", "merge_view/dirW/sib", false, false, false, false), // BaseA != BaseB2
		plan("open", "rename", PathSame, RetENOENT, false, src, dirW+"/dst._renamed_x", "", src, false, false, false, false),                     // wrong root
	}
	for i, p := range invalid {
		if validateDirectedPlanLive(hmcfg, lcs, &p) {
			t.Fatalf("invalid fresh plan %d accepted: %+v", i, p)
		}
	}
}

// TestPickDirectedBatchMaterializesFreshPaths pins fresh-path materialization:
// deterministic per seed, fresh paths absent from the tree and prefix-shaped.
func TestPickDirectedBatchMaterializesFreshPaths(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	pick := func(seed int64) *directedBatch {
		b, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		return b
	}
	batches, freshPlans := 0, 0
	for seed := int64(0); seed < 200; seed++ {
		b1, b2 := pick(seed), pick(seed)
		if b1 == nil {
			continue
		}
		batches++
		if b2 == nil || len(b1.Plans) != len(b2.Plans) {
			t.Fatalf("seed %d: batch not reproducible", seed)
		}
		for i := range b1.Plans {
			if b1.Plans[i] != b2.Plans[i] {
				t.Fatalf("seed %d: plan %d differs", seed, i)
			}
		}
		for i := range b1.Plans {
			p := &b1.Plans[i]
			switch {
			case p.FreshA:
				if p.BaseA == "" || hmcfg.FileTree.FindNode(p.BaseA) != nil {
					t.Fatalf("fresh A not materialized/free: %+v", p)
				}
				if !strings.Contains(p.BaseA, "/mut_dir_") && !strings.Contains(p.BaseA, "/._creat_") {
					t.Fatalf("fresh A lacks the naming convention: %q", p.BaseA)
				}
			case p.FreshA2:
				if p.BaseA2 == "" || hmcfg.FileTree.FindNode(p.BaseA2) != nil ||
					!strings.Contains(p.BaseA2, "._renamed_") {
					t.Fatalf("fresh rename dst not materialized: %+v", p)
				}
			case p.FreshB:
				if p.BaseB == "" || hmcfg.FileTree.FindNode(p.BaseB) != nil {
					t.Fatalf("fresh B not materialized/free: %+v", p)
				}
			case p.FreshSame:
				if p.BaseA == "" || p.BaseA != p.BaseB {
					t.Fatalf("fresh-fresh same-path not materialized: %+v", p)
				}
			default:
				continue
			}
			freshPlans++
			return
		}
	}
	t.Fatalf("no fresh plan sampled in 200 seeds (batches=%d fresh=%d)", batches, freshPlans)
}

// TestInsertFreshRootPlanCommits pins the mkdir/creat root generation on a
// materialized fresh path: the inserted root call carries that exact path.
func TestInsertFreshRootPlanCommits(t *testing.T) {
	target, hmcfg, sc, lcs := renameFixture(t)
	batches, freshBatches, insertFails := 0, 0, 0
	for seed := int64(0); seed < 200; seed++ {
		ps := directedPlanBaseProgs(target)
		batch, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if batch == nil {
			continue
		}
		batches++
		var fresh *directedPlan
		for i := range batch.Plans {
			if batch.Plans[i].FreshA {
				fresh = &batch.Plans[i]
				break
			}
		}
		if fresh == nil {
			continue
		}
		freshBatches++
		if !insertDirectedBatch(ps, newRand(target, rand.NewSource(seed+1000)), sc, hmcfg, lcs, batch) {
			insertFails++
			continue
		}
		for _, c := range ps[0].Calls {
			if c == nil || c.Meta == nil || c.Meta.Name != fresh.Cell.Combo.Root {
				continue
			}
			if extractPathFromCall(c) == fresh.BaseA {
				return
			}
		}
		t.Fatalf("fresh root call %q on %q not found", fresh.Cell.Combo.Root, fresh.BaseA)
	}
	t.Fatalf("no fresh-root plan inserted in 200 seeds (batches=%d fresh=%d insertFail=%d)", batches, freshBatches, insertFails)
}

// TestInsertFreshSameRegularVariantCommits pins the insertion of a G-c
// non-creating variant on the materialized fresh path: the stat call lands on
// the batch's fresh path (BaseB), not on the anchor directory or the tree path.
func TestInsertFreshSameRegularVariantCommits(t *testing.T) {
	target, hmcfg, sc, lcs := renameFixture(t)
	batches, freshBatches, insertFails := 0, 0, 0
	for seed := int64(0); seed < 200; seed++ {
		ps := directedPlanBaseProgs(target)
		batch, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if batch == nil {
			continue
		}
		batches++
		var fresh *directedPlan
		for i := range batch.Plans {
			p := &batch.Plans[i]
			if p.FreshSame && p.Cell.Combo.Variant == "stat" {
				fresh = p
				break
			}
		}
		if fresh == nil {
			continue
		}
		freshBatches++
		if !insertDirectedBatch(ps, newRand(target, rand.NewSource(seed+1000)), sc, hmcfg, lcs, batch) {
			insertFails++
			continue
		}
		for _, c := range ps[fresh.NodeX].Calls {
			if c == nil || c.Meta == nil || c.Meta.Name != "stat" {
				continue
			}
			if extractPathFromCall(c) == fresh.BaseB {
				return
			}
		}
		t.Fatalf("fresh-fresh stat call on %q not found", fresh.BaseB)
	}
	t.Fatalf("no fresh-fresh stat plan inserted in 200 seeds (batches=%d fresh=%d insertFail=%d)", batches, freshBatches, insertFails)
}

// TestDirectedPlanUnlinkVariant pins the delete forms: unlink variants only
// target files, the ENOENT ret is planned only when the same batch's root is an
// unlink on the same path (or in fresh-fresh races), and unlink roots execute.
func TestDirectedPlanUnlinkVariant(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	var success, enoent, unlinkRoot bool
	for _, p := range plans {
		if p.Cell.Combo.Root == "unlink" {
			unlinkRoot = true
		}
		if p.Cell.Combo.Variant != "unlink" {
			continue
		}
		if p.Cell.TypeB {
			t.Fatalf("unlink variant must target a file: %+v", p)
		}
		switch p.Cell.RetB {
		case RetSuccess:
			success = true
		case RetENOENT:
			if p.FreshSame {
				continue // fresh-fresh create/delete race
			}
			if p.Cell.Combo.Root != "unlink" || p.Cell.Combo.Rel != PathSame {
				t.Fatalf("ENOENT planned outside root-unlink Same: %+v", p)
			}
			enoent = true
		}
	}
	if !success {
		t.Fatal("no unlink Success plans")
	}
	if !enoent {
		t.Fatal("no root-unlink Same ENOENT plan")
	}
	if !unlinkRoot {
		t.Fatal("no unlink root plans")
	}
}

// TestDirectedPlanDestructiveSameGate pins the destructive-Same root gates:
// root unlink (and the FreshA2 rename) removes the Same path, so HB placements
// may realize a recreate (creat Success), an fd success (the embedded open can
// win the race and the fd survives the unlink), or a not-found failure; CC
// placements may not realize a second remover/mover Success, and the
// name-resolving variants gain the observable ENOENT form (file targets only).
func TestDirectedPlanDestructiveSameGate(t *testing.T) {
	target, hmcfg, _, lcs := renameFixture(t)
	ps := directedPlanBaseProgs(target)
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("no candidate plans")
	}
	var creatHB, fdHB, statENOENT, openENOENT, chmodENOENT, truncateENOENT, renameENOENT bool
	for _, p := range plans {
		dest := (p.Cell.Combo.Root == "unlink" && p.Cell.Combo.Rel == PathSame) ||
			(p.FreshA2 && p.Cell.Combo.Root == "rename" && p.Cell.Combo.Rel == PathSame)
		if !dest {
			continue
		}
		v, ret := p.Cell.Combo.Variant, p.Cell.RetB
		if p.Cell.Temporal == TemporalHB && destructiveHBDoomed(v, ret) {
			t.Fatalf("doomed HB plan under a destructive Same root: %+v", p)
		}
		if p.Cell.Temporal == TemporalConcurrent && destructiveCCExclusive(v, ret) {
			t.Fatalf("mutually exclusive CC plan under a destructive Same root: %+v", p)
		}
		if ret == RetENOENT {
			if p.Cell.TypeB {
				t.Fatalf("directory ENOENT form must not be planned: %+v", p)
			}
			if p.Cell.Temporal == TemporalHB {
				switch v {
				case "stat":
					statENOENT = true
				case "open":
					openENOENT = true
				case "chmod":
					chmodENOENT = true
				case "truncate":
					truncateENOENT = true
				case "rename":
					renameENOENT = true
					if p.BaseA != p.BaseB2 {
						t.Fatalf("rename ENOENT plan source mismatch: %+v", p)
					}
				}
			}
		}
		if p.Cell.Temporal == TemporalHB && ret == RetSuccess && fdHBVariants[v] {
			fdHB = true
		}
		if p.Cell.Combo.Root == "unlink" && v == "creat" && ret == RetSuccess &&
			p.Cell.Temporal == TemporalHB {
			creatHB = true
		}
	}
	if !creatHB || !fdHB || !statENOENT || !openENOENT || !chmodENOENT ||
		!truncateENOENT || !renameENOENT {
		t.Fatalf("missing destructive-Same forms: creatHB=%v fdHB=%v statENOENT=%v openENOENT=%v chmodENOENT=%v truncateENOENT=%v renameENOENT=%v",
			creatHB, fdHB, statENOENT, openENOENT, chmodENOENT, truncateENOENT, renameENOENT)
	}
}

// TestDirectedPlannerSkipsKnownShapes pins the event-shape selection guard: a
// cell whose event shape is already known (here pwrite64's Zero-bucket shape,
// registered through its write twin) is no longer planned, while a cell with
// an unknown shape (the Beyond bucket) stays.
func TestDirectedPlannerSkipsKnownShapes(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	cids := hmdfsSmokeCids()[:2]
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir:  map[string][]string{cids[0]: {"merge_view/dirW"}},
		Init_file: map[string][]string{cids[0]: {"merge_view/dirW/a", "merge_view/dirW/b"}},
	})
	hmcfg := &Hmdfs_config{Cids: cids, Node_num: 2, FileTree: ft}
	newTestCoverageBoard(t)
	ps := make([]*Prog, 2)
	for i := range ps {
		ps[i] = &Prog{Target: target}
	}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	has := func(plans map[cellKey]*directedPlan, off uint64) bool {
		for _, p := range plans {
			if p.Cell.Combo.Root == "open" && p.Cell.Combo.Variant == "pwrite64" &&
				p.Cell.Combo.Rel == PathSame && p.Cell.OffB == off {
				return true
			}
		}
		return false
	}
	plans, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if !has(plans, offsetBucketZero) {
		t.Fatal("fixture: open/pwrite64/Same Zero plan missing")
	}
	// Register the Zero shape through the write twin: the pwrite64 cell itself
	// stays undiscovered, so only the shape guard can suppress it.
	RecordCoveragePairs([]DAGPair{{
		A: &DAGVertex{CallName: "open", Path: "merge_view/dirW/a",
			RetBucket: RetSuccess, FuncID: FuncFileOpen},
		B: &DAGVertex{CallName: "write", Path: "merge_view/dirW/a",
			RetBucket: RetSuccess, FuncID: FuncWrite, Size: 0, Off: 0},
		Temporal: TemporalConcurrent,
		PathRel:  PathSamePath,
	}})
	plans2, _ := planDirectedCells(ps, lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if has(plans2, offsetBucketZero) {
		t.Fatal("known event shape must suppress the twin plan")
	}
	if !has(plans2, offsetBucketBeyond) {
		t.Fatal("unknown-shape cell must stay planned")
	}
}

// TestDirectedTruncateLoserKeepsSize pins the truncate realization rule: a
// Success plan uses its realized BLen, while the raced loser form truncates to
// the current size (a size no-op if the race is won), so co-batched same-B
// plans keep their predictions.
func TestDirectedTruncateLoserKeepsSize(t *testing.T) {
	target, hmcfg, sc, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	const path = "merge_view/dirA/a"
	const size = 4096
	n := lcs.FileTree.FindNode(path)
	if n == nil {
		t.Fatalf("fixture file %q missing", path)
	}
	n.Size = size
	r := newRand(target, rand.NewSource(1))
	truncLen := func(ret RetBucket, blen uint64) uint64 {
		plan := &directedPlan{
			Cell: directedCell{
				Combo: comboKey{Root: "unlink", Variant: "truncate", Rel: PathSame},
				RetB:  ret, OffB: offsetBucketNA,
			},
			BaseB: path,
			BLen:  blen,
		}
		calls := directedVariantCallsSeq(r, sc, lcs, hmcfg, 1, ps[0], plan)
		if len(calls) != 1 || len(calls[0].Args) < 2 {
			t.Fatalf("truncate call not materialized: %v", calls)
		}
		ca, ok := calls[0].Args[1].(*ConstArg)
		if !ok {
			t.Fatalf("truncate length arg is not a ConstArg")
		}
		return ca.Val
	}
	if got := truncLen(RetSuccess, 123); got != 123 {
		t.Fatalf("success plan length = %d, want BLen 123", got)
	}
	if got := truncLen(RetENOENT, 0); got != size {
		t.Fatalf("loser plan length = %d, want current size %d", got, size)
	}
}

// TestPlanStateEffectMkdir pins the authority classification: any mkdir plan
// counts as a creator (its EEXIST prediction can flip to a successful creation
// when the batch root removes or moves the dir), while the remover loser forms
// stay observers on purpose.
func TestPlanStateEffectMkdir(t *testing.T) {
	eff := func(variant string, ret RetBucket) planEffect {
		return planStateEffect(&directedPlan{
			Cell: directedCell{
				Combo: comboKey{Root: "unlink", Variant: variant, Rel: PathSame},
				RetB:  ret,
			},
		})
	}
	if eff("mkdir", RetEEXIST) == planEffectObserver {
		t.Fatal("mkdir EEXIST must count as a creator for the authority filter")
	}
	if eff("truncate", RetENOENT) != planEffectObserver {
		t.Fatal("truncate loser form stays an observer (realization is a size no-op)")
	}
	if eff("unlink", RetENOENT) != planEffectObserver {
		t.Fatal("remover loser forms stay observers (same effect as the batch root)")
	}
	if eff("truncate", RetSuccess) != planEffectSize {
		t.Fatal("truncate Success must stay a size writer")
	}
}
