package prog

import (
	"syscall"
	"testing"
)

// TestEventIsDir covers the event-time type decision: i_mode wins, then
// the function's unambiguous entry type, then the post-execution fallback.
func TestEventIsDir(t *testing.T) {
	dirMode := uint32(syscall.S_IFDIR | 0o755)
	fileMode := uint32(syscall.S_IFREG | 0o644)
	cases := []struct {
		name     string
		ev       HmdfsTraceEvent
		fallback bool
		want     bool
	}{
		{"mode dir", HmdfsTraceEvent{FuncID: FuncGetattr, Mode: dirMode}, false, true},
		{"mode file overrides fallback", HmdfsTraceEvent{FuncID: FuncGetattr, Mode: fileMode}, true, false},
		{"no mode rmdir is dir", HmdfsTraceEvent{FuncID: FuncRmdir}, false, true},
		{"no mode create is file", HmdfsTraceEvent{FuncID: FuncCreate}, true, false},
		{"no mode getattr falls back", HmdfsTraceEvent{FuncID: FuncGetattr}, true, true},
	}
	for _, c := range cases {
		if got := eventIsDir(&c.ev, c.fallback); got != c.want {
			t.Fatalf("%s: eventIsDir = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFuncDefaultIsDir pins the unambiguous function types.
func TestFuncDefaultIsDir(t *testing.T) {
	dirFns := []uint32{FuncMkdir, FuncRmdir, FuncDirOpen, FuncIterate}
	fileFns := []uint32{FuncCreate, FuncUnlink, FuncWrite, FuncRead,
		FuncFileOpen, FuncFsync, FuncWritepage}
	for _, fid := range dirFns {
		if d, known := funcDefaultIsDir(fid); !known || !d {
			t.Fatalf("func %d: default dir = %v/%v, want true/true", fid, d, known)
		}
	}
	for _, fid := range fileFns {
		if d, known := funcDefaultIsDir(fid); !known || d {
			t.Fatalf("func %d: default dir = %v/%v, want false/true", fid, d, known)
		}
	}
	for _, fid := range []uint32{FuncRename, FuncGetattr, FuncSetattr, FuncRelease, FuncLookup} {
		if _, known := funcDefaultIsDir(fid); known {
			t.Fatalf("func %d: type should be unknown", fid)
		}
	}
}

// TestVertexIno covers the per-function inode rule: parent-inode functions
// use EntryIno exclusively (0 stays 0, never the parent), all others keep
// the event's Ino.
func TestVertexIno(t *testing.T) {
	for _, fid := range []uint32{FuncMkdir, FuncCreate, FuncRename, FuncLookup} {
		if !isEntryInoFunc(fid) {
			t.Fatalf("func %d: want entry-ino function", fid)
		}
		ev := HmdfsTraceEvent{FuncID: fid, Ino: 111, EntryIno: 0}
		if got := vertexIno(&ev); got != 0 {
			t.Fatalf("func %d: vertexIno with unknown entry = %d, want 0", fid, got)
		}
		ev.EntryIno = 222
		if got := vertexIno(&ev); got != 222 {
			t.Fatalf("func %d: vertexIno = %d, want 222", fid, got)
		}
	}
	for _, fid := range []uint32{FuncRmdir, FuncUnlink, FuncWrite, FuncRead, FuncGetattr, FuncSetattr} {
		if isEntryInoFunc(fid) {
			t.Fatalf("func %d: must not be an entry-ino function", fid)
		}
		ev := HmdfsTraceEvent{FuncID: fid, Ino: 333, EntryIno: 0}
		if got := vertexIno(&ev); got != 333 {
			t.Fatalf("func %d: vertexIno = %d, want 333", fid, got)
		}
	}
}

// TestExtractPairsRelHist verifies the produced-pair PathRel histogram:
// same-parent and same-path pairs are counted under their own relation, and
// SameInode is node-scoped (equal inode numbers on different nodes are
// independent namespaces and must not be reported as the same file).
func TestExtractPairsRelHist(t *testing.T) {
	mk := func(path string, ino uint64, progIdx int) DAGVertex {
		return DAGVertex{
			FuncID:    FuncWrite,
			RetBucket: RetSuccess,
			Path:      path,
			Ino:       ino,
			Stime:     100,
			Etime:     200,
			ProgIdx:   progIdx,
		}
	}
	diag := &DagDiag{}
	ExtractPairs([]DAGVertex{mk("merge_view/d/a", 1, 0), mk("merge_view/d/b", 2, 0)}, diag)
	if diag.RelHist[PathSameParent] != 1 {
		t.Fatalf("same-parent rel = %d, want 1", diag.RelHist[PathSameParent])
	}
	diag = &DagDiag{}
	ExtractPairs([]DAGVertex{mk("merge_view/d/a", 1, 0), mk("merge_view/d/a", 1, 0)}, diag)
	if diag.RelHist[PathSamePath] != 1 {
		t.Fatalf("same-path rel = %d, want 1", diag.RelHist[PathSamePath])
	}
	// Equal inode number on the same node: a real alias (hardlink/rename).
	diag = &DagDiag{}
	ExtractPairs([]DAGVertex{mk("merge_view/d/a", 7, 0), mk("merge_view/d/b", 7, 0)}, diag)
	if diag.RelHist[PathSameInode] != 1 {
		t.Fatalf("same-node same-ino rel = %d, want 1", diag.RelHist[PathSameInode])
	}
	// Equal inode number on different nodes: independent namespaces, so the
	// pair keeps its geometric SameParent relation and is not SameInode.
	diag = &DagDiag{}
	ExtractPairs([]DAGVertex{mk("merge_view/d/a", 7, 0), mk("merge_view/d/b", 7, 1)}, diag)
	if diag.RelHist[PathSameInode] != 0 || diag.RelHist[PathSameParent] != 1 {
		t.Fatalf("cross-node same-ino rel = sameInode %d / sameParent %d, want 0/1",
			diag.RelHist[PathSameInode], diag.RelHist[PathSameParent])
	}
}

// TestDagCallTypeInoDiag covers the per-call-type diagnostic marginals:
// the offset bucket per offset-call row (entry semantics, gated to offset
// functions), the event-time type per FuncID, and the entry-ino capture
// mode of the namespace functions.
func TestDagCallTypeInoDiag(t *testing.T) {
	diag := &DagDiag{}

	// pwrite64 beyond EOF: entry Off=100 > entry Size=50 -> beyond bucket.
	ev := HmdfsTraceEvent{FuncID: FuncWrite, Ino: 1}
	v := DAGVertex{FuncID: FuncWrite, CallName: "pwrite64", Off: 100, Size: 50}
	recordCallTypeIno(diag, &ev, &v)
	if got := diag.OffByCall[2][offsetBucketBeyond]; got != 1 {
		t.Fatalf("pwrite64 beyond = %d, want 1", got)
	}
	if diag.TypeByFunc[FuncWrite][0] != 1 {
		t.Fatalf("write type file = %d, want 1", diag.TypeByFunc[FuncWrite][0])
	}

	// A LOOKUP event produced by a truncate call's own path resolution must
	// not enter the truncate row: only offset functions are counted.
	before := diag.OffByCall
	ev = HmdfsTraceEvent{FuncID: FuncLookup, Ino: 5}
	v = DAGVertex{FuncID: FuncLookup, CallName: "truncate"}
	recordCallTypeIno(diag, &ev, &v)
	if diag.OffByCall != before {
		t.Fatalf("lookup event polluted an offset-call row: %v", diag.OffByCall)
	}

	// A real truncate event (FuncSetattr) does enter the truncate row.
	ev = HmdfsTraceEvent{FuncID: FuncSetattr, Ino: 5}
	v = DAGVertex{FuncID: FuncSetattr, CallName: "truncate", Off: 0, Size: 0}
	recordCallTypeIno(diag, &ev, &v)
	if got := diag.OffByCall[4][offsetBucketZero]; got != 1 {
		t.Fatalf("truncate zero = %d, want 1", got)
	}

	// chmod shares FuncSetattr but the call-name gate keeps it NA.
	ev = HmdfsTraceEvent{FuncID: FuncSetattr, Ino: 5}
	v = DAGVertex{FuncID: FuncSetattr, CallName: "chmod", Off: 0, Size: 0}
	recordCallTypeIno(diag, &ev, &v)
	if got := diag.OffByCall[5][offsetBucketNA]; got != 1 {
		t.Fatalf("chmod NA = %d, want 1", got)
	}

	// mkdir: entry ino differs from the parent ino held by Ino; event type dir.
	ev = HmdfsTraceEvent{FuncID: FuncMkdir, Ino: 111, EntryIno: 222, Ret: 0}
	v = DAGVertex{FuncID: FuncMkdir, CallName: "mkdir", IsDir: true}
	recordCallTypeIno(diag, &ev, &v)
	if diag.TypeByFunc[FuncMkdir][1] != 1 {
		t.Fatalf("mkdir type dir = %d, want 1", diag.TypeByFunc[FuncMkdir][1])
	}
	if diag.InoByFunc[FuncMkdir][2] != 1 {
		t.Fatalf("mkdir entry-ino ne = %d, want 1", diag.InoByFunc[FuncMkdir][2])
	}
	if diag.InoByFunc[FuncMkdir][1] != 0 {
		t.Fatalf("mkdir successful entry-ino zeroOK = %d, want 0", diag.InoByFunc[FuncMkdir][1])
	}

	// A successful create with no entry inode is the capture-failure alarm.
	ev = HmdfsTraceEvent{FuncID: FuncCreate, Ino: 111, EntryIno: 0, Ret: 0}
	v = DAGVertex{FuncID: FuncCreate, CallName: "open"}
	recordCallTypeIno(diag, &ev, &v)
	if diag.InoByFunc[FuncCreate][0] != 1 || diag.InoByFunc[FuncCreate][1] != 1 {
		t.Fatalf("create zero/zeroOK = %d/%d, want 1/1",
			diag.InoByFunc[FuncCreate][0], diag.InoByFunc[FuncCreate][1])
	}

	// Negative lookup: no entry inode captured, but zeroOK stays 0 because
	// the operation failed (a negative dentry is the correct answer).
	ev = HmdfsTraceEvent{FuncID: FuncLookup, Ino: 111, EntryIno: 0, Ret: -2}
	v = DAGVertex{FuncID: FuncLookup, CallName: "stat"}
	recordCallTypeIno(diag, &ev, &v)
	if diag.InoByFunc[FuncLookup][0] != 1 || diag.InoByFunc[FuncLookup][1] != 0 {
		t.Fatalf("lookup zero/zeroOK = %d/%d, want 1/0",
			diag.InoByFunc[FuncLookup][0], diag.InoByFunc[FuncLookup][1])
	}

	// EntryIno == Ino is the rare third bucket.
	ev = HmdfsTraceEvent{FuncID: FuncRename, Ino: 7, EntryIno: 7}
	v = DAGVertex{FuncID: FuncRename, CallName: "rename"}
	recordCallTypeIno(diag, &ev, &v)
	if diag.InoByFunc[FuncRename][3] != 1 {
		t.Fatalf("rename entry-ino eq = %d, want 1", diag.InoByFunc[FuncRename][3])
	}

	// Non-offset calls and empty names do not enter OffByCall.
	before = diag.OffByCall
	ev = HmdfsTraceEvent{FuncID: FuncFsync}
	v = DAGVertex{FuncID: FuncFsync, CallName: "fsync"}
	recordCallTypeIno(diag, &ev, &v)
	v = DAGVertex{FuncID: FuncFsync}
	recordCallTypeIno(diag, &ev, &v)
	if diag.OffByCall != before {
		t.Fatalf("OffByCall changed for non-offset calls: %v", diag.OffByCall)
	}
}
