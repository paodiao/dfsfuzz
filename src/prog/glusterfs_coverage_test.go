package prog

import (
	"math/rand"
	"testing"
)

// glusterfsCoverageConfig initializes a fresh GlusterFS coverage board. The
// profile is copied so the dual-path gate can be toggled without mutating the
// package default.
func glusterfsCoverageConfig(t *testing.T, dual bool) *Hmdfs_config {
	t.Helper()
	gp := *glusterfsProfile
	gp.Plan.DualPathCausalEnabled = dual
	hmcfg := &Hmdfs_config{
		Profile: &gp, DfsName: "glusterfs", Node_num: 3,
		Cids: hmdfsSmokeCids(), FileTree: glusterfsPlanTree(),
	}
	ResetCoverageBoard()
	InitCoverageBoard(hmcfg)
	t.Cleanup(ResetCoverageBoard)
	return hmcfg
}

func glusterfsCoverageBaseProgs(target *Target) []*Prog {
	ps := make([]*Prog, 0, 3)
	for i := 0; i < 3; i++ {
		o := mkOpen(target, "dirA/a")
		c := mkClose(target, o)
		o.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		c.CheckInfo = &FileMetadata{Stime: 210, Etime: 260}
		ps = append(ps, &Prog{Target: target, Calls: []*Call{o, c}})
	}
	return ps
}

func glusterfsCoverageCell(root, variant string, rel PathRelation, temporal TemporalRel,
	ret RetBucket, off uint64, isDir bool) cellKey {
	return cellKey{
		Root: root, Variant: variant, Rel: rel, Temporal: temporal,
		RetA: RetSuccess, RetB: ret, OffB: off, PersistB: false,
		DepthB: 1, TypeB: isDir,
	}
}

// TestGlusterfsFreshSameFormsKeepRaceMatrix pins that the fresh-fresh race
// matrix is DFS-agnostic and stays complete for GlusterFS. The narrowing is
// applied by the plannable layer, not by deleting forms from the table.
func TestGlusterfsFreshSameFormsKeepRaceMatrix(t *testing.T) {
	forms := freshSameFormsFor(glusterfsProfile)
	if len(forms) != len(freshSameForms) {
		t.Fatalf("GlusterFS freshSame table size = %d, want the full race matrix %d",
			len(forms), len(freshSameForms))
	}
	for _, tc := range []struct {
		root, variant string
		ret           RetBucket
		isDir         bool
	}{
		{"mkdir", "mkdir", RetEEXIST, true},
		{"creat", "mkdir", RetEEXIST, true},
		{"mkdir", "creat", RetFailure, false},
		{"mkdir", "rmdir", RetENOENT, true},
		{"creat", "stat", RetENOENT, false},
	} {
		if !freshSameFormAllowedFor(glusterfsProfile, tc.root, tc.variant, tc.ret,
			offsetBucketNA, tc.isDir) {
			t.Fatalf("GlusterFS freshSame missing race form %+v", tc)
		}
	}
}

// TestGlusterfsPlannableFailurePolicy pins the FailurePolicy table exposed by
// the plannable layer.
func TestGlusterfsPlannableFailurePolicy(t *testing.T) {
	glusterfsCoverageConfig(t, false)

	covBoard.mu.RLock()
	defer covBoard.mu.RUnlock()

	cases := []struct {
		name    string
		variant string
		ret     RetBucket
		off     uint64
		isDir   bool
		want    bool
	}{
		{"mkdir Success", "mkdir", RetSuccess, offsetBucketNA, true, true},
		{"mkdir EEXIST", "mkdir", RetEEXIST, offsetBucketNA, true, false},
		{"creat Success", "creat", RetSuccess, offsetBucketNA, false, true},
		{"creat Failure", "creat", RetFailure, offsetBucketNA, false, false},
		{"open Success", "open", RetSuccess, offsetBucketNA, false, true},
		{"open ENOENT", "open", RetENOENT, offsetBucketNA, false, false},
		{"read offset 0", "read", RetSuccess, offsetBucketZero, false, true},
		{"read offset mid", "read", RetSuccess, offsetBucketMid, false, false},
		{"write Success", "write", RetSuccess, offsetBucketZero, false, true},
		{"write Failure", "write", RetFailure, offsetBucketZero, false, false},
		{"rmdir Failure", "rmdir", RetFailure, offsetBucketNA, true, true},
		{"rename Failure", "rename", RetFailure, offsetBucketNA, false, true},
		{"getxattr Failure", "getxattr", RetFailure, offsetBucketNA, false, true},
		{"setxattr EEXIST", "setxattr", RetEEXIST, offsetBucketNA, false, true},
		{"link Success", "link", RetSuccess, offsetBucketNA, false, false},
		{"symlink Success", "symlink", RetSuccess, offsetBucketNA, false, false},
	}
	for _, tc := range cases {
		ck := glusterfsCoverageCell("write", tc.variant, PathSibling, TemporalHB, tc.ret, tc.off, tc.isDir)
		if got := covBoard.cellInPlannable(ck); got != tc.want {
			t.Fatalf("%s: cellInPlannable = %v, want %v (%+v)", tc.name, got, tc.want, ck)
		}
	}
	// mkdir EEXIST stays inside the model layer, so natural observations are
	// still scored there; only the planner view excludes it.
	mkdirEEXIST := glusterfsCoverageCell("write", "mkdir", PathSibling, TemporalHB,
		RetEEXIST, offsetBucketNA, true)
	if !covBoard.cellInModel(mkdirEEXIST) {
		t.Fatal("mkdir EEXIST must remain model-visible")
	}
}

// TestGlusterfsPlannableFreshSameRaces pins B's combo-aware split: the
// fresh-fresh race matrix is plannable only for creation-root PathSame combos,
// while the same ret buckets on ordinary existing-object combos stay out.
func TestGlusterfsPlannableFreshSameRaces(t *testing.T) {
	glusterfsCoverageConfig(t, false)

	covBoard.mu.RLock()
	defer covBoard.mu.RUnlock()

	cases := []struct {
		name    string
		root    string
		variant string
		ret     RetBucket
		off     uint64
		isDir   bool
		want    bool
	}{
		{"creat/mkdir EEXIST", "creat", "mkdir", RetEEXIST, offsetBucketNA, true, true},
		{"mkdir/creat Failure", "mkdir", "creat", RetFailure, offsetBucketNA, false, true},
		{"mkdir/mkdir EEXIST", "mkdir", "mkdir", RetEEXIST, offsetBucketNA, true, true},
		{"mkdir/mkdir Success", "mkdir", "mkdir", RetSuccess, offsetBucketNA, true, false},
		{"mkdir/rmdir ENOENT", "mkdir", "rmdir", RetENOENT, offsetBucketNA, true, true},
		{"open/rmdir ENOENT", "open", "rmdir", RetENOENT, offsetBucketNA, true, false},
		{"creat/unlink ENOENT", "creat", "unlink", RetENOENT, offsetBucketNA, false, true},
		{"open/unlink ENOENT", "open", "unlink", RetENOENT, offsetBucketNA, false, false},
		{"creat/stat ENOENT", "creat", "stat", RetENOENT, offsetBucketNA, false, true},
		{"open/stat ENOENT", "open", "stat", RetENOENT, offsetBucketNA, false, false},
		{"mkdir/getdents64 Success", "mkdir", "getdents64", RetSuccess, offsetBucketNA, true, true},
		{"creat/read Success zero", "creat", "read", RetSuccess, offsetBucketZero, false, true},
	}
	for _, tc := range cases {
		ck := glusterfsCoverageCell(tc.root, tc.variant, PathSame, TemporalHB,
			tc.ret, tc.off, tc.isDir)
		if got := covBoard.cellInPlannable(ck); got != tc.want {
			t.Fatalf("%s: cellInPlannable = %v, want %v (%+v)", tc.name, got, tc.want, ck)
		}
		if tc.want && !covBoard.cellInModel(ck) {
			t.Fatalf("%s: plannable cell is outside the model: %+v", tc.name, ck)
		}
	}

	// Enumeration must agree with the combo-aware plannable forms: the
	// creat/mkdir Same combo yields only the EEXIST/dir race cells.
	combo := comboKey{Root: "creat", Variant: "mkdir", Rel: PathSame}
	var cells []cellKey
	covBoard.forEachUncoveredCellKey(combo, func(c cellKey) {
		cells = append(cells, c)
	})
	if len(cells) == 0 {
		t.Fatal("creat/mkdir Same race combo has no enumerable cells")
	}
	for _, c := range cells {
		if c.RetB != RetEEXIST || !c.TypeB {
			t.Fatalf("creat/mkdir Same enumeration produced a non-race cell: %+v", c)
		}
	}
}

// TestGlusterfsPlannableDualPathGate pins the v1 gate: link/symlink/readlink
// are observed/model-visible but not actively plannable until the dual-path
// gate is enabled.
func TestGlusterfsPlannableDualPathGate(t *testing.T) {
	glusterfsCoverageConfig(t, false)

	covBoard.mu.RLock()
	linkCell := glusterfsCoverageCell("write", "link", PathSibling, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	symlinkCell := glusterfsCoverageCell("write", "symlink", PathSibling, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	readlinkCell := glusterfsCoverageCell("symlink", "readlink", PathSibling, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	if covBoard.cellInPlannable(linkCell) || covBoard.cellInPlannable(symlinkCell) ||
		covBoard.cellInPlannable(readlinkCell) {
		t.Fatal("disabled dual-path gate must keep link/symlink/readlink out of plannable")
	}
	covBoard.mu.RUnlock()

	glusterfsCoverageConfig(t, true)

	covBoard.mu.RLock()
	defer covBoard.mu.RUnlock()
	if !covBoard.cellInPlannable(linkCell) {
		t.Fatal("enabled gate must make link plannable")
	}
	if !covBoard.cellInPlannable(symlinkCell) {
		t.Fatal("enabled gate must make symlink plannable")
	}
	if !covBoard.cellInPlannable(readlinkCell) {
		t.Fatal("enabled gate must make symlink-root readlink plannable")
	}
	badReadlink := glusterfsCoverageCell("write", "readlink", PathSibling, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	if covBoard.cellInPlannable(badReadlink) {
		t.Fatal("readlink must not be plannable under a non-symlink root")
	}
}

// TestGlusterfsPlannableSubsetOfModel is the table-drift guard: every
// plannable form stays inside the model's dimensions.
func TestGlusterfsPlannableSubsetOfModel(t *testing.T) {
	glusterfsCoverageConfig(t, true)

	covBoard.mu.RLock()
	defer covBoard.mu.RUnlock()
	for ck, pp := range covBoard.plannable {
		mp, ok := covBoard.producible[ck]
		if !ok {
			t.Fatalf("plannable combo %+v missing from model", ck)
		}
		for tm := 0; tm < 2; tm++ {
			if pp[tm] > mp[tm] {
				t.Fatalf("combo %+v temporal %d: plannable %d > model %d",
					ck, tm, pp[tm], mp[tm])
			}
		}
	}
}

func TestHMDFSPlannableMatchesModel(t *testing.T) {
	ResetCoverageBoard()
	InitCoverageBoard(&Hmdfs_config{Persistence_dir: "merge_view/persist"})
	t.Cleanup(ResetCoverageBoard)

	covBoard.mu.RLock()
	defer covBoard.mu.RUnlock()
	for ck, mp := range covBoard.producible {
		pp, ok := covBoard.plannable[ck]
		if !ok {
			t.Fatalf("HMDFS combo %+v missing from plannable", ck)
		}
		if pp != mp {
			t.Fatalf("HMDFS combo %+v: plannable %v != model %v", ck, pp, mp)
		}
	}
}

// TestGlusterfsObservedVsModelOnly pins the three ledgers on the two failure
// classes: a truly out-of-model return and a model-only return (mkdir EEXIST).
func TestGlusterfsObservedVsModelOnly(t *testing.T) {
	glusterfsCoverageConfig(t, false)

	outOfModel := mkCoveragePair("write", "link", PathSameParent, TemporalHB,
		RetSuccess, RetEEXIST, "dirA/a")
	modelOnly := mkCoveragePair("write", "mkdir", PathSameParent, TemporalHB,
		RetSuccess, RetEEXIST, "dirA/a")
	modelOnly.B.IsDir = true
	RecordCoveragePairs([]DAGPair{outOfModel, modelOnly})

	cov := CoverageLayerStats()
	if cov.ObservedCells != 2 {
		t.Fatalf("ObservedCells = %d, want 2", cov.ObservedCells)
	}
	if cov.ModelFound != 1 {
		t.Fatalf("ModelFound = %d, want 1 (mkdir EEXIST only)", cov.ModelFound)
	}
	if cov.PlannableFound != 0 {
		t.Fatalf("PlannableFound = %d, want 0", cov.PlannableFound)
	}
	if cov.OutOfModel != 1 {
		t.Fatalf("OutOfModel = %d, want 1 (link EEXIST)", cov.OutOfModel)
	}
}

// TestMkdirEEXISTDoesNotDeductPlannablePotential pins the standalone case:
// natural existing-object mkdir EEXIST is observed in the model, but it neither
// removes a plannable cell nor becomes a planner target. The fresh-fresh
// mkdir EEXIST race forms are covered separately.
func TestMkdirEEXISTDoesNotDeductPlannablePotential(t *testing.T) {
	glusterfsCoverageConfig(t, false)

	pot0 := CoveragePotential("write", "mkdir", PathSibling)
	if pot0 <= 0 {
		t.Fatalf("mkdir plannable potential = %d, want > 0", pot0)
	}
	eexist := mkCoveragePair("write", "mkdir", PathSameParent, TemporalHB,
		RetSuccess, RetEEXIST, "dirA/a")
	eexist.B.IsDir = true
	RecordCoveragePairs([]DAGPair{eexist})
	if got := CoveragePotential("write", "mkdir", PathSibling); got != pot0 {
		t.Fatalf("mkdir EEXIST deducted plannable potential: %d -> %d", pot0, got)
	}

	success := mkCoveragePair("write", "mkdir", PathSameParent, TemporalHB,
		RetSuccess, RetSuccess, "dirA/a")
	success.B.IsDir = true
	RecordCoveragePairs([]DAGPair{success})
	if got := CoveragePotential("write", "mkdir", PathSibling); got != pot0-1 {
		t.Fatalf("mkdir Success did not deduct plannable potential: %d -> %d, want %d",
			pot0, got, pot0-1)
	}
}

// TestGlusterfsDirectedPlansArePlannable pins the normal planner invariant:
// every emitted plan is inside the plannable layer, standalone mkdir EEXIST
// stays out, and the fresh-fresh race forms under creation roots are emitted.
func TestGlusterfsDirectedPlansArePlannable(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, false)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(73))
	plans, _ := planDirectedCells(glusterfsCoverageBaseProgs(target), lcs, hmcfg, r)
	if len(plans) == 0 {
		t.Fatal("no normal plans generated")
	}
	sawMkdirCreatFailure := false
	sawCreatMkdirEEXIST := false
	for ck, p := range plans {
		if !covBoard.cellUncovered(ck) {
			t.Fatalf("plan outside plannable: %+v (%+v)", ck, p)
		}
		// mkdir EEXIST is legal only as a fresh-fresh race under a creation
		// root; the standalone existing-object form stays unplanned.
		if ck.Variant == "mkdir" && ck.RetB == RetEEXIST &&
			!(ck.Rel == PathSame && (ck.Root == "mkdir" || ck.Root == "creat")) {
			t.Fatalf("standalone mkdir EEXIST plan leaked into the normal planner: %+v", ck)
		}
		if ck.Root == "mkdir" && ck.Variant == "creat" &&
			ck.Rel == PathSame && ck.RetB == RetFailure {
			sawMkdirCreatFailure = true
		}
		if ck.Root == "creat" && ck.Variant == "mkdir" &&
			ck.Rel == PathSame && ck.RetB == RetEEXIST {
			sawCreatMkdirEEXIST = true
		}
		if ck.Root == "link" || ck.Root == "symlink" {
			t.Fatalf("disabled dual-path gate emitted a dual root: %+v", ck)
		}
	}
	if !sawMkdirCreatFailure || !sawCreatMkdirEEXIST {
		t.Fatalf("missing fresh-fresh race plans: mkdir/creat Failure=%v creat/mkdir EEXIST=%v",
			sawMkdirCreatFailure, sawCreatMkdirEEXIST)
	}
}

// TestGlusterfsNormalDualRootBatchInsert pins the execution-side wiring: a
// normal dual-root batch is accepted by insertDirectedBatch, which materializes
// the link call from RootSource (target) and RootDest (linkpath).
func TestGlusterfsNormalDualRootBatchInsert(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	sc := hmdfsSmokeSpecialCalls(t, target)
	r := newRand(target, rand.NewSource(101))
	plans, _ := planDirectedCells(glusterfsCoverageBaseProgs(target), lcs, hmcfg, r)

	var dual, ccDual *directedPlan
	for _, p := range plans {
		if p.Cell.Combo.Root != "link" ||
			(p.Cell.Combo.Variant != "open" && p.Cell.Combo.Variant != "stat") {
			continue
		}
		cp := *p
		if p.Cell.Temporal == TemporalConcurrent {
			ccDual = &cp
			break
		}
		if dual == nil {
			dual = &cp
		}
	}
	if ccDual != nil {
		dual = ccDual
	}
	if dual == nil {
		t.Fatal("no normal link-root plan with an open/stat variant")
	}
	dual.NodeX = 1
	batch := &directedBatch{
		Root: dual.Cell.Combo.Root, BaseA: dual.BaseA,
		RootSource: dual.BaseA, RootDest: dual.RootLinkPath,
		Plans: []directedPlan{*dual},
	}
	ps := glusterfsCoverageBaseProgs(target)
	if !insertDirectedBatch(ps, r, sc, hmcfg, lcs, batch) {
		t.Fatal("normal dual-root batch failed to insert")
	}
}

// TestGlusterfsNormalDualRootPlans pins the normal dual-root pass: with the
// gate enabled, link/symlink roots carry a target BaseA plus a fresh
// RootLinkPath, and readlink's B side is exactly that linkpath.
func TestGlusterfsNormalDualRootPlans(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(91))
	plans, _ := planDirectedCells(glusterfsCoverageBaseProgs(target), lcs, hmcfg, r)

	seen := map[string]bool{}
	readlinkChecked := false
	for _, p := range plans {
		root := p.Cell.Combo.Root
		if root != "link" && root != "symlink" {
			continue
		}
		if !p.Causal || p.BaseA == "" || p.RootLinkPath == "" {
			t.Fatalf("malformed normal dual plan: %+v", p)
		}
		if GetParentDir(p.BaseA) != GetParentDir(p.RootLinkPath) {
			t.Fatalf("linkpath %q is not next to target %q", p.RootLinkPath, p.BaseA)
		}
		if root == "link" && p.Cell.TypeB {
			t.Fatalf("link target must be a file: %+v", p)
		}
		if p.Cell.Combo.Variant == "readlink" {
			readlinkChecked = true
			if p.BaseB != p.RootLinkPath {
				t.Fatalf("readlink BaseB = %q, want RootLinkPath %q",
					p.BaseB, p.RootLinkPath)
			}
		}
		seen[root] = true
	}
	if !seen["link"] || !seen["symlink"] {
		t.Fatalf("missing normal dual roots: %v", seen)
	}
	if !readlinkChecked {
		t.Fatal("no readlink plan under a symlink root")
	}
}
