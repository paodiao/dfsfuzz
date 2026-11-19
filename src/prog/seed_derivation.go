package prog

import "sort"

type CausalDerivationTable struct {
	Weights map[string]map[CallVariant]int
}

var seedRootRelations = []PathRelation{
	PathSame, PathParent, PathChild, PathSibling, PathNoRel,
}

func newCausalDerivationTable() *CausalDerivationTable {
	return newCausalDerivationTableFor(hmdfsProfile)
}

func newCausalDerivationTableFor(profile *DFSProfile) *CausalDerivationTable {
	if profile == nil {
		profile = hmdfsProfile
	}
	rootCalls := profile.Calls.CausalRootCalls
	if len(rootCalls) == 0 {
		rootCalls = causalRootCalls
	}
	t := &CausalDerivationTable{Weights: make(map[string]map[CallVariant]int)}
	for _, anchorCall := range rootCalls {
		weights := make(map[CallVariant]int)
		for _, rootCall := range rootCalls {
			for _, rel := range seedRootRelations {
				w := relBaseOf(rel) * profile.ConflictCoeffOf(anchorCall, rootCall) / 100
				if w < 5 {
					w = 5
				}
				if w > 45 {
					w = 45
				}
				weights[CallVariant{CallName: rootCall, PathRelation: rel}] = w
			}
		}
		t.Weights[anchorCall] = weights
	}
	return t
}

type seedAnchorRole int

const (
	seedAnchorNormal seedAnchorRole = iota
	seedAnchorRenameSrc
	seedAnchorRenameDst
)

type seedAnchor struct {
	Pos      GroupPosition
	CallName string
	Path     string
	IsDir    bool
	PathKind AnchorPathKind
	Role     seedAnchorRole
	// Dual anchors (link/symlink) keep both canonical paths. Path remains the
	// compatibility projection: target for link, linkpath for symlink.
	Dual        DualPathKind
	Target      string
	LinkPath    string
	FollowIsDir bool
}

type seedRootDerivation struct {
	Anchor     seedAnchor
	RootCall   string
	Relation   PathRelation
	RootSource string
	RootDest   string
	RootIsDir  bool
	Dual       DualPathKind
	TargetPath string
	LinkPath   string
}

// RootPaths returns the unified dual-path view of the derived root.
func (d seedRootDerivation) RootPaths() RootPaths {
	switch d.Dual {
	case DualRename:
		return RootPaths{Kind: DualRename, PrePath: d.RootSource, PostPath: d.RootDest}
	case DualLink, DualSymlink:
		return RootPaths{Kind: d.Dual, PrePath: d.TargetPath, PostPath: d.LinkPath,
			TargetPath: d.TargetPath, LinkPath: d.LinkPath}
	case DualReadlink:
		return RootPaths{Kind: DualReadlink, PrePath: d.LinkPath, PostPath: d.LinkPath,
			LinkPath: d.LinkPath}
	}
	return RootPaths{Kind: DualNone, PrePath: d.RootSource, PostPath: d.RootDest}
}

// seedAnchorPaths returns the program-visible path roles of an anchor call.
// link: target + linkpath; symlink: canonical target + linkpath; readlink:
// linkpath. Other calls use their effective path.
func seedAnchorPaths(p *Prog, pos GroupPosition, call *Call) []string {
	if call == nil || call.Meta == nil {
		return nil
	}
	switch call.Meta.CallName {
	case "link":
		return []string{extractPathFromCallByArgIdx(call, 0), extractPathFromCallByArgIdx(call, 1)}
	case "symlink":
		linkPath := extractPathFromCallByArgIdx(call, 1)
		target := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(call, 0))
		return []string{target, linkPath}
	case "readlink":
		return []string{extractPathFromCallByArgIdx(call, 0)}
	}
	return []string{pathOfCall([]*Prog{p}, pos)}
}

func collectSeedAnchors(p *Prog, ft *FileTree, requireSuccess bool) []seedAnchor {
	if p == nil || ft == nil {
		return nil
	}
	seen := make(map[string]bool)
	var anchors []seedAnchor
	add := func(pos GroupPosition, path string, role seedAnchorRole) {
		if path == "" {
			return
		}
		call := p.Calls[pos.CallIdx]
		if call == nil || call.Meta == nil {
			return
		}
		key := call.Meta.CallName + "\x00" + path
		if seen[key] {
			return
		}
		info, ok := classifyAnchorPath(p, ft, pos, path, requireSuccess)
		if !ok || info.Kind == AnchorPathUnknown {
			return
		}
		if node := ft.FindNode(path); node != nil {
			if !aEligible(node, ft.Root) {
				return
			}
		} else if !freshParentNodeEligible(ft, path) {
			return
		}
		seen[key] = true
		anchor := seedAnchor{
			Pos:      pos,
			CallName: call.Meta.CallName,
			Path:     path,
			IsDir:    info.IsDir,
			PathKind: info.Kind,
			Role:     role,
		}
		switch call.Meta.CallName {
		case "link":
			anchor.Dual = DualLink
			anchor.Target = extractPathFromCallByArgIdx(call, 0)
			anchor.LinkPath = extractPathFromCallByArgIdx(call, 1)
		case "symlink":
			anchor.Dual = DualSymlink
			anchor.LinkPath = extractPathFromCallByArgIdx(call, 1)
			anchor.Target = symlinkCanonicalTarget(anchor.LinkPath,
				extractPathFromCallByArgIdx(call, 0))
			anchor.FollowIsDir = info.IsDir
		case "readlink":
			anchor.Dual = DualReadlink
			anchor.LinkPath = path
		}
		anchors = append(anchors, anchor)
	}
	for i, call := range p.Calls {
		if call == nil || call.Meta == nil {
			continue
		}
		pos := GroupPosition{ProgIdx: 0, CallIdx: i}
		if call.Meta.CallName == "rename" {
			src, dst := extractRenamePaths(call)
			add(pos, src, seedAnchorRenameSrc)
			add(pos, dst, seedAnchorRenameDst)
			continue
		}
		for _, path := range seedAnchorPaths(p, pos, call) {
			add(pos, path, seedAnchorNormal)
		}
	}
	sort.SliceStable(anchors, func(i, j int) bool {
		if anchors[i].Pos.CallIdx != anchors[j].Pos.CallIdx {
			return anchors[i].Pos.CallIdx < anchors[j].Pos.CallIdx
		}
		return anchors[i].Path < anchors[j].Path
	})
	return anchors
}

func seedRelationAllowed(anchor seedAnchor, rootCall string, rel PathRelation) bool {
	// A successful rename moves the source away. Source parent/sibling paths
	// remain usable, but Same/Child paths are pre-anchor state.
	if anchor.Role == seedAnchorRenameSrc &&
		(rel == PathSame || rel == PathChild) {
		return false
	}
	if rootCall == "rename" {
		if anchor.PathKind.IsFresh() {
			if rel == PathSame || rel == PathParent || rel == PathSibling || rel == PathNoRel {
				return true
			}
			return false
		}
		return rel == PathSame || rel == PathParent || rel == PathChild || rel == PathSibling || rel == PathNoRel
	}
	if rootCall == "mkdir" || rootCall == "creat" {
		if rel != PathChild && rel != PathSibling && rel != PathNoRel {
			return false
		}
		if rel == PathChild && !anchor.IsDir {
			return false
		}
		return true
	}
	if anchor.PathKind.IsFresh() {
		return rel == PathParent || rel == PathSibling || rel == PathNoRel
	}
	return rel == PathSame || rel == PathParent || rel == PathChild || rel == PathSibling || rel == PathNoRel
}

// seedRootPathAllowed is a defense-in-depth filter for rename source anchors.
// Relation filtering already excludes Same/Child, but a future derivation
// branch must not accidentally reintroduce a path under the moved source.
func seedRootPathAllowed(anchor seedAnchor, rootPath string) bool {
	if anchor.Role != seedAnchorRenameSrc || rootPath == "" {
		return true
	}
	return !pathUnderRoot(rootPath, anchor.Path)
}

func deriveSeedRootPath(ft *FileTree, anchor seedAnchor, rootCall string, rel PathRelation, r *randGen) (seedRootDerivation, bool) {
	if ft == nil || r == nil || anchor.Path == "" || !seedRelationAllowed(anchor, rootCall, rel) {
		return seedRootDerivation{}, false
	}
	out := seedRootDerivation{Anchor: anchor, RootCall: rootCall, Relation: rel}
	if rootCall == "rename" {
		source := seedRenameSource(ft, anchor, rel, r)
		if source == "" {
			return seedRootDerivation{}, false
		}
		srcNode := ft.FindNode(source)
		isDir := anchor.IsDir
		if srcNode != nil {
			isDir = srcNode.Type != NodeTypeFile
		}
		if srcNode == nil && !anchor.PathKind.IsFresh() {
			return seedRootDerivation{}, false
		}
		var dest string
		if r.profile().Name == "glusterfs" {
			dest = pickRenameDstPolicy(r.profile(), ft, source, rel, r.Rand)
		} else {
			for attempt := 0; attempt < 8; attempt++ {
				dest = freshRenameDst(source, r.Rand)
				if ft.FindNode(dest) == nil && dest != source {
					break
				}
				dest = ""
			}
		}
		if dest == "" || dest == source {
			return seedRootDerivation{}, false
		}
		if r.profile().Name != "glusterfs" &&
			GetParentDir(dest) != GetParentDir(source) {
			return seedRootDerivation{}, false
		}
		out.RootSource = source
		out.RootDest = dest
		out.RootIsDir = isDir
		out.Dual = DualRename
		out.TargetPath = source
		out.LinkPath = dest
		if !seedRootPathAllowed(anchor, out.RootSource) ||
			!seedRootPathAllowed(anchor, out.RootDest) {
			return seedRootDerivation{}, false
		}
		return out, true
	}
	if rootCall == "mkdir" || rootCall == "creat" {
		parent := ""
		switch rel {
		case PathChild:
			if anchor.IsDir {
				parent = anchor.Path
			}
		case PathSibling:
			parent = parentDirOrRoot(anchor.Path)
		case PathNoRel:
			parent = ft.getRandomUnrelatedPath(anchor.Path, r.Rand, relTargetDir)
		}
		if parent == "" {
			if !freshParentEligible(ft, parent) {
				return seedRootDerivation{}, false
			}
		} else if parent == anchor.Path {
			if anchor.PathKind.IsFresh() {
				if !freshParentNodeEligible(ft, parent) {
					return seedRootDerivation{}, false
				}
			} else if !freshParentEligible(ft, parent) {
				return seedRootDerivation{}, false
			}
		} else if !freshParentEligible(ft, parent) {
			return seedRootDerivation{}, false
		}
		path := r.freshChildPath(parent, rootCall)
		if path == "" || ft.FindNode(path) != nil {
			return seedRootDerivation{}, false
		}
		if !seedRootPathAllowed(anchor, path) {
			return seedRootDerivation{}, false
		}
		out.RootSource = path
		out.RootDest = path
		out.RootIsDir = rootCall == "mkdir"
		return out, true
	}
	if rootCall == "link" || rootCall == "symlink" {
		constraint := r.profile().TargetConstraint(rootCall)
		target := ft.GetPathByRelation(anchor.Path, "", rel, r.Rand, false, constraint)
		if target == "" {
			return seedRootDerivation{}, false
		}
		node := ft.FindNode(target)
		if node == nil || !nodeMatchesConstraint(node, constraint) ||
			!aEligible(node, ft.Root) {
			return seedRootDerivation{}, false
		}
		linkPath := r.freshLinkPath(target, rootCall)
		if linkPath == "" || ft.FindNode(linkPath) != nil {
			return seedRootDerivation{}, false
		}
		if !seedRootPathAllowed(anchor, target) ||
			!seedRootPathAllowed(anchor, linkPath) {
			return seedRootDerivation{}, false
		}
		out.RootSource = target
		out.RootDest = linkPath
		out.RootIsDir = node.Type != NodeTypeFile
		out.Dual = dualKindForCall(rootCall)
		out.TargetPath = target
		out.LinkPath = linkPath
		return out, true
	}
	constraint := r.profile().TargetConstraint(rootCall)
	path := ft.GetPathByRelation(anchor.Path, "", rel, r.Rand, false, constraint)
	if path == "" {
		return seedRootDerivation{}, false
	}
	node := ft.FindNode(path)
	if node == nil || !nodeMatchesConstraint(node, constraint) ||
		!aEligible(node, ft.Root) {
		return seedRootDerivation{}, false
	}
	if !seedRootPathAllowed(anchor, path) {
		return seedRootDerivation{}, false
	}
	out.RootSource = path
	out.RootDest = path
	out.RootIsDir = node.Type != NodeTypeFile
	return out, true
}

func seedRenameSource(ft *FileTree, anchor seedAnchor, rel PathRelation, r *randGen) string {
	if ft == nil {
		return ""
	}
	if anchor.PathKind.IsFresh() && rel == PathChild {
		return ""
	}
	path := ft.GetPathByRelation(anchor.Path, "", rel, r.Rand, false, relTargetAny)
	if path == "" {
		return ""
	}
	node := ft.FindNode(path)
	if node != nil {
		if !aEligible(node, ft.Root) {
			return ""
		}
		return path
	}
	if anchor.PathKind.IsFresh() && path == anchor.Path &&
		freshParentNodeEligible(ft, path) {
		return path
	}
	return ""
}

func sampleSeedDerivation(p *Prog, ft *FileTree, table *CausalDerivationTable, r *randGen, requireSuccess bool) (seedRootDerivation, bool) {
	if p == nil || ft == nil || r == nil {
		return seedRootDerivation{}, false
	}
	if table == nil {
		table = newCausalDerivationTableFor(r.profile())
	}
	anchors := collectSeedAnchors(p, ft, requireSuccess)
	if len(anchors) == 0 {
		return seedRootDerivation{}, false
	}
	type candidate struct {
		d      seedRootDerivation
		weight int
	}
	var candidates []candidate
	for _, anchor := range anchors {
		weights := table.Weights[anchor.CallName]
		variants := make([]CallVariant, 0, len(weights))
		for variant := range weights {
			variants = append(variants, variant)
		}
		sort.Slice(variants, func(i, j int) bool {
			if variants[i].CallName != variants[j].CallName {
				return variants[i].CallName < variants[j].CallName
			}
			return variants[i].PathRelation < variants[j].PathRelation
		})
		for _, variant := range variants {
			weight := weights[variant]
			if weight <= 0 || !seedRelationAllowed(anchor, variant.CallName, variant.PathRelation) {
				continue
			}
			d, ok := deriveSeedRootPath(ft, anchor, variant.CallName, variant.PathRelation, r)
			if !ok {
				continue
			}
			candidates = append(candidates, candidate{d: d, weight: weight})
		}
	}
	if len(candidates) == 0 {
		return seedRootDerivation{}, false
	}
	total := 0
	for _, c := range candidates {
		total += c.weight
	}
	pick := r.Intn(total)
	for _, c := range candidates {
		if pick < c.weight {
			return c.d, true
		}
		pick -= c.weight
	}
	return seedRootDerivation{}, false
}

func seedFreshPathKind(d seedRootDerivation) int {
	if d.RootCall == "mkdir" {
		return relTargetDir
	}
	if d.RootCall == "creat" {
		return relTargetFile
	}
	if d.RootCall == "rename" {
		if d.RootIsDir {
			return relTargetDir
		}
		return relTargetFile
	}
	return 0
}
