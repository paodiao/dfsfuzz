package prog

// DualPathKind identifies a root call with two program-visible paths. It is a
// planner-only classification: it never enters CallVariant, DCT weights,
// comboKey or cellKey.
type DualPathKind uint8

const (
	DualNone DualPathKind = iota
	DualRename
	DualLink
	DualSymlink
	DualReadlink
)

// RootPaths is the shared dual-path view of a root call. All paths are
// canonical test-root-relative coordinates. The symlink payload is materialized
// only when the symlink call is generated; it is never stored here.
//
//	rename:   PrePath=src,     PostPath=dst
//	link:     PrePath=target,  PostPath=linkpath
//	symlink:  PrePath=target,  PostPath=linkpath
//	readlink: PrePath=PostPath=linkpath
type RootPaths struct {
	Kind         DualPathKind
	PrePath      string
	PostPath     string
	TargetPath   string
	LinkPath     string
	ReadlinkPath string
}

// Source and Dest are the compatibility projection used by the existing
// RootSource/RootDest fields.
func (rp RootPaths) Source() string {
	switch rp.Kind {
	case DualLink, DualSymlink:
		return rp.TargetPath
	case DualReadlink:
		return rp.LinkPath
	default:
		return rp.PrePath
	}
}

func (rp RootPaths) Dest() string {
	switch rp.Kind {
	case DualLink, DualSymlink:
		return rp.LinkPath
	case DualReadlink:
		return rp.LinkPath
	default:
		return rp.PostPath
	}
}

// EntryPath is the construction-time/entry path used for variant selection.
// Link/symlink roots are observed on the target (the linkpath is a fresh
// program-local binding); readlink is observed on its linkpath. Rename callers
// must choose the temporal form explicitly: CC observes PrePath/Source, HB
// observes PostPath/Dest.
func (rp RootPaths) EntryPath() string {
	switch rp.Kind {
	case DualLink, DualSymlink:
		return rp.TargetPath
	case DualReadlink:
		return rp.LinkPath
	}
	return rp.PrePath
}

// ObservedPath is kept for compatibility.
//
// Deprecated: use EntryPath, or ObservedPathFor when the temporal form is
// known. A rename root's HB observation path is PostPath/Dest, which
// ObservedPath intentionally does not return.
func (rp RootPaths) ObservedPath() string {
	return rp.EntryPath()
}

// ObservedPathFor chooses the temporal observation path for DCT/derived
// insertion. A rename's concurrent form still sees the entry/source path while
// its HB form sees the completed result/destination. Non-rename roots observe
// the same path in both forms.
func (rp RootPaths) ObservedPathFor(temporal TemporalRel) string {
	if rp.Kind == DualRename {
		if temporal == TemporalHB {
			return rp.PostPath
		}
		return rp.PrePath
	}
	return rp.EntryPath()
}

func dualKindForCall(call string) DualPathKind {
	switch call {
	case "rename":
		return DualRename
	case "link":
		return DualLink
	case "symlink":
		return DualSymlink
	case "readlink":
		return DualReadlink
	}
	return DualNone
}

// rootPaths constructs the dual-path view for a root call from its canonical
// target/linkpath (or src/dst) paths.
func rootPaths(call, pre, post string) RootPaths {
	switch dualKindForCall(call) {
	case DualRename:
		return RootPaths{Kind: DualRename, PrePath: pre, PostPath: post, TargetPath: pre}
	case DualLink, DualSymlink:
		return RootPaths{Kind: dualKindForCall(call), PrePath: pre, PostPath: post,
			TargetPath: pre, LinkPath: post}
	case DualReadlink:
		return RootPaths{Kind: DualReadlink, PrePath: pre, PostPath: pre, LinkPath: pre}
	}
	return RootPaths{Kind: DualNone, PrePath: pre, PostPath: pre, TargetPath: pre}
}
