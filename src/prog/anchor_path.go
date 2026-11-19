package prog

type AnchorPathKind int

const (
	AnchorPathUnknown AnchorPathKind = iota
	AnchorPathExisting
	AnchorPathFreshDir
	AnchorPathFreshFile
	AnchorPathFreshRenameDst
	AnchorPathFreshSymlink
)

func (k AnchorPathKind) IsFresh() bool {
	return k == AnchorPathFreshDir || k == AnchorPathFreshFile ||
		k == AnchorPathFreshRenameDst || k == AnchorPathFreshSymlink
}

type anchorPathInfo struct {
	Kind        AnchorPathKind
	IsDir       bool
	FollowIsDir bool
	CreatorIdx  int
}

func classifyAnchorPath(p *Prog, ft *FileTree, pos GroupPosition, path string, requireSuccess bool, profiles ...*DFSProfile) (anchorPathInfo, bool) {
	profile := hmdfsProfile
	if len(profiles) > 0 && profiles[0] != nil {
		profile = profiles[0]
	}
	if p == nil || ft == nil || path == "" || pos.ProgIdx != 0 ||
		pos.CallIdx < 0 || pos.CallIdx >= len(p.Calls) {
		return anchorPathInfo{}, false
	}
	path = canonicalFreshPath(ft, path)
	if path == "" {
		return anchorPathInfo{}, false
	}
	if profile.TracksPathState() {
		if absent := freshPathAbsentBefore(p, pos.CallIdx, profile); pathSetAbsentIn(absent, path) {
			// The path is absent when the anchor starts. Only the anchor's
			// own creator effect may make it valid.
			kind, isDir, idx, ok := anchorCreatorKind(p, ft, p.Calls[pos.CallIdx],
				path, pos.CallIdx, requireSuccess, profile)
			if !ok || idx != pos.CallIdx {
				return anchorPathInfo{}, false
			}
			return anchorPathInfo{Kind: kind, IsDir: isDir, FollowIsDir: isDir, CreatorIdx: idx}, true
		}
		if node := ft.FindNode(path); node != nil {
			// The baseline node exists. A successful in-program deletion
			// followed by a recreation changes its type; the latest creator
			// wins when the path was absent immediately before it.
			if kind, isDir, idx, ok := findAnchorCreator(p, ft, path, pos.CallIdx, requireSuccess, profile); ok {
				if pathSetAbsentIn(freshPathAbsentBefore(p, idx, profile), path) {
					return anchorPathInfo{Kind: kind, IsDir: isDir, FollowIsDir: isDir, CreatorIdx: idx}, true
				}
			}
			return anchorPathInfo{
				Kind:        AnchorPathExisting,
				IsDir:       node.Type != NodeTypeFile,
				FollowIsDir: node.Type != NodeTypeFile,
				CreatorIdx:  -1,
			}, true
		}
		kind, isDir, idx, ok := findAnchorCreator(p, ft, path, pos.CallIdx, requireSuccess, profile)
		if !ok {
			return anchorPathInfo{}, false
		}
		return anchorPathInfo{Kind: kind, IsDir: isDir, FollowIsDir: isDir, CreatorIdx: idx}, true
	}
	if node := ft.FindNode(path); node != nil {
		return anchorPathInfo{
			Kind:        AnchorPathExisting,
			IsDir:       node.Type != NodeTypeFile,
			FollowIsDir: node.Type != NodeTypeFile,
			CreatorIdx:  -1,
		}, true
	}
	kind, isDir, idx, ok := findAnchorCreator(p, ft, path, pos.CallIdx, requireSuccess, profile)
	if !ok {
		return anchorPathInfo{}, false
	}
	return anchorPathInfo{Kind: kind, IsDir: isDir, FollowIsDir: isDir, CreatorIdx: idx}, true
}

// anchorCreatorKind classifies one call as the creator of path. It is shared
// by the forward/backward scan and by the anchor's own-effect check.
func anchorCreatorKind(p *Prog, ft *FileTree, call *Call, path string, idx int,
	requireSuccess bool, profile *DFSProfile) (AnchorPathKind, bool, int, bool) {
	if p == nil || call == nil || call.Meta == nil || path == "" {
		return AnchorPathUnknown, false, -1, false
	}
	if call.CheckInfo != nil && call.CheckInfo.Retv < 0 {
		// A failed syscall never creates a path, even when the caller does
		// not require success (seed anchors may run without CheckInfo).
		return AnchorPathUnknown, false, -1, false
	}
	if requireSuccess && !callReturnedSuccess(call) {
		return AnchorPathUnknown, false, -1, false
	}
	switch call.Meta.CallName {
	case "mkdir":
		if canonicalFreshPath(ft, extractPathFromCall(call)) == path {
			return AnchorPathFreshDir, true, idx, true
		}
	case "creat":
		if canonicalFreshPath(ft, extractPathFromCall(call)) == path {
			return AnchorPathFreshFile, false, idx, true
		}
	case "open":
		if !openPathCreatesFile(p.Target, call) {
			return AnchorPathUnknown, false, -1, false
		}
		if canonicalFreshPath(ft, extractPathFromCall(call)) == path {
			return AnchorPathFreshFile, false, idx, true
		}
	case "rename":
		src, dst := extractRenamePaths(call)
		if canonicalFreshPath(ft, dst) != path {
			return AnchorPathUnknown, false, -1, false
		}
		isDir, ok := anchorSourceIsDir(p, ft, canonicalFreshPath(ft, src), idx, requireSuccess, profile)
		if !ok {
			return AnchorPathUnknown, false, -1, false
		}
		return AnchorPathFreshRenameDst, isDir, idx, true
	case "link":
		linkPath := canonicalFreshPath(ft, extractPathFromCallByArgIdx(call, 1))
		if linkPath != path {
			break
		}
		target := canonicalFreshPath(ft, extractPathFromCallByArgIdx(call, 0))
		if target == "" {
			return AnchorPathUnknown, false, -1, false
		}
		if profile != nil && profile.TracksPathState() &&
			pathSetAbsentIn(freshPathAbsentBefore(p, idx, profile), target) {
			return AnchorPathUnknown, false, -1, false
		}
		if node := ft.FindNode(target); node != nil {
			if node.Type != NodeTypeFile {
				return AnchorPathUnknown, false, -1, false
			}
		} else {
			kind, isDir, _, ok := findAnchorCreator(p, ft, target, idx-1, requireSuccess, profile)
			if !ok || isDir ||
				(kind != AnchorPathFreshFile && kind != AnchorPathFreshRenameDst) {
				return AnchorPathUnknown, false, -1, false
			}
		}
		// v1 link targets are regular files; the new dentry is a file alias
		// and follows the target type.
		return AnchorPathFreshFile, false, idx, true
	case "symlink":
		linkPath := canonicalFreshPath(ft, extractPathFromCallByArgIdx(call, 1))
		if linkPath != path {
			return AnchorPathUnknown, false, -1, false
		}
		target := canonicalFreshPath(ft, symlinkCanonicalTarget(linkPath,
			extractPathFromCallByArgIdx(call, 0)))
		isDir, ok := anchorSourceIsDir(p, ft, target, idx, requireSuccess, profile)
		if !ok {
			return AnchorPathUnknown, false, -1, false
		}
		return AnchorPathFreshSymlink, isDir, idx, true
	}
	return AnchorPathUnknown, false, -1, false
}

func findAnchorCreator(p *Prog, ft *FileTree, path string, end int, requireSuccess bool, profiles ...*DFSProfile) (AnchorPathKind, bool, int, bool) {
	profile := hmdfsProfile
	if len(profiles) > 0 && profiles[0] != nil {
		profile = profiles[0]
	}
	if p == nil || path == "" || end < 0 {
		return AnchorPathUnknown, false, -1, false
	}
	path = canonicalFreshPath(ft, path)
	if path == "" {
		return AnchorPathUnknown, false, -1, false
	}
	if end >= len(p.Calls) {
		end = len(p.Calls) - 1
	}
	if profile.TracksPathState() {
		// GlusterFS: a successful deletion followed by a recreation must
		// classify the current state, so the latest creator wins.
		for i := end; i >= 0; i-- {
			if kind, isDir, idx, ok := anchorCreatorKind(p, ft, p.Calls[i], path, i,
				requireSuccess, profile); ok {
				return kind, isDir, idx, true
			}
		}
	} else {
		// HMDFS legacy: the first creator wins.
		for i := 0; i <= end; i++ {
			if kind, isDir, idx, ok := anchorCreatorKind(p, ft, p.Calls[i], path, i,
				requireSuccess, profile); ok {
				return kind, isDir, idx, true
			}
		}
	}
	return AnchorPathUnknown, false, -1, false
}

func anchorSourceIsDir(p *Prog, ft *FileTree, path string, before int, requireSuccess bool, profiles ...*DFSProfile) (bool, bool) {
	profile := hmdfsProfile
	if len(profiles) > 0 && profiles[0] != nil {
		profile = profiles[0]
	}
	if p == nil || path == "" {
		return false, false
	}
	path = canonicalFreshPath(ft, path)
	if path == "" {
		return false, false
	}
	if profile.TracksPathState() && pathSetAbsentIn(freshPathAbsentBefore(p, before, profile), path) {
		return false, false
	}
	kind, isDir, idx, ok := findAnchorCreator(p, ft, path, before-1, requireSuccess, profile)
	if ok && profile.TracksPathState() &&
		pathSetAbsentIn(freshPathAbsentBefore(p, idx, profile), path) {
		return isDir, true
	}
	if node := ft.FindNode(path); node != nil {
		return node.Type != NodeTypeFile, true
	}
	if !ok || !kind.IsFresh() {
		return false, false
	}
	return isDir, true
}

func callSucceeded(call *Call) bool {
	return call != nil && call.CheckInfo != nil && call.CheckInfo.Retv == 0
}

// callReturnedSuccess reports a successful syscall by its raw return value:
// negative errno on failure, zero or a positive fd/byte count on success.
// callSucceeded's Retv==0 is only valid for calls whose success return is
// exactly zero; anchor admission and open creators must accept fd>=0 and
// write byte counts >0.
func callReturnedSuccess(call *Call) bool {
	return call != nil && call.CheckInfo != nil && call.CheckInfo.Retv >= 0
}

func openPathCreatesFile(target *Target, call *Call) bool {
	created, known := openCallCreatesFile(target, call)
	return known && created
}

func openCallCreatesFile(target *Target, call *Call) (bool, bool) {
	if target == nil || call == nil {
		return false, false
	}
	flags, ok := parseOpenFlags(call)
	if !ok {
		return false, false
	}
	oCreate, ok := target.LookupConst("O_CREAT")
	if !ok {
		return false, false
	}
	if flags&oCreate == 0 {
		return false, true
	}
	if oTmp, ok := target.LookupConst("O_TMPFILE"); ok {
		if flags&oTmp != 0 {
			return false, true
		}
	} else {
		oDir, ok := target.LookupConst("O_DIRECTORY")
		if !ok || flags&oDir != 0 {
			return false, false
		}
	}
	return true, true
}

func freshParentNodeEligible(ft *FileTree, path string) bool {
	if ft == nil || path == "" {
		return false
	}
	parent := parentDirOrRoot(path)
	if parent == "" {
		// The test root is a valid parent only when it is the namespace root
		// itself (GlusterFS has an empty prefix); HMDFS paths must stay under
		// merge_view/.
		return ft.Prefix == "" && ft.Root != nil
	}
	node := ft.FindNode(parent)
	return node != nil && node.Type != NodeTypeFile && node.Type != NodeTypeTmpDir
}

// freshParentEligible reports whether parent can host a fresh child. Empty
// parent is the namespace root only for profiles with an empty tree prefix
// (GlusterFS); HMDFS must stay under merge_view/. Unlike
// freshParentNodeEligible, this checks the parent node itself, not the
// parent of a fresh path.
func freshParentEligible(ft *FileTree, parent string) bool {
	if ft == nil {
		return false
	}
	if parent == "" {
		return ft.Prefix == "" && ft.Root != nil
	}
	node := ft.FindNode(parent)
	return node != nil && node.Type != NodeTypeFile && node.Type != NodeTypeTmpDir
}

// freshPathParentEligible is reserved for future nested fresh-parent support.
// C1 and seed derivation intentionally use freshParentNodeEligible to enforce
// the first-version one-layer fresh rule. Do not use this helper as a
// production gate without revisiting that rule and adding nested tests.
func freshPathParentEligible(p *Prog, ft *FileTree, pos GroupPosition, path string, requireSuccess bool) bool {
	if p == nil || ft == nil || path == "" {
		return false
	}
	parent := GetParentDir(path)
	if parent == "" {
		return false
	}
	if node := ft.FindNode(parent); node != nil {
		return node.Type != NodeTypeFile && node.Type != NodeTypeTmpDir
	}
	info, ok := classifyAnchorPath(p, ft, pos, parent, requireSuccess)
	return ok && info.Kind == AnchorPathFreshDir && info.IsDir
}
