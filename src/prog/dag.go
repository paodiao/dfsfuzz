// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Operation-DAG feedback for the hmdfs merge-view fuzzer.
//
// eBPF trace events (see src/executor/hmdfs_trace.bpf.c) are mapped onto the
// syscall calls that triggered them using per-call execution windows
// (Call.CheckInfo.Stime/Etime, raw guest TSC) and normalized per-VM TSC
// offsets. Vertices are then paired into happens-before edges and concurrent
// pairs, and each pair is hashed into a novelty feedback bit.

package prog

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
)

// RetBucket classifies the return value of an operation.
type RetBucket uint32

const (
	RetSuccess RetBucket = iota
	RetEEXIST
	RetENOENT
	RetFailure
	RetWritepageDone
	RetWritepageErr
)

// TemporalRel is the temporal relation between two operations.
type TemporalRel uint32

const (
	TemporalHB TemporalRel = iota
	TemporalConcurrent
)

// PathRel is the path relation between two operations.
type PathRel uint32

const (
	PathSamePath PathRel = iota
	PathSameInode
	PathParentChild
	PathSameParent
	PathNone
)

// FuncID constants, must match the enum in hmdfs_trace.bpf.c.
const (
	FuncMkdir     uint32 = 0
	FuncCreate    uint32 = 1
	FuncRmdir     uint32 = 2
	FuncUnlink    uint32 = 3
	FuncRename    uint32 = 4
	FuncWrite     uint32 = 5
	FuncRead      uint32 = 6
	FuncFileOpen  uint32 = 7
	FuncDirOpen   uint32 = 8
	FuncGetattr   uint32 = 9
	FuncSetattr   uint32 = 10
	FuncFsync     uint32 = 11
	FuncRelease   uint32 = 12
	FuncLookup    uint32 = 13
	FuncIterate   uint32 = 14
	FuncWritepage uint32 = 15
)

// DAGVertex is a single traced operation bound to a concrete path.
type DAGVertex struct {
	FuncID    uint32
	CallName  string // syscall name that triggered the event (from window matching)
	Path      string
	OldPath   string // rename source path (only for rename vertices)
	RetBucket RetBucket
	Stime     uint64
	Etime     uint64
	Ino       uint64
	ProgIdx   int
	IsDir     bool
	Off       uint64 // write/read: kiocb->ki_pos; truncate: ia_size; else 0
	Size      uint64 // file size at execution (post-exec fsMd)
}

// DAGPair is an ordered pair of vertices with temporal and path relations.
type DAGPair struct {
	A        *DAGVertex
	B        *DAGVertex
	Temporal TemporalRel
	PathRel  PathRel
}

// DagDiag captures per-round diagnostics of the DAG feedback pipeline
// (BuildVertices/ExtractPairs/ComputeFeedback). It is emitted as a single
// log line per execution to pinpoint where events get dropped or pairs
// filtered out.
type DagDiag struct {
	// BuildVertices.
	Events          int   // events fed into BuildVertices
	MatchFailed     int   // events dropped because no call window matched
	ProgIdxBad      int   // events dropped because ProgIdx is out of range
	PathEmpty       int   // events dropped because the path could not be resolved
	PerNodeVertices []int // vertices per ProgIdx (node)
	// Function-level breakdown (indexed by FuncID 0..15).
	PerFuncVertices [16]int // matched vertices per FuncID
	MatchFailFunc   [16]int // unmatched events per FuncID
	MatchFailRetOK  [16]int // unmatched events with a succeeded ret per FuncID
	// Match failure reasons (only counted once per event, in the vertex pass).
	MatchFailNoFunc int // no call of the event's function type in the program
	MatchFailNoCI   int // matching calls exist but CheckInfo is nil
	MatchFailTime   int // calls exist with windows, timestamp outside all
	// mfTime attribution: direction + nearest-window samples.
	MatchFailByNode []int    // unmatched events per ProgIdx
	MFTimeLate      int      // event ts later than every same-type window end
	MFTimeEarly     int      // event ts earlier than every same-type window start
	MFGap           int      // event sits between two same-type windows (prevE/nextS both exist)
	MFShift         int      // event on the outer side of all same-type windows (unilateral)
	MFSamples       []string // first few mfTime events (capped), for log output
	// Event-window distance histogram (buckets: <7µs / 7-21 / 21-52 /
	// 52-140 / 140-350 / 350µs-1ms / >1ms): inside = matched events'
	// distance to the nearest window edge, outside = -4 events' distance to
	// the nearest window boundary.
	MatchDistIn  [7]int
	MatchDistOut [7]int
	// ExtractPairs.
	TotalPairs      int // all vertex pairs considered
	OverlapPairs    int // pairs with overlapping windows (cc candidates)
	HBForwardPairs  int // pairs where A ends before B starts (hb candidates, A first)
	HBReversePairs  int // pairs where B ends before A starts (hb candidates, B first)
	FilteredNoMod   int // candidates dropped: no succeeded modifier on the ordering side
	FilteredPathRel int // candidates dropped: path relation is PathNone
	HBPairs         int // produced happens-before pairs
	CCPairs         int // produced concurrent pairs
	// ComputeFeedback.
	PairBitsUnique int // deduplicated pair hash count
}

type renameEvent struct {
	ts      uint64
	oldPath string
	newPath string
	ret     int32 // kernel return value: only ret==0 renames change the path
}

type deleteEvent struct {
	ts   uint64
	path string
	ret  int32 // kernel return value: only ret==0 deletes remove the path
}

// BuildVertices maps eBPF trace events to concrete (func, path) vertices.
// Events without a resolvable path are dropped.
func BuildVertices(events []HmdfsTraceEvent, ps []*Prog,
	fsMds []map[string]FileMetadata, hmcfg *Hmdfs_config,
	tscoffs []int64) ([]DAGVertex, *DagDiag) {

	diag := &DagDiag{Events: len(events)}

	// 1. per-node ino -> path (inos are node-local; cross-node the same file
	// has a different ino, so lookups must use the event's own node first)
	// and path -> isDir from the post-execution fsMd state. The merged map
	// is kept as a fallback for nodes whose fsMd is missing.
	inoToPathByNode := make(map[int]map[uint64]string)
	inoToPath := make(map[uint64]string)
	nodeType := make(map[string]bool)
	pathSize := make(map[string]uint64)
	for nodeIdx, fsMd := range fsMds {
		if fsMd == nil {
			continue
		}
		m := make(map[uint64]string, len(fsMd))
		for path, md := range fsMd {
			path = NormalizeMergeViewPath(path) // fsMd keys are "./<rel>" — align with call-argument paths
			ino := md.StatMd.Ino & 0xFFFFFFFF
			if _, ok := m[ino]; !ok {
				m[ino] = path
			}
			if _, ok := inoToPath[ino]; !ok {
				inoToPath[ino] = path
			}
			nodeType[path] = md.StatMd.Mode&syscall.S_IFMT == syscall.S_IFDIR
			if md.StatMd.Size >= 0 {
				pathSize[path] = uint64(md.StatMd.Size)
			}
		}
		inoToPathByNode[nodeIdx] = m
	}

	// 2. Rename timeline keyed by the old path (paths are global across
	// nodes, unlike inos) and delete timeline keyed by node+ino.
	renameTLByPath := make(map[string][]renameEvent)
	deleteTLByNode := make(map[int]map[uint64][]deleteEvent)
	for i := range events {
		ev := &events[i]
		if ev.ProgIdx < 0 || ev.ProgIdx >= len(ps) {
			continue
		}
		p := ps[ev.ProgIdx]
		callIdx := matchEventToCall(ev, p, tscoffs)
		if callIdx < 0 {
			continue
		}
		call := p.Calls[callIdx]
		switch ev.FuncID {
		case FuncRename:
			oldP, newP := extractRenamePaths(call)
			if oldP != "" && newP != "" {
				renameTLByPath[oldP] = append(renameTLByPath[oldP],
					renameEvent{ts: ev.Timestamp, oldPath: oldP, newPath: newP, ret: ev.Ret})
			}
		case FuncUnlink, FuncRmdir:
			if path := extractPathFromCall(call); path != "" {
				if deleteTLByNode[ev.ProgIdx] == nil {
					deleteTLByNode[ev.ProgIdx] = make(map[uint64][]deleteEvent)
				}
				deleteTLByNode[ev.ProgIdx][ev.Ino] = append(deleteTLByNode[ev.ProgIdx][ev.Ino],
					deleteEvent{ts: ev.Timestamp, path: path, ret: ev.Ret})
			}
		}
	}
	for oldPath := range renameTLByPath {
		sort.Slice(renameTLByPath[oldPath], func(i, j int) bool {
			return renameTLByPath[oldPath][i].ts < renameTLByPath[oldPath][j].ts
		})
	}
	for _, nodeMap := range deleteTLByNode {
		for ino := range nodeMap {
			sort.Slice(nodeMap[ino], func(i, j int) bool {
				return nodeMap[ino][i].ts < nodeMap[ino][j].ts
			})
		}
	}

	// 3. Build one vertex per resolvable event.
	vertices := make([]DAGVertex, 0, len(events))
	for i := range events {
		ev := &events[i]
		if ev.ProgIdx < 0 || ev.ProgIdx >= len(ps) {
			diag.ProgIdxBad++
			continue
		}
		p := ps[ev.ProgIdx]
		v := DAGVertex{
			FuncID:    ev.FuncID,
			RetBucket: bucketizeRet(ev.FuncID, ev.Ret),
			Ino:       ev.Ino,
			ProgIdx:   ev.ProgIdx,
		}
		switch ev.FuncID {
		case FuncWritepage:
			// Async callback: no call window, point-in-time vertex.
			v.Stime, v.Etime = ev.Timestamp, ev.Timestamp
			pth := ""
			if m, ok := inoToPathByNode[ev.ProgIdx]; ok {
				pth = m[ev.Ino&0xFFFFFFFF]
			}
			if pth == "" {
				pth = inoToPath[ev.Ino&0xFFFFFFFF]
			}
			if pth != "" {
				pth = renamePathAt(pth, ev.Timestamp, renameTLByPath)
			}
			if pth == "" {
				pth = deletePathAt(ev.ProgIdx, ev.Ino, ev.Timestamp, deleteTLByNode)
			}
			v.Path = pth
		default:
			callIdx := matchEventToCall(ev, p, tscoffs)
			if callIdx < 0 {
				diag.MatchFailed++
				if int(ev.FuncID) < len(diag.MatchFailFunc) {
					diag.MatchFailFunc[ev.FuncID]++
					if b := bucketizeRet(ev.FuncID, ev.Ret); b == RetSuccess || b == RetWritepageDone {
						diag.MatchFailRetOK[ev.FuncID]++
					}
				}
				switch callIdx {
				case -2:
					diag.MatchFailNoFunc++
				case -3:
					diag.MatchFailNoCI++
				default:
					diag.MatchFailTime++
					// mfTime attribution: nearest same-type window distance,
					// direction and per-node split.
					if diag.MatchFailByNode == nil {
						diag.MatchFailByNode = make([]int, len(ps))
					}
					if ev.ProgIdx >= 0 && ev.ProgIdx < len(diag.MatchFailByNode) {
						diag.MatchFailByNode[ev.ProgIdx]++
					}
					if d, ws, we, prevE, nextS, found := nearestWindowOfFunc(ev, p, tscoffs); found {
						if d > 0 {
							diag.MFTimeLate++
						} else if d < 0 {
							diag.MFTimeEarly++
						}
						// prevE/nextS both present: the event sits in the gap
						// between two same-type windows (async/late trigger).
						// Otherwise it is on the outer side of all windows
						// (unilateral: calibration/domain drift candidate).
						if prevE != -1 && nextS != -1 {
							diag.MFGap++
						} else {
							diag.MFShift++
						}
						abs := d
						if abs < 0 {
							abs = -abs
						}
						diag.MatchDistOut[matchDistBucket(abs)]++
						if len(diag.MFSamples) < 4 {
							diag.MFSamples = append(diag.MFSamples,
								fmt.Sprintf("idx=%d func=%d d=%+d win=[%d,%d] prevE=%d nextS=%d",
									ev.ProgIdx, ev.FuncID, d, ws, we, prevE, nextS))
						}
					}
				}
				continue
			}
			call := p.Calls[callIdx]
			v.CallName = call.Meta.Name
			ci := call.CheckInfo
			if ci == nil {
				continue
			}
			off := tscoffFor(tscoffs, ev.ProgIdx)
			ws := int64(ci.Stime) - off
			we := int64(ci.Etime) - off
			// Distance to the nearest window edge (inside the window).
			dist := int64(ev.Timestamp) - ws
			if d2 := we - int64(ev.Timestamp); d2 < dist {
				dist = d2
			}
			diag.MatchDistIn[matchDistBucket(dist)]++
			v.Stime = uint64(ws)
			v.Etime = uint64(we)
			switch ev.FuncID {
			case FuncMkdir, FuncCreate:
				v.Path = extractPathFromCall(call)
			case FuncRmdir, FuncUnlink:
				v.Path = extractPathFromCall(call)
				if pth := renamePathAt(v.Path, ev.Timestamp, renameTLByPath); pth != "" {
					v.Path = pth
				}
			case FuncRename:
				v.OldPath, v.Path = extractRenamePaths(call)
			case FuncLookup:
				v.Path = lookupPath(ev, call, inoToPathByNode, inoToPath)
			default:
				v.Path = extractPathFromCall(call)
				if v.Path == "" {
					v.Path = resolveFdToPath(p, call)
				}
				if pth := renamePathAt(v.Path, ev.Timestamp, renameTLByPath); pth != "" {
					v.Path = pth
				}
			}
		}
		if v.Path == "" {
			diag.PathEmpty++
			continue
		}
		v.IsDir = nodeType[v.Path]
		v.Off = ev.Off
		v.Size = pathSize[v.Path]
		if int(v.FuncID) < len(diag.PerFuncVertices) {
			diag.PerFuncVertices[v.FuncID]++
		}
		vertices = append(vertices, v)
		if diag.PerNodeVertices == nil {
			diag.PerNodeVertices = make([]int, len(ps))
		}
		diag.PerNodeVertices[v.ProgIdx]++
	}

	sort.Slice(vertices, func(i, j int) bool {
		if vertices[i].ProgIdx != vertices[j].ProgIdx {
			return vertices[i].ProgIdx < vertices[j].ProgIdx
		}
		return vertices[i].Stime < vertices[j].Stime
	})
	return vertices, diag
}

// ExtractPairs computes happens-before edges (from succeeded modifiers) and
// concurrent pairs (at least one modifier, overlapping windows).
func ExtractPairs(vertices []DAGVertex, diag *DagDiag) (hbPairs, ccPairs []DAGPair) {
	if diag == nil {
		diag = &DagDiag{}
	}
	n := len(vertices)
	diag.TotalPairs = n * (n - 1) / 2
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			A, B := &vertices[i], &vertices[j]
			overlap := A.Stime <= B.Etime && B.Stime <= A.Etime
			aMod, bMod := isSucceededModifier(A), isSucceededModifier(B)
			switch {
			case overlap:
				diag.OverlapPairs++
				// Concurrent pairs need a path relation and at least one
				// modifier operation (by type, not by success): failed
				// writes/mkdirs still carry state impact worth exploring,
				// while pure read||read pairs have no consistency value.
				if rel := determinePathRel(A, B, false); rel != PathNone &&
					(isModifierFunc(A.FuncID) || isModifierFunc(B.FuncID)) {
					ccPairs = append(ccPairs, DAGPair{A: A, B: B, Temporal: TemporalConcurrent, PathRel: rel})
				} else if rel == PathNone {
					diag.FilteredPathRel++
				} else {
					diag.FilteredNoMod++
				}
			case A.Etime < B.Stime:
				diag.HBForwardPairs++
				if rel := determinePathRel(A, B, true); rel != PathNone && aMod {
					hbPairs = append(hbPairs, DAGPair{A: A, B: B, Temporal: TemporalHB, PathRel: rel})
				} else if rel == PathNone {
					diag.FilteredPathRel++
				} else {
					diag.FilteredNoMod++
				}
			default:
				diag.HBReversePairs++
				if rel := determinePathRel(B, A, true); rel != PathNone && bMod {
					hbPairs = append(hbPairs, DAGPair{A: B, B: A, Temporal: TemporalHB, PathRel: rel})
				} else if rel == PathNone {
					diag.FilteredPathRel++
				} else {
					diag.FilteredNoMod++
				}
			}
		}
	}
	diag.HBPairs = len(hbPairs)
	diag.CCPairs = len(ccPairs)
	return
}

// ComputeFeedback hashes every pair into a novelty bit; the schedule bit
// hashes the whole sorted pair set.
func ComputeFeedback(hbPairs, ccPairs []DAGPair, hmcfg *Hmdfs_config, diag *DagDiag) (pairBits []uint32, schedBit uint32) {
	all := make([]DAGPair, 0, len(hbPairs)+len(ccPairs))
	all = append(all, hbPairs...)
	all = append(all, ccPairs...)
	hashes := make([]uint64, 0, len(all))
	pairBits = make([]uint32, 0, len(all))
	for _, p := range all {
		h := hashPair(p, hmcfg)
		hashes = append(hashes, h)
		pairBits = append(pairBits, uint32(h))
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	if diag != nil {
		unique := 0
		for i := range hashes {
			if i == 0 || hashes[i] != hashes[i-1] {
				unique++
			}
		}
		diag.PairBitsUnique = unique
	}
	h := fnv1a64()
	for _, v := range hashes {
		h = fnvAdd64(h, v)
	}
	return pairBits, uint32(h)
}

// DagPairToVariant maps a DAG pair to the (rootCall, variant) DCT combo that
// could generate it, together with the pair's temporal relation (which form
// of the combo produced it: concurrent or causal/HB). Returns ok=false for
// pairs whose operations have no direct DCT representation.
func DagPairToVariant(p *DAGPair) (string, CallVariant, TemporalRel, bool) {
	rootCall := p.A.CallName
	if rootCall == "" {
		rootCall = callNameOfFunc(p.A.FuncID)
	}
	if rootCall == "" {
		return "", CallVariant{}, 0, false
	}
	variantCall := p.B.CallName
	if variantCall == "" {
		variantCall = callNameOfFunc(p.B.FuncID)
	}
	if variantCall == "" {
		return "", CallVariant{}, 0, false
	}
	rel, ok := dagRelToDctRel(p)
	if !ok {
		return "", CallVariant{}, 0, false
	}
	return rootCall, CallVariant{CallName: variantCall, PathRelation: rel}, p.Temporal, true
}

// callNameOfFunc maps a DAG func_id to the syscall name that triggers it.
// Funcs without a concrete call name (SETATTR, LOOKUP, WRITEPAGE_CB) return "".
func callNameOfFunc(fid uint32) string {
	switch fid {
	case FuncMkdir:
		return "mkdir"
	case FuncCreate:
		return "creat"
	case FuncRmdir:
		return "rmdir"
	case FuncUnlink:
		return "unlink"
	case FuncRename:
		return "rename"
	case FuncWrite:
		return "write"
	case FuncRead:
		return "read"
	case FuncFsync:
		return "fsync"
	case FuncGetattr:
		return "stat"
	case FuncFileOpen:
		return "open"
	case FuncIterate:
		return "getdents64"
	}
	return ""
}

// dagRelToDctRel maps the DAG path relation onto the DCT PathRelation
// (the variant's path relative to the root's path).
func dagRelToDctRel(p *DAGPair) (PathRelation, bool) {
	switch p.PathRel {
	case PathSamePath, PathSameInode:
		return PathSame, true
	case PathParentChild:
		if pathIsAncestorOf(p.A.Path, p.B.Path) {
			return PathChild, true
		}
		if pathIsAncestorOf(p.B.Path, p.A.Path) {
			return PathParent, true
		}
		return 0, false
	case PathSameParent:
		return PathSibling, true
	}
	return 0, false
}

func pathIsAncestorOf(parent, child string) bool {
	return parent != "" && strings.HasPrefix(child, parent+"/")
}

// mfTolTicks is the tolerance (≈100µs @ 3.42GHz) applied to event-window
// matching: kretprobe timestamps lag the window end by tens of µs and the
// ns→TSC calibration has µs-level accuracy, while genuine async/gap events
// (>1ms, e.g. writeback) stay excluded. The default only applies when the
// executor-reported calibration ratio is unavailable; SetMfTolTicksFromRatio
// converts the 100µs semantic per-machine (3418 ticks/µs at 3.42GHz).
var mfTolTicks int64 = 342000

// SetMfTolTicksFromRatio sets the match tolerance from the executor's
// calibrated ns-per-tick ratio: 100µs = 100000ns / (ns/tick). Out-of-range
// ratios keep the default.
func SetMfTolTicksFromRatio(ratio float64) {
	if ratio < 0.1 || ratio > 10 {
		return
	}
	tol := int64(100000.0 / ratio)
	if tol >= 10000 && tol <= 10000000 {
		atomic.StoreInt64(&mfTolTicks, tol)
	}
}

// MfTolTicks returns the current event-window match tolerance in ticks.
func MfTolTicks() int64 { return atomic.LoadInt64(&mfTolTicks) }

// matchDistBucket buckets a distance in ticks (≈3418 ticks/µs @ 3.42GHz):
// <7µs / 7-21 / 21-52 / 52-140 / 140-350 / 350µs-1ms / >1ms.
func matchDistBucket(ticks int64) int {
	switch {
	case ticks < 24000:
		return 0
	case ticks < 72000:
		return 1
	case ticks < 178000:
		return 2
	case ticks < 479000:
		return 3
	case ticks < 1196000:
		return 4
	case ticks < 3418000:
		return 5
	default:
		return 6
	}
}

// matchEventToCall finds the call whose execution window contains the event
// timestamp, within mfTolTicks of the window edges (the event may be sampled
// slightly after the window end by the kretprobe handler, or a few µs before
// the window start by calibration imprecision). Among overlapping windows
// (should not happen within one VM since calls run sequentially) the one
// started last wins.
// Returns the call index on success, or a negative reason code:
//
//	-1 no matching call
//	-2 no call of the event's function type
//	-3 calls exist but CheckInfo is nil
//	-4 calls exist with windows, but the timestamp is outside all of them
func matchEventToCall(ev *HmdfsTraceEvent, p *Prog, tscoffs []int64) int {
	off := tscoffFor(tscoffs, ev.ProgIdx)
	best := -1
	hasFunc := false
	hasCI := false
	var bestStime int64
	ts := int64(ev.Timestamp)
	for i, call := range p.Calls {
		if !funcMatchesCall(ev.FuncID, call.Meta.Name) {
			continue
		}
		hasFunc = true
		ci := call.CheckInfo
		if ci == nil {
			continue
		}
		hasCI = true
		ws := int64(ci.Stime) - off
		we := int64(ci.Etime) - off
		if ts >= ws-atomic.LoadInt64(&mfTolTicks) && ts <= we+atomic.LoadInt64(&mfTolTicks) && (best == -1 || ws > bestStime) {
			best, bestStime = i, ws
		}
	}
	if best != -1 {
		return best
	}
	if !hasFunc {
		return -2
	}
	if !hasCI {
		return -3
	}
	return -4
}

// nearestWindowOfFunc returns the signed distance from an unmatched event's
// timestamp to the nearest window boundary among same-type calls (positive:
// event is later than the window end; negative: earlier than the window
// start), along with that window's raw Stime/Etime, the previous same-type
// window's Etime (prevE) and the next same-type window's Stime (nextS;
// -1 when absent). Only meaningful when matchEventToCall returned -4.
func nearestWindowOfFunc(ev *HmdfsTraceEvent, p *Prog, tscoffs []int64) (delta int64, stime, etime uint64, prevE, nextS int64, found bool) {
	off := tscoffFor(tscoffs, ev.ProgIdx)
	ts := int64(ev.Timestamp)
	prevE, nextS = -1, -1
	var bestAbs int64 = -1
	for _, call := range p.Calls {
		if !funcMatchesCall(ev.FuncID, call.Meta.Name) {
			continue
		}
		ci := call.CheckInfo
		if ci == nil {
			continue
		}
		ws := int64(ci.Stime) - off
		we := int64(ci.Etime) - off
		if we < ts && (prevE == -1 || we > prevE) {
			prevE = we
		}
		if ws > ts && (nextS == -1 || ws < nextS) {
			nextS = ws
		}
		var d int64
		switch {
		case ts > we:
			d = ts - we // late
		case ts < ws:
			d = ws - ts // early (negative)
			d = -d
		default:
			return 0, uint64(ws), uint64(we), prevE, nextS, true // inside; shouldn't happen for -4
		}
		abs := d
		if abs < 0 {
			abs = -abs
		}
		if bestAbs == -1 || abs < bestAbs {
			bestAbs = abs
			delta, stime, etime, found = d, uint64(ws), uint64(we), true
		}
	}
	return
}

// lookupPath resolves the looked-up path component via the parent directory
// ino (LOOKUP events carry the parent's inode). The event's own node is
// consulted first (inos are node-local), falling back to the merged map.
func lookupPath(ev *HmdfsTraceEvent, call *Call,
	inoToPathByNode map[int]map[uint64]string, inoToPath map[uint64]string) string {
	full := extractPathFromCall(call)
	if full == "" {
		return ""
	}
	ino := ev.Ino & 0xFFFFFFFF
	parentPath, ok := inoToPathByNode[ev.ProgIdx][ino]
	if !ok {
		parentPath, ok = inoToPath[ino]
	}
	if !ok {
		return full
	}
	for comp := full; comp != ""; {
		if GetParentDir(comp) == parentPath {
			return comp
		}
		parent := GetParentDir(comp)
		if parent == comp {
			break
		}
		comp = parent
	}
	return full
}

// renamePathAt rewrites path to its post-rename value at time ts, applying
// every rename (with ts <= ts) that touches the path or one of its ancestors,
// in chronological order (this handles chains like /A -> /B -> /C).
func renamePathAt(path string, ts uint64, renameTL map[string][]renameEvent) string {
	type app struct {
		ts      uint64
		oldPath string
		newPath string
	}
	var apps []app
	for oldPath, renames := range renameTL {
		for _, r := range renames {
			if r.ret != 0 {
				continue // failed renames do not change the path
			}
			if r.ts <= ts {
				apps = append(apps, app{r.ts, oldPath, r.newPath})
			}
		}
	}
	if len(apps) == 0 {
		return ""
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ts < apps[j].ts })
	cur := path
	for _, a := range apps {
		if cur == a.oldPath || strings.HasPrefix(cur, a.oldPath+"/") {
			cur = a.newPath + strings.TrimPrefix(cur, a.oldPath)
		}
	}
	return cur
}

// deletePathAt returns the path of ino on node at time ts if it was deleted
// by then (fallback for async writepage events on already-unlinked files).
func deletePathAt(nodeIdx int, ino, ts uint64,
	deleteTL map[int]map[uint64][]deleteEvent) string {
	dels, ok := deleteTL[nodeIdx][ino]
	if !ok {
		return ""
	}
	last := ""
	for _, d := range dels {
		if d.ret != 0 {
			continue // failed deletes do not remove the path
		}
		if d.ts <= ts {
			last = d.path
		} else {
			break
		}
	}
	return last
}

func tscoffFor(tscoffs []int64, idx int) int64 {
	if idx >= 0 && idx < len(tscoffs) {
		return tscoffs[idx]
	}
	if len(tscoffs) == 0 {
		return 0
	}
	return tscoffs[len(tscoffs)-1]
}

func bucketizeRet(fid uint32, ret int32) RetBucket {
	if fid == FuncWritepage {
		if ret == 0 {
			return RetWritepageDone
		}
		return RetWritepageErr
	}
	switch {
	case ret == 0:
		return RetSuccess
	case ret == -17: // -EEXIST
		return RetEEXIST
	case ret == -2: // -ENOENT
		return RetENOENT
	case ret > 0: // write/read return bytes written/read — positive means success
		return RetSuccess
	}
	return RetFailure
}

func isModifierFunc(fid uint32) bool {
	switch fid {
	case FuncMkdir, FuncCreate, FuncRmdir, FuncUnlink, FuncRename,
		FuncWrite, FuncSetattr, FuncFsync, FuncWritepage:
		return true
	}
	return false
}

func isSucceededModifier(v *DAGVertex) bool {
	if !isModifierFunc(v.FuncID) {
		return false
	}
	return v.RetBucket == RetSuccess || v.RetBucket == RetWritepageDone
}

// prePath returns the path the operation saw on entry (rename: old path).
func prePath(v *DAGVertex) string {
	if v.FuncID == FuncRename && v.OldPath != "" {
		return v.OldPath
	}
	return v.Path
}

// determinePathRel classifies the path relation between two vertices.
// For ordered pairs the post-path of A is compared against the pre-path of B.
func determinePathRel(A, B *DAGVertex, ordered bool) PathRel {
	aPaths := []string{A.Path, prePath(A)}
	bPaths := []string{B.Path, prePath(B)}
	for _, a := range aPaths {
		for _, b := range bPaths {
			if a != "" && a == b {
				return PathSamePath
			}
		}
	}
	if A.Ino != 0 && A.Ino == B.Ino {
		return PathSameInode
	}
	for _, a := range aPaths {
		for _, b := range bPaths {
			if a == "" || b == "" {
				continue
			}
			if strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/") {
				return PathParentChild
			}
		}
	}
	for _, a := range aPaths {
		for _, b := range bPaths {
			if a != "" && b != "" && GetParentDir(a) != "" && GetParentDir(a) == GetParentDir(b) {
				return PathSameParent
			}
		}
	}
	return PathNone
}

// funcMatchesCall reports whether a call name can trigger the given func_id.
func funcMatchesCall(fid uint32, name string) bool {
	switch fid {
	case FuncMkdir:
		return strings.Contains(name, "mkdir")
	case FuncCreate:
		// create_merge fires on any inode creation, including open(O_CREAT).
		return strings.Contains(name, "creat") || strings.Contains(name, "open")
	case FuncRmdir:
		return name == "rmdir"
	case FuncUnlink:
		return name == "unlink"
	case FuncRename:
		return name == "rename"
	case FuncWrite:
		return strings.Contains(name, "write")
	case FuncRead:
		return strings.Contains(name, "read") && !strings.Contains(name, "getdents")
	case FuncFileOpen, FuncDirOpen:
		return strings.Contains(name, "open")
	case FuncGetattr:
		return name == "stat" || strings.Contains(name, "fstatat")
	case FuncSetattr:
		return strings.Contains(name, "chmod") || strings.Contains(name, "truncate") ||
			strings.Contains(name, "utime")
	case FuncFsync:
		return strings.Contains(name, "fsync") || strings.Contains(name, "fdatasync")
	case FuncRelease:
		return name == "close"
	case FuncLookup:
		return isPathResolvingCall(name)
	case FuncIterate:
		return strings.Contains(name, "getdents")
	}
	return false
}

func isPathResolvingCall(name string) bool {
	return strings.Contains(name, "mkdir") || strings.Contains(name, "creat") ||
		name == "rmdir" || name == "unlink" || name == "rename" ||
		strings.Contains(name, "open") || name == "stat" ||
		strings.Contains(name, "fstatat") || strings.Contains(name, "chmod") ||
		strings.Contains(name, "truncate") || strings.Contains(name, "utime")
}

// featuresOf packs the vertex feature vector into one uint64:
// funcID(5b) | ret(3b) | depth(2b) | nodeType(1b) | persist(1b) | offset(3b).
func featuresOf(v *DAGVertex, hmcfg *Hmdfs_config) uint64 {
	depth := DepthBucketOf(v.Path)
	nt := uint64(0)
	if v.IsDir {
		nt = 1
	}
	persist := uint64(0)
	if hmcfg != nil && hmcfg.Persistence_dir != "" && strings.HasPrefix(v.Path, hmcfg.Persistence_dir) {
		persist = 1
	}
	return uint64(v.FuncID) | uint64(v.RetBucket)<<5 | depth<<8 | nt<<10 | persist<<11 | offsetBucketOf(v)<<12
}

// offset bucket constants (3 bits, values 0-4).
const (
	offsetBucketNA     = 0 // no offset semantics (mkdir/rmdir/.../fsync)
	offsetBucketZero   = 1 // pos == 0 (file start)
	offsetBucketMid    = 2 // 0 < pos < size-r (full-page region)
	offsetBucketTail   = 3 // size-r <= pos < size (last partial page)
	offsetBucketBeyond = 4 // pos >= size (sparse write / truncate expansion)
)

// writebackBlocksize is HMDFS_PAGE_SIZE (hmdfs.h) — the writeback unit that
// makes the last partial page (size % blocksize) a distinct behavior: HMDFS
// writes only size-pos bytes for it (file_remote.c hmdfs_get_writecount).
const writebackBlocksize = 4096

// isOffsetFunc reports whether the function carries offset semantics
// (write/read via kiocb->ki_pos; truncate via iattr->ia_size).
func isOffsetFunc(fid uint32) bool {
	return fid == FuncWrite || fid == FuncRead || fid == FuncSetattr
}

// offsetBucketOf maps (pos, size) to the offset bucket. Buckets mirror the
// HMDFS writeback behavior (file_remote.c hmdfs_get_writecount):
//
//	pos >= size            -> count = 0          (beyond — no remote write)
//	size < pos + PAGE_SIZE -> count = size - pos (tail — partial page)
//	otherwise              -> count = PAGE_SIZE  (full page)
func offsetBucketOf(v *DAGVertex) uint64 {
	if !isOffsetFunc(v.FuncID) {
		return offsetBucketNA
	}
	pos := v.Off
	size := v.Size
	if pos == 0 {
		return offsetBucketZero
	}
	if pos >= size {
		return offsetBucketBeyond
	}
	if r := size % writebackBlocksize; r > 0 && pos >= size-r {
		return offsetBucketTail
	}
	return offsetBucketMid
}

// DepthBucketOf maps a path to its depth bucket (0/1/2-4/5+ '/' counts).
// Bucket 0 is unreachable for merge_view paths (the prefix always contains
// a '/'); bucket 1 = merge_view/file; bucket 2 = 2-4; bucket 3 = 5+.
func DepthBucketOf(path string) uint64 {
	switch n := strings.Count(path, "/"); {
	case n == 0:
		return 0
	case n == 1:
		return 1
	case n <= 4:
		return 2
	default:
		return 3
	}
}

func hashPair(p DAGPair, hmcfg *Hmdfs_config) uint64 {
	h := fnv1a64()
	h = fnvAdd64(h, uint64(p.Temporal))
	h = fnvAdd64(h, uint64(p.PathRel))
	h = fnvAdd64(h, featuresOf(p.A, hmcfg))
	h = fnvAdd64(h, featuresOf(p.B, hmcfg))
	return h
}

func fnv1a64() uint64 {
	return 0xcbf29ce484222325
}

func fnvAdd64(h, v uint64) uint64 {
	for i := 0; i < 8; i++ {
		h ^= (v >> (8 * i)) & 0xff
		h *= 0x100000001b3
	}
	return h
}
