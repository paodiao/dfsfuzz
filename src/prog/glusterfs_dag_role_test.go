package prog

import "testing"

// TestGlusterfsLinkAEmitsTargetAndLinkpathPairs pins the dual-role HB A rule:
// link(A/a, A/l1) followed by unlink(A/l1) produces one target pair (sibling)
// and one linkpath pair (same path). The old single-pair extractor returned
// only the linkpath/Same pair because pairPathCandidates tried Path first.
func TestGlusterfsLinkAEmitsTargetAndLinkpathPairs(t *testing.T) {
	link := DAGVertex{
		FuncID: FuncLink, CallName: "link",
		Path: "dirA/l1", OldPath: "dirA/a",
		RetBucket: RetSuccess, Stime: 100, Etime: 200, ProgIdx: 0,
	}
	unlink := DAGVertex{
		FuncID: FuncUnlink, CallName: "unlink",
		Path: "dirA/l1", RetBucket: RetSuccess,
		Stime: 300, Etime: 400, ProgIdx: 0,
	}
	hb, cc := ExtractPairsFor(glusterfsProfile, []DAGVertex{link, unlink}, nil)
	if len(cc) != 0 {
		t.Fatalf("cc pairs = %d, want 0", len(cc))
	}
	if len(hb) != 2 {
		t.Fatalf("hb pairs = %d, want 2: %+v", len(hb), hb)
	}
	got := make(map[string]PathRel, len(hb))
	for _, p := range hb {
		got[p.APath+"\x00"+p.BPath] = p.PathRel
	}
	if rel, ok := got["dirA/a\x00dirA/l1"]; !ok || rel != PathSameParent {
		t.Fatalf("target pair = %v (found=%v), want dirA/a/dirA/l1 SameParent", got, ok)
	}
	if rel, ok := got["dirA/l1\x00dirA/l1"]; !ok || rel != PathSamePath {
		t.Fatalf("linkpath pair = %v (found=%v), want dirA/l1/dirA/l1 SamePath", got, ok)
	}
}

// TestGlusterfsLinkBCellUsesTarget pins the B-side rule: link is the later
// operation, so the cell depth/persist dimensions come from its target, not
// its program-local linkpath. The persist directory intentionally contains
// only the linkpath, which keeps the regression observable.
func TestGlusterfsLinkBCellUsesTarget(t *testing.T) {
	const (
		targetPath = "dirA/a"
		linkPath   = "dirA/sub/deep/l1"
		persistDir = "dirA/sub"
	)
	unlink := DAGVertex{
		FuncID: FuncUnlink, CallName: "unlink",
		Path: targetPath, RetBucket: RetSuccess,
		Stime: 100, Etime: 200, ProgIdx: 0,
	}
	link := DAGVertex{
		FuncID: FuncLink, CallName: "link",
		Path: linkPath, OldPath: targetPath,
		RetBucket: RetSuccess, Stime: 300, Etime: 400, ProgIdx: 0,
	}
	hb, _ := ExtractPairsFor(glusterfsProfile, []DAGVertex{unlink, link}, nil)
	if len(hb) != 1 {
		t.Fatalf("hb pairs = %d, want 1: %+v", len(hb), hb)
	}
	p := hb[0]
	if p.BPath != targetPath {
		t.Fatalf("BPath = %q, want target %q", p.BPath, targetPath)
	}
	ck, ok := cellKeyOf(&p, persistDir)
	if !ok {
		t.Fatal("cellKeyOf failed for link B pair")
	}
	if ck.DepthB != DepthBucketOf(targetPath) {
		t.Fatalf("DepthB = %d, want target bucket %d", ck.DepthB, DepthBucketOf(targetPath))
	}
	if ck.DepthB == DepthBucketOf(linkPath) && targetPath != linkPath {
		t.Fatalf("DepthB used the linkpath bucket")
	}
	if ck.PersistB {
		t.Fatalf("PersistB = true; linkpath leaked into the cell (target is outside %q)", persistDir)
	}
}

// TestGlusterfsRenameBUsesSourcePath pins the B-side rule: rename B's
// relation/cell path is the source, while the destination is only used to
// materialize rename(src,dst). A write at the destination before the rename is
// a sibling, not the same path.
func TestGlusterfsRenameBUsesSourcePath(t *testing.T) {
	write := DAGVertex{
		FuncID: FuncWrite, CallName: "write",
		Path: "dirA/b", RetBucket: RetSuccess,
		Stime: 100, Etime: 200, ProgIdx: 0,
	}
	rename := DAGVertex{
		FuncID: FuncRename, CallName: "rename",
		Path: "dirA/b", OldPath: "dirA/a",
		RetBucket: RetSuccess, Stime: 300, Etime: 400, ProgIdx: 0,
	}
	hb, _ := ExtractPairsFor(glusterfsProfile, []DAGVertex{write, rename}, nil)
	if len(hb) != 1 {
		t.Fatalf("hb pairs = %d, want 1: %+v", len(hb), hb)
	}
	p := hb[0]
	if p.BPath != "dirA/a" || p.PathRel != PathSameParent {
		t.Fatalf("rename B pair = %+v, want src dirA/a + SameParent", p)
	}
	ck, ok := cellKeyOf(&p, "")
	if !ok {
		t.Fatal("cellKeyOf failed for rename pair")
	}
	if ck.DepthB != DepthBucketOf("dirA/a") {
		t.Fatalf("rename DepthB = %d, want source bucket %d", ck.DepthB, DepthBucketOf("dirA/a"))
	}
}

// TestGlusterfsRenameRoleCases covers the four HB orderings that used to be
// conflated by the {dst,src} union.
func TestGlusterfsRenameRoleCases(t *testing.T) {
	mk := func(fid uint32, name, path, old string, st, et uint64) DAGVertex {
		return DAGVertex{FuncID: fid, CallName: name, Path: path, OldPath: old,
			RetBucket: RetSuccess, Stime: st, Etime: et, ProgIdx: 0}
	}
	cases := []struct {
		name string
		a, b DAGVertex
		rel  PathRel
		bp   string
	}{
		{
			name: "rename A then stat old source",
			a:    mk(FuncRename, "rename", "dirA/b", "dirA/a", 100, 200),
			b:    mk(FuncGetattr, "stat", "dirA/a", "", 300, 400),
			rel:  PathSameParent, bp: "dirA/a",
		},
		{
			name: "rename A then stat new destination",
			a:    mk(FuncRename, "rename", "dirA/b", "dirA/a", 100, 200),
			b:    mk(FuncGetattr, "stat", "dirA/b", "", 300, 400),
			rel:  PathSamePath, bp: "dirA/b",
		},
		{
			name: "write old source then rename B",
			a:    mk(FuncWrite, "write", "dirA/a", "", 100, 200),
			b:    mk(FuncRename, "rename", "dirA/b", "dirA/a", 300, 400),
			rel:  PathSamePath, bp: "dirA/a",
		},
		{
			name: "write destination then rename B",
			a:    mk(FuncWrite, "write", "dirA/b", "", 100, 200),
			b:    mk(FuncRename, "rename", "dirA/b", "dirA/a", 300, 400),
			rel:  PathSameParent, bp: "dirA/a",
		},
	}
	for _, tc := range cases {
		hb, _ := ExtractPairsFor(glusterfsProfile, []DAGVertex{tc.a, tc.b}, nil)
		if len(hb) != 1 {
			t.Fatalf("%s: hb pairs = %d, want 1: %+v", tc.name, len(hb), hb)
		}
		p := hb[0]
		if p.PathRel != tc.rel || p.BPath != tc.bp {
			t.Fatalf("%s: pair = %+v, want rel=%v BPath=%q", tc.name, p, tc.rel, tc.bp)
		}
	}
}

// TestHmdfsRenameUsesRolePaths pins that the rename rule is shared by HMDFS,
// not a GlusterFS-only special case.
func TestHmdfsRenameUsesRolePaths(t *testing.T) {
	write := DAGVertex{
		FuncID: FuncWrite, CallName: "write",
		Path: "merge_view/d/a", RetBucket: RetSuccess,
		Stime: 100, Etime: 200, ProgIdx: 0,
	}
	rename := DAGVertex{
		FuncID: FuncRename, CallName: "rename",
		Path: "merge_view/d/b", OldPath: "merge_view/d/a",
		RetBucket: RetSuccess, Stime: 300, Etime: 400, ProgIdx: 0,
	}
	hb, _ := ExtractPairsFor(hmdfsProfile, []DAGVertex{write, rename}, nil)
	if len(hb) != 1 || hb[0].BPath != "merge_view/d/a" || hb[0].PathRel != PathSamePath {
		t.Fatalf("HMDFS rename pair = %+v, want source Same", hb)
	}
}

// TestRenameLookupRolePaths pins the lookup role rule: the lookup's resolved
// Path is the only role candidate. Its OldPath ambiguity must not reintroduce
// a rename-destination Same pair.
func TestRenameLookupRolePaths(t *testing.T) {
	for _, profile := range []*DFSProfile{hmdfsProfile, glusterfsProfile} {
		rename := DAGVertex{
			FuncID: FuncRename, CallName: "rename",
			Path: "dirA/b", OldPath: "dirA/a",
			RetBucket: RetSuccess, Stime: 100, Etime: 200, ProgIdx: 0,
		}
		lookupSource := DAGVertex{
			FuncID: FuncLookup, CallName: "lookup",
			Path: "dirA/a", OldPath: "dirA/b",
			RetBucket: RetSuccess, Stime: 300, Etime: 400, ProgIdx: 0,
		}
		hb, _ := ExtractPairsFor(profile, []DAGVertex{rename, lookupSource}, nil)
		if len(hb) != 1 {
			t.Fatalf("%s: rename+lookup source pairs = %d, want 1: %+v",
				profile.Name, len(hb), hb)
		}
		p := hb[0]
		if p.PathRel != PathSameParent || p.BPath != "dirA/a" {
			t.Fatalf("%s: rename+lookup source pair = %+v, want src Sibling",
				profile.Name, p)
		}

		lookupDest := lookupSource
		lookupDest.Path, lookupDest.OldPath = "dirA/b", "dirA/a"
		hb, _ = ExtractPairsFor(profile, []DAGVertex{rename, lookupDest}, nil)
		if len(hb) != 1 || hb[0].PathRel != PathSamePath || hb[0].BPath != "dirA/b" {
			t.Fatalf("%s: rename+lookup dest pair = %+v, want dst Same",
				profile.Name, hb)
		}
	}
}

// TestLinkRolePathTypeBit pins the pair-aware type override: a symlink whose
// target is a directory has a directory target role but a file linkpath role.
func TestLinkRolePathTypeBit(t *testing.T) {
	sym := DAGVertex{
		FuncID: FuncSymlink, CallName: "symlink",
		Path: "dirA/l2", OldPath: "dirA/a", IsDir: true,
		RetBucket: RetSuccess, Stime: 100, Etime: 200, ProgIdx: 0,
	}
	unlink := DAGVertex{
		FuncID: FuncUnlink, CallName: "unlink",
		Path: "dirA/l2", RetBucket: RetSuccess,
		Stime: 300, Etime: 400, ProgIdx: 0,
	}
	hb, _ := ExtractPairsFor(glusterfsProfile, []DAGVertex{sym, unlink}, nil)
	if len(hb) != 2 {
		t.Fatalf("symlink HB pairs = %d, want target+linkpath: %+v", len(hb), hb)
	}
	byPath := make(map[string]DAGPair, len(hb))
	for _, p := range hb {
		byPath[p.APath] = p
	}
	target, ok := byPath["dirA/a"]
	if !ok || target.APathType != pairPathTypeDir {
		t.Fatalf("target role = %+v, want directory type", target)
	}
	link, ok := byPath["dirA/l2"]
	if !ok || link.APathType != pairPathTypeFile {
		t.Fatalf("linkpath role = %+v, want file type", link)
	}
	tf := featuresOfPairPath(target.A, target.APath, "", target.APathType)
	lf := featuresOfPairPath(link.A, link.APath, "", link.APathType)
	if (tf>>10)&1 != 1 {
		t.Fatalf("target role type bit = %d, want dir", (tf>>10)&1)
	}
	if (lf>>10)&1 != 0 {
		t.Fatalf("linkpath role type bit = %d, want file", (lf>>10)&1)
	}
}
