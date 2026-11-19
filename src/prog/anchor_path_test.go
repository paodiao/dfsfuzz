package prog

import (
	"math/rand"
	"testing"
)

func TestLookupConstAndParseOpenFlags(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.LookupConst("O_CREAT"); !ok {
		t.Fatal("O_CREAT missing from smoke target")
	}
	if _, ok := target.LookupConst("__MONARCH_MISSING_CONST__"); ok {
		t.Fatal("missing constant unexpectedly found")
	}
	call := mkOpen(target, "merge_view/dirA/a")
	if flags, ok := parseOpenFlags(call); !ok || flags == 0 {
		t.Fatalf("parseOpenFlags = %#x, %v", flags, ok)
	}
}

func TestClassifyAnchorPathCreator(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	prog := &Prog{Target: target}
	mkdir := mkMkdir(target, "merge_view/dirA/created")
	mkdir.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	prog.Calls = append(prog.Calls, mkdir)
	info, ok := classifyAnchorPath(prog, lcs.FileTree, GroupPosition{ProgIdx: 0, CallIdx: 0},
		"merge_view/dirA/created", true)
	if !ok || info.Kind != AnchorPathFreshDir || !info.IsDir {
		t.Fatalf("mkdir classification = %+v, ok=%v", info, ok)
	}
	mod := mkTruncate(prog.Target, "merge_view/dirA/created", 1)
	mod.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	prog.Calls = append(prog.Calls, mod)
	info, ok = classifyAnchorPath(prog, lcs.FileTree, GroupPosition{ProgIdx: 0, CallIdx: 1},
		"merge_view/dirA/created", true)
	if !ok || info.Kind != AnchorPathFreshDir || info.CreatorIdx != 0 {
		t.Fatalf("modifier anchor creator classification = %+v, ok=%v", info, ok)
	}
}

func TestFindAnchorCreatorAcceptsOpenCreate(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	if _, ok := target.SyscallMap["open"]; !ok {
		t.Skip("target has no open")
	}
	oCreate, ok := target.LookupConst("O_CREAT")
	if !ok {
		t.Skip("target has no O_CREAT")
	}
	const path = "merge_view/dirA/fresh"

	open := mkOpen(target, path)
	flags, ok := open.Args[1].(*ConstArg)
	if !ok {
		t.Fatalf("open flags arg = %T, want *ConstArg", open.Args[1])
	}
	flags.Val |= oCreate
	open.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 3}
	p := &Prog{Target: target, Calls: []*Call{open}}
	kind, isDir, idx, ok := findAnchorCreator(p, lcs.FileTree, path, 0, true)
	if !ok || kind != AnchorPathFreshFile || isDir || idx != 0 {
		t.Fatalf("open(O_CREAT) creator = kind %v isDir %v idx %d ok %v",
			kind, isDir, idx, ok)
	}

	open.CheckInfo.Retv = -1
	if _, _, _, ok := findAnchorCreator(p, lcs.FileTree, path, 0, true); ok {
		t.Fatal("failed open(O_CREAT) accepted as a creator")
	}
}

func TestSeedDerivationTableAndPath(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	table := newCausalDerivationTable()
	if len(table.Weights) == 0 {
		t.Fatal("empty causal derivation table")
	}
	prog := &Prog{Target: target}
	call := mkTruncate(target, "merge_view/dirA/a", 1)
	prog.Calls = append(prog.Calls, call)
	anchors := collectSeedAnchors(prog, lcs.FileTree, false)
	if len(anchors) == 0 {
		t.Fatal("no seed anchors collected")
	}
	r := newRand(target, rand.NewSource(1))
	derivation, ok := sampleSeedDerivation(prog, lcs.FileTree, table, r, false)
	if !ok {
		t.Fatal("no seed derivation sampled")
	}
	if derivation.RootSource == "" {
		t.Fatalf("empty root source: %+v", derivation)
	}
	if derivation.RootCall == "rename" {
		if derivation.RootDest == "" || GetParentDir(derivation.RootDest) != GetParentDir(derivation.RootSource) {
			t.Fatalf("bad rename derivation: %+v", derivation)
		}
	}
}

func TestDerivedRenameSourceDest(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	anchor := seedAnchor{
		Pos:      GroupPosition{ProgIdx: 0, CallIdx: 0},
		Path:     "merge_view/dirA/a",
		IsDir:    false,
		PathKind: AnchorPathExisting,
	}
	r := newRand(target, rand.NewSource(2))
	d, ok := deriveSeedRootPath(lcs.FileTree, anchor, "rename", PathSame, r)
	if !ok {
		t.Fatal("rename derivation failed")
	}
	if d.RootSource != anchor.Path || d.RootDest == "" || d.RootDest == d.RootSource {
		t.Fatalf("bad rename derivation: %+v", d)
	}
	if GetParentDir(d.RootDest) != GetParentDir(d.RootSource) {
		t.Fatalf("rename dest not same-parent: %+v", d)
	}
	if lcs.FileTree.FindNode(d.RootDest) != nil {
		t.Fatalf("rename dest is not fresh: %+v", d)
	}
}

func TestInsertDerivedCallFromDCT(t *testing.T) {
	target, hmcfg, sc, lcs := seedOptLCS(t)
	ps := directedPlanBaseProgs(target)
	r := newRand(target, rand.NewSource(1))
	before := len(ps[0].Calls)
	if !insertDerivedCallFromDCT(ps, r, sc, hmcfg, lcs, false, true) {
		t.Fatal("derived insertion failed")
	}
	if len(ps[0].Calls) <= before {
		t.Fatal("derived root not inserted")
	}
	for _, p := range ps {
		if p == nil || p.HasBrokenRefs() {
			t.Fatal("derived insertion produced broken refs")
		}
	}
}

func TestSeedRenameAnchorRoles(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	prog := &Prog{Target: target}
	prog.Calls = append(prog.Calls, mkRenameCall(t, target,
		"merge_view/dirA/a", "merge_view/dirA/b", 100, 200))
	anchors := collectSeedAnchors(prog, lcs.FileTree, false)
	if len(anchors) != 2 {
		t.Fatalf("rename anchors = %d, want source+destination", len(anchors))
	}
	var src, dst seedAnchor
	for _, anchor := range anchors {
		switch anchor.Role {
		case seedAnchorRenameSrc:
			src = anchor
		case seedAnchorRenameDst:
			dst = anchor
		}
	}
	if src.Path != "merge_view/dirA/a" || dst.Path != "merge_view/dirA/b" {
		t.Fatalf("rename roles = src %+v dst %+v", src, dst)
	}
	if !seedRelationAllowed(src, "stat", PathParent) ||
		!seedRelationAllowed(src, "stat", PathSibling) ||
		!seedRelationAllowed(src, "stat", PathNoRel) {
		t.Fatalf("source safe relations rejected: %+v", src)
	}
	if seedRelationAllowed(src, "stat", PathSame) ||
		seedRelationAllowed(src, "stat", PathChild) ||
		seedRelationAllowed(src, "mkdir", PathChild) ||
		seedRelationAllowed(src, "rename", PathSame) ||
		seedRelationAllowed(src, "rename", PathChild) {
		t.Fatalf("source unsafe relation accepted: %+v", src)
	}
	if !seedRelationAllowed(dst, "stat", PathSame) ||
		!seedRelationAllowed(dst, "stat", PathChild) {
		t.Fatalf("destination relations were restricted: %+v", dst)
	}
}

func TestDeriveSeedRootPathRenameSource(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	src := seedAnchor{
		Pos:      GroupPosition{ProgIdx: 0, CallIdx: 0},
		CallName: "rename",
		Path:     "merge_view/dirA/a",
		IsDir:    false,
		PathKind: AnchorPathExisting,
		Role:     seedAnchorRenameSrc,
	}
	r := newRand(target, rand.NewSource(3))
	if _, ok := deriveSeedRootPath(lcs.FileTree, src, "stat", PathSame, r); ok {
		t.Fatal("source Same root accepted")
	}
	if _, ok := deriveSeedRootPath(lcs.FileTree, src, "stat", PathChild, r); ok {
		t.Fatal("source Child root accepted")
	}
	d, ok := deriveSeedRootPath(lcs.FileTree, src, "stat", PathParent, r)
	if !ok || d.RootSource != "merge_view/dirA" {
		t.Fatalf("source Parent derivation = %+v, ok=%v", d, ok)
	}
	if _, ok := deriveSeedRootPath(lcs.FileTree, src, "mkdir", PathChild, r); ok {
		t.Fatal("source fresh Child root accepted")
	}
	if _, ok := deriveSeedRootPath(lcs.FileTree, src, "rename", PathSame, r); ok {
		t.Fatal("source rename Same root accepted")
	}
	if _, ok := deriveSeedRootPath(lcs.FileTree, src, "rename", PathChild, r); ok {
		t.Fatal("source rename Child root accepted")
	}
}

func TestSeedFreshAnchorDepth(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	const (
		parentPath = "merge_view/dirA/newParent"
		nestedPath = "merge_view/dirA/newParent/newAnchor"
		onePath    = "merge_view/dirA/oneLevel"
	)
	parent := mkMkdir(target, parentPath)
	parent.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	nested := mkMkdir(target, nestedPath)
	nested.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	one := mkMkdir(target, onePath)
	one.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
	prog := &Prog{Target: target, Calls: []*Call{parent, nested, one}}
	anchors := collectSeedAnchors(prog, lcs.FileTree, false)
	seen := make(map[string]bool)
	for _, anchor := range anchors {
		seen[anchor.Path] = true
	}
	if !seen[onePath] {
		t.Fatal("one-level fresh anchor was rejected")
	}
	if seen[nestedPath] {
		t.Fatal("nested fresh anchor was accepted")
	}
	oneAnchor := seedAnchor{
		Path:     onePath,
		IsDir:    true,
		PathKind: AnchorPathFreshDir,
		Role:     seedAnchorNormal,
	}
	nestedAnchor := seedAnchor{
		Path:     nestedPath,
		IsDir:    true,
		PathKind: AnchorPathFreshDir,
		Role:     seedAnchorNormal,
	}
	r := newRand(target, rand.NewSource(4))
	if _, ok := deriveSeedRootPath(lcs.FileTree, nestedAnchor, "mkdir", PathChild, r); ok {
		t.Fatal("nested fresh parent produced a root plan")
	}
	if _, ok := deriveSeedRootPath(lcs.FileTree, oneAnchor, "mkdir", PathChild, r); !ok {
		t.Fatal("one-level fresh parent did not produce a root plan")
	}
}

func TestFreshAnchorRenameRootChild(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	anchor := seedAnchor{
		Path:     "merge_view/dirA/newAnchor",
		IsDir:    true,
		PathKind: AnchorPathFreshDir,
		Role:     seedAnchorNormal,
	}
	if seedRelationAllowed(anchor, "rename", PathChild) {
		t.Fatal("fresh anchor rename Child relation allowed")
	}
	if _, ok := deriveSeedRootPath(lcs.FileTree, anchor, "rename", PathChild,
		newRand(target, rand.NewSource(5))); ok {
		t.Fatal("fresh anchor rename Child root derived")
	}
}

func TestDerivedMkdirExistingVariant(t *testing.T) {
	cases := []struct {
		name     string
		anchor   string
		relation PathRelation
		wantPath string
	}{
		{"same", "merge_view/dirA", PathSame, "merge_view/dirA"},
		{"parent", "merge_view/dirA/a", PathParent, "merge_view/dirA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, hmcfg, sc, lcs := seedOptLCS(t)
			anchor := mkTruncate(target, tc.anchor, 1)
			anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
			ps := directedPlanBaseProgs(target)
			ps[0] = &Prog{Target: target, Calls: []*Call{anchor}}
			lcs.CausalDerivationTable.Weights["truncate"] = map[CallVariant]int{
				{CallName: "stat", PathRelation: PathSame}: 100,
			}
			lcs.GetDCT().Weights["stat"] = map[CallVariant]int{
				{CallName: "mkdir", PathRelation: tc.relation}: 100,
			}
			if !insertDerivedCallFromDCT(ps, newRand(target, rand.NewSource(6)),
				sc, hmcfg, lcs, false, true) {
				t.Fatal("derived insertion failed")
			}
			found := false
			for _, p := range ps[1:] {
				for _, call := range p.Calls {
					if call != nil && call.Meta != nil &&
						call.Meta.CallName == "mkdir" &&
						extractPathFromCall(call) == tc.wantPath {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("derived mkdir %s variant path %q not inserted", tc.name, tc.wantPath)
			}
		})
	}
}
