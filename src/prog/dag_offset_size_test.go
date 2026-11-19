package prog

import (
	"syscall"
	"testing"
)

func offsetSizeTree() *Hmdfs_config {
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA", NodeTypeDir, "c1")
	n := ft.AddNode("dirA/a", NodeTypeFile, "c1")
	n.Size = 4096
	return &Hmdfs_config{Profile: glusterfsProfile, FileTree: ft}
}

func TestGlusterfsOffsetSizeUsesBaselineTree(t *testing.T) {
	hmcfg := offsetSizeTree()
	if got := glusterfsOffsetSize(hmcfg, "dirA/a"); got != 4096 {
		t.Fatalf("baseline size = %d, want 4096", got)
	}
	if got := glusterfsOffsetSize(hmcfg, "dirA/new"); got != 0 {
		t.Fatalf("fresh path size = %d, want 0", got)
	}
	if got := glusterfsOffsetSize(nil, "dirA/a"); got != 0 {
		t.Fatalf("nil config size = %d, want 0", got)
	}
	if got := glusterfsOffsetSize(&Hmdfs_config{FileTree: hmcfg.FileTree}, ""); got != 0 {
		t.Fatalf("empty path size = %d, want 0", got)
	}
}

func TestGlusterfsOffsetBucketUsesTreeSize(t *testing.T) {
	hmcfg := offsetSizeTree()
	v := &DAGVertex{FuncID: FuncWrite, CallName: "write", Off: 100}
	v.Size = glusterfsOffsetSize(hmcfg, "dirA/a")
	if got := offsetBucketOf(v); got != offsetBucketMid {
		t.Fatalf("baseline write bucket = %d, want Mid", got)
	}
	v.Size = 1 // the GlusterFS request length; must not be used
	if got := offsetBucketOf(v); got != offsetBucketBeyond {
		t.Fatalf("request-length write bucket = %d, want Beyond", got)
	}
}

func TestGlusterfsOffsetBucketNewFileZero(t *testing.T) {
	hmcfg := offsetSizeTree()
	v := &DAGVertex{FuncID: FuncWrite, CallName: "write", Off: 100}
	v.Size = glusterfsOffsetSize(hmcfg, "dirA/new")
	if v.Size != 0 {
		t.Fatalf("fresh path size = %d, want 0", v.Size)
	}
	if got := offsetBucketOf(v); got != offsetBucketBeyond {
		t.Fatalf("fresh path bucket = %d, want Beyond", got)
	}
}

func TestGlusterfsOffsetBucketTruncateUsesTreeSize(t *testing.T) {
	hmcfg := offsetSizeTree()
	v := &DAGVertex{FuncID: FuncSetattr, CallName: "truncate", Off: 100}
	v.Size = glusterfsOffsetSize(hmcfg, "dirA/a")
	if got := offsetBucketOf(v); got != offsetBucketMid {
		t.Fatalf("baseline truncate bucket = %d, want Mid", got)
	}
	v.Size = 0 // the trace's truncate size; must not be used
	if got := offsetBucketOf(v); got != offsetBucketBeyond {
		t.Fatalf("zero-size truncate bucket = %d, want Beyond", got)
	}
}

// TestGlusterfsOffsetBucketUsesTreeSizeNotFsMd pins the size source at the
// BuildVertices boundary: fsMd is post-execution (8192) and the baseline tree
// is 4096. A truncate at 5000 is Beyond with the tree size but Mid with fsMd;
// the vertex must carry the tree size.
func TestGlusterfsOffsetBucketUsesTreeSizeNotFsMd(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkPathCall(t, target, "truncate", "dirA/a", 1000, 2000)
	p := &Prog{Target: target, Calls: []*Call{call}}
	hmcfg := offsetSizeTree()
	fsMds := []map[string]FileMetadata{{"./dirA/a": {
		StatMd: syscall.Stat_t{Ino: 0x2A, Size: 8192},
	}}}
	events := []HmdfsTraceEvent{{
		ProgIdx: 0, FuncID: FuncSetattr, Off: 5000,
		EntryTs: 1100, Timestamp: 1900,
	}}
	vertices, _ := BuildVertices(events, []*Prog{p}, fsMds, hmcfg, []int64{0})
	v := dagEventVertex(t, vertices, FuncSetattr)
	if v.Size != 4096 {
		t.Fatalf("vertex size = %d, want baseline FileTree 4096 (fsMd 8192 must be ignored)", v.Size)
	}
	if got := offsetBucketOf(&v); got != offsetBucketBeyond {
		t.Fatalf("bucket = %d, want Beyond from baseline 4096 at off 5000", got)
	}
}
