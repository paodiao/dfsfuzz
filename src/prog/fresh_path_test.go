package prog

import (
	"math/rand"
	"strings"
	"testing"
)

func TestDirectedFreshNameTopLevel(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	r := newRand(target, rand.NewSource(401))

	name := directedFreshName("mkdir", "", r.Rand)
	if strings.HasPrefix(name, "/") || strings.Contains(name, "/") {
		t.Fatalf("top-level fresh name = %q, want a bare name", name)
	}
	placeholder := directedFreshPlaceholder("mkdir", "")
	if strings.HasPrefix(placeholder, "/") || strings.Contains(placeholder, "/") {
		t.Fatalf("top-level fresh placeholder = %q, want a bare name", placeholder)
	}

	nested := directedFreshName("creat", "dirA", r.Rand)
	if !strings.HasPrefix(nested, "dirA/") {
		t.Fatalf("nested fresh name = %q, want dirA/ prefix", nested)
	}
	nestedPlaceholder := directedFreshPlaceholder("creat", "dirA")
	if !strings.HasPrefix(nestedPlaceholder, "dirA/") {
		t.Fatalf("nested fresh placeholder = %q, want dirA/ prefix", nestedPlaceholder)
	}
}

func TestFreshParentEligible(t *testing.T) {
	glusterfs := NewFileTreeWithPrefix("")
	glusterfs.AddNode("dirA/a", NodeTypeFile, "")
	if !freshParentEligible(glusterfs, "") {
		t.Fatal("GlusterFS namespace root rejected")
	}
	if !freshParentEligible(glusterfs, "dirA") {
		t.Fatal("GlusterFS directory parent rejected")
	}
	if freshParentEligible(glusterfs, "dirA/a") {
		t.Fatal("GlusterFS file parent accepted")
	}
	if freshParentEligible(nil, "") {
		t.Fatal("nil tree accepted")
	}

	hmdfs := NewFileTree()
	if freshParentEligible(hmdfs, "") {
		t.Fatal("HMDFS empty parent accepted")
	}
	hmdfs.AddNode("merge_view/dirA", NodeTypeDir, "")
	if !freshParentEligible(hmdfs, "merge_view/dirA") {
		t.Fatal("HMDFS directory parent rejected")
	}
}

func TestCausalTopLevelFreshRoot(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ps := glusterfsCoverageBaseProgs(target)

	anchor := mkMkdir(target, "newTop")
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)

	r := newRand(target, rand.NewSource(402))
	r.hmcfg = hmcfg
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	if len(plans) == 0 {
		t.Fatal("top-level fresh anchor produced no C1 plans")
	}
	found := false
	for _, p := range plans {
		if p.RootKind != causalRootFresh || p.RootParent != "" {
			continue
		}
		if strings.Contains(p.RootPath, "/") {
			t.Fatalf("top-level fresh root has a slash: %+v", p)
		}
		found = true
	}
	if !found {
		t.Fatal("no top-level fresh root plan (RootParent empty)")
	}
}

func TestCausalExistingTopLevelFreshRoot(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	cid := hmdfsSmokeCids()[0]
	hmcfg.FileTree.AddNode("topFile", NodeTypeFile, cid)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ps := glusterfsCoverageBaseProgs(target)

	anchor := mkTruncate(target, "topFile", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)

	r := newRand(target, rand.NewSource(405))
	r.hmcfg = hmcfg
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	if len(plans) == 0 {
		t.Fatal("top-level existing anchor produced no C1 plans")
	}
	found := false
	for _, p := range plans {
		if p.RootKind != causalRootFresh || p.RootParent != "" {
			continue
		}
		if strings.Contains(p.RootPath, "/") {
			t.Fatalf("top-level fresh root has a slash: %+v", p)
		}
		found = true
	}
	if !found {
		t.Fatal("no top-level fresh root plan from an existing anchor")
	}
}

func TestCausalC2TopLevelFreshRoot(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(403))
	r.hmcfg = hmcfg

	byCell, cells := planCausalC2CellsWithParent(nil, lcs, hmcfg,
		"mkdir", "mut_dir_", "", AnchorPathUnknown, false, false,
		"", "", "", false, causalRootFresh, r)
	if len(cells) == 0 {
		t.Fatal("top-level fresh C2 root produced no plans")
	}
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil {
			continue
		}
		if p.BaseA != "mut_dir_" || p.BaseA2 != "" {
			t.Fatalf("top-level fresh C2 plan = %+v, want BaseA mut_dir_ and empty BaseA2", p)
		}
	}
}

func TestValidateTopLevelFreshPlan(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)

	plan := &directedPlan{
		Cell: directedCell{
			Combo: comboKey{Root: "mkdir", Variant: "stat", Rel: PathSibling},
			RetB:  RetSuccess, OffB: offsetBucketNA,
			DepthB: DepthBucketOf("dirA"), TypeB: true,
		},
		BaseA:  "mut_dir_x",
		BaseA2: "",
		BaseB:  "dirA",
		FreshA: true,
		Causal: true,
	}
	if !validateCausalPlan(hmcfg, lcs, plan) {
		t.Fatal("top-level fresh causal plan rejected")
	}
	if !validateDirectedPlanLive(hmcfg, lcs, plan) {
		t.Fatal("top-level fresh directed plan rejected")
	}

	same := &directedPlan{
		Cell: directedCell{
			Combo: comboKey{Root: "mkdir", Variant: "mkdir", Rel: PathSame},
			RetB:  RetEEXIST, OffB: offsetBucketNA,
			DepthB: DepthBucketOf("mut_dir_x"), TypeB: true,
		},
		BaseA:     "mut_dir_x",
		BaseA2:    "",
		BaseB:     "mut_dir_x",
		FreshA:    true,
		FreshSame: true,
		Causal:    true,
	}
	if !validateCausalPlan(hmcfg, lcs, same) {
		t.Fatal("top-level FreshSame causal plan rejected")
	}
	if !validateDirectedPlanLive(hmcfg, lcs, same) {
		t.Fatal("top-level FreshSame directed plan rejected")
	}
}

func TestPathSetTopLevelParent(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	ps := glusterfsCoverageBaseProgs(target)
	pos := GroupPosition{ProgIdx: 0, CallIdx: 0}

	if !pathSetParentValid(ps, pos, "mut_dir_x", hmcfg.FileTree) {
		t.Fatal("top-level fresh path parent rejected")
	}
	if !pathSetParentValid(ps, pos, "dirA/a", hmcfg.FileTree) {
		t.Fatal("existing directory parent rejected")
	}
	if pathSetParentValid(ps, pos, "dirA/a/x", hmcfg.FileTree) {
		t.Fatal("file parent accepted")
	}
}

func TestHMDFSTopLevelFreshRejected(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	if freshParentEligible(lcs.FileTree, "") {
		t.Fatal("HMDFS empty parent accepted")
	}
	r := newRand(target, rand.NewSource(404))
	r.hmcfg = hmcfg
	byCell, cells := planCausalC2CellsWithParent(nil, lcs, hmcfg,
		"mkdir", "merge_view/mut_dir_", "", AnchorPathUnknown, false, false,
		"", "", "", false, causalRootFresh, r)
	if len(cells) != 0 || len(byCell) != 0 {
		t.Fatalf("HMDFS top-level fresh C2 plans = %d/%d, want none", len(cells), len(byCell))
	}
}
