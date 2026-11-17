package prog

import (
	"strings"
	"sync/atomic"
)

type pathKind int

const (
	pathExisting pathKind = iota
	pathFresh
)

type pathRole int

const (
	pathRoleDirect pathRole = iota
	pathRoleMkdir
	pathRoleCreat
	pathRoleRenameSrc
	pathRoleRenameDst
	pathRoleLinkTarget
	pathRoleSymlinkTarget
)

type pathSpec struct {
	Path           string
	Kind           pathKind
	Role           pathRole
	ArgIdx         int
	TypeConstraint int
}

type pathMember struct {
	Pos        GroupPosition
	Paths      []pathSpec
	Constraint int
}

type pathSet struct {
	Anchor  GroupPosition
	Members []pathMember
	Prefix  string
}

type pathTarget struct {
	Anchor      GroupPosition
	Member      GroupPosition
	Rel         PathRelation
	DepthBucket int
}

type pathRelocationCandidate struct {
	OldBase     string
	NewBase     string
	NewParent   string
	AnchorNew   string
	SelectedDir bool
}

type pathPlan struct {
	Updates []pathUpdate
	Base    string
}

type pathPlanQuality struct {
	SoftParent     bool
	DuplicateFresh int
}

type pathUpdate struct {
	Pos   GroupPosition
	Specs []pathSpec
}

var pathSetStats struct {
	builds                    uint64
	directedAttempts          uint64
	directedSingleRoot        uint64
	directedMultiRoot         uint64
	externalParentCandidates  uint64
	selectedDirCandidates     uint64
	selected                  uint64
	noTarget                  uint64
	noCandidates              uint64
	candidateCount            uint64
	committedFallbackCount    uint64
	exact                     uint64
	level1a                   uint64
	level1b                   uint64
	lost                      uint64
	noPlan                    uint64
	applyFail                 uint64
	candidateConnectivityFail uint64
	failOther                 uint64
	pathSetSizeSum            uint64
	pathSetSizeMax            uint64
	directedPathSetSizeSum    uint64
	directedPathSetSizeMax    uint64
	duplicateFresh            uint64
}

type PathSetDiagnostics struct {
	Builds                        uint64
	DirectedAttempts              uint64
	DirectedSingleRoot            uint64
	DirectedMultiRoot             uint64
	ExternalParentCandidates      uint64
	SelectedDirCandidates         uint64
	TargetSelected                uint64
	NoTarget                      uint64
	NoCandidates                  uint64
	NoPlan                        uint64
	CandidateCount                uint64
	CommittedFallbackCount        uint64
	Exact                         uint64
	Level1a                       uint64
	Level1b                       uint64
	Lost                          uint64
	ApplyFail                     uint64
	CandidateConnectivityFailures uint64
	FailOther                     uint64
	PathSetSizeSum                uint64
	PathSetSizeMax                uint64
	DirectedPathSetSizeSum        uint64
	DirectedPathSetSizeMax        uint64
	DuplicateFresh                uint64
}

// PathSetTargetStats returns directed PathSet target diagnostics:
// selected, exact, level1a, level1b, lost, planBuildFail, applyFail.
// NoTarget/NoCandidates are separate in PathSetDiagnostics.
func PathSetTargetStats() (uint64, uint64, uint64, uint64, uint64, uint64, uint64) {
	return atomic.LoadUint64(&pathSetStats.selected),
		atomic.LoadUint64(&pathSetStats.exact),
		atomic.LoadUint64(&pathSetStats.level1a),
		atomic.LoadUint64(&pathSetStats.level1b),
		atomic.LoadUint64(&pathSetStats.lost),
		atomic.LoadUint64(&pathSetStats.noPlan),
		atomic.LoadUint64(&pathSetStats.applyFail)
}

func PathSetDiagnosticsSnapshot() PathSetDiagnostics {
	return PathSetDiagnostics{
		Builds:                        atomic.LoadUint64(&pathSetStats.builds),
		DirectedAttempts:              atomic.LoadUint64(&pathSetStats.directedAttempts),
		DirectedSingleRoot:            atomic.LoadUint64(&pathSetStats.directedSingleRoot),
		DirectedMultiRoot:             atomic.LoadUint64(&pathSetStats.directedMultiRoot),
		ExternalParentCandidates:      atomic.LoadUint64(&pathSetStats.externalParentCandidates),
		SelectedDirCandidates:         atomic.LoadUint64(&pathSetStats.selectedDirCandidates),
		TargetSelected:                atomic.LoadUint64(&pathSetStats.selected),
		NoTarget:                      atomic.LoadUint64(&pathSetStats.noTarget),
		NoCandidates:                  atomic.LoadUint64(&pathSetStats.noCandidates),
		NoPlan:                        atomic.LoadUint64(&pathSetStats.noPlan),
		CandidateCount:                atomic.LoadUint64(&pathSetStats.candidateCount),
		CommittedFallbackCount:        atomic.LoadUint64(&pathSetStats.committedFallbackCount),
		Exact:                         atomic.LoadUint64(&pathSetStats.exact),
		Level1a:                       atomic.LoadUint64(&pathSetStats.level1a),
		Level1b:                       atomic.LoadUint64(&pathSetStats.level1b),
		Lost:                          atomic.LoadUint64(&pathSetStats.lost),
		ApplyFail:                     atomic.LoadUint64(&pathSetStats.applyFail),
		CandidateConnectivityFailures: atomic.LoadUint64(&pathSetStats.candidateConnectivityFail),
		FailOther:                     atomic.LoadUint64(&pathSetStats.failOther),
		PathSetSizeSum:                atomic.LoadUint64(&pathSetStats.pathSetSizeSum),
		PathSetSizeMax:                atomic.LoadUint64(&pathSetStats.pathSetSizeMax),
		DirectedPathSetSizeSum:        atomic.LoadUint64(&pathSetStats.directedPathSetSizeSum),
		DirectedPathSetSizeMax:        atomic.LoadUint64(&pathSetStats.directedPathSetSizeMax),
		DuplicateFresh:                atomic.LoadUint64(&pathSetStats.duplicateFresh),
	}
}

func resetPathSetTargetStats() {
	atomic.StoreUint64(&pathSetStats.builds, 0)
	atomic.StoreUint64(&pathSetStats.directedAttempts, 0)
	atomic.StoreUint64(&pathSetStats.directedSingleRoot, 0)
	atomic.StoreUint64(&pathSetStats.directedMultiRoot, 0)
	atomic.StoreUint64(&pathSetStats.externalParentCandidates, 0)
	atomic.StoreUint64(&pathSetStats.selectedDirCandidates, 0)
	atomic.StoreUint64(&pathSetStats.selected, 0)
	atomic.StoreUint64(&pathSetStats.noTarget, 0)
	atomic.StoreUint64(&pathSetStats.noCandidates, 0)
	atomic.StoreUint64(&pathSetStats.candidateCount, 0)
	atomic.StoreUint64(&pathSetStats.committedFallbackCount, 0)
	atomic.StoreUint64(&pathSetStats.exact, 0)
	atomic.StoreUint64(&pathSetStats.level1a, 0)
	atomic.StoreUint64(&pathSetStats.level1b, 0)
	atomic.StoreUint64(&pathSetStats.lost, 0)
	atomic.StoreUint64(&pathSetStats.noPlan, 0)
	atomic.StoreUint64(&pathSetStats.applyFail, 0)
	atomic.StoreUint64(&pathSetStats.candidateConnectivityFail, 0)
	atomic.StoreUint64(&pathSetStats.failOther, 0)
	atomic.StoreUint64(&pathSetStats.pathSetSizeSum, 0)
	atomic.StoreUint64(&pathSetStats.pathSetSizeMax, 0)
	atomic.StoreUint64(&pathSetStats.directedPathSetSizeSum, 0)
	atomic.StoreUint64(&pathSetStats.directedPathSetSizeMax, 0)
	atomic.StoreUint64(&pathSetStats.duplicateFresh, 0)
}

func notePathSetBuilt(set *pathSet, roots []string, directed bool) {
	if set == nil {
		return
	}
	size := uint64(len(set.Members))
	atomic.AddUint64(&pathSetStats.builds, 1)
	atomic.AddUint64(&pathSetStats.pathSetSizeSum, size)
	noteMax(&pathSetStats.pathSetSizeMax, size)
	if !directed {
		return
	}
	atomic.AddUint64(&pathSetStats.directedAttempts, 1)
	atomic.AddUint64(&pathSetStats.directedPathSetSizeSum, size)
	noteMax(&pathSetStats.directedPathSetSizeMax, size)
	if len(roots) <= 1 {
		atomic.AddUint64(&pathSetStats.directedSingleRoot, 1)
	} else {
		atomic.AddUint64(&pathSetStats.directedMultiRoot, 1)
	}
}

func noteMax(dst *uint64, value uint64) {
	for {
		old := atomic.LoadUint64(dst)
		if value <= old || atomic.CompareAndSwapUint64(dst, old, value) {
			return
		}
	}
}

type pathDSU struct {
	parent []int
}

func newPathDSU(n int) *pathDSU {
	d := &pathDSU{parent: make([]int, n)}
	for i := range d.parent {
		d.parent[i] = i
	}
	return d
}

func (d *pathDSU) find(x int) int {
	for d.parent[x] != x {
		d.parent[x] = d.parent[d.parent[x]]
		x = d.parent[x]
	}
	return x
}

func (d *pathDSU) union(a, b int) {
	a, b = d.find(a), d.find(b)
	if a != b {
		d.parent[b] = a
	}
}

// normalizePathSetPath aligns a call-argument path with the FileTree namespace.
// HMDFS keeps its merge_view prefix; GlusterFS has an empty prefix and must
// not inherit NormalizeMergeViewPath's merge_view/ prepend. FindNode already
// applies the same prefix internally, so specs must use this canonical form.
func normalizePathSetPath(ft *FileTree, path string) string {
	if path == "" {
		return ""
	}
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "/")
	if ft == nil || ft.Prefix == "" {
		return path
	}
	prefix := strings.TrimSuffix(ft.Prefix, "/")
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		return path
	}
	return prefix + "/" + path
}

func pathSetNodeConstraint(ft *FileTree, path string) int {
	if ft == nil {
		return relTargetAny
	}
	node := ft.FindNode(path)
	if node == nil {
		return relTargetAny
	}
	if nodeIsDir(node) {
		return relTargetDir
	}
	return relTargetFile
}

func buildPathMembers(ps []*Prog, ft *FileTree, profiles ...*DFSProfile) []pathMember {
	profile := hmdfsProfile
	if len(profiles) > 0 && profiles[0] != nil {
		profile = profiles[0]
	}
	var members []pathMember
	for pi, p := range ps {
		for ci, call := range p.Calls {
			if call == nil || call.Meta == nil {
				continue
			}
			pos := GroupPosition{ProgIdx: pi, CallIdx: ci}
			name := call.Meta.CallName
			// readlink's linkpath is a program-local binding, not a FileTree
			// node; PathSet does not move it on its own.
			if name == "readlink" {
				continue
			}
			constraint, ok := memberPathConstraint(ps, pos)
			if !ok {
				continue
			}
			switch name {
			case "link":
				constraint = relTargetFile
			case "symlink":
				constraint = relTargetAny
			}
			if name == "link" || name == "symlink" {
				target := extractPathFromCallByArgIdx(call, 0)
				if name == "symlink" {
					linkPath := extractPathFromCallByArgIdx(call, 1)
					target = symlinkCanonicalTarget(linkPath, target)
				}
				target = normalizePathSetPath(ft, target)
				if target != "" {
					node := ft.FindNode(target)
					if node == nil || !nodeMatchesConstraint(node, constraint) {
						continue
					}
					role := pathRoleLinkTarget
					if name == "symlink" {
						role = pathRoleSymlinkTarget
					}
					members = append(members, pathMember{
						Pos: pos,
						Paths: []pathSpec{{
							Path: target, Kind: pathExisting, Role: role, ArgIdx: 0,
							TypeConstraint: constraint,
						}},
						Constraint: constraint,
					})
				}
				continue
			}
			if strings.Contains(name, "rename") {
				src, dst := extractRenamePaths(call)
				src = normalizePathSetPath(ft, src)
				dst = normalizePathSetPath(ft, dst)
				sourceType := pathSetNodeConstraint(ft, src)
				var specs []pathSpec
				if src != "" {
					specs = append(specs, pathSpec{
						Path: src, Kind: pathExisting, Role: pathRoleRenameSrc,
						ArgIdx: 0, TypeConstraint: sourceType,
					})
				}
				if dst != "" {
					kind := pathFresh
					if ft != nil && ft.FindNode(dst) != nil {
						kind = pathExisting
					}
					specs = append(specs, pathSpec{
						Path: dst, Kind: kind, Role: pathRoleRenameDst,
						ArgIdx: 1, TypeConstraint: sourceType,
					})
				}
				if len(specs) > 0 {
					members = append(members, pathMember{Pos: pos, Paths: specs, Constraint: constraint})
				}
				continue
			}
			path := extractPathFromCall(call)
			if path == "" {
				continue
			}
			path = normalizePathSetPath(ft, path)
			if path == "" {
				continue
			}
			kind := pathExisting
			role := pathRoleDirect
			switch {
			case strings.Contains(name, "mkdir"):
				role = pathRoleMkdir
				if ft == nil || ft.FindNode(path) == nil {
					kind = pathFresh
				}
			case strings.Contains(name, "creat"):
				role = pathRoleCreat
				if ft == nil || ft.FindNode(path) == nil {
					kind = pathFresh
				}
			case strings.Contains(name, "open"):
				if profile.Name == "glusterfs" && openPathCreatesFile(p.Target, call) {
					role = pathRoleCreat
					if ft == nil || ft.FindNode(path) == nil {
						kind = pathFresh
					}
				}
			}
			members = append(members, pathMember{
				Pos: pos,
				Paths: []pathSpec{{
					Path: path, Kind: kind, Role: role, ArgIdx: 0,
					TypeConstraint: constraint,
				}},
				Constraint: constraint,
			})
		}
	}
	return members
}

func pathMembersRelated(a, b pathMember) bool {
	for _, pa := range a.Paths {
		for _, pb := range b.Paths {
			if pathRelBetween(pa.Path, pb.Path) != PathNoRel {
				return true
			}
		}
	}
	return false
}

func buildPathSet(ps []*Prog, anchor GroupPosition, ft *FileTree, profiles ...*DFSProfile) (*pathSet, bool) {
	members := buildPathMembers(ps, ft, profiles...)
	if len(members) == 0 {
		return nil, false
	}
	anchorIdx := -1
	for i := range members {
		if members[i].Pos == anchor {
			anchorIdx = i
			break
		}
	}
	if anchorIdx < 0 {
		return nil, false
	}
	dsu := newPathDSU(len(members))
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			if pathMembersRelated(members[i], members[j]) {
				dsu.union(i, j)
			}
		}
	}
	root := dsu.find(anchorIdx)
	var set []pathMember
	for i := range members {
		if dsu.find(i) == root {
			set = append(set, members[i])
		}
	}
	if len(set) == 0 {
		return nil, false
	}
	prefix := ""
	if ft != nil {
		prefix = ft.Prefix
	}
	return &pathSet{Anchor: anchor, Members: set, Prefix: prefix}, true
}

func pathSetFindMember(set *pathSet, pos GroupPosition) (*pathMember, bool) {
	for i := range set.Members {
		if set.Members[i].Pos == pos {
			return &set.Members[i], true
		}
	}
	return nil, false
}

func pathSetSinglePath(m *pathMember) (pathSpec, bool) {
	if m == nil || len(m.Paths) != 1 {
		return pathSpec{}, false
	}
	return m.Paths[0], true
}

func pathSetCallName(ps []*Prog, pos GroupPosition) string {
	if pos.ProgIdx < 0 || pos.ProgIdx >= len(ps) {
		return ""
	}
	p := ps[pos.ProgIdx]
	if pos.CallIdx < 0 || pos.CallIdx >= len(p.Calls) {
		return ""
	}
	call := p.Calls[pos.CallIdx]
	if call == nil || call.Meta == nil {
		return ""
	}
	return call.Meta.CallName
}

func pickPathSetTarget(ps []*Prog, lcs *LayeredChoiceStrategy, set *pathSet, r *randGen) (*pathTarget, bool) {
	if !CoverageBoardReady() || set == nil {
		return nil, false
	}
	anchor, ok := pathSetFindMember(set, set.Anchor)
	if !ok {
		return nil, false
	}
	if _, ok := pathSetSinglePath(anchor); !ok {
		return nil, false
	}
	anchorName := pathSetCallName(ps, anchor.Pos)
	if anchorName == "" {
		return nil, false
	}
	anchorKind := lcs.profileOrDefault().TargetConstraint(anchorName)
	rels := []PathRelation{PathSame, PathChild, PathParent, PathSibling}
	var weights []int
	var members []*pathMember
	var pickedRels []PathRelation
	for i := range set.Members {
		m := &set.Members[i]
		if m.Pos == anchor.Pos {
			continue
		}
		if _, ok := pathSetSinglePath(m); !ok {
			continue
		}
		name := pathSetCallName(ps, m.Pos)
		if name == "" {
			continue
		}
		kind := lcs.profileOrDefault().TargetConstraint(name)
		for _, rel := range rels {
			if rel == PathChild && anchorKind == relTargetFile {
				continue
			}
			if rel == PathParent && kind == relTargetFile {
				continue
			}
			if rel == PathSame {
				if anchorKind == relTargetFile && kind == relTargetDir {
					continue
				}
				if anchorKind == relTargetDir && kind == relTargetFile {
					continue
				}
			}
			pot := CoveragePotential(anchorName, name, rel)
			if pot <= 0 {
				continue
			}
			weights = append(weights, pot)
			members = append(members, m)
			pickedRels = append(pickedRels, rel)
		}
	}
	idx := pickWeightedIndex(weights, r.Rand)
	if idx < 0 {
		return nil, false
	}
	member := members[idx]
	rel := pickedRels[idx]
	memberName := pathSetCallName(ps, member.Pos)
	depths := covBoard.uncoveredDepthPots(anchorName, memberName, rel)
	depths[0] = 0
	depth := pickWeightedIndex(depths[:], r.Rand)
	if depth <= 0 {
		return nil, false
	}
	return &pathTarget{
		Anchor:      anchor.Pos,
		Member:      member.Pos,
		Rel:         rel,
		DepthBucket: depth,
	}, true
}

func pathSetTopRoot(prefix, path string) string {
	p := path
	if prefix != "" {
		p = strings.TrimPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

func pathSetTopPath(prefix, top string) string {
	if top == "" {
		return ""
	}
	if prefix == "" {
		return top
	}
	return strings.TrimSuffix(prefix, "/") + "/" + top
}

func pathSetRoots(set *pathSet) []string {
	seen := make(map[string]bool)
	var roots []string
	for _, m := range set.Members {
		for _, spec := range m.Paths {
			top := pathSetTopRoot(set.Prefix, spec.Path)
			if top == "" || seen[top] {
				continue
			}
			seen[top] = true
			roots = append(roots, top)
		}
	}
	return roots
}

func pathUnderRoot(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func pathSetCommonPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	parts := strings.Split(paths[0], "/")
	for _, p := range paths[1:] {
		other := strings.Split(p, "/")
		n := len(parts)
		if len(other) < n {
			n = len(other)
		}
		i := 0
		for i < n && parts[i] == other[i] {
			i++
		}
		parts = parts[:i]
	}
	return strings.Join(parts, "/")
}

func pathSetBaseKind(set *pathSet, ft *FileTree, anchorConstraint int) int {
	var paths []string
	for _, m := range set.Members {
		for _, spec := range m.Paths {
			paths = append(paths, spec.Path)
		}
	}
	oldBase := pathSetCommonPrefix(paths)
	if oldBase == "" {
		return anchorConstraint
	}
	if node := ft.FindNode(oldBase); node != nil {
		if nodeIsDir(node) {
			return relTargetDir
		}
		return relTargetFile
	}
	for _, path := range paths {
		if path != oldBase && pathUnderRoot(path, oldBase) {
			return relTargetDir
		}
	}
	return anchorConstraint
}

func choosePathSetBase(set *pathSet, ps []*Prog, ft *FileTree, r *randGen) (string, []string, bool) {
	roots := pathSetRoots(set)
	if len(roots) == 0 {
		return "", nil, false
	}
	anchor, ok := pathSetFindMember(set, set.Anchor)
	if !ok {
		return "", nil, false
	}
	anchorConstraint := anchor.Constraint
	if len(roots) == 1 {
		var paths []string
		for _, m := range set.Members {
			for _, spec := range m.Paths {
				paths = append(paths, spec.Path)
			}
		}
		oldBase := pathSetCommonPrefix(paths)
		if oldBase == "" {
			return "", nil, false
		}
		baseKind := pathSetBaseKind(set, ft, anchorConstraint)
		for attempt := 0; attempt < 8; attempt++ {
			newBase := pickNewBasePath(ft, baseKind, -1, r.Rand, "", oldBase)
			if newBase == "" || newBase == oldBase || newBase == ft.Root.FullPath ||
				pathUnderRoot(newBase, oldBase) || !pathSatisfiesType(ft, newBase, baseKind) {
				continue
			}
			return newBase, nil, true
		}
		return "", nil, false
	}
	for attempt := 0; attempt < 8; attempt++ {
		newParent := pickNewBasePath(ft, relTargetDir, -1, r.Rand, "", "")
		if newParent == "" || newParent == ft.Root.FullPath {
			continue
		}
		bad := false
		for _, root := range roots {
			rootPath := pathSetTopPath(ft.Prefix, root)
			if pathUnderRoot(newParent, rootPath) {
				bad = true
				break
			}
		}
		if !bad {
			return "", []string{newParent}, true
		}
	}
	// No parent outside the selected roots: use an existing selected directory
	// as the new parent. The selected directory itself stays in place; the
	// other top-level paths become its children (Sibling -> Parent/Child).
	for _, root := range roots {
		rootPath := pathSetTopPath(ft.Prefix, root)
		if node := ft.FindNode(rootPath); node != nil && nodeIsDir(node) {
			return "", []string{rootPath}, true
		}
	}
	return "", nil, false
}

func mapPathSetPath(prefix, path, base string, parent string, roots []string) string {
	if parent == "" {
		suffix := strings.TrimPrefix(path, base)
		return base + suffix
	}
	if path == parent || strings.HasPrefix(path, parent+"/") {
		return path
	}
	top := pathSetTopRoot(prefix, path)
	if top == "" {
		return ""
	}
	rootPath := pathSetTopPath(prefix, top)
	rest := strings.TrimPrefix(path, rootPath)
	return parent + "/" + top + rest
}

func pathSetFreshParentCreated(ps []*Prog, ft *FileTree, pos GroupPosition, parent string) bool {
	if pos.ProgIdx < 0 || pos.ProgIdx >= len(ps) {
		return false
	}
	p := ps[pos.ProgIdx]
	end := pos.CallIdx
	if end > len(p.Calls) {
		end = len(p.Calls)
	}
	for i := 0; i < end; i++ {
		call := p.Calls[i]
		if call == nil || call.Meta == nil {
			continue
		}
		name := call.Meta.CallName
		if strings.Contains(name, "mkdir") {
			if normalizePathSetPath(ft, extractPathFromCall(call)) == parent {
				return true
			}
		}
	}
	return false
}

func pathSetParentValidAt(ps []*Prog, pos GroupPosition, parent string, ft *FileTree) bool {
	if ft == nil {
		return false
	}
	node := ft.FindNode(parent)
	if node != nil && nodeIsDir(node) {
		return true
	}
	return pathSetFreshParentCreated(ps, ft, pos, parent)
}

func pathSetParentValid(ps []*Prog, pos GroupPosition, path string, ft *FileTree) bool {
	return pathSetParentValidAt(ps, pos, GetParentDir(path), ft)
}

func pathSetProvisionalParentValid(ft *FileTree, parent string) bool {
	if ft == nil {
		return false
	}
	node := ft.FindNode(parent)
	if node == nil {
		return true
	}
	return nodeIsDir(node)
}

func pathSetSpecValidWithParent(ps []*Prog, pos GroupPosition, spec pathSpec, constraint int, ft *FileTree,
	parentValid func(string) bool) bool {
	if ft == nil || spec.Path == "" {
		return false
	}
	typeConstraint := constraint
	if spec.TypeConstraint != relTargetAny {
		typeConstraint = spec.TypeConstraint
	}
	node := ft.FindNode(spec.Path)
	switch spec.Role {
	case pathRoleMkdir:
		if spec.Kind == pathFresh {
			return node == nil && parentValid(GetParentDir(spec.Path))
		}
		return node != nil && nodeIsDir(node)
	case pathRoleCreat:
		if spec.Kind == pathFresh {
			if node != nil && node.Type != NodeTypeFile {
				return false
			}
			return parentValid(GetParentDir(spec.Path))
		}
		return node != nil && node.Type == NodeTypeFile
	case pathRoleRenameSrc:
		return node != nil && nodeMatchesConstraint(node, typeConstraint)
	case pathRoleRenameDst:
		if spec.Kind == pathFresh {
			if node != nil {
				return nodeMatchesConstraint(node, typeConstraint)
			}
			return parentValid(GetParentDir(spec.Path))
		}
		return node != nil && nodeMatchesConstraint(node, typeConstraint)
	default:
		return node != nil && nodeMatchesConstraint(node, typeConstraint)
	}
}

func pathSetSpecValid(ps []*Prog, pos GroupPosition, spec pathSpec, constraint int, ft *FileTree) bool {
	return pathSetSpecValidWithParent(ps, pos, spec, constraint, ft, func(parent string) bool {
		return pathSetParentValidAt(ps, pos, parent, ft)
	})
}

func pathSetSpecValidProvisional(ps []*Prog, pos GroupPosition, spec pathSpec, constraint int, ft *FileTree) bool {
	return pathSetSpecValidWithParent(ps, pos, spec, constraint, ft, func(parent string) bool {
		return pathSetProvisionalParentValid(ft, parent)
	})
}

func pathSetFallbackPathWithParent(ps []*Prog, pos GroupPosition, desired, base string, spec pathSpec,
	constraint int, ft *FileTree, r *randGen, parentValid func(string) bool) string {
	candidates := []string{desired, base}
	if p := GetParentDir(desired); p != "" {
		candidates = append(candidates, p)
	}
	if p := GetParentDir(base); p != "" {
		candidates = append(candidates, p)
	}
	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		next := spec
		next.Path = cand
		if pathSetSpecValidWithParent(ps, pos, next, constraint, ft, parentValid) {
			return cand
		}
	}
	if ft != nil {
		for _, rel := range []PathRelation{PathSame, PathParent, PathChild, PathSibling} {
			cand := ft.GetPathByRelation(base, "", rel, r.Rand, false, constraint)
			if cand == "" {
				continue
			}
			next := spec
			next.Path = cand
			if pathSetSpecValidWithParent(ps, pos, next, constraint, ft, parentValid) {
				return cand
			}
		}
	}
	return ""
}

func pathSetFallbackPath(ps []*Prog, pos GroupPosition, desired, base string, spec pathSpec,
	constraint int, ft *FileTree, r *randGen) string {
	return pathSetFallbackPathWithParent(ps, pos, desired, base, spec, constraint, ft, r, func(parent string) bool {
		return pathSetParentValidAt(ps, pos, parent, ft)
	})
}

func pathSetFallbackPathProvisional(ps []*Prog, pos GroupPosition, desired, base string, spec pathSpec,
	constraint int, ft *FileTree, r *randGen) string {
	return pathSetFallbackPathWithParent(ps, pos, desired, base, spec, constraint, ft, r, func(parent string) bool {
		return pathSetProvisionalParentValid(ft, parent)
	})
}

func pathSetAllPaths(set *pathSet) []string {
	var paths []string
	for _, m := range set.Members {
		for _, spec := range m.Paths {
			paths = append(paths, spec.Path)
		}
	}
	return paths
}

func pathSetDirectedBaseKind(ps []*Prog, set *pathSet, target *pathTarget) (int, bool) {
	anchor, ok := pathSetFindMember(set, target.Anchor)
	if !ok {
		return relTargetAny, false
	}
	member, ok := pathSetFindMember(set, target.Member)
	if !ok {
		return relTargetAny, false
	}
	anchorConstraint := anchor.Constraint
	memberConstraint := member.Constraint
	baseKind := anchorConstraint
	switch target.Rel {
	case PathChild:
		if baseKind == relTargetAny {
			baseKind = relTargetDir
		}
	case PathSame:
		if baseKind == relTargetAny && memberConstraint != relTargetAny {
			baseKind = memberConstraint
		}
	}
	return baseKind, true
}

func pathSetDirectedCandidates(ps []*Prog, set *pathSet, target *pathTarget, ft *FileTree, r *randGen) []pathRelocationCandidate {
	roots := pathSetRoots(set)
	anchor, ok := pathSetFindMember(set, target.Anchor)
	if !ok {
		return nil
	}
	anchorSpec, ok := pathSetSinglePath(anchor)
	if !ok {
		return nil
	}
	if len(roots) == 1 {
		oldBase := pathSetCommonPrefix(pathSetAllPaths(set))
		if oldBase == "" {
			return nil
		}
		baseKind, ok := pathSetDirectedBaseKind(ps, set, target)
		if !ok {
			return nil
		}
		bucket := pathAnchorBucket(target.Rel, target.DepthBucket)
		if bucket == 0 {
			return nil
		}
		var out []pathRelocationCandidate
		for attempt := 0; attempt < 8; attempt++ {
			newBase := pickNewBasePath(ft, baseKind, bucket, r.Rand, "", oldBase)
			if newBase == "" || newBase == oldBase || newBase == ft.Root.FullPath ||
				pathUnderRoot(newBase, oldBase) || !pathSatisfiesType(ft, newBase, baseKind) {
				continue
			}
			out = append(out, pathRelocationCandidate{
				OldBase: oldBase, NewBase: newBase, AnchorNew: newBase,
			})
		}
		return out
	}
	var out []pathRelocationCandidate
	seen := make(map[string]bool)
	add := func(parent string, selectedDir bool) {
		if parent == "" || seen[parent] {
			return
		}
		seen[parent] = true
		anchorNew := mapPathSetPath(ft.Prefix, anchorSpec.Path, "", parent, roots)
		if anchorNew != "" {
			out = append(out, pathRelocationCandidate{
				NewParent: parent, AnchorNew: anchorNew, SelectedDir: selectedDir,
			})
		}
	}
	for _, bucket := range pathSetForestParentBuckets(target.DepthBucket) {
		attempts := 3
		if bucket == -1 {
			attempts = 5
		}
		for attempt := 0; attempt < attempts; attempt++ {
			parent := pickNewBasePath(ft, relTargetDir, bucket, r.Rand, "", "")
			if parent == "" || parent == ft.Root.FullPath {
				continue
			}
			bad := false
			for _, root := range roots {
				if pathUnderRoot(parent, pathSetTopPath(ft.Prefix, root)) {
					bad = true
					break
				}
			}
			if !bad {
				add(parent, false)
			}
		}
	}
	for _, root := range roots {
		rootPath := pathSetTopPath(ft.Prefix, root)
		if node := ft.FindNode(rootPath); node != nil && nodeIsDir(node) {
			add(rootPath, true)
		}
	}
	return out
}

func pathSetForestParentBuckets(targetDepth int) []int {
	seen := make(map[int]bool)
	var out []int
	add := func(b int) {
		if b < 1 || b > 3 || seen[b] {
			return
		}
		seen[b] = true
		out = append(out, b)
	}
	add(targetDepth)
	add(targetDepth - 1)
	add(1)
	add(2)
	add(3)
	out = append(out, -1)
	return out
}

func pathSetCandidatePath(prefix, path string, cand pathRelocationCandidate, roots []string) string {
	if cand.NewParent != "" {
		return mapPathSetPath(prefix, path, "", cand.NewParent, roots)
	}
	if cand.OldBase == "" || cand.NewBase == "" {
		return ""
	}
	suffix := strings.TrimPrefix(path, cand.OldBase)
	return cand.NewBase + suffix
}

func pathSetTargetMemberPaths(ps []*Prog, set *pathSet, target *pathTarget,
	cand pathRelocationCandidate, ft *FileTree, r *randGen) []string {
	member, ok := pathSetFindMember(set, target.Member)
	if !ok {
		return nil
	}
	spec, ok := pathSetSinglePath(member)
	if !ok {
		return nil
	}
	constraint := member.Constraint
	seen := make(map[string]bool)
	var out []string
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		next := spec
		next.Path = path
		if !pathSetSpecValidProvisional(ps, member.Pos, next, constraint, ft) {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	if target.Rel == PathSame {
		add(cand.AnchorNew)
	} else {
		add(ft.GetPathByRelation(cand.AnchorNew, "", target.Rel, r.Rand, false, constraint))
	}
	add(pathSetCandidatePath(ft.Prefix, spec.Path, cand, pathSetRoots(set)))
	return out
}

func pathSetTargetLevel(target *pathTarget, anchorPath, memberPath string) int {
	if target == nil || anchorPath == "" || memberPath == "" {
		return 3
	}
	rel := pathRelBetween(anchorPath, memberPath)
	depth := int(DepthBucketOf(memberPath))
	if rel == target.Rel && depth == target.DepthBucket {
		return 0
	}
	if rel == target.Rel {
		return 1
	}
	if rel != PathNoRel && depth == target.DepthBucket {
		return 2
	}
	return 3
}

func buildTargetPathPlan(ps []*Prog, set *pathSet, target *pathTarget,
	cand pathRelocationCandidate, memberPath string, ft *FileTree, r *randGen) (*pathPlan, int, int, bool) {
	roots := pathSetRoots(set)
	plan := &pathPlan{Base: cand.NewBase}
	if cand.NewParent != "" {
		plan.Base = cand.NewParent
	}
	fallbacks := 0
	for _, m := range set.Members {
		update := pathUpdate{Pos: m.Pos}
		for _, spec := range m.Paths {
			mapped := spec
			switch {
			case m.Pos == target.Anchor:
				mapped.Path = cand.AnchorNew
			case m.Pos == target.Member:
				mapped.Path = memberPath
			default:
				mapped.Path = pathSetCandidatePath(ft.Prefix, spec.Path, cand, roots)
			}
			if mapped.Path == "" {
				return nil, 3, fallbacks, false
			}
			targetMember := m.Pos == target.Anchor || m.Pos == target.Member
			if !pathSetSpecValidProvisional(ps, m.Pos, mapped, m.Constraint, ft) {
				if targetMember {
					return nil, 3, fallbacks, false
				}
				mapped.Path = pathSetFallbackPathProvisional(ps, m.Pos, mapped.Path, plan.Base, spec, m.Constraint, ft, r)
				fallbacks++
			}
			if mapped.Path == "" {
				return nil, 3, fallbacks, false
			}
			update.Specs = append(update.Specs, mapped)
		}
		plan.Updates = append(plan.Updates, update)
	}
	if !pathSetConnected(set, plan) {
		atomic.AddUint64(&pathSetStats.candidateConnectivityFail, 1)
		return nil, 3, fallbacks, false
	}
	return plan, pathSetTargetLevel(target, cand.AnchorNew, memberPath), fallbacks, true
}

func mutatePathSetDirected(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	if !CoverageBoardReady() || lcs == nil || lcs.FileTree == nil {
		return false
	}
	anchor, _, ok := pickAnchor(ps, r, false)
	if !ok {
		return false
	}
	anchorPos := resolveFdTarget(ps, anchor)
	set, ok := buildPathSet(ps, anchorPos, lcs.FileTree, lcs.profileOrDefault())
	if !ok {
		return false
	}
	notePathSetBuilt(set, pathSetRoots(set), true)
	target, ok := pickPathSetTarget(ps, lcs, set, r)
	if !ok {
		atomic.AddUint64(&pathSetStats.noTarget, 1)
		return false
	}
	atomic.AddUint64(&pathSetStats.selected, 1)
	candidates := pathSetDirectedCandidates(ps, set, target, lcs.FileTree, r)
	if len(candidates) == 0 {
		atomic.AddUint64(&pathSetStats.noCandidates, 1)
		return false
	}
	atomic.AddUint64(&pathSetStats.candidateCount, uint64(len(candidates)))
	for _, cand := range candidates {
		if cand.NewParent == "" {
			continue
		}
		if cand.SelectedDir {
			atomic.AddUint64(&pathSetStats.selectedDirCandidates, 1)
		} else {
			atomic.AddUint64(&pathSetStats.externalParentCandidates, 1)
		}
	}
	bestLevel := 4
	bestFallbacks := 0
	bestSoft := false
	var bestPlan *pathPlan
	for _, cand := range candidates {
		for _, memberPath := range pathSetTargetMemberPaths(ps, set, target, cand, lcs.FileTree, r) {
			plan, level, fallbacks, ok := buildTargetPathPlan(ps, set, target, cand, memberPath, lcs.FileTree, r)
			if !ok {
				continue
			}
			quality, ok := validatePathSetPlan(ps, set, plan, lcs.FileTree, lcs.tscoffs)
			if !ok {
				continue
			}
			better := bestPlan == nil || level < bestLevel ||
				(level == bestLevel && bestSoft && !quality.SoftParent)
			if !better {
				continue
			}
			bestLevel = level
			bestPlan = plan
			bestFallbacks = fallbacks
			bestSoft = quality.SoftParent
			if bestLevel == 0 && !bestSoft {
				break
			}
		}
		if bestLevel == 0 && !bestSoft {
			break
		}
	}
	if bestPlan == nil {
		atomic.AddUint64(&pathSetStats.noPlan, 1)
		atomic.AddUint64(&pathSetStats.failOther, 1)
		return false
	}
	if !applyPathSetPlan(ps, bestPlan, r) {
		atomic.AddUint64(&pathSetStats.applyFail, 1)
		atomic.AddUint64(&pathSetStats.failOther, 1)
		return false
	}
	if bestFallbacks > 0 {
		atomic.AddUint64(&pathSetStats.committedFallbackCount, uint64(bestFallbacks))
	}
	switch bestLevel {
	case 0:
		atomic.AddUint64(&pathSetStats.exact, 1)
	case 1:
		atomic.AddUint64(&pathSetStats.level1a, 1)
	case 2:
		atomic.AddUint64(&pathSetStats.level1b, 1)
	default:
		atomic.AddUint64(&pathSetStats.lost, 1)
	}
	recordLastMutation("MutatePathSetDirected")
	return true
}

func buildPathPlan(ps []*Prog, set *pathSet, base, parent string, roots []string, ft *FileTree, r *randGen) (*pathPlan, bool) {
	plan := &pathPlan{Base: base}
	if parent != "" {
		plan.Base = parent
	}
	for _, m := range set.Members {
		update := pathUpdate{Pos: m.Pos}
		for _, spec := range m.Paths {
			desired := mapPathSetPath(ft.Prefix, spec.Path, base, parent, roots)
			if desired == "" {
				return nil, false
			}
			mapped := spec
			mapped.Path = desired
			if !pathSetSpecValidProvisional(ps, m.Pos, mapped, m.Constraint, ft) {
				desired = pathSetFallbackPathProvisional(ps, m.Pos, desired, plan.Base, spec, m.Constraint, ft, r)
				mapped.Path = desired
			}
			if desired == "" {
				return nil, false
			}
			update.Specs = append(update.Specs, mapped)
		}
		plan.Updates = append(plan.Updates, update)
	}
	return plan, true
}

func pathSetMappedMember(m pathMember, update pathUpdate) pathMember {
	if len(update.Specs) != len(m.Paths) {
		return pathMember{Pos: m.Pos, Constraint: m.Constraint}
	}
	m.Paths = append([]pathSpec(nil), update.Specs...)
	return m
}

func pathSetConnected(set *pathSet, plan *pathPlan) bool {
	if set == nil || plan == nil || len(plan.Updates) == 0 || len(plan.Updates) != len(set.Members) {
		return false
	}
	mapped := make([]pathMember, len(plan.Updates))
	for i := range plan.Updates {
		update := plan.Updates[i]
		member := set.Members[i]
		if update.Pos != member.Pos || len(update.Specs) != len(member.Paths) {
			return false
		}
		mapped[i] = pathSetMappedMember(member, update)
	}
	dsu := newPathDSU(len(mapped))
	for i := 0; i < len(mapped); i++ {
		for j := i + 1; j < len(mapped); j++ {
			if pathMembersRelated(mapped[i], mapped[j]) {
				dsu.union(i, j)
			}
		}
	}
	root := dsu.find(0)
	for i := 1; i < len(mapped); i++ {
		if dsu.find(i) != root {
			return false
		}
	}
	return true
}

type pathPlanMappedSpec struct {
	pos  GroupPosition
	spec pathSpec
}

func pathSetPlanPosValid(ps []*Prog, pos GroupPosition) bool {
	if pos.ProgIdx < 0 || pos.ProgIdx >= len(ps) {
		return false
	}
	return pos.CallIdx >= 0 && pos.CallIdx < len(ps[pos.ProgIdx].Calls)
}

func pathSetPlanParentValid(ps []*Prog, mapped []pathPlanMappedSpec, ft *FileTree, tscoffs []int64,
	consumer GroupPosition, parent string, quality *pathPlanQuality) bool {
	if ft == nil {
		return false
	}
	if node := ft.FindNode(parent); node != nil {
		return nodeIsDir(node)
	}
	for _, ms := range mapped {
		if ms.spec.Role != pathRoleMkdir || ms.spec.Kind != pathFresh || ms.spec.Path != parent {
			continue
		}
		if ms.pos == consumer {
			continue
		}
		if ms.pos.ProgIdx == consumer.ProgIdx {
			if ms.pos.CallIdx < consumer.CallIdx {
				return true
			}
			continue
		}
		if tscoffs == nil || !pathSetPlanPosValid(ps, ms.pos) || !pathSetPlanPosValid(ps, consumer) {
			continue
		}
		mkdir := ps[ms.pos.ProgIdx].Calls[ms.pos.CallIdx]
		consumerCall := ps[consumer.ProgIdx].Calls[consumer.CallIdx]
		if mkdir == nil || consumerCall == nil || mkdir.CheckInfo == nil || consumerCall.CheckInfo == nil {
			continue
		}
		mkdirE := int64(mkdir.CheckInfo.Etime) - tscoffFor(tscoffs, ms.pos.ProgIdx)
		consumerS := int64(consumerCall.CheckInfo.Stime) - tscoffFor(tscoffs, consumer.ProgIdx)
		if mkdirE < consumerS {
			quality.SoftParent = true
			return true
		}
	}
	return false
}

func validatePathSetPlan(ps []*Prog, set *pathSet, plan *pathPlan, ft *FileTree,
	tscoffs []int64) (pathPlanQuality, bool) {
	var quality pathPlanQuality
	if set == nil || plan == nil || ft == nil || len(plan.Updates) == 0 ||
		len(plan.Updates) != len(set.Members) {
		return quality, false
	}
	mapped := make([]pathPlanMappedSpec, 0, len(plan.Updates))
	for i := range plan.Updates {
		update := plan.Updates[i]
		member := set.Members[i]
		if update.Pos != member.Pos || len(update.Specs) != len(member.Paths) {
			return quality, false
		}
		for _, spec := range update.Specs {
			mapped = append(mapped, pathPlanMappedSpec{pos: member.Pos, spec: spec})
		}
	}
	freshPathPos := make(map[string]GroupPosition)
	for _, ms := range mapped {
		if ms.spec.Kind != pathFresh {
			continue
		}
		if prev, ok := freshPathPos[ms.spec.Path]; ok {
			if prev != ms.pos {
				quality.DuplicateFresh++
			}
			continue
		}
		freshPathPos[ms.spec.Path] = ms.pos
	}
	if quality.DuplicateFresh > 0 {
		atomic.AddUint64(&pathSetStats.duplicateFresh, uint64(quality.DuplicateFresh))
	}
	for i := range plan.Updates {
		member := set.Members[i]
		update := plan.Updates[i]
		for _, spec := range update.Specs {
			parentValid := func(parent string) bool {
				return pathSetPlanParentValid(ps, mapped, ft, tscoffs, member.Pos, parent, &quality)
			}
			if !pathSetSpecValidWithParent(ps, member.Pos, spec, member.Constraint, ft, parentValid) {
				return quality, false
			}
		}
	}
	return quality, true
}

func relocatePathSet(ps []*Prog, lcs *LayeredChoiceStrategy, set *pathSet, r *randGen) bool {
	if set == nil || lcs == nil || lcs.FileTree == nil {
		return false
	}
	roots := pathSetRoots(set)
	var bestPlan *pathPlan
	bestSoft := true
	for attempt := 0; attempt < 4; attempt++ {
		base, parents, ok := choosePathSetBase(set, ps, lcs.FileTree, r)
		if !ok {
			break
		}
		parent := ""
		if len(parents) > 0 {
			parent = parents[0]
		}
		plan, ok := buildPathPlan(ps, set, base, parent, roots, lcs.FileTree, r)
		if !ok || !pathSetConnected(set, plan) {
			continue
		}
		quality, ok := validatePathSetPlan(ps, set, plan, lcs.FileTree, lcs.tscoffs)
		if !ok {
			continue
		}
		if bestPlan == nil || (bestSoft && !quality.SoftParent) {
			bestPlan = plan
			bestSoft = quality.SoftParent
		}
		if !bestSoft {
			break
		}
	}
	if bestPlan != nil && applyPathSetPlan(ps, bestPlan, r) {
		return true
	}
	return false
}

func applyPathSetPlan(ps []*Prog, plan *pathPlan, r *randGen) bool {
	clones := Clones(ps)
	for _, update := range plan.Updates {
		if update.Pos.ProgIdx < 0 || update.Pos.ProgIdx >= len(clones) {
			return false
		}
		p := clones[update.Pos.ProgIdx]
		if update.Pos.CallIdx < 0 || update.Pos.CallIdx >= len(p.Calls) {
			return false
		}
		call := p.Calls[update.Pos.CallIdx]
		if len(update.Specs) == 0 {
			return false
		}
		for _, spec := range update.Specs {
			if spec.ArgIdx < 0 || spec.ArgIdx >= len(call.Args) {
				return false
			}
			if spec.Role == pathRoleSymlinkTarget {
				linkPath := extractPathFromCallByArgIdx(call, 1)
				payload := symlinkPayload(linkPath, spec.Path)
				if payload == "" ||
					!updateCallPathByArgIdx(p, call, 0, payload, r) {
					return false
				}
				continue
			}
			if !updateCallPathByArgIdx(p, call, spec.ArgIdx, spec.Path, r) {
				return false
			}
		}
	}
	for i := range ps {
		ps[i] = clones[i]
	}
	return true
}
