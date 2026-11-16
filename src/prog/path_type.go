package prog

// pathTypeState is the conservative path-type state used by the DAG symlink
// resolver. It is deliberately not a full filesystem model.
type pathTypeState uint8

const (
	pathTypeUnknown pathTypeState = iota
	pathTypeFile
	pathTypeDir
	pathTypeGone
)

// pathTypeBefore resolves the type of target for a symlink event.
//
// The resolution is an approximation:
//  1. scan the producing program's calls before callIdx (including deletion,
//     rename, link and symlink state);
//  2. fall back to the baseline FileTree;
//  3. scan other client programs from the beginning in program order and use
//     the first call that establishes target's type.
//
// Cross-program order is not a visibility proof; it is only a deterministic
// fallback for fresh paths and is used solely for the DAG B-side type.
func pathTypeBefore(ps []*Prog, progIdx, callIdx, servNum int,
	ft *FileTree, target string) (isDir bool, known bool) {
	if target == "" {
		return false, false
	}
	if progIdx >= 0 && progIdx < len(ps) && callIdx >= 0 && callIdx <= len(ps[progIdx].Calls) {
		if st, ok := pathTypePrefix(ps[progIdx], ft, callIdx, target); ok {
			return st == pathTypeDir, true
		}
	}
	if n := findPathNode(ft, target); n != nil {
		return n.Type != NodeTypeFile, true
	}
	for idx, p := range ps {
		if idx == progIdx || idx < servNum || p == nil {
			continue
		}
		for ci, call := range p.Calls {
			if st, ok := crossProgramCallType(p, ft, ci, call, target); ok {
				return st == pathTypeDir, true
			}
		}
	}
	return false, false
}

// pathTypePrefix scans p.Calls[:end] and returns the state of target. Baseline
// FileTree is consulted only when the prefix did not explicitly delete or
// rename the path.
func pathTypePrefix(p *Prog, ft *FileTree, end int, target string) (pathTypeState, bool) {
	if p == nil || target == "" {
		return pathTypeUnknown, false
	}
	if end > len(p.Calls) {
		end = len(p.Calls)
	}
	states := make(map[string]pathTypeState)
	stateOf := func(path string) pathTypeState {
		if path == "" {
			return pathTypeUnknown
		}
		if st, ok := states[path]; ok {
			return st
		}
		if n := findPathNode(ft, path); n != nil {
			if n.Type == NodeTypeFile {
				return pathTypeFile
			}
			return pathTypeDir
		}
		return pathTypeUnknown
	}
	set := func(path string, st pathTypeState) {
		if path != "" && st != pathTypeUnknown {
			states[path] = st
		}
	}
	for i := 0; i < end; i++ {
		c := p.Calls[i]
		if c == nil || c.Meta == nil || !callAssumedSucceeded(c) {
			continue
		}
		switch c.Meta.CallName {
		case "mkdir":
			set(extractPathFromCall(c), pathTypeDir)
		case "creat":
			set(extractPathFromCall(c), pathTypeFile)
		case "open":
			if created, known := openCallCreatesFile(p.Target, c); known && created {
				set(extractPathFromCall(c), pathTypeFile)
			}
		case "unlink", "rmdir":
			set(extractPathFromCall(c), pathTypeGone)
		case "rename":
			src, dst := extractRenamePaths(c)
			st := stateOf(src)
			if st == pathTypeFile || st == pathTypeDir {
				set(dst, st)
			}
			if src != "" {
				states[src] = pathTypeGone
			}
		case "link":
			set(extractPathFromCallByArgIdx(c, 1), pathTypeFile)
		case "symlink":
			linkPath := extractPathFromCallByArgIdx(c, 1)
			targetPath := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(c, 0))
			st := stateOf(targetPath)
			if st == pathTypeFile || st == pathTypeDir {
				set(linkPath, st)
			}
		}
	}
	if st, ok := states[target]; ok {
		return st, true
	}
	if n := findPathNode(ft, target); n != nil {
		if n.Type == NodeTypeFile {
			return pathTypeFile, true
		}
		return pathTypeDir, true
	}
	return pathTypeUnknown, false
}

// crossProgramCallType returns the type established by a call in another
// program. Only creators/aliases are considered; unlink/rmdir do not establish
// a type.
func crossProgramCallType(p *Prog, ft *FileTree, callIdx int, call *Call, target string) (pathTypeState, bool) {
	if p == nil || call == nil || call.Meta == nil || !callAssumedSucceeded(call) {
		return pathTypeUnknown, false
	}
	switch call.Meta.CallName {
	case "mkdir":
		if extractPathFromCall(call) == target {
			return pathTypeDir, true
		}
	case "creat":
		if extractPathFromCall(call) == target {
			return pathTypeFile, true
		}
	case "open":
		if created, known := openCallCreatesFile(p.Target, call); known && created &&
			extractPathFromCall(call) == target {
			return pathTypeFile, true
		}
	case "link":
		if extractPathFromCallByArgIdx(call, 1) == target {
			return pathTypeFile, true
		}
	case "rename":
		_, dst := extractRenamePaths(call)
		if dst == target {
			src, _ := extractRenamePaths(call)
			if st, ok := pathTypePrefix(p, ft, callIdx, src); ok &&
				(st == pathTypeFile || st == pathTypeDir) {
				return st, true
			}
		}
	case "symlink":
		linkPath := extractPathFromCallByArgIdx(call, 1)
		if linkPath == target {
			targetPath := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(call, 0))
			if st, ok := pathTypePrefix(p, ft, callIdx, targetPath); ok &&
				(st == pathTypeFile || st == pathTypeDir) {
				return st, true
			}
		}
	}
	return pathTypeUnknown, false
}

func findPathNode(ft *FileTree, path string) *FileNode {
	if ft == nil || path == "" {
		return nil
	}
	return ft.FindNode(path)
}
