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

func classifyAnchorPath(p *Prog, ft *FileTree, pos GroupPosition, path string, requireSuccess bool) (anchorPathInfo, bool) {
	if p == nil || ft == nil || path == "" || pos.ProgIdx != 0 ||
		pos.CallIdx < 0 || pos.CallIdx >= len(p.Calls) {
		return anchorPathInfo{}, false
	}
	if node := ft.FindNode(path); node != nil {
		return anchorPathInfo{
			Kind:        AnchorPathExisting,
			IsDir:       node.Type != NodeTypeFile,
			FollowIsDir: node.Type != NodeTypeFile,
			CreatorIdx:  -1,
		}, true
	}
	kind, isDir, idx, ok := findAnchorCreator(p, ft, path, pos.CallIdx, requireSuccess)
	if !ok {
		return anchorPathInfo{}, false
	}
	return anchorPathInfo{Kind: kind, IsDir: isDir, FollowIsDir: isDir, CreatorIdx: idx}, true
}

func findAnchorCreator(p *Prog, ft *FileTree, path string, end int, requireSuccess bool) (AnchorPathKind, bool, int, bool) {
	if p == nil || path == "" || end < 0 {
		return AnchorPathUnknown, false, -1, false
	}
	if end >= len(p.Calls) {
		end = len(p.Calls) - 1
	}
	for i := 0; i <= end; i++ {
		call := p.Calls[i]
		if call == nil || call.Meta == nil {
			continue
		}
		if requireSuccess && !callReturnedSuccess(call) {
			continue
		}
		switch call.Meta.CallName {
		case "mkdir":
			if extractPathFromCall(call) == path {
				return AnchorPathFreshDir, true, i, true
			}
		case "creat":
			if extractPathFromCall(call) == path {
				return AnchorPathFreshFile, false, i, true
			}
		case "open":
			if !openPathCreatesFile(p.Target, call) {
				continue
			}
			if extractPathFromCall(call) == path {
				return AnchorPathFreshFile, false, i, true
			}
		case "rename":
			src, dst := extractRenamePaths(call)
			if dst != path {
				continue
			}
			isDir, ok := anchorSourceIsDir(p, ft, src, i, requireSuccess)
			if !ok {
				return AnchorPathUnknown, false, -1, false
			}
			return AnchorPathFreshRenameDst, isDir, i, true
		case "link":
			if extractPathFromCallByArgIdx(call, 1) == path {
				// v1 link targets are regular files; the new dentry is a file
				// alias and follows the target type.
				return AnchorPathFreshFile, false, i, true
			}
		case "symlink":
			if extractPathFromCallByArgIdx(call, 1) != path {
				continue
			}
			target := symlinkCanonicalTarget(path, extractPathFromCallByArgIdx(call, 0))
			isDir, ok := anchorSourceIsDir(p, ft, target, i, requireSuccess)
			if !ok {
				return AnchorPathUnknown, false, -1, false
			}
			return AnchorPathFreshSymlink, isDir, i, true
		}
	}
	return AnchorPathUnknown, false, -1, false
}

func anchorSourceIsDir(p *Prog, ft *FileTree, path string, before int, requireSuccess bool) (bool, bool) {
	if path == "" {
		return false, false
	}
	if node := ft.FindNode(path); node != nil {
		return node.Type != NodeTypeFile, true
	}
	kind, isDir, _, ok := findAnchorCreator(p, ft, path, before-1, requireSuccess)
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
