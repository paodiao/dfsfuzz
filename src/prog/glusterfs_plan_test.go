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
	if profile.DualPathCausalEnabled() {
		t.Fatal("GlusterFS v1 must keep causal dual-path insertion disabled")
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
		if got := extractPathFromCallByArgIdx(calls[0], 0); got != tc.source {
			t.Fatalf("%s arg0 = %q, want target %q", tc.root, got, tc.source)
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
		},
		BaseA:        "dirA/a",
		BaseB:        linkPath,
		RootLinkPath: linkPath,
		Causal:       true,
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
