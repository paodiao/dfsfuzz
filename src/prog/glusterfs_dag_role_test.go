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

// TestGlusterfsRenamePairStillUsesLegacyPath pins the deliberate scope of the
// role-path patch: rename-involved pairs keep the legacy union relation and
// dst-based cell path until the src/dst semantics are decided separately.
func TestGlusterfsRenamePairStillUsesLegacyPath(t *testing.T) {
	write := DAGVertex{
		FuncID: FuncWrite, CallName: "write",
		Path: "dirA/a", RetBucket: RetSuccess,
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
	if p.APath != "" || p.BPath != "" {
		t.Fatalf("rename pair unexpectedly got role paths: %+v", p)
	}
	ck, ok := cellKeyOf(&p, "")
	if !ok {
		t.Fatal("cellKeyOf failed for rename pair")
	}
	if ck.DepthB != DepthBucketOf("dirA/b") {
		t.Fatalf("rename DepthB = %d, want destination bucket %d", ck.DepthB, DepthBucketOf("dirA/b"))
	}
}

// TestHmdfsLinkPairsKeepLegacySinglePair pins the profile gate: the dual-role
// extractor is GlusterFS-only, so an HMDFS link-shaped vertex keeps the old
// single-pair behavior.
func TestHmdfsLinkPairsKeepLegacySinglePair(t *testing.T) {
	link := DAGVertex{
		FuncID: FuncLink, CallName: "link",
		Path: "merge_view/d/l1", OldPath: "merge_view/d/a",
		RetBucket: RetSuccess, Stime: 100, Etime: 200, ProgIdx: 0,
	}
	unlink := DAGVertex{
		FuncID: FuncUnlink, CallName: "unlink",
		Path: "merge_view/d/l1", RetBucket: RetSuccess,
		Stime: 300, Etime: 400, ProgIdx: 0,
	}
	hb, cc := ExtractPairsFor(hmdfsProfile, []DAGVertex{link, unlink}, nil)
	if len(cc) != 0 || len(hb) != 1 {
		t.Fatalf("pairs = hb %d / cc %d, want 1/0", len(hb), len(cc))
	}
	if hb[0].PathRel != PathSamePath || hb[0].BPath != "" {
		t.Fatalf("HMDFS pair = %+v, want legacy linkpath Same pair", hb[0])
	}
}
