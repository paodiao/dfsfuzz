package prog

import (
	"math/rand"
	"testing"
)

func TestCausalRootRelPathsForDualRenameTemporal(t *testing.T) {
	dual := *glusterfsProfile
	legacy := *hmdfsProfile

	pre, post := causalRootRelPathsFor(&dual, TemporalConcurrent, causalRootRename,
		"rename", "dirA/b", "dirA/a", "dirA/b", "")
	if pre != "dirA/a" || post != "dirA/a" {
		t.Fatalf("dual CC rename A = (%q,%q), want source", pre, post)
	}
	pre, post = causalRootRelPathsFor(&dual, TemporalHB, causalRootRename,
		"rename", "dirA/b", "dirA/a", "dirA/b", "")
	if pre != "dirA/b" || post != "dirA/b" {
		t.Fatalf("dual HB rename A = (%q,%q), want destination", pre, post)
	}
	pre, post = causalRootRelPathsFor(&legacy, TemporalConcurrent, causalRootRename,
		"rename", "merge_view/dirA/b", "merge_view/dirA/a", "merge_view/dirA/b", "")
	if pre != "merge_view/dirA/b" || post != "merge_view/dirA/b" {
		t.Fatalf("legacy CC rename A = (%q,%q), want historical destination", pre, post)
	}
}

func TestCausalRenameRootC2TemporalRelations(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	r := newRand(target, rand.NewSource(301))
	r.hmcfg = hmcfg

	byCell, cells := planCausalC2CellsWithParent(nil, lcs, hmcfg,
		"rename", "dirA/b", "dirA", AnchorPathUnknown, false, false,
		"dirA/a", "dirA/b", "", true, causalRootRename, r)

	var ccSameSource, hbSameDest bool
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil || p.Cell.Combo.Variant != "stat" {
			continue
		}
		if p.Cell.Temporal == TemporalHB && p.BaseB == "dirA/a" {
			t.Fatalf("HB stat on the rename source survived: %+v", p)
		}
		if p.Cell.Temporal == TemporalConcurrent && p.BaseB == "dirA/a" &&
			p.Cell.Combo.Rel == PathSame {
			ccSameSource = true
		}
		if p.Cell.Temporal == TemporalHB && p.BaseB == "dirA/b" &&
			p.Cell.Combo.Rel == PathSame {
			hbSameDest = true
		}
	}
	if !ccSameSource {
		t.Fatal("CC rename root did not plan Same on the source")
	}
	if !hbSameDest {
		t.Fatal("HB rename root did not plan Same on the destination")
	}
}

func TestCausalUnlinkAnchorFiltersRemovedPath(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ps := glusterfsCoverageBaseProgs(target)

	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{unlink}, ps[0].Calls...)

	r := newRand(target, rand.NewSource(302))
	r.hmcfg = hmcfg
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	if len(plans) == 0 {
		t.Fatal("no C1 plans for a successful unlink anchor")
	}
	safe := false
	for _, p := range plans {
		if p.Anchor.Path != "dirA/a" {
			continue
		}
		if p.RootPath == "dirA/a" || pathUnderRoot(p.RootPath, "dirA/a") {
			t.Fatalf("root path on the unlinked path survived: %+v", p)
		}
		if p.RootPath == "dirA/b" || p.RootParent == "dirA" {
			safe = true
		}
	}
	if !safe {
		t.Fatal("unlink anchor lost safe parent/sibling roots")
	}
}

func TestCausalRmdirAnchorFiltersRemovedPath(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ps := glusterfsCoverageBaseProgs(target)
	hmcfg.FileTree.AddNode("dirA/empty", NodeTypeEmptyDir, hmdfsSmokeCids()[0])

	rmdir := mkRmdirCallForTest(t, target, "dirA/empty")
	rmdir.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{rmdir}, ps[0].Calls...)

	r := newRand(target, rand.NewSource(303))
	r.hmcfg = hmcfg
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	if len(plans) == 0 {
		t.Fatal("no C1 plans for a successful rmdir anchor")
	}
	for _, p := range plans {
		if p.Anchor.Path != "dirA/empty" {
			continue
		}
		if p.RootPath == "dirA/empty" || pathUnderRoot(p.RootPath, "dirA/empty") {
			t.Fatalf("root path on the removed directory survived: %+v", p)
		}
	}
}

func TestCausalRenameAnchorFiltersSourceSubtree(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ps := glusterfsCoverageBaseProgs(target)

	rename := mkRenameCall(t, target, "dirA/a", "dirA/b", 100, 200)
	ps[0].Calls = append([]*Call{rename}, ps[0].Calls...)

	r := newRand(target, rand.NewSource(304))
	r.hmcfg = hmcfg
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	if len(plans) == 0 {
		t.Fatal("no C1 plans for a successful rename anchor")
	}
	safe := false
	for _, p := range plans {
		if p.Anchor.Path != "dirA/b" {
			continue
		}
		if p.RootPath == "dirA/a" || pathUnderRoot(p.RootPath, "dirA/a") {
			t.Fatalf("root path in the rename source subtree survived: %+v", p)
		}
		if p.RootPath == "dirA/b" || p.RootParent == "dirA" {
			safe = true
		}
	}
	if !safe {
		t.Fatal("rename anchor lost safe destination/parent roots")
	}
}

func TestCausalFailedAnchorRejected(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ps := glusterfsCoverageBaseProgs(target)

	unlink := mkUnlinkCall(t, target, "dirB/c")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: -1}
	ps[0].Calls = append([]*Call{unlink}, ps[0].Calls...)

	r := newRand(target, rand.NewSource(305))
	r.hmcfg = hmcfg
	for _, p := range collectCausalRootPlans(ps[0], lcs, hmcfg, r) {
		if p.Anchor.Path == "dirB/c" {
			t.Fatalf("failed anchor produced a C1 plan: %+v", p)
		}
	}
}

func TestPickCausalRootBatchFiltersAnchorRemovedPath(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := glusterfsCoverageConfig(t, true)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	ps := glusterfsCoverageBaseProgs(target)

	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{unlink}, ps[0].Calls...)

	r := newRand(target, rand.NewSource(306))
	r.hmcfg = hmcfg
	batch, ok := pickCausalRootBatch(ps, lcs, hmcfg, r)
	if !ok {
		t.Fatal("no constructible causal batch for a successful unlink anchor")
	}
	for _, p := range batch.Plans {
		if p.Cell.Combo.Variant == "creat" && p.BaseB == "dirA/a" {
			continue // creat may recreate the exact removed path
		}
		if p.BaseB == "dirA/a" || pathUnderRoot(p.BaseB, "dirA/a") {
			t.Fatalf("C2 plan on the unlinked path: %+v", p)
		}
		if p.Cell.Combo.Variant == "rename" &&
			(p.BaseB2 == "dirA/a" || pathUnderRoot(p.BaseB2, "dirA/a")) {
			t.Fatalf("rename variant source on the unlinked path: %+v", p)
		}
	}
}

func TestBatchRootReservationsOK(t *testing.T) {
	batch := &directedBatch{
		BaseA:  "dirA/src",
		BaseA2: "dirA/dst",
		Plans: []directedPlan{{
			Cell:          directedCell{Combo: comboKey{Variant: "rename"}},
			BaseB:         "dirA/dst",
			RenameReplace: true,
		}},
	}
	if batchRootReservationsOK(batch) {
		t.Fatal("E destination on the root destination accepted")
	}
	batch.Plans[0].BaseB = "dirA/other"
	if !batchRootReservationsOK(batch) {
		t.Fatal("safe E destination rejected")
	}
	batch.Plans = append(batch.Plans, directedPlan{
		Cell:          directedCell{Combo: comboKey{Variant: "rename"}},
		BaseB:         "dirA/other",
		RenameReplace: true,
	})
	if batchRootReservationsOK(batch) {
		t.Fatal("duplicate E destination accepted")
	}
}
