package prog

import (
	"math/rand"
	"strings"
	"testing"
)

func TestDualPathProjection(t *testing.T) {
	rename := rootPaths("rename", "dir/src", "dir/dst")
	if rename.Kind != DualRename || rename.Source() != "dir/src" ||
		rename.Dest() != "dir/dst" || rename.ObservedPath() != "dir/src" {
		t.Fatalf("rename projection = %+v", rename)
	}
	link := rootPaths("link", "dir/a", "dir/l1")
	if link.Kind != DualLink || link.Source() != "dir/a" ||
		link.Dest() != "dir/l1" || link.ObservedPath() != "dir/a" {
		t.Fatalf("link projection = %+v", link)
	}
	sym := rootPaths("symlink", "dir/a", "dir/l2")
	if sym.Kind != DualSymlink || sym.Source() != "dir/a" ||
		sym.Dest() != "dir/l2" || sym.ObservedPath() != "dir/a" {
		t.Fatalf("symlink projection = %+v", sym)
	}
	read := rootPaths("readlink", "dir/l2", "dir/l2")
	if read.Kind != DualReadlink || read.Source() != "dir/l2" ||
		read.Dest() != "dir/l2" || read.ObservedPath() != "dir/l2" {
		t.Fatalf("readlink projection = %+v", read)
	}
}

func TestDeriveSeedRootPathLinkSymlink(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	r := newRand(target, rand.NewSource(21))
	r.hmcfg = &Hmdfs_config{Profile: glusterfsProfile}

	anchor := seedAnchor{
		Pos:      GroupPosition{ProgIdx: 0, CallIdx: 0},
		Path:     "merge_view/dirA/a",
		PathKind: AnchorPathExisting,
	}
	link, ok := deriveSeedRootPath(lcs.FileTree, anchor, "link", PathSame, r)
	if !ok {
		t.Fatal("link derivation failed")
	}
	if link.TargetPath != anchor.Path || link.Dual != DualLink ||
		!strings.HasPrefix(link.LinkPath, "merge_view/dirA/Eris_link_") {
		t.Fatalf("link derivation = %+v", link)
	}
	if lcs.FileTree.FindNode(link.LinkPath) != nil {
		t.Fatalf("linkpath unexpectedly exists: %q", link.LinkPath)
	}
	if got := link.RootPaths().ObservedPath(); got != anchor.Path {
		t.Fatalf("link observed path = %q, want target", got)
	}

	dirAnchor := anchor
	dirAnchor.Path = "merge_view/dirA"
	sym, ok := deriveSeedRootPath(lcs.FileTree, dirAnchor, "symlink", PathSame, r)
	if !ok {
		t.Fatal("symlink derivation failed")
	}
	if sym.TargetPath != dirAnchor.Path || sym.Dual != DualSymlink || !sym.RootIsDir {
		t.Fatalf("symlink derivation = %+v", sym)
	}
	if !strings.HasPrefix(sym.LinkPath, "merge_view/dirA/Eris_symlink_") {
		t.Fatalf("symlink linkpath = %q", sym.LinkPath)
	}
}

func TestCollectSeedAnchorsCanonicalLinkTarget(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	prog := &Prog{Target: target}
	link := mkLinkCall(t, target, "merge_view/dirA/a", "merge_view/dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	sym := mkSymlinkCall(t, target, "a", "merge_view/dirA/l2")
	sym.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	prog.Calls = []*Call{link, sym}

	anchors := collectSeedAnchors(prog, lcs.FileTree, true)
	byPath := map[string]seedAnchor{}
	for _, a := range anchors {
		byPath[a.Path] = a
	}
	if _, ok := byPath["merge_view/dirA/a"]; !ok {
		t.Fatalf("canonical target anchor missing: %+v", byPath)
	}
	if a, ok := byPath["merge_view/dirA/l2"]; !ok || a.PathKind != AnchorPathFreshSymlink || a.Dual != DualSymlink {
		t.Fatalf("symlink linkpath anchor = %+v, ok=%v", a, ok)
	}
	if _, ok := byPath["a"]; ok {
		t.Fatal("symlink payload leaked as an anchor path")
	}
	for path, a := range byPath {
		if a.CallName == "symlink" && path != "merge_view/dirA/a" &&
			path != "merge_view/dirA/l2" {
			t.Fatalf("unexpected symlink anchor %q: %+v", path, a)
		}
	}
}

func TestGlusterfsCausalDerivationRoots(t *testing.T) {
	table := newCausalDerivationTableFor(glusterfsProfile)
	for _, want := range []string{"link", "symlink", "setxattr"} {
		weights, ok := table.Weights[want]
		if !ok || len(weights) == 0 {
			t.Fatalf("glusterfs causal table missing anchor %q", want)
		}
	}
	for _, forbidden := range []string{"readlink", "getxattr"} {
		if _, ok := table.Weights[forbidden]; ok {
			t.Fatalf("glusterfs causal table has non-root anchor %q", forbidden)
		}
	}
	weights := table.Weights["open"]
	for variant := range weights {
		if variant.CallName == "readlink" || variant.CallName == "getxattr" {
			t.Fatalf("causal table root variant %q leaked", variant.CallName)
		}
	}
}

func TestDctBaseCandidatesLinkRoles(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	link := mkLinkCall(t, target, "merge_view/dirA/a", "merge_view/dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	sym := mkSymlinkCall(t, target, "a", "merge_view/dirA/l2")
	sym.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	read := mkReadlinkCall(t, target, "merge_view/dirA/l1")
	read.CheckInfo = &FileMetadata{Stime: 270, Etime: 300, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{link, sym, read}}

	// insertPos=2: link and symlink are before the insertion point, readlink
	// is not. Only the executed link/symlink create usable linkpaths.
	got := dctBaseCandidates(p, 2)
	want := map[string]bool{
		"merge_view/dirA/a":  true,
		"merge_view/dirA/l1": true,
		"merge_view/dirA/l2": true,
	}
	for _, path := range got {
		if !want[path] {
			t.Fatalf("unexpected base candidate %q in %v", path, got)
		}
		delete(want, path)
	}
	if len(want) != 0 {
		t.Fatalf("missing base candidates %v in %v", want, got)
	}

	// insertPos=0: no call has executed, so linkpaths must not be candidates.
	got = dctBaseCandidates(p, 0)
	for _, path := range got {
		if strings.Contains(path, "/l1") || strings.Contains(path, "/l2") {
			t.Fatalf("unexecuted linkpath became a base candidate: %q", path)
		}
	}
}
