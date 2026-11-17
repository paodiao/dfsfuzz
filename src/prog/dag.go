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

// FuncID constants, must match the enum in hmdfs_trace.bpf.c and
// glusterfs_trace.bpf.c. 0..15 are the shared/HMDFS functions; 16..20 are
// GlusterFS-only (v1) and are never emitted by HMDFS.
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
	FuncWritepage uint32 = 15 // HMDFS only

	FuncLink     uint32 = 16
	FuncSymlink  uint32 = 17
	FuncReadlink uint32 = 18
	FuncGetxattr uint32 = 19
	FuncSetxattr uint32 = 20

	FuncCount = 21
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
	Off       uint64 // entry position: write/read entry ki_pos (O_APPEND=EOF); truncate ia_size; else 0
	Size      uint64 // entry-time file size for write/read/truncate (entry kprobe); else 0
}

// DAGPair is an ordered pair of vertices with temporal and path relations.
//
// APath/BPath carry the role-selected paths that produced PathRel and feed
// the coverage cell/event-shape dimensions. They are currently set only for
// GlusterFS link/symlink role pairs (HB A target/linkpath, HB B/CC target);
// an empty field means "use A.Path/B.Path", preserving HMDFS and legacy
// rename behavior.
type DAGPair struct {
	A        *DAGVertex
	B        *DAGVertex
	APath    string
	BPath    string
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
	// Function-level breakdown (indexed by FuncID 0..20).
	PerFuncVertices [FuncCount]int // matched vertices per FuncID
	MatchFailFunc   [FuncCount]int // unmatched events per FuncID
	MatchFailRetOK  [FuncCount]int // unmatched events with a succeeded ret per FuncID
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
	// Feature marginals of matched vertices / produced pairs (per execution),
	// emitted to dag.log for offline feature-coverage analysis.
	RetHist       [6]int            // matched vertices per RetBucket (0..5)
	ErrHist       [64]int           // negative rets: index = -ret-1 (-1..-64)
	DepthHist     [4]int            // matched vertices per depth bucket 0..3
	OffHist       [5]int            // matched vertices per offset bucket 0..4
	PersistHist   [2]int            // 0 = non-persistence path, 1 = under hmcfg.Persistence_dir
	TypeHist      [2]int            // 0 = file, 1 = dir
	RetByFunc     [FuncCount][6]int // matched vertices per (FuncID, RetBucket)
	OffByCall     [6][5]int         // matched vertices per (off-call row, offset bucket)
	TypeByFunc    [FuncCount][2]int // matched vertices per (FuncID, 0=file 1=dir)
	InoByFunc     [FuncCount][4]int // namespace funcs: 0=EntryIno==0, 1=EntryIno==0 and RetSuccess (capture-failure alarm; RENAME fresh-dst successes legitimately land here — the upper new_dentry has no inode until VFS's post-rename d_move, which runs after the kretprobe; the BPF entry fallback supplies the event-time type), 2=EntryIno!=Ino, 3=EntryIno==Ino
	RelHist       [5]int            // produced hb+cc pairs per PathRel (SamePath..None)
	PairRetCombos [6][6]int         // produced hb+cc pairs per (A.RetBucket, B.RetBucket)
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

// inoKey is the shared reverse-lookup key: the writepage tracepoint only
// carries i_ino&0xFFFFFFFF while the kprobe events carry the full composed
// DOMAIN:dev_id:ino_raw value.
func inoKey(ino uint64) uint64 {
	return ino & 0xFFFFFFFF
}

// pathInDir reports whether path is dir itself or a descendant of it. A plain
// string prefix is not enough: "merge_view/persist" must not match a sibling
// named "merge_view/persistent".
func pathInDir(path, dir string) bool {
	return dir != "" && (path == dir || strings.HasPrefix(path, dir+"/"))
}

// BuildVertices maps eBPF trace events to concrete (func, path) vertices.
// Events without a resolvable path are dropped.
func BuildVertices(events []HmdfsTraceEvent, ps []*Prog,
	fsMds []map[string]FileMetadata, hmcfg *Hmdfs_config,
	tscoffs []int64) ([]DAGVertex, *DagDiag) {

	diag := &DagDiag{Events: len(events)}
	glusterfs := hmcfg != nil && hmcfg.ProfileOrDefault().Name == "glusterfs"

	// 1. per-node ino -> path (inos are node-local; cross-node the same file
	// has a different ino, so lookups must use the event's own node first).
	// nodeType is the post-execution fallback for vertices whose event did
	// not carry an i_mode (e.g. negative lookups).
	inoToPathByNode := make(map[int]map[uint64]string)
	inoToPath := make(map[uint64]string)
	nodeType := make(map[string]bool)
	for nodeIdx, fsMd := range fsMds {
		if fsMd == nil {
			continue
		}
		m := make(map[uint64]string, len(fsMd))
		for path, md := range fsMd {
			path = NormalizeMergeViewPath(path) // fsMd keys are "./<rel>" — align with call-argument paths
			ino := inoKey(md.StatMd.Ino)
			if _, ok := m[ino]; !ok {
				m[ino] = path
			}
			if _, ok := inoToPath[ino]; !ok {
				inoToPath[ino] = path
			}
			nodeType[path] = md.StatMd.Mode&syscall.S_IFMT == syscall.S_IFDIR
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
		if glusterfs && hmcfg.Serv_num > 0 && ev.ProgIdx < hmcfg.Serv_num {
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
				deleteTLByNode[ev.ProgIdx][inoKey(ev.Ino)] = append(deleteTLByNode[ev.ProgIdx][inoKey(ev.Ino)],
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
		if glusterfs && hmcfg.Serv_num > 0 && ev.ProgIdx < hmcfg.Serv_num {
			// GlusterFS server programs do not attach uprobes; any server
			// ProgIdx in a malformed event is not a client observation.
			continue
		}
		p := ps[ev.ProgIdx]
		v := DAGVertex{
			FuncID:    ev.FuncID,
			RetBucket: bucketizeRet(ev.FuncID, ev.Ret),
			Ino:       vertexIno(ev),
			ProgIdx:   ev.ProgIdx,
		}
		switch ev.FuncID {
		case FuncWritepage:
			// Async callback: no call window, point-in-time vertex.
			v.Stime, v.Etime = ev.Timestamp, ev.Timestamp
			// Current (post-execution) path when the file still exists.
			pth := ""
			if m, ok := inoToPathByNode[ev.ProgIdx]; ok {
				pth = m[inoKey(ev.Ino)]
			}
			// Already-unlinked files: the node-local delete timeline is
			// authoritative, so consult it before the merged cross-node map
			// (raw inos are node-local).
			deleted := false
			if pth == "" {
				if d := deletePathAt(ev.ProgIdx, inoKey(ev.Ino), ev.Timestamp, deleteTLByNode); d != "" {
					pth, deleted = d, true
				}
			}
			if pth == "" {
				pth = inoToPath[inoKey(ev.Ino)]
			}
			// A delete-time path is a historical snapshot: a later rename of
			// the same path string belongs to a different file. renamePathAt
			// returns "" when no earlier rename applies, so keep pth then.
			if pth != "" && !deleted {
				if renamed := renamePathAt(pth, ev.Timestamp, renameTLByPath); renamed != "" {
					pth = renamed
				}
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
			evStart, evEnd := eventEdges(ev)
			// Distance to the raw call window (0 inside; tolerance admissions
			// get their true edge distance instead of folding into bucket 0).
			// Both endpoints of the event's own window must be admitted, so
			// the farther of the two edges decides.
			dist := edgeDist(evStart, ws, we)
			if d := edgeDist(evEnd, ws, we); d > dist {
				dist = d
			}
			diag.MatchDistIn[matchDistBucket(dist)]++
			// Vertex time is the event's own execution window (entry kprobe ->
			// return probe), not the enclosing syscall window: the call window
			// is only the attribution/filter bound. Fall back to the call
			// window when the entry state was lost.
			if ev.EntryTs != 0 && ev.EntryTs <= ev.Timestamp {
				v.Stime, v.Etime = ev.EntryTs, ev.Timestamp
			} else {
				v.Stime, v.Etime = uint64(ws), uint64(we)
			}
			refTs := eventRefTs(ev)
			switch ev.FuncID {
			case FuncMkdir, FuncCreate:
				v.Path = extractPathFromCall(call)
			case FuncRmdir, FuncUnlink:
				// The call argument is the path used at execution time; a
				// rename-timeline rewrite would corrupt a re-created path.
				v.Path = extractPathFromCall(call)
			case FuncRename:
				v.OldPath, v.Path = extractRenamePaths(call)
			case FuncLookup:
				if glusterfs {
					v.Path = ev.Path
				} else {
					v.Path, v.OldPath = lookupPath(ev, call, inoToPathByNode, inoToPath)
				}
			case FuncLink:
				if glusterfs {
					v.OldPath, v.Path = glusterfsLinkVertexPaths(call)
				}
			case FuncSymlink:
				if glusterfs {
					v.Path, v.OldPath = glusterfsSymlinkVertexPaths(call)
				}
			default:
				v.Path = extractPathFromCall(call)
				if v.Path == "" {
					// FD-derived paths come from an earlier open call and may
					// predate a rename: rewrite them to the current path only.
					v.Path = resolveFdToPath(p, call)
					if pth := renamePathAt(v.Path, uint64(refTs), renameTLByPath); pth != "" {
						v.Path = pth
					}
				}
			}
		}
		if v.Path == "" {
			diag.PathEmpty++
			continue
		}
		v.IsDir = eventIsDir(ev, nodeType[v.Path])
		if glusterfs && ev.FuncID == FuncSymlink {
			// symlink's B-side type is the canonical target, not the
			// linkpath. The tracer has no target inode, so resolve it from the
			// producing program's call prefix plus a deterministic
			// cross-client fallback. ps here is the full server+client batch;
			// pathTypeBefore requires that shape with the absolute Serv_num.
			matchIdx := matchEventToCall(ev, p, tscoffs)
			if isDir, known := pathTypeBefore(ps, ev.ProgIdx, matchIdx, hmcfg.Serv_num,
				hmcfg.FileTree, v.OldPath); known {
				v.IsDir = isDir
			} else if n := findPathNode(hmcfg.FileTree, v.OldPath); n != nil {
				v.IsDir = n.Type != NodeTypeFile
			} else if d, ok := nodeType[v.OldPath]; ok {
				v.IsDir = d
			} else {
				v.IsDir = false
			}
		}
		v.Off = ev.Off
		v.Size = ev.Size
		if int(v.FuncID) < len(diag.PerFuncVertices) {
			diag.PerFuncVertices[v.FuncID]++
		}
		// Feature marginals (see DagDiag), matched vertices only.
		rb := int(v.RetBucket)
		if rb >= 0 && rb < len(diag.RetHist) {
			diag.RetHist[rb]++
			if int(v.FuncID) < len(diag.RetByFunc) {
				diag.RetByFunc[v.FuncID][rb]++
			}
		}
		if ev.Ret < 0 && ev.Ret >= -int32(len(diag.ErrHist)) {
			diag.ErrHist[-ev.Ret-1]++
		}
		diag.DepthHist[int(DepthBucketOf(v.Path))]++
		diag.OffHist[int(offsetBucketOf(&v))]++
		if v.IsDir {
			diag.TypeHist[1]++
		} else {
			diag.TypeHist[0]++
		}
		if hmcfg != nil && pathInDir(v.Path, hmcfg.Persistence_dir) {
			diag.PersistHist[1]++
		} else {
			diag.PersistHist[0]++
		}
		recordCallTypeIno(diag, ev, &v)
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

// isEntryInoFunc reports whether the event's Ino field is the PARENT
// directory (old parent for rename) rather than the operation's own entry.
// For these functions the vertex must use EntryIno instead (0 when the
// entry is unknown, e.g. a negative lookup).
func isEntryInoFunc(fid uint32) bool {
	switch fid {
	case FuncMkdir, FuncCreate, FuncRename, FuncLookup:
		return true
	}
	return false
}

// vertexIno returns the inode that identifies the operation's own entry:
// EntryIno for the parent-inode functions (never falling back to the
// parent), the event's Ino for everyone else.
func vertexIno(ev *HmdfsTraceEvent) uint64 {
	if isEntryInoFunc(ev.FuncID) {
		return ev.EntryIno
	}
	return ev.Ino
}

// eventIsDir decides whether the vertex's path is a directory. The
// event-time i_mode wins, then the operation's unambiguous entry type for
// functions that can only create/delete/open one kind (fixing the
// post-execution fsMd skew where e.g. a removed directory is no longer
// listed and would be typed as a file), and only then the post-execution
// nodeType fallback.
func eventIsDir(ev *HmdfsTraceEvent, fallback bool) bool {
	if ev.Mode != 0 {
		return ev.Mode&syscall.S_IFMT == syscall.S_IFDIR
	}
	if d, known := funcDefaultIsDir(ev.FuncID); known {
		return d
	}
	return fallback
}

// funcDefaultIsDir returns the operation's unambiguous entry type and
// whether it is known from the function alone.
func funcDefaultIsDir(fid uint32) (isDir bool, known bool) {
	switch fid {
	case FuncMkdir, FuncRmdir, FuncDirOpen, FuncIterate:
		return true, true
	case FuncCreate, FuncUnlink, FuncWrite, FuncRead, FuncFileOpen,
		FuncFsync, FuncWritepage, FuncLink, FuncSymlink, FuncReadlink:
		return false, true
	}
	return false, false
}

// OffCallNames is the fixed row order of DagDiag.OffByCall.
var OffCallNames = [6]string{"write", "read", "pwrite64", "pread64", "truncate", "chmod"}

// offCallRow maps a call name to the OffByCall row. CallName may carry
// variant suffixes (mirroring the Contains style of isWriteCall etc.), so the
// more specific names are matched first; non-offset calls and empty names
// return -1 (not counted).
func offCallRow(name string) int {
	switch {
	case strings.Contains(name, "pwrite64"):
		return 2
	case strings.Contains(name, "pread64"):
		return 3
	case strings.Contains(name, "truncate"):
		return 4
	case strings.Contains(name, "chmod"):
		return 5
	case strings.Contains(name, "write"):
		return 0
	case strings.Contains(name, "read"):
		return 1
	}
	return -1
}

// recordCallTypeIno fills the per-call-type diagnostic marginals of matched
// vertices: the entry-semantics offset bucket per offset-call row, the
// event-time type per FuncID, and the entry-ino capture mode of the namespace
// functions (EntryIno==0 / EntryIno==0 with success / !=Ino / ==Ino).
//
// The offset-call rows only accept vertices whose event function actually
// carries offset semantics (write/read/setattr): a path-resolving call like
// truncate/chmod also produces LOOKUP events for its own path resolution,
// and those matched vertices must not pollute the call's offset buckets.
// Diagnostics only.
func recordCallTypeIno(diag *DagDiag, ev *HmdfsTraceEvent, v *DAGVertex) {
	if isOffsetFunc(ev.FuncID) {
		if row := offCallRow(v.CallName); row >= 0 {
			diag.OffByCall[row][int(offsetBucketOf(v))]++
		}
	}
	if int(v.FuncID) < len(diag.TypeByFunc) {
		if v.IsDir {
			diag.TypeByFunc[v.FuncID][1]++
		} else {
			diag.TypeByFunc[v.FuncID][0]++
		}
	}
	if isEntryInoFunc(ev.FuncID) && int(ev.FuncID) < len(diag.InoByFunc) {
		switch {
		case ev.EntryIno == 0:
			diag.InoByFunc[ev.FuncID][0]++
			// A successful namespace op that still has no entry inode
			// means the entry capture itself failed (the alarm counter).
			if bucketizeRet(ev.FuncID, ev.Ret) == RetSuccess {
				diag.InoByFunc[ev.FuncID][1]++
			}
		case ev.EntryIno != ev.Ino:
			diag.InoByFunc[ev.FuncID][2]++
		default:
			diag.InoByFunc[ev.FuncID][3]++
		}
	}
}

// ExtractPairs computes happens-before edges (from succeeded modifiers) and
// concurrent pairs (at least one modifier, overlapping windows).
func ExtractPairs(vertices []DAGVertex, diag *DagDiag) (hbPairs, ccPairs []DAGPair) {
	return ExtractPairsFor(hmdfsProfile, vertices, diag)
}

// pairSide identifies which side of a DAG pair is being resolved.
type pairSide uint8

const (
	pairSideA pairSide = iota
	pairSideB
)

// pairPathRel is one role-selected path pair and its relation.
type pairPathRel struct {
	aPath string
	bPath string
	rel   PathRel
}

func isLinkSymlinkVertex(v *DAGVertex) bool {
	if v == nil {
		return false
	}
	return v.FuncID == FuncLink || v.FuncID == FuncSymlink ||
		v.CallName == "link" || v.CallName == "symlink"
}

func isRenameVertex(v *DAGVertex) bool {
	if v == nil {
		return false
	}
	return v.FuncID == FuncRename || v.CallName == "rename"
}

// pairRolePathsForSide returns the GlusterFS link/symlink role paths for one
// side of a pair. HB A enumerates both target and linkpath; HB B and CC use
// the target only (linkpath is a program-local binding). Other calls keep the
// single Path. The caller invokes this only for the GlusterFS link/symlink
// branch; HMDFS and rename-involved pairs keep the legacy candidates.
func pairRolePathsForSide(v *DAGVertex, side pairSide, temporal TemporalRel) []string {
	if v == nil {
		return nil
	}
	name := v.CallName
	if name == "" {
		name = callNameOfFunc(v.FuncID)
	}
	switch name {
	case "link", "symlink":
		target := prePath(v)
		if target == "" {
			target = v.Path
		}
		if target == "" {
			return nil
		}
		if temporal == TemporalHB && side == pairSideA && v.Path != "" && v.Path != target {
			return []string{target, v.Path}
		}
		return []string{target}
	case "rename":
		// HB A is the post-state destination; HB B and CC are the
		// entry-state source. The destination still exists on the vertex for
		// materialization, but it is not the relation/cell path here.
		if temporal == TemporalHB && side == pairSideA {
			if v.Path == "" {
				return nil
			}
			return []string{v.Path}
		}
		src := prePath(v)
		if src == "" {
			return nil
		}
		return []string{src}
	case "readlink":
		if v.Path == "" {
			return nil
		}
		return []string{v.Path}
	default:
		if v.Path == "" {
			return nil
		}
		return []string{v.Path}
	}
}

// classifyPairPaths classifies one explicitly selected pair path. The order
// mirrors determinePathRelFor's single-candidate order.
func classifyPairPaths(profile *DFSProfile, A, B *DAGVertex, aPath, bPath string) PathRel {
	if aPath == "" || bPath == "" {
		return PathNone
	}
	if aPath == bPath {
		return PathSamePath
	}
	if profile != nil && profile.Events.SupportsSameInode &&
		A.ProgIdx == B.ProgIdx && A.Ino != 0 && A.Ino == B.Ino {
		return PathSameInode
	}
	if pathIsAncestorOf(aPath, bPath) || pathIsAncestorOf(bPath, aPath) {
		return PathParentChild
	}
	if GetParentDir(aPath) != "" && GetParentDir(aPath) == GetParentDir(bPath) {
		return PathSameParent
	}
	return PathNone
}

// relationPathsFor returns the path relation(s) for one event pair. Rename and
// link/symlink role pairs are enumerated explicitly (rename HB A=dst, HB B/CC=
// src; link/symlink HB A=target/linkpath, HB B/CC=target). Everything else
// keeps the legacy single-relation behavior.
func relationPathsFor(profile *DFSProfile, A, B *DAGVertex, ordered bool, temporal TemporalRel) []pairPathRel {
	if profile != nil &&
		(isLinkSymlinkVertex(A) || isLinkSymlinkVertex(B) ||
			isRenameVertex(A) || isRenameVertex(B)) {
		aPaths := pairRolePathsForSide(A, pairSideA, temporal)
		bPaths := pairRolePathsForSide(B, pairSideB, temporal)
		var out []pairPathRel
		for _, aPath := range aPaths {
			for _, bPath := range bPaths {
				rel := classifyPairPaths(profile, A, B, aPath, bPath)
				if rel == PathNone {
					continue
				}
				out = append(out, pairPathRel{aPath: aPath, bPath: bPath, rel: rel})
			}
		}
		return out
	}
	rel := determinePathRelFor(profile, A, B, ordered)
	if rel == PathNone {
		return nil
	}
	return []pairPathRel{{rel: rel}}
}

// ExtractPairsFor is the profile-aware pair extractor. GlusterFS v1 has no
// PathSameInode classification: pairs are classified by canonical path only.
func ExtractPairsFor(profile *DFSProfile, vertices []DAGVertex, diag *DagDiag) (hbPairs, ccPairs []DAGPair) {
	if profile == nil {
		profile = hmdfsProfile
	}
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
				rels := relationPathsFor(profile, A, B, false, TemporalConcurrent)
				if len(rels) == 0 {
					diag.FilteredPathRel++
					break
				}
				if !(isModifierFunc(A.FuncID) || isModifierFunc(B.FuncID)) {
					diag.FilteredNoMod++
					break
				}
				for _, r := range rels {
					diag.RelHist[r.rel]++
					ccPairs = append(ccPairs, DAGPair{
						A: A, B: B, APath: r.aPath, BPath: r.bPath,
						Temporal: TemporalConcurrent, PathRel: r.rel,
					})
				}
			case A.Etime < B.Stime:
				diag.HBForwardPairs++
				rels := relationPathsFor(profile, A, B, true, TemporalHB)
				if len(rels) == 0 {
					diag.FilteredPathRel++
					break
				}
				if !aMod {
					diag.FilteredNoMod++
					break
				}
				for _, r := range rels {
					diag.RelHist[r.rel]++
					hbPairs = append(hbPairs, DAGPair{
						A: A, B: B, APath: r.aPath, BPath: r.bPath,
						Temporal: TemporalHB, PathRel: r.rel,
					})
				}
			default:
				diag.HBReversePairs++
				rels := relationPathsFor(profile, B, A, true, TemporalHB)
				if len(rels) == 0 {
					diag.FilteredPathRel++
					break
				}
				if !bMod {
					diag.FilteredNoMod++
					break
				}
				for _, r := range rels {
					diag.RelHist[r.rel]++
					hbPairs = append(hbPairs, DAGPair{
						A: B, B: A, APath: r.aPath, BPath: r.bPath,
						Temporal: TemporalHB, PathRel: r.rel,
					})
				}
			}
		}
	}
	diag.HBPairs = len(hbPairs)
	diag.CCPairs = len(ccPairs)
	// Ret-bucket combos of produced pairs (the set fed into ComputeFeedback).
	for i := range hbPairs {
		p := &hbPairs[i]
		if p.A != nil && p.B != nil {
			diag.PairRetCombos[int(p.A.RetBucket)][int(p.B.RetBucket)]++
		}
	}
	for i := range ccPairs {
		p := &ccPairs[i]
		if p.A != nil && p.B != nil {
			diag.PairRetCombos[int(p.A.RetBucket)][int(p.B.RetBucket)]++
		}
	}
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
		// Count distinct signal bits, not distinct 64-bit hashes: pairBits
		// truncates each hash to uint32 (a collision sets a single bit), so
		// the diagnostic must use the same granularity.
		bits := make([]uint32, len(hashes))
		for i, h := range hashes {
			bits[i] = uint32(h)
		}
		sort.Slice(bits, func(i, j int) bool { return bits[i] < bits[j] })
		unique := 0
		for i := range bits {
			if i == 0 || bits[i] != bits[i-1] {
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
	case FuncLink:
		return "link"
	case FuncSymlink:
		return "symlink"
	case FuncReadlink:
		return "readlink"
	case FuncGetxattr:
		return "getxattr"
	case FuncSetxattr:
		return "setxattr"
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
// (the variant's path relative to the root's path). The Parent/Child
// direction is decided on the same candidate path basis as determinePathRel
// ({post, pre} per vertex): a rename vertex's post path (destination) does not
// preserve the relation established through its pre path (source), so a
// post-only check drops the rename Parent/Child forms (variant rename + Parent
// and root rename + Child in particular). The cell's depth/persist dims are
// taken from pairBPath: for role-selected link/symlink pairs that is the
// target, while legacy rename pairs still use B's post path (dst), which is
// sound because every generated HMDFS destination is a same-parent sibling of
// its source; cross-parent rename would need the matched B path instead.
// determinePathRel classifies a pair as PathParentChild only when some
// candidate combination has an ancestry relation, so for real extracted pairs
// one of the two loops below always matches; the false return is defensive
// against hand-built or inconsistent inputs only.
func dagRelToDctRel(p *DAGPair) (PathRelation, bool) {
	switch p.PathRel {
	case PathSamePath, PathSameInode:
		return PathSame, true
	case PathParentChild:
		if p.APath != "" || p.BPath != "" {
			a, b := pairAPath(p), pairBPath(p)
			if pathIsAncestorOf(a, b) {
				return PathChild, true
			}
			if pathIsAncestorOf(b, a) {
				return PathParent, true
			}
			return 0, false
		}
		ap, bp := pairPathCandidates(p.A), pairPathCandidates(p.B)
		for _, a := range ap {
			for _, b := range bp {
				if pathIsAncestorOf(a, b) {
					return PathChild, true
				}
			}
		}
		for _, a := range ap {
			for _, b := range bp {
				if pathIsAncestorOf(b, a) {
					return PathParent, true
				}
			}
		}
		return 0, false
	case PathSameParent:
		return PathSibling, true
	}
	return 0, false
}

// pairPathCandidates returns the path candidates of one vertex used by the
// path-relation classifier: the post path plus the pre path (rename old path,
// identical otherwise).
func pairPathCandidates(v *DAGVertex) [2]string {
	return [2]string{v.Path, prePath(v)}
}

// pairAPath/pairBPath return the role-selected relation/cell path of a pair,
// falling back to the vertex's primary path for HMDFS and all non-role pairs.
func pairAPath(p *DAGPair) string {
	if p != nil && p.APath != "" {
		return p.APath
	}
	if p == nil || p.A == nil {
		return ""
	}
	return p.A.Path
}

func pairBPath(p *DAGPair) string {
	if p != nil && p.BPath != "" {
		return p.BPath
	}
	if p == nil || p.B == nil {
		return ""
	}
	return p.B.Path
}

func pathIsAncestorOf(parent, child string) bool {
	return parent != "" && strings.HasPrefix(child, parent+"/")
}

// mfTolTicks is the tolerance (≈60µs @ 3.42GHz) applied to event-window
// matching: kretprobe timestamps lag the window end by tens of µs and the
// ns→TSC calibration has µs-level accuracy (per-round recalibration keeps
// the drift at ~4µs), while genuine async/gap events (>1ms, e.g. writeback)
// stay excluded. The default only applies when the executor-reported
// calibration ratio is unavailable; SetMfTolTicksFromRatio converts the
// 60µs semantic per-machine (3420 ticks/µs at 3.42GHz).
var mfTolTicks int64 = 205200

// SetMfTolTicksFromRatio sets the match tolerance from the executor's
// calibrated ns-per-tick ratio: 60µs = 60000ns / (ns/tick). Out-of-range
// ratios keep the default.
func SetMfTolTicksFromRatio(ratio float64) {
	if ratio < 0.1 || ratio > 10 {
		return
	}
	tol := int64(60000.0 / ratio)
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
// the window start by calibration imprecision). The tolerance makes adjacent
// same-type windows overlap at their edges, so the nearest admissible window
// wins; ties keep the previous latest-start preference.
// Returns the call index on success, or a negative reason code:
//
//	-1 no matching call
//	-2 no call of the event's function type
//	-3 calls exist but CheckInfo is nil
//	-4 calls exist with windows, but the timestamp is outside all of them
//
// eventEdges returns the event's own execution window in the global domain:
// the paired entry kprobe timestamp (start) and the return timestamp (end).
// A missing/lost entry state falls back to the return timestamp.
func eventEdges(ev *HmdfsTraceEvent) (start, end int64) {
	end = int64(ev.Timestamp)
	start = end
	if ev.EntryTs != 0 && ev.EntryTs <= ev.Timestamp {
		start = int64(ev.EntryTs)
	}
	return start, end
}

// eventRefTs is the path-history reference time of an operation: the start of
// its own window, i.e. what the operation saw when it began.
func eventRefTs(ev *HmdfsTraceEvent) int64 {
	start, _ := eventEdges(ev)
	return start
}

// edgeDist returns the distance of ts to the [ws,we] window (0 inside).
func edgeDist(ts, ws, we int64) int64 {
	if ts < ws {
		return ws - ts
	}
	if ts > we {
		return ts - we
	}
	return 0
}

func matchEventToCall(ev *HmdfsTraceEvent, p *Prog, tscoffs []int64) int {
	off := tscoffFor(tscoffs, ev.ProgIdx)
	best := -1
	hasFunc := false
	hasCI := false
	var bestStime, bestDist int64
	tol := atomic.LoadInt64(&mfTolTicks)
	evStart, evEnd := eventEdges(ev)
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
		// Both endpoints of the event's own window must be admitted (within
		// the tolerance). A tolerance-only admission is farther from the raw
		// window than an exact containment, so the farther edge decides: an
		// event fully inside a window can never be stolen by a later
		// same-type call whose tolerance-extended window also admits it.
		dStart, dEnd := edgeDist(evStart, ws, we), edgeDist(evEnd, ws, we)
		if dStart > tol || dEnd > tol {
			continue
		}
		dist := dStart
		if dEnd > dist {
			dist = dEnd
		}
		if best == -1 || dist < bestDist || (dist == bestDist && ws > bestStime) {
			best, bestStime, bestDist = i, ws, dist
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

// hmdfsRootMergeIno is HMDFS_ROOT_MERGE (hmdfs/inode.h): the low 32 bits of
// the merge-view root's i_ino. The root is never part of the fsMd snapshot,
// so root-parent lookups need this sentinel.
const hmdfsRootMergeIno = 6

// glusterfsLinkVertexPaths reads link(old, new): Path is the new linkpath and
// OldPath is the existing source. The hardlink alias itself is program-local
// and never authoritative.
func glusterfsLinkVertexPaths(call *Call) (oldPath, newPath string) {
	return extractPathFromCallByArgIdx(call, 0), extractPathFromCallByArgIdx(call, 1)
}

// glusterfsSymlinkVertexPaths reads symlink(target, linkpath): Path is the
// linkpath and OldPath is the canonical test-root target. The on-wire payload
// is relative to the linkpath parent.
func glusterfsSymlinkVertexPaths(call *Call) (linkPath, canonicalTarget string) {
	linkPath = extractPathFromCallByArgIdx(call, 1)
	return linkPath, symlinkCanonicalTarget(linkPath,
		extractPathFromCallByArgIdx(call, 0))
}

// lookupPath resolves the looked-up path component of a LOOKUP event. The
// event carries the parent inode (ev.Ino) and, for positive lookups, the
// child inode (ev.EntryIno; 0 for negative ones). The event's own node is
// consulted first (inos are node-local), falling back to the merged map.
// The second return value is an alternative candidate for the same component,
// used only for the rename source/destination ambiguity: the relation
// classifier consumes it through prePath, so an ambiguous lookup never loses
// a real path relation.
func lookupPath(ev *HmdfsTraceEvent, call *Call,
	inoToPathByNode map[int]map[uint64]string, inoToPath map[uint64]string) (string, string) {
	full := extractPathFromCall(call)
	if full == "" {
		return "", ""
	}
	ino := inoKey(ev.Ino)
	parentPath, ok := inoToPathByNode[ev.ProgIdx][ino]
	if !ok {
		parentPath, ok = inoToPath[ino]
	}
	rootParent := ino == hmdfsRootMergeIno && !ok
	// Positive lookup: the child inode resolves the exact component. Only
	// accept the reverse lookup when it is a component of the call's own path
	// (a later rename would otherwise relabel the destination here).
	var child string
	var okChild bool
	if ev.EntryIno != 0 {
		eino := inoKey(ev.EntryIno)
		child, okChild = inoToPathByNode[ev.ProgIdx][eino]
		if !okChild {
			child, okChild = inoToPath[eino]
		}
	}
	childOnCall := okChild && (child == full || strings.HasPrefix(full, child+"/")) &&
		(rootParent || parentPath == GetParentDir(child))
	// A rename resolves two final components (source and destination). The
	// post-execution ino map cannot disambiguate same-parent renames (after a
	// successful rename the source inode lives at the destination), and the
	// tracer only sees fs-level lookups, so a dcache-served source leaves no
	// event at all: exact per-event disambiguation is impossible. The parent
	// inode still pins cross-parent finals and the child inode pins
	// intermediates; a same-parent final keeps both candidates (sign picks
	// the primary, the alternative carries the other side for the relation
	// classifier).
	if call != nil && call.Meta != nil && call.Meta.Name == "rename" {
		oldP, newP := extractRenamePaths(call)
		if oldP != "" && newP != "" {
			oldPar, newPar := GetParentDir(oldP), GetParentDir(newP)
			mergeRoot := strings.TrimSuffix(MergeViewPrefix, "/")
			onOld := parentPath == oldPar || (rootParent && oldPar == mergeRoot)
			onNew := parentPath == newPar || (rootParent && newPar == mergeRoot)
			if ev.EntryIno == 0 {
				if oldPar != newPar {
					// Cross-parent: the parent side decides exactly.
					if onOld && !onNew {
						return oldP, ""
					}
					if onNew && !onOld {
						return newP, ""
					}
				} else if onOld {
					// Same-parent final component: the positive lookup is the
					// source, the negative one the destination (a missing
					// source removes the rename's only lookup). Both
					// candidates are kept so the relation classifier sees the
					// other side as well.
					return newP, oldP
				}
			} else if !childOnCall {
				// A positive event whose child cannot be pinned to the call's
				// own path: a source/destination final (an intermediate would
				// have resolved through childOnCall).
				if oldPar != newPar {
					if onOld && !onNew {
						return oldP, ""
					}
					if onNew && !onOld {
						return newP, ""
					}
				} else if onOld {
					// Same-parent final: the positive lookup is the source;
					// keep the destination as the alternative candidate
					// (positive destination lookups of an overwrite rename
					// land here too).
					return oldP, newP
				}
			}
		}
	}
	if childOnCall {
		return child, ""
	}
	// Root-parent lookup: the merge-view root is not in fsMd; the looked-up
	// component is the first path element under it.
	if rootParent {
		if comp := firstComponentUnderRoot(full); comp != "" {
			return comp, ""
		}
	}
	if !ok {
		return full, ""
	}
	for comp := full; comp != ""; {
		if GetParentDir(comp) == parentPath {
			return comp, ""
		}
		parent := GetParentDir(comp)
		if parent == comp {
			break
		}
		comp = parent
	}
	return full, ""
}

// firstComponentUnderRoot returns "merge_view/<first>" for the first path
// component under the merge-view root; "" when the path is not under it.
func firstComponentUnderRoot(path string) string {
	rest := strings.TrimPrefix(path, MergeViewPrefix)
	if rest == path || rest == "" {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return MergeViewPrefix + rest
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
// ino is inoKey-truncated, like the fsMd reverse maps.
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
		FuncWrite, FuncSetattr, FuncFsync, FuncWritepage,
		FuncLink, FuncSymlink, FuncSetxattr:
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

// prePath returns the path the operation saw on entry: the rename old path,
// or the alternative candidate of an ambiguous rename lookup. Identical to
// Path otherwise.
func prePath(v *DAGVertex) string {
	if v.OldPath != "" {
		return v.OldPath
	}
	return v.Path
}

// determinePathRel classifies the path relation between two vertices.
// The ordered flag is currently unused: the implementation conservatively
// compares the Cartesian product of {A.Path, prePath(A)} × {B.Path,
// prePath(B)}. For ordered pairs this over-approximates (a later rename's
// old path may match an earlier op's post path), but never misses a real
// relation. SameInode additionally requires both vertices to be on the same
// node: inode numbers are per-node identifiers, so equal values on different
// nodes do not identify the same file (cross-node same-path pairs are
// classified as SamePath first).
func determinePathRel(A, B *DAGVertex, ordered bool) PathRel {
	return determinePathRelFor(hmdfsProfile, A, B, ordered)
}

func determinePathRelFor(profile *DFSProfile, A, B *DAGVertex, ordered bool) PathRel {
	ap, bp := pairPathCandidates(A), pairPathCandidates(B)
	aPaths, bPaths := ap[:], bp[:]
	for _, a := range aPaths {
		for _, b := range bPaths {
			if a != "" && a == b {
				return PathSamePath
			}
		}
	}
	if profile == nil {
		profile = hmdfsProfile
	}
	if profile.Events.SupportsSameInode &&
		A.ProgIdx == B.ProgIdx && A.Ino != 0 && A.Ino == B.Ino {
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
		return strings.Contains(name, "read") && !strings.Contains(name, "getdents") &&
			!strings.Contains(name, "readlink")
	case FuncFileOpen:
		// creat() reaches the file .open (hmdfs_file_open_merge), so its
		// FILE_OPEN event must be attributable to the creat call too.
		return strings.Contains(name, "open") || strings.Contains(name, "creat")
	case FuncDirOpen:
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
	case FuncLink:
		return name == "link"
	case FuncSymlink:
		return name == "symlink"
	case FuncReadlink:
		return name == "readlink"
	case FuncGetxattr:
		return strings.Contains(name, "getxattr")
	case FuncSetxattr:
		return strings.Contains(name, "setxattr")
	}
	return false
}

func isPathResolvingCall(name string) bool {
	return strings.Contains(name, "mkdir") || strings.Contains(name, "creat") ||
		name == "rmdir" || name == "unlink" || name == "rename" ||
		strings.Contains(name, "open") || name == "stat" ||
		strings.Contains(name, "fstatat") || strings.Contains(name, "chmod") ||
		strings.Contains(name, "truncate") || strings.Contains(name, "utime") ||
		name == "link" || name == "symlink" || name == "readlink" ||
		strings.Contains(name, "getxattr") || strings.Contains(name, "setxattr")
}

// packFeatures packs the vertex feature vector into one uint64:
// funcID(5b) | ret(3b) | depth(2b) | nodeType(1b) | persist(1b) | offset(3b).
func packFeatures(funcID uint32, ret RetBucket, depth uint64, isDir, persist bool, off uint64) uint64 {
	nt := uint64(0)
	if isDir {
		nt = 1
	}
	p := uint64(0)
	if persist {
		p = 1
	}
	return uint64(funcID) | uint64(ret)<<5 | depth<<8 | nt<<10 | p<<11 | off<<12
}

// featuresOfPath packs a vertex's feature vector using an explicit path under
// an explicit persist prefix. Role-selected DAG pairs pass APath/BPath here so
// a link/symlink B side is measured at its target, not its linkpath.
func featuresOfPath(v *DAGVertex, path, persistDir string) uint64 {
	if v == nil {
		return 0
	}
	persist := pathInDir(path, persistDir)
	return packFeatures(v.FuncID, v.RetBucket, DepthBucketOf(path), v.IsDir, persist, offsetBucketOf(v))
}

// featuresOfDir packs a vertex's feature vector under an explicit persist
// prefix (the coverage board stores only persistDir, not the whole config).
func featuresOfDir(v *DAGVertex, persistDir string) uint64 {
	if v == nil {
		return 0
	}
	return featuresOfPath(v, v.Path, persistDir)
}

// featuresOf packs the vertex feature vector (see packFeatures).
func featuresOf(v *DAGVertex, hmcfg *Hmdfs_config) uint64 {
	pd := ""
	if hmcfg != nil {
		pd = hmcfg.Persistence_dir
	}
	return featuresOfDir(v, pd)
}

// hashShape mixes one abstract event shape — the pair's dimensions without any
// call names — exactly like hashPair does for a concrete pair. Feedback is
// event-level by design: two syscalls that trigger the same low-level func
// with the same margins carry the same shape.
func hashShape(temporal TemporalRel, rel PathRel, af, bf uint64) uint64 {
	h := fnv1a64()
	h = fnvAdd64(h, uint64(temporal))
	h = fnvAdd64(h, uint64(rel))
	h = fnvAdd64(h, af)
	h = fnvAdd64(h, bf)
	return h
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

// isOffsetFunc reports whether the kernel function carries offset semantics
// (write/read via kiocb->ki_pos; truncate via iattr->ia_size). It is only the
// FuncID-level gate: callers that score features must also refine by call name
// (isOffsetCall), because chmod/utime share FuncSetattr with truncate.
func isOffsetFunc(fid uint32) bool {
	return fid == FuncWrite || fid == FuncRead || fid == FuncSetattr
}

// offsetBucketOf maps (pos, size) to the offset bucket. Buckets mirror the
// HMDFS writeback behavior (file_remote.c hmdfs_get_writecount):
//
//	pos >= size            -> count = 0          (beyond — no remote write)
//	size < pos + PAGE_SIZE -> count = size - pos (tail — partial page)
//	otherwise              -> count = PAGE_SIZE  (full page)
//
// The name gate (isOffsetCall) keeps non-offset operations on offset FuncIDs
// out of the bucket space: chmod/utime write off=0 in the event, which would
// otherwise read as "truncate to 0" (Zero bucket). With the gate, features,
// cells, off=/offCall= marginals and producibleOffBuckets all agree.
func offsetBucketOf(v *DAGVertex) uint64 {
	if !isOffsetFunc(v.FuncID) || !isOffsetCall(v.CallName) {
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

// DepthBucketOfWithPrefix maps a path relative to prefix to the same bucket
// as DepthBucketOf does for merge_view paths.
func DepthBucketOfWithPrefix(path, prefix string) uint64 {
	if prefix != "" && path == strings.TrimSuffix(prefix, "/") {
		return 0
	}
	rel := NormalizePath(prefix, path)
	if rel == "" {
		return 0
	}
	switch n := strings.Count(rel, "/"); {
	case n == 0:
		return 1
	case n <= 3:
		return 2
	default:
		return 3
	}
}

func hashPair(p DAGPair, hmcfg *Hmdfs_config) uint64 {
	pd := ""
	if hmcfg != nil {
		pd = hmcfg.Persistence_dir
	}
	return hashShape(p.Temporal, p.PathRel,
		featuresOfPath(p.A, pairAPath(&p), pd),
		featuresOfPath(p.B, pairBPath(&p), pd))
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
