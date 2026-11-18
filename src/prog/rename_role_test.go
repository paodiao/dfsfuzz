package prog

import (
	"math/rand"
	"testing"
)

func TestRenameCellRoleNone(t *testing.T) {
	glusterfsCoverageConfig(t, true)

	renameRoot := &DAGVertex{
		FuncID: FuncRename, CallName: "rename",
		Path: "dirA/b", OldPath: "dirA/a", RetBucket: RetSuccess,
	}
	linkVariant := &DAGVertex{
		FuncID: FuncLink, CallName: "link",
		Path: "dirA/l1", OldPath: "dirA/c", RetBucket: RetSuccess,
	}
	rootPair := DAGPair{
		A: renameRoot, B: linkVariant,
		APath: "dirA/b", BPath: "dirA/l1",
		PathRel: PathSameParent, Temporal: TemporalHB,
	}
	ck, ok := cellKeyOf(&rootPair, "")
	if !ok || ck.Role != cellRoleNone {
		t.Fatalf("rename root cell = %+v (ok=%v), want role none", ck, ok)
	}

	linkRoot := &DAGVertex{
		FuncID: FuncLink, CallName: "link",
		Path: "dirA/l1", OldPath: "dirA/a", RetBucket: RetSuccess,
	}
	renameVariant := &DAGVertex{
		FuncID: FuncRename, CallName: "rename",
		Path: "dirA/b", OldPath: "dirA/c", RetBucket: RetSuccess,
	}
	variantPair := DAGPair{
		A: linkRoot, B: renameVariant,
		APath: "dirA/l1", BPath: "dirA/c",
		PathRel: PathSameParent, Temporal: TemporalHB,
	}
	ck, ok = cellKeyOf(&variantPair, "")
	if !ok || ck.Role != cellRoleLinkPath {
		t.Fatalf("link-root rename-variant cell = %+v (ok=%v), want linkpath role", ck, ok)
	}
	covBoard.mu.RLock()
	if !covBoard.cellInPlannable(ck) {
		t.Fatalf("link-root rename-variant cell not plannable: %+v", ck)
	}
	covBoard.mu.RUnlock()
}

func TestCausalLinkAnchorRenameRootRoles(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(141))
	r.hmcfg = hmcfg
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	p0 := &Prog{Target: target, Calls: []*Call{link}}
	plans := collectCausalRootPlans(p0, lcs, hmcfg, r)

	var targetRole, linkPathRole bool
	for _, p := range plans {
		if p.Cell.Root != "link" || p.RootCall != "rename" {
			continue
		}
		if p.RootSource == "" || p.RootDest == "" {
			t.Fatalf("C1 link-anchor rename-root plan lost source/dest: %+v", p)
		}
		switch p.Cell.Role {
		case cellRoleNone:
			targetRole = true
		case cellRoleLinkPath:
			linkPathRole = true
			if p.Cell.Rel == PathChild || p.Cell.Rel == PathSame {
				t.Fatalf("C1 linkpath rename-root planned impossible relation: %+v", p)
			}
		}
	}
	if !targetRole {
		t.Fatal("C1 link anchor produced no target-role rename-root plan")
	}
	if !linkPathRole {
		t.Fatal("C1 link anchor produced no linkpath-role rename-root plan")
	}
}

func TestCausalC2LinkRootRenameVariantRoles(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(151))
	r.hmcfg = hmcfg
	const (
		targetPath = "dirA/a"
		linkPath   = "dirB/l1"
	)
	byCell, cells := planCausalC2CellsWithParent(nil, lcs, hmcfg,
		"link", targetPath, "", AnchorPathUnknown, false, false,
		targetPath, linkPath, linkPath, false, causalRootNormal, r)

	var linkPathRole, targetRole bool
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil || p.Cell.Combo.Variant != "rename" {
			continue
		}
		if p.BaseB2 == "" || p.BaseB == "" || p.BaseB == p.BaseB2 {
			t.Fatalf("C2 link-root rename variant lost source/dest: %+v", p)
		}
		switch p.Cell.Role {
		case cellRoleNone:
			targetRole = true
		case cellRoleLinkPath:
			linkPathRole = true
			if p.Cell.Combo.Rel == PathChild || p.Cell.Combo.Rel == PathSame {
				t.Fatalf("C2 linkpath rename variant planned impossible relation: %+v", p)
			}
		}
	}
	if !targetRole {
		t.Fatal("C2 link root produced no target-role rename variant")
	}
	if !linkPathRole {
		t.Fatal("C2 link root produced no linkpath-role rename variant")
	}
}

func TestCausalRenameAnchorLinkRootRoleNone(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(161))
	r.hmcfg = hmcfg
	rename := mkRenameCall(t, target, "dirA/a", "dirA/b", 100, 200)
	p0 := &Prog{Target: target, Calls: []*Call{rename}}
	plans := collectCausalRootPlans(p0, lcs, hmcfg, r)

	var seen bool
	for _, p := range plans {
		if p.Cell.Root != "rename" ||
			(p.RootCall != "link" && p.RootCall != "symlink") {
			continue
		}
		seen = true
		if p.Cell.Role != cellRoleNone {
			t.Fatalf("C1 rename anchor link root cell role = %v, want none: %+v",
				p.Cell.Role, p)
		}
		if p.RootSource == "" || p.RootDest == "" {
			t.Fatalf("C1 rename anchor link root lost source/dest: %+v", p)
		}
	}
	if !seen {
		t.Fatal("C1 rename anchor produced no link/symlink root plan")
	}
}
