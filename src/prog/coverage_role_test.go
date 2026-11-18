package prog

import "testing"

func linkRolePair(target, linkPath, rel string, temporal TemporalRel) DAGPair {
	a := &DAGVertex{
		FuncID: FuncLink, CallName: "link",
		Path: linkPath, OldPath: target, RetBucket: RetSuccess,
	}
	b := &DAGVertex{
		FuncID: FuncGetattr, CallName: "stat",
		Path: linkPath, RetBucket: RetSuccess,
	}
	pathRel := PathSamePath
	if rel == "sibling" {
		pathRel = PathSameParent
	}
	return DAGPair{
		A: a, B: b, APath: linkPath, BPath: linkPath,
		PathRel: pathRel, Temporal: temporal,
	}
}

func TestCellRoleLinkRoot(t *testing.T) {
	glusterfsCoverageConfig(t, true)
	const (
		target   = "dirA/a"
		linkPath = "dirA/l1"
	)
	targetPair := linkRolePair(target, linkPath, "sibling", TemporalHB)
	targetPair.APath = target
	targetPair.PathRel = PathSameParent
	ck, ok := cellKeyOf(&targetPair, "")
	if !ok || ck.Role != cellRoleNone {
		t.Fatalf("target-role cell = %+v (ok=%v), want default role", ck, ok)
	}

	linkPair := linkRolePair(target, linkPath, "same", TemporalHB)
	ck, ok = cellKeyOf(&linkPair, "")
	if !ok || ck.Role != cellRoleLinkPath {
		t.Fatalf("linkpath-role cell = %+v (ok=%v), want linkpath role", ck, ok)
	}

	cc := ck
	cc.Temporal = TemporalConcurrent
	cc.Role = cellRoleLinkPath
	covBoard.mu.RLock()
	if covBoard.cellInModel(cc) || covBoard.cellInPlannable(cc) {
		t.Fatalf("linkpath CC cell accepted: %+v", cc)
	}
	cc.Role = cellRoleNone
	if !covBoard.cellInPlannable(cc) {
		t.Fatalf("target CC cell rejected: %+v", cc)
	}
	hb := cc
	hb.Temporal = TemporalHB
	hb.Role = cellRoleLinkPath
	if !covBoard.cellInPlannable(hb) {
		t.Fatalf("linkpath HB cell rejected: %+v", hb)
	}
	covBoard.mu.RUnlock()
}

func TestDefaultGlusterfsDualPathPlannable(t *testing.T) {
	hmcfg := &Hmdfs_config{Profile: glusterfsProfile, DfsName: "glusterfs",
		Node_num: 3, Cids: hmdfsSmokeCids(), FileTree: glusterfsPlanTree()}
	ResetCoverageBoard()
	InitCoverageBoard(hmcfg)
	t.Cleanup(ResetCoverageBoard)
	cell := glusterfsCoverageCell("write", "link", PathSibling, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	covBoard.mu.RLock()
	defer covBoard.mu.RUnlock()
	if !covBoard.cellInPlannable(cell) {
		t.Fatalf("default GlusterFS profile excluded dual-path variant: %+v", cell)
	}
}

func TestCellRoleReadlinkSingle(t *testing.T) {
	glusterfsCoverageConfig(t, true)
	cell := glusterfsCoverageCell("symlink", "readlink", PathSame, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	if cell.Role != cellRoleSymlinkLinkPath {
		t.Fatalf("readlink role = %v, want symlink-linkpath", cell.Role)
	}
	covBoard.mu.RLock()
	if !covBoard.cellInPlannable(cell) {
		t.Fatalf("readlink HB cell rejected: %+v", cell)
	}
	cc := cell
	cc.Temporal = TemporalConcurrent
	if covBoard.cellInPlannable(cc) {
		t.Fatalf("readlink CC cell accepted: %+v", cc)
	}
	covBoard.mu.RUnlock()
}

func TestCellRoleRejectsLinkpathOnOrdinaryRoot(t *testing.T) {
	glusterfsCoverageConfig(t, true)
	cell := glusterfsCoverageCell("write", "stat", PathSibling, TemporalHB,
		RetSuccess, offsetBucketNA, false)
	cell.Role = cellRoleLinkPath
	covBoard.mu.RLock()
	if covBoard.cellInModel(cell) || covBoard.cellInPlannable(cell) {
		t.Fatalf("ordinary-root cell accepted linkpath role: %+v", cell)
	}
	cell.Role = cellRoleNone
	if !covBoard.cellInPlannable(cell) {
		t.Fatalf("ordinary-root default cell rejected: %+v", cell)
	}
	covBoard.mu.RUnlock()
}

func TestForEachUncoveredCellRoleLinkpathHBOnly(t *testing.T) {
	glusterfsCoverageConfig(t, true)
	ck := comboKey{Root: "link", Variant: "stat", Rel: PathSibling}
	var linkPathCells int
	covBoard.mu.RLock()
	covBoard.forEachUncoveredCellKey(ck, func(cell cellKey) {
		if cell.Role == cellRoleLinkPath {
			linkPathCells++
			if cell.Temporal != TemporalHB {
				t.Fatalf("linkpath role enumerated non-HB cell: %+v", cell)
			}
		}
	})
	covBoard.mu.RUnlock()
	if linkPathCells == 0 {
		t.Fatal("linkpath role produced no HB cells")
	}
}
