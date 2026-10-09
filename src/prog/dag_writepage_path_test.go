package prog

import (
	"syscall"
	"testing"
)

func mkPathCall(t *testing.T, target *Target, name, path string, stime, etime uint64) *Call {
	t.Helper()
	meta := target.SyscallMap[name]
	ptrType := meta.Args[0].Type.(*PtrType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr}
	target.assignSizesCall(c)
	c.CheckInfo = &FileMetadata{Stime: stime, Etime: etime}
	return c
}

func mkRenameCall(t *testing.T, target *Target, oldPath, newPath string, stime, etime uint64) *Call {
	t.Helper()
	meta := target.SyscallMap["rename"]
	oldType := meta.Args[0].Type.(*PtrType)
	newType := meta.Args[1].Type.(*PtrType)
	oldArg := MakePointerArg(oldType, DirIn, 0,
		MakeDataArg(oldType.Elem, DirIn, []byte(oldPath+"\x00")))
	newArg := MakePointerArg(newType, DirIn, 0,
		MakeDataArg(newType.Elem, DirIn, []byte(newPath+"\x00")))
	c := MakeCall(meta, nil)
	c.Args = []Arg{oldArg, newArg}
	target.assignSizesCall(c)
	c.CheckInfo = &FileMetadata{Stime: stime, Etime: etime}
	return c
}

func writepageVertex(t *testing.T, vertices []DAGVertex) DAGVertex {
	t.Helper()
	for i := range vertices {
		if vertices[i].FuncID == FuncWritepage {
			return vertices[i]
		}
	}
	t.Fatalf("no writepage vertex in %d vertices", len(vertices))
	return DAGVertex{}
}

// TestWritepageDeletedFilePath pins the async-callback path resolution order:
// the node-local delete timeline is consulted before the merged cross-node ino
// map, and its keys are the same low-32 inoKey truncation as the fsMd maps.
func TestWritepageDeletedFilePath(t *testing.T) {
	target, err := GetTarget("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}

	// Node 0: the file was unlinked during the program, so the post-execution
	// fsMd no longer lists its ino. Node 1 happens to have an unrelated file
	// with the same low-32 raw ino (raw inos are node-local: local files use
	// the lower filesystem's ino, so cross-node value collisions are normal).
	unlinkCall := mkPathCall(t, target, "unlink", "merge_view/a", 100, 200)
	ps := []*Prog{{Calls: []*Call{unlinkCall}}}
	fsMds := []map[string]FileMetadata{
		{"./live": {StatMd: syscall.Stat_t{Ino: 111}}},
		{"./other": {StatMd: syscall.Stat_t{Ino: 0x2A}}},
	}
	events := []HmdfsTraceEvent{
		// kprobe unlink carries the full composed DOMAIN:dev_id:ino_raw ino.
		{ProgIdx: 0, FuncID: FuncUnlink, Timestamp: 150,
			Ino: 0x400000000000002A, Ret: 0},
		// The writepage tracepoint only carries i_ino&0xFFFFFFFF.
		{ProgIdx: 0, FuncID: FuncWritepage, Timestamp: 300, Ino: 0x2A},
	}
	vertices, _ := BuildVertices(events, ps, fsMds, nil, []int64{0})
	if got := writepageVertex(t, vertices).Path; got != "merge_view/a" {
		t.Fatalf("deleted writepage path = %q, want merge_view/a (merged map must not win)", got)
	}

	// Live file: the node-local map wins over the merged cross-node map.
	liveEvents := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncWritepage, Timestamp: 300, Ino: 0x2A},
	}
	liveMds := []map[string]FileMetadata{
		{"./live": {StatMd: syscall.Stat_t{Ino: 0x2A}}},
		{"./other": {StatMd: syscall.Stat_t{Ino: 0x2A}}},
	}
	vertices, _ = BuildVertices(liveEvents, []*Prog{{}}, liveMds, nil, []int64{0})
	if got := writepageVertex(t, vertices).Path; got != "merge_view/live" {
		t.Fatalf("live writepage path = %q, want merge_view/live", got)
	}

	// The unlink happened after the callback: deletePathAt's timestamp filter
	// must reject it. Node-local misses (node 0 lists a different ino), so the
	// branch runs and the merged fallback would be overridden by a delete path
	// if the filter ignored d.ts > event ts.
	lateCall := mkPathCall(t, target, "unlink", "merge_view/a", 350, 450)
	lateMds := []map[string]FileMetadata{
		{"./live": {StatMd: syscall.Stat_t{Ino: 111}}},
		{"./other": {StatMd: syscall.Stat_t{Ino: 0x2A}}},
	}
	lateEvents := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncUnlink, Timestamp: 400, Ino: 0x400000000000002A},
		{ProgIdx: 0, FuncID: FuncWritepage, Timestamp: 300, Ino: 0x2A},
	}
	vertices, _ = BuildVertices(lateEvents, []*Prog{{Calls: []*Call{lateCall}}}, lateMds, nil, []int64{0})
	if got := writepageVertex(t, vertices).Path; got != "merge_view/other" {
		t.Fatalf("delete-after-callback path = %q, want merge_view/other (delete ts filter)", got)
	}

	// No earlier rename in the round: renamePathAt returns "" and must not
	// wipe the live path (the old unconditional assignment dropped it).
	noRenameEvents := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncWritepage, Timestamp: 300, Ino: 0x2A},
	}
	noRenameVertices, _ := BuildVertices(noRenameEvents, []*Prog{{}}, liveMds, nil, []int64{0})
	if got := writepageVertex(t, noRenameVertices).Path; got != "merge_view/live" {
		t.Fatalf("no-rename live path = %q, want merge_view/live", got)
	}

	// Regression guard for the merged-map fallback with an applying rename:
	// the mapped path is rewritten to the post-rename path (passes on the old
	// code too; the R19 discriminators are the cases above).
	fallbackMds := []map[string]FileMetadata{
		{"./live": {StatMd: syscall.Stat_t{Ino: 111}}},
		{"./a": {StatMd: syscall.Stat_t{Ino: 0x2A}}},
	}
	fallbackRename := mkRenameCall(t, target, "merge_view/a", "merge_view/b", 250, 350)
	fallbackEvents := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncRename, Timestamp: 300,
			Ino: 0x4000000000000005, EntryIno: 0x400000000000002A, Ret: 0},
		{ProgIdx: 0, FuncID: FuncWritepage, Timestamp: 400, Ino: 0x2A},
	}
	vertices, _ = BuildVertices(fallbackEvents,
		[]*Prog{{Calls: []*Call{fallbackRename}}}, fallbackMds, nil, []int64{0})
	if got := writepageVertex(t, vertices).Path; got != "merge_view/b" {
		t.Fatalf("merged-map rename path = %q, want merge_view/b", got)
	}

	// Delete then rename of the same path string: the deleted file's path is a
	// historical snapshot and must not be rewritten to the new owner's path.
	unlinkCall = mkPathCall(t, target, "unlink", "merge_view/a", 100, 200)
	renameCall := mkRenameCall(t, target, "merge_view/a", "merge_view/b", 250, 350)
	renameEvents := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncUnlink, Timestamp: 150,
			Ino: 0x400000000000002A, Ret: 0},
		{ProgIdx: 0, FuncID: FuncRename, Timestamp: 300,
			Ino: 0x4000000000000005, EntryIno: 0x400000000000002A, Ret: 0},
		{ProgIdx: 0, FuncID: FuncWritepage, Timestamp: 400, Ino: 0x2A},
	}
	vertices, _ = BuildVertices(renameEvents,
		[]*Prog{{Calls: []*Call{unlinkCall, renameCall}}}, fsMds, nil, []int64{0})
	if got := writepageVertex(t, vertices).Path; got != "merge_view/a" {
		t.Fatalf("delete-then-rename path = %q, want merge_view/a", got)
	}
}
