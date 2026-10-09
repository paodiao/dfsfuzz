package prog

import (
	"math/rand"
	"testing"
)

// TestRenameSameParentContract pins the single same-parent predicate shared by
// validation, E candidate sampling and the batch postcondition checker.
func TestRenameSameParentContract(t *testing.T) {
	cases := []struct {
		src, dst string
		want     bool
	}{
		{"merge_view/d/a", "merge_view/d/b", true},
		{"merge_view/d/a", "merge_view/x/b", false},
		{"merge_view/d", "merge_view/d/a", false},
		{"merge_view/d/a", "merge_view/d/a", true},
		{"", "merge_view/d/a", false},
		{"merge_view/d/a", "", false},
	}
	for _, c := range cases {
		if got := renameSameParent(c.src, c.dst); got != c.want {
			t.Fatalf("renameSameParent(%q,%q)=%v want %v", c.src, c.dst, got, c.want)
		}
	}
}

// TestRenameSuffixBaseContract pins the suffix normalization: one trailing
// "._renamed_*" level is stripped and non-rename extensions stay untouched, so
// repeated materialization cannot accumulate one suffix per round.
func TestRenameSuffixBaseContract(t *testing.T) {
	cases := []struct{ in, want string }{
		{"merge_view/d/a", "merge_view/d/a"},
		{"merge_view/d/a._renamed_aaaaaa", "merge_view/d/a"},
		{"merge_view/d/a.txt", "merge_view/d/a.txt"},
		{"merge_view/d/a.txt._renamed_aaaaaa", "merge_view/d/a.txt"},
		{"merge_view/d/a._renamed_x._renamed_y", "merge_view/d/a._renamed_x"},
	}
	for _, c := range cases {
		if got := renameSuffixBase(c.in); got != c.want {
			t.Fatalf("renameSuffixBase(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestPlanStateEffectTable pins the ret-aware effect classification driving
// the batch's one-state-authority rule. mkdir is classified by capability
// (any ret is a creator), while the remover loser forms stay observers.
func TestPlanStateEffectTable(t *testing.T) {
	mk := func(variant string, ret RetBucket, freshB bool) directedPlan {
		p := directedPlan{
			Cell: directedCell{
				Combo: comboKey{Root: "open", Variant: variant, Rel: PathSame},
				RetB:  ret,
			},
		}
		p.FreshB = freshB
		return p
	}
	cases := []struct {
		name string
		p    directedPlan
		want planEffect
	}{
		{"unlink success", mk("unlink", RetSuccess, false), planEffectDestroy},
		{"unlink enoent", mk("unlink", RetENOENT, false), planEffectObserver},
		{"rmdir failure", mk("rmdir", RetFailure, false), planEffectObserver},
		{"rename success", mk("rename", RetSuccess, false), planEffectDestroy},
		{"rename enoent", mk("rename", RetENOENT, false), planEffectObserver},
		{"write success", mk("write", RetSuccess, false), planEffectSize},
		{"pwrite64 success", mk("pwrite64", RetSuccess, false), planEffectSize},
		{"truncate success", mk("truncate", RetSuccess, false), planEffectSize},
		{"creat existing", mk("creat", RetSuccess, false), planEffectSize},
		{"creat fresh B", mk("creat", RetSuccess, true), planEffectObserver},
		{"mkdir eexist", mk("mkdir", RetEEXIST, false), planEffectCreate},
		{"stat", mk("stat", RetSuccess, false), planEffectObserver},
		{"chmod", mk("chmod", RetSuccess, false), planEffectObserver},
		{"read", mk("read", RetSuccess, false), planEffectObserver},
		{"pread64", mk("pread64", RetSuccess, false), planEffectObserver},
		{"fsync", mk("fsync", RetSuccess, false), planEffectObserver},
	}
	for _, c := range cases {
		if got := planStateEffect(&c.p); got != c.want {
			t.Fatalf("%s: effect=%v want %v", c.name, got, c.want)
		}
	}
}

// TestBatchBaseBAuthorityOK pins the assembler postcondition checker: writers
// force a single plan per path, observers coexist, FreshSame is exempt, and E
// destinations must stay reserved.
func TestBatchBaseBAuthorityOK(t *testing.T) {
	mk := func(variant string, ret RetBucket, baseB string, replace bool) directedPlan {
		p := directedPlan{
			Cell:  directedCell{Combo: comboKey{Root: "stat", Variant: variant, Rel: PathSame}, RetB: ret},
			BaseA: "merge_view/d/r", BaseB: baseB,
		}
		p.RenameReplace = replace
		return p
	}
	if !batchBaseBAuthorityOK([]directedPlan{
		mk("stat", RetSuccess, "merge_view/d/a", false),
		mk("chmod", RetSuccess, "merge_view/d/a", false),
	}) {
		t.Fatal("two observers on one path must pass")
	}
	if batchBaseBAuthorityOK([]directedPlan{
		mk("write", RetSuccess, "merge_view/d/a", false),
		mk("stat", RetSuccess, "merge_view/d/a", false),
	}) {
		t.Fatal("writer + observer on one path must fail")
	}
	if batchBaseBAuthorityOK([]directedPlan{
		mk("write", RetSuccess, "merge_view/d/a", false),
		mk("truncate", RetSuccess, "merge_view/d/a", false),
	}) {
		t.Fatal("two writers on one path must fail")
	}
	fresh := mk("stat", RetSuccess, "merge_view/d/a", false)
	fresh.FreshSame = true
	if !batchBaseBAuthorityOK([]directedPlan{fresh, mk("write", RetSuccess, "merge_view/d/a", false)}) {
		t.Fatal("FreshSame entries must be exempt")
	}
	// E destination equal to a sibling rename source.
	eA := mk("rename", RetSuccess, "merge_view/d/x", true)
	eA.BaseB2 = "merge_view/d/s"
	eB := mk("rename", RetSuccess, "merge_view/d/y", true)
	eB.BaseB2 = "merge_view/d/x"
	if batchBaseBAuthorityOK([]directedPlan{eA, eB}) {
		t.Fatal("E destination equal to a sibling source must fail")
	}
	// Duplicate E destination.
	dupA := mk("rename", RetSuccess, "merge_view/d/x", true)
	dupA.BaseB2 = "merge_view/d/s"
	dupB := mk("rename", RetSuccess, "merge_view/d/x", true)
	dupB.BaseB2 = "merge_view/d/t"
	if batchBaseBAuthorityOK([]directedPlan{dupA, dupB}) {
		t.Fatal("duplicate E destination must fail")
	}
	// E destination equal to a sibling anchor.
	anchorHit := mk("rename", RetSuccess, "merge_view/d/a", true)
	anchorHit.BaseB2 = "merge_view/d/s"
	other := mk("stat", RetSuccess, "merge_view/d/z", false)
	other.BaseA = "merge_view/d/a"
	if batchBaseBAuthorityOK([]directedPlan{anchorHit, other}) {
		t.Fatal("E destination equal to an anchor must fail")
	}
}

// TestInsertDirectedBatchRejectsAuthorityDrift pins the insert-time ledger
// protection: a batch whose authority postcondition is violated (here by
// duplicating a writer plan on the same BaseB) is rejected as insertFail
// before any mutation.
func TestInsertDirectedBatchRejectsAuthorityDrift(t *testing.T) {
	target, hmcfg, sc, lcs := renameFixture(t)
	var batch *directedBatch
	var ps []*Prog
	batches, writerBatches := 0, 0
	for seed := int64(0); seed < 300 && batch == nil; seed++ {
		ps = directedPlanBaseProgs(target)
		cand, _ := PickDirectedBatch(ps, lcs, hmcfg, newRand(target, rand.NewSource(seed)))
		if cand == nil || len(cand.Plans) == 0 {
			continue
		}
		batches++
		dup := -1
		for i := range cand.Plans {
			if cand.Plans[i].FreshSame {
				continue
			}
			if planStateEffect(&cand.Plans[i]) != planEffectObserver {
				dup = i
				break
			}
		}
		if dup < 0 {
			continue
		}
		writerBatches++
		p := cand.Plans[dup]
		p.NodeX = 1
		q := p
		q.NodeX = 2
		bad := &directedBatch{
			Root: cand.Root, BaseA: cand.BaseA, BaseA2: cand.BaseA2,
			Plans: []directedPlan{p, q},
		}
		if len(ps) < 1+len(bad.Plans) {
			continue
		}
		saved := len(ps[0].Calls)
		if insertDirectedBatch(ps, newRand(target, rand.NewSource(7)), sc, hmcfg, lcs, bad) {
			t.Fatal("authority-drifted batch accepted")
		}
		if len(ps[0].Calls) != saved {
			t.Fatal("ps mutated on rejected batch")
		}
		if len(lcs.PendingDirected) != 0 {
			t.Fatal("pending recorded on rejected batch")
		}
		batch = cand
	}
	if batch == nil {
		t.Fatalf("no writer batch sampled in 300 seeds (batches=%d writer=%d)", batches, writerBatches)
	}
}
