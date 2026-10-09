package prog

import (
	"math/rand"
	"sort"
)

// Phase 2b-v1: exact directed plans.
//
// The coverage board's producible model is an arithmetic count, so selecting a
// concrete target requires a constructibility pass. PickDirectedBatch builds a
// concrete batch — one root call on node 0 and one variant per remaining node,
// all sharing the root call and root base path (the DCT motif) — whose runtime
// outcome is predictable from the FileTree state, keeps only plans whose
// predicted cell is still uncovered, and picks the batch cells by uniform
// sampling (seed cell + without-replacement group selection).
// insertDirectedBatch realizes the batch on cloned progs all-or-nothing and
// records the pending cells for exact post-execution matching.
//
// The enumeration is B-driven over the global merged view: the FileTree is the
// union of every node's observations (NodesByCid is creation ownership, not a
// visibility scope), so B may be any node except the merge_view root and A is
// any node that DCT's root-base selection would accept (no TmpDir, no root).
// Each B derives its A witness per relation in O(1): Child uses the direct
// A-eligible parent, Parent the direct A-eligible children (direct adjacency,
// mirroring GetPathByRelation's 1-hop semantics), Sibling the parent's other
// A-eligible children. Candidate sets are deterministic (ascending path
// order from the sorted walk) and the concrete witness is sampled with the
// plan RNG, so retries vary paths while a fixed seed stays reproducible. The
// whole tree is covered in O(N) with no cid partition and no per-cid
// truncation window.
//
// Scope: variants follow the template table in the plan doc. Fresh-path forms
// are supported on both sides: mkdir/creat roots materialize a fresh child of
// an existing directory (A4), the rename root moves an existing source to a
// fresh same-parent destination (BaseA=source, BaseA2=destination), mkdir/creat
// variants materialize a fresh child (A1), and fresh-fresh pairs put both sides
// on the same fresh path (create/delete races). unlink is in as both a root
// (existing file) and a variant. The variant ret gates are context-driven
// (variantKindAllowed + the fresh-fresh table), and remaining root-failure
// shapes stay out of scope with RetA pinned to Success.

// directedCell is the plan-time identity of a cell target: comboKey plus the
// pair-local marginals. RetA is implicitly RetSuccess (2a board scope).
type directedCell struct {
	Combo    comboKey
	Temporal TemporalRel
	RetB     RetBucket
	OffB     uint64
	PersistB bool
	DepthB   uint64
	TypeB    bool
}

func (dc directedCell) toKey() cellKey {
	return cellKey{
		Root: dc.Combo.Root, Variant: dc.Combo.Variant, Rel: dc.Combo.Rel,
		Temporal: dc.Temporal, RetA: RetSuccess, RetB: dc.RetB,
		OffB: dc.OffB, PersistB: dc.PersistB, DepthB: dc.DepthB, TypeB: dc.TypeB,
	}
}

// DirectedPending records one exact cell target of the most recent directed
// batch; the batch is resolved by ResolveDirectedPending after the following
// execution (each hit clears the cell baseline, each executed miss increments
// it).
type DirectedPending struct {
	Cell directedCell
}

// directedPlan is a constructible plan: the base paths, the target node for
// the variant, and the explicit B parameters when the template carries them.
// BaseB2 is the rename source (BaseB becomes the destination); it is empty for
// every non-rename variant. The Fresh* flags mark paths that do not exist at
// planning time and are materialized when the plan enters a batch:
//
//	FreshA:    BaseA  = fresh child of BaseA2 (mkdir/creat root)
//	FreshA2:   BaseA2 = fresh same-parent sibling of BaseA (rename root dst)
//	FreshB:    BaseB  = fresh child of BaseB2 (mkdir/creat variant)
//	FreshSame: BaseA == BaseB = fresh child of BaseA2 (fresh-fresh pair)
type directedPlan struct {
	Cell    directedCell
	NodeX   int // target node for the variant, assigned 1..M by batch assembly
	BaseA   string
	BaseB   string
	BaseB2  string // rename source path (BaseB = destination); "" otherwise
	BaseA2  string // fresh-path anchor: parent dir or rename destination
	BOffset uint64 // explicit entry offset (pread64/pwrite64)
	BLen    uint64 // pread read size / pwrite data length / truncate new size

	FreshA    bool
	FreshA2   bool
	FreshB    bool
	FreshSame bool
	// RenameReplace marks the E rename form (existing same-class same-parent
	// destination, Success); F (fresh destination) and B (ENOTEMPTY) leave it
	// false. Explicit because the live recheck must not infer the shape from
	// destination existence (a materialized F name could race into existence).
	RenameReplace bool
}

// directedBatch is one Phase-2b insertion unit: a single root call on BaseA
// (node 0) serving one variant per remaining node. All variants share the root
// call name and the root base path (the DCT motif: every variant's path is
// related to the same A), so one root execution can form every planned pair.
// BaseA2 is the rename root's fresh destination (empty otherwise). NodeX is
// assigned 1..M at assembly; M is capped by len(ps)-1 and by the number of
// pickable cells sharing the root.
type directedBatch struct {
	Root   string
	BaseA  string
	BaseA2 string
	Plans  []directedPlan
}

// directedRootCalls is the planned root set. mkdir/creat/rename roots are the
// 2b2 fresh-path forms (synthesized A paths, see the fresh passes); rmdir stays
// out of the root set.
var directedRootCalls = []string{
	"open", "read", "pread64", "write", "pwrite64", "truncate",
	"stat", "chmod", "fsync", "fdatasync", "getdents64",
	"unlink", "mkdir", "creat", "rename",
}

// directedVariantCalls is the planned variant subset (template table). rename
// is the dual-path form; unlink deletes an existing file (or, in fresh-fresh
// pairs, races the root creation); mkdir/creat materialize synthesized fresh
// paths (A1) or race a fresh root path.
var directedVariantCalls = []string{
	"open", "stat", "chmod", "fsync", "fdatasync", "getdents64",
	"read", "write", "pread64", "pwrite64", "truncate", "rmdir", "rename",
	"unlink", "mkdir", "creat",
}

// sizeChangingRoots mutate the base file's size; a variant pinning an offset
// bucket on the same path cannot be predicted after them.
var sizeChangingRoots = map[string]bool{"write": true, "pwrite64": true, "truncate": true}

// offsetVariants carry an explicit offset or length (the only way to pin a
// non-zero entry bucket).
var offsetVariants = map[string]bool{"pread64": true, "pwrite64": true, "truncate": true}

// isModifierCall mirrors isModifierFunc (dag.go) at the syscall-name level:
// HB pairs only form when the earlier vertex is a modifier.
func isModifierCall(name string) bool {
	switch name {
	case "mkdir", "creat", "rmdir", "unlink", "rename",
		"write", "pwrite64", "truncate", "chmod", "fsync", "fdatasync":
		return true
	}
	return false
}

// directedNode is a FileTree node view used by the planner.
type directedNode struct {
	path     string
	isDir    bool
	size     uint64
	depth    uint64
	persist  bool
	nonEmpty bool
}

// witnessCands holds the A-eligible candidate paths of one relation, split by
// type in lexicographic order. The candidate SET is deterministic (built from
// the sorted tree walk); the witness is sampled from it with the plan RNG, so
// repeated plans exercise different concrete paths while a fixed seed stays
// reproducible. Sampling order over the two slices is file then dir (the
// historical combined-list order), not a merged lexicographic order.
type witnessCands struct {
	file []string
	dir  []string
}

// directedBNode is one candidate variant-side (B) node of the planning view,
// with the relation witnesses derived from the live tree: for every supported
// relation the planner can answer "which A puts B in this relation to it" in
// O(1). Child/Parent use direct adjacency, mirroring GetPathByRelation's
// 1-hop semantics (the any-depth prefix view belongs to the classification
// layer, determinePathRel, and no longer widens the generated witnesses).
// Sibling matches PathSameParent, i.e. the same direct parent.
//
// The FileTree is the global merged view, so B may be any node (including an
// intermediate TmpDir, which DCT's relation-derived path selection accepts).
// A, however, must be a valid root base: DCT's root-base selection never picks
// TmpDir and never uses the merge_view root, so the witness sets below only
// contain such A-eligible nodes.
type directedBNode struct {
	directedNode
	isTmpDir bool         // TmpDir: usable as B but never as A (Same witness off)
	anc      witnessCands // direct A-eligible parent (Child witness)
	sub      witnessCands // direct A-eligible children (Parent witness)
	// parentEligible marks a parent that may host a fresh root path (non-TmpDir,
	// non-root): the generation side mirrors DCT's root-base selection, where a
	// randomly picked base never lands on a TmpDir.
	parentEligible bool
	// Sibling witness: the parent's A-eligible children are shared read-only
	// with the view (no per-B "minus self" copy); up to two slots are skipped
	// by index when sampling (B itself and, for rename, the source, which must
	// not be the A of a non-Same relation). -1 = slot not present.
	sibFile     []string
	sibDir      []string
	sibFileIdx  int
	sibDirIdx   int
	sibFileIdx2 int
	sibDirIdx2  int
	// Synthesized rename-variant B (Phase 2b2): srcPath marks a rename node;
	// path is the destination (a placeholder for a fresh destination, see
	// renameFresh), srcPath is the rename source (plan.BaseB2). renameFresh
	// selects the single predictable ret: fresh dst => Success, existing
	// non-empty dir dst => Failure ENOTEMPTY. The E overwrite shape shares the
	// fresh cells and is sampled at batch assembly (plan.RenameReplace).
	srcPath     string
	srcTmp      bool // rename source is a TmpDir: variant-side B, but never a root base
	renameFresh bool
	// Synthesized fresh-path B (A1 fresh-name variants): the variant creates a
	// fresh child of freshParent; only freshBCall is served, and only in its
	// predictable Success form.
	freshB      bool
	freshBCall  string
	freshParent string
}

// directedView is the planning view: every node of the global merged view
// except the merge_view root itself. It is built under one FileTree read lock
// and is deliberately uncapped: the enumeration is B-driven (A is derived from
// B by relation), so the whole tree is covered in O(N).
type directedView struct {
	bNodes []directedBNode
}

// aEligible reports whether n may serve as a root base (A): any merged-view
// node except the merge_view root and intermediate TmpDirs, mirroring DCT's
// root-base selection (dirBucketFilter always excludes TmpDir; the root path
// is never selected).
func aEligible(n, root *FileNode) bool {
	return n != nil && n != root && n.Type != NodeTypeTmpDir
}

// buildDirectedView snapshots the planner inputs under one tree read lock by
// walking the global merged view. The walk follows map iteration over
// Children, so the node list is sorted by path to keep the enumeration order
// (cells slice order and per-cell representative) deterministic.
func buildDirectedView(ft *FileTree, persistDir string) *directedView {
	v := &directedView{}
	if ft == nil {
		return v
	}
	ft.mu.RLock()
	defer ft.mu.RUnlock()
	root := ft.Root
	if root == nil {
		return v
	}
	var all []*FileNode
	var walk func(n *FileNode)
	walk = func(n *FileNode) {
		for _, c := range n.Children {
			if c == nil || c.FullPath == "" {
				continue
			}
			all = append(all, c)
			walk(c)
		}
	}
	walk(root)
	// The walk follows map iteration over Children; sort by path so the B order
	// (cells slice order and per-cell representative) is deterministic.
	sort.Slice(all, func(i, j int) bool { return all[i].FullPath < all[j].FullPath })
	// Candidate lists keyed by a tree node, restricted to A-eligible nodes
	// (aEligible = non-TmpDir, non-root, mirroring DCT's root-base selection).
	// The generation side is direct-adjacency only, mirroring
	// GetPathByRelation's 1-hop semantics: Child derives the direct parent,
	// Parent the direct eligible children, Sibling the same-parent children.
	// `all` is sorted by path, so appending in that order keeps every list
	// ascending; the walk's map order never leaks into the candidate order.
	parentChildren := make(map[*FileNode]witnessCands)
	addCand := func(c witnessCands, path string, isDir bool) witnessCands {
		if isDir {
			c.dir = append(c.dir, path)
		} else {
			c.file = append(c.file, path)
		}
		return c
	}
	for _, n := range all {
		if !aEligible(n, root) || n.Parent == nil {
			continue
		}
		parentChildren[n.Parent] = addCand(parentChildren[n.Parent], n.FullPath, n.Type != NodeTypeFile)
	}
	idx0 := make(map[string]int, len(all))
	for _, n := range all {
		bn := directedBNode{
			directedNode: directedNode{
				path:     n.FullPath,
				isDir:    n.Type != NodeTypeFile,
				size:     n.Size,
				depth:    DepthBucketOf(n.FullPath),
				persist:  pathInDir(n.FullPath, persistDir),
				nonEmpty: len(n.Children) > 0,
			},
			isTmpDir:       n.Type == NodeTypeTmpDir,
			parentEligible: n.Parent != nil && aEligible(n.Parent, root),
			// -1 = slot absent (see directedBNode).
			sibFileIdx: -1,
			sibDirIdx:  -1,
			// -1 = second skipped sibling slot absent.
			sibFileIdx2: -1,
			sibDirIdx2:  -1,
		}
		// Child witness: the direct parent, when it is A-eligible.
		if n.Parent != nil && aEligible(n.Parent, root) {
			bn.anc = addCand(bn.anc, n.Parent.FullPath, n.Parent.Type != NodeTypeFile)
		}
		// Parent witness: the direct A-eligible children (shared read-only).
		bn.sub = parentChildren[n]
		// Sibling: share the parent's A-eligible children (read-only); B's own
		// slot index is skipped at sampling time, so no per-B copy is built.
		if n.Parent != nil {
			pc := parentChildren[n.Parent]
			bn.sibFile, bn.sibDir = pc.file, pc.dir
			if i := sort.SearchStrings(pc.file, n.FullPath); i < len(pc.file) && pc.file[i] == n.FullPath {
				bn.sibFileIdx = i
			}
			if i := sort.SearchStrings(pc.dir, n.FullPath); i < len(pc.dir) && pc.dir[i] == n.FullPath {
				bn.sibDirIdx = i
			}
		}
		v.bNodes = append(v.bNodes, bn)
		idx0[n.FullPath] = len(v.bNodes) - 1
	}
	// Synthesized rename-variant B nodes (dual-path 2b2). The cell anchors on
	// the destination (bn.path), while Same classifies through the source
	// (prePath => BaseB2), so the witness sets are the destination's (fresh:
	// the source's, which shares the parent chain) minus the source slot:
	//   fresh: one node per source (the source is a variant-side path, so a
	//     TmpDir is allowed); the destination is a fresh sibling name
	//     materialized at batch assembly (plan.BaseB stays empty here). The E
	//     overwrite shape (existing same-class destination) shares these cells
	//     and is sampled at batch assembly instead (see PickDirectedBatch).
	//     Predictable ret: Success.
	//   enotempty: one node per non-empty plain dir destination, with the first
	//     sibling directory (TmpDir allowed: it is a variant-side path) as
	//     source. Predictable ret: Failure.
	// `all` is path-sorted and the parent child lists follow it, so both passes
	// stay deterministic.
	parentKids := make(map[*FileNode][]*FileNode)
	for _, n := range all {
		if n.Parent != nil {
			parentKids[n.Parent] = append(parentKids[n.Parent], n)
		}
	}
	appendRename := func(src, dst string, fresh bool, srcSibDirIdx int, srcTmp bool) {
		base := dst
		if fresh {
			base = src
		}
		orig := v.bNodes[idx0[base]]
		bn := directedBNode{
			directedNode:   orig.directedNode,
			anc:            orig.anc,
			sub:            orig.sub,
			parentEligible: orig.parentEligible,
			sibFile:        orig.sibFile,
			sibDir:         orig.sibDir,
			sibFileIdx:     orig.sibFileIdx,
			sibDirIdx:      orig.sibDirIdx,
			sibFileIdx2:    -1,
			sibDirIdx2:     -1,
			srcPath:        src,
			srcTmp:         srcTmp,
			renameFresh:    fresh,
		}
		if fresh {
			// Dims/type come from the source; a fresh destination has no
			// descendants and only the source slot is skipped. The placeholder
			// is normalized like freshRenameDst so it stays a prefix of the
			// materialized name (pre-suffixed sources otherwise break the
			// persist-prefix relation).
			bn.sub = witnessCands{}
			bn.path = renamePlaceholder(src)
			bn.depth = DepthBucketOf(bn.path)
			bn.persist = pathInDir(bn.path, persistDir)
		} else {
			// Skip the destination (orig.sibDirIdx) and the source, keeping the
			// skip indices ascending.
			lo, hi := orig.sibDirIdx, srcSibDirIdx
			if hi < lo {
				lo, hi = hi, lo
			}
			bn.sibDirIdx, bn.sibDirIdx2 = lo, hi
		}
		v.bNodes = append(v.bNodes, bn)
	}
	for _, s := range all {
		if s.Parent == nil {
			continue
		}
		appendRename(s.FullPath, renamePlaceholder(s.FullPath), true, -1, s.Type == NodeTypeTmpDir)
	}
	for _, d := range all {
		if d.Parent == nil || d.Type != NodeTypeDir || len(d.Children) == 0 {
			continue
		}
		src := ""
		srcTmp := false
		for _, s := range parentKids[d.Parent] {
			if s != d && s.Type != NodeTypeFile {
				src = s.FullPath
				srcTmp = s.Type == NodeTypeTmpDir
				break
			}
		}
		if src == "" {
			continue
		}
		orig := v.bNodes[idx0[d.FullPath]]
		// The source is a variant-side path: a TmpDir source is not in the
		// A-eligible sibling list, so there is no slot to skip (it is not a
		// Sibling candidate anyway).
		si := sort.SearchStrings(orig.sibDir, src)
		if si >= len(orig.sibDir) || orig.sibDir[si] != src {
			si = -1
		}
		appendRename(src, d.FullPath, false, si, srcTmp)
	}
	// Synthesized fresh-path variant B nodes (A1 mkdir/creat): one per (parent
	// directory, call). The Child witness is the direct A-eligible parent, the
	// Sibling witnesses the parent's eligible children; there is no Same (A is
	// existing) and no Parent (the fresh path has no descendants). The concrete
	// name is materialized at batch assembly.
	for _, d := range all {
		if d.Type == NodeTypeFile {
			continue
		}
		var anc witnessCands
		if aEligible(d, root) {
			anc = addCand(anc, d.FullPath, d.Type != NodeTypeFile)
		}
		pc := parentChildren[d]
		for _, call := range []string{"mkdir", "creat"} {
			ph := directedFreshPlaceholder(call, d.FullPath)
			isDir := call == "mkdir"
			v.bNodes = append(v.bNodes, directedBNode{
				directedNode: directedNode{
					path:    ph,
					isDir:   isDir,
					depth:   DepthBucketOf(ph),
					persist: pathInDir(ph, persistDir),
				},
				anc:         anc,
				sibFile:     pc.file,
				sibDir:      pc.dir,
				sibFileIdx:  -1,
				sibDirIdx:   -1,
				sibFileIdx2: -1,
				sibDirIdx2:  -1,
				freshB:      true,
				freshBCall:  call,
				freshParent: d.FullPath,
			})
		}
	}
	return v
}

// witnessFor returns the A base path that puts B in relation rel to A for the
// given root call, or ok=false when no A-eligible witness exists or the root's
// type constraint does not fit.
func (bn *directedBNode) witnessFor(rel PathRelation, root string, r *rand.Rand) (string, bool) {
	switch rel {
	case PathSame:
		// A = B must itself be a valid root base: TmpDir B has no Same witness.
		// Rename B anchors on the destination but classifies Same through the
		// source (prePath), so its Same witness is srcPath. A synthesized fresh
		// B has no existing A at all (fresh-fresh pairs are planned directly).
		if bn.freshB {
			return "", false
		}
		a := bn.path
		if bn.srcPath != "" {
			if bn.srcTmp {
				// The rename source becomes the root base for Same, and a
				// TmpDir is never a root base.
				return "", false
			}
			a = bn.srcPath
		}
		if bn.isTmpDir || !nodeFitsConstraint(bn.directedNode, root) {
			return "", false
		}
		return a, true
	case PathChild:
		return pickWitness(&bn.anc, root, r)
	case PathParent:
		return pickWitness(&bn.sub, root, r)
	case PathSibling:
		return bn.pickSibling(root, r)
	}
	return "", false
}

// pickSibling samples one A-eligible sibling without materializing a
// "minus self" list: the parent's per-type lists are shared read-only from the
// view and up to two slots (the skipped ones, in ascending order) are excluded
// by index mapping. -1 = slot absent.
func (bn *directedBNode) pickSibling(root string, r *rand.Rand) (string, bool) {
	pick := func(list []string, i1, i2 int) (string, bool) {
		n, e := len(list), 0
		if i1 >= 0 {
			e++
		}
		if i2 >= 0 {
			e++
		}
		if n-e <= 0 {
			return "", false
		}
		k := r.Intn(n - e)
		if i1 >= 0 && k >= i1 {
			k++
		}
		if i2 >= 0 && k >= i2 {
			k++
		}
		return list[k], true
	}
	switch callNameConstraint(root) {
	case relTargetFile:
		return pick(bn.sibFile, bn.sibFileIdx, bn.sibFileIdx2)
	case relTargetDir:
		return pick(bn.sibDir, bn.sibDirIdx, bn.sibDirIdx2)
	}
	// Any: the logical list is file candidates followed by dir candidates.
	// Index into the two slices directly (the same draw and mapping the old
	// combined list produced).
	nf, nd := len(bn.sibFile), len(bn.sibDir)
	ef, ed := 0, 0
	if bn.sibFileIdx >= 0 {
		ef++
	}
	if bn.sibFileIdx2 >= 0 {
		ef++
	}
	if bn.sibDirIdx >= 0 {
		ed++
	}
	if bn.sibDirIdx2 >= 0 {
		ed++
	}
	total := nf - ef + nd - ed
	if total == 0 {
		return "", false
	}
	i := r.Intn(total)
	if i < nf-ef {
		if bn.sibFileIdx >= 0 && i >= bn.sibFileIdx {
			i++
		}
		if bn.sibFileIdx2 >= 0 && i >= bn.sibFileIdx2 {
			i++
		}
		return bn.sibFile[i], true
	}
	i -= nf - ef
	if bn.sibDirIdx >= 0 && i >= bn.sibDirIdx {
		i++
	}
	if bn.sibDirIdx2 >= 0 && i >= bn.sibDirIdx2 {
		i++
	}
	return bn.sibDir[i], true
}

// firstCand returns the first candidate path of a witness set (files first),
// deterministically. The fresh-root passes need a fixed counterpart rather
// than a sampled witness.
func firstCand(c *witnessCands) string {
	if len(c.file) > 0 {
		return c.file[0]
	}
	if len(c.dir) > 0 {
		return c.dir[0]
	}
	return ""
}

// pickWitness samples one A-eligible candidate matching the root's type
// constraint. The candidate lists are deterministic; the choice is not, so
// repeated plans for the same cell exercise different concrete paths while a
// fixed RNG seed stays reproducible.
func pickWitness(c *witnessCands, root string, r *rand.Rand) (string, bool) {
	switch callNameConstraint(root) {
	case relTargetFile:
		if len(c.file) == 0 {
			return "", false
		}
		return c.file[r.Intn(len(c.file))], true
	case relTargetDir:
		if len(c.dir) == 0 {
			return "", false
		}
		return c.dir[r.Intn(len(c.dir))], true
	}
	// Any: the logical list is file candidates followed by dir candidates.
	// Index into the two slices directly (the same draw and mapping the old
	// combined list produced) so wide candidate sets are never copied per
	// draw (a large directory's sibling list is O(children)).
	total := len(c.file) + len(c.dir)
	if total == 0 {
		return "", false
	}
	i := r.Intn(total)
	if i < len(c.file) {
		return c.file[i], true
	}
	return c.dir[i-len(c.file)], true
}

// nodeFitsConstraint checks a node against the call's type constraint.
func nodeFitsConstraint(n directedNode, call string) bool {
	switch callNameConstraint(call) {
	case relTargetFile:
		return !n.isDir
	case relTargetDir:
		return n.isDir
	}
	return true
}

// directedKind is one realizable (offset bucket, return bucket) form of a
// variant call. Most variants have a single form; rename has two — fresh-dst
// Success and non-empty-dst ENOTEMPTY Failure — whose paths the synthesized
// B node selects between.
type directedKind struct {
	bucket uint64
	retB   RetBucket
}

// directedVariantKinds returns the realizable (offset bucket, return bucket)
// forms of a v1 variant call. ok=false when the call is out of scope.
func directedVariantKinds(call string) (kinds []directedKind, ok bool) {
	switch call {
	case "pread64", "pwrite64", "truncate":
		return []directedKind{
			{offsetBucketZero, RetSuccess}, {offsetBucketMid, RetSuccess},
			{offsetBucketTail, RetSuccess}, {offsetBucketBeyond, RetSuccess},
		}, true
	case "read", "write":
		return []directedKind{{offsetBucketZero, RetSuccess}}, true
	case "open", "stat", "chmod", "fsync", "fdatasync", "getdents64":
		return []directedKind{{offsetBucketNA, RetSuccess}}, true
	case "rmdir":
		return []directedKind{
			{offsetBucketNA, RetSuccess}, {offsetBucketNA, RetFailure},
			{offsetBucketNA, RetENOENT},
		}, true
	case "unlink":
		return []directedKind{
			{offsetBucketNA, RetSuccess}, {offsetBucketNA, RetENOENT},
		}, true
	case "mkdir":
		return []directedKind{
			{offsetBucketNA, RetSuccess}, {offsetBucketNA, RetEEXIST},
		}, true
	case "creat":
		return []directedKind{{offsetBucketNA, RetSuccess}}, true
	case "rename":
		return []directedKind{
			{offsetBucketNA, RetSuccess}, {offsetBucketNA, RetFailure},
		}, true
	}
	return nil, false
}

// variantKindAllowed applies the per-context return-bucket gates of a variant
// form against the concrete B node, root and relation:
//   - synthesized rename B: only its single shape ret;
//   - fresh B (A1): only the named call, only Success (the fresh path does not
//     exist yet);
//   - rmdir: Failure on an existing non-empty plain dir (Success/ENOENT belong
//     to the fresh-fresh pass);
//   - unlink: B is a file; ENOENT only where the same batch's root removes the
//     path (root unlink + Same) — fresh-fresh races plan it separately;
//   - mkdir: existing dir -> EEXIST; creat: existing file -> Success (truncate);
//     rename on existing nodes is never constructible (needs a dual path).
func variantKindAllowed(variant string, retB RetBucket, bn *directedBNode, root string, rel PathRelation) bool {
	if bn.srcPath != "" {
		if variant == "rename" && retB == renameKindRet(bn) {
			return true
		}
		// The batch's destructive root (unlink on the Same path) removes the
		// rename source before the variant runs, so the rename observes ENOENT
		// (the source lookup fails before the parent comparison).
		return variant == "rename" && retB == RetENOENT &&
			root == "unlink" && rel == PathSame
	}
	if bn.freshB {
		return variant == bn.freshBCall && retB == RetSuccess
	}
	switch variant {
	case "rmdir":
		return retB == RetFailure && bn.isDir && bn.nonEmpty
	case "unlink":
		if bn.isDir {
			return false
		}
		switch retB {
		case RetSuccess:
			return true
		case RetENOENT:
			return root == "unlink" && rel == PathSame
		}
		return false
	case "mkdir":
		return retB == RetEEXIST && bn.isDir
	case "creat":
		return retB == RetSuccess && !bn.isDir
	case "rename":
		return false
	}
	return true
}

// destructiveSameRoot reports whether the planned root removes (unlink) or
// moves away (the FreshA2 rename) the exact path a Same-relation variant
// targets. The variant can then only succeed by winning the CC race, by
// recreating the path (creat), or on the negative lookup form; everything
// else is unplannable, and an HB placement cannot construct Success at all.
//
// These gates are a planner-side constructibility choice, not an observability
// claim: they assume the root's removal is visible to the variant's node by
// execution time. On the target's lazy per-node views a stale entry may
// persist, so the coverage model still counts these cells and the 2a path can
// produce them.
func destructiveSameRoot(root string, rel PathRelation, freshA2 bool) bool {
	if rel != PathSame {
		return false
	}
	return root == "unlink" || (freshA2 && root == "rename")
}

// variantKindsFor returns the realizable kinds of a variant under a context.
// Under a destructive Same root the name-resolving variants gain the
// not-found form: the variant's lookup runs after the root's removal, so
// ENOENT is the fresh-view prediction (racy under CC; with lazy per-node views
// the variant's node may still see the stale entry, which the Success kinds
// cover where they are kept). The form is planned for files only: a negative
// lookup carries no i_mode and FuncLookup has no funcDefaultIsDir entry
// (dag.go), so eventIsDir falls back to the post-execution fsMd snapshot,
// where a removed directory is already gone and its type cannot be pinned. The
// model still counts the directory-type ENOENT cells (computeProducibleLocked),
// so this is a deliberate model/planner asymmetry, not an oversight.
// truncate/rename carry the same lookup-failure form (FuncLookup, OffB=NA; the
// model admits it via producibleOffBucketsForRet).
func variantKindsFor(variant, root string, rel PathRelation, freshA2, isDir bool) ([]directedKind, bool) {
	kinds, ok := directedVariantKinds(variant)
	if !ok {
		return nil, false
	}
	if destructiveSameRoot(root, rel, freshA2) && !isDir {
		switch variant {
		case "stat", "open", "chmod", "truncate", "rename":
			kinds = append(kinds, directedKind{offsetBucketNA, RetENOENT})
		}
	}
	return kinds, true
}

// fdHBVariants are the fd-based variants whose HB Success stays plannable
// under a destructive Same root: their embedded open may win the race before
// the removal takes effect, and the open fd keeps the inode alive after the
// name is gone (POSIX unlink only removes the name), so the following op can
// start after the root ends -> HB Success. Shared by the gate and its test.
var fdHBVariants = map[string]bool{
	"read": true, "write": true, "pread64": true, "pwrite64": true,
	"fsync": true, "fdatasync": true,
}

// destructiveHBDoomed reports whether an HB placement cannot produce this
// kind after the destructive root removed the path. Name-resolving single
// calls (stat/open/chmod/truncate) can only fail ENOENT once the path is gone,
// a second remover/mover (unlink/rmdir/rename) can never both Success with
// the root, and the non-ENOENT failures (ENOTEMPTY/EEXIST) are impossible too.
// Only creat (recreate), the fd variants (fdHBVariants) and the ENOENT forms
// remain.
func destructiveHBDoomed(variant string, retB RetBucket) bool {
	if retB == RetENOENT {
		return false
	}
	if retB != RetSuccess {
		return true
	}
	return !(variant == "creat" || fdHBVariants[variant])
}

// destructiveCCExclusive reports whether a CC placement of this kind is
// mutually exclusive with the destructive root's own Success on the same
// path: two removers/movers cannot both succeed.
func destructiveCCExclusive(variant string, retB RetBucket) bool {
	if retB != RetSuccess {
		return false
	}
	switch variant {
	case "unlink", "rename", "rmdir":
		return true
	}
	return false
}

// directedFreshName materializes the directed fresh path of call under parent,
// mirroring the 2a generation naming conventions.
func directedFreshName(call, parent string, r *rand.Rand) string {
	switch call {
	case "mkdir":
		return parent + "/mut_dir_" + randomSuffix(r)
	case "creat":
		return parent + "/._creat_" + randomSuffix(r) + ".txt"
	}
	return parent + "/fresh_" + randomSuffix(r)
}

// directedFreshPlaceholder returns a prefix of directedFreshName(call, parent)
// with the same parent and component count, so depth/persist margins computed
// from the placeholder match the materialized fresh name. The random suffix is
// unknown at planning; a persist dir reaching into it is the irreducible
// straddle case, caught by the live recheck.
func directedFreshPlaceholder(call, parent string) string {
	switch call {
	case "mkdir":
		return parent + "/mut_dir_"
	case "creat":
		return parent + "/._creat_"
	}
	return parent + "/fresh_"
}

// renamePlaceholder returns the prefix placeholder of a fresh rename
// destination: a prefix of freshRenameDst(src). Stripping one previous
// "._renamed_*" level keeps it a prefix even for already-renamed sources, so
// the depth/persist margins computed from the placeholder match the
// materialized name.
func renamePlaceholder(src string) string {
	return renameSuffixBase(src) + "._renamed"
}

// renameKindRet is the single predictable ret of a synthesized rename B node:
// fresh destinations succeed, non-empty dir destinations return ENOTEMPTY
// (RetFailure after bucketizeRet).
func renameKindRet(bn *directedBNode) RetBucket {
	if bn.renameFresh {
		return RetSuccess
	}
	return RetFailure
}

// realizeDirectedParams computes the explicit variant parameters that realize
// the target bucket and verifies the prediction with bucketOfOffset (the doc's
// bucket recomputation rule). ok=false when the bucket is not realizable on
// this node (e.g. mid/tail on a too-small file).
func realizeDirectedParams(variant string, bucket uint64, b directedNode, r *randGen) (off, blen uint64, ok bool) {
	if bucket == offsetBucketNA {
		return 0, 0, true
	}
	switch variant {
	case "read", "write":
		// Sequential I/O from a fresh fd always enters at position 0.
		return 0, 0, bucket == offsetBucketZero
	case "pread64", "pwrite64":
		off, ok := realizeDirectedOffset(r, b.size, bucket)
		if !ok {
			return 0, 0, false
		}
		return off, 1, true
	case "truncate":
		// Single source with the data-mutation arm: both must agree on which
		// (size, bucket) combinations are realizable.
		l, ok := realizeDirectedTruncateLen(r, b.size, bucket)
		if !ok {
			return 0, 0, false
		}
		return 0, l, true
	}
	return 0, 0, false
}

// directedPlanStatus classifies why PickDirectedBatch returned no plan.
type directedPlanStatus int

const (
	// directedPlanFound: a plan was returned.
	directedPlanFound directedPlanStatus = iota
	// directedPlanNone: nothing to target (enumeration space saturated, tree
	// too small/immature, or all candidate combos filtered).
	directedPlanNone
	// directedPlanCfgErr: a precondition/config anomaly that should never
	// occur in a healthy run (e.g. an empty or mismatched hmcfg.Cids).
	directedPlanCfgErr
)

// pickRenameReplaceDst returns an existing same-class non-TmpDir sibling of
// srcPath usable as the E overwrite destination (directories must still be
// empty: a non-empty target is the ENOTEMPTY form), "" when none exists. The
// caller samples it at batch assembly, where the plan keeps its fresh cell and
// only the destination shape (plus plan.RenameReplace) changes. Guards keep
// the converted plan inside its cell's validation envelope:
//   - used carries the batch's reserved paths (every plan's anchor, rename
//     source and target, plus already chosen E destinations): landing on the
//     plan's own BaseA would classify the observation as Same (path equality
//     is checked first by determinePathRel), and landing on a sibling plan's
//     path would break that plan (cross-BaseB interference the per-path
//     authority filter cannot see);
//   - the destination's persist margin must equal the cell's PersistB (the
//     fresh placeholder and an existing sibling can disagree only when
//     persistDir is a file path).
func pickRenameReplaceDst(ft *FileTree, srcPath string, used map[string]bool, persistDir string, wantPersist bool, r *rand.Rand) string {
	if ft == nil || r == nil {
		return ""
	}
	src := ft.FindNode(srcPath)
	if src == nil {
		return ""
	}
	cands := ft.GetNonTmpDirSiblings(srcPath)
	if len(cands) == 0 {
		return ""
	}
	sort.Strings(cands)
	var pick []string
	for _, c := range cands {
		if c == srcPath || used[c] || !renameSameParent(srcPath, c) {
			continue
		}
		n := ft.FindNode(c)
		if n == nil || n.Type == NodeTypeTmpDir {
			continue
		}
		if (n.Type == NodeTypeFile) != (src.Type == NodeTypeFile) {
			continue
		}
		if src.Type != NodeTypeFile && len(n.Children) > 0 {
			continue
		}
		if pathInDir(c, persistDir) != wantPersist {
			continue
		}
		pick = append(pick, c)
	}
	if len(pick) == 0 {
		return ""
	}
	return pick[r.Intn(len(pick))]
}

// renameSameParent reports whether src and dst satisfy hmdfs' hard rename
// requirement: both have a parent and share it (hmdfs_rename_merge compares
// the parent paths before tracing; cross-parent renames return EPERM). It is
// the single source for the assembly/validation checks and the E destination
// candidates.
func renameSameParent(src, dst string) bool {
	ps, pd := GetParentDir(src), GetParentDir(dst)
	return ps != "" && ps == pd
}

// planEffect classifies the state impact of a planned variant on its target
// path (BaseB). Only plans that actually change the path's existence or size
// can invalidate a sibling plan's prediction, so the batch assembler keeps at
// most one state authority per path; no-op failure forms and read-only calls
// coexist.
type planEffect int

const (
	planEffectObserver planEffect = iota // no state change on BaseB
	planEffectSize                       // content/size change (write family, truncate, creat on an existing file)
	planEffectDestroy                    // path removed or moved (unlink/rmdir/rename success)
	planEffectCreate                     // an entry is created (mkdir; its EEXIST prediction can be flipped when the batch root removes or moves the dir)
)

// planStateEffect returns the plan's effect on its BaseB. chmod counts as an
// observer: the executor runs as root and the cell model has no EACCES bucket,
// so a mode change cannot flip a sibling's predicted ret; fsync/fdatasync are
// durability-only; open/stat/read/pread64/getdents64 do not mutate the path.
// A fresh-B creat builds a new random name and never touches existing
// siblings, so only main-loop creat (existing-file truncation) is a size
// change. The loser forms (ENOENT) of unlink/rmdir/rename stay observers on
// purpose: a raced win performs exactly what the batch root is already doing
// on the same path, so it adds no interference beyond the planned race. mkdir
// always counts as a creator: its EEXIST prediction can be flipped to a
// successful creation whenever the batch root removes or moves the directory.
// FreshSame entries are exempted by the caller (intentional race).
func planStateEffect(p *directedPlan) planEffect {
	v, ret := p.Cell.Combo.Variant, p.Cell.RetB
	switch v {
	case "unlink", "rmdir", "rename":
		if ret == RetSuccess {
			return planEffectDestroy
		}
	case "write", "pwrite64", "truncate":
		if ret == RetSuccess {
			return planEffectSize
		}
	case "creat":
		if ret == RetSuccess && !p.FreshB {
			return planEffectSize
		}
	case "mkdir":
		return planEffectCreate
	}
	return planEffectObserver
}

// planTouchedPaths returns the paths whose state a plan depends on or
// changes: BaseB always, plus the rename source BaseB2 for the dual-path form
// (a successful rename moves the source away, so a sibling plan on that path
// would observe a state it did not predict). The anchor is not added on its
// own (a Same-form rename source may coincide with it).
func planTouchedPaths(p *directedPlan) []string {
	if p.Cell.Combo.Variant == "rename" && p.BaseB2 != "" && p.BaseB2 != p.BaseB {
		return []string{p.BaseB, p.BaseB2}
	}
	return []string{p.BaseB}
}

// filterBaseBAuthority enforces the batch's one-state-authority-per-path rule
// over every touched path (BaseB and, for a successful rename, its source
// BaseB2): the first plan of each path is always kept (the assembler puts the
// seed first); a writer is kept only when no kept plan already touches its
// paths, an observer only when no kept plan writes there. FreshSame plans are
// exempt (their same-fresh-path races are the intended G-c semantics).
func filterBaseBAuthority(plans []directedPlan) []directedPlan {
	seen := make(map[string]bool, len(plans))
	writerKept := make(map[string]bool, len(plans))
	out := plans[:0]
	for i := range plans {
		p := &plans[i]
		if p.FreshSame {
			out = append(out, *p)
			continue
		}
		writer := planStateEffect(p) != planEffectObserver
		paths := planTouchedPaths(p)
		conflict := false
		for _, b := range paths {
			if (writer && seen[b]) || (!writer && writerKept[b]) {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		for _, b := range paths {
			seen[b] = true
			if writer {
				writerKept[b] = true
			}
		}
		out = append(out, *p)
	}
	return out
}

// batchBaseBAuthorityOK verifies the assembled batch's postconditions: on
// every touched path (BaseB and, for a successful rename, its source BaseB2)
// either all kept plans are observers or exactly one plan is kept when a
// writer is involved (FreshSame exempt), and every E destination is reserved
// (never an anchor, a sibling plan's source or target, or reused). It turns a
// future assembler drift into a cfgErr/insertFail instead of silent
// order-dependent miss noise.
func batchBaseBAuthorityOK(plans []directedPlan) bool {
	anchors := make(map[string]bool, len(plans))
	for i := range plans {
		anchors[plans[i].BaseA] = true
	}
	seen := make(map[string]bool, len(plans))
	writerSeen := make(map[string]bool, len(plans))
	for i := range plans {
		p := &plans[i]
		if !p.FreshSame {
			w := planStateEffect(p) != planEffectObserver
			paths := planTouchedPaths(p)
			for _, b := range paths {
				if (w && seen[b]) || (!w && writerSeen[b]) {
					return false
				}
			}
			for _, b := range paths {
				seen[b] = true
				if w {
					writerSeen[b] = true
				}
			}
		}
		if p.RenameReplace {
			if anchors[p.BaseB] {
				return false
			}
			for j := range plans {
				if j == i {
					continue
				}
				q := &plans[j]
				if q.BaseB2 != "" && q.BaseB2 == p.BaseB {
					return false
				}
				if q.BaseB != "" && q.BaseB == p.BaseB {
					return false
				}
			}
		}
	}
	return true
}

// PickDirectedBatch enumerates constructible plans for uncovered target cells,
// samples one cell uniformly, and returns a batch of up to len(ps)-1 plans
// sharing that cell's root call and root base path (sampled without
// replacement when the group is larger than the node budget). The status tells
// the caller how to account for a nil batch: nothing to target
// (directedPlanNone) or a precondition/config anomaly (directedPlanCfgErr).
func PickDirectedBatch(ps []*Prog, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, r *randGen) (*directedBatch, directedPlanStatus) {
	if lcs == nil || lcs.FileTree == nil || hmcfg == nil || r == nil || len(ps) < 2 {
		return nil, directedPlanCfgErr
	}
	if !CoverageBoardReady() {
		return nil, directedPlanCfgErr
	}
	// The deployment invariant is len(Cids) == FuzzingVMs == len(ps) (the
	// config loader rejects any mismatch); a violation here means the planner
	// is fed a corrupted config, so classify it as cfgErr, not as "no plan".
	if len(hmcfg.Cids) == 0 || len(hmcfg.Cids) != len(ps) {
		return nil, directedPlanCfgErr
	}
	planByCell, cells := planDirectedCells(ps, lcs, hmcfg, r)
	if len(cells) == 0 {
		return nil, directedPlanNone
	}
	flags := covBoard.uncoveredCellFlags(cells)
	var pickable []int
	for i, ok := range flags {
		if ok {
			pickable = append(pickable, i)
		}
	}
	if len(pickable) == 0 {
		return nil, directedPlanNone
	}
	seed := planByCell[cells[pickable[r.Intn(len(pickable))]]]
	// Group: every still-pickable cell whose representative shares the seed's
	// root call name and root base paths, so one root execution serves all of
	// them. BaseA2 participates for fresh-path roots (it anchors the parent
	// directory or the rename destination).
	var group []int
	for i, ok := range flags {
		if !ok {
			continue
		}
		p := planByCell[cells[i]]
		if p.Cell.Combo.Root == seed.Cell.Combo.Root && p.BaseA == seed.BaseA &&
			p.BaseA2 == seed.BaseA2 {
			group = append(group, i)
		}
	}
	// Two rename plans of the same source cannot both succeed in one batch (the
	// first rename moves the source away, the second observes ENOENT): keep one
	// plan per rename source, always keeping the seed.
	var seedRenameSrc string
	if seed.Cell.Combo.Variant == "rename" {
		seedRenameSrc = seed.BaseB2
	}
	seenRenameSrc := make(map[string]bool)
	deduped := group[:0]
	for _, ci := range group {
		p := planByCell[cells[ci]]
		if p.Cell.Combo.Variant == "rename" {
			if p != seed && (p.BaseB2 == seedRenameSrc || seenRenameSrc[p.BaseB2]) {
				continue
			}
			seenRenameSrc[p.BaseB2] = true
		}
		deduped = append(deduped, ci)
	}
	group = deduped
	m := len(ps) - 1
	if len(group) < m {
		m = len(group)
	}
	if m <= 0 {
		return nil, directedPlanNone
	}
	seedKey := seed.Cell.toKey()
	seedCI := -1
	for _, ci := range group {
		if planByCell[cells[ci]].Cell.toKey() == seedKey {
			seedCI = ci
			break
		}
	}
	// Uniform sample without replacement (partial Fisher-Yates) when the group
	// is larger than the node budget. The draw must not drop the seed: put it
	// back by replacing the last sampled slot when it was shuffled out, so the
	// RNG stream stays identical to a plain draw.
	if len(group) > m {
		for i := 0; i < m; i++ {
			j := i + r.Intn(len(group)-i)
			group[i], group[j] = group[j], group[i]
		}
		group = group[:m]
		if seedCI >= 0 {
			found := false
			for _, ci := range group {
				if ci == seedCI {
					found = true
					break
				}
			}
			if !found {
				group[m-1] = seedCI
			}
		}
	}
	// The seed leads the materialization/filter order so the per-path
	// authority filter below always keeps the sampled cell.
	for i, ci := range group {
		if planByCell[cells[ci]].Cell.toKey() == seedKey {
			group[0], group[i] = group[i], group[0]
			break
		}
	}
	// Batch-level path reservations for the E overwrite destination: a rename
	// must not land on another plan's anchor, source, or target (cross-BaseB
	// interference the per-path authority filter cannot see), nor reuse an
	// already chosen destination.
	usedDst := make(map[string]bool, 2*len(group))
	for _, ci := range group {
		q := planByCell[cells[ci]]
		usedDst[q.BaseA] = true
		if q.BaseB != "" {
			usedDst[q.BaseB] = true
		}
		if q.BaseB2 != "" {
			usedDst[q.BaseB2] = true
		}
	}
	batch := &directedBatch{Root: seed.Cell.Combo.Root, BaseA: seed.BaseA, BaseA2: seed.BaseA2}
	if seed.FreshA || seed.FreshSame {
		batch.BaseA = directedFreshName(seed.Cell.Combo.Root, seed.BaseA2, r.Rand)
	}
	if seed.FreshA2 {
		// Same suffix convention as the DCT rename: strip one previous
		// "._renamed_*" level before appending, so batch materialization
		// cannot accumulate suffixes round over round.
		batch.BaseA2 = freshRenameDst(seed.BaseA, r.Rand)
	}
	for i, ci := range group {
		p := *planByCell[cells[ci]]
		p.NodeX = i + 1
		switch {
		case p.FreshA, p.FreshSame:
			p.BaseA = batch.BaseA
		case p.FreshA2:
			p.BaseA2 = batch.BaseA2
		}
		if p.FreshB {
			p.BaseB = directedFreshName(p.Cell.Combo.Variant, p.BaseB2, r.Rand)
		}
		if p.FreshSame {
			p.BaseB = batch.BaseA
		}
		if p.Cell.Combo.Variant == "rename" && p.BaseB == "" {
			// Fresh rename variant destination: materialize a unique sibling
			// name now so retries vary paths while a fixed seed stays
			// reproducible.
			p.BaseB = freshRenameDst(p.BaseB2, r.Rand)
		}
		if p.Cell.Combo.Variant == "rename" && p.Cell.RetB == RetSuccess && !p.RenameReplace {
			// E overwrite sampling: E shares the fresh cells, so half the time
			// the plan keeps its cell but swaps the fresh destination for an
			// existing same-class sibling (the shape F can never produce).
			if r.Rand.Intn(2) == 0 {
				if dst := pickRenameReplaceDst(lcs.FileTree, p.BaseB2, usedDst,
					hmcfg.Persistence_dir, p.Cell.PersistB, r.Rand); dst != "" {
					p.RenameReplace = true
					p.BaseB = dst
					usedDst[dst] = true
				}
			}
		}
		batch.Plans = append(batch.Plans, p)
	}
	// One state authority per BaseB: a sibling writer changes size/existence
	// before an observer or another writer runs, charging order-dependent
	// misses. Keep the seed plus, per path, either all observers or exactly
	// one plan when a writer is involved. FreshSame batches are exempt.
	batch.Plans = filterBaseBAuthority(batch.Plans)
	for i := range batch.Plans {
		batch.Plans[i].NodeX = i + 1
	}
	if !batchBaseBAuthorityOK(batch.Plans) {
		return nil, directedPlanCfgErr
	}
	return batch, directedPlanFound
}

// callFuncClass maps a planned call name (with its target kind) onto the
// traced low-level func that fires, mirroring funcMatchesCall. ok=false means
// the func cannot be predicted from the name, so the shape guard leaves the
// plan alone. open is classified by the planned flags: the directed generator
// never sets O_CREAT, so only FileOpen/DirOpen are possible.
func callFuncClass(call string, isDir bool) (uint32, bool) {
	switch call {
	case "write", "pwrite64":
		return FuncWrite, true
	case "read", "pread64":
		return FuncRead, true
	case "fsync", "fdatasync":
		return FuncFsync, true
	case "stat":
		return FuncGetattr, true
	case "chmod", "truncate":
		return FuncSetattr, true
	case "mkdir":
		return FuncMkdir, true
	case "creat":
		return FuncCreate, true
	case "rmdir":
		return FuncRmdir, true
	case "unlink":
		return FuncUnlink, true
	case "rename":
		return FuncRename, true
	case "getdents64":
		return FuncIterate, true
	case "open":
		if isDir {
			return FuncDirOpen, true
		}
		return FuncFileOpen, true
	}
	return 0, false
}

// callFuncClasses mirrors funcMatchesCall for the shape guards: creat() runs
// both the create and file-open paths, so it maps to two traced funcs. The
// guards enumerate every func a realized call may fire, so a known subset
// never suppresses the unknown half.
func callFuncClasses(call string, isDir bool) ([]uint32, bool) {
	if call == "creat" {
		return []uint32{FuncCreate, FuncFileOpen}, true
	}
	f, ok := callFuncClass(call, isDir)
	if !ok {
		return nil, false
	}
	return []uint32{f}, true
}

// predictedEventFuncs returns the funcs a realized call may fire for the given
// ret bucket and temporal form. An ENOENT under HB placement is a guaranteed
// path-lookup failure (the destructive root already removed the path), so only
// the lookup event is possible. Under CC the variant races the root: either the
// lookup fails, or it succeeds and the op func itself returns ENOENT. Both
// shapes must be enumerated, otherwise a recorded lookup shape (hashShape is
// call-name agnostic) could mark the unknown op-func shape as covered.
func predictedEventFuncs(call string, isDir bool, ret RetBucket, temporal TemporalRel) ([]uint32, bool) {
	if ret == RetENOENT {
		if temporal == TemporalConcurrent {
			funcs, ok := callFuncClasses(call, isDir)
			if !ok {
				return nil, false
			}
			return append([]uint32{FuncLookup}, funcs...), true
		}
		return []uint32{FuncLookup}, true
	}
	return callFuncClasses(call, isDir)
}

// dagRelOfPathRelation maps a DCT/planner relation onto the classifier's
// coarse event-level relation. PathNone means "not derivable".
func dagRelOfPathRelation(rel PathRelation) PathRel {
	switch rel {
	case PathSame:
		return PathSamePath
	case PathChild, PathParent:
		return PathParentChild
	case PathSibling:
		return PathSameParent
	}
	return PathNone
}

// rootOffsetBuckets lists a root call's possible offset buckets. pread64/
// pwrite64/truncate roots are generated without pinned parameters, so every
// bucket remains possible. read/write roots depend on the generation context:
// with freshFD the generator opens its own fd (insertDirectedBatch passes
// ExistingFd=nil), so the entry offset is always 0; without it (direction3
// roots generated by insertCallFromDCT) the root may reuse an existing fd
// whose position is non-zero, so all buckets stay possible.
func rootOffsetBuckets(root string, freshFD bool) []uint64 {
	switch root {
	case "pread64", "pwrite64", "truncate":
		return []uint64{offsetBucketZero, offsetBucketMid, offsetBucketTail, offsetBucketBeyond}
	case "read", "write":
		if freshFD {
			return []uint64{offsetBucketZero}
		}
		return []uint64{offsetBucketZero, offsetBucketMid, offsetBucketTail, offsetBucketBeyond}
	}
	return []uint64{offsetBucketNA}
}

// planDirectedCells builds one representative plan per distinct candidate cell
// for the current tree/board state. It is B-driven: every node of the global
// merged view (except the merge_view root) derives its A witness per relation,
// so the enumeration is O(N) and neither a cid partition nor a lexicographic
// window biases the pick. The returned cells slice is in deterministic
// plan-insertion order.
func planDirectedCells(ps []*Prog, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, r *randGen) (map[cellKey]*directedPlan, []cellKey) {
	planByCell := make(map[cellKey]*directedPlan)
	var cells []cellKey
	view := buildDirectedView(lcs.FileTree, hmcfg.Persistence_dir)
	if len(view.bNodes) == 0 {
		return planByCell, cells
	}
	// A-side dims of every ordinary node (the witnesses): the event-shape
	// guard needs the depth/persist/type of the root call's observed path, and
	// looking it up in the view avoids a tree lock per plan.
	type aShape struct {
		depth   uint64
		isDir   bool
		persist bool
	}
	byPath := make(map[string]aShape, len(view.bNodes))
	for i := range view.bNodes {
		bn := &view.bNodes[i]
		if bn.srcPath != "" || bn.freshB {
			continue
		}
		byPath[bn.path] = aShape{depth: bn.depth, isDir: bn.isDir, persist: bn.persist}
	}
	// aShapeFor resolves the A-side event dims of a plan. Fresh A paths are
	// materialized later; the prefix placeholder (directedFreshPlaceholder,
	// or the normalized rename destination) carries the same depth/persist
	// margins and the root call fixes the type. For a rename root (FreshA2)
	// the fresh destination is a same-parent sibling of the source, so the
	// source carries the type.
	aShapeFor := func(plan *directedPlan) (aShape, bool) {
		switch {
		case plan.FreshA, plan.FreshSame:
			host := directedFreshPlaceholder(plan.Cell.Combo.Root, plan.BaseA2)
			return aShape{
				depth:   DepthBucketOf(host),
				isDir:   plan.Cell.Combo.Root == "mkdir",
				persist: pathInDir(host, hmcfg.Persistence_dir),
			}, true
		case plan.FreshA2:
			// Rename root: the observed A path is the fresh destination (a
			// same-parent sibling of the source). The placeholder is normalized
			// exactly like freshRenameDst so the persist margin matches; the
			// source carries the type.
			if in, ok := byPath[plan.BaseA]; ok {
				host := renamePlaceholder(plan.BaseA)
				return aShape{
					depth:   DepthBucketOf(host),
					isDir:   in.isDir,
					persist: pathInDir(host, hmcfg.Persistence_dir),
				}, true
			}
			return aShape{}, false
		default:
			in, ok := byPath[plan.BaseA]
			return in, ok
		}
	}
	// shapeCovered reports whether every possible event shape of the planned
	// pair is already known: the cell can then only reproduce information the
	// feedback already has, so it is not worth planning again. The feedback
	// unit is deliberately event-level (no call names), so write/pwrite64
	// twins share a shape. Only the representative plan's A context is
	// consulted (the first candidate enumerated for the cell); another A
	// witness could realize an unknown shape, so this is an approximation at
	// the (cell, representative-A) granularity.
	shapeCovered := func(plan *directedPlan) bool {
		a, ok := aShapeFor(plan)
		if !ok {
			return false
		}
		aFuncs, ok := callFuncClasses(plan.Cell.Combo.Root, a.isDir)
		if !ok {
			return false
		}
		bFuncs, ok := predictedEventFuncs(plan.Cell.Combo.Variant, plan.Cell.TypeB, plan.Cell.RetB, plan.Cell.Temporal)
		if !ok {
			return false
		}
		rel := dagRelOfPathRelation(plan.Cell.Combo.Rel)
		if rel == PathNone {
			return false
		}
		var shapes []uint64
		for _, aFunc := range aFuncs {
			for _, off := range rootOffsetBuckets(plan.Cell.Combo.Root, true) {
				af := packFeatures(aFunc, RetSuccess, a.depth, a.isDir, a.persist, off)
				for _, bFunc := range bFuncs {
					bf := packFeatures(bFunc, plan.Cell.RetB, plan.Cell.DepthB, plan.Cell.TypeB, plan.Cell.PersistB, plan.Cell.OffB)
					shapes = append(shapes, hashShape(plan.Cell.Temporal, rel, af, bf))
				}
			}
		}
		return covBoard.shapesAllKnown(shapes)
	}
	add := func(plan directedPlan) {
		ck := plan.Cell.toKey()
		if _, ok := planByCell[ck]; ok {
			return
		}
		if shapeCovered(&plan) {
			return
		}
		p := plan
		planByCell[ck] = &p
		cells = append(cells, ck)
	}
	// Live combos only: producible, not combo-suppressed, and with potential
	// left. found[t] counts distinct discovered cells and comboPotentials
	// subtracts the currently cell-suppressed cells, so potential 0 means no
	// 2b-available cell is left (discovered or suppressed): the prefilter is
	// exact for the planner, not just an optimization.
	rels := []PathRelation{PathSame, PathParent, PathChild, PathSibling}
	var combos []comboKey
	for _, root := range directedRootCalls {
		for _, variant := range directedVariantCalls {
			for _, rel := range rels {
				combos = append(combos, comboKey{Root: root, Variant: variant, Rel: rel})
			}
		}
	}
	enabled := covBoard.comboSelectableFlags(combos)
	pots := covBoard.comboPotentials(combos)
	rootsByVariantRel := make(map[string]map[PathRelation][]string)
	comboLive := make(map[comboKey]bool, len(combos))
	for i, ck := range combos {
		if !enabled[ck] || pots[i] <= 0 {
			continue
		}
		comboLive[ck] = true
		m := rootsByVariantRel[ck.Variant]
		if m == nil {
			m = make(map[PathRelation][]string)
			rootsByVariantRel[ck.Variant] = m
		}
		m[ck.Rel] = append(m[ck.Rel], ck.Root)
	}
	if len(rootsByVariantRel) == 0 {
		return planByCell, cells
	}
	// NodeX stays unassigned here (0): batch assembly assigns 1..M so that
	// every plan in a batch gets its own node.
	for i := range view.bNodes {
		bn := &view.bNodes[i]
		renameNode := bn.srcPath != ""
		for _, variant := range directedVariantCalls {
			if bn.freshB && variant != bn.freshBCall {
				continue
			}
			// Synthesized rename nodes serve only the rename variant; existing
			// nodes never serve it (the dual path needs a synthesized B).
			if !bn.freshB && (variant == "rename") != renameNode {
				continue
			}
			relRoots, ok := rootsByVariantRel[variant]
			if !ok {
				continue
			}
			if !nodeFitsConstraint(bn.directedNode, variant) {
				continue
			}
			for _, rel := range rels {
				roots := relRoots[rel]
				if len(roots) == 0 {
					continue
				}
				for _, root := range roots {
					if rel == PathSame && offsetVariants[variant] && sizeChangingRoots[root] {
						continue // entry size not predictable after the root
					}
					if root == "mkdir" || root == "creat" || root == "rename" {
						continue // fresh-path roots have their own passes
					}
					dest := destructiveSameRoot(root, rel, false)
					kinds, inScope := variantKindsFor(variant, root, rel, false, bn.isDir)
					if !inScope {
						continue
					}
					for _, kind := range kinds {
						bucket, retB := kind.bucket, kind.retB
						if !variantKindAllowed(variant, retB, bn, root, rel) {
							continue
						}
						if dest && destructiveCCExclusive(variant, retB) {
							continue
						}
						off, blen, ok := realizeDirectedParams(variant, bucket, bn.directedNode, r)
						if !ok {
							continue
						}
						// The cell identity does not depend on the A witness:
						// skip sampling for already-planned cells so a wide
						// tree does not re-sample the same targets. The
						// concurrent key decides for both temporal forms
						// (a modifier root's CC and HB cells are always
						// added together).
						cell := directedCell{
							Combo: comboKey{Root: root, Variant: variant, Rel: rel},
							RetB:  retB, OffB: bucket,
							PersistB: bn.persist, DepthB: bn.depth, TypeB: bn.isDir,
						}
						cell.Temporal = TemporalConcurrent
						if _, ok := planByCell[cell.toKey()]; ok {
							continue
						}
						aPath, ok := bn.witnessFor(rel, root, r.Rand)
						if !ok {
							continue
						}
						baseB := bn.path
						baseB2 := bn.srcPath
						switch {
						case bn.freshB:
							// Synthesized fresh B: materialized at batch
							// assembly from the parent anchor.
							baseB = ""
							baseB2 = bn.freshParent
						case renameNode && bn.renameFresh:
							// Fresh rename destinations are materialized when
							// the plan enters a batch.
							baseB = ""
						}
						plan := directedPlan{
							Cell:    cell,
							BaseA:   aPath,
							BaseB:   baseB,
							BaseB2:  baseB2,
							FreshB:  bn.freshB,
							BOffset: off, BLen: blen,
						}
						add(plan)
						if isModifierCall(root) && !(dest && destructiveHBDoomed(variant, retB)) {
							plan.Cell.Temporal = TemporalHB
							add(plan)
						}
					}
				}
			}
		}
	}
	// ── 2b2 fresh-path passes ────────────────────────────────────────────
	// The main loop derives A from an existing B; these passes handle the
	// combinations where the root's A, the variant's B, or both are fresh
	// paths materialized at batch assembly.
	//
	// FreshA: mkdir/creat root creates a fresh child of BaseA2; B is the
	// existing parent (Parent rel) or an existing sibling (Sibling rel). The
	// variant side stays an ordinary existing-B cell.
	for i := range view.bNodes {
		bn := &view.bNodes[i]
		if bn.srcPath != "" || bn.freshB {
			continue
		}
		for _, variant := range directedVariantCalls {
			kinds, ok := directedVariantKinds(variant)
			if !ok {
				continue
			}
			for _, root := range []string{"mkdir", "creat"} {
				for _, rel := range []PathRelation{PathParent, PathSibling} {
					if !comboLive[comboKey{Root: root, Variant: variant, Rel: rel}] {
						continue
					}
					if rel == PathParent && (!bn.isDir || bn.isTmpDir) {
						continue // the fresh root's container must be selector-eligible
					}
					if rel == PathSibling && !bn.parentEligible {
						continue
					}
					if !nodeFitsConstraint(bn.directedNode, variant) {
						continue
					}
					parent := bn.path
					if rel == PathSibling {
						parent = GetParentDir(bn.path)
					}
					if parent == "" {
						continue
					}
					for _, kind := range kinds {
						if !variantKindAllowed(variant, kind.retB, bn, root, rel) {
							continue
						}
						off, blen, ok := realizeDirectedParams(variant, kind.bucket, bn.directedNode, r)
						if !ok {
							continue
						}
						cell := directedCell{
							Combo: comboKey{Root: root, Variant: variant, Rel: rel},
							RetB:  kind.retB, OffB: kind.bucket,
							PersistB: bn.persist, DepthB: bn.depth, TypeB: bn.isDir,
						}
						cell.Temporal = TemporalConcurrent
						if _, ok := planByCell[cell.toKey()]; ok {
							continue
						}
						plan := directedPlan{
							Cell:   cell,
							BaseA2: parent, BaseB: bn.path,
							FreshA: true, BOffset: off, BLen: blen,
						}
						add(plan)
						if isModifierCall(root) {
							plan.Cell.Temporal = TemporalHB
							add(plan)
						}
					}
				}
			}
		}
	}
	// FreshA2: rename root moves an existing source to a fresh same-parent
	// destination (Success only). Matching anchors on the destination path, so
	// the constructible relations are Same (B = source), Sibling and Parent
	// (B an ancestor of the source/destination); a descendant-of-source B
	// would require a fresh-fresh pair.
	for i := range view.bNodes {
		bn := &view.bNodes[i]
		if bn.srcPath != "" || bn.freshB {
			continue
		}
		for _, variant := range directedVariantCalls {
			if variant == "rename" {
				continue
			}
			if !nodeFitsConstraint(bn.directedNode, variant) {
				continue
			}
			for _, rel := range []PathRelation{PathSame, PathSibling, PathParent} {
				if !comboLive[comboKey{Root: "rename", Variant: variant, Rel: rel}] {
					continue
				}
				src := ""
				firstOther := func(list []string) string {
					for _, p := range list {
						if p != bn.path {
							return p
						}
					}
					return ""
				}
				switch rel {
				case PathSame:
					if bn.isTmpDir {
						continue
					}
					src = bn.path
				case PathSibling:
					src = firstOther(bn.sibFile)
					if src == "" {
						src = firstOther(bn.sibDir)
					}
				case PathParent:
					if !bn.isDir {
						continue
					}
					src = firstCand(&bn.sub)
				}
				if src == "" {
					continue
				}
				dest := destructiveSameRoot("rename", rel, true)
				kinds, ok := variantKindsFor(variant, "rename", rel, true, bn.isDir)
				if !ok {
					continue
				}
				for _, kind := range kinds {
					if !variantKindAllowed(variant, kind.retB, bn, "rename", rel) {
						continue
					}
					if dest && destructiveCCExclusive(variant, kind.retB) {
						continue
					}
					off, blen, ok := realizeDirectedParams(variant, kind.bucket, bn.directedNode, r)
					if !ok {
						continue
					}
					cell := directedCell{
						Combo: comboKey{Root: "rename", Variant: variant, Rel: rel},
						RetB:  kind.retB, OffB: kind.bucket,
						PersistB: bn.persist, DepthB: bn.depth, TypeB: bn.isDir,
					}
					cell.Temporal = TemporalConcurrent
					if _, ok := planByCell[cell.toKey()]; ok {
						continue
					}
					plan := directedPlan{
						Cell:  cell,
						BaseA: src, FreshA2: true,
						BaseB: bn.path, BOffset: off, BLen: blen,
					}
					add(plan)
					if !(dest && destructiveHBDoomed(variant, kind.retB)) {
						plan.Cell.Temporal = TemporalHB
						add(plan)
					}
				}
			}
		}
	}
	// FreshSame: root and variant both operate on the same fresh path under an
	// existing directory (concurrent/ordered create-lookup/-delete races). The
	// ret prediction assumes the root executes first; CC attempts where the
	// variant wins either land the paired ENOENT form or, for fd-dependent
	// variants (no variant event when the embedded open fails), charge a miss
	// that the hit baseline absorbs.
	for i := range view.bNodes {
		bn := &view.bNodes[i]
		if bn.srcPath != "" || bn.freshB || !bn.isDir || bn.isTmpDir {
			continue
		}
		for _, fp := range freshSameForms {
			if !comboLive[comboKey{Root: fp.root, Variant: fp.variant, Rel: PathSame}] {
				continue
			}
			// Root-aware placeholder: a prefix of directedFreshName(root, ...)
			// so PersistB matches the materialized fresh path (creat uses the
			// ._creat_ convention, not mut_dir).
			ph := directedFreshPlaceholder(fp.root, bn.path)
			cell := directedCell{
				Combo: comboKey{Root: fp.root, Variant: fp.variant, Rel: PathSame},
				RetB:  fp.ret, OffB: fp.off,
				PersistB: pathInDir(ph, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(ph),
				TypeB:    fp.isDir,
			}
			cell.Temporal = TemporalConcurrent
			if _, ok := planByCell[cell.toKey()]; ok {
				continue
			}
			plan := directedPlan{Cell: cell, BaseA2: bn.path, FreshSame: true}
			add(plan)
			// Every form keeps its HB twin, including the ENOENT forms: under
			// HB the root has completed, so a later lookup that still fails is
			// a clean cross-node "creation invisible" oracle (G-c rationale).
			// Correct runs miss such a cell and it is suppressed after
			// cellMissThreshold like any other.
			if isModifierCall(fp.root) {
				plan.Cell.Temporal = TemporalHB
				add(plan)
			}
		}
	}
	return planByCell, cells
}

// ResolveDirectedPending settles the pending batch after an execution: every
// pending cell observed in the pairs is a hit (baseline reset), every other
// cell is an executed miss (miss counter), and a non-executed round clears the
// batch without counting.
func (lcs *LayeredChoiceStrategy) ResolveDirectedPending(pairs []DAGPair, persistDir string, executed bool) {
	if lcs == nil || len(lcs.PendingDirected) == 0 {
		return
	}
	pending := lcs.PendingDirected
	lcs.PendingDirected = nil
	observed := make(map[cellKey]bool, len(pairs))
	for i := range pairs {
		if ck, ok := cellKeyOf(&pairs[i], persistDir); ok {
			observed[ck] = true
		}
	}
	for i := range pending {
		ck := pending[i].Cell.toKey()
		switch {
		case observed[ck]:
			covBoard.noteDirectedHit(ck)
		case executed:
			covBoard.noteDirectedMiss(ck)
		}
	}
}

// directedNodeSize returns the last known size of a path (0 when unknown).
func directedNodeSize(lcs *LayeredChoiceStrategy, path string) uint64 {
	if lcs == nil || lcs.FileTree == nil {
		return 0
	}
	if n := lcs.FileTree.FindNode(path); n != nil {
		return n.Size
	}
	return 0
}

// directedNodeFlags returns the open flags matching the path's node type.
func directedNodeFlags(r *randGen, lcs *LayeredChoiceStrategy, path string) uint64 {
	flags := uint64(2) // O_RDWR default, mirroring the DCT insertion path
	if lcs != nil && lcs.FileTree != nil {
		if nodeIsDir(lcs.FileTree.FindNode(path)) {
			flags = r.target.GetConst("O_DIRECTORY")
		}
	}
	return flags
}

// directedVariantCallsSeq generates the B call sequence for the plan template.
// Offset/length variants bypass generateCallByName so their entry parameters
// are exactly the planned ones.
func directedVariantCallsSeq(r *randGen, sCalls *SpecialCalls, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, nodeIdx int, p *Prog, plan *directedPlan) []*Call {
	s := stateFromProg(p)
	variant := plan.Cell.Combo.Variant
	path := plan.BaseB
	cid := ""
	if nodeIdx < len(hmcfg.Cids) {
		cid = hmcfg.Cids[nodeIdx]
	}
	size := directedNodeSize(lcs, path)
	switch variant {
	case "pread64":
		openCalls, fd := r.generateOpenCallSeq(s, sCalls, path,
			r.randOpenFlags(r.target, capNeeded("read"), false))
		if len(openCalls) == 0 || fd == nil {
			return nil
		}
		return append(append(openCalls, r.generatePreadCallsSeq(s, sCalls, path, plan.BLen, plan.BOffset, fd)...),
			r.generateCloseCall(s, sCalls, fd))
	case "pwrite64":
		openCalls, fd := r.generateOpenCallSeq(s, sCalls, path,
			r.randOpenFlags(r.target, capNeeded("write"), false))
		if len(openCalls) == 0 || fd == nil {
			return nil
		}
		data := make([]byte, plan.BLen)
		r.Read(data)
		return append(append(openCalls, r.generatePwriteCallsSeq(s, sCalls, path, data, plan.BOffset, fd)...),
			r.generateCloseCall(s, sCalls, fd))
	case "truncate":
		c := r.generateTruncateCallWithPath(s, sCalls, path, size)
		if c == nil || len(c.Args) < 2 {
			return nil
		}
		ca, ok := c.Args[1].(*ConstArg)
		if !ok {
			return nil
		}
		if plan.Cell.RetB == RetSuccess {
			ca.Val = plan.BLen
		} else {
			// The raced loser form truncates to the current size, i.e. a
			// no-op: if the race goes the other way and the call succeeds, it
			// must not resize the file and break co-batched same-B plans'
			// predictions. (The generator's initial length is a random value
			// from the value model, so it must be overwritten explicitly.)
			ca.Val = size
		}
		return []*Call{c}
	case "getdents64":
		// Fixed page-size buffer (rationale in DAG_KNOWN_ISSUES #19): a tiny
		// count can make getdents64 fail with EINVAL, while the plan predicts
		// Success. generateGetdents64Calls is self-contained (open+op+close).
		return r.generateGetdents64Calls(s, sCalls, path)
	case "rename":
		// Dual-path form: old = source (BaseB2), new = destination (BaseB).
		c := r.generateRenameCallWithPaths(s, sCalls, plan.BaseB2, plan.BaseB)
		if c == nil {
			return nil
		}
		return []*Call{c}
	default:
		flags := directedNodeFlags(r, lcs, path)
		if plan.FreshSame && plan.Cell.TypeB && variant == "open" {
			// A fresh directory path is not in the tree yet, so the tree-based
			// flag lookup cannot see its type; the planned open must carry
			// O_DIRECTORY (O_RDWR on a directory would fail EISDIR and never
			// produce the planned pair).
			flags = r.target.GetConst("O_DIRECTORY")
		}
		return r.generateCallByName(s, sCalls, variant, path, "", cid, nil, false, size, flags)
	}
}

// hasNilCall reports whether calls contains a nil element (the generators can
// append nil when an optional syscall metadata lookup fails).
func hasNilCall(calls []*Call) bool {
	for _, c := range calls {
		if c == nil {
			return true
		}
	}
	return false
}

// insertProgCalls inserts calls at pos (the DCT insertion convention).
func insertProgCalls(p *Prog, pos int, calls []*Call) {
	if pos >= len(p.Calls) {
		p.Calls = append(p.Calls, calls...)
		return
	}
	p.Calls = append(p.Calls[:pos], append(calls, p.Calls[pos:]...)...)
}

// validateDirectedPlanLive re-checks a plan's tree assumptions against the
// live FileTree right before commit. It only reads (FindNode takes the tree
// lock) and never mutates; on any mismatch insertDirectedBatch returns false,
// accounted as insertFail and never as a cell miss. Without the recheck a
// plan whose prediction silently drifted (missing/renamed base, type change,
// size change moving the offset bucket, empty rmdir target) would emit calls
// that cannot produce the planned cell, and the post-execution miss
// accounting would eventually blacklist a reachable cell.
func validateDirectedPlanLive(hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, plan *directedPlan) bool {
	ft := lcs.FileTree
	if ft == nil {
		return false
	}
	if plan.Cell.Combo.Variant != "rename" && plan.BaseB2 != "" && !plan.FreshB {
		return false
	}
	if plan.FreshA || plan.FreshA2 || plan.FreshB || plan.FreshSame {
		return validateFreshPlanLive(ft, hmcfg, plan)
	}
	a := ft.FindNode(plan.BaseA)
	if a == nil {
		return false
	}
	mkNode := func(n *FileNode) directedNode {
		return directedNode{
			path:     n.FullPath,
			isDir:    n.Type != NodeTypeFile,
			size:     n.Size,
			depth:    DepthBucketOf(n.FullPath),
			persist:  pathInDir(n.FullPath, hmcfg.Persistence_dir),
			nonEmpty: len(n.Children) > 0,
		}
	}
	da := mkNode(a)
	if !nodeFitsConstraint(da, plan.Cell.Combo.Root) {
		return false
	}
	if plan.Cell.Combo.Variant == "rename" {
		src := ft.FindNode(plan.BaseB2)
		if src == nil || plan.BaseB2 == plan.BaseB {
			return false
		}
		// The kernel rejects cross-directory renames (hmdfs_rename_merge
		// compares the parent paths and returns EPERM), so the pair must be
		// same-parent to be reachable at all.
		if !renameSameParent(plan.BaseB2, plan.BaseB) {
			return false
		}
		var db directedNode
		switch plan.Cell.RetB {
		case RetSuccess:
			if plan.RenameReplace {
				// E: the destination exists, is a same-class non-TmpDir
				// sibling, and (for directories) is still empty — a non-empty
				// target is the ENOTEMPTY form instead.
				dst := ft.FindNode(plan.BaseB)
				if dst == nil || dst.Type == NodeTypeTmpDir {
					return false
				}
				if (dst.Type == NodeTypeFile) != (src.Type == NodeTypeFile) {
					return false
				}
				if src.Type != NodeTypeFile && len(dst.Children) > 0 {
					return false
				}
				db = mkNode(dst)
			} else {
				// F: fresh same-parent destination; it must not exist yet and
				// keeps the source's type (rename preserves it).
				if ft.FindNode(plan.BaseB) != nil {
					return false
				}
				db = directedNode{
					path:    plan.BaseB,
					isDir:   src.Type != NodeTypeFile,
					size:    src.Size,
					depth:   DepthBucketOf(plan.BaseB),
					persist: pathInDir(plan.BaseB, hmcfg.Persistence_dir),
				}
			}
		case RetFailure:
			if plan.RenameReplace {
				return false
			}
			// ENOTEMPTY: destination is an existing non-empty plain dir and
			// the source is a directory.
			dst := ft.FindNode(plan.BaseB)
			if dst == nil || dst.Type != NodeTypeDir || len(dst.Children) == 0 {
				return false
			}
			if src.Type == NodeTypeFile {
				return false
			}
			db = mkNode(dst)
		case RetENOENT:
			// The batch's root (unlink on the Same path) removes the source
			// before the variant runs, so the rename lookup fails. The tree
			// still has the source at validation time (the root runs only at
			// execution), and the destination shape is irrelevant.
			if plan.RenameReplace || plan.Cell.Combo.Root != "unlink" ||
				plan.Cell.Combo.Rel != PathSame {
				return false
			}
			db = directedNode{
				path:    plan.BaseB,
				isDir:   src.Type != NodeTypeFile,
				size:    src.Size,
				depth:   DepthBucketOf(plan.BaseB),
				persist: pathInDir(plan.BaseB, hmcfg.Persistence_dir),
			}
		default:
			return false
		}
		if db.isDir != plan.Cell.TypeB || db.depth != plan.Cell.DepthB ||
			db.persist != plan.Cell.PersistB {
			return false
		}
		if plan.Cell.Combo.Rel == PathSame {
			// Same classifies through the source path (prePath), not dst; the
			// source is then the root base and must be A-eligible.
			return src.Type != NodeTypeTmpDir && plan.BaseA == plan.BaseB2
		}
		if plan.BaseA == plan.BaseB || plan.BaseA == plan.BaseB2 {
			return false
		}
		return pathRelBetween(plan.BaseA, plan.BaseB) == plan.Cell.Combo.Rel
	}
	b := ft.FindNode(plan.BaseB)
	if b == nil {
		return false
	}
	db := mkNode(b)
	if !nodeFitsConstraint(db, plan.Cell.Combo.Variant) {
		return false
	}
	bnTmp := &directedBNode{directedNode: db}
	if !variantKindAllowed(plan.Cell.Combo.Variant, plan.Cell.RetB, bnTmp, plan.Cell.Combo.Root, plan.Cell.Combo.Rel) {
		return false
	}
	if db.isDir != plan.Cell.TypeB || db.depth != plan.Cell.DepthB ||
		db.persist != plan.Cell.PersistB {
		return false
	}
	if pathRelBetween(plan.BaseA, plan.BaseB) != plan.Cell.Combo.Rel {
		return false
	}
	if offsetVariants[plan.Cell.Combo.Variant] && plan.Cell.RetB != RetENOENT {
		pos := plan.BOffset
		if plan.Cell.Combo.Variant == "truncate" {
			pos = plan.BLen
		}
		if bucketOfOffset(db.size, pos) != plan.Cell.OffB {
			return false
		}
	}
	return true
}

// freshSameForm is one realizable (root, variant) form of a fresh-fresh pair,
// pinned by the return bucket, offset bucket and created type the variant
// observes. The table is the single source of truth for both generation
// (planDirectedCells) and the live recheck (validateFreshPlanLive).
//
// Semantics: the root creates the fresh path (creat -> file, mkdir -> dir) and
// the ret prediction assumes the root executes first (HB always; CC by race),
// so the variant sees the root's outcome. ENOENT observations come from the
// variant winning the race; their offset bucket is NA because a failed lookup
// never enters the offset-bucket space (FuncLookup), and their TypeB is the
// root's created type: the event-time mode is zero for a negative lookup, so
// eventIsDir falls back to the post-execution tree snapshot, which the root's
// later creation fills in. The ENOENT forms keep their HB twin as that
// coherence probe: an HB placement can only observe ENOENT if the completed
// creation is invisible to the later lookup (a bug clue), so the cell is
// planned, not a phantom.
type freshSameForm struct {
	root, variant string
	ret           RetBucket
	off           uint64
	isDir         bool
}

var freshSameForms = []freshSameForm{
	// A4 create/delete table (semantics unchanged).
	{"creat", "creat", RetSuccess, offsetBucketNA, false},
	{"mkdir", "mkdir", RetEEXIST, offsetBucketNA, true},
	{"creat", "mkdir", RetEEXIST, offsetBucketNA, true},
	{"mkdir", "creat", RetFailure, offsetBucketNA, false},
	{"creat", "unlink", RetSuccess, offsetBucketNA, false},
	{"creat", "unlink", RetENOENT, offsetBucketNA, false},
	{"mkdir", "rmdir", RetSuccess, offsetBucketNA, true},
	{"mkdir", "rmdir", RetENOENT, offsetBucketNA, true},
	// G-c scope S: lookup/metadata/open races against the create.
	{"creat", "stat", RetSuccess, offsetBucketNA, false},
	{"creat", "stat", RetENOENT, offsetBucketNA, false},
	{"creat", "chmod", RetSuccess, offsetBucketNA, false},
	{"creat", "chmod", RetENOENT, offsetBucketNA, false},
	{"creat", "open", RetSuccess, offsetBucketNA, false},
	{"creat", "open", RetENOENT, offsetBucketNA, false},
	// G-c scope S: the newly created inode's size-0 data path (the first I/O
	// of the generated read/write sequence is always at offset 0).
	{"creat", "read", RetSuccess, offsetBucketZero, false},
	{"creat", "write", RetSuccess, offsetBucketZero, false},
	// G-c scope S: dir-root variants (stat/chmod/open/getdents64 on the fresh
	// directory).
	{"mkdir", "stat", RetSuccess, offsetBucketNA, true},
	{"mkdir", "stat", RetENOENT, offsetBucketNA, true},
	{"mkdir", "chmod", RetSuccess, offsetBucketNA, true},
	{"mkdir", "chmod", RetENOENT, offsetBucketNA, true},
	{"mkdir", "open", RetSuccess, offsetBucketNA, true},
	{"mkdir", "open", RetENOENT, offsetBucketNA, true},
	{"mkdir", "getdents64", RetSuccess, offsetBucketNA, true},
}

// freshSameFormAllowed reports whether (root, variant) with the given cell
// margins is a pinned fresh-fresh form.
func freshSameFormAllowed(root, variant string, retB RetBucket, offB uint64, isDir bool) bool {
	for _, fp := range freshSameForms {
		if fp.root == root && fp.variant == variant && fp.ret == retB &&
			fp.off == offB && fp.isDir == isDir {
			return true
		}
	}
	return false
}

// validateFreshPlanLive rechecks a plan whose A, B, or both paths are fresh
// (materialized at batch assembly): the fresh paths must be absent yet, their
// parent must be an existing directory, and the counterpart keeps the ordinary
// liveness requirements.
func validateFreshPlanLive(ft *FileTree, hmcfg *Hmdfs_config, plan *directedPlan) bool {
	mkNode := func(n *FileNode) directedNode {
		return directedNode{
			path:     n.FullPath,
			isDir:    n.Type != NodeTypeFile,
			size:     n.Size,
			depth:    DepthBucketOf(n.FullPath),
			persist:  pathInDir(n.FullPath, hmcfg.Persistence_dir),
			nonEmpty: len(n.Children) > 0,
		}
	}
	persistOf := func(p string) bool {
		return pathInDir(p, hmcfg.Persistence_dir)
	}
	checkExistingB := func(db directedNode) bool {
		if !nodeFitsConstraint(db, plan.Cell.Combo.Variant) {
			return false
		}
		if db.isDir != plan.Cell.TypeB || db.depth != plan.Cell.DepthB ||
			db.persist != plan.Cell.PersistB {
			return false
		}
		bn := &directedBNode{directedNode: db}
		if !variantKindAllowed(plan.Cell.Combo.Variant, plan.Cell.RetB, bn,
			plan.Cell.Combo.Root, plan.Cell.Combo.Rel) {
			return false
		}
		if offsetVariants[plan.Cell.Combo.Variant] && plan.Cell.RetB != RetENOENT {
			pos := plan.BOffset
			if plan.Cell.Combo.Variant == "truncate" {
				pos = plan.BLen
			}
			if bucketOfOffset(db.size, pos) != plan.Cell.OffB {
				return false
			}
		}
		return true
	}
	switch {
	case plan.FreshSame:
		parent := ft.FindNode(plan.BaseA2)
		if parent == nil || parent.Type == NodeTypeFile || parent.Type == NodeTypeTmpDir {
			return false
		}
		if plan.BaseA == "" || plan.BaseA != plan.BaseB || ft.FindNode(plan.BaseA) != nil {
			return false
		}
		if GetParentDir(plan.BaseA) != plan.BaseA2 || plan.Cell.Combo.Rel != PathSame {
			return false
		}
		if !freshSameFormAllowed(plan.Cell.Combo.Root, plan.Cell.Combo.Variant,
			plan.Cell.RetB, plan.Cell.OffB, plan.Cell.TypeB) {
			return false
		}
		if DepthBucketOf(plan.BaseA) != plan.Cell.DepthB || persistOf(plan.BaseA) != plan.Cell.PersistB {
			return false
		}
		return true
	case plan.FreshA:
		parent := ft.FindNode(plan.BaseA2)
		if parent == nil || parent.Type == NodeTypeFile || parent.Type == NodeTypeTmpDir {
			return false
		}
		if plan.Cell.Combo.Root != "mkdir" && plan.Cell.Combo.Root != "creat" {
			return false
		}
		if plan.BaseA == "" || ft.FindNode(plan.BaseA) != nil ||
			GetParentDir(plan.BaseA) != plan.BaseA2 {
			return false
		}
		b := ft.FindNode(plan.BaseB)
		if b == nil {
			return false
		}
		if !checkExistingB(mkNode(b)) {
			return false
		}
		return pathRelBetween(plan.BaseA, plan.BaseB) == plan.Cell.Combo.Rel
	case plan.FreshA2:
		// Rename root: existing source, fresh same-parent destination.
		src := ft.FindNode(plan.BaseA)
		if src == nil || src.Type == NodeTypeTmpDir || plan.Cell.Combo.Root != "rename" {
			return false
		}
		if plan.BaseA2 == "" || ft.FindNode(plan.BaseA2) != nil ||
			!renameSameParent(plan.BaseA, plan.BaseA2) {
			return false
		}
		b := ft.FindNode(plan.BaseB)
		if b == nil {
			return false
		}
		if !checkExistingB(mkNode(b)) {
			return false
		}
		if plan.Cell.Combo.Rel == PathSame {
			return plan.BaseA == plan.BaseB
		}
		if plan.BaseA == plan.BaseB || plan.BaseA2 == plan.BaseB {
			return false
		}
		return pathRelBetween(plan.BaseA2, plan.BaseB) == plan.Cell.Combo.Rel
	default: // FreshB: synthesized mkdir/creat under BaseB2.
		parent := ft.FindNode(plan.BaseB2)
		if parent == nil || parent.Type == NodeTypeFile {
			return false
		}
		if plan.Cell.Combo.Variant != "mkdir" && plan.Cell.Combo.Variant != "creat" {
			return false
		}
		if plan.BaseB == "" || ft.FindNode(plan.BaseB) != nil ||
			GetParentDir(plan.BaseB) != plan.BaseB2 {
			return false
		}
		wantDir := plan.Cell.Combo.Variant == "mkdir"
		if plan.Cell.TypeB != wantDir || plan.Cell.RetB != RetSuccess ||
			plan.Cell.OffB != offsetBucketNA {
			return false
		}
		if DepthBucketOf(plan.BaseB) != plan.Cell.DepthB || persistOf(plan.BaseB) != plan.Cell.PersistB {
			return false
		}
		a := ft.FindNode(plan.BaseA)
		if a == nil || !nodeFitsConstraint(mkNode(a), plan.Cell.Combo.Root) {
			return false
		}
		return pathRelBetween(plan.BaseA, plan.BaseB) == plan.Cell.Combo.Rel
	}
}

// insertDirectedBatch realizes batch on cloned progs: the root call on node 0
// (at insertPos) and one variant per plan on nodes 1..M, aligned to the root's
// reference (HB: first boundary after it; CC: closest boundary). Every
// precondition — live recheck of all plans, root generation, per-node timing
// anchors, variant generation — is evaluated before the commit, so a failure
// aborts the whole batch and leaves ps untouched (one insertFail, never a
// cell miss).
func insertDirectedBatch(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, batch *directedBatch) bool {
	if batch == nil || lcs == nil || hmcfg == nil || len(batch.Plans) == 0 {
		return false
	}
	if len(ps) < 1+len(batch.Plans) {
		return false
	}
	// Ledger protection: the assembler's one-state-authority and E reservation
	// postconditions. A violation is a future assembler bug, degrading it to
	// insertFail keeps the cell ledger clean.
	if !batchBaseBAuthorityOK(batch.Plans) {
		return false
	}
	for i := range batch.Plans {
		plan := &batch.Plans[i]
		if plan.NodeX != i+1 {
			return false
		}
		// HB pairs require the temporally earlier vertex (the pair's root) to
		// be a modifier (ExtractPairs); no plan should ever violate this, so a
		// future assembler drift degrades to insertFail instead of charging
		// spurious misses.
		if plan.Cell.Temporal == TemporalHB && !isModifierCall(plan.Cell.Combo.Root) {
			return false
		}
		// Ledger-protection assertion, unreachable from PickDirectedBatch:
		// the batch hangs off one root call on batch.BaseA, so a plan from
		// another root group would produce pairs that can never match its
		// pending cell (up to cellMissThreshold spurious misses per cell,
		// poisoning cellUnreachable). Degrading a future assembler bug to
		// insertFail keeps the cell ledger clean.
		if plan.Cell.Combo.Root != batch.Root || plan.BaseA != batch.BaseA {
			return false
		}
		if plan.FreshA2 && plan.BaseA2 != batch.BaseA2 {
			return false
		}
		if !validateDirectedPlanLive(hmcfg, lcs, plan) {
			return false
		}
	}
	p0 := ps[0]
	insertPos := r.biasedRand(len(p0.Calls)+1, 5)
	refTime := int64(-1)
	if insertPos == 0 {
		refTime = 0
	} else if p0.Calls[insertPos-1].CheckInfo != nil {
		refTime = int64(p0.Calls[insertPos-1].CheckInfo.Etime) - lcs.tscoffFor(0)
	}
	// Root on a clone of node 0.
	clone0 := p0.Clone()
	if clone0 == nil {
		return false
	}
	cid0 := ""
	if len(hmcfg.Cids) > 0 {
		cid0 = hmcfg.Cids[0]
	}
	var rootCalls []*Call
	if batch.Root == "rename" {
		if batch.BaseA2 == "" {
			return false
		}
		if c := r.generateRenameCallWithPaths(stateFromProg(clone0), sCalls, batch.BaseA, batch.BaseA2); c != nil {
			rootCalls = []*Call{c}
		}
	} else {
		rootCalls = r.generateCallByName(stateFromProg(clone0), sCalls, batch.Root,
			batch.BaseA, "", cid0, nil, false, directedNodeSize(lcs, batch.BaseA),
			directedNodeFlags(r, lcs, batch.BaseA))
	}
	if len(rootCalls) == 0 || hasNilCall(rootCalls) {
		return false
	}
	insertProgCalls(clone0, insertPos, rootCalls)
	// Variants on clones of nodes 1..M, aligned to the root's reference.
	clones := make([]*Prog, len(batch.Plans))
	for i := range batch.Plans {
		plan := &batch.Plans[i]
		node := plan.NodeX
		cloneX := ps[node].Clone()
		if cloneX == nil {
			return false
		}
		posX := -1
		if plan.Cell.Temporal == TemporalHB {
			if p := firstBoundaryAfter(cloneX, refTime, lcs.tscoffFor(node)); p >= 0 {
				posX = p
			}
		} else {
			if p := TimeAlignedInsertPos(cloneX, refTime, lcs.tscoffFor(node)); p >= 0 {
				posX = p
			}
		}
		if posX < 0 {
			// No timing anchor: the planned temporal form cannot be placed in
			// a controlled way. Abort the batch (insertFail, fall back to the
			// regular DCT insertion) instead of inserting at an index and
			// risking a spurious cell miss.
			return false
		}
		variantCalls := directedVariantCallsSeq(r, sCalls, lcs, hmcfg, node, cloneX, plan)
		if len(variantCalls) == 0 || hasNilCall(variantCalls) {
			return false
		}
		insertProgCalls(cloneX, posX, variantCalls)
		clones[i] = cloneX
	}
	// Ledger protection: a previous directed batch must have been resolved
	// (ResolveDirectedPending runs after every execution) before another one
	// is recorded; otherwise the earlier ledger is silently dropped. Not
	// reachable at procs=1; a future concurrent caller degrades to insertFail
	// instead of losing the ledger.
	if len(lcs.PendingDirected) > 0 {
		return false
	}
	// Commit: every prog at once, then record the exact pending cells.
	ps[0] = clone0
	for i := range clones {
		ps[batch.Plans[i].NodeX] = clones[i]
	}
	pending := make([]DirectedPending, len(batch.Plans))
	for i := range batch.Plans {
		pending[i] = DirectedPending{Cell: batch.Plans[i].Cell}
	}
	lcs.PendingDirected = pending
	covBoard.noteDirectedPlans(len(batch.Plans))
	return true
}
