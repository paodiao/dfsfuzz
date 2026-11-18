package prog

import (
	"math/rand"
	"strings"
	"testing"
)

func TestLegacyPathMutableCall(t *testing.T) {
	for _, name := range []string{"link", "symlink", "readlink"} {
		if legacyPathMutableCall(name) {
			t.Fatalf("%s must not enter legacy path mutation", name)
		}
	}
	for _, name := range []string{"open", "rename", "stat", "unlink"} {
		if !legacyPathMutableCall(name) {
			t.Fatalf("%s should remain legacy path mutable", name)
		}
	}
}

func TestFilterLegacyPathCalls(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	if _, ok := target.SyscallMap["readlink"]; !ok {
		t.Skip("target has no readlink")
	}
	p := &Prog{Target: target, Calls: []*Call{
		mkOpen(target, "merge_view/dirA/a"),
		mkLinkCall(t, target, "merge_view/dirA/a", "merge_view/dirA/l1"),
		mkSymlinkCall(t, target, "a", "merge_view/dirA/l2"),
		mkReadlinkCall(t, target, "merge_view/dirA/l2"),
	}}
	in := []ConcurrentCall{
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 0}},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 1}},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 2}},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 3}},
	}
	out := filterLegacyPathCalls([]*Prog{p}, in)
	if len(out) != 1 || out[0].Pos.CallIdx != 0 {
		t.Fatalf("filtered legacy group = %+v, want only the open", out)
	}
}

func TestUpdateCallPathInProgSkipsDualPathCalls(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	if _, ok := target.SyscallMap["readlink"]; !ok {
		t.Skip("target has no readlink")
	}
	link := mkLinkCall(t, target, "merge_view/dirA/a", "merge_view/dirA/l1")
	sym := mkSymlinkCall(t, target, "a", "merge_view/dirA/l2")
	read := mkReadlinkCall(t, target, "merge_view/dirA/l2")
	p := &Prog{Target: target, Calls: []*Call{link, sym, read}}
	r := newRand(target, rand.NewSource(11))

	for i, want := range []string{
		"merge_view/dirA/a",
		"a",
		"merge_view/dirA/l2",
	} {
		if updateCallPathInProg(p, i, "merge_view/dirB/new", r, nil) {
			t.Fatalf("call %d accepted legacy path mutation", i)
		}
		if got := extractPathFromCallByArgIdx(p.Calls[i], 0); got != want {
			t.Fatalf("call %d arg0 = %q, want unchanged %q", i, got, want)
		}
	}
}

func TestUpdateCallPathRenameHMDFSPolicy(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree(), Profile: hmdfsProfile}
	rn := mkRenameCall(t, target, "merge_view/dirA/a", "merge_view/dirA/b", 100, 200)
	p := &Prog{Target: target, Calls: []*Call{rn}}
	r := newRand(target, rand.NewSource(12))
	newSrc := "merge_view/dirB/c"
	if !updateCallPathInProg(p, 0, newSrc, r, lcs) {
		t.Fatal("HMDFS rename update failed")
	}
	if got := extractPathFromCallByArgIdx(p.Calls[0], 0); got != newSrc {
		t.Fatalf("rename source = %q, want %q", got, newSrc)
	}
	dst := extractPathFromCallByArgIdx(p.Calls[0], 1)
	if GetParentDir(dst) != GetParentDir(newSrc) || !strings.Contains(dst, "._renamed_") {
		t.Fatalf("HMDFS rename dst = %q, want fresh same-parent suffix", dst)
	}
}

func TestUpdateCallPathRenameGlusterfsPolicy(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	lcs := &LayeredChoiceStrategy{FileTree: glusterfsPlanTree(), Profile: glusterfsProfile}
	rn := mkRenameCall(t, target, "dirA/a", "dirA/b", 100, 200)
	p := &Prog{Target: target, Calls: []*Call{rn}}
	const newSrc = "dirB/c"
	expectedRand := newRand(target, rand.NewSource(13))
	expected := lcs.pickRenameDstFor(newSrc, PathNoRel, expectedRand.Rand)
	if expected == "" {
		t.Fatal("GlusterFS rename policy produced no destination")
	}
	r := newRand(target, rand.NewSource(13))
	if !updateCallPathInProg(p, 0, newSrc, r, lcs) {
		t.Fatal("GlusterFS rename update failed")
	}
	if got := extractPathFromCallByArgIdx(p.Calls[0], 0); got != newSrc {
		t.Fatalf("rename source = %q, want %q", got, newSrc)
	}
	if got := extractPathFromCallByArgIdx(p.Calls[0], 1); got != expected {
		t.Fatalf("GlusterFS rename dst = %q, want policy result %q", got, expected)
	}
}
