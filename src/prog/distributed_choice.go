package prog

import (
	"fmt"
	"math/rand"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	MergeViewPrefix = "merge_view/"
)

var (
	DirOnlyCalls = []string{
		"mkdir",
		"rmdir",
		"getdents64",
	}

	FileOnlyCalls = []string{
		"creat",
		"truncate",
		"pread64",
		"pwrite64",
		"read",
		"write",
		"fsync",
		"fdatasync",
		"unlink",
		"link",
		"symlink",
		"readlink",
	}

	FileOrDirCalls = []string{
		"open",
		"stat",
		"chmod",
		"rename",
		"getxattr",
		"setxattr",
	}

	FdRequiredCalls = []string{
		"read",
		"write",
		"pread64",
		"pwrite64",
		"fsync",
		"fdatasync",
		"getdents64",
		"close",
		"fstat",
		"fchmod",
	}
)

// FsopsSubCalls is the merged seed-type call allowlist: every call the fsops
// generator/mutator can build via generateCallByName (root or variant).
var FsopsSubCalls = []string{
	"open", "read", "write", "pread64", "pwrite64", "fsync", "fdatasync", "truncate",
	"mkdir", "rmdir", "creat", "unlink", "rename", "chmod", "stat", "getdents64",
}

// FsopsChoiceCalls is the allowlist of the DCT/pattern root-choice table. It
// additionally enables close so that a bias sampled from a close call
// (ubiquitous in fd self-contained sequences) is a legal bias source for
// ChoiceTable.choose; chooseRootCallName never returns close as a root.
// Mirrors the pre-merge sub-call tables, which both contained close.
var FsopsChoiceCalls = []string{
	"open", "read", "write", "pread64", "pwrite64", "fsync", "fdatasync", "truncate",
	"mkdir", "rmdir", "creat", "unlink", "rename", "chmod", "stat", "getdents64",
	"close",
}

func IsDirOnlyCall(callName string) bool {
	for _, name := range DirOnlyCalls {
		if strings.Contains(callName, name) {
			return true
		}
	}
	return false
}

func IsFileOnlyCall(callName string) bool {
	for _, name := range FileOnlyCalls {
		if strings.Contains(callName, name) {
			return true
		}
	}
	return false
}

func IsFileOrDirCall(callName string) bool {
	for _, name := range FileOrDirCalls {
		if strings.Contains(callName, name) {
			return true
		}
	}
	return false
}

func IsFdRequiredCall(callName string) bool {
	for _, name := range FdRequiredCalls {
		if strings.Contains(callName, name) {
			return true
		}
	}
	return false
}

type PathRelation int

const (
	PathSelf PathRelation = iota
	PathSelfTwo
	PathSame
	PathParent
	PathChild
	PathSibling
	PathNoRel
)

func (pr PathRelation) String() string {
	switch pr {
	case PathSelf:
		return "self"
	case PathSelfTwo:
		return "selftwo"
	case PathSame:
		return "same"
	case PathParent:
		return "parent"
	case PathChild:
		return "child"
	case PathSibling:
		return "sibling"
	case PathNoRel:
		return "norel"
	default:
		return "unknown"
	}
}

type OffsetRelationType int

const (
	OffsetUnspecified OffsetRelationType = iota
	OffsetSame
	OffsetDifferent
)

type CallVariant struct {
	CallName     string
	PathRelation PathRelation
}

func (cv CallVariant) String() string {
	return fmt.Sprintf("%s(%s)", cv.CallName, cv.PathRelation)
}

type FileNodeType int

const (
	NodeTypeFile FileNodeType = iota
	NodeTypeDir
	NodeTypeEmptyDir
	NodeTypeTmpDir
)

type FileNode struct {
	Name       string
	FullPath   string
	Type       FileNodeType
	OwnerCid   string
	Size       uint64
	PathLength int
	Children   map[string]*FileNode
	Parent     *FileNode
}

type FileTree struct {
	Root       *FileNode
	Prefix     string
	NodesByCid map[string][]*FileNode
	FilesByCid map[string][]*FileNode
	DirsByCid  map[string][]*FileNode

	// Depth-bucket indexes: bucket 1 (merge_view/x) / 2 (depths 2-4) /
	// 3 (depth >= 5) per cid. Rebuilt lazily after any tree mutation; used
	// by the depth-weighted Select* APIs.
	filesByBucket map[string][4][]*FileNode
	dirsByBucket  map[string][4][]*FileNode
	bucketDirty   bool

	mu sync.RWMutex
}

func NewFileTree() *FileTree {
	return NewFileTreeWithPrefix(MergeViewPrefix)
}

// NewFileTreeWithPrefix builds a tree whose namespace root is prefix
// ("merge_view/" for HMDFS, "" for GlusterFS). The empty-prefix root has
// an empty FullPath so children are "file" and "a/b", not "/file".
func NewFileTreeWithPrefix(prefix string) *FileTree {
	rootName, rootPath := "", ""
	if prefix != "" {
		rootPath = strings.TrimSuffix(prefix, "/")
		rootName = rootPath
	}
	return &FileTree{
		Root: &FileNode{
			Name:     rootName,
			FullPath: rootPath,
			Type:     NodeTypeDir,
			Children: make(map[string]*FileNode),
		},
		Prefix:      prefix,
		NodesByCid:  make(map[string][]*FileNode),
		FilesByCid:  make(map[string][]*FileNode),
		DirsByCid:   make(map[string][]*FileNode),
		bucketDirty: true,
	}
}

// RootPath returns the logical namespace root without a trailing slash.
func (ft *FileTree) RootPath() string {
	if ft == nil {
		return ""
	}
	return strings.TrimSuffix(ft.Prefix, "/")
}

// JoinRoot joins a relative path to the tree's logical root. It never emits
// a leading slash or "./" and returns the relative path unchanged when the
// root is empty.
func (ft *FileTree) JoinRoot(rel string) string {
	rel = normalizeRelativePath(rel)
	if ft == nil || ft.Prefix == "" || rel == "" {
		return rel
	}
	root := strings.TrimSuffix(ft.Prefix, "/")
	return root + "/" + rel
}

func (ft *FileTree) DepthBucketOf(path string) uint64 {
	prefix := ""
	if ft != nil {
		prefix = ft.Prefix
	}
	return DepthBucketOfWithPrefix(path, prefix)
}

// defaultTestDir returns the root-relative directory fallback used by legacy
// generators when the tree has no candidate. GlusterFS uses an empty prefix
// (the executor's cwd is the test root), so it returns "".
func defaultTestDir(c *Hmdfs_config) string {
	if c != nil && c.FileTree != nil {
		return c.FileTree.RootPath()
	}
	return strings.TrimSuffix(MergeViewPrefix, "/")
}

// joinTestPath joins a root-relative directory and a leaf without producing a
// leading slash for the empty GlusterFS root.
func joinTestPath(dir, name string) string {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// parentDirOrRoot returns the test-root-relative parent of path. Top-level
// names such as "q0" have no parent; filepath.Dir would report ".".
func parentDirOrRoot(path string) string {
	parent := filepath.Dir(path)
	if parent == "." || parent == "/" || parent == "\\" {
		return ""
	}
	return filepath.ToSlash(parent)
}

// symlinkCanonicalTarget resolves a symlink payload relative to the linkpath
// parent into a test-root-relative path. Only the test root may be addressed
// absolutely; escaping paths return "". Symlink payload itself is never used
// as a FileTree node.
func symlinkCanonicalTarget(linkPath, payload string) string {
	p := strings.TrimSpace(payload)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "/") {
		// Absolute payloads outside the test root are host paths and must not
		// become FileTree coordinates.
		if p != "/eifuzz_root" && !strings.HasPrefix(p, "/eifuzz_root/") {
			return ""
		}
		p = strings.TrimPrefix(p, "/")
		if p == "eifuzz_root" {
			return ""
		}
		p = strings.TrimPrefix(p, "eifuzz_root/")
		p = path.Clean(p)
	} else {
		p = path.Clean(path.Join(parentDirOrRoot(linkPath), p))
	}
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

// SymlinkCanonicalTarget exposes the symlink payload resolver to the checker's
// initTreeSubset. The linkpath itself remains a program-local binding; only a
// canonical target that resolves inside the test root is useful as a FileTree
// coordinate.
func SymlinkCanonicalTarget(linkPath, payload string) string {
	return symlinkCanonicalTarget(linkPath, payload)
}

// symlinkPayload materializes the program-visible symlink target for a
// canonical test-root path. The payload is relative to the linkpath parent, so
// the kernel resolves it back to canonicalTarget without escaping the root.
func symlinkPayload(linkPath, canonicalTarget string) string {
	target := strings.TrimPrefix(strings.TrimSpace(canonicalTarget), "/")
	if target == "" {
		return ""
	}
	parent := parentDirOrRoot(linkPath)
	rel, err := filepath.Rel(filepath.FromSlash("/"+parent), filepath.FromSlash("/"+target))
	if err != nil || rel == "" {
		return ""
	}
	return filepath.ToSlash(rel)
}

func normalizeRelativePath(path string) string {
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "/")
	return path
}

func (ft *FileTree) normalizePath(path string) string {
	if ft == nil {
		return normalizeRelativePath(path)
	}
	if ft.Prefix != "" {
		path = strings.TrimPrefix(path, ft.Prefix)
		path = strings.TrimPrefix(path, strings.TrimSuffix(ft.Prefix, "/")+"/")
	}
	return normalizeRelativePath(path)
}

func (ft *FileTree) fullPath(parent *FileNode, name string) string {
	if parent == nil || parent.FullPath == "" {
		return name
	}
	return parent.FullPath + "/" + name
}

func (ft *FileTree) InitFromHmdfsConfig(hmcfg *Hmdfs_config) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	for cid, files := range hmcfg.Init_file {
		for _, f := range files {
			node := ft.addNodeLocked(f, NodeTypeFile, cid)
			if node != nil && hmcfg.FileSize != nil {
				if sizes, ok := hmcfg.FileSize[cid]; ok {
					if sz, ok := sizes[f]; ok {
						node.Size = sz
					}
				}
			}
		}
	}

	for cid, dirs := range hmcfg.Init_dir {
		for _, d := range dirs {
			ft.addNodeLocked(d, NodeTypeDir, cid)
		}
	}

	for cid, dirs := range hmcfg.Init_tmpdir {
		for _, d := range dirs {
			ft.addNodeLocked(d, NodeTypeTmpDir, cid)
		}
	}

	for cid, dirs := range hmcfg.Init_empty_dir {
		for _, d := range dirs {
			ft.addNodeLocked(d, NodeTypeEmptyDir, cid)
		}
	}

	for cid, files := range hmcfg.File_in_persistence_dir {
		for _, f := range files {
			node := ft.addNodeLocked(f, NodeTypeFile, cid)
			if node != nil && hmcfg.FileSize != nil {
				if sizes, ok := hmcfg.FileSize[cid]; ok {
					if sz, ok := sizes[f]; ok {
						node.Size = sz
					}
				}
			}
		}
	}
}

func (ft *FileTree) addNodeLocked(path string, nodeType FileNodeType, ownerCid string) *FileNode {
	ft.bucketDirty = true
	cleanPath := ft.normalizePath(path)
	if cleanPath == "" {
		return nil
	}

	parts := strings.Split(cleanPath, "/")
	current := ft.Root

	for i, part := range parts {
		if part == "" {
			continue
		}

		isLast := i == len(parts)-1

		if child, exists := current.Children[part]; exists {
			current = child
			if isLast {
				if current.Type != nodeType || current.OwnerCid != ownerCid {
					ft.removeNodeFromCidLists(current)
				}
				current.Type = nodeType
				current.OwnerCid = ownerCid
			}
		} else {
			newNode := &FileNode{
				Name:     part,
				FullPath: ft.fullPath(current, part),
				Parent:   current,
				Children: make(map[string]*FileNode),
			}

			if isLast {
				newNode.Type = nodeType
				newNode.OwnerCid = ownerCid
			} else {
				newNode.Type = NodeTypeTmpDir
				// TODO: add Tmp Dir Cid?
			}

			current.Children[part] = newNode
			current = newNode
		}
	}

	if !containsNode(ft.NodesByCid[ownerCid], current) {
		ft.NodesByCid[ownerCid] = append(ft.NodesByCid[ownerCid], current)
	}
	current.PathLength = len(current.FullPath)
	if nodeType == NodeTypeFile {
		if !containsNode(ft.FilesByCid[ownerCid], current) {
			ft.FilesByCid[ownerCid] = append(ft.FilesByCid[ownerCid], current)
		}
	} else {
		if !containsNode(ft.DirsByCid[ownerCid], current) {
			ft.DirsByCid[ownerCid] = append(ft.DirsByCid[ownerCid], current)
		}
	}

	return current
}

func (ft *FileTree) AddNode(path string, nodeType FileNodeType, ownerCid string) *FileNode {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.addNodeLocked(path, nodeType, ownerCid)
}

func (ft *FileTree) UpdateNodeSize(path string, size uint64) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	node := ft.findNodeLocked(path)
	if node != nil && node.Type == NodeTypeFile {
		node.Size = size
	}
}

func (ft *FileTree) RemoveNode(path string) bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.removeNodeLocked(path)
}

func (ft *FileTree) removeNodeLocked(path string) bool {
	ft.bucketDirty = true
	// Caller must hold ft.mu.
	cleanPath := ft.normalizePath(path)
	if cleanPath == "" {
		return false
	}

	parts := strings.Split(cleanPath, "/")
	current := ft.Root

	for _, part := range parts {
		if part == "" {
			continue
		}
		if child, exists := current.Children[part]; exists {
			current = child
		} else {
			return false
		}
	}

	// A vanished directory takes its whole subtree with it: sweep the
	// cid/type lists for the node and every descendant before detaching.
	// Otherwise a child whose walk would later fail (its parent already
	// removed) stays in the lists forever, and list-based selectors keep
	// picking nonexistent paths.
	ft.purgeSubtreeLocked(current)
	return true
}

// purgeSubtreeLocked sweeps node and all descendants out of the cid/type
// lists and detaches node from its parent. Caller must hold ft.mu.
func (ft *FileTree) purgeSubtreeLocked(node *FileNode) {
	var sweep func(*FileNode)
	sweep = func(n *FileNode) {
		ft.removeNodeFromCidLists(n)
		for _, child := range n.Children {
			sweep(child)
		}
	}
	sweep(node)
	if node.Parent != nil {
		delete(node.Parent.Children, node.Name)
	}
}

// removeNodesFromList removes every identity match of node from list and
// returns the compacted slice (a node can be listed more than once after a
// type flip without re-linking).
func removeNodesFromList(list []*FileNode, node *FileNode) []*FileNode {
	out := list[:0]
	for _, n := range list {
		if n != node {
			out = append(out, n)
		}
	}
	return out
}

func (ft *FileTree) removeNodeFromCidLists(node *FileNode) {
	ft.bucketDirty = true
	// The node may appear in several cid lists (the same path registered
	// under multiple cids) — sweep all of them, not just the current OwnerCid.
	for cid, list := range ft.NodesByCid {
		ft.NodesByCid[cid] = removeNodesFromList(list, node)
	}
	// A type flip without re-linking can leave the node in both type lists;
	// purge both regardless of the current Type (identity is exact).
	for cid, list := range ft.FilesByCid {
		ft.FilesByCid[cid] = removeNodesFromList(list, node)
	}
	for cid, list := range ft.DirsByCid {
		ft.DirsByCid[cid] = removeNodesFromList(list, node)
	}
}

func containsNode(list []*FileNode, node *FileNode) bool {
	for _, n := range list {
		if n == node {
			return true
		}
	}
	return false
}

// removeNodeFromTypeList removes node from FilesByCid/DirsByCid only (not
// NodesByCid — the node stays in the tree on a type change). Caller must
// hold ft.mu.
func (ft *FileTree) removeNodeFromTypeList(node *FileNode) {
	ft.bucketDirty = true
	cid := node.OwnerCid
	if node.Type == NodeTypeFile {
		if list, ok := ft.FilesByCid[cid]; ok {
			for i, n := range list {
				if n == node {
					ft.FilesByCid[cid] = append(list[:i], list[i+1:]...)
					break
				}
			}
		}
	} else {
		if list, ok := ft.DirsByCid[cid]; ok {
			for i, n := range list {
				if n == node {
					ft.DirsByCid[cid] = append(list[:i], list[i+1:]...)
					break
				}
			}
		}
	}
}

// RenameNode moves oldPath to newPath under strict rename(2) semantics: every
// precondition is resolved before any mutation (a failed rename leaves the
// tree untouched), a directory cannot move into its own subtree, and an
// existing destination is overwritten only when the classes match and a
// directory target is empty (ENOTDIR/EISDIR/ENOTEMPTY otherwise). The target
// subtree is purged from the cid/type lists before the source is relinked, so
// no ghost node survives an overwrite.
func (ft *FileTree) RenameNode(oldPath, newPath string) bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	cleanOldPath := ft.normalizePath(oldPath)
	cleanNewPath := ft.normalizePath(newPath)
	if cleanOldPath == "" || cleanNewPath == "" {
		return false
	}

	// Resolve the source and the destination parent before touching anything.
	current := ft.findNodeLocked(cleanOldPath)
	if current == nil || current == ft.Root {
		return false
	}
	newParts := strings.Split(cleanNewPath, "/")
	newName := newParts[len(newParts)-1]
	if newName == "" {
		return false // trailing slash: no terminal name
	}
	newParent := ft.Root
	for i := 0; i < len(newParts)-1; i++ {
		part := newParts[i]
		if part == "" {
			continue
		}
		child, exists := newParent.Children[part]
		if !exists {
			return false
		}
		newParent = child
	}
	if !nodeIsDir(newParent) {
		return false // ENOTDIR
	}
	// A directory must not move into its own subtree.
	for p := newParent; p != nil; p = p.Parent {
		if p == current {
			return false
		}
	}
	if existing, ok := newParent.Children[newName]; ok {
		if existing == current {
			return true // rename onto its own path: no-op success
		}
		if nodeIsDir(current) != nodeIsDir(existing) {
			return false // EISDIR / ENOTDIR
		}
		if nodeIsDir(existing) && len(existing.Children) != 0 {
			return false // ENOTEMPTY
		}
		ft.purgeSubtreeLocked(existing)
	}

	// Commit: unlink the source and relink it under the destination parent.
	if current.Parent != nil {
		delete(current.Parent.Children, current.Name)
	}
	current.Name = newName
	current.FullPath = ft.fullPath(newParent, newName)
	current.PathLength = len(current.FullPath)
	current.Parent = newParent
	newParent.Children[newName] = current

	ft.bucketDirty = true
	ft.updateChildPaths(current)
	return true
}

func (ft *FileTree) updateChildPaths(node *FileNode) {
	for _, child := range node.Children {
		child.FullPath = ft.fullPath(node, child.Name)
		child.PathLength = len(child.FullPath)
		ft.updateChildPaths(child)
	}
}

func (ft *FileTree) FindNode(path string) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()
	return ft.findNodeLocked(path)
}

func (ft *FileTree) findNodeLocked(path string) *FileNode {
	// Caller must hold ft.mu (read or write).
	cleanPath := ft.normalizePath(path)
	if cleanPath == "" {
		return ft.Root
	}

	parts := strings.Split(cleanPath, "/")
	current := ft.Root

	for _, part := range parts {
		if part == "" {
			continue
		}
		if child, exists := current.Children[part]; exists {
			current = child
		} else {
			return nil
		}
	}

	return current
}

// parentNodeLocked resolves the parent directory of path. A depth-1 path is
// a child of Root for both HMDFS ("merge_view/file") and GlusterFS ("file").
// Caller must hold ft.mu.
func (ft *FileTree) parentNodeLocked(path string) *FileNode {
	cleanPath := ft.normalizePath(path)
	if cleanPath == "" || (ft.RootPath() != "" && cleanPath == ft.RootPath()) {
		return nil
	}
	if i := strings.LastIndexByte(cleanPath, '/'); i >= 0 {
		parent := cleanPath[:i]
		if parent == "" {
			return ft.Root
		}
		return ft.findNodeLocked(parent)
	}
	return ft.Root
}

// FindParent returns the parent directory node of path (the merge-view root
// included), or nil when the parent does not resolve in the tree.
func (ft *FileTree) FindParent(path string) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()
	return ft.parentNodeLocked(path)
}

func (ft *FileTree) GetParent(node *FileNode) *FileNode {
	if node == nil || node.Parent == nil {
		return nil
	}
	return node.Parent
}

func (ft *FileTree) GetChildren(node *FileNode) []*FileNode {
	if node == nil {
		return nil
	}
	children := make([]*FileNode, 0, len(node.Children))
	for _, child := range node.Children {
		children = append(children, child)
	}
	return children
}

func (ft *FileTree) GetEntriesUnderDir(dirPath string) []string {
	ft.mu.RLock()
	defer ft.mu.RUnlock()
	node := ft.findNodeLocked(dirPath)
	if node == nil {
		return nil
	}
	entries := make([]string, 0, len(node.Children))
	for _, child := range node.Children {
		entries = append(entries, child.FullPath)
	}
	return entries
}

func (ft *FileTree) GetFileEntriesUnderDir(dirPath string) []string {
	ft.mu.RLock()
	defer ft.mu.RUnlock()
	node := ft.findNodeLocked(dirPath)
	if node == nil {
		return nil
	}
	var entries []string
	for _, child := range node.Children {
		if child.Type == NodeTypeFile {
			entries = append(entries, child.FullPath)
		}
	}
	return entries
}

func (ft *FileTree) GetNonTmpDirEntriesUnderDir(dirPath string) []string {
	ft.mu.RLock()
	defer ft.mu.RUnlock()
	node := ft.findNodeLocked(dirPath)
	if node == nil {
		return nil
	}
	var entries []string
	for _, child := range node.Children {
		if child.Type == NodeTypeDir || child.Type == NodeTypeEmptyDir {
			entries = append(entries, child.FullPath)
		}
	}
	return entries
}

// GetNonTmpDirSiblings returns the non-TmpDir children of srcPath's parent,
// excluding srcPath itself (the candidate pool of an existing rename
// destination). The order follows map iteration; callers that need
// determinism must sort.
func (ft *FileTree) GetNonTmpDirSiblings(srcPath string) []string {
	ft.mu.RLock()
	defer ft.mu.RUnlock()
	node := ft.findNodeLocked(srcPath)
	if node == nil || node.Parent == nil {
		return nil
	}
	var entries []string
	for _, child := range node.Parent.Children {
		if child == nil || child == node || child.Type == NodeTypeTmpDir {
			continue
		}
		entries = append(entries, child.FullPath)
	}
	return entries
}

func (ft *FileTree) GetSibling(node *FileNode) []*FileNode {
	if node == nil || node.Parent == nil {
		return nil
	}
	siblings := make([]*FileNode, 0)
	for _, child := range node.Parent.Children {
		if child != node {
			siblings = append(siblings, child)
		}
	}
	return siblings
}

// DepthBucketWeights is the path-depth bucket distribution for path
// selection: bucket 1 (merge_view/x) 15%, bucket 2 (depths 2-4) 45%,
// bucket 3 (depth >= 5) 40%. Bucket 0 is unreachable for tree paths.
var DepthBucketWeights = [3]float64{0.15, 0.45, 0.40}

// ensureBucketIndexLocked rebuilds the depth-bucket indexes when the tree
// changed. O(nodes) ~1700; runs at most once per mutation round. The caller
// must hold ft.mu for writing: the selector holds the same lock across the
// rebuild and the pick, so a pick can never observe a stale index (no
// two-phase ensure/read window).
func (ft *FileTree) ensureBucketIndexLocked() {
	if !ft.bucketDirty && ft.filesByBucket != nil {
		return
	}
	ft.filesByBucket = make(map[string][4][]*FileNode)
	ft.dirsByBucket = make(map[string][4][]*FileNode)
	var walk func(n *FileNode)
	walk = func(n *FileNode) {
		if n != ft.Root {
			b := int(DepthBucketOf(n.FullPath))
			if b < 0 {
				b = 0
			}
			if b > 3 {
				b = 3
			}
			m := ft.dirsByBucket
			if n.Type == NodeTypeFile {
				m = ft.filesByBucket
			}
			lst := m[n.OwnerCid]
			lst[b] = append(lst[b], n)
			m[n.OwnerCid] = lst
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(ft.Root)
	ft.bucketDirty = false
}

// dirBucketFilter is the directory filter of the selectors: TmpDir is always
// excluded; EmptyDir is excluded when allowEmpty is false.
func dirBucketFilter(allowEmpty bool) func(*FileNode) bool {
	return func(n *FileNode) bool {
		if n.Type == NodeTypeTmpDir {
			return false
		}
		if !allowEmpty && n.Type == NodeTypeEmptyDir {
			return false
		}
		return true
	}
}

// pickByBucket picks a depth bucket by weights over the non-empty (and
// non-zero-weight) candidate buckets, then a uniform node inside it. When no
// bucket is weighted, the non-empty buckets are used uniformly.
func pickByBucket(r *rand.Rand, buckets [4][]*FileNode, weights [3]float64, filter func(*FileNode) bool) *FileNode {
	var cands [3][]*FileNode
	for i := 1; i <= 3; i++ {
		for _, n := range buckets[i] {
			if filter == nil || filter(n) {
				cands[i-1] = append(cands[i-1], n)
			}
		}
	}
	var ws [3]float64
	total := 0.0
	for i := 0; i < 3; i++ {
		if len(cands[i]) > 0 && weights[i] > 0 {
			ws[i] = weights[i]
			total += ws[i]
		}
	}
	if total <= 0 {
		for i := 0; i < 3; i++ {
			if len(cands[i]) > 0 {
				ws[i] = 1
				total += 1
			}
		}
	}
	if total <= 0 {
		return nil
	}
	x := r.Float64() * total
	cum := 0.0
	for i := 0; i < 3; i++ {
		cum += ws[i]
		if ws[i] > 0 && x < cum {
			return cands[i][r.Intn(len(cands[i]))]
		}
	}
	for i := 2; i >= 0; i-- {
		if len(cands[i]) > 0 {
			return cands[i][r.Intn(len(cands[i]))]
		}
	}
	return nil
}

// SelectFile picks a file of cid with the depth-bucket weight distribution.
// The rebuild-if-dirty and the pick happen under one write lock so the index
// cannot go stale between them.
func (ft *FileTree) SelectFile(r *rand.Rand, cid string, weights [3]float64) *FileNode {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.ensureBucketIndexLocked()
	return pickByBucket(r, ft.filesByBucket[cid], weights, nil)
}

// SelectDir picks a directory of cid with the depth-bucket weight
// distribution; allowEmpty=false excludes empty dirs (TmpDir always excluded).
func (ft *FileTree) SelectDir(r *rand.Rand, cid string, allowEmpty bool, weights [3]float64) *FileNode {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.ensureBucketIndexLocked()
	return pickByBucket(r, ft.dirsByBucket[cid], weights, dirBucketFilter(allowEmpty))
}

func (ft *FileTree) GetRandomFile(r *rand.Rand, cid string) *FileNode {
	return ft.SelectFile(r, cid, DepthBucketWeights)
}

func (ft *FileTree) GetRandomFileExcluding(r *rand.Rand, excludeCid string) *FileNode {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.ensureBucketIndexLocked()

	var buckets [4][]*FileNode
	for cid, byBucket := range ft.filesByBucket {
		if cid == excludeCid {
			continue
		}
		for b := 1; b <= 3; b++ {
			buckets[b] = append(buckets[b], byBucket[b]...)
		}
	}
	return pickByBucket(r, buckets, DepthBucketWeights, nil)
}

func (ft *FileTree) GetRandomDir(r *rand.Rand, cid string, allowEmpty bool) *FileNode {
	return ft.SelectDir(r, cid, allowEmpty, DepthBucketWeights)
}

func (ft *FileTree) GetRandomDirExcluding(r *rand.Rand, excludeCid string) *FileNode {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.ensureBucketIndexLocked()

	var buckets [4][]*FileNode
	for cid, byBucket := range ft.dirsByBucket {
		if cid == excludeCid {
			continue
		}
		for b := 1; b <= 3; b++ {
			buckets[b] = append(buckets[b], byBucket[b]...)
		}
	}
	return pickByBucket(r, buckets, DepthBucketWeights, dirBucketFilter(false))
}

func (ft *FileTree) GetAllFileNodesExcluding(excludeCid string) []*FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	var nodes []*FileNode
	for cid, files := range ft.FilesByCid {
		if cid == excludeCid {
			continue
		}
		nodes = append(nodes, files...)
	}
	return nodes
}

func (ft *FileTree) GetAllNonTmpDirNodesExcluding(excludeCid string) []*FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	var nodes []*FileNode
	for cid, dirs := range ft.DirsByCid {
		if cid == excludeCid {
			continue
		}
		for _, d := range dirs {
			if d.Type == NodeTypeDir || d.Type == NodeTypeEmptyDir {
				nodes = append(nodes, d)
			}
		}
	}
	return nodes
}

func (ft *FileTree) GetRandomEmptyDir(r *rand.Rand, cid string) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	dirs, ok := ft.DirsByCid[cid]
	if !ok {
		return nil
	}

	emptyDirs := make([]*FileNode, 0)
	for _, d := range dirs {
		if d.Type == NodeTypeEmptyDir {
			emptyDirs = append(emptyDirs, d)
		}
	}

	if len(emptyDirs) == 0 {
		return nil
	}
	return emptyDirs[r.Intn(len(emptyDirs))]
}

func (ft *FileTree) GetRandomNode(r *rand.Rand, cid string) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	nodes, ok := ft.NodesByCid[cid]
	if !ok || len(nodes) == 0 {
		return nil
	}
	return nodes[r.Intn(len(nodes))]
}

// Target-type constraint for variant path generation: file-only calls must
// land on file paths, dir-only calls on directory paths, generic calls accept
// either.
const (
	relTargetAny = iota
	relTargetFile
	relTargetDir
)

// callNameConstraint maps a call to the node type its variant path must have.
func callNameConstraint(callName string) int {
	if isFileOnlyCallByName(callName) {
		return relTargetFile
	}
	if isDirOnlyCall(callName) {
		return relTargetDir
	}
	return relTargetAny
}

func nodeMatchesConstraint(node *FileNode, constraint int) bool {
	switch constraint {
	case relTargetFile:
		return node.Type == NodeTypeFile
	case relTargetDir:
		return node.Type != NodeTypeFile
	}
	return true
}

// nodeTypeConstraint is the tree-node counterpart of callNameConstraint: it
// maps a node to the type constraint its class satisfies (file -> File; the
// directory families, TmpDir included, -> Dir), so relation availability and
// relation resolution share one class rule. nil defaults to Dir.
func nodeTypeConstraint(node *FileNode) int {
	if node != nil && node.Type == NodeTypeFile {
		return relTargetFile
	}
	return relTargetDir
}

// nodeIsDir is the physical directory predicate of the flags/classification
// layer: any node that is not a file (Dir/EmptyDir/TmpDir) is a directory.
// It mirrors nodeMatchesConstraint(relTargetDir)/nodeTypeConstraint/
// countNodesByType so relation availability and open-flag computation share
// one rule (a TmpDir is a real directory; omitting it makes open() fail
// EISDIR at the VFS before the hmdfs trace point).
func nodeIsDir(n *FileNode) bool {
	return n != nil && n.Type != NodeTypeFile
}

// patternBaseMatches reports whether an extracted base path suits the seed
// type of a pattern insertion (file-kind patterns operate on files, dir-kind
// ones on directories). Paths outside the tree are excluded: an unbuilt
// target (e.g. a not-yet-created mkdir directory) would make creation ops
// hit the directory path itself and fail.
func patternBaseMatches(ft *FileTree, baseKind, basePath string) bool {
	node := ft.FindNode(basePath)
	if node == nil {
		return false
	}
	if baseKind == "file" {
		return node.Type == NodeTypeFile
	}
	return node.Type != NodeTypeFile
}

func filterNodesByConstraint(nodes []*FileNode, constraint int) []*FileNode {
	if constraint == relTargetAny {
		return nodes
	}
	filtered := make([]*FileNode, 0, len(nodes))
	for _, n := range nodes {
		if nodeMatchesConstraint(n, constraint) {
			filtered = append(filtered, n)
		}
	}
	return filtered
}

func (ft *FileTree) GetPathByRelation(basePath string, basePath2 string, relation PathRelation, r *rand.Rand, isTwoPath bool, constraint int) string {
	var baseNode *FileNode = nil
	//传入两条路径时，我们默认这两个路径权重相同，随机选取一个来基于relation获取路径，当然这只是基于从rename调用的两个路径来基于relation获取路径考虑的，还没考虑link和symlink
	if isTwoPath {
		if r.Intn(2) == 0 {
			baseNode = ft.FindNode(basePath)
		} else {
			baseNode = ft.FindNode(basePath2)
		}
	} else {
		baseNode = ft.FindNode(basePath)
	}

	if baseNode == nil {
		// basePath is outside the tree (e.g. a fresh mkdir/creat target):
		// Same always works (the path itself); the other relations anchor on
		// the parent directory when it is in the tree. A fresh base has no
		// existing children, so Child can never be produced here.
		switch relation {
		case PathSelf, PathSelfTwo, PathSame:
			return basePath
		}
		parent := ft.FindParent(basePath)
		if parent == nil {
			return ""
		}
		switch relation {
		case PathParent:
			if constraint == relTargetFile || parent == ft.Root {
				// A parent is always a directory; the merge-view root is not a
				// legal rename/rmdir source (VFS EBUSY on the mount point).
				return ""
			}
			return parent.FullPath
		case PathSibling:
			children := filterNodesByConstraint(ft.GetChildren(parent), constraint)
			if len(children) == 0 {
				return ""
			}
			return children[r.Intn(len(children))].FullPath
		case PathNoRel:
			return ft.getRandomUnrelatedPath(basePath, r, constraint)
		default:
			return ""
		}
	}

	switch relation {
	case PathSelf, PathSelfTwo, PathSame:
		return basePath

	case PathParent:
		if constraint == relTargetFile {
			return "" // a parent is always a directory
		}
		parent := ft.GetParent(baseNode)
		if parent == nil || parent == ft.Root {
			// The merge-view root is not a legal rename/rmdir source (EBUSY on
			// the mount point), so a depth-1 base has no Parent relation.
			return ""
		}
		return parent.FullPath
	case PathChild:
		children := filterNodesByConstraint(ft.GetChildren(baseNode), constraint)
		if len(children) == 0 {
			return ""
		}
		return children[r.Intn(len(children))].FullPath
	case PathSibling:
		siblings := filterNodesByConstraint(ft.GetSibling(baseNode), constraint)
		if len(siblings) == 0 {
			return ""
		}
		return siblings[r.Intn(len(siblings))].FullPath
	case PathNoRel:
		return ft.getRandomUnrelatedPath(basePath, r, constraint)
	default:
		return ""
	}
}

func (ft *FileTree) getRandomUnrelatedPath(excludePath string, r *rand.Rand, constraint int) string {
	// NoRel is defined tree-geometrically and cid-independent: any node that
	// is not the base path itself, not an ancestor/descendant (the Parent and
	// Child lines) and not a sibling under the same parent directory counts
	// as unrelated. Candidates are collected over the whole tree (the tree
	// root itself is not a candidate).
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	excludeDir := GetParentDir(excludePath)
	candidates := make([]*FileNode, 0)
	var walk func(n *FileNode)
	walk = func(n *FileNode) {
		for _, child := range n.Children {
			if nodeMatchesConstraint(child, constraint) {
				fp := child.FullPath
				if fp != excludePath &&
					!strings.HasPrefix(excludePath, fp+"/") &&
					!strings.HasPrefix(fp, excludePath+"/") &&
					GetParentDir(fp) != excludeDir {
					candidates = append(candidates, child)
				}
			}
			walk(child)
		}
	}
	walk(ft.Root)

	if len(candidates) == 0 {
		return ""
	}
	// Depth-weighted pick over the unrelated candidates.
	var buckets [4][]*FileNode
	for _, n := range candidates {
		b := int(DepthBucketOf(n.FullPath))
		if b < 0 {
			b = 0
		}
		if b > 3 {
			b = 3
		}
		buckets[b] = append(buckets[b], n)
	}
	if picked := pickByBucket(r, buckets, DepthBucketWeights, nil); picked != nil {
		return picked.FullPath
	}
	return candidates[r.Intn(len(candidates))].FullPath
}

func (ft *FileTree) GetFilesForCid(cid string) []string {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	files, ok := ft.FilesByCid[cid]
	if !ok {
		return nil
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.FullPath
	}
	return paths
}

func (ft *FileTree) GetDirsForCid(cid string) []string {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	dirs, ok := ft.DirsByCid[cid]
	if !ok {
		return nil
	}
	paths := make([]string, len(dirs))
	for i, d := range dirs {
		paths[i] = d.FullPath
	}
	return paths
}

func (ft *FileTree) UpdateHmdfsConfig(hmcfg *Hmdfs_config) {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	hmcfg.Init_file = make(map[string][]string)
	hmcfg.Init_dir = make(map[string][]string)
	hmcfg.Init_tmpdir = make(map[string][]string)
	hmcfg.Init_empty_dir = make(map[string][]string)
	hmcfg.File_in_persistence_dir = make(map[string][]string)

	hmcfg.FileSize = make(map[string]map[string]uint64)

	for cid, nodes := range ft.NodesByCid {
		for _, node := range nodes {
			switch node.Type {
			case NodeTypeFile:
				hmcfg.Init_file[cid] = append(hmcfg.Init_file[cid], node.FullPath)
				if node.Size > 0 {
					if hmcfg.FileSize[cid] == nil {
						hmcfg.FileSize[cid] = make(map[string]uint64)
					}
					hmcfg.FileSize[cid][node.FullPath] = node.Size
				}
			case NodeTypeDir:
				hmcfg.Init_dir[cid] = append(hmcfg.Init_dir[cid], node.FullPath)
			case NodeTypeEmptyDir:
				hmcfg.Init_empty_dir[cid] = append(hmcfg.Init_empty_dir[cid], node.FullPath)
			case NodeTypeTmpDir:
				hmcfg.Init_tmpdir[cid] = append(hmcfg.Init_tmpdir[cid], node.FullPath)
			}
		}
	}
}

type ConcurrentOp struct {
	CallName string
	// PathArgs names the op's path arguments: single-path ops carry one entry;
	// a rename carries two slots, [0] the source relation resolved against the
	// pattern base and [1] the destination relation resolved against the
	// source. PathSelfTwo is the rename-pair token (the first path in [0], the
	// derived fresh second path in [1]); PathSibling destinations are
	// geometric (same parent, fresh fallback via pickRenameDst); PathSame
	// denotes the pattern base itself and requires a same-class sibling as
	// source (see renameDstKindOf).
	PathArgs []PathArgSpec
}

type PathArgSpec struct {
	Relation PathRelation
}

type ConcurrentPattern struct {
	Name        string
	ClientCount int
	Operations  [][]ConcurrentOp
	BaseKind    string // "file" or "dir": the node type the pattern's base needs
	OffsetRel   OffsetRelationType
}

type PredefinedPatterns struct {
	Patterns []ConcurrentPattern
}

func NewPredefinedPatterns() *PredefinedPatterns {
	pp := &PredefinedPatterns{Patterns: make([]ConcurrentPattern, 0, 15)}

	pp.initFilePatterns()
	pp.initDirPatterns()

	return pp
}

func (pp *PredefinedPatterns) initFilePatterns() {
	pp.Patterns = append(pp.Patterns, []ConcurrentPattern{
		{
			Name:        "write_write_diff_offset",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}}},
				{{"write", []PathArgSpec{{PathSame}}}},
			},
			BaseKind:  "file",
			OffsetRel: OffsetDifferent,
		},
		{
			Name:        "write_write_same_offset",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}}},
				{{"write", []PathArgSpec{{PathSame}}}},
			},
			BaseKind:  "file",
			OffsetRel: OffsetSame,
		},
		{
			Name:        "write_read_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}}},
				{{"read", []PathArgSpec{{PathSame}}}},
			},
			BaseKind:  "file",
			OffsetRel: OffsetSame,
		},
		{
			Name:        "truncate_read_write_concurrent",
			ClientCount: 3,
			Operations: [][]ConcurrentOp{
				{{"truncate", []PathArgSpec{{PathSelf}}}},
				{{"read", []PathArgSpec{{PathSame}}}},
				{{"write", []PathArgSpec{{PathSame}}}},
			},
			BaseKind:  "file",
			OffsetRel: OffsetUnspecified,
		},
	}...)

	pp.Patterns = append(pp.Patterns,
		ConcurrentPattern{
			Name:        "fsync_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}},
					{"fsync", []PathArgSpec{}}},
				{{"read", []PathArgSpec{{PathSame}}}},
			},
			BaseKind:  "file",
			OffsetRel: OffsetUnspecified,
		},
	)
}

func (pp *PredefinedPatterns) initDirPatterns() {
	pp.Patterns = append(pp.Patterns, []ConcurrentPattern{
		{
			Name:        "concurrent_mkdir_same",
			ClientCount: 3,
			Operations: [][]ConcurrentOp{
				{{"mkdir", []PathArgSpec{{PathSelf}}}},
				{{"mkdir", []PathArgSpec{{PathSame}}}},
				{{"mkdir", []PathArgSpec{{PathSame}}}},
			},
			BaseKind: "dir",
		},
		{
			Name:        "create_unlink_same",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"creat", []PathArgSpec{{PathSelf}}}},
				{{"unlink", []PathArgSpec{{PathSame}}}},
			},
			BaseKind: "dir",
		},
		{
			Name:        "mkdir_stat_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"mkdir", []PathArgSpec{{PathSelf}}}},
				{{"stat", []PathArgSpec{{PathSame}}}},
			},
			BaseKind: "dir",
		},
		{
			Name:        "rename_open_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"rename", []PathArgSpec{{PathSelfTwo}, {PathSelfTwo}}}},
				{{"open", []PathArgSpec{{PathSame}}}},
			},
			BaseKind: "dir",
		},
		{
			Name:        "rename_rename_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"rename", []PathArgSpec{{PathSelfTwo}, {PathSelfTwo}}}},
				{{"rename", []PathArgSpec{{PathSibling}, {PathSame}}}},
			},
			BaseKind: "dir",
		},
		{
			Name:        "readdir_creat_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"getdents64", []PathArgSpec{{PathSelf}}}},
				{{"creat", []PathArgSpec{{PathChild}}}},
			},
			BaseKind: "dir",
		},
		{
			Name:        "chmod_getattr_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"chmod", []PathArgSpec{{PathSelf}}}},
				{{"stat", []PathArgSpec{{PathSame}}}},
			},
			BaseKind: "dir",
		},
		{
			Name:        "rmdir_mkdir_child_concurrent",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"rmdir", []PathArgSpec{{PathSelf}}}},
				{{"mkdir", []PathArgSpec{{PathChild}}}},
			},
			BaseKind: "dir",
		},
	}...)

	pp.Patterns = append(pp.Patterns,
		ConcurrentPattern{
			Name:        "multi_rename_same_file",
			ClientCount: 3,
			Operations: [][]ConcurrentOp{
				{{"rename", []PathArgSpec{{PathSelfTwo}, {PathSelfTwo}}}},
				{{"rename", []PathArgSpec{{PathSame}, {PathSibling}}}},
				{{"rename", []PathArgSpec{{PathSame}, {PathSibling}}}},
			},
			BaseKind: "dir",
		},
		ConcurrentPattern{
			Name:        "rmdir_mkdir_same_name",
			ClientCount: 2,
			Operations: [][]ConcurrentOp{
				{{"rmdir", []PathArgSpec{{PathSelf}}}},
				{{"mkdir", []PathArgSpec{{PathSame}}}},
			},
			BaseKind: "dir",
		},
	)
}

func (pp *PredefinedPatterns) GetRandomPattern(r *rand.Rand) *ConcurrentPattern {
	if len(pp.Patterns) == 0 {
		return nil
	}
	return &pp.Patterns[r.Intn(len(pp.Patterns))]
}

type DistributedChoiceTable struct {
	mu sync.RWMutex

	// Profile selects call type and role policy; it never changes the DCT
	// key shape (RootCalls/Variants/Weights stay profile-neutral).
	Profile *DFSProfile

	RootCalls map[string]bool

	Variants map[string][]CallVariant

	Weights map[string]map[CallVariant]int
}

func NewDistributedChoiceTable() *DistributedChoiceTable {
	return BuildDefaultDCT(hmdfsProfile)
}

// BuildDefaultDCT builds the DCT for a profile. HMDFS keeps the historical
// root/variant list; GlusterFS uses its explicit 19/21 lists.
func BuildDefaultDCT(profile *DFSProfile) *DistributedChoiceTable {
	if profile == nil {
		profile = hmdfsProfile
	}
	dct := &DistributedChoiceTable{
		Profile:   profile,
		RootCalls: make(map[string]bool),
		Variants:  make(map[string][]CallVariant),
		Weights:   make(map[string]map[CallVariant]int),
	}
	rootCalls, variantCalls := FsopsSubCalls, FsopsSubCalls
	if profile != nil {
		if len(profile.Calls.RootCalls) > 0 {
			rootCalls = profile.Calls.RootCalls
		} else if len(profile.Calls.SubCalls) > 0 {
			rootCalls = profile.Calls.SubCalls
		}
		if len(profile.Calls.VariantCalls) > 0 {
			variantCalls = profile.Calls.VariantCalls
		} else if len(profile.Calls.SubCalls) > 0 {
			variantCalls = profile.Calls.SubCalls
		}
	}
	dct.initConfig(rootCalls, variantCalls)
	return dct
}

func (dct *DistributedChoiceTable) initDefaultConfig() {
	if dct.Profile == nil {
		dct.Profile = hmdfsProfile
	}
	dct.initConfig(FsopsSubCalls, FsopsSubCalls)
}

func (dct *DistributedChoiceTable) profileOrDefault() *DFSProfile {
	if dct != nil && dct.Profile != nil {
		return dct.Profile
	}
	return hmdfsProfile
}

func (dct *DistributedChoiceTable) initConfig(rootCallNames, variantCallNames []string) {
	//TODO: root and variant call may need optimization
	for _, callName := range rootCallNames {
		dct.RootCalls[callName] = true
	}

	relations := []PathRelation{PathSame, PathParent, PathChild, PathSibling, PathNoRel}

	for _, rootCall := range rootCallNames {
		variants := make([]CallVariant, 0)
		weights := make(map[CallVariant]int)

		for _, variantCall := range variantCallNames {
			variantRelations := relations
			// GlusterFS v1: readlink is only a follower of a symlink root and
			// only in the Same relation; it can never be a root or a
			// standalone variant of another root.
			if variantCall == "readlink" && dct.profileOrDefault().Name == "glusterfs" {
				if rootCall != "symlink" {
					continue
				}
				variantRelations = []PathRelation{PathSame}
			}
			if variantCall == "rename" {
				for _, srcRel := range variantRelations {
					for _, tgtRel := range variantRelations {
						if srcRel == tgtRel {
							cv := CallVariant{
								CallName:     variantCall,
								PathRelation: srcRel,
							}
							variants = append(variants, cv)
							weights[cv] = dct.getInitialWeight(rootCall, cv)
						}
					}
				}
			} else {
				for _, rel := range variantRelations {
					cv := CallVariant{
						CallName:     variantCall,
						PathRelation: rel,
					}
					variants = append(variants, cv)
					weights[cv] = dct.getInitialWeight(rootCall, cv)
				}
			}
		}

		dct.Variants[rootCall] = variants
		dct.Weights[rootCall] = weights
	}
}

// Seed-call categories for the initial weight formula now live in the
// per-profile CallProfile tables (NamespaceCalls/DataModCalls/DataObserverCalls).

// relBaseOf is the relation base weight: Same 30 / Parent-Child-Sibling 20 /
// NoRel (and anything else) 10.
func relBaseOf(rel PathRelation) int {
	switch rel {
	case PathSame:
		return 30
	case PathParent, PathChild, PathSibling:
		return 20
	default:
		return 10
	}
}

func profileExactListContains(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// conflictCoeffOf returns the pair conflict coefficient in percent:
// same call 150; namespace x namespace 150; namespace x data-modifier 120;
// namespace x data-observer 100; data-modifier x data-modifier 120
// (write x truncate 150); data-modifier x data-observer 120; any pair
// involving open 120; data-observer x data-observer 50.
func (p *DFSProfile) ConflictCoeffOf(rootCall, variantCall string) int {
	if p == nil {
		p = hmdfsProfile
	}
	if rootCall == variantCall {
		return 150
	}
	if rootCall == "open" || variantCall == "open" {
		return 120
	}
	rNS := profileExactListContains(p.Calls.NamespaceCalls, rootCall)
	vNS := profileExactListContains(p.Calls.NamespaceCalls, variantCall)
	rDM := profileExactListContains(p.Calls.DataModCalls, rootCall)
	vDM := profileExactListContains(p.Calls.DataModCalls, variantCall)
	rOB := profileExactListContains(p.Calls.DataObserverCalls, rootCall)
	vOB := profileExactListContains(p.Calls.DataObserverCalls, variantCall)
	switch {
	case rNS && vNS:
		return 150
	case (rNS && vDM) || (rDM && vNS):
		return 120
	case (rNS && vOB) || (rOB && vNS):
		return 100
	case rDM && vDM:
		if (rootCall == "write" && variantCall == "truncate") || (rootCall == "truncate" && variantCall == "write") {
			return 150
		}
		return 120
	case (rDM && vOB) || (rOB && vDM):
		return 120
	case rOB && vOB:
		return 50
	}
	return 100
}

// conflictCoeffOf is the HMDFS legacy wrapper.
func conflictCoeffOf(rootCall, variantCall string) int {
	return hmdfsProfile.ConflictCoeffOf(rootCall, variantCall)
}

// getInitialWeight computes relBase * conflictCoeff, clamped to [5, 45].
func (dct *DistributedChoiceTable) getInitialWeight(rootCall string, variant CallVariant) int {
	w := relBaseOf(variant.PathRelation) * dct.profileOrDefault().ConflictCoeffOf(rootCall, variant.CallName) / 100
	if w < 5 {
		w = 5
	}
	if w > 45 {
		w = 45
	}
	return w
}

func (dct *DistributedChoiceTable) ChooseVariant(rootCall string, r *rand.Rand) *CallVariant {
	dct.mu.Lock()
	defer dct.mu.Unlock()

	return dct.chooseVariant(rootCall, r, false, "", nil, true, true)
}

func (dct *DistributedChoiceTable) ChooseVariantFiltered(rootCall string, r *rand.Rand, baseIsFile bool, basePath string, relOKByType map[int]map[PathRelation]bool, sibParentOK bool, allowDirected bool) *CallVariant {
	dct.mu.Lock()
	defer dct.mu.Unlock()

	return dct.chooseVariant(rootCall, r, baseIsFile, basePath, relOKByType, sibParentOK, allowDirected)
}

// chooseVariant picks a variant for rootCall. Phase 3: candidates are all
// eligible (root, variant, relation) combos weighted by their static initial
// weight (the weight dynamics and explore-first pool are gone). Direction 3
// (directed coverage): with directedBiasProb and an initialized coverage
// board, a candidate is sampled proportionally to its uncovered target-cell
// count (potential roulette via pickWeightedIndex, zero-potential entries
// never picked); exhausted or suppressed candidates (potential 0) fall back
// to the normal sampler. relOKByType (may be nil = all relations allowed)
// restricts the candidate set per variant-call type (relTargetAny/File/Dir) to
// path relations that can currently produce a path of a compatible type for
// the base, so the picked combo is always generatable downstream.
// allowDirected gates Direction 3: Phase 2b keeps it on the generation side
// and disables it for the mutation path (whose directed budget is dispatched
// by MutateFsopsWithDCT). A picked candidate whose cells can only reproduce
// already-known event shapes on this base path (shape-drain guard, memoized
// by the board's shape epoch) falls back to the static sampler instead of
// spending a directed pick. basePath is the root call's target path
// (generation side); the mutation path may pass anything since Direction 3 is
// disabled there. The caller must hold dct.mu (write lock).
func (dct *DistributedChoiceTable) chooseVariant(rootCall string, r *rand.Rand, baseIsFile bool, basePath string, relOKByType map[int]map[PathRelation]bool, sibParentOK bool, allowDirected bool) *CallVariant {
	variants, ok := dct.Variants[rootCall]
	if !ok || len(variants) == 0 {
		return nil
	}

	weights := dct.Weights[rootCall]
	profile := dct.profileOrDefault()

	eligible := func(cv CallVariant) bool {
		// readlink only follows a symlink root in the Same relation. The DCT
		// builder already prunes other roots, but keep the gate local so a
		// stale table or a test-built DCT cannot emit phantom readlink cells.
		if cv.CallName == "readlink" {
			if profile.Name == "glusterfs" &&
				(rootCall != "symlink" || cv.PathRelation != PathSame) {
				return false
			}
		}
		// mkdir as a variant creates a directory; its target follows the
		// relation's geometric position: Same hits the base itself (rebuild
		// timing race), Parent hits the parent (same treatment as Same),
		// Child creates a fresh child inside the base, Sibling creates a
		// fresh sibling inside the parent, NoRel creates inside an unrelated
		// directory. Child/Sibling/NoRel build new names and need no existing
		// nodes; Same/Child land on or inside the base itself and therefore
		// need a directory base, while Parent/Sibling/NoRel target elsewhere
		// (the parent, a fresh sibling, an unrelated dir), so they only need
		// that location to exist and are valid for a file-typed base too.
		// Parent additionally needs a non-root parent (the merge root is not
		// a valid target); Sibling only needs the parent directory to exist
		// (the fresh name is what makes it viable; the merge root counts).
		if cv.CallName == "mkdir" {
			switch cv.PathRelation {
			case PathSame, PathChild:
				return !baseIsFile
			case PathNoRel:
				return true
			case PathParent:
				return relOKByType == nil || relOKByType[relTargetDir][PathParent]
			case PathSibling:
				return relOKByType == nil || sibParentOK
			}
			return false
		}
		// Same targets the base itself, so its type must match the call:
		// dir-only calls on a file base are doomed. Other relations target
		// elsewhere (parent/sibling/unrelated), whose types are handled by
		// the per-type relOK sets and the downstream constraint filtering.
		if baseIsFile && cv.PathRelation == PathSame &&
			profile.TargetConstraint(cv.CallName) == relTargetDir {
			return false
		}
		if relOKByType != nil {
			callType := profile.TargetConstraint(cv.CallName)
			if !relOKByType[callType][cv.PathRelation] {
				return false
			}
		}
		if !baseIsFile && isFileOnlyVariantRel(profile, cv) {
			return false
		}
		return true
	}

	all := make([]CallVariant, 0, len(variants))
	for _, cv := range variants {
		if eligible(cv) {
			all = append(all, cv)
		}
	}
	if len(all) == 0 {
		return nil
	}

	// Phase 3: the explore-first pool and the weight dynamics are gone; every
	// eligible candidate is selectable with its static initial weight.
	cand := all

	// Direction 3 (directed coverage): with directedBiasProb, sample the
	// candidate proportionally to its uncovered target cells (potential
	// roulette); exhausted/unreachable candidates fall back to the normal
	// sampler (never aim at a combo with nothing left to offer).
	if allowDirected && CoverageBoardReady() && r.Float64() < directedBiasProb {
		if v, pot := pickByDirectedPairPotential(cand, rootCall, r); v != nil {
			if !covBoard.direction3ShapeDrain(rootCall, v.CallName, v.PathRelation, basePath, !baseIsFile) {
				covBoard.noteDirectedPick(comboKey{Root: rootCall, Variant: v.CallName, Rel: v.PathRelation}, pot)
				return v
			}
		}
	}

	total := 0
	for _, cv := range cand {
		total += weights[cv]
	}

	var picked *CallVariant
	if total == 0 {
		picked = &cand[r.Intn(len(cand))]
	} else {
		x := r.Intn(total)
		cumSum := 0
		for i := range cand {
			cumSum += weights[cand[i]]
			if x < cumSum {
				picked = &cand[i]
				break
			}
		}
		if picked == nil {
			picked = &cand[len(cand)-1]
		}
	}

	return picked
}

// HasRoot reports whether rootCall has a variant table in this DCT.
func (dct *DistributedChoiceTable) HasRoot(rootCall string) bool {
	dct.mu.RLock()
	defer dct.mu.RUnlock()
	_, ok := dct.RootCalls[rootCall]
	return ok
}

// WeightOf returns the current weight of (rootCall, variant).
func (dct *DistributedChoiceTable) WeightOf(rootCall string, variant CallVariant) int {
	dct.mu.RLock()
	defer dct.mu.RUnlock()
	if weights, ok := dct.Weights[rootCall]; ok {
		return weights[variant]
	}
	return 0
}

func isDirOnlyCall(callName string) bool {
	for _, name := range DirOnlyCalls {
		if strings.Contains(callName, name) {
			return true
		}
	}
	return false
}

func isFileOnlyCallByName(callName string) bool {
	for _, name := range FileOnlyCalls {
		if strings.Contains(callName, name) {
			return true
		}
	}
	return false
}

func isFileOnlyVariantRel(profile *DFSProfile, cv CallVariant) bool {
	return profile.TargetConstraint(cv.CallName) == relTargetFile &&
		(cv.PathRelation == PathSelf || cv.PathRelation == PathSame || cv.PathRelation == PathParent)
}

// basePathIsFile decides whether the root call's base path is a file for
// variant filtering purposes. The intent of the root call wins over the tree
// state: mkdir/rmdir/getdents64 operate on directories (a fresh mkdir target
// is a directory even though it is not in the tree yet); file-intent roots
// (creat, truncate, read/write family, unlink, link) operate on files.
// Ambiguous roots (open/stat/chmod/rename/getxattr/setxattr) fall back to the
// tree state. Kept as the HMDFS wrapper; profile-aware callers use
// (*DFSProfile).BasePathIsFile.
func basePathIsFile(ft *FileTree, rootCallName, basePath string) bool {
	return hmdfsProfile.BasePathIsFile(ft, rootCallName, basePath)
}

func isDirPath(ft *FileTree, path string) bool {
	return nodeIsDir(ft.FindNode(path))
}

// ChooseTemporal picks the insertion form of a combo. Phase 3: no learning.
// Modifier roots are 50/50; non-modifier roots always use the concurrent form
// (an HB placement after such a root cannot produce an HB pair of this combo:
// the earlier vertex would be the non-modifier root).
// ChooseTemporal selects the temporal form of a root/variant pair. The
// variant argument is currently intentionally unused: callers must choose the
// temporal form before selecting the variant and its observed path (a rename
// root observes the source in CC and the destination in HB). Do not make the
// draw depend on variant without revisiting those callers.
func (dct *DistributedChoiceTable) ChooseTemporal(rootCall string, variant CallVariant, r *rand.Rand) TemporalRel {
	if !isModifierCall(rootCall) {
		return TemporalConcurrent
	}
	if r.Intn(2) == 0 {
		return TemporalConcurrent
	}
	return TemporalHB
}

func (dct *DistributedChoiceTable) ChoosePathRelation(rootCall, variantCallName string, r *rand.Rand) PathRelation {
	dct.mu.RLock()
	defer dct.mu.RUnlock()

	variants, ok := dct.Variants[rootCall]
	if !ok || len(variants) == 0 {
		return PathSelf
	}

	weights := dct.Weights[rootCall]
	total := 0
	for _, cv := range variants {
		if cv.CallName == variantCallName {
			total += weights[cv]
		}
	}
	if total == 0 {
		return PathSelf
	}

	x := r.Intn(total)
	cumSum := 0
	for _, cv := range variants {
		if cv.CallName != variantCallName {
			continue
		}
		cumSum += weights[cv]
		if x < cumSum {
			return cv.PathRelation
		}
	}
	return PathSelf
}

func (dct *DistributedChoiceTable) IsRootCall(callName string) bool {
	dct.mu.RLock()
	defer dct.mu.RUnlock()
	return dct.RootCalls[callName]
}

func (dct *DistributedChoiceTable) GetVariants(rootCall string) []CallVariant {
	dct.mu.RLock()
	defer dct.mu.RUnlock()

	if variants, ok := dct.Variants[rootCall]; ok {
		result := make([]CallVariant, len(variants))
		copy(result, variants)
		return result
	}
	return nil
}

type LayeredChoiceStrategy struct {
	PredefinedPatterns    *PredefinedPatterns
	DCTFsops              *DistributedChoiceTable
	CausalDerivationTable *CausalDerivationTable
	FileTree              *FileTree
	// Profile is the active per-DFS policy table. It is planner-only and
	// never part of DCT/coverage keys.
	Profile *DFSProfile

	// FsopsChoiceTable selects root calls for DCT insertion, built from the
	// fsops allowlist (close enabled as a legal bias source, never a root).
	FsopsChoiceTable *ChoiceTable

	// patternProbability is the seed-generation probability of building a
	// program from a predefined pattern (generateFromPredefinedPattern). The
	// mutation insertion path no longer uses patterns (removed 2026-09-28).
	patternProbability float64
	// Per-VM TSC offsets for normalizing call timing across VMs when
	// aligning concurrent insertions by execution time.
	tscoffs []int64

	// PendingDirected is the Phase 2b exact-cell pending batch (one entry per
	// inserted variant), resolved by ResolveDirectedPending after the
	// following execution.
	PendingDirected []DirectedPending
}

func NewLayeredChoiceStrategy(hmcfg *Hmdfs_config, target *Target) *LayeredChoiceStrategy {
	profile := hmdfsProfile
	if hmcfg != nil {
		profile = hmcfg.ProfileOrDefault()
	}
	lcs := &LayeredChoiceStrategy{
		PredefinedPatterns:    NewPredefinedPatterns(),
		DCTFsops:              BuildDefaultDCT(profile),
		CausalDerivationTable: newCausalDerivationTableFor(profile),
		FileTree:              nil,
		Profile:               profile,
		patternProbability:    0.15,
	}

	if hmcfg != nil && hmcfg.FileTree != nil {
		lcs.FileTree = hmcfg.FileTree
	} else if hmcfg != nil {
		lcs.FileTree = NewFileTreeForConfig(hmcfg)
		lcs.FileTree.InitFromHmdfsConfig(hmcfg)
	}

	if target != nil {
		choiceCalls := FsopsChoiceCalls
		if hmcfg != nil && hmcfg.Profile != nil && len(hmcfg.Profile.Calls.ChoiceCalls) > 0 {
			choiceCalls = hmcfg.Profile.Calls.ChoiceCalls
		}
		lcs.FsopsChoiceTable = buildSubChoiceTable(target, choiceCalls)
	}

	return lcs
}

func buildSubChoiceTable(target *Target, callNames []string) *ChoiceTable {
	enabled := make(map[*Syscall]bool)
	for _, meta := range target.Syscalls {
		for _, name := range callNames {
			if meta.Name == name {
				enabled[meta] = true
				break
			}
		}
	}
	return target.BuildChoiceTable(nil, enabled)
}

// ShouldUsePattern reports whether seed generation takes the predefined
// pattern path. Only GenerateProgsForHmdfsFsops consults it now; the mutation
// insertion path no longer calls it (removed 2026-09-28).
func (lcs *LayeredChoiceStrategy) ShouldUsePattern(r *rand.Rand) bool {
	return r.Float64() < lcs.patternProbability
}

func (lcs *LayeredChoiceStrategy) GetDCT() *DistributedChoiceTable {
	return lcs.DCTFsops
}

func (lcs *LayeredChoiceStrategy) profileOrDefault() *DFSProfile {
	if lcs != nil && lcs.Profile != nil {
		return lcs.Profile
	}
	return hmdfsProfile
}

func (lcs *LayeredChoiceStrategy) ChooseConcurrentCall(rootCallName string, r *rand.Rand) *CallVariant {
	dct := lcs.GetDCT()
	return dct.ChooseVariant(rootCallName, r)
}

func (lcs *LayeredChoiceStrategy) ChooseConcurrentCallFiltered(rootCallName string, r *rand.Rand, baseIsFile bool, basePath string, allowDirected bool) *CallVariant {
	dct := lcs.GetDCT()
	relOKByType, sibParentOK := lcs.availableRelationsForBase(basePath)
	return dct.ChooseVariantFiltered(rootCallName, r, baseIsFile, basePath, relOKByType, sibParentOK, allowDirected)
}

// countNodesByType counts file vs directory nodes in a candidate set
// (dir = anything that is not a file node, consistent with
// nodeMatchesConstraint).
func countNodesByType(nodes []*FileNode) (files, dirs int) {
	for _, n := range nodes {
		if n.Type == NodeTypeFile {
			files++
		} else {
			dirs++
		}
	}
	return files, dirs
}

// availableRelationsForBase reports, per variant-call type
// (relTargetAny/File/Dir), which path relations can currently produce a path
// of a compatible type relative to basePath. The checks are deterministic (no
// RNG consumption) and mirror the empty semantics of GetPathByRelation —
// including the fresh-base parent anchoring and the merge-view root never
// being a Parent target — so a combo picked under these sets is generatable
// downstream. Same is always available; NoRel is normally available but can
// come back empty on a degenerate tree with no unrelated node, in which case
// callers keep their stat/skip fallback. A parent is always a directory, so
// file-typed calls never get Parent; Child/Sibling availability is type-aware
// (file-typed calls need existing file children/siblings, dir-typed calls need
// directory ones). The second return value reports whether basePath's parent
// directory exists (the merge-view root counts): mkdir Sibling builds a fresh
// name inside that parent and therefore needs no existing sibling.
func (lcs *LayeredChoiceStrategy) availableRelationsForBase(basePath string) (map[int]map[PathRelation]bool, bool) {
	relOKByType := map[int]map[PathRelation]bool{
		relTargetAny:  {PathSame: true, PathNoRel: true},
		relTargetFile: {PathSame: true, PathNoRel: true, PathParent: false},
		relTargetDir:  {PathSame: true, PathNoRel: true},
	}
	ft := lcs.FileTree
	if ft == nil {
		return relOKByType, false
	}
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	baseNode := ft.findNodeLocked(basePath)
	if baseNode != nil {
		parent := ft.GetParent(baseNode)
		parentOK := parent != nil
		hasParent := parent != nil && parent != ft.Root
		relOKByType[relTargetAny][PathParent] = hasParent
		relOKByType[relTargetDir][PathParent] = hasParent
		// relTargetFile: PathParent stays false (parent is a directory).
		files, dirs := countNodesByType(ft.GetChildren(baseNode))
		relOKByType[relTargetAny][PathChild] = files+dirs > 0
		relOKByType[relTargetFile][PathChild] = files > 0
		relOKByType[relTargetDir][PathChild] = dirs > 0
		sf, sd := countNodesByType(ft.GetSibling(baseNode))
		relOKByType[relTargetAny][PathSibling] = sf+sd > 0
		relOKByType[relTargetFile][PathSibling] = sf > 0
		relOKByType[relTargetDir][PathSibling] = sd > 0
		return relOKByType, parentOK
	}
	// Fresh base (outside the tree): anchor on the parent directory. A fresh
	// path directly under the merge-view root has no legal Parent relation
	// (the root is not a rename/rmdir source), but its Sibling relation (the
	// other depth-1 nodes) is still available, so the early return only
	// triggers when the parent itself is outside the tree.
	parent := ft.parentNodeLocked(basePath)
	if parent == nil {
		return relOKByType, false // only Same and NoRel
	}
	if parent != ft.Root {
		relOKByType[relTargetAny][PathParent] = true
		relOKByType[relTargetDir][PathParent] = true
	}
	// Child stays false for every type: a fresh base has no existing children.
	files, dirs := countNodesByType(ft.GetChildren(parent))
	relOKByType[relTargetAny][PathSibling] = files+dirs > 0
	relOKByType[relTargetFile][PathSibling] = files > 0
	relOKByType[relTargetDir][PathSibling] = dirs > 0
	return relOKByType, true
}

// SetTscOffsets configures per-VM TSC offsets used to normalize call timing
// across VMs for time-aligned concurrent insertions.
func (lcs *LayeredChoiceStrategy) SetTscOffsets(tscoffs []int64) {
	lcs.tscoffs = tscoffs
}

// tscoffFor returns the TSC offset of VM nodeIdx (0 if unknown).
func (lcs *LayeredChoiceStrategy) tscoffFor(nodeIdx int) int64 {
	if nodeIdx >= 0 && nodeIdx < len(lcs.tscoffs) {
		return lcs.tscoffs[nodeIdx]
	}
	if len(lcs.tscoffs) == 0 {
		return 0
	}
	return lcs.tscoffs[len(lcs.tscoffs)-1]
}

func (lcs *LayeredChoiceStrategy) GetPathForVariant(basePath string, basePath2 string, variant CallVariant, r *rand.Rand, isTwoPath bool) string {
	return lcs.FileTree.GetPathByRelation(basePath, basePath2, variant.PathRelation, r, isTwoPath, relTargetAny)
}

func (lcs *LayeredChoiceStrategy) GetPathsForRenameVariant(basePath string, basePath2 string, pathRelation PathRelation, r *rand.Rand, isTwoPath bool) (string, string) {
	srcPath := lcs.FileTree.GetPathByRelation(basePath, basePath2, pathRelation, r, isTwoPath, relTargetAny)
	if srcPath == "" {
		// 该关系无匹配（无子/无兄弟/无父/无无关路径）——不产出；处理方式由
		// 调用方决定（种子构建侧保底 stat、变异插入侧跳过该节点）。关系语义
		// 保持——空路径 rename 恒 ENOENT 且退化会改变路径关系。
		return "", ""
	}
	return srcPath, lcs.pickRenameDst(srcPath, r)
}

// renameExistingDstProb is the probability that a DCT-generated rename picks
// an existing same-parent destination (the E overwrite shape) instead of a
// fresh sibling name.
const renameExistingDstProb = 0.30

// renameSuffixBase strips one trailing "._renamed_*" extension (the historical
// rename-destination convention) from path. It is the single normalization
// step shared by the DCT destination chooser and the 2b batch assembler, so a
// name cannot accumulate one suffix per rename round.
func renameSuffixBase(path string) string {
	ext := filepath.Ext(path)
	if strings.Contains(ext, "._renamed_") {
		return path[:len(path)-len(ext)]
	}
	return path
}

// freshRenameDst returns the fresh sibling destination name of src, reusing the
// historical "._renamed_<suffix>" convention and stripping a previous suffix so
// repeated renames never accumulate extensions.
func freshRenameDst(src string, r *rand.Rand) string {
	return renameSuffixBase(src) + "._renamed_" + randomSuffix(r)
}

// pickRenameDst chooses the destination path of a DCT rename: with probability
// renameExistingDstProb an existing non-TmpDir sibling of the same class
// (file -> file; dir -> dir, empty or not), uniformly among the candidates
// sorted by path; otherwise a fresh sibling name. Same-parent is a hard hmdfs
// requirement (hmdfs_rename_merge compares the parent paths before tracing), so
// cross-parent destinations are never generated.
func (lcs *LayeredChoiceStrategy) pickRenameDst(src string, r *rand.Rand) string {
	return pickRenameDstPolicy(hmdfsProfile, lcsFileTree(lcs), src, PathSibling, r)
}

func lcsFileTree(lcs *LayeredChoiceStrategy) *FileTree {
	if lcs == nil {
		return nil
	}
	return lcs.FileTree
}

func (lcs *LayeredChoiceStrategy) pickRenameDstFor(src string, rel PathRelation, r *rand.Rand) string {
	profile := hmdfsProfile
	if lcs != nil && lcs.Profile != nil {
		profile = lcs.Profile
	}
	return pickRenameDstPolicy(profile, lcsFileTree(lcs), src, rel, r)
}

// pickRenameDstPolicy is the profile-aware RenamePolicy hook. HMDFS keeps the
// historical same-parent selection. GlusterFS v1 allows cross-parent
// destinations; for a fresh destination it samples the relation's parent and
// otherwise falls back to the source parent.
func pickRenameDstPolicy(profile *DFSProfile, ft *FileTree, src string, rel PathRelation, r *rand.Rand) string {
	if r == nil {
		return src + "._renamed_000000"
	}
	if profile == nil || profile.Name != "glusterfs" {
		return pickRenameDstHmdfs(ft, src, r)
	}
	if ft == nil {
		return freshRenameDst(src, r)
	}
	srcNode := ft.FindNode(src)
	if srcNode == nil {
		return freshRenameDst(src, r)
	}
	srcIsDir := srcNode.Type != NodeTypeFile
	parent := GetParentDir(src)
	targetParent := parent
	switch rel {
	case PathParent:
		if parent != "" {
			if p := ft.FindNode(parentDirOrRoot(parent)); p != nil && p.Type != NodeTypeFile &&
				p.Type != NodeTypeTmpDir {
				targetParent = p.FullPath
			}
		}
	case PathChild:
		if srcIsDir {
			var dirs []string
			for _, n := range ft.GetChildren(srcNode) {
				if n != nil && n.Type != NodeTypeFile && n.Type != NodeTypeTmpDir {
					dirs = append(dirs, n.FullPath)
				}
			}
			if len(dirs) > 0 {
				sort.Strings(dirs)
				targetParent = dirs[r.Intn(len(dirs))]
			}
		}
	case PathNoRel:
		if p := ft.getRandomUnrelatedPath(src, r, relTargetDir); p != "" {
			targetParent = p
		}
	}
	if targetParent == "" {
		targetParent = parent
	}
	// Existing same-class destination in the target parent.
	if r.Float64() < renameExistingDstProb {
		parentNode := ft.FindNode(targetParent)
		if parentNode != nil {
			var pick []string
			for _, n := range ft.GetChildren(parentNode) {
				if n == nil || n.Type == NodeTypeTmpDir || n.FullPath == src {
					continue
				}
				if (n.Type == NodeTypeFile) != (srcNode.Type == NodeTypeFile) {
					continue
				}
				if n.Type != NodeTypeFile && len(n.Children) > 0 && rel != PathNoRel {
					continue
				}
				if pathUnderRoot(n.FullPath, src) {
					continue
				}
				pick = append(pick, n.FullPath)
			}
			if len(pick) > 0 {
				sort.Strings(pick)
				return pick[r.Intn(len(pick))]
			}
		}
	}
	base := GetFileName(src)
	for i := 0; i < 8; i++ {
		dst := joinTestPath(targetParent, base+"._renamed_"+randomSuffix(r))
		if dst != src && !pathUnderRoot(dst, src) && ft.FindNode(dst) == nil {
			return dst
		}
	}
	return ""
}

func pickRenameDstHmdfs(ft *FileTree, src string, r *rand.Rand) string {
	if r == nil {
		return src + "._renamed_000000"
	}
	if ft == nil {
		return freshRenameDst(src, r)
	}
	if r.Float64() >= renameExistingDstProb {
		return freshRenameDst(src, r)
	}
	if sib := existingRenameDstSibling(ft, src, r); sib != "" {
		return sib
	}
	return freshRenameDst(src, r)
}

// existingRenameDstSibling returns an existing non-TmpDir sibling of src with
// the same file/dir class, or "" when none exists. It is the strict
// counterpart of pickRenameDst's existing-destination branch (no fresh
// fallback): the non-TmpDir filter is the deliberate E-destination policy
// (B/variant-side relations may land on TmpDir, an existing rename destination
// may not). It is a destination helper only — source selection uses
// GetPathByRelation with nodeTypeConstraint instead.
func (lcs *LayeredChoiceStrategy) existingRenameDstSibling(src string, r *rand.Rand) string {
	return existingRenameDstSibling(lcsFileTree(lcs), src, r)
}

func existingRenameDstSibling(ft *FileTree, src string, r *rand.Rand) string {
	if r == nil || ft == nil {
		return ""
	}
	srcNode := ft.FindNode(src)
	if srcNode == nil {
		return ""
	}
	srcClass := nodeTypeConstraint(srcNode)
	cands := ft.GetNonTmpDirSiblings(src)
	if len(cands) == 0 {
		return ""
	}
	sort.Strings(cands)
	var pick []string
	for _, c := range cands {
		n := ft.FindNode(c)
		if n == nil {
			continue
		}
		if nodeTypeConstraint(n) != srcClass {
			continue
		}
		pick = append(pick, c)
	}
	if len(pick) == 0 {
		return ""
	}
	return pick[r.Intn(len(pick))]
}

func randomSuffix(r *rand.Rand) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = charset[r.Intn(len(charset))]
	}
	return string(b)
}

func (lcs *LayeredChoiceStrategy) AddCreatedFile(path string, cid string) {
	lcs.FileTree.AddNode(path, NodeTypeFile, cid)
}

func (lcs *LayeredChoiceStrategy) AddCreatedDir(path string, cid string, isEmpty bool) {
	nodeType := NodeTypeDir
	if isEmpty {
		//TODO: 不应该是空就是tmp，而是为了创建目标文件而中途创建的目录才是tmp
		nodeType = NodeTypeEmptyDir
	}
	lcs.FileTree.AddNode(path, nodeType, cid)
}

func (lcs *LayeredChoiceStrategy) RemoveFile(path string) {
	lcs.FileTree.RemoveNode(path)
}

func (lcs *LayeredChoiceStrategy) RenameFile(oldPath, newPath string) {
	lcs.FileTree.RenameNode(oldPath, newPath)
}

func (lcs *LayeredChoiceStrategy) UpdateHmdfsConfig(hmcfg *Hmdfs_config) {
	lcs.FileTree.UpdateHmdfsConfig(hmcfg)
}

func GetParentDir(path string) string {
	return filepath.Dir(path)
}

func GetFileName(path string) string {
	return filepath.Base(path)
}

func JoinPath(dir, name string) string {
	return filepath.Join(dir, name)
}

func IsPathInMergeView(path string) bool {
	return strings.HasPrefix(path, MergeViewPrefix)
}

// NormalizePath normalizes a path to prefix-relative form. An empty prefix
// yields a relative path with no leading slash or "./".
func NormalizePath(prefix, path string) string {
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "/")
	if prefix == "" {
		return path
	}
	root := strings.TrimSuffix(prefix, "/")
	path = strings.TrimPrefix(path, prefix)
	path = strings.TrimPrefix(path, root+"/")
	return path
}

func IsUnderRoot(prefix, path string) bool {
	if prefix == "" {
		return true
	}
	root := strings.TrimSuffix(prefix, "/")
	return path == root || strings.HasPrefix(path, prefix) || strings.HasPrefix(path, root+"/")
}

// NormalizeFsMdPath aligns an fsMd key with the active FileTree namespace.
// fsMd keys are relative to the test cwd and may carry a leading "./"; prefix
// is the FileTree's namespace (merge_view for HMDFS, empty for GlusterFS).
func NormalizeFsMdPath(prefix, path string) string {
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "/")
	if prefix == "" {
		return path
	}
	root := strings.TrimSuffix(prefix, "/")
	if path == root || strings.HasPrefix(path, root+"/") {
		return path
	}
	return root + "/" + path
}

// NormalizeMergeViewPath preserves the historical HMDFS wrapper.
func NormalizeMergeViewPath(path string) string {
	return NormalizeFsMdPath(MergeViewPrefix, path)
}

func SortCallVariants(variants []CallVariant) {
	sort.Slice(variants, func(i, j int) bool {
		if variants[i].CallName != variants[j].CallName {
			return variants[i].CallName < variants[j].CallName
		}
		return variants[i].PathRelation < variants[j].PathRelation
	})
}

func extractRenamePaths(call *Call) (string, string) {
	if len(call.Args) < 2 {
		return "", ""
	}

	oldPath := extractPathFromCallByArgIdx(call, 0)
	newPath := extractPathFromCallByArgIdx(call, 1)

	return oldPath, newPath
}

func resolveFdToPath(p *Prog, fdCall *Call) string {
	if len(fdCall.Args) < 1 {
		return ""
	}
	fdArg, ok := fdCall.Args[0].(*ResultArg)
	if !ok || fdArg.Res == nil {
		return ""
	}
	for _, call := range p.Calls {
		if !strings.Contains(call.Meta.Name, "open") && !strings.Contains(call.Meta.Name, "creat") {
			continue
		}
		if call.Ret == nil {
			continue
		}
		if usesFdCheck(call.Ret, fdArg.Res) {
			return extractPathFromCall(call)
		}
	}
	return ""
}

func usesFdCheck(openRet *ResultArg, fdRes *ResultArg) bool {
	return fdRes == openRet
}

func isPathInEmptyDirList(path string, hmcfg *Hmdfs_config) bool {
	for _, dirs := range hmcfg.Init_empty_dir {
		for _, dir := range dirs {
			if dir == path {
				return true
			}
		}
	}
	return false
}

func isPathInPersistenceDir(path string, hmcfg *Hmdfs_config) bool {
	return pathInDir(path, hmcfg.Persistence_dir)
}

func updateFileSizeInHmdfsConfig(path string, cid string, size uint64, hmcfg *Hmdfs_config) {
	if hmcfg.FileSize == nil {
		hmcfg.FileSize = make(map[string]map[string]uint64)
	}
	if hmcfg.FileSize[cid] == nil {
		hmcfg.FileSize[cid] = make(map[string]uint64)
	}
	hmcfg.FileSize[cid][path] = size
}

func removeFromHmdfsConfig(path string, isDir bool, cid string, hmcfg *Hmdfs_config) {
	//是不是有可能操作到tmpdir上？
	if isDir {
		if dirs, ok := hmcfg.Init_dir[cid]; ok {
			for i, d := range dirs {
				if d == path {
					hmcfg.Init_dir[cid] = append(dirs[:i], dirs[i+1:]...)
					break
				}
			}
		}
		if dirs, ok := hmcfg.Init_empty_dir[cid]; ok {
			for i, d := range dirs {
				if d == path {
					hmcfg.Init_empty_dir[cid] = append(dirs[:i], dirs[i+1:]...)
					break
				}
			}
		}
	} else {
		if files, ok := hmcfg.Init_file[cid]; ok {
			for i, f := range files {
				if f == path {
					hmcfg.Init_file[cid] = append(files[:i], files[i+1:]...)
					break
				}
			}
		}
		if files, ok := hmcfg.File_in_persistence_dir[cid]; ok {
			for i, f := range files {
				if f == path {
					hmcfg.File_in_persistence_dir[cid] = append(files[:i], files[i+1:]...)
					break
				}
			}
		}
		if sizes, ok := hmcfg.FileSize[cid]; ok {
			delete(sizes, path)
		}
	}
}

func renameInHmdfsConfig(oldPath, newPath, cid string, hmcfg *Hmdfs_config) {
	if files, ok := hmcfg.Init_file[cid]; ok {
		for i, f := range files {
			if f == oldPath {
				hmcfg.Init_file[cid][i] = newPath
				break
			}
		}
	}
	if files, ok := hmcfg.File_in_persistence_dir[cid]; ok {
		for i, f := range files {
			if f == oldPath {
				hmcfg.File_in_persistence_dir[cid][i] = newPath
				break
			}
		}
	}
	if dirs, ok := hmcfg.Init_dir[cid]; ok {
		for i, d := range dirs {
			if d == oldPath {
				hmcfg.Init_dir[cid][i] = newPath
				break
			}
		}
	}
	if dirs, ok := hmcfg.Init_empty_dir[cid]; ok {
		for i, d := range dirs {
			if d == oldPath {
				hmcfg.Init_empty_dir[cid][i] = newPath
				break
			}
		}
	}
	if sizes, ok := hmcfg.FileSize[cid]; ok {
		if sz, ok := sizes[oldPath]; ok {
			sizes[newPath] = sz
			delete(sizes, oldPath)
		}
	}
	//empty dir和普通dir的处理，可能还会有其它地方做得不对
}

type CreateCallType int

const (
	CreateTypeFile CreateCallType = iota
	CreateTypeMkdir
	CreateTypeOpen
	CreateTypeRename
)

func GetCreateInfo(call *Call) (path string, oldPath string, ctype CreateCallType) {
	callName := call.Meta.Name
	switch {
	case strings.Contains(callName, "creat"):
		return extractPathFromCall(call), "", CreateTypeFile
	case strings.Contains(callName, "mkdir"):
		return extractPathFromCall(call), "", CreateTypeMkdir
	case strings.Contains(callName, "open"):
		flags := extractOpenFlags(call)
		if flags&0x40 != 0 {
			return extractPathFromCall(call), "", CreateTypeOpen
		}
	case strings.Contains(callName, "rename"):
		newPath := extractPathFromCallByArgIdx(call, 1)
		oldP := extractPathFromCallByArgIdx(call, 0)
		if newPath != "" {
			return newPath, oldP, CreateTypeRename
		}
	}
	return "", "", -1
}

func parseOpenFlags(call *Call) (uint64, bool) {
	if call == nil || len(call.Args) < 2 {
		return 0, false
	}
	flagsArg, ok := call.Args[1].(*ConstArg)
	if !ok {
		return 0, false
	}
	return flagsArg.Val, true
}

func extractOpenFlags(call *Call) uint64 {
	flags, _ := parseOpenFlags(call)
	return flags
}

func IsUnlinkCall(callName string) bool {
	return strings.Contains(callName, "unlink")
}

func GetDeletePath(call *Call) string {
	if strings.Contains(call.Meta.Name, "unlink") {
		return extractPathFromCall(call)
	}
	return ""
}

func findCidFromHmcfg(path string, hmcfg *Hmdfs_config) string {
	for cid, files := range hmcfg.Init_file {
		for _, f := range files {
			if f == path {
				return cid
			}
		}
	}
	for cid, dirs := range hmcfg.Init_dir {
		for _, d := range dirs {
			if d == path {
				return cid
			}
		}
	}
	for cid, dirs := range hmcfg.Init_tmpdir {
		for _, d := range dirs {
			if d == path {
				return cid
			}
		}
	}
	for cid, dirs := range hmcfg.Init_empty_dir {
		for _, d := range dirs {
			if d == path {
				return cid
			}
		}
	}
	for cid, files := range hmcfg.File_in_persistence_dir {
		for _, f := range files {
			if f == path {
				return cid
			}
		}
	}
	return ""
}

func SyncFileTreeFromFsMd(fsMd map[string]FileMetadata, ownerMap map[string]string, hmcfg *Hmdfs_config) {
	if hmcfg.FileTree == nil {
		// Keep the namespace profile-aware: GlusterFS uses an empty prefix,
		// while HMDFS keeps merge_view.
		hmcfg.FileTree = NewFileTreeForConfig(hmcfg)
	}
	fsMdPrefix := hmcfg.FileTree.Prefix

	hmcfg.FileTree.mu.Lock()
	defer hmcfg.FileTree.mu.Unlock()

	oldNodes := make(map[string]*FileNode)
	var collectOld func(*FileNode)
	collectOld = func(node *FileNode) {
		if node != hmcfg.FileTree.Root {
			oldNodes[node.FullPath] = node
		}
		for _, child := range node.Children {
			collectOld(child)
		}
	}
	collectOld(hmcfg.FileTree.Root)

	// All normalized fsMd paths: an empty directory has no child path in it.
	paths := make([]string, 0, len(fsMd))
	for p := range fsMd {
		paths = append(paths, NormalizeFsMdPath(fsMdPrefix, p))
	}
	isDirEmpty := func(dirPath string) bool {
		prefix := dirPath + "/"
		for _, p := range paths {
			if strings.HasPrefix(p, prefix) {
				return false
			}
		}
		return true
	}
	// TmpDir is a generator-planning concept: once created it stays TmpDir.
	// Only Dir <-> EmptyDir transitions follow the fsMd snapshot.
	resolveNodeType := func(oldType FileNodeType, stat syscall.Stat_t, path string) FileNodeType {
		if oldType == NodeTypeTmpDir {
			return oldType
		}
		nt := determineNodeType(stat)
		if nt == NodeTypeDir && isDirEmpty(path) {
			return NodeTypeEmptyDir
		}
		return nt
	}

	for path, md := range fsMd {
		path = NormalizeFsMdPath(fsMdPrefix, path) // fsMd keys are "./<rel>" — align with the tree namespace
		oldNode := oldNodes[path]

		if oldNode == nil {
			nodeType := determineNodeType(md.StatMd)
			if nodeType == NodeTypeDir && isDirEmpty(path) {
				nodeType = NodeTypeEmptyDir
			}
			cid := resolveCidForPath(path, ownerMap, hmcfg)
			node := hmcfg.FileTree.addNodeLocked(path, nodeType, cid)
			if node != nil && nodeType == NodeTypeFile && md.StatMd.Size > 0 {
				node.Size = uint64(md.StatMd.Size)
				updateFileSizeInHmdfsConfig(path, cid, uint64(md.StatMd.Size), hmcfg)
			}
		} else {
			nodeType := resolveNodeType(oldNode.Type, md.StatMd, path)
			cid := oldNode.OwnerCid
			if ownerMap != nil {
				if c, ok := ownerMap[path]; ok {
					cid = c
				}
			}
			ownerChanged := oldNode.OwnerCid != cid
			typeChanged := oldNode.Type != nodeType
			if ownerChanged {
				// Ownership change: re-link NodesByCid unconditionally
				// (independent of type/size changes — a pure owner
				// re-resolution must stick).
				hmcfg.FileTree.removeNodeFromCidLists(oldNode)
				oldNode.OwnerCid = cid
				hmcfg.FileTree.NodesByCid[cid] = append(hmcfg.FileTree.NodesByCid[cid], oldNode)
			}
			if ownerChanged || typeChanged {
				// Owner or type change: re-link Files/DirsByCid exactly once.
				// The flags are captured BEFORE OwnerCid is overwritten:
				// testing oldNode.OwnerCid != cid here would be false after
				// the assignment above, so a pure owner change would drop the
				// node from every type list (lost for list-based selectors).
				hmcfg.FileTree.removeNodeFromTypeList(oldNode)
				if nodeType == NodeTypeFile {
					hmcfg.FileTree.FilesByCid[oldNode.OwnerCid] = append(hmcfg.FileTree.FilesByCid[oldNode.OwnerCid], oldNode)
				} else {
					hmcfg.FileTree.DirsByCid[oldNode.OwnerCid] = append(hmcfg.FileTree.DirsByCid[oldNode.OwnerCid], oldNode)
				}
			}
			if oldNode.Type != nodeType || (nodeType == NodeTypeFile && oldNode.Size != uint64(md.StatMd.Size)) {
				oldNode.Type = nodeType
				oldNode.Size = uint64(md.StatMd.Size)
				if nodeType == NodeTypeFile && md.StatMd.Size > 0 {
					updateFileSizeInHmdfsConfig(path, cid, uint64(md.StatMd.Size), hmcfg)
				}
			}
		}
		delete(oldNodes, path)
	}

	for _, oldNode := range oldNodes {
		hmcfg.FileTree.removeNodeLocked(oldNode.FullPath) // caller holds ft.mu
	}
}

func resolveCidForPath(path string, ownerMap map[string]string, hmcfg *Hmdfs_config) string {
	if ownerMap != nil {
		if cid, ok := ownerMap[path]; ok {
			return cid
		}
	}
	return findCidFromHmcfg(path, hmcfg)
}

func determineNodeType(stat syscall.Stat_t) FileNodeType {
	if stat.Mode == 0 {
		return NodeTypeFile
	}
	if stat.Mode&syscall.S_IFDIR != 0 {
		return NodeTypeDir
	}
	return NodeTypeFile
}
