package prog

import "testing"

// TestBuildVerticesDropsTruncatedPaths pins P2-T2: the GlusterFS wire event
// carries a truncated bitmap. A truncated path or path2 makes the vertex path
// unsafe, so the event must not enter the DAG; a truncated xattr name only
// affects the xattr key and must not discard the whole event.
func TestBuildVerticesDropsTruncatedPaths(t *testing.T) {
	target := dagEventTestTarget(t)
	call := mkPathCall(t, target, "truncate", "dirA/a", 1000, 2000)
	p := &Prog{Target: target, Calls: []*Call{call}}
	hmcfg := offsetSizeTree()
	events := []HmdfsTraceEvent{
		{ProgIdx: 0, FuncID: FuncSetattr, Off: 100, EntryTs: 1100, Timestamp: 1900, Truncated: 1},
		{ProgIdx: 0, FuncID: FuncSetattr, Off: 100, EntryTs: 1100, Timestamp: 1900, Truncated: 2},
		{ProgIdx: 0, FuncID: FuncSetattr, Off: 100, EntryTs: 1100, Timestamp: 1900, Truncated: 4},
	}
	vertices, diag := BuildVertices(events, []*Prog{p}, nil, hmcfg, []int64{0})
	if diag.PathTruncated != 2 {
		t.Fatalf("PathTruncated = %d, want 2", diag.PathTruncated)
	}
	if len(vertices) != 1 {
		t.Fatalf("vertices = %d, want 1 (only the xattr-bit event)", len(vertices))
	}
	if vertices[0].FuncID != FuncSetattr {
		t.Fatalf("vertex func = %d, want FuncSetattr", vertices[0].FuncID)
	}
}
