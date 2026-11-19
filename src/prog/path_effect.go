package prog

// pathEffectKind classifies how a successful destructive call changes the
// path namespace after it runs.
type pathEffectKind uint8

const (
	pathEffectNone pathEffectKind = iota
	pathEffectRemove
	pathEffectMove
)

// pathEffect is the post-state effect of one successful unlink/rmdir/rename.
// allows reports whether a path may still be used after the effect: the
// removed/moved source and its subtree may not, while the move destination is
// a normal existing path. The effect deliberately does not model inode
// identity; the DAG relation classifier owns that.
type pathEffect struct {
	kind pathEffectKind
	src  string
	dst  string
}

func (e pathEffect) active() bool {
	return e.kind != pathEffectNone && e.src != ""
}

func (e pathEffect) allows(path string) bool {
	if path == "" || !e.active() {
		return true
	}
	return !pathUnderRoot(normalizeRelativePath(path), e.src)
}

// plannedPathEffect returns the effect of a call that is assumed to succeed.
// It is used for C2 roots (the planner owns their success prediction) and for
// C1 anchors only after callSucceeded has checked the prior execution.
//
// unlink removes a non-directory entry. A path absent from the baseline tree
// is still treated as removed: the successful call may have removed a path
// created earlier in p0 (or a symlink linkpath, which the tree does not hold).
// rmdir only removes an empty directory; a non-empty baseline directory is
// left alone so a stale tree does not over-filter root candidates. rename
// moves the source and its subtree; the destination is available afterwards.
func plannedPathEffect(ft *FileTree, call, path, source, dest string) pathEffect {
	if ft == nil || call == "" {
		return pathEffect{}
	}
	path = normalizeRelativePath(path)
	source = normalizeRelativePath(source)
	dest = normalizeRelativePath(dest)
	switch call {
	case "unlink":
		if path == "" {
			return pathEffect{}
		}
		if node := ft.FindNode(path); node != nil && node.Type != NodeTypeFile {
			return pathEffect{}
		}
		return pathEffect{kind: pathEffectRemove, src: path}
	case "rmdir":
		if path == "" {
			return pathEffect{}
		}
		node := ft.FindNode(path)
		if node != nil {
			if node == ft.Root || node.Type == NodeTypeFile ||
				node.Type == NodeTypeTmpDir || len(node.Children) > 0 {
				return pathEffect{}
			}
		}
		// A successful rmdir of a path absent from the baseline tree removed a
		// directory created earlier in p0; a failed call never reaches here.
		return pathEffect{kind: pathEffectRemove, src: path}
	case "rename":
		if source == "" || dest == "" || source == dest {
			return pathEffect{}
		}
		return pathEffect{kind: pathEffectMove, src: source, dst: dest}
	}
	return pathEffect{}
}

// anchorPathEffect returns the effect of an already executed C1 anchor. A
// failed or unchecked call has no effect. The anchor itself is admitted by
// callReturnedSuccess; destructive effects still require the strict zero
// return because unlink/rmdir/rename never return a positive value.
func anchorPathEffect(ft *FileTree, call *Call, a causalAnchor) pathEffect {
	if call == nil || call.Meta == nil || !callSucceeded(call) {
		return pathEffect{}
	}
	return plannedPathEffect(ft, call.Meta.CallName, a.Path, a.Source, a.Dest)
}

// causalRootPathEffect returns the effect of a planned C2 root. A rename root
// moves RootSource to RootDest; every other destructive root acts on RootPath.
func causalRootPathEffect(ft *FileTree, rootKind causalRootKind, rootCall, rootPath, rootSource, rootDest string) pathEffect {
	if rootKind == causalRootRename {
		return plannedPathEffect(ft, "rename", rootPath, rootSource, rootDest)
	}
	return plannedPathEffect(ft, rootCall, rootPath, rootPath, rootPath)
}

// c2VariantEffectPath is the B-side path whose availability the variant needs.
// A rename variant needs its source (the destination may be fresh); a fresh-B
// variant needs its parent; FreshSame and ordinary variants use BaseB.
func c2VariantEffectPath(cell directedCell, baseB, baseB2 string, freshB, freshSame bool) string {
	if cell.Combo.Variant == "rename" || freshB {
		return baseB2
	}
	return baseB
}

// c2VariantAllowedAfterEffect reports whether a variant may be placed after a
// destructive effect. creat can recreate the exact source path of a removed or
// moved entry, so its Success form stays observable; mkdir is not exempt
// because the existing C2 planner predicts EEXIST for an existing path and the
// post-effect Success would flip the cell. FD-based variants are not exempt:
// directed variants always reopen their path in another proc.
func c2VariantAllowedAfterEffect(e pathEffect, cell directedCell, baseB, baseB2 string, freshB, freshSame bool) bool {
	if !e.active() {
		return true
	}
	path := normalizeRelativePath(c2VariantEffectPath(cell, baseB, baseB2, freshB, freshSame))
	if e.allows(path) {
		return true
	}
	if freshB {
		// baseB2 is the fresh variant's parent, not a creat target: a creat
		// under a removed parent cannot recreate the parent.
		return false
	}
	return cell.Combo.Variant == "creat" && cell.RetB == RetSuccess && path == e.src
}

// c2PlanAllowedAfterEffect applies the C1 anchor effect to a fully planned C2
// variant. Unlike the root effect, the anchor precedes both the concurrent and
// the HB form, so both temporal forms are filtered.
func c2PlanAllowedAfterEffect(p *directedPlan, e pathEffect) bool {
	if p == nil {
		return false
	}
	return c2VariantAllowedAfterEffect(e, p.Cell, p.BaseB, p.BaseB2, p.FreshB, p.FreshSame)
}
