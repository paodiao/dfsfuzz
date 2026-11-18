package prog

import (
	"math/rand"
	"testing"
)

func TestGlusterfsPlanProfileLists(t *testing.T) {
	profile := glusterfsProfile
	for _, want := range []string{"link", "symlink", "setxattr"} {
		found := false
		for _, call := range profile.DirectedRootCalls() {
			if call == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("directed roots missing %q", want)
		}
	}
	for _, forbidden := range []string{"readlink", "getxattr"} {
		for _, call := range profile.DirectedRootCalls() {
			if call == forbidden {
				t.Fatalf("directed roots must not contain %q", forbidden)
			}
		}
	}
	for _, want := range []string{"link", "symlink", "readlink", "getxattr", "setxattr"} {
		found := false
		for _, call := range profile.DirectedVariantCalls() {
			if call == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("directed variants missing %q", want)
		}
	}
	if !profile.DualPathCausalEnabled() {
		t.Fatal("GlusterFS must enable causal dual-path insertion")
	}
}

func TestGlusterfsDirectedVariantKinds(t *testing.T) {
	cases := []struct {
		call string
		want map[RetBucket]bool
	}{
		{"link", map[RetBucket]bool{RetSuccess: true}},
		{"symlink", map[RetBucket]bool{RetSuccess: true}},
		{"readlink", map[RetBucket]bool{RetSuccess: true}},
		{"getxattr", map[RetBucket]bool{RetSuccess: true, RetFailure: true}},
		{"setxattr", map[RetBucket]bool{RetSuccess: true, RetEEXIST: true}},
		{"rmdir", map[RetBucket]bool{RetSuccess: true, RetFailure: true}},
		{"unlink", map[RetBucket]bool{RetSuccess: true}},
	}
	for _, tc := range cases {
		kinds, ok := directedVariantKinds(tc.call, glusterfsProfile)
		if !ok {
			t.Fatalf("directedVariantKinds(%q) missing", tc.call)
		}
		seen := map[RetBucket]bool{}
		for _, k := range kinds {
			seen[k.retB] = true
		}
		if len(seen) != len(tc.want) {
			t.Fatalf("%q kinds = %v, want %v", tc.call, seen, tc.want)
		}
		for ret := range tc.want {
			if !seen[ret] {
				t.Fatalf("%q kinds missing %v: %v", tc.call, ret, seen)
			}
		}
	}
	if kinds, _ := directedVariantKinds("rmdir"); len(kinds) != 3 {
		t.Fatalf("HMDFS rmdir must keep ENOENT, got %v", kinds)
	}
}

func TestGlusterfsVariantKindAllowed(t *testing.T) {
	file := &directedBNode{directedNode: directedNode{isDir: false}}
	dir := &directedBNode{directedNode: directedNode{isDir: true, nonEmpty: true}}
	if !variantKindAllowed("link", RetSuccess, file, "open", PathSame) {
		t.Fatal("link Success on file must be allowed")
	}
	if variantKindAllowed("link", RetSuccess, dir, "open", PathSame) {
		t.Fatal("link Success on directory must be rejected")
	}
	if !variantKindAllowed("symlink", RetSuccess, dir, "open", PathSame) {
		t.Fatal("symlink Success on directory must be allowed")
	}
	if !variantKindAllowed("getxattr", RetFailure, file, "open", PathSame) {
		t.Fatal("getxattr ENODATA without a prior setxattr must be allowed")
	}
	if variantKindAllowed("getxattr", RetSuccess, file, "open", PathSame) {
		t.Fatal("getxattr Success without a prior setxattr must be rejected")
	}
	if !variantKindAllowed("getxattr", RetSuccess, file, "setxattr", PathSame) {
		t.Fatal("getxattr Success after same-path setxattr must be allowed")
	}
	if !variantKindAllowed("setxattr", RetEEXIST, file, "setxattr", PathSame) {
		t.Fatal("setxattr EEXIST after same-path setxattr must be allowed")
	}
	if variantKindAllowed("setxattr", RetEEXIST, file, "open", PathSame) {
		t.Fatal("setxattr EEXIST without a prior setxattr must be rejected")
	}
}

func glusterfsPlanTree() *FileTree {
	cid := hmdfsSmokeCids()[0]
	ft := NewFileTreeWithPrefix("")
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Profile: glusterfsProfile,
		Init_dir: map[string][]string{
			cid: {"dirA", "dirB"},
		},
		Init_file: map[string][]string{
			cid: {"dirA/a", "dirA/b", "dirB/c"},
		},
	})
	return ft
}

func TestCausalRootCallsSeqLinkSymlink(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	hmcfg := &Hmdfs_config{Profile: glusterfsProfile, Node_num: 3, Cids: hmdfsSmokeCids(),
		FileTree: glusterfsPlanTree()}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(31))
	r.hmcfg = hmcfg
	p := &Prog{Target: target}
	sc := hmdfsSmokeSpecialCalls(t, target)

	for _, tc := range []struct {
		root, source, dest string
	}{
		{"link", "dirA/a", "dirA/l1"},
		{"symlink", "dirA/a", "dirA/l2"},
	} {
		batch := &directedBatch{Root: tc.root, BaseA: tc.source,
			RootSource: tc.source, RootDest: tc.dest}
		calls := causalRootCallsSeq(r, sc, lcs, hmcfg, p, batch)
		if len(calls) != 1 || calls[0] == nil {
			t.Fatalf("%s calls = %v", tc.root, calls)
		}
		if got := extractPathFromCallByArgIdx(calls[0], 1); got != tc.dest {
			t.Fatalf("%s arg1 = %q, want linkpath %q", tc.root, got, tc.dest)
		}
		arg0 := extractPathFromCallByArgIdx(calls[0], 0)
		if tc.root == "link" {
			if arg0 != tc.source {
				t.Fatalf("link arg0 = %q, want target %q", arg0, tc.source)
			}
		} else {
			// The on-wire payload must be relative to the linkpath parent and
			// resolve back to the canonical target.
			if arg0 != "a" {
				t.Fatalf("symlink payload = %q, want a", arg0)
			}
			if got := symlinkCanonicalTarget(tc.dest, arg0); got != tc.source {
				t.Fatalf("symlink payload resolves to %q, want %q", got, tc.source)
			}
		}
	}
}

func TestValidateCausalPlanReadlink(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	hmcfg := &Hmdfs_config{Profile: glusterfsProfile, Node_num: 3, Cids: hmdfsSmokeCids(),
		FileTree: glusterfsPlanTree()}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	linkPath := "dirA/l2"
	rel, ok := causalPairRelation("dirA/a", linkPath, linkPath, linkPath)
	if !ok {
		t.Fatal("readlink relation not constructible")
	}
	plan := &directedPlan{
		Cell: directedCell{
			Combo:    comboKey{Root: "symlink", Variant: "readlink", Rel: rel},
			Temporal: TemporalHB,
			RetB:     RetSuccess,
			OffB:     offsetBucketNA,
			PersistB: false,
			DepthB:   DepthBucketOf(linkPath),
			TypeB:    false,
			Role:     cellRoleSymlinkLinkPath,
		},
		BaseA:            "dirA/a",
		BaseB:            linkPath,
		RootLinkPath:     linkPath,
		RootRelationPath: linkPath,
		Causal:           true,
	}
	if !validateCausalPlan(hmcfg, lcs, plan) {
		t.Fatal("valid readlink C2 plan rejected")
	}
	bad := *plan
	bad.BaseB = "dirA/other"
	if validateCausalPlan(hmcfg, lcs, &bad) {
		t.Fatal("readlink plan with a non-linkpath B accepted")
	}
	badRoot := *plan
	badRoot.Cell.Combo.Root = "open"
	if validateCausalPlan(hmcfg, lcs, &badRoot) {
		t.Fatal("readlink plan under a non-symlink root accepted")
	}
}

func TestValidateCausalPlanLinkRootTargetOnly(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := &Hmdfs_config{Profile: glusterfsProfile, DfsName: "glusterfs", Node_num: 3,
		Cids: hmdfsSmokeCids(), FileTree: glusterfsPlanTree()}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	const (
		targetPath = "dirA/a"
		sibling    = "dirB/c"
		linkPath   = "dirB/l1"
	)
	node := hmcfg.FileTree.FindNode(sibling)
	if node == nil {
		t.Fatalf("sibling %q missing", sibling)
	}
	plan := &directedPlan{
		Cell: directedCell{
			Combo:    comboKey{Root: "link", Variant: "stat", Rel: PathSibling},
			RetB:     RetSuccess,
			OffB:     offsetBucketNA,
			PersistB: pathInDir(sibling, hmcfg.Persistence_dir),
			DepthB:   DepthBucketOf(sibling),
			TypeB:    node.Type != NodeTypeFile,
		},
		BaseA:        targetPath,
		BaseB:        sibling,
		RootLinkPath: linkPath,
		Causal:       true,
	}
	if validateCausalPlan(hmcfg, lcs, plan) {
		t.Fatalf("link root relation through linkpath accepted: %+v", plan)
	}
}

func TestValidateCausalPlanLinkpathRole(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := &Hmdfs_config{Profile: glusterfsProfile, DfsName: "glusterfs", Node_num: 3,
		Cids: hmdfsSmokeCids(), FileTree: glusterfsPlanTree()}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	const (
		targetPath = "dirA/a"
		linkPath   = "dirA/l1"
	)
	node := hmcfg.FileTree.FindNode(targetPath)
	if node == nil {
		t.Fatalf("target %q missing", targetPath)
	}
	plan := &directedPlan{
		Cell: directedCell{
			Combo:    comboKey{Root: "link", Variant: "stat", Rel: PathSibling},
			RetB:     RetSuccess,
			OffB:     offsetBucketNA,
			PersistB: pathInDir(targetPath, hmcfg.Persistence_dir),
			DepthB:   DepthBucketOf(targetPath),
			TypeB:    node.Type != NodeTypeFile,
			Role:     cellRoleLinkPath,
		},
		BaseA:            targetPath,
		BaseB:            targetPath,
		RootLinkPath:     linkPath,
		RootRelationPath: linkPath,
		Causal:           true,
	}
	if !validateCausalPlan(hmcfg, lcs, plan) {
		t.Fatalf("linkpath-role plan rejected: %+v", plan)
	}
	ccPlan := *plan
	ccPlan.Cell.Temporal = TemporalConcurrent
	if validateCausalPlan(hmcfg, lcs, &ccPlan) {
		t.Fatalf("linkpath-role CC plan accepted: %+v", ccPlan)
	}
}

// TestCausalC2LinkpathRoleHBOnly pins the observation rule: a link/symlink
// root is observable at the program-local linkpath only in HB pairs. The C2
// planner must not enqueue linkpath-role CC cells that pair extraction can
// never report.
func TestCausalC2LinkpathRoleHBOnly(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	const (
		targetPath = "dirA"
		linkPath   = "dirB/l1"
	)
	r := newRand(target, rand.NewSource(97))
	r.hmcfg = hmcfg
	byCell, cells := planCausalC2CellsWithParent(nil, lcs, hmcfg,
		"symlink", targetPath, "", AnchorPathUnknown, false, false,
		targetPath, linkPath, linkPath, false, causalRootNormal, r)
	if len(cells) == 0 {
		t.Fatal("linkpath-role C2 enumeration produced no plans")
	}
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil {
			continue
		}
		if p.RootRelationPath != linkPath {
			t.Fatalf("linkpath-role plan relation path = %q, want %q",
				p.RootRelationPath, linkPath)
		}
		if p.Cell.Temporal != TemporalHB {
			t.Fatalf("linkpath-role plan has non-HB temporal: %+v", p)
		}
		if p.Cell.Role != cellRoleSymlinkLinkPath {
			t.Fatalf("linkpath-role plan cell role = %v, want symlink-linkpath: %+v",
				p.Cell.Role, p)
		}
	}

	r = newRand(target, rand.NewSource(98))
	r.hmcfg = hmcfg
	byCell, cells = planCausalC2CellsWithParent(nil, lcs, hmcfg,
		"symlink", targetPath, "", AnchorPathUnknown, false, false,
		targetPath, linkPath, targetPath, false, causalRootNormal, r)
	sawCC := false
	for _, ck := range cells {
		p := byCell[ck]
		if p != nil && p.Cell.Temporal == TemporalConcurrent {
			sawCC = true
			break
		}
	}
	if !sawCC {
		t.Fatal("target-role C2 enumeration produced no CC plan")
	}
}

func TestGlusterfsCollectCausalDualRoots(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	gp := *glusterfsProfile
	gp.Plan.DualPathCausalEnabled = true
	ft := glusterfsPlanTree()
	hmcfg := &Hmdfs_config{Profile: &gp, DfsName: "glusterfs", Node_num: 3,
		Cids: hmdfsSmokeCids(), FileTree: ft}
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ResetCoverageBoard()
	InitCoverageBoard(hmcfg)
	t.Cleanup(ResetCoverageBoard)

	anchor := mkTruncate(target, "dirA/a", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	p0 := &Prog{Target: target, Calls: []*Call{anchor}}
	plans := collectCausalRootPlans(p0, lcs, hmcfg, newRand(target, rand.NewSource(51)))
	for _, p := range plans {
		if p.RootCall != "link" && p.RootCall != "symlink" {
			continue
		}
		if p.RootRelationPath != "" && p.RootRelationPath != p.RootPath {
			t.Fatalf("C1 %s linkpath-role plan generated: %+v", p.RootCall, p)
		}
	}
	seen := map[string]*causalRootPlan{}
	for _, p := range plans {
		if p.RootCall == "link" || p.RootCall == "symlink" {
			seen[p.RootCall] = p
		}
	}
	for _, call := range []string{"link", "symlink"} {
		p := seen[call]
		if p == nil {
			t.Fatalf("no C1 %s plan from a timed truncate anchor", call)
		}
		if p.RootSource != p.RootPath || p.RootDest == "" ||
			ft.FindNode(p.RootDest) != nil {
			t.Fatalf("bad C1 %s dual paths: %+v", call, p)
		}
		if call == "link" && p.Cell.TypeB != false {
			t.Fatalf("link target type = %v, want file", p.Cell.TypeB)
		}
		relationPath := p.RootRelationPath
		if relationPath == "" {
			relationPath = p.RootPath
		}
		wantRel, ok := causalPairRelation(p.Anchor.Source, p.Anchor.Path,
			relationPath, relationPath)
		if !ok || p.Anchor.Relation != wantRel {
			t.Fatalf("%s C1 relation = %v, relation path %q gives %v (ok=%v)",
				call, p.Anchor.Relation, relationPath, wantRel, ok)
		}
	}
}

func TestGlusterfsDirectedDualRootGate(t *testing.T) {
	if !directedVariantAllowed(glusterfsProfile, "link", "open") {
		t.Fatal("GlusterFS must plan a link root with dual-path causal enabled")
	}
	if !directedVariantAllowed(glusterfsProfile, "symlink", "open") {
		t.Fatal("GlusterFS must plan a symlink root with dual-path causal enabled")
	}
	gp := *glusterfsProfile
	gp.Plan.DualPathCausalEnabled = false
	if directedVariantAllowed(&gp, "link", "open") {
		t.Fatal("disabled dual-path profile must not plan a link root")
	}
	if directedVariantAllowed(&gp, "symlink", "open") {
		t.Fatal("disabled dual-path profile must not plan a symlink root")
	}
	if directedVariantAllowed(&gp, "open", "readlink") {
		t.Fatal("readlink must only follow a symlink root")
	}
	if variantKindAllowed("readlink", RetSuccess, &directedBNode{directedNode: directedNode{isDir: true}}, "symlink", PathSame) {
		t.Fatal("readlink B side must not be a directory")
	}
}

func TestDctBaseCandidatesRenameProfile(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	src, dst := "dirA/a", "dirA/moved"
	okCall := mkRenameCall(t, target, src, dst, 100, 200)
	okCall.CheckInfo.Retv = 0
	p := &Prog{Target: target, Calls: []*Call{okCall}}

	if got := dctBaseCandidatesFor(glusterfsProfile, p, 1); len(got) != 1 || got[0] != dst {
		t.Fatalf("glusterfs rename-before candidates = %v, want [%s]", got, dst)
	}
	if got := dctBaseCandidatesFor(glusterfsProfile, p, 0); len(got) != 1 || got[0] != src {
		t.Fatalf("glusterfs rename-after candidates = %v, want [%s]", got, src)
	}
	if got := dctBaseCandidatesFor(hmdfsProfile, p, 1); len(got) != 2 {
		t.Fatalf("hmdfs rename candidates = %v, want src+dst", got)
	}

	failCall := mkRenameCall(t, target, src, dst, 100, 200)
	failCall.CheckInfo.Retv = -17
	p = &Prog{Target: target, Calls: []*Call{failCall}}
	if got := dctBaseCandidatesFor(glusterfsProfile, p, 1); len(got) != 1 || got[0] != src {
		t.Fatalf("failed rename candidates = %v, want [%s]", got, src)
	}
}

func TestFreshPathKindsBeforeGlusterfsFailure(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	ft := glusterfsPlanTree()
	creat := mkCreat(target, "dirA/fresh")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	unlink := mkUnlinkCall(t, target, "dirA/fresh")
	unlink.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: -17}
	p := &Prog{Target: target, Calls: []*Call{creat, unlink}}
	if got := freshPathKindsBefore(p, ft, 2, glusterfsProfile); got["dirA/fresh"] != relTargetFile {
		t.Fatalf("failed unlink removed the fresh hint: %v", got)
	}
	unlink.CheckInfo.Retv = 0
	if _, ok := freshPathKindsBefore(p, ft, 2, glusterfsProfile)["dirA/fresh"]; ok {
		t.Fatal("successful unlink left the fresh hint")
	}
	if got := freshPathKindsBefore(p, ft, 2, hmdfsProfile); got["dirA/fresh"] != relTargetFile {
		t.Fatal("HMDFS unlink must keep the historical hint behavior")
	}
}

func TestGenerateSetxattrCallFull(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["setxattr"]; !ok {
		t.Skip("target has no setxattr")
	}
	sc := hmdfsSmokeSpecialCalls(t, target)
	s := newState(target, nil, nil)
	r := newRand(target, rand.NewSource(41))
	c := r.generateSetxattrCallFull(s, sc, "dirA/a", "user.eifuzz", "bar", 1)
	if c == nil {
		t.Fatal("setxattr generator returned nil")
	}
	if name := extractPathFromCallByArgIdx(c, 1); name != "user.eifuzz" {
		t.Fatalf("xattr name = %q", name)
	}
	flags, ok := c.Args[4].(*ConstArg)
	if !ok || flags.Val != 1 {
		t.Fatalf("xattr flags = %+v, want 1", c.Args[4])
	}
}
