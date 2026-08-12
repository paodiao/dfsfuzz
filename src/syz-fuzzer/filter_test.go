package main

import (
	"testing"

	"monarch/pkg/ipc"
	"monarch/pkg/signal"
	"monarch/prog"
)

// TestFilterNewDagPairsDedup verifies that filterNewDagPairs collapses pairs
// sharing the same hash (identical abstract features) to one entry: feedback
// counts pair types, not instances.
func TestFilterNewDagPairsDedup(t *testing.T) {
	const h1 = uint32(111)
	const h2 = uint32(222)
	info := &ipc.ProgInfo{
		DagSignal: []uint32{h1, h1, h2, h1},
		DagPairs: []prog.DAGPair{
			{A: &prog.DAGVertex{}, B: &prog.DAGVertex{}},
			{A: &prog.DAGVertex{}, B: &prog.DAGVertex{}},
			{A: &prog.DAGVertex{}, B: &prog.DAGVertex{}},
			{A: &prog.DAGVertex{}, B: &prog.DAGVertex{}},
		},
	}
	newBits := signal.FromRaw([]uint32{h1, h2}, 0)
	res := filterNewDagPairs(info, newBits)
	if len(res) != 2 {
		t.Fatalf("filterNewDagPairs returned %v pairs, want 2 (types %v/%v)", len(res), h1, h2)
	}
	// Mismatched lengths: no filtering.
	bad := &ipc.ProgInfo{DagSignal: []uint32{h1}, DagPairs: nil}
	if r := filterNewDagPairs(bad, newBits); len(r) != 0 {
		t.Fatalf("mismatched lengths not guarded: %v", len(r))
	}
}
