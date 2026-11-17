package prog

import (
	"math/rand"
	"testing"
)

func causalRootFixture(t *testing.T) (*Target, *Hmdfs_config, *SpecialCalls, *LayeredChoiceStrategy, []*Prog) {
	t.Helper()
	target, hmcfg, sc, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkTruncate(target, "merge_view/dirA/a", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	return target, hmcfg, sc, lcs, ps
}

func TestCollectCausalRootPlansNeedsTimedModifier(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	if plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(1))); len(plans) != 0 {
		t.Fatalf("open/close p0 produced causal plans: %d", len(plans))
	}
	anchor := mkTruncate(target, "merge_view/dirA/a", 1)
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	if plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(1))); len(plans) != 0 {
		t.Fatalf("untimed anchor produced causal plans: %d", len(plans))
	}
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(1)))
	if len(plans) == 0 {
		t.Fatal("timed truncate anchor produced no causal plans")
	}
	for _, p := range plans {
		if p.Cell.Root != "truncate" || p.Cell.Temporal != TemporalHB || p.Cell.RetB != RetSuccess {
			t.Fatalf("bad C1 plan: %+v", p)
		}
		if p.Anchor.Path != "merge_view/dirA/a" || p.Anchor.InsertPos != 1 {
			t.Fatalf("bad anchor: %+v", p.Anchor)
		}
	}
}

func TestPickCausalRootBatchAndInsert(t *testing.T) {
	target, hmcfg, sc, lcs, ps := causalRootFixture(t)
	r := newRand(target, rand.NewSource(1))
	batch, ok := pickCausalRootBatch(ps, lcs, hmcfg, r)
	if !ok || batch == nil || batch.Anchor == nil {
		t.Fatalf("causal batch = %+v, ok=%v", batch, ok)
	}
	if batch.Anchor.InsertPos != 1 || batch.Anchor.Path != "merge_view/dirA/a" {
		t.Fatalf("unexpected anchor: %+v", batch.Anchor)
	}
	if len(batch.Plans) == 0 {
		t.Fatal("causal batch has no C2 plans")
	}
	for _, p := range batch.Plans {
		if p.BaseA != batch.BaseA || p.Cell.Combo.Root != batch.Root {
			t.Fatalf("C2 plan does not use fixed root: %+v batch=%+v", p, batch)
		}
	}
	if !insertDirectedBatch(ps, newRand(target, rand.NewSource(2)), sc, hmcfg, lcs, batch) {
		t.Fatal("causal batch insertion failed")
	}
	anchorIdx := 0
	if ps[0].Calls[anchorIdx].Meta.CallName != "truncate" {
		t.Fatalf("anchor moved: %s", ps[0].Calls[anchorIdx].Meta.CallName)
	}
	rootFound := false
	for i := anchorIdx + 1; i < len(ps[0].Calls); i++ {
		if ps[0].Calls[i] != nil && ps[0].Calls[i].Meta.CallName == batch.Root {
			rootFound = true
			break
		}
	}
	if !rootFound {
		t.Fatalf("root %s not inserted after anchor", batch.Root)
	}
	if len(lcs.PendingDirected) == 0 {
		t.Fatal("C2 pending was not recorded")
	}
}

func TestCausalRootFallsBackWithoutAnchor(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	if batch, ok := pickCausalRootBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(3))); ok || batch != nil {
		t.Fatalf("causal batch without anchor = %+v, ok=%v", batch, ok)
	}
	if _, status := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(3))); status != directedPlanFound {
		t.Fatalf("normal planner did not fall back with found status: %v", status)
	}
}

func TestCausalRootBatchDeterministic(t *testing.T) {
	target, hmcfg, _, lcs, ps := causalRootFixture(t)
	pick := func(seed int64) *directedBatch {
		b, ok := pickCausalRootBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if !ok {
			t.Fatal("causal plan missing")
		}
		return b
	}
	a := pick(11)
	b := pick(11)
	if a.Root != b.Root || a.BaseA != b.BaseA || a.Anchor.InsertPos != b.Anchor.InsertPos ||
		a.Anchor.RefTime != b.Anchor.RefTime || len(a.Plans) != len(b.Plans) {
		t.Fatalf("non-deterministic causal batch: %+v vs %+v", a, b)
	}
	for i := range a.Plans {
		if a.Plans[i].Cell != b.Plans[i].Cell {
			t.Fatalf("plan %d differs: %+v vs %+v", i, a.Plans[i], b.Plans[i])
		}
	}
}

func TestCausalRootOffsetArgsMatchCell(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkTruncate(target, "merge_view/dirA/a", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(4)))
	found := false
	for _, p := range plans {
		if p.RootCall != "pwrite64" {
			continue
		}
		found = true
		node := hmcfg.FileTree.FindNode(p.RootPath)
		if node == nil {
			t.Fatalf("offset root path missing: %+v", p)
		}
		if p.Cell.OffB == offsetBucketNA {
			t.Fatalf("pwrite C1 cell has NA offset: %+v", p)
		}
		if got := bucketOfOffset(node.Size, p.RootOffset); got != p.Cell.OffB {
			t.Fatalf("pwrite offset bucket = %d, cell OffB = %d (%+v)", got, p.Cell.OffB, p)
		}
		if p.RootLen == 0 {
			t.Fatalf("pwrite root length is zero: %+v", p)
		}
	}
	if !found {
		t.Fatal("no pwrite causal plan")
	}
}

func TestCausalRootFreshCandidates(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkTruncate(target, "merge_view/dirA/a", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(5)))
	found := false
	for _, p := range plans {
		if p.RootCall != "mkdir" && p.RootCall != "creat" {
			continue
		}
		found = true
		if p.RootKind != causalRootFresh || p.RootParent == "" {
			t.Fatalf("fresh root metadata missing: %+v", p)
		}
		if DepthBucketOf(p.RootPath) != p.Cell.DepthB {
			t.Fatalf("fresh root depth mismatch: %+v", p)
		}
	}
	if !found {
		t.Fatal("no fresh-root causal plan")
	}
}

func TestCausalRootFreshAnchorPlans(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkMkdir(target, "merge_view/dirA/new_anchor")
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(21)))
	if len(plans) == 0 {
		t.Fatal("fresh anchor produced no C1 plans")
	}
	sawFresh := false
	for _, p := range plans {
		if p.Anchor.PathKind == AnchorPathFreshDir {
			sawFresh = true
		}
	}
	if !sawFresh {
		t.Fatal("fresh anchor was not classified as freshDir")
	}
}

func TestCausalRootBothFreshChildPlan(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkMkdir(target, "merge_view/dirA/fresh_parent")
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(22)))
	found := false
	for _, p := range plans {
		if p.RootKind == causalRootFresh && p.RootParent == "merge_view/dirA/fresh_parent" &&
			p.RootParentIsAnchor && p.RootCall == "mkdir" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("both-fresh Child root plan was not produced")
	}
}

func TestCausalRootFreshC2AndInsert(t *testing.T) {
	target, hmcfg, sc, lcs, ps := causalRootFixture(t)
	r := newRand(target, rand.NewSource(6))
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	var fresh *causalRootPlan
	for _, p := range plans {
		if p.RootKind == causalRootFresh {
			fresh = p
			break
		}
	}
	if fresh == nil {
		t.Fatal("no fresh root plan")
	}
	fresh.RootPath = directedFreshName(fresh.RootCall, fresh.RootParent, r.Rand)
	byCell, cells := planCausalC2Cells(ps, lcs, hmcfg, fresh.RootCall, fresh.RootPath, fresh.RootParent, "", "", false, fresh.RootKind, r)
	if len(cells) == 0 {
		t.Fatal("fresh root produced no C2 plans")
	}
	sawFreshSame := false
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil || !p.FreshSame {
			continue
		}
		sawFreshSame = true
		if p.BaseA == "" || p.BaseA != p.BaseB || p.BaseB2 != "" ||
			p.BaseA2 != fresh.RootParent {
			t.Fatalf("bad fresh-fresh plan: %+v", p)
		}
	}
	if !sawFreshSame {
		t.Fatal("fresh root produced no fresh-fresh plan")
	}
	var batchPlans []directedPlan
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil {
			continue
		}
		q := *p
		q.NodeX = len(batchPlans) + 1
		batchPlans = append(batchPlans, q)
		if len(batchPlans) >= len(ps)-1 {
			break
		}
	}
	batchPlans = filterBaseBAuthority(batchPlans)
	for i := range batchPlans {
		batchPlans[i].NodeX = i + 1
		if !batchPlans[i].FreshA || batchPlans[i].BaseA != fresh.RootPath ||
			batchPlans[i].BaseA2 != fresh.RootParent {
			t.Fatalf("bad fresh C2 plan: %+v", batchPlans[i])
		}
	}
	batch := &directedBatch{
		Root:       fresh.RootCall,
		BaseA:      fresh.RootPath,
		BaseA2:     fresh.RootParent,
		RootKind:   fresh.RootKind,
		RootParent: fresh.RootParent,
		Anchor:     &fresh.Anchor,
		Plans:      batchPlans,
	}
	if !batchBaseBAuthorityOK(batch.Plans) {
		t.Fatalf("fresh batch authority invalid: %+v", batch.Plans)
	}
	if !insertDirectedBatch(ps, newRand(target, rand.NewSource(7)), sc, hmcfg, lcs, batch) {
		t.Fatal("fresh causal batch insertion failed")
	}
	if len(ps[0].Calls) < 2 || ps[0].Calls[1] == nil {
		t.Fatalf("fresh root not inserted after anchor: %d calls", len(ps[0].Calls))
	}
	if got := ps[0].Calls[1].Meta.CallName; got != fresh.RootCall {
		t.Fatalf("fresh root call = %s, want %s", got, fresh.RootCall)
	}
	if len(lcs.PendingDirected) == 0 {
		t.Fatal("fresh C2 pending not recorded")
	}
}

func TestCausalRootRenameC2Candidates(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkTruncate(target, "merge_view/dirA/a", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	r := newRand(target, rand.NewSource(8))
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	found := false
	for _, p := range plans {
		if p.RootKind != causalRootRename {
			continue
		}
		found = true
		if p.RootSource == "" || p.RootDest == "" {
			t.Fatalf("rename root missing source/dest: %+v", p)
		}
		if GetParentDir(p.RootSource) != GetParentDir(p.RootDest) {
			t.Fatalf("rename root not same-parent: %+v", p)
		}
		_, cells := planCausalC2Cells(ps, lcs, hmcfg, p.RootCall, p.RootPath,
			p.RootParent, p.RootSource, p.RootDest, p.RootRenameReplace, p.RootKind, r)
		if len(cells) == 0 {
			t.Fatalf("rename root produced no C2 plans: %+v", p)
		}
	}
	if !found {
		t.Fatal("no rename-root causal plan")
	}
}

func TestCausalAnchorRenameAndClosedFd(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	rename := mkRenameCall(t, target, "merge_view/dirA/a", "merge_view/dirA/renamed", 100, 200)
	ps[0].Calls = append([]*Call{rename}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(9)))
	found := false
	for _, p := range plans {
		if p.Anchor.Source != "merge_view/dirA/a" || p.Anchor.Dest != "merge_view/dirA/renamed" ||
			p.Anchor.Path != "merge_view/dirA/renamed" {
			continue
		}
		found = true
	}
	if !found {
		t.Fatalf("rename anchor not resolved to post path; plans=%d", len(plans))
	}

	open := mkOpen(target, "merge_view/dirA/a")
	open.CheckInfo = &FileMetadata{Stime: 10, Etime: 20}
	closeCall := mkClose(target, open)
	closeCall.CheckInfo = &FileMetadata{Stime: 30, Etime: 40}
	pw := mkPwrite64(target, open, 8, 0)
	pw.CheckInfo = &FileMetadata{Stime: 50, Etime: 60}
	ps[0].Calls = []*Call{open, closeCall, pw}
	if plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(10))); len(plans) != 0 {
		t.Fatalf("closed-fd anchor produced plans: %d", len(plans))
	}
}

func TestCausalRootC2RenameAndFreshBPlans(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	r := newRand(target, rand.NewSource(12))
	byCell, cells := planCausalC2Cells(ps, lcs, hmcfg, "stat", "merge_view/dirA/a", "", "", "", false, causalRootNormal, r)
	if len(cells) == 0 {
		t.Fatal("no C2 plans")
	}
	var sawFreshRename, sawFreshB bool
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil {
			continue
		}
		if p.FreshRenameDst {
			sawFreshRename = true
			if p.Cell.Combo.Variant != "rename" || p.BaseB != "" || p.BaseB2 == "" {
				t.Fatalf("bad fresh rename variant plan: %+v", p)
			}
		}
		if p.FreshB {
			sawFreshB = true
			if p.BaseB != "" || p.BaseB2 == "" {
				t.Fatalf("bad fresh B plan: %+v", p)
			}
		}
	}
	if !sawFreshRename || !sawFreshB {
		t.Fatalf("missing plans: freshRename=%v freshB=%v", sawFreshRename, sawFreshB)
	}
}

func TestCausalPairRelationUsesPostAndPreCandidates(t *testing.T) {
	const (
		rootSrc = "merge_view/dirA/a"
		rootDst = "merge_view/dirA/b"
		other   = "merge_view/dirA/c"
		other2  = "merge_view/dirA/d"
	)
	cases := []struct {
		name string
		rp   string
		ro   string
		vp   string
		vo   string
		want PathRelation
		ok   bool
	}{
		{"same pre", rootSrc, rootDst, rootSrc, other2, PathSame, true},
		{"same post only", rootSrc, rootDst, other, rootDst, PathSame, true},
		{"root pre equals variant post", rootSrc, rootDst, other, rootSrc, PathSame, true},
		{"root post equals variant pre", rootSrc, rootDst, rootDst, other2, PathSame, true},
		{"sibling via post", rootSrc, rootDst, other, other2, PathSibling, true},
		{"parent via post", rootSrc, rootDst, rootDst + "/child", other2, PathChild, true},
		{"unrelated", rootSrc, rootDst, "merge_view/dirB/c", "merge_view/dirB/d", PathNoRel, false},
	}
	for _, tc := range cases {
		got, ok := causalPairRelation(tc.rp, tc.ro, tc.vp, tc.vo)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("%s: got (%v,%v), want (%v,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCausalRootC2VariantRenameSameUsesSource(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	const root = "merge_view/dirA/a"
	byCell, cells := planCausalC2Cells(ps, lcs, hmcfg, "stat", root, "",
		"", "", false, causalRootNormal, newRand(target, rand.NewSource(13)))
	sawSourceSame := false
	for _, ck := range cells {
		p := byCell[ck]
		if p == nil || p.Cell.Combo.Variant != "rename" {
			continue
		}
		if p.BaseB2 == root {
			sawSourceSame = true
			if p.Cell.Combo.Rel != PathSame {
				t.Fatalf("rename source %q planned as %v, want Same: %+v",
					root, p.Cell.Combo.Rel, p)
			}
		}
	}
	if !sawSourceSame {
		t.Fatal("no rename variant shares the root source path")
	}
}

func TestCausalRootRenameC2Relations(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkTruncate(target, "merge_view/dirA/a", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	r := newRand(target, rand.NewSource(14))
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	tested := 0
	for _, p := range plans {
		if p.RootKind != causalRootRename {
			continue
		}
		byCell, cells := planCausalC2Cells(ps, lcs, hmcfg, p.RootCall, p.RootPath,
			p.RootParent, p.RootSource, p.RootDest, p.RootRenameReplace, p.RootKind, r)
		for _, ck := range cells {
			q := byCell[ck]
			if q == nil {
				continue
			}
			varPre, varPost := q.BaseB, q.BaseB
			if q.Cell.Combo.Variant == "rename" {
				varPre, varPost = q.BaseB2, q.BaseB2
			}
			if varPre == "" || varPost == "" {
				continue
			}
			// Rename root A is the destination; rename variant B is the source.
			rel, ok := causalPairRelation(p.RootDest, p.RootDest, varPre, varPost)
			if !ok {
				t.Fatalf("planned C2 plan has no observed relation: root=%+v plan=%+v", p, q)
			}
			if q.Cell.Combo.Rel != rel {
				t.Fatalf("C2 relation = %v, want %v: root=%+v plan=%+v",
					q.Cell.Combo.Rel, rel, p, q)
			}
		}
		tested++
	}
	if tested == 0 {
		t.Fatal("no rename-root C1 plan")
	}
}

func TestValidateCausalPlanRenameRelations(t *testing.T) {
	_, hmcfg, _, lcs := directedPlanFixture(t)
	const (
		src = "merge_view/dirA/a"
		dst = "merge_view/dirA/a._renamed_test"
	)
	node := lcs.FileTree.FindNode(src)
	if node == nil {
		t.Fatalf("rename source %q missing", src)
	}
	if lcs.FileTree.FindNode(dst) != nil {
		t.Fatalf("rename destination %q unexpectedly exists", dst)
	}
	mk := func(rel PathRelation) *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: "rename", Variant: "stat", Rel: rel},
				RetB:     RetENOENT,
				OffB:     offsetBucketNA,
				PersistB: pathInDir(src, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(src),
				TypeB:    node.Type != NodeTypeFile,
			},
			BaseA:   src,
			BaseA2:  dst,
			BaseB:   src,
			FreshA2: true,
			Causal:  true,
		}
	}
	// HB A is dst, B is src: same-parent different names is Sibling, not Same.
	if !validateCausalPlan(hmcfg, lcs, mk(PathSibling)) {
		t.Fatal("rename destination/source Sibling rejected")
	}
	if validateCausalPlan(hmcfg, lcs, mk(PathSame)) {
		t.Fatal("rename destination/source Same accepted")
	}
}

func TestValidateCausalPlanPersistence(t *testing.T) {
	_, hmcfg, _, lcs := directedPlanFixture(t)
	const (
		src = "merge_view/dirA/a"
		dst = "merge_view/dirA/a._renamed_test"
	)
	node := lcs.FileTree.FindNode(src)
	if node == nil || lcs.FileTree.FindNode(dst) != nil {
		t.Fatalf("bad persistence fixture: src=%v dst-exists=%v",
			node != nil, lcs.FileTree.FindNode(dst) != nil)
	}
	plan := func(persist bool) *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: "rename", Variant: "stat", Rel: PathSibling},
				RetB:     RetENOENT,
				OffB:     offsetBucketNA,
				PersistB: persist,
				DepthB:   DepthBucketOf(src),
				TypeB:    node.Type != NodeTypeFile,
			},
			BaseA:   src,
			BaseA2:  dst,
			BaseB:   src,
			FreshA2: true,
			Causal:  true,
		}
	}
	for _, tc := range []struct {
		name    string
		dir     string
		persist bool
	}{
		{"configured", "merge_view/dirA", true},
		{"none", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hmcfg.Persistence_dir = tc.dir
			if got := validateCausalPlan(hmcfg, lcs, plan(tc.persist)); !got {
				t.Fatalf("persistence=%q plan rejected", tc.dir)
			}
		})
	}
}

func TestValidateCausalPlanExistingBRecheck(t *testing.T) {
	_, hmcfg, _, lcs := directedPlanFixture(t)
	const src = "merge_view/dirA/a"
	node := lcs.FileTree.FindNode(src)
	if node == nil {
		t.Fatalf("fixture path %q missing", src)
	}
	mk := func(variant string, ret RetBucket, off uint64) *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: "stat", Variant: variant, Rel: PathSame},
				RetB:     ret,
				OffB:     off,
				PersistB: pathInDir(src, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(src),
				TypeB:    node.Type != NodeTypeFile,
			},
			BaseA:  src,
			BaseB:  src,
			Causal: true,
		}
	}
	if !validateCausalPlan(hmcfg, lcs, mk("stat", RetSuccess, offsetBucketNA)) {
		t.Fatal("valid existing-B observer rejected")
	}
	if validateCausalPlan(hmcfg, lcs, mk("unlink", RetENOENT, offsetBucketNA)) {
		t.Fatal("unlink ENOENT under non-destructive root accepted")
	}
	if validateCausalPlan(hmcfg, lcs, mk("pread64", RetSuccess, offsetBucketNA)) {
		t.Fatal("stale pread offset bucket accepted")
	}
	if !validateCausalPlan(hmcfg, lcs, mk("pread64", RetSuccess, bucketOfOffset(node.Size, 0))) {
		t.Fatal("valid pread offset bucket rejected")
	}
}

func TestValidateCausalPlanFreshBRecheck(t *testing.T) {
	_, hmcfg, _, lcs := directedPlanFixture(t)
	const (
		parent = "merge_view/dirA"
		fresh  = "merge_view/dirA/mut_dir_test"
	)
	if lcs.FileTree.FindNode(parent) == nil || lcs.FileTree.FindNode(fresh) != nil {
		t.Fatal("bad fresh-B fixture")
	}
	mk := func(variant string, ret RetBucket, off uint64, typeB bool, baseB string) *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: "stat", Variant: variant, Rel: PathChild},
				RetB:     ret,
				OffB:     off,
				PersistB: pathInDir(baseB, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(baseB),
				TypeB:    typeB,
			},
			BaseA:  parent,
			BaseB:  baseB,
			BaseB2: parent,
			FreshB: true,
			Causal: true,
		}
	}
	if !validateCausalPlan(hmcfg, lcs, mk("mkdir", RetSuccess, offsetBucketNA, true, fresh)) {
		t.Fatal("valid fresh-B plan rejected")
	}
	if validateCausalPlan(hmcfg, lcs, mk("mkdir", RetFailure, offsetBucketNA, true, fresh)) {
		t.Fatal("fresh-B failure ret accepted")
	}
	if validateCausalPlan(hmcfg, lcs, mk("mkdir", RetSuccess, offsetBucketZero, true, fresh)) {
		t.Fatal("fresh-B non-NA offset accepted")
	}
	if validateCausalPlan(hmcfg, lcs, mk("mkdir", RetSuccess, offsetBucketNA, false, fresh)) {
		t.Fatal("fresh-B wrong type accepted")
	}
	if validateCausalPlan(hmcfg, lcs, mk("creat", RetSuccess, offsetBucketNA, false, "merge_view/dirA/a")) {
		t.Fatal("fresh-B existing path accepted")
	}
}

func TestCausalRenameSourceAcceptsDirClass(t *testing.T) {
	ft := hmdfsSmokeFileTree()
	cids := hmdfsSmokeCids()
	ft.AddNode("merge_view/dirA/empty", NodeTypeEmptyDir, cids[0])
	ft.AddNode("merge_view/dirA/plain", NodeTypeDir, cids[0])
	got := causalRenameSource(ft, "merge_view/dirA/empty")
	if got != "merge_view/dirA/plain" {
		t.Fatalf("rename source = %q, want merge_view/dirA/plain", got)
	}
}

func TestCausalRootC2SkipsRetiredCombo(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	byCell, cells := planCausalC2Cells(ps, lcs, hmcfg, "stat", "merge_view/dirA/a", "",
		"", "", false, causalRootNormal, newRand(target, rand.NewSource(15)))
	if len(cells) == 0 {
		t.Fatal("causal C2 fixture produced no plans")
	}
	var retired comboKey
	for _, ck := range cells {
		if p := byCell[ck]; p != nil {
			retired = p.Cell.Combo
			break
		}
	}
	covBoard.mu.Lock()
	covBoard.unreachable[retired] = true
	covBoard.mu.Unlock()

	byCell, cells = planCausalC2Cells(ps, lcs, hmcfg, "stat", "merge_view/dirA/a", "",
		"", "", false, causalRootNormal, newRand(target, rand.NewSource(16)))
	for _, ck := range cells {
		if p := byCell[ck]; p != nil && p.Cell.Combo == retired {
			t.Fatalf("retired combo still planned: %+v", p)
		}
	}
}

func TestValidateCausalPlanRenameENOTEMPTY(t *testing.T) {
	_, hmcfg, _, lcs := directedPlanFixture(t)
	ft := lcs.FileTree
	const (
		srcDir  = "merge_view/dirA"
		dstDir  = "merge_view/dirB"
		srcFile = "merge_view/dirA/a"
		dstFile = "merge_view/dirA/b"
		empty   = "merge_view/emptyDest"
	)
	ft.AddNode(empty, NodeTypeEmptyDir, hmcfg.Cids[0])
	mk := func(rootPath, baseB2, baseB string, isDir bool) *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: "stat", Variant: "rename", Rel: PathSame},
				RetB:     RetFailure,
				OffB:     offsetBucketNA,
				PersistB: pathInDir(baseB, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(baseB),
				TypeB:    isDir,
			},
			BaseA:  rootPath,
			BaseB2: baseB2,
			BaseB:  baseB,
			Causal: true,
		}
	}
	if !validateCausalPlan(hmcfg, lcs, mk(srcDir, srcDir, dstDir, true)) {
		t.Fatal("valid directory ENOTEMPTY plan rejected")
	}
	if validateCausalPlan(hmcfg, lcs, mk(srcFile, srcFile, dstFile, false)) {
		t.Fatal("file-to-file Failure plan accepted")
	}
	if validateCausalPlan(hmcfg, lcs, mk(srcDir, srcDir, empty, true)) {
		t.Fatal("empty-destination ENOTEMPTY plan accepted")
	}
}

func TestValidateCausalPlanRootChecks(t *testing.T) {
	_, hmcfg, _, lcs := directedPlanFixture(t)
	const (
		src    = "merge_view/dirA/a"
		dst    = "merge_view/dirA/a._renamed_test"
		parent = "merge_view/dirA"
		fresh  = "merge_view/dirA/mut_dir_test"
	)
	file := lcs.FileTree.FindNode(src)
	if file == nil || lcs.FileTree.FindNode(parent) == nil || lcs.FileTree.FindNode(dst) != nil {
		t.Fatal("bad root-check fixture")
	}
	freshPlan := func(root string) *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: root, Variant: "stat", Rel: PathParent},
				RetB:     RetSuccess,
				OffB:     offsetBucketNA,
				PersistB: false,
				DepthB:   DepthBucketOf(parent),
				TypeB:    true,
			},
			BaseA:  fresh,
			BaseA2: parent,
			BaseB:  parent,
			FreshA: true,
			Causal: true,
		}
	}
	if validateCausalPlan(hmcfg, lcs, freshPlan("stat")) {
		t.Fatal("FreshA with non-mkdir/creat root accepted")
	}
	if !validateCausalPlan(hmcfg, lcs, freshPlan("mkdir")) {
		t.Fatal("valid FreshA plan rejected")
	}

	renamePlan := func(root string) *directedPlan {
		return &directedPlan{
			Cell: directedCell{
				Combo:    comboKey{Root: root, Variant: "stat", Rel: PathSibling},
				RetB:     RetENOENT,
				OffB:     offsetBucketNA,
				PersistB: false,
				DepthB:   DepthBucketOf(src),
				TypeB:    false,
			},
			BaseA:   src,
			BaseA2:  dst,
			BaseB:   src,
			FreshA2: true,
			Causal:  true,
		}
	}
	if validateCausalPlan(hmcfg, lcs, renamePlan("stat")) {
		t.Fatal("FreshA2 with non-rename root accepted")
	}
	if !validateCausalPlan(hmcfg, lcs, renamePlan("rename")) {
		t.Fatal("valid FreshA2 plan rejected")
	}

	treeRootPlan := &directedPlan{
		Cell: directedCell{
			Combo:    comboKey{Root: "stat", Variant: "stat", Rel: PathChild},
			RetB:     RetSuccess,
			OffB:     offsetBucketNA,
			PersistB: false,
			DepthB:   DepthBucketOf(parent),
			TypeB:    true,
		},
		BaseA:  "merge_view",
		BaseB:  parent,
		Causal: true,
	}
	if validateCausalPlan(hmcfg, lcs, treeRootPlan) {
		t.Fatal("merge_view tree root accepted as a normal root")
	}

	treeRootFresh := freshPlan("mkdir")
	treeRootFresh.BaseA = "merge_view/freshRoot"
	treeRootFresh.BaseA2 = "merge_view"
	treeRootFresh.Cell.DepthB = DepthBucketOf(treeRootFresh.BaseA)
	treeRootFresh.Cell.PersistB = pathInDir(treeRootFresh.BaseA, hmcfg.Persistence_dir)
	if validateCausalPlan(hmcfg, lcs, treeRootFresh) {
		t.Fatal("FreshA with merge_view root parent accepted")
	}
}

func TestCausalRenameRootPathAllowed(t *testing.T) {
	plain := causalAnchor{Path: "merge_view/dirA/a", Source: "merge_view/dirA/a"}
	if !causalRenameRootPathAllowed(plain, "merge_view/dirA/a") {
		t.Fatal("non-rename anchor root path was filtered")
	}
	rename := causalAnchor{Path: "merge_view/dirA/dst", Source: "merge_view/dirA/src"}
	for _, rootPath := range []string{
		"merge_view/dirA/src",
		"merge_view/dirA/src/kid",
	} {
		if causalRenameRootPathAllowed(rename, rootPath) {
			t.Fatalf("rename source subtree root %q accepted", rootPath)
		}
	}
	for _, rootPath := range []string{
		"merge_view/dirA",
		"merge_view/dirA/sibling",
	} {
		if !causalRenameRootPathAllowed(rename, rootPath) {
			t.Fatalf("safe rename root %q rejected", rootPath)
		}
	}
}

func TestCausalRootC1FiltersRenameSourceSubtree(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	ft := lcs.FileTree
	const (
		source  = "merge_view/dirA/srcDir"
		kid     = "merge_view/dirA/srcDir/kid"
		sibling = "merge_view/dirA/srcSibling"
		dest    = "merge_view/dirA/renamedDir"
	)
	ft.AddNode(source, NodeTypeDir, hmcfg.Cids[0])
	ft.AddNode(kid, NodeTypeFile, hmcfg.Cids[0])
	ft.AddNode(sibling, NodeTypeDir, hmcfg.Cids[0])
	ft.AddNode(dest, NodeTypeDir, hmcfg.Cids[0])
	anchor := mkRenameCall(t, target, source, dest, 100, 200)
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(17)))
	parentFound := false
	for _, p := range plans {
		if pathUnderRoot(p.RootPath, source) {
			t.Fatalf("anchor source subtree root was planned: %+v", p)
		}
		if p.RootPath == "merge_view/dirA" {
			parentFound = true
		}
	}
	if !parentFound {
		t.Fatal("C1 lost the root path through the anchor source parent")
	}
}

func TestCausalRootC1FreshRenameFiltersSourceSubtree(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	ft := lcs.FileTree
	const (
		source  = "merge_view/dirA/srcDir"
		kid     = "merge_view/dirA/srcDir/kid"
		sibling = "merge_view/dirA/srcSibling"
		dest    = "merge_view/dirA/newDir"
	)
	ft.AddNode(source, NodeTypeDir, hmcfg.Cids[0])
	ft.AddNode(kid, NodeTypeFile, hmcfg.Cids[0])
	ft.AddNode(sibling, NodeTypeDir, hmcfg.Cids[0])
	anchor := mkRenameCall(t, target, source, dest, 100, 200)
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(23)))
	if len(plans) == 0 {
		t.Fatal("fresh rename anchor produced no C1 plans")
	}
	parentFound, siblingFound := false, false
	for _, p := range plans {
		if pathUnderRoot(p.RootPath, source) {
			t.Fatalf("fresh rename source subtree root was planned: %+v", p)
		}
		if p.RootPath == "merge_view/dirA" {
			parentFound = true
		}
		if p.RootPath == sibling {
			siblingFound = true
		}
	}
	if !parentFound {
		t.Fatal("C1 lost the fresh-rename source parent root path")
	}
	if !siblingFound {
		t.Fatal("C1 lost the fresh-rename source sibling root path")
	}
}

func TestPlanCausalC2RejectsTreeRootFreshParent(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	byCell, cells := planCausalC2Cells(ps, lcs, hmcfg, "mkdir",
		"merge_view/freshRoot", "merge_view", "", "", false,
		causalRootFresh, newRand(target, rand.NewSource(25)))
	if len(cells) != 0 || len(byCell) != 0 {
		t.Fatalf("C2 accepted merge_view as fresh root parent: cells=%d plans=%d", len(cells), len(byCell))
	}
}

func TestPlanCausalC2RejectsNestedFreshRootParent(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	const nestedParent = "merge_view/dirA/newParent/newChild"
	byCell, cells := planCausalC2CellsWithParent(ps, lcs, hmcfg, "mkdir",
		nestedParent+"/root", nestedParent, AnchorPathFreshDir, true, true,
		"", "", false, causalRootFresh, newRand(target, rand.NewSource(26)))
	if len(cells) != 0 || len(byCell) != 0 {
		t.Fatalf("C2 accepted nested fresh parent: cells=%d plans=%d", len(cells), len(byCell))
	}
}

func TestCausalRootC1RenameSame(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkTruncate(target, "merge_view/dirA/a", 1)
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(18)))
	found := false
	for _, p := range plans {
		if p.RootKind != causalRootRename ||
			p.RootSource != "merge_view/dirA/a" ||
			p.RootPath != "merge_view/dirA/b" {
			continue
		}
		found = true
		if p.Cell.Rel != PathSame {
			t.Fatalf("rename root from the anchor path planned as %v, want Same: %+v", p.Cell.Rel, p)
		}
	}
	if !found {
		t.Fatal("C1 missed the anchor-source rename relation")
	}
}

func TestCausalRootC1RejectsNestedFreshRootPlan(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	const (
		parentPath = "merge_view/dirA/newParent"
		nestedPath = "merge_view/dirA/newParent/newChild"
	)
	parent := mkMkdir(target, parentPath)
	parent.CheckInfo = &FileMetadata{Stime: 300, Etime: 400, Retv: 0}
	nested := mkMkdir(target, nestedPath)
	nested.CheckInfo = &FileMetadata{Stime: 410, Etime: 500, Retv: 0}
	ps[0].Calls = append(ps[0].Calls, parent, nested)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(24)))
	for _, p := range plans {
		if p.Anchor.Path != nestedPath {
			continue
		}
		// Normal root Parent/Sibling plans remain valid for a nested fresh
		// anchor; only fresh-root plans under the fresh parent are forbidden.
		if p.RootParent == nestedPath || pathUnderRoot(p.RootPath, nestedPath) {
			t.Fatalf("nested fresh parent produced a fresh-root plan: %+v", p)
		}
	}
}

func TestCausalRootRelPathsLinkTargetOnly(t *testing.T) {
	cases := []struct {
		name     string
		kind     causalRootKind
		call     string
		path     string
		source   string
		dest     string
		wantPre  string
		wantPost string
	}{
		{
			name: "link target", kind: causalRootNormal, call: "link",
			path: "dirA/a", source: "dirA/a", dest: "dirB/l1",
			wantPre: "dirA/a", wantPost: "dirA/a",
		},
		{
			name: "symlink target", kind: causalRootNormal, call: "symlink",
			path: "dirA/a", source: "dirA/a", dest: "dirB/l2",
			wantPre: "dirA/a", wantPost: "dirA/a",
		},
		{
			name: "rename destination", kind: causalRootRename, call: "rename",
			path: "dirA/b", source: "dirA/a", dest: "dirA/b",
			wantPre: "dirA/b", wantPost: "dirA/b",
		},
		{
			name: "ordinary root", kind: causalRootNormal, call: "stat",
			path: "dirA/a", source: "dirA/a", dest: "dirA/a",
			wantPre: "dirA/a", wantPost: "dirA/a",
		},
	}
	for _, tc := range cases {
		pre, post := causalRootRelPaths(tc.kind, tc.call, tc.path, tc.source, tc.dest)
		if pre != tc.wantPre || post != tc.wantPost {
			t.Fatalf("%s: paths = %q/%q, want %q/%q",
				tc.name, pre, post, tc.wantPre, tc.wantPost)
		}
	}
}

func TestCausalRootC1FreshParentEligibility(t *testing.T) {
	target, hmcfg, _, lcs := directedPlanFixture(t)
	ps := directedPlanBaseProgs(target)
	anchor := mkMkdir(target, "merge_view/dirA")
	anchor.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	ps[0].Calls = append([]*Call{anchor}, ps[0].Calls...)
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, newRand(target, rand.NewSource(19)))
	sawAllowed := false
	for _, p := range plans {
		if p.RootKind != causalRootFresh {
			continue
		}
		if p.RootParent == "merge_view" {
			t.Fatalf("fresh root used the merge_view tree root as parent: %+v", p)
		}
		if p.RootParent == "merge_view/dirA" {
			sawAllowed = true
		}
	}
	if !sawAllowed {
		t.Fatal("fresh root under an eligible directory was lost")
	}
}
