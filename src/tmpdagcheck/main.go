package main

import (
	"fmt"
	"math/rand"
	"monarch/prog"
)

func main() {
	// ---- DCT direction-1/2 checks ----
	dct := prog.NewDistributedChoiceTable("inodeops")
	r := rand.New(rand.NewSource(1))

	v1 := dct.ChooseVariant("mkdir", r)
	fmt.Printf("choose mkdir variant: %v\n", v1)
	if v1 == nil {
		panic("ChooseVariant returned nil")
	}

	// 25 selections without any yield: the combo's weight must never grow,
	// and if it was picked >= noYieldThreshold(20) times it must have been
	// down-weighted by noYieldDelta(5).
	combo := *v1
	w0 := dct.WeightOf("mkdir", combo)
	picked := 0
	for i := 0; i < 25; i++ {
		v := dct.ChooseVariant("mkdir", r)
		if v != nil && *v == combo {
			picked++
		}
	}
	w25 := dct.WeightOf("mkdir", combo)
	fmt.Printf("combo=%v picked=%d/25 weight %d -> %d (no growth expected, %d down if picked>=20)\n",
		combo, picked, w0, w25, 5)
	if w25 > w0 {
		panic("weight grew without any yield")
	}

	// A yield marks the combo explored and resets its no-yield counter:
	// further picks should not down-weight it again soon.
	dct.MarkYield("mkdir", combo)
	prev := dct.WeightOf("mkdir", combo)
	for i := 0; i < 10; i++ {
		dct.ChooseVariant("mkdir", r)
	}
	after := dct.WeightOf("mkdir", combo)
	fmt.Printf("weight after yield + 10 picks: %d -> %d\n", prev, after)
	if after > prev {
		panic("weight grew after yield")
	}

	fmt.Printf("HasRoot(mkdir)=%v HasRoot(open)=%v\n",
		dct.HasRoot("mkdir"), dct.HasRoot("open"))

	// ---- Time-aligned insertion position ----
	// Calls with windows [1000,2000] [3000,4000] [5000,6000].
	mkCall := func(stime, etime uint64) *prog.Call {
		return &prog.Call{CheckInfo: &prog.FileMetadata{Stime: stime, Etime: etime}}
	}
	tp := &prog.Prog{Calls: []*prog.Call{mkCall(1000, 2000), mkCall(3000, 4000), mkCall(5000, 6000)}}
	// refTime=2500: boundary times are 0/2000/4000/6000; 2000 is closest -> pos 1.
	pos := prog.TimeAlignedInsertPos(tp, 2500, 0)
	fmt.Printf("aligned pos for ref=2500: %d (expect 1)\n", pos)
	if pos != 1 {
		panic("bad aligned pos")
	}
	// refTime=0: program start -> pos 0.
	pos = prog.TimeAlignedInsertPos(tp, 0, 0)
	fmt.Printf("aligned pos for ref=0: %d (expect 0)\n", pos)
	if pos != 0 {
		panic("bad aligned pos 0")
	}
	// refTime=5500: boundary 6000 is closest -> pos 3 (end).
	pos = prog.TimeAlignedInsertPos(tp, 5500, 0)
	fmt.Printf("aligned pos for ref=5500: %d (expect 3)\n", pos)
	if pos != 3 {
		panic("bad aligned pos 3")
	}
	// No timing info -> -1.
	np := &prog.Prog{Calls: []*prog.Call{{}, {}}}
	pos = prog.TimeAlignedInsertPos(np, 2500, 0)
	fmt.Printf("aligned pos without timing: %d (expect -1)\n", pos)
	if pos != -1 {
		panic("bad no-timing pos")
	}

	// ---- Depth buckets ----
	d1 := prog.DepthBucketOf("merge_view/file")
	d2 := prog.DepthBucketOf("merge_view/a/b")
	d3 := prog.DepthBucketOf("merge_view/a/b/c/d/e")
	fmt.Printf("depth buckets: root-file=%d shallow=%d deep=%d (expect 1/2/3)\n", d1, d2, d3)
	if d1 != 1 || d2 != 2 || d3 != 3 {
		panic("bad depth buckets")
	}

	fmt.Println("ALL DAG-DCT CHECKS PASSED")

	// ---- DAG pair -> DCT variant mapping ----
	mk := &prog.DAGVertex{FuncID: prog.FuncMkdir, Path: "merge_view/100/a"}
	cr := &prog.DAGVertex{FuncID: prog.FuncCreate, Path: "merge_view/100/a/f"}
	pair := &prog.DAGPair{A: mk, B: cr, PathRel: prog.PathParentChild, Temporal: prog.TemporalConcurrent}
	root, variant, temporal, ok := prog.DagPairToVariant(pair)
	fmt.Printf("pair(mkdir->creat parent-child) -> root=%q variant=%v temporal=%v ok=%v\n", root, variant, temporal, ok)
	if !ok || root != "mkdir" || variant.CallName != "creat" || variant.PathRelation != prog.PathChild || temporal != prog.TemporalConcurrent {
		panic("bad parent-child mapping")
	}

	pair2 := &prog.DAGPair{A: cr, B: mk, PathRel: prog.PathSamePath, Temporal: prog.TemporalHB}
	root2, variant2, temporal2, ok2 := prog.DagPairToVariant(pair2)
	fmt.Printf("pair(creat->mkdir same) -> root=%q variant=%v temporal=%v ok=%v\n", root2, variant2, temporal2, ok2)
	if !ok2 || root2 != "creat" || variant2.PathRelation != prog.PathSame || temporal2 != prog.TemporalHB {
		panic("bad same-path mapping")
	}

	fmt.Println("ALL DAG-DCT CHECKS PASSED")
}
