package prog

import (
	"sort"
	"strings"
)

// causalAnchor is the existing p0 call that is placed immediately before the
// directed root. It is deliberately not a directed cell: the C2 batch still
// owns coverage/pending accounting.
type causalAnchor struct {
	Pos       GroupPosition
	Path      string
	Source    string
	Dest      string
	PathKind  AnchorPathKind
	IsDir     bool
	InsertPos int
	RefTime   int64
	Relation  PathRelation
}

// causalRootPlan is the first-layer (anchor -> root) plan. Its Cell is the C1
// causal cell used to pick the root; it is not a C2 pending cell.
type causalRootKind int

const (
	causalRootNormal causalRootKind = iota
	causalRootFresh
	causalRootRename
)

type causalRootPlan struct {
	Anchor             causalAnchor
	Cell               cellKey
	RootCall           string
	RootPath           string
	RootOffset         uint64
	RootLen            uint64
	RootKind           causalRootKind
	RootParent         string
	RootParentKind     AnchorPathKind
	RootParentIsDir    bool
	RootParentIsAnchor bool
	RootSource         string
	RootDest           string
	RootRelationPath   string
	RootRenameReplace  bool
}

// causalRootCalls is the subset we can construct from an existing p0 path
// without the fresh-path materialization passes. Offset roots use their
// zero-bucket form here; rename roots are added by a later stage.
var causalRootCalls = []string{
	"open", "read", "write", "pread64", "pwrite64", "truncate",
	"stat", "chmod", "fsync", "fdatasync", "getdents64", "unlink",
	"mkdir", "creat", "rename",
}

// pickCausalRootBatch tries the two-layer causal-root construction first. A
// false return means "not constructible", and the caller falls back to the
// normal directed planner.
func pickCausalRootBatch(ps []*Prog, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, r *randGen) (*directedBatch, bool) {
	if lcs == nil || lcs.FileTree == nil || hmcfg == nil || r == nil || len(ps) < 2 {
		return nil, false
	}
	if !CoverageBoardReady() || len(hmcfg.Cids) != len(ps) {
		return nil, false
	}
	plans := collectCausalRootPlans(ps[0], lcs, hmcfg, r)
	if len(plans) == 0 {
		return nil, false
	}
	c1 := plans[r.Intn(len(plans))]
	if c1.RootKind == causalRootFresh {
		c1.RootPath = directedFreshName(c1.RootCall, c1.RootParent, r.Rand)
	}
	planByCell, cells := planCausalC2CellsWithParent(ps, lcs, hmcfg, c1.RootCall, c1.RootPath, c1.RootParent, c1.RootParentKind, c1.RootParentIsDir, c1.RootParentIsAnchor, c1.RootSource, c1.RootDest, c1.RootRelationPath, c1.RootRenameReplace, c1.RootKind, r)
	if len(cells) == 0 {
		return nil, false
	}
	var picked []*directedPlan
	for _, ck := range cells {
		p := planByCell[ck]
		if p == nil || !covBoard.cellUncovered(p.Cell.toKey()) {
			continue
		}
		picked = append(picked, p)
	}
	if len(picked) == 0 {
		return nil, false
	}
	seed := picked[r.Intn(len(picked))]
	var group []*directedPlan
	for _, p := range picked {
		if p.Cell.Combo.Root == seed.Cell.Combo.Root &&
			p.BaseA == seed.BaseA && p.BaseA2 == seed.BaseA2 &&
			p.RootLinkPath == seed.RootLinkPath {
			group = append(group, p)
		}
	}
	m := len(ps) - 1
	if len(group) < m {
		m = len(group)
	}
	if m <= 0 {
		return nil, false
	}
	// Shuffle, then keep the seed in the batch. The normal planner also samples
	// without replacement; we only need a deterministic, bounded subset here.
	for i := 0; i < m; i++ {
		j := i + r.Intn(len(group)-i)
		group[i], group[j] = group[j], group[i]
	}
	group = group[:m]
	foundSeed := false
	for i, p := range group {
		if p == seed {
			group[0], group[i] = group[i], group[0]
			foundSeed = true
			break
		}
	}
	if !foundSeed {
		group[0] = seed
	}
	for _, p := range group {
		if p.FreshB {
			p.BaseB = directedFreshName(p.Cell.Combo.Variant, p.BaseB2, r.Rand)
		}
		if p.FreshRenameDst {
			p.BaseB = freshRenameDst(p.BaseB2, r.Rand)
			if p.Cell.RetB == RetSuccess && r.Rand.Intn(2) == 0 {
				if dst := pickRenameReplaceDst(lcs.FileTree, p.BaseB2, nil,
					hmcfg.Persistence_dir, p.Cell.PersistB, r.Rand); dst != "" {
					p.BaseB = dst
					p.RenameReplace = true
					p.FreshRenameDst = false
				}
			}
		}
	}
	baseA, baseA2 := c1.RootPath, c1.RootParent
	if c1.RootKind == causalRootRename {
		baseA, baseA2 = c1.RootSource, c1.RootDest
	}
	batch := &directedBatch{
		Root:              c1.RootCall,
		BaseA:             baseA,
		BaseA2:            baseA2,
		RootOffset:        c1.RootOffset,
		RootLen:           c1.RootLen,
		RootKind:          c1.RootKind,
		RootParent:        c1.RootParent,
		RootSource:        c1.RootSource,
		RootDest:          c1.RootDest,
		RootXattrName:     "",
		RootRenameReplace: c1.RootRenameReplace,
		Anchor:            &c1.Anchor,
		Plans:             make([]directedPlan, 0, len(group)),
	}
	if c1.RootCall == "setxattr" {
		batch.RootXattrName = "user.eifuzz"
	}
	for i, p := range group {
		q := *p
		q.NodeX = i + 1
		switch q.Cell.Combo.Variant {
		case "setxattr":
			if q.Cell.Combo.Root == "setxattr" && q.Cell.Combo.Rel == PathSame &&
				batch.RootXattrName != "" {
				q.XattrName = batch.RootXattrName
			} else {
				q.XattrName = "user.eifuzz_set"
			}
		case "getxattr":
			if q.Cell.Combo.Root == "setxattr" && q.Cell.Combo.Rel == PathSame &&
				batch.RootXattrName != "" {
				q.XattrName = batch.RootXattrName
			} else {
				q.XattrName = "user.eifuzz_missing"
			}
		}
		batch.Plans = append(batch.Plans, q)
	}
	batch.Plans = filterBaseBAuthority(batch.Plans)
	for i := range batch.Plans {
		batch.Plans[i].NodeX = i + 1
	}
	if !batchBaseBAuthorityOK(batch.Plans) {
		return nil, false
	}
	return batch, true
}

// causalRootRelPaths returns the root's pre/post paths for C2 relation
// classification. A rename root is the HB A side and uses its destination
// (post-state) for every relation; all other roots have one effective path.
func causalRootRelPaths(rootKind causalRootKind, rootCall, rootPath, rootSource, rootDest, rootRelationPath string) (string, string) {
	if rootKind == causalRootRename {
		// HB A uses the rename's post-state destination. Source/Dest remain
		// available for materializing rename(src,dst).
		return rootDest, rootDest
	}
	if rootCall == "link" || rootCall == "symlink" {
		// The relation uses the explicit role path. Empty preserves the
		// historical target-role behavior; readlink sets rootRelationPath to
		// the linkpath.
		relationPath := rootRelationPath
		if relationPath == "" {
			relationPath = rootSource
			if relationPath == "" {
				relationPath = rootPath
			}
		}
		return relationPath, relationPath
	}
	return rootPath, rootPath
}

// causalVariantRelPaths returns a variant's pre/post paths. A rename variant
// is the B side and uses its source (entry-state) for every relation.
func causalVariantRelPaths(variant, path, source string) (string, string) {
	if variant == "rename" {
		// HB B/CC use the rename's entry-state source. path is the
		// destination used only for materialization and RetB.
		return source, source
	}
	return path, path
}

// causalPairRelation reuses the DAG feedback classifier so causal planning
// sees the same {post,pre} x {post,pre} relations as execution feedback. The
// synthetic vertices carry only paths; SameInode cannot fire (Ino is zero).
func causalPairRelation(rootPre, rootPost, varPre, varPost string) (PathRelation, bool) {
	if rootPost == "" || varPost == "" {
		return PathNoRel, false
	}
	a := &DAGVertex{Path: rootPost, OldPath: rootPre}
	b := &DAGVertex{Path: varPost, OldPath: varPre}
	pr := determinePathRel(a, b, true)
	return dagRelToDctRel(&DAGPair{A: a, B: b, PathRel: pr})
}

// causalC2ComboLive returns the selectable combos for the fixed C2 root.
// The second return value is false when the coverage board is not ready; in
// that case the caller must not apply the filter.
func causalC2ComboLive(profile *DFSProfile, root string) (map[comboKey]bool, bool) {
	if !covBoard.isReady() {
		return nil, false
	}
	if profile == nil {
		profile = hmdfsProfile
	}
	variants := make(map[string]bool)
	for _, variant := range profile.DirectedVariantCalls() {
		if (variant == "link" || variant == "symlink" || variant == "readlink") &&
			!profile.DualPathCausalEnabled() {
			continue
		}
		if variant == "readlink" && root != "symlink" {
			continue
		}
		variants[variant] = true
	}
	for _, fp := range freshSameFormsFor(profile) {
		variants[fp.variant] = true
	}
	rels := []PathRelation{PathSame, PathParent, PathChild, PathSibling}
	combos := make([]comboKey, 0, len(variants)*len(rels))
	for variant := range variants {
		variantRels := rels
		if variant == "readlink" {
			// readlink only follows the root linkpath, so its only
			// constructible relation is Same.
			variantRels = []PathRelation{PathSame}
		}
		for _, rel := range variantRels {
			combos = append(combos, comboKey{Root: root, Variant: variant, Rel: rel})
		}
	}
	return covBoard.rootDirectedSelectableFlags(combos), true
}

// planCausalC2Cells enumerates C2 plans for a root that C1 has already fixed.
// It is a fixed-root enumeration, not a post-filter over the global A-witness
// planner, so a cell is not lost merely because the normal planner sampled a
// different valid A witness for it.
func planCausalC2Cells(ps []*Prog, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, rootCall, rootPath, rootParent, rootSource, rootDest, rootRelationPath string, rootRenameReplace bool, rootKind causalRootKind, r *randGen) (map[cellKey]*directedPlan, []cellKey) {
	return planCausalC2CellsWithParent(ps, lcs, hmcfg, rootCall, rootPath, rootParent,
		AnchorPathUnknown, false, false, rootSource, rootDest, rootRelationPath, rootRenameReplace, rootKind, r)
}

func planCausalC2CellsWithParent(ps []*Prog, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, rootCall, rootPath, rootParent string, rootParentKind AnchorPathKind, rootParentIsDir, rootParentIsAnchor bool, rootSource, rootDest, rootRelationPath string, rootRenameReplace bool, rootKind causalRootKind, r *randGen) (map[cellKey]*directedPlan, []cellKey) {
	planByCell := make(map[cellKey]*directedPlan)
	var cells []cellKey
	if lcs == nil || lcs.FileTree == nil || hmcfg == nil || r == nil {
		return planByCell, cells
	}
	ft := lcs.FileTree
	profile := lcs.profileOrDefault()
	variantCalls := profile.DirectedVariantCalls()
	var dn directedNode
	switch rootKind {
	case causalRootFresh:
		if rootParentIsAnchor {
			if !rootParentKind.IsFresh() || !rootParentIsDir ||
				!freshParentNodeEligible(ft, rootParent) {
				return planByCell, cells
			}
		} else {
			parent := ft.FindNode(rootParent)
			if parent == nil || parent.Type == NodeTypeFile || parent.Type == NodeTypeTmpDir ||
				!aEligible(parent, ft.Root) {
				return planByCell, cells
			}
		}
		isDir := rootCall == "mkdir"
		dn = directedNode{
			path:    rootPath,
			isDir:   isDir,
			depth:   DepthBucketOf(rootPath),
			persist: pathInDir(rootPath, hmcfg.Persistence_dir),
		}
	case causalRootRename:
		source := ft.FindNode(rootSource)
		if source == nil || source.Type == NodeTypeTmpDir {
			return planByCell, cells
		}
		if rootRenameReplace {
			if rootSource == rootDest || !renameSameParent(rootSource, rootDest) {
				return planByCell, cells
			}
			dest := ft.FindNode(rootDest)
			if dest == nil || dest.Type == NodeTypeTmpDir ||
				(dest.Type == NodeTypeFile) != (source.Type == NodeTypeFile) ||
				(dest.Type != NodeTypeFile && len(dest.Children) > 0) {
				return planByCell, cells
			}
			dn = directedNode{
				path:    rootDest,
				isDir:   dest.Type != NodeTypeFile,
				size:    dest.Size,
				depth:   DepthBucketOf(rootDest),
				persist: pathInDir(rootDest, hmcfg.Persistence_dir),
			}
		} else {
			if rootDest == "" || ft.FindNode(rootDest) != nil || !renameSameParent(rootSource, rootDest) {
				return planByCell, cells
			}
			dn = directedNode{
				path:    rootDest,
				isDir:   source.Type != NodeTypeFile,
				depth:   DepthBucketOf(rootDest),
				persist: pathInDir(rootDest, hmcfg.Persistence_dir),
			}
		}
	case causalRootNormal:
		rootNode := ft.FindNode(rootPath)
		if rootNode == nil || rootNode == ft.Root || !aEligible(rootNode, ft.Root) {
			return planByCell, cells
		}
		dn = directedNode{
			path:     rootPath,
			isDir:    rootNode.Type != NodeTypeFile,
			size:     rootNode.Size,
			depth:    DepthBucketOf(rootPath),
			persist:  pathInDir(rootPath, hmcfg.Persistence_dir),
			nonEmpty: len(rootNode.Children) > 0,
		}
	default:
		return planByCell, cells
	}
	if !nodeFitsConstraintFor(profile, dn, rootCall) {
		return planByCell, cells
	}
	supportedRoot := false
	for _, c := range profile.DirectedRootCalls() {
		if c == rootCall {
			supportedRoot = true
			break
		}
	}
	if !supportedRoot {
		return planByCell, cells
	}
	view := buildDirectedView(ft, hmcfg.Persistence_dir)
	planA, planA2 := rootPath, rootParent
	freshA2 := false
	if rootKind == causalRootRename {
		planA, planA2 = rootSource, rootDest
		freshA2 = true
	}
	rootPre, rootPost := causalRootRelPaths(rootKind, rootCall, rootPath, rootSource, rootDest, rootRelationPath)
	destructiveSame := func(varPre string, rel PathRelation) bool {
		if rel != PathSame {
			return false
		}
		return rootCall == "unlink" || (rootKind == causalRootRename && rootPre != "" && rootPre == varPre)
	}
	liveCombos, filterCombos := causalC2ComboLive(profile, rootCall)
	addC2 := func(cell directedCell, baseB, baseB2 string, off, blen uint64, freshRenameDst, freshB, freshSame bool) {
		if (cell.Combo.Variant == "link" || cell.Combo.Variant == "symlink" ||
			cell.Combo.Variant == "readlink") && !profile.DualPathCausalEnabled() {
			return
		}
		if cell.Combo.Variant == "readlink" {
			if rootCall != "symlink" || cell.Temporal != TemporalHB ||
				cell.RetB != RetSuccess {
				return
			}
		}
		ck := cell.toKey()
		if filterCombos && !liveCombos[cell.Combo] {
			return
		}
		if !covBoard.cellUncovered(ck) {
			return
		}
		if _, exists := planByCell[ck]; exists {
			return
		}
		planRootLink := ""
		if rootCall == "link" || rootCall == "symlink" {
			planRootLink = rootDest
		}
		planByCell[ck] = &directedPlan{
			Cell:               cell,
			BaseA:              planA,
			BaseA2:             planA2,
			BaseB:              baseB,
			BaseB2:             baseB2,
			BOffset:            off,
			BLen:               blen,
			FreshA:             rootKind == causalRootFresh,
			FreshA2:            freshA2,
			FreshB:             freshB,
			FreshSame:          freshSame,
			FreshRenameDst:     freshRenameDst,
			RootRenameReplace:  rootRenameReplace,
			Causal:             true,
			RootParentKind:     rootParentKind,
			RootParentIsDir:    rootParentIsDir,
			RootParentIsAnchor: rootParentIsAnchor,
			RootLinkPath:       planRootLink,
			RootRelationPath:   rootRelationPath,
		}
		cells = append(cells, ck)
	}
	if rootKind == causalRootFresh {
		for _, fp := range freshSameFormsFor(profile) {
			if fp.root != rootCall {
				continue
			}
			cell := directedCell{
				Combo:    comboKey{Root: rootCall, Variant: fp.variant, Rel: PathSame},
				RetB:     fp.ret,
				OffB:     fp.off,
				PersistB: pathInDir(rootPath, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(rootPath),
				TypeB:    fp.isDir,
			}
			cell.Temporal = TemporalConcurrent
			addC2(cell, rootPath, "", 0, 0, false, false, true)
			if isModifierCall(rootCall) {
				cell.Temporal = TemporalHB
				addC2(cell, rootPath, "", 0, 0, false, false, true)
			}
		}
	}
	for i := range view.bNodes {
		bn := &view.bNodes[i]
		if bn.freshB {
			rel, ok := causalPairRelation(rootPre, rootPost, bn.path, bn.path)
			if !ok {
				continue
			}
			variant := bn.freshBCall
			kinds, ok := variantKindsFor(variant, rootCall, rel, destructiveSame(bn.path, rel), bn.isDir, profile)
			if !ok {
				continue
			}
			for _, kind := range kinds {
				if !variantKindAllowed(variant, kind.retB, bn, rootCall, rel) {
					continue
				}
				off, blen, ok := realizeDirectedParams(variant, kind.bucket, bn.directedNode, r)
				if !ok {
					continue
				}
				cell := directedCell{
					Combo:    comboKey{Root: rootCall, Variant: variant, Rel: rel},
					RetB:     kind.retB,
					OffB:     kind.bucket,
					PersistB: bn.persist,
					DepthB:   bn.depth,
					TypeB:    bn.isDir,
				}
				cell.Temporal = TemporalConcurrent
				addC2(cell, "", bn.freshParent, off, blen, false, true, false)
				if isModifierCall(rootCall) {
					cell.Temporal = TemporalHB
					addC2(cell, "", bn.freshParent, off, blen, false, true, false)
				}
			}
			continue
		}
		if bn.srcPath != "" {
			variant := "rename"
			varPre, varPost := causalVariantRelPaths(variant, bn.path, bn.srcPath)
			rel, ok := causalPairRelation(rootPre, rootPost, varPre, varPost)
			if !ok {
				continue
			}
			kinds, ok := variantKindsFor(variant, rootCall, rel, destructiveSame(varPre, rel), bn.isDir, profile)
			if !ok {
				continue
			}
			for _, kind := range kinds {
				if !variantKindAllowed(variant, kind.retB, bn, rootCall, rel) {
					continue
				}
				dest := destructiveSame(varPre, rel)
				if dest && destructiveCCExclusive(variant, kind.retB) {
					continue
				}
				off, blen, ok := realizeDirectedParams(variant, kind.bucket, bn.directedNode, r)
				if !ok {
					continue
				}
				cell := directedCell{
					Combo:    comboKey{Root: rootCall, Variant: variant, Rel: rel},
					RetB:     kind.retB,
					OffB:     kind.bucket,
					PersistB: bn.cellPersist(hmcfg.Persistence_dir),
					DepthB:   bn.cellDepth(),
					TypeB:    bn.isDir,
				}
				baseB := bn.path
				if bn.renameFresh {
					baseB = ""
				}
				cell.Temporal = TemporalConcurrent
				addC2(cell, baseB, bn.srcPath, off, blen, bn.renameFresh, false, false)
				if isModifierCall(rootCall) && !(dest && destructiveHBDoomed(variant, kind.retB)) {
					cell.Temporal = TemporalHB
					addC2(cell, baseB, bn.srcPath, off, blen, bn.renameFresh, false, false)
				}
			}
			continue
		}
		varPre, varPost := causalVariantRelPaths("", bn.path, "")
		rel, ok := causalPairRelation(rootPre, rootPost, varPre, varPost)
		if !ok {
			continue
		}
		for _, variant := range variantCalls {
			if variant == "rename" {
				continue // rename is a later stage
			}
			if variant == "readlink" {
				// readlink is the single linkpath-follower form below: its B
				// must be the root LinkPath, not an arbitrary tree node.
				continue
			}
			if (variant == "link" || variant == "symlink") &&
				!profile.DualPathCausalEnabled() {
				continue
			}
			if !nodeFitsConstraintFor(profile, bn.directedNode, variant) {
				continue
			}
			kinds, ok := variantKindsFor(variant, rootCall, rel, destructiveSame(varPre, rel), bn.isDir, profile)
			if !ok {
				continue
			}
			for _, kind := range kinds {
				if !variantKindAllowed(variant, kind.retB, bn, rootCall, rel) {
					continue
				}
				dest := destructiveSame(varPre, rel)
				if dest && destructiveCCExclusive(variant, kind.retB) {
					continue
				}
				off, blen, ok := realizeDirectedParams(variant, kind.bucket, bn.directedNode, r)
				if !ok {
					continue
				}
				cell := directedCell{
					Combo:    comboKey{Root: rootCall, Variant: variant, Rel: rel},
					RetB:     kind.retB,
					OffB:     kind.bucket,
					PersistB: bn.persist,
					DepthB:   bn.depth,
					TypeB:    bn.isDir,
				}
				cell.Temporal = TemporalConcurrent
				addC2(cell, bn.path, "", off, blen, false, false, false)
				if isModifierCall(rootCall) && !(dest && destructiveHBDoomed(variant, kind.retB)) {
					cell.Temporal = TemporalHB
					addC2(cell, bn.path, "", off, blen, false, false, false)
				}
			}
		}
	}
	if profile.DualPathCausalEnabled() && rootCall == "symlink" && rootDest != "" {
		// readlink observes the linkpath role of the symlink root; the
		// ordinary target-based root relation does not apply here.
		if rel, ok := causalPairRelation(rootDest, rootDest, rootDest, rootDest); ok {
			cell := directedCell{
				Combo:    comboKey{Root: rootCall, Variant: "readlink", Rel: rel},
				Temporal: TemporalHB,
				RetB:     RetSuccess,
				OffB:     offsetBucketNA,
				PersistB: pathInDir(rootDest, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(rootDest),
				TypeB:    false,
			}
			addC2(cell, rootDest, "", 0, 0, false, false, false)
			if p := planByCell[cell.toKey()]; p != nil {
				p.RootLinkPath = rootDest
				p.RootRelationPath = rootDest
			}
		}
	}
	return planByCell, cells
}

// collectCausalRootPlans enumerates C1 plans that are constructible from p0
// and target a currently uncovered anchor->root HB cell. The C1 cell is
// deduplicated: several anchors/paths may realize the same abstract cell.
func collectCausalRootPlans(p0 *Prog, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, r *randGen) []*causalRootPlan {
	if lcs == nil {
		return nil
	}
	ft := lcs.FileTree
	if p0 == nil || ft == nil || hmcfg == nil || p0.Target == nil {
		return nil
	}
	profile := lcs.profileOrDefault()
	rootCalls := profile.Calls.CausalRootCalls
	if len(rootCalls) == 0 {
		rootCalls = causalRootCalls
	}
	byCell := make(map[cellKey]*causalRootPlan)
	for ci, call := range p0.Calls {
		if call == nil || call.Meta == nil {
			continue
		}
		if !isModifierCall(call.Meta.Name) {
			continue
		}
		pos := GroupPosition{ProgIdx: 0, CallIdx: ci}
		path, source, dest := causalAnchorPaths(p0, pos)
		if path == "" {
			continue
		}
		ciInfo := call.CheckInfo
		if ciInfo == nil {
			continue
		}
		anchor := causalAnchor{
			Pos:       pos,
			Path:      path,
			Source:    source,
			Dest:      dest,
			InsertPos: ci + 1,
			RefTime:   int64(ciInfo.Etime) - lcs.tscoffFor(0),
			Relation:  PathNoRel,
		}
		if !validateCausalAnchor(p0, &anchor, lcs) {
			continue
		}
		info, ok := classifyAnchorPath(p0, ft, pos, path, true)
		if !ok || info.Kind == AnchorPathUnknown {
			continue
		}
		if node := ft.FindNode(path); node == ft.Root {
			continue
		}
		anchor.PathKind = info.Kind
		anchor.IsDir = info.IsDir
		// Rename anchor A is its post-state destination; link/symlink keep
		// the target/linkpath role candidates, and ordinary anchors are the
		// same path on both sides.
		anchorPre, anchorPost := anchor.Source, anchor.Path
		if strings.Contains(call.Meta.Name, "rename") {
			anchorPre, anchorPost = anchor.Path, anchor.Path
		}
		var anchorNodes []*FileNode
		if node := ft.FindNode(path); node != nil {
			anchorNodes = append(anchorNodes, node)
		}
		if sourceNode := ft.FindNode(source); sourceNode != nil {
			if len(anchorNodes) == 0 || sourceNode != anchorNodes[0] {
				anchorNodes = append(anchorNodes, sourceNode)
			}
		}
		rels := []PathRelation{PathSame, PathParent, PathChild, PathSibling}
		pathsByRel := make(map[PathRelation][]string, len(rels))
		for _, anchorNode := range anchorNodes {
			for _, rel := range rels {
				pathsByRel[rel] = append(pathsByRel[rel], causalRootPaths(ft, anchorNode, rel)...)
			}
		}
		if info.Kind.IsFresh() {
			for _, rel := range rels {
				pathsByRel[rel] = append(pathsByRel[rel], causalRootPathsByPath(ft, path, info, rel)...)
			}
		}
		addFreshPlan := func(rootCall string, rel PathRelation, parent string, parentKind AnchorPathKind, parentIsDir, parentIsAnchor bool) {
			if parent == "" {
				return
			}
			if !parentIsAnchor {
				parentNode := ft.FindNode(parent)
				if parentNode == nil || parentNode.Type == NodeTypeFile ||
					!aEligible(parentNode, ft.Root) {
					return
				}
			} else if !parentKind.IsFresh() || !parentIsDir ||
				!freshParentNodeEligible(ft, parent) {
				return
			}
			rootPath := directedFreshPlaceholder(rootCall, parent)
			if !causalRenameRootPathAllowed(anchor, rootPath) {
				return
			}
			actualRel, ok := causalPairRelation(anchorPre, anchorPost, "", rootPath)
			if !ok {
				return
			}
			isDir := rootCall == "mkdir"
			ck := cellKey{
				Root:     call.Meta.CallName,
				Variant:  rootCall,
				Rel:      actualRel,
				Temporal: TemporalHB,
				RetA:     RetSuccess,
				RetB:     RetSuccess,
				OffB:     offsetBucketNA,
				PersistB: pathInDir(rootPath, hmcfg.Persistence_dir),
				DepthB:   DepthBucketOf(rootPath),
				TypeB:    isDir,
			}
			if !covBoard.cellUncovered(ck) {
				return
			}
			a := anchor
			a.Relation = actualRel
			plan := &causalRootPlan{
				Anchor:             a,
				Cell:               ck,
				RootCall:           rootCall,
				RootPath:           rootPath,
				RootKind:           causalRootFresh,
				RootParent:         parent,
				RootParentKind:     parentKind,
				RootParentIsDir:    parentIsDir,
				RootParentIsAnchor: parentIsAnchor,
			}
			if _, ok := byCell[ck]; !ok {
				byCell[ck] = plan
			}
		}
		for _, rootCall := range rootCalls {
			if (rootCall == "link" || rootCall == "symlink") &&
				!profile.DualPathCausalEnabled() {
				continue
			}
			for _, rel := range rels {
				paths := pathsByRel[rel]
				for _, rootPath := range paths {
					if !causalRenameRootPathAllowed(anchor, rootPath) {
						continue
					}
					rootNode := ft.FindNode(rootPath)
					if rootNode == nil || rootNode == ft.Root || !aEligible(rootNode, ft.Root) {
						continue
					}
					if !causalRootFitsFor(profile, rootCall, rootNode) {
						continue
					}
					if rootCall == "link" || rootCall == "symlink" {
						linkPath := r.freshLinkPath(rootPath, rootCall)
						if linkPath == "" || ft.FindNode(linkPath) != nil ||
							!causalRenameRootPathAllowed(anchor, linkPath) {
							continue
						}
						// C1: link/symlink is the B side. The root has both
						// target and linkpath roles; generate one plan per role.
						// A cell collision keeps the first (target-role) plan,
						// matching the role-free cell identity.
						for _, relationPath := range []string{rootPath, linkPath} {
							actualRel, ok := causalPairRelation(anchorPre, anchorPost,
								relationPath, relationPath)
							if !ok {
								continue
							}
							isDir := relationPath == rootPath && rootNode.Type != NodeTypeFile
							ck := cellKey{
								Root:     call.Meta.CallName,
								Variant:  rootCall,
								Rel:      actualRel,
								Temporal: TemporalHB,
								RetA:     RetSuccess,
								RetB:     RetSuccess,
								OffB:     offsetBucketNA,
								PersistB: pathInDir(relationPath, hmcfg.Persistence_dir),
								DepthB:   DepthBucketOf(relationPath),
								TypeB:    isDir,
							}
							if !covBoard.cellUncovered(ck) {
								continue
							}
							a := anchor
							a.Relation = actualRel
							plan := &causalRootPlan{
								Anchor:           a,
								Cell:             ck,
								RootCall:         rootCall,
								RootPath:         rootPath,
								RootKind:         causalRootNormal,
								RootParent:       parentDirOrRoot(rootPath),
								RootSource:       rootPath,
								RootDest:         linkPath,
								RootRelationPath: relationPath,
							}
							if _, ok := byCell[ck]; !ok {
								byCell[ck] = plan
							}
						}
						continue
					}
					actualRel, ok := causalPairRelation(anchorPre, anchorPost, "", rootPath)
					if !ok {
						continue
					}
					dn := directedNode{
						path:     rootPath,
						isDir:    rootNode.Type != NodeTypeFile,
						size:     rootNode.Size,
						depth:    DepthBucketOf(rootPath),
						persist:  pathInDir(rootPath, hmcfg.Persistence_dir),
						nonEmpty: len(rootNode.Children) > 0,
					}
					if !nodeFitsConstraintFor(profile, dn, rootCall) {
						continue
					}
					for _, args := range causalRootArgs(rootCall, dn, r) {
						ck := cellKey{
							Root:     call.Meta.CallName,
							Variant:  rootCall,
							Rel:      actualRel,
							Temporal: TemporalHB,
							RetA:     RetSuccess,
							RetB:     RetSuccess,
							OffB:     args.offB,
							PersistB: dn.persist,
							DepthB:   dn.depth,
							TypeB:    dn.isDir,
						}
						if !covBoard.cellUncovered(ck) {
							continue
						}
						a := anchor
						a.Relation = actualRel
						plan := &causalRootPlan{
							Anchor:     a,
							Cell:       ck,
							RootCall:   rootCall,
							RootPath:   rootPath,
							RootOffset: args.offset,
							RootLen:    args.length,
						}
						if _, ok := byCell[ck]; !ok {
							byCell[ck] = plan
						}
					}
				}
				if (rootCall == "mkdir" || rootCall == "creat") &&
					(rel == PathChild || rel == PathSibling) {
					for _, anchorNode := range anchorNodes {
						parent := ""
						switch rel {
						case PathChild:
							if anchorNode.Type != NodeTypeFile {
								parent = anchorNode.FullPath
							}
						case PathSibling:
							parent = GetParentDir(anchorNode.FullPath)
						}
						if parent == "" {
							continue
						}
						parentNode := ft.FindNode(parent)
						if parentNode == nil || parentNode.Type == NodeTypeFile ||
							!aEligible(parentNode, ft.Root) {
							continue
						}
						rootPath := directedFreshPlaceholder(rootCall, parent)
						if !causalRenameRootPathAllowed(anchor, rootPath) {
							continue
						}
						actualRel, ok := causalPairRelation(anchorPre, anchorPost, "", rootPath)
						if !ok {
							continue
						}
						isDir := rootCall == "mkdir"
						ck := cellKey{
							Root:     call.Meta.CallName,
							Variant:  rootCall,
							Rel:      actualRel,
							Temporal: TemporalHB,
							RetA:     RetSuccess,
							RetB:     RetSuccess,
							OffB:     offsetBucketNA,
							PersistB: pathInDir(rootPath, hmcfg.Persistence_dir),
							DepthB:   DepthBucketOf(rootPath),
							TypeB:    isDir,
						}
						if !covBoard.cellUncovered(ck) {
							continue
						}
						a := anchor
						a.Relation = actualRel
						plan := &causalRootPlan{
							Anchor:     a,
							Cell:       ck,
							RootCall:   rootCall,
							RootPath:   rootPath,
							RootKind:   causalRootFresh,
							RootParent: parent,
						}
						if _, ok := byCell[ck]; !ok {
							byCell[ck] = plan
						}
					}
					if info.Kind.IsFresh() {
						switch rel {
						case PathChild:
							addFreshPlan(rootCall, rel, path, info.Kind, info.IsDir, true)
						case PathSibling:
							addFreshPlan(rootCall, rel, GetParentDir(path), AnchorPathExisting, true, false)
						}
					}
				}
				if rootCall == "rename" {
					for _, rootPath := range paths {
						if !causalRenameRootPathAllowed(anchor, rootPath) {
							continue
						}
						rootNode := ft.FindNode(rootPath)
						if rootNode == nil || rootNode == ft.Root || !aEligible(rootNode, ft.Root) ||
							rootNode.Type == NodeTypeTmpDir {
							continue
						}
						if rootNode.Type != NodeTypeFile && len(rootNode.Children) > 0 {
							continue // root E requires an empty directory
						}
						source := causalRenameSource(ft, rootPath)
						if source == "" || !causalRenameRootPathAllowed(anchor, source) {
							continue
						}
						sourceNode := ft.FindNode(source)
						if sourceNode == nil {
							continue
						}
						// C1 root rename is the B side: its relation and cell
						// path are the source, not the destination.
						actualRel, ok := causalPairRelation(anchorPre, anchorPost, source, source)
						if !ok {
							continue
						}
						ck := cellKey{
							Root:     call.Meta.CallName,
							Variant:  rootCall,
							Rel:      actualRel,
							Temporal: TemporalHB,
							RetA:     RetSuccess,
							RetB:     RetSuccess,
							OffB:     offsetBucketNA,
							PersistB: pathInDir(source, hmcfg.Persistence_dir),
							DepthB:   DepthBucketOf(source),
							TypeB:    sourceNode.Type != NodeTypeFile,
						}
						if !covBoard.cellUncovered(ck) {
							continue
						}
						a := anchor
						a.Relation = actualRel
						plan := &causalRootPlan{
							Anchor:            a,
							Cell:              ck,
							RootCall:          rootCall,
							RootPath:          rootPath,
							RootKind:          causalRootRename,
							RootParent:        GetParentDir(rootPath),
							RootSource:        source,
							RootDest:          rootPath,
							RootRenameReplace: true,
						}
						if _, ok := byCell[ck]; !ok {
							byCell[ck] = plan
						}
					}
					if rel == PathChild || rel == PathSibling {
						for _, anchorNode := range anchorNodes {
							parent := ""
							if rel == PathChild && anchorNode.Type != NodeTypeFile {
								parent = anchorNode.FullPath
							} else if rel == PathSibling {
								parent = GetParentDir(anchorNode.FullPath)
							}
							if parent == "" {
								continue
							}
							source := causalRenameSourceUnder(ft, parent)
							if source == "" || !causalRenameRootPathAllowed(anchor, source) {
								continue
							}
							dest := freshRenameDst(source, r.Rand)
							if ft.FindNode(dest) != nil || !causalRenameRootPathAllowed(anchor, dest) {
								continue
							}
							// C1 root rename is the B side: relation/cell use
							// the source; dest is only the materialized target.
							actualRel, ok := causalPairRelation(anchorPre, anchorPost, source, source)
							if !ok {
								continue
							}
							sourceNode := ft.FindNode(source)
							if sourceNode == nil {
								continue
							}
							ck := cellKey{
								Root:     call.Meta.CallName,
								Variant:  rootCall,
								Rel:      actualRel,
								Temporal: TemporalHB,
								RetA:     RetSuccess,
								RetB:     RetSuccess,
								OffB:     offsetBucketNA,
								PersistB: pathInDir(source, hmcfg.Persistence_dir),
								DepthB:   DepthBucketOf(source),
								TypeB:    sourceNode.Type != NodeTypeFile,
							}
							if !covBoard.cellUncovered(ck) {
								continue
							}
							a := anchor
							a.Relation = actualRel
							plan := &causalRootPlan{
								Anchor:            a,
								Cell:              ck,
								RootCall:          rootCall,
								RootPath:          dest,
								RootKind:          causalRootRename,
								RootParent:        parent,
								RootSource:        source,
								RootDest:          dest,
								RootRenameReplace: false,
							}
							if _, ok := byCell[ck]; !ok {
								byCell[ck] = plan
							}
						}
					}
				}
			}
		}
	}
	out := make([]*causalRootPlan, 0, len(byCell))
	for _, p := range byCell {
		out = append(out, p)
	}
	// Deterministic ordering before the RNG chooses a cell.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Cell, out[j].Cell
		if a.Root != b.Root {
			return a.Root < b.Root
		}
		if a.Variant != b.Variant {
			return a.Variant < b.Variant
		}
		if a.Rel != b.Rel {
			return a.Rel < b.Rel
		}
		if a.OffB != b.OffB {
			return a.OffB < b.OffB
		}
		if a.PersistB != b.PersistB {
			return !a.PersistB
		}
		if a.TypeB != b.TypeB {
			return !a.TypeB
		}
		if out[i].RootPath != out[j].RootPath {
			return out[i].RootPath < out[j].RootPath
		}
		if out[i].RootOffset != out[j].RootOffset {
			return out[i].RootOffset < out[j].RootOffset
		}
		if out[i].RootLen != out[j].RootLen {
			return out[i].RootLen < out[j].RootLen
		}
		return out[i].Anchor.Pos.CallIdx < out[j].Anchor.Pos.CallIdx
	})
	return out
}

func causalRootFits(rootCall string, node *FileNode) bool {
	return causalRootFitsFor(hmdfsProfile, rootCall, node)
}

func causalRootFitsFor(profile *DFSProfile, rootCall string, node *FileNode) bool {
	if node == nil {
		return false
	}
	isDir := node.Type != NodeTypeFile
	switch rootCall {
	case "read", "write", "pread64", "pwrite64", "truncate", "unlink", "link":
		return !isDir
	case "getdents64":
		return isDir
	case "open", "stat", "chmod", "fsync", "fdatasync", "symlink", "setxattr":
		return true
	}
	return false
}

type causalRootArg struct {
	offset uint64
	length uint64
	offB   uint64
}

func causalRootArgs(rootCall string, dn directedNode, r *randGen) []causalRootArg {
	switch rootCall {
	case "read", "write":
		return []causalRootArg{{offB: offsetBucketZero}}
	case "pread64", "pwrite64", "truncate":
		if r == nil {
			return nil
		}
		var out []causalRootArg
		for _, bucket := range []uint64{
			offsetBucketZero, offsetBucketMid, offsetBucketTail, offsetBucketBeyond,
		} {
			off, blen, ok := realizeDirectedParams(rootCall, bucket, dn, r)
			if !ok {
				continue
			}
			out = append(out, causalRootArg{offset: off, length: blen, offB: bucket})
		}
		return out
	default:
		return []causalRootArg{{offB: offsetBucketNA}}
	}
}

func causalRootPaths(ft *FileTree, anchor *FileNode, rel PathRelation) []string {
	if ft == nil || anchor == nil {
		return nil
	}
	switch rel {
	case PathSame:
		return []string{anchor.FullPath}
	case PathParent:
		var out []string
		for n := ft.GetParent(anchor); n != nil; n = ft.GetParent(n) {
			if n == ft.Root {
				break
			}
			out = append(out, n.FullPath)
		}
		sort.Strings(out)
		return out
	case PathSibling:
		var out []string
		for _, n := range ft.GetSibling(anchor) {
			if n != nil {
				out = append(out, n.FullPath)
			}
		}
		sort.Strings(out)
		return out
	case PathChild:
		var out []string
		var walk func(*FileNode)
		walk = func(n *FileNode) {
			for _, c := range ft.GetChildren(n) {
				if c == nil {
					continue
				}
				out = append(out, c.FullPath)
				walk(c)
			}
		}
		walk(anchor)
		sort.Strings(out)
		return out
	}
	return nil
}

// causalRootPathsByPath enumerates the existing paths related to a fresh
// anchor path. A fresh path has no FileTree node and no existing children, so
// Same/Child are intentionally empty here.
func causalRootPathsByPath(ft *FileTree, path string, info anchorPathInfo, rel PathRelation) []string {
	if ft == nil || path == "" || !info.Kind.IsFresh() {
		return nil
	}
	switch rel {
	case PathParent:
		var out []string
		for p := GetParentDir(path); p != ""; p = GetParentDir(p) {
			n := ft.FindNode(p)
			if n == nil {
				continue
			}
			if n == ft.Root {
				break
			}
			out = append(out, n.FullPath)
		}
		sort.Strings(out)
		return out
	case PathSibling:
		parent := ft.FindNode(GetParentDir(path))
		if parent == nil {
			return nil
		}
		var out []string
		for _, n := range ft.GetChildren(parent) {
			if n == nil || n.FullPath == path {
				continue
			}
			out = append(out, n.FullPath)
		}
		sort.Strings(out)
		return out
	default:
		return nil
	}
}

// causalRenameRootPathAllowed rejects root paths that live in a successful
// rename anchor's source subtree. The root is inserted after the anchor, so
// the source path and its descendants have moved by then. Source parent and
// sibling paths remain valid and must still pass the normal node constraints.
func causalRenameRootPathAllowed(a causalAnchor, rootPath string) bool {
	if a.Source == "" || a.Source == a.Path {
		return true
	}
	return !pathUnderRoot(rootPath, a.Source)
}

func causalRootCallsSeq(r *randGen, sCalls *SpecialCalls, lcs *LayeredChoiceStrategy, hmcfg *Hmdfs_config, p *Prog, batch *directedBatch) []*Call {
	if r == nil || sCalls == nil || batch == nil {
		return nil
	}
	s := stateFromProg(p)
	cid := ""
	if hmcfg != nil && len(hmcfg.Cids) > 0 {
		cid = hmcfg.Cids[0]
	}
	path := batch.BaseA
	switch batch.Root {
	case "pread64":
		openCalls, fd := r.generateOpenCallSeq(s, sCalls, path,
			r.randOpenFlags(r.target, capNeeded("read"), false))
		if len(openCalls) == 0 || fd == nil {
			return nil
		}
		return append(append(openCalls,
			r.generatePreadCallsSeq(s, sCalls, path, batch.RootLen, batch.RootOffset, fd)...),
			r.generateCloseCall(s, sCalls, fd))
	case "pwrite64":
		openCalls, fd := r.generateOpenCallSeq(s, sCalls, path,
			r.randOpenFlags(r.target, capNeeded("write"), false))
		if len(openCalls) == 0 || fd == nil {
			return nil
		}
		data := make([]byte, batch.RootLen)
		r.Read(data)
		return append(append(openCalls,
			r.generatePwriteCallsSeq(s, sCalls, path, data, batch.RootOffset, fd)...),
			r.generateCloseCall(s, sCalls, fd))
	case "truncate":
		c := r.generateTruncateCallWithPath(s, sCalls, path, directedNodeSize(lcs, path))
		if c == nil || len(c.Args) < 2 {
			return nil
		}
		ca, ok := c.Args[1].(*ConstArg)
		if !ok {
			return nil
		}
		ca.Val = batch.RootLen
		return []*Call{c}
	case "rename":
		if batch.RootSource == "" || batch.RootDest == "" || batch.RootSource == batch.RootDest {
			return nil
		}
		c := r.generateRenameCallWithPaths(s, sCalls, batch.RootSource, batch.RootDest)
		if c == nil {
			return nil
		}
		return []*Call{c}
	case "link", "symlink":
		if batch.RootSource == "" || batch.RootDest == "" ||
			batch.RootSource == batch.RootDest {
			return nil
		}
		calls := r.generateCallByName(s, sCalls, batch.Root, batch.RootDest,
			batch.RootSource, cid, nil, false,
			directedNodeSize(lcs, batch.RootSource),
			directedNodeFlags(r, lcs, batch.RootSource))
		if len(calls) == 0 || hasNilCall(calls) {
			return nil
		}
		return calls
	case "setxattr":
		name := batch.RootXattrName
		if name == "" {
			name = "user.eifuzz"
		}
		c := r.generateSetxattrCallFull(s, sCalls, path, name, "bar", 0)
		if c == nil {
			return nil
		}
		return []*Call{c}
	default:
		return r.generateCallByName(s, sCalls, batch.Root, path, "", cid, nil, false,
			directedNodeSize(lcs, path), directedNodeFlags(r, lcs, path))
	}
}

func causalRenameSource(ft *FileTree, destPath string) string {
	dest := ft.FindNode(destPath)
	if dest == nil {
		return ""
	}
	parent := ft.GetParent(dest)
	if parent == nil {
		return ""
	}
	var cands []*FileNode
	for _, n := range ft.GetChildren(parent) {
		if n == nil || n == dest || n.Type == NodeTypeTmpDir ||
			(n.Type == NodeTypeFile) != (dest.Type == NodeTypeFile) {
			continue
		}
		cands = append(cands, n)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].FullPath < cands[j].FullPath })
	if len(cands) == 0 {
		return ""
	}
	return cands[0].FullPath
}

func causalRenameSourceUnder(ft *FileTree, parentPath string) string {
	parent := ft.FindNode(parentPath)
	if parent == nil || parent.Type == NodeTypeFile || parent.Type == NodeTypeTmpDir {
		return ""
	}
	var cands []*FileNode
	for _, n := range ft.GetChildren(parent) {
		if n == nil || n.Type == NodeTypeTmpDir {
			continue
		}
		cands = append(cands, n)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].FullPath < cands[j].FullPath })
	if len(cands) == 0 {
		return ""
	}
	return cands[0].FullPath
}

func validateCausalAnchor(p *Prog, a *causalAnchor, lcs *LayeredChoiceStrategy) bool {
	if p == nil || a == nil || lcs == nil {
		return false
	}
	if a.Pos.ProgIdx != 0 || a.Pos.CallIdx < 0 || a.Pos.CallIdx >= len(p.Calls) {
		return false
	}
	if a.InsertPos != a.Pos.CallIdx+1 || a.InsertPos < 0 || a.InsertPos > len(p.Calls) {
		return false
	}
	call := p.Calls[a.Pos.CallIdx]
	if call == nil || call.Meta == nil || !isModifierCall(call.Meta.Name) {
		return false
	}
	path, source, dest := causalAnchorPaths(p, a.Pos)
	if path != a.Path || source != a.Source || dest != a.Dest {
		return false
	}
	ci := call.CheckInfo
	if ci == nil || ci.Etime == 0 || ci.Stime > ci.Etime {
		return false
	}
	if a.RefTime != int64(ci.Etime)-lcs.tscoffFor(0) {
		return false
	}
	tol := MfTolTicks()
	if a.Pos.CallIdx > 0 {
		if prev := p.Calls[a.Pos.CallIdx-1]; prev != nil && prev.CheckInfo != nil &&
			int64(ci.Stime)+tol < int64(prev.CheckInfo.Etime) {
			return false
		}
	}
	if a.Pos.CallIdx+1 < len(p.Calls) {
		if next := p.Calls[a.Pos.CallIdx+1]; next != nil && next.CheckInfo != nil &&
			int64(ci.Etime) > int64(next.CheckInfo.Stime)+tol {
			return false
		}
	}
	return true
}

func causalAnchorPaths(p *Prog, pos GroupPosition) (string, string, string) {
	if p == nil || pos.ProgIdx != 0 || pos.CallIdx < 0 || pos.CallIdx >= len(p.Calls) {
		return "", "", ""
	}
	call := p.Calls[pos.CallIdx]
	if call == nil || call.Meta == nil {
		return "", "", ""
	}
	if strings.Contains(call.Meta.Name, "rename") {
		src, dst := extractRenamePaths(call)
		return dst, src, dst
	}
	switch call.Meta.CallName {
	case "link":
		target := extractPathFromCallByArgIdx(call, 0)
		linkPath := extractPathFromCallByArgIdx(call, 1)
		return linkPath, target, linkPath
	case "symlink":
		linkPath := extractPathFromCallByArgIdx(call, 1)
		target := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(call, 0))
		return linkPath, target, linkPath
	case "readlink":
		linkPath := extractPathFromCallByArgIdx(call, 0)
		return linkPath, linkPath, linkPath
	}
	path := causalEffectivePath(p, pos)
	return path, path, path
}

func causalEffectivePath(p *Prog, pos GroupPosition) string {
	if p == nil || pos.ProgIdx < 0 || pos.ProgIdx >= 1 || pos.CallIdx < 0 || pos.CallIdx >= len(p.Calls) {
		return ""
	}
	call := p.Calls[pos.CallIdx]
	if call == nil || call.Meta == nil {
		return ""
	}
	if strings.Contains(call.Meta.Name, "rename") {
		_, dst := extractRenamePaths(call)
		return dst
	}
	if path := extractPathFromCall(call); path != "" {
		return path
	}
	if len(call.Args) == 0 {
		return ""
	}
	fd, ok := call.Args[0].(*ResultArg)
	if !ok || fd == nil || fd.Res == nil {
		return ""
	}
	ownerIdx := -1
	for i := pos.CallIdx - 1; i >= 0; i-- {
		prev := p.Calls[i]
		if prev == nil || prev.Meta == nil || prev.Ret == nil {
			continue
		}
		if !strings.Contains(prev.Meta.Name, "open") && !strings.Contains(prev.Meta.Name, "creat") {
			continue
		}
		if prev.Ret == fd.Res {
			ownerIdx = i
			break
		}
	}
	if ownerIdx < 0 {
		return ""
	}
	for i := ownerIdx + 1; i < pos.CallIdx; i++ {
		prev := p.Calls[i]
		if prev != nil && prev.Meta != nil && strings.Contains(prev.Meta.Name, "close") &&
			usesFd(prev, p.Calls[ownerIdx].Ret) {
			return ""
		}
	}
	return extractPathFromCall(p.Calls[ownerIdx])
}

func causalPlanNode(ft *FileTree, path, persistDir string) (directedNode, bool) {
	n := ft.FindNode(path)
	if n == nil {
		return directedNode{}, false
	}
	return directedNode{
		path:     n.FullPath,
		isDir:    n.Type != NodeTypeFile,
		size:     n.Size,
		depth:    DepthBucketOf(n.FullPath),
		persist:  pathInDir(n.FullPath, persistDir),
		nonEmpty: len(n.Children) > 0,
	}, true
}

func validateCausalPlan(hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, plan *directedPlan) bool {
	if plan == nil || hmcfg == nil || lcs == nil || lcs.FileTree == nil {
		return false
	}
	ft := lcs.FileTree
	profile := lcs.profileOrDefault()
	// Root side.
	switch {
	case plan.FreshA2:
		if plan.Cell.Combo.Root != "rename" {
			return false
		}
		src := ft.FindNode(plan.BaseA)
		if src == nil || src.Type == NodeTypeTmpDir || plan.BaseA == plan.BaseA2 ||
			!renameSameParent(plan.BaseA, plan.BaseA2) {
			return false
		}
		dst := ft.FindNode(plan.BaseA2)
		if plan.RootRenameReplace {
			if dst == nil || dst.Type == NodeTypeTmpDir ||
				(dst.Type == NodeTypeFile) != (src.Type == NodeTypeFile) ||
				(dst.Type != NodeTypeFile && len(dst.Children) > 0) {
				return false
			}
		} else if dst != nil {
			return false
		}
	case plan.FreshA:
		if plan.Cell.Combo.Root != "mkdir" && plan.Cell.Combo.Root != "creat" {
			return false
		}
		if plan.RootParentIsAnchor {
			if !plan.RootParentKind.IsFresh() || !plan.RootParentIsDir ||
				!freshParentNodeEligible(ft, plan.BaseA2) {
				return false
			}
		} else {
			parent := ft.FindNode(plan.BaseA2)
			if parent == nil || parent.Type == NodeTypeFile || parent.Type == NodeTypeTmpDir ||
				!aEligible(parent, ft.Root) {
				return false
			}
		}
		if plan.BaseA == "" || ft.FindNode(plan.BaseA) != nil ||
			GetParentDir(plan.BaseA) != plan.BaseA2 {
			return false
		}
	default:
		rootNode := ft.FindNode(plan.BaseA)
		if rootNode == nil || rootNode == ft.Root || rootNode.Type == NodeTypeTmpDir ||
			!aEligible(rootNode, ft.Root) {
			return false
		}
		root, ok := causalPlanNode(ft, plan.BaseA, hmcfg.Persistence_dir)
		if !ok || !nodeFitsConstraintFor(profile, root, plan.Cell.Combo.Root) {
			return false
		}
	}
	if plan.Cell.Combo.Root == "link" || plan.Cell.Combo.Root == "symlink" {
		if plan.RootLinkPath == "" || ft.FindNode(plan.RootLinkPath) != nil {
			return false
		}
		relationPath := plan.RootRelationPath
		if relationPath == "" {
			relationPath = plan.BaseA
		}
		if relationPath != plan.BaseA && relationPath != plan.RootLinkPath {
			return false
		}
	}
	// Variant side.
	if plan.FreshSame {
		if plan.BaseA == "" || plan.BaseA != plan.BaseB ||
			plan.BaseA2 == "" || GetParentDir(plan.BaseA) != plan.BaseA2 {
			return false
		}
		if !freshSameFormAllowedFor(profile, plan.Cell.Combo.Root, plan.Cell.Combo.Variant,
			plan.Cell.RetB, plan.Cell.OffB, plan.Cell.TypeB) {
			return false
		}
		if DepthBucketOf(plan.BaseB) != plan.Cell.DepthB ||
			pathInDir(plan.BaseB, hmcfg.Persistence_dir) != plan.Cell.PersistB {
			return false
		}
	} else if plan.Cell.Combo.Variant == "rename" {
		src := ft.FindNode(plan.BaseB2)
		if src == nil || plan.BaseB == "" || plan.BaseB == plan.BaseB2 ||
			!renameSameParent(plan.BaseB2, plan.BaseB) ||
			(src.Type == NodeTypeFile) != !plan.Cell.TypeB {
			return false
		}
		dst := ft.FindNode(plan.BaseB)
		switch {
		case plan.FreshRenameDst:
			if dst != nil || plan.Cell.RetB != RetSuccess {
				return false
			}
		case plan.RenameReplace:
			if dst == nil || dst.Type == NodeTypeTmpDir ||
				(dst.Type == NodeTypeFile) != (src.Type == NodeTypeFile) ||
				(dst.Type != NodeTypeFile && len(dst.Children) > 0) || plan.Cell.RetB != RetSuccess {
				return false
			}
		default:
			if dst == nil || plan.Cell.RetB != RetFailure ||
				src.Type == NodeTypeFile || dst.Type != NodeTypeDir ||
				len(dst.Children) == 0 {
				return false
			}
		}
		// Rename variant B cell is anchored on the source; BaseB is the
		// destination used to materialize rename(src,dst) and shape RetB.
		if DepthBucketOf(plan.BaseB2) != plan.Cell.DepthB ||
			pathInDir(plan.BaseB2, hmcfg.Persistence_dir) != plan.Cell.PersistB {
			return false
		}
	} else if plan.FreshB {
		parent := ft.FindNode(plan.BaseB2)
		if parent == nil || parent.Type == NodeTypeFile || parent.Type == NodeTypeTmpDir ||
			plan.BaseB == "" || ft.FindNode(plan.BaseB) != nil ||
			GetParentDir(plan.BaseB) != plan.BaseB2 ||
			(plan.Cell.Combo.Variant != "mkdir" && plan.Cell.Combo.Variant != "creat") ||
			plan.Cell.RetB != RetSuccess || plan.Cell.OffB != offsetBucketNA {
			return false
		}
		wantDir := plan.Cell.Combo.Variant == "mkdir"
		if plan.Cell.TypeB != wantDir {
			return false
		}
		b := directedNode{
			path:    plan.BaseB,
			isDir:   wantDir,
			depth:   DepthBucketOf(plan.BaseB),
			persist: pathInDir(plan.BaseB, hmcfg.Persistence_dir),
		}
		if !nodeFitsConstraint(b, plan.Cell.Combo.Variant) ||
			b.depth != plan.Cell.DepthB || b.persist != plan.Cell.PersistB {
			return false
		}
	} else if plan.Cell.Combo.Variant == "readlink" {
		if plan.Cell.Combo.Root != "symlink" || plan.RootLinkPath == "" ||
			plan.BaseB != plan.RootLinkPath ||
			plan.Cell.RetB != RetSuccess || plan.Cell.OffB != offsetBucketNA ||
			plan.Cell.TypeB {
			return false
		}
		if DepthBucketOf(plan.BaseB) != plan.Cell.DepthB ||
			pathInDir(plan.BaseB, hmcfg.Persistence_dir) != plan.Cell.PersistB {
			return false
		}
	} else {
		b, ok := causalPlanNode(ft, plan.BaseB, hmcfg.Persistence_dir)
		if !ok || !nodeFitsConstraintFor(profile, b, plan.Cell.Combo.Variant) ||
			b.isDir != plan.Cell.TypeB || b.depth != plan.Cell.DepthB ||
			b.persist != pathInDir(plan.BaseB, hmcfg.Persistence_dir) {
			return false
		}
		bn := &directedBNode{directedNode: b}
		if !variantKindAllowed(plan.Cell.Combo.Variant, plan.Cell.RetB, bn,
			plan.Cell.Combo.Root, plan.Cell.Combo.Rel) {
			return false
		}
		if offsetVariants[plan.Cell.Combo.Variant] && plan.Cell.RetB != RetENOENT {
			pos := plan.BOffset
			if plan.Cell.Combo.Variant == "truncate" {
				pos = plan.BLen
			}
			if bucketOfOffset(b.size, pos) != plan.Cell.OffB {
				return false
			}
		}
	}
	// The pair relation is root -> variant. Rename Same is classified via
	// source/pre path, all other forms via destination/post path.
	if plan.FreshSame {
		return pathRelBetween(plan.BaseA, plan.BaseB) == plan.Cell.Combo.Rel
	}
	rootRelationPath := plan.RootRelationPath
	if rootRelationPath == "" {
		rootRelationPath = plan.BaseA
	}
	rootPre, rootPost := rootRelationPath, rootRelationPath
	if plan.FreshA2 {
		// HB A of a rename root is the post-state destination.
		rootPre, rootPost = plan.BaseA2, plan.BaseA2
	}
	if (plan.Cell.Combo.Root == "link" || plan.Cell.Combo.Root == "symlink") &&
		plan.Cell.Combo.Variant == "readlink" && plan.RootLinkPath != "" {
		// readlink is the linkpath follower: its B is the root linkpath, so
		// the root A role is the linkpath, not the target.
		rootPre, rootPost = plan.RootLinkPath, plan.RootLinkPath
	}
	varPre, varPost := plan.BaseB, plan.BaseB
	if plan.Cell.Combo.Variant == "rename" {
		// HB B of a rename is the entry-state source.
		varPre, varPost = plan.BaseB2, plan.BaseB2
	}
	rel, ok := causalPairRelation(rootPre, rootPost, varPre, varPost)
	return ok && rel == plan.Cell.Combo.Rel
}
