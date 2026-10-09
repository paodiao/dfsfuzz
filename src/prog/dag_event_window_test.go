package prog

import (
	"syscall"
	"testing"
)

func dagEventTestTarget(t *testing.T) *Target {
	t.Helper()
	target, err := GetTarget("linux", "amd64")
	if err != nil {
		t.Skipf("no linux/amd64 target data: %v", err)
	}
	return target
}

func dagEventVertex(t *testing.T, vertices []DAGVertex, fid uint32) DAGVertex {
	t.Helper()
	for i := range vertices {
		if vertices[i].FuncID == fid {
			return vertices[i]
		}
	}
	t.Fatalf("no vertex with func %d", fid)
	return DAGVertex{}
}

// TestMatchEventToCallEventWindow pins the dual-endpoint admission: both the
// event's entry and return timestamps must fall inside the call window (within
// the tolerance), and the farther edge decides which window wins.
func TestMatchEventToCallEventWindow(t *testing.T) {
	target := dagEventTestTarget(t)
	c0 := mkPathCall(t, target, "unlink", "merge_view/a", 1000, 2000)
	c1 := mkPathCall(t, target, "unlink", "merge_view/b", 3000, 4000)
	p := &Prog{Target: target, Calls: []*Call{c0, c1}}
	tscoffs := []int64{0}

	// Fully inside call 1.
	ev := &HmdfsTraceEvent{FuncID: FuncUnlink, EntryTs: 3100, Timestamp: 3900}
	if got := matchEventToCall(ev, p, tscoffs); got != 1 {
		t.Fatalf("contained event matched call %d, want 1", got)
	}
	// Tolerance admission before call 1: entry 50 ticks early, exit 10.
	ev = &HmdfsTraceEvent{FuncID: FuncUnlink, EntryTs: 2950, Timestamp: 2990}
	if got := matchEventToCall(ev, p, tscoffs); got != 1 {
		t.Fatalf("early event matched call %d, want 1", got)
	}
	// Entry before call 0's end but exit just after: stays on call 0.
	ev = &HmdfsTraceEvent{FuncID: FuncUnlink, EntryTs: 1900, Timestamp: 2100}
	if got := matchEventToCall(ev, p, tscoffs); got != 0 {
		t.Fatalf("straddling event matched call %d, want 0", got)
	}
	// No same-type call.
	ev = &HmdfsTraceEvent{FuncID: FuncRename, EntryTs: 2100, Timestamp: 2200}
	if got := matchEventToCall(ev, p, tscoffs); got != -2 {
		t.Fatalf("no-func event = %d, want -2", got)
	}
	// Same-type call without CheckInfo.
	noCI := &Prog{Target: target, Calls: []*Call{{Meta: target.SyscallMap["unlink"]}}}
	ev = &HmdfsTraceEvent{FuncID: FuncUnlink, EntryTs: 1500, Timestamp: 1600}
	if got := matchEventToCall(ev, noCI, tscoffs); got != -3 {
		t.Fatalf("no-CheckInfo event = %d, want -3", got)
	}
	// Far outside every window.
	ev = &HmdfsTraceEvent{FuncID: FuncUnlink, EntryTs: 2000000, Timestamp: 2000001}
	if got := matchEventToCall(ev, p, tscoffs); got != -4 {
		t.Fatalf("far event = %d, want -4", got)
	}
}

// TestCreatCreateOpenHBPair pins the vertex time basis: an event's window is
// its own [entry, return] interval, so the create and file-open events of one
// creat call no longer share the syscall window and form a real HB pair.
func TestCreatCreateOpenHBPair(t *testing.T) {
	target := dagEventTestTarget(t)
	creat := mkPathCall(t, target, "creat", "merge_view/a", 1000, 5000)
	p := &Prog{Target: target, Calls: []*Call{creat}}
	fsMds := []map[string]FileMetadata{{"./a": {StatMd: syscall.Stat_t{Ino: 0x2A}}}}
	events := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncCreate, EntryTs: 1100, Timestamp: 2000, Ret: 0},
		{ProgIdx: 0, FuncID: FuncFileOpen, EntryTs: 3000, Timestamp: 4000, Ret: 0},
	}
	vertices, _ := BuildVertices(events, []*Prog{p}, fsMds, nil, []int64{0})
	cr := dagEventVertex(t, vertices, FuncCreate)
	fo := dagEventVertex(t, vertices, FuncFileOpen)
	if cr.Stime != 1100 || cr.Etime != 2000 || fo.Stime != 3000 || fo.Etime != 4000 {
		t.Fatalf("vertex windows = [%d,%d]/[%d,%d], want event windows",
			cr.Stime, cr.Etime, fo.Stime, fo.Etime)
	}
	hb, cc := ExtractPairs(vertices, &DagDiag{})
	found := false
	for _, pr := range hb {
		if pr.A.FuncID == FuncCreate && pr.B.FuncID == FuncFileOpen &&
			pr.Temporal == TemporalHB && pr.PathRel == PathSamePath {
			found = true
		}
	}
	if !found {
		t.Fatalf("create->file_open not an HB pair (hb=%d cc=%d)", len(hb), len(cc))
	}
}

// TestEntryTsFallbackUsesCallWindow pins the graceful degradation: an event
// without a captured entry timestamp keeps the enclosing call window.
func TestEntryTsFallbackUsesCallWindow(t *testing.T) {
	target := dagEventTestTarget(t)
	creat := mkPathCall(t, target, "creat", "merge_view/a", 1000, 5000)
	p := &Prog{Target: target, Calls: []*Call{creat}}
	fsMds := []map[string]FileMetadata{{"./a": {StatMd: syscall.Stat_t{Ino: 0x2A}}}}
	events := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncCreate, EntryTs: 0, Timestamp: 2000, Ret: 0},
	}
	vertices, _ := BuildVertices(events, []*Prog{p}, fsMds, nil, []int64{0})
	cr := dagEventVertex(t, vertices, FuncCreate)
	if cr.Stime != 1000 || cr.Etime != 5000 {
		t.Fatalf("fallback window = [%d,%d], want call window [1000,5000]", cr.Stime, cr.Etime)
	}
}

// TestRenamePathAtEntryCutoff pins the fd-path rename rewrite cutoff: the
// reference time is the event's own window start, so a rename completed after
// the operation began does not rewrite its path.
func TestRenamePathAtEntryCutoff(t *testing.T) {
	target := dagEventTestTarget(t)
	openMeta := target.SyscallMap["open"]
	openCall := mkOpen(target, "merge_view/a")
	openCall.Ret = MakeResultArg(openMeta.Ret, DirOut, nil, 0)
	openCall.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
	ren := mkRenameCall(t, target, "merge_view/a", "merge_view/b", 300, 400)
	pw := mkPwrite64(target, openCall, 8, 0)
	pw.CheckInfo = &FileMetadata{Stime: 500, Etime: 600}
	p := &Prog{Target: target, Calls: []*Call{openCall, ren, pw}}
	fsMds := []map[string]FileMetadata{{"./a": {StatMd: syscall.Stat_t{Ino: 0x2A}}}}

	events := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncRename, EntryTs: 310, Timestamp: 390,
			Ino: 0x4000000000000005, EntryIno: 0x400000000000002A, Ret: 0},
		{ProgIdx: 0, FuncID: FuncWrite, EntryTs: 510, Timestamp: 590},
	}
	vertices, _ := BuildVertices(events, []*Prog{p}, fsMds, nil, []int64{0})
	if got := dagEventVertex(t, vertices, FuncWrite).Path; got != "merge_view/b" {
		t.Fatalf("post-rename write path = %q, want merge_view/b", got)
	}

	// Same execution, but the write began before the rename completed.
	events[1].EntryTs, events[1].Timestamp = 250, 290
	vertices, _ = BuildVertices(events, []*Prog{p}, fsMds, nil, []int64{0})
	if got := dagEventVertex(t, vertices, FuncWrite).Path; got != "merge_view/a" {
		t.Fatalf("pre-rename write path = %q, want merge_view/a", got)
	}

	// Overlap: the rename completes inside the write's window. The path
	// snapshot is the operation's start (entry), so the write keeps the old
	// path even though the rename had completed by its return.
	events[1].EntryTs, events[1].Timestamp = 350, 450
	vertices, _ = BuildVertices(events, []*Prog{p}, fsMds, nil, []int64{0})
	if got := dagEventVertex(t, vertices, FuncWrite).Path; got != "merge_view/a" {
		t.Fatalf("overlapping-rename write path = %q, want merge_view/a (entry cutoff)", got)
	}
}

// TestRenameThenRecreateKeepsArgPath pins the argument-path scope: a path
// re-created after it was renamed away is a different file and must not be
// rewritten to the rename destination.
func TestRenameThenRecreateKeepsArgPath(t *testing.T) {
	target := dagEventTestTarget(t)
	ren := mkRenameCall(t, target, "merge_view/a", "merge_view/b", 100, 200)
	creat := mkPathCall(t, target, "creat", "merge_view/a", 300, 400)
	p := &Prog{Target: target, Calls: []*Call{ren, creat}}
	fsMds := []map[string]FileMetadata{{"./a": {StatMd: syscall.Stat_t{Ino: 0x2A}}}}
	events := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncRename, EntryTs: 110, Timestamp: 190,
			Ino: 0x4000000000000005, EntryIno: 0x400000000000002A, Ret: 0},
		{ProgIdx: 0, FuncID: FuncCreate, EntryTs: 310, Timestamp: 390, Ret: 0},
		{ProgIdx: 0, FuncID: FuncFileOpen, EntryTs: 320, Timestamp: 395, Ret: 0},
	}
	vertices, _ := BuildVertices(events, []*Prog{p}, fsMds, nil, []int64{0})
	for _, fid := range []uint32{FuncCreate, FuncFileOpen} {
		if got := dagEventVertex(t, vertices, fid).Path; got != "merge_view/a" {
			t.Fatalf("func %d path = %q, want merge_view/a (recreated path must stay)", fid, got)
		}
	}
}

// TestLookupPathRootParent pins the merge-view root special case: the root is
// absent from fsMd, so a root-parent lookup resolves to the first component.
func TestLookupPathRootParent(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkPathCall(t, target, "stat", "merge_view/dirA/file", 100, 200)
	empty := map[int]map[uint64]string{0: {}}
	ev := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup, Ino: hmdfsRootMergeIno,
		EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(ev, call, empty, nil); got != "merge_view/dirA" || alt != "" {
		t.Fatalf("root-parent negative lookup = %q/%q, want merge_view/dirA", got, alt)
	}
	// Positive intermediate lookup: EntryIno resolves the looked-up directory.
	ev.EntryIno = 0x400000000000002A
	maps := map[int]map[uint64]string{0: {0x2A: "merge_view/dirA"}}
	if got, alt := lookupPath(ev, call, maps, nil); got != "merge_view/dirA" || alt != "" {
		t.Fatalf("root-parent positive lookup = %q/%q, want merge_view/dirA", got, alt)
	}
}

// TestLookupPathRenameSrcDst pins the rename two-component rule: the positive
// lookup is the source, the negative one the destination (same parent); the
// alternative candidate carries the other side for the relation classifier.
func TestLookupPathRenameSrcDst(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkRenameCall(t, target, "merge_view/dirA/a", "merge_view/dirA/b", 100, 200)
	maps := map[int]map[uint64]string{0: {0x2A: "merge_view/dirA"}}
	neg := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup,
		Ino: 0x400000000000002A, EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(neg, call, maps, nil); got != "merge_view/dirA/b" || alt != "merge_view/dirA/a" {
		t.Fatalf("negative rename lookup = %q/%q, want dirA/b + alt dirA/a", got, alt)
	}
	pos := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup,
		Ino: 0x400000000000002A, EntryIno: 0x4000000000000077, EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(pos, call, maps, nil); got != "merge_view/dirA/a" || alt != "merge_view/dirA/b" {
		t.Fatalf("positive rename lookup = %q/%q, want dirA/a + alt dirA/b", got, alt)
	}
}

// TestLookupPathRenameIntermediate pins the component gating: a rename's
// intermediate lookups (parent side matches neither final parent) resolve via
// the generic component chain instead of the source/destination rule.
func TestLookupPathRenameIntermediate(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkRenameCall(t, target, "merge_view/dirA/sub/x", "merge_view/dirA/sub/y", 100, 200)
	maps := map[int]map[uint64]string{0: {
		0x2A: "merge_view/dirA",
		0x2B: "merge_view/dirA/sub",
	}}
	// dirA lookup under the merge-view root.
	ev := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup, Ino: hmdfsRootMergeIno,
		EntryIno: 0x400000000000002A, EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(ev, call, maps, nil); got != "merge_view/dirA" || alt != "" {
		t.Fatalf("rename intermediate lookup = %q/%q, want merge_view/dirA", got, alt)
	}
	// sub lookup under dirA.
	ev = &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup, Ino: 0x400000000000002A,
		EntryIno: 0x400000000000002B, EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(ev, call, maps, nil); got != "merge_view/dirA/sub" || alt != "" {
		t.Fatalf("rename nested intermediate lookup = %q/%q, want dirA/sub", got, alt)
	}
}

// TestLookupPathRenameCrossParent pins the parent-side rule for cross-parent
// renames: the event's parent inode identifies the source or destination even
// when both final components resolve to different directories.
func TestLookupPathRenameCrossParent(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkRenameCall(t, target, "merge_view/dirA/x", "merge_view/dirB/y", 100, 200)
	maps := map[int]map[uint64]string{0: {
		0x2A: "merge_view/dirA",
		0x2B: "merge_view/dirB",
	}}
	src := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup, Ino: 0x400000000000002A,
		EntryIno: 0x4000000000000077, EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(src, call, maps, nil); got != "merge_view/dirA/x" || alt != "" {
		t.Fatalf("cross-parent source lookup = %q/%q, want dirA/x", got, alt)
	}
	dst := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup, Ino: 0x400000000000002B,
		EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(dst, call, maps, nil); got != "merge_view/dirB/y" || alt != "" {
		t.Fatalf("cross-parent dest lookup = %q/%q, want dirB/y", got, alt)
	}
}

// TestLookupPathRenameMissingSource pins the directed-ENOENT case: the root
// unlink removes the rename source, so the negative lookup is the source and
// the pair must stay SamePath (the alternative candidate preserves it).
func TestLookupPathRenameMissingSource(t *testing.T) {
	target := dagEventTestTarget(t)
	un := mkPathCall(t, target, "unlink", "merge_view/dirA/a", 100, 200)
	ren := mkRenameCall(t, target, "merge_view/dirA/a", "merge_view/dirA/b", 300, 400)
	p := &Prog{Target: target, Calls: []*Call{un, ren}}
	fsMds := []map[string]FileMetadata{{"./dirA": {StatMd: syscall.Stat_t{Ino: 0x2A}}}}
	events := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncUnlink, Ino: 0x400000000000002A,
			EntryIno: 0x4000000000000077, EntryTs: 110, Timestamp: 190, Ret: 0},
		{ProgIdx: 0, FuncID: FuncLookup, Ino: 0x400000000000002A,
			EntryTs: 310, Timestamp: 390, Ret: -2},
	}
	vertices, _ := BuildVertices(events, []*Prog{p}, fsMds, nil, []int64{0})
	lv := dagEventVertex(t, vertices, FuncLookup)
	if lv.Path != "merge_view/dirA/b" || lv.OldPath != "merge_view/dirA/a" {
		t.Fatalf("missing-source lookup = %q/%q, want dirA/b + alt dirA/a", lv.Path, lv.OldPath)
	}
	hb, cc := ExtractPairs(vertices, &DagDiag{})
	for _, pr := range hb {
		if pr.A.FuncID == FuncUnlink && pr.B.FuncID == FuncLookup && pr.PathRel == PathSamePath {
			return
		}
	}
	t.Fatalf("unlink->lookup missing-source pair not SamePath (hb=%d cc=%d)", len(hb), len(cc))
}

// TestLookupPathRenamePosDst pins the overwrite case: the destination lookup
// is positive too, and the alternative candidate keeps it SamePath with a
// vertex at the destination.
func TestLookupPathRenamePosDst(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkRenameCall(t, target, "merge_view/dirA/a", "merge_view/dirA/b", 100, 200)
	maps := map[int]map[uint64]string{0: {0x2A: "merge_view/dirA"}}
	pos := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup,
		Ino: 0x400000000000002A, EntryIno: 0x4000000000000099, EntryTs: 110, Timestamp: 190}
	got, alt := lookupPath(pos, call, maps, nil)
	if got != "merge_view/dirA/a" || alt != "merge_view/dirA/b" {
		t.Fatalf("overwrite dst lookup = %q/%q, want dirA/a + alt dirA/b", got, alt)
	}
	lv := DAGVertex{FuncID: FuncLookup, Path: got, OldPath: alt, CallName: "rename", RetBucket: RetSuccess}
	ov := DAGVertex{FuncID: FuncFileOpen, Path: "merge_view/dirA/b", CallName: "open", RetBucket: RetSuccess}
	if rel := determinePathRel(&ov, &lv, false); rel != PathSamePath {
		t.Fatalf("dst vertex relation = %d, want PathSamePath", rel)
	}
}

// TestLookupPathPositiveComponent pins non-rename positive resolution and the
// later-rename guard: an EntryIno hit is only accepted when it is a component
// of the call's own path.
func TestLookupPathPositiveComponent(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkPathCall(t, target, "open", "merge_view/dirA/file", 100, 200)
	maps := map[int]map[uint64]string{0: {
		0x2A: "merge_view/dirA",
		0x77: "merge_view/dirA/file",
	}}
	ev := &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup,
		Ino: 0x400000000000002A, EntryIno: 0x4000000000000077, EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(ev, call, maps, nil); got != "merge_view/dirA/file" || alt != "" {
		t.Fatalf("positive lookup = %q/%q, want merge_view/dirA/file", got, alt)
	}
	// The child inode now lives at a different path (renamed after the
	// lookup): the reverse lookup is rejected, the component chain wins.
	moved := map[int]map[uint64]string{0: {
		0x2A: "merge_view/dirA",
		0x77: "merge_view/dirA/renamed",
	}}
	if got, alt := lookupPath(ev, call, moved, nil); got != "merge_view/dirA/file" || alt != "" {
		t.Fatalf("post-rename positive lookup = %q/%q, want merge_view/dirA/file", got, alt)
	}
	// Intermediate component under the root.
	ev = &HmdfsTraceEvent{ProgIdx: 0, FuncID: FuncLookup,
		Ino: hmdfsRootMergeIno, EntryIno: 0x400000000000002A, EntryTs: 110, Timestamp: 190}
	if got, alt := lookupPath(ev, call, maps, nil); got != "merge_view/dirA" || alt != "" {
		t.Fatalf("intermediate lookup = %q/%q, want merge_view/dirA", got, alt)
	}
}
