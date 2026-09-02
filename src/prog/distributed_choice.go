package prog

import (
	"fmt"
	"math/rand"
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
	}

	FileOrDirCalls = []string{
		"open",
		"stat",
		"chmod",
		"rename",
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

var FileopsSubCalls = []string{
	"open", "close", "read", "write", "pread64", "pwrite64",
	"fsync", "fdatasync", "truncate",
}

var InodeopsSubCalls = []string{
	"mkdir", "rmdir", "creat", "unlink", "rename",
	"chmod", "truncate", "stat",
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
	NodesByCid map[string][]*FileNode
	FilesByCid map[string][]*FileNode
	DirsByCid  map[string][]*FileNode
	mu         sync.RWMutex
}

func NewFileTree() *FileTree {
	return &FileTree{
		Root: &FileNode{
			Name:     "merge_view",
			FullPath: "merge_view",
			Type:     NodeTypeDir,
			Children: make(map[string]*FileNode),
		},
		NodesByCid: make(map[string][]*FileNode),
		FilesByCid: make(map[string][]*FileNode),
		DirsByCid:  make(map[string][]*FileNode),
	}
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
	cleanPath := strings.TrimPrefix(path, MergeViewPrefix)
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
				current.Type = nodeType
				current.OwnerCid = ownerCid
			}
		} else {
			newNode := &FileNode{
				Name:     part,
				FullPath: current.FullPath + "/" + part,
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
	// Caller must hold ft.mu.
	cleanPath := strings.TrimPrefix(path, MergeViewPrefix)
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

	if current.Parent != nil {
		delete(current.Parent.Children, current.Name)
	}

	ft.removeNodeFromCidLists(current)
	return true
}

func (ft *FileTree) removeNodeFromCidLists(node *FileNode) {
	// The node may appear in several cid lists (the same path registered
	// under multiple cids) — sweep all of them, not just the current OwnerCid.
	for cid, list := range ft.NodesByCid {
		for i, n := range list {
			if n == node {
				ft.NodesByCid[cid] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}

	if node.Type == NodeTypeFile {
		for cid, list := range ft.FilesByCid {
			for i, n := range list {
				if n == node {
					ft.FilesByCid[cid] = append(list[:i], list[i+1:]...)
					break
				}
			}
		}
	} else {
		for cid, list := range ft.DirsByCid {
			for i, n := range list {
				if n == node {
					ft.DirsByCid[cid] = append(list[:i], list[i+1:]...)
					break
				}
			}
		}
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

func (ft *FileTree) RenameNode(oldPath, newPath string) bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	cleanOldPath := strings.TrimPrefix(oldPath, MergeViewPrefix)
	cleanNewPath := strings.TrimPrefix(newPath, MergeViewPrefix)

	if cleanOldPath == "" || cleanNewPath == "" {
		return false
	}

	oldParts := strings.Split(cleanOldPath, "/")
	current := ft.Root

	for _, part := range oldParts {
		if part == "" {
			continue
		}
		if child, exists := current.Children[part]; exists {
			current = child
		} else {
			return false
		}
	}

	if current.Parent != nil {
		delete(current.Parent.Children, current.Name)
	}

	newParts := strings.Split(cleanNewPath, "/")
	newParent := ft.Root
	for i := 0; i < len(newParts)-1; i++ {
		part := newParts[i]
		if part == "" {
			continue
		}
		if child, exists := newParent.Children[part]; exists {
			newParent = child
		} else {
			return false
		}
	}

	newName := newParts[len(newParts)-1]
	current.Name = newName
	current.FullPath = newParent.FullPath + "/" + newName
	current.PathLength = len(current.FullPath)
	current.Parent = newParent
	newParent.Children[newName] = current

	ft.updateChildPaths(current)

	return true
}

func (ft *FileTree) updateChildPaths(node *FileNode) {
	for _, child := range node.Children {
		child.FullPath = node.FullPath + "/" + child.Name
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
	cleanPath := strings.TrimPrefix(path, MergeViewPrefix)
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
	node := ft.FindNode(dirPath)
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
	node := ft.FindNode(dirPath)
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

func (ft *FileTree) GetRandomFile(r *rand.Rand, cid string) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	files, ok := ft.FilesByCid[cid]
	if !ok || len(files) == 0 {
		return nil
	}
	return files[r.Intn(len(files))]
}

func (ft *FileTree) GetRandomFileExcluding(r *rand.Rand, excludeCid string) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	var candidates []*FileNode
	for cid, files := range ft.FilesByCid {
		if cid != excludeCid {
			candidates = append(candidates, files...)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[r.Intn(len(candidates))]
}

func (ft *FileTree) GetRandomDir(r *rand.Rand, cid string, allowEmpty bool) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	dirs, ok := ft.DirsByCid[cid]
	if !ok || len(dirs) == 0 {
		return nil
	}
	CandidatesDirs := make([]*FileNode, 0)
	if allowEmpty {
		for _, d := range dirs {
			if d.Type != NodeTypeTmpDir {
				CandidatesDirs = append(CandidatesDirs, d)
			}
		}
		//return dirs[r.Intn(len(dirs))]
	} else {
		for _, d := range dirs {
			if d.Type != NodeTypeEmptyDir && d.Type != NodeTypeTmpDir {
				CandidatesDirs = append(CandidatesDirs, d)
			}
		}
	}

	if len(CandidatesDirs) == 0 {
		return nil
	}
	return CandidatesDirs[r.Intn(len(CandidatesDirs))]
}

func (ft *FileTree) GetRandomDirExcluding(r *rand.Rand, excludeCid string) *FileNode {
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	var candidates []*FileNode
	for cid, dirs := range ft.DirsByCid {
		if cid == excludeCid {
			continue
		}
		for _, d := range dirs {
			if d.Type != NodeTypeEmptyDir && d.Type != NodeTypeTmpDir {
				candidates = append(candidates, d)
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[r.Intn(len(candidates))]
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

func (ft *FileTree) GetPathByRelation(basePath string, basePath2 string, relation PathRelation, r *rand.Rand, cid string, isTwoPath bool) string {
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
		return ""
	}

	switch relation {
	case PathSelf, PathSelfTwo, PathSame:
		return basePath

	case PathParent:
		parent := ft.GetParent(baseNode)
		if parent == nil {
			return ""
		}
		return parent.FullPath
	case PathChild:
		children := ft.GetChildren(baseNode)
		if len(children) == 0 {
			return ""
		}
		return children[r.Intn(len(children))].FullPath
	case PathSibling:
		siblings := ft.GetSibling(baseNode)
		if len(siblings) == 0 {
			return ""
		}
		return siblings[r.Intn(len(siblings))].FullPath
	case PathNoRel:
		return ft.getRandomUnrelatedPath(basePath, r, cid)
	default:
		return ""
	}
}

func (ft *FileTree) getRandomUnrelatedPath(excludePath string, r *rand.Rand, cid string) string {
	//TODO: get random path without cid
	ft.mu.RLock()
	defer ft.mu.RUnlock()

	nodes, ok := ft.NodesByCid[cid]
	if !ok || len(nodes) == 0 {
		return ""
	}

	candidates := make([]*FileNode, 0)
	for _, node := range nodes {
		if node.FullPath != excludePath &&
			!strings.HasPrefix(excludePath, node.FullPath+"/") &&
			!strings.HasPrefix(node.FullPath, excludePath+"/") {
			candidates = append(candidates, node)
		}
	}

	if len(candidates) == 0 {
		return ""
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
	PathArgs []PathArgSpec
}

type PathArgSpec struct {
	Relation PathRelation
}

type ConcurrentPattern struct {
	Name        string
	ClientCount int
	SharedRes   string
	Operations  [][]ConcurrentOp
	TestPoint   string
	SeedType    string
	OffsetRel   OffsetRelationType
	LengthRel   OffsetRelationType
}

type PredefinedPatterns struct {
	FileopsPatterns  []ConcurrentPattern
	InodeopsPatterns []ConcurrentPattern
}

func NewPredefinedPatterns() *PredefinedPatterns {
	pp := &PredefinedPatterns{
		FileopsPatterns:  make([]ConcurrentPattern, 0),
		InodeopsPatterns: make([]ConcurrentPattern, 0),
	}

	pp.initFileopsPatterns()
	pp.initInodeopsPatterns()

	return pp
}

func (pp *PredefinedPatterns) initFileopsPatterns() {
	pp.FileopsPatterns = []ConcurrentPattern{
		{
			Name:        "write_write_diff_offset",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}}},
				{{"write", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "async_writeback_merge",
			SeedType:  "fileops",
			OffsetRel: OffsetDifferent,
			LengthRel: OffsetDifferent,
		},
		{
			Name:        "write_write_same_offset",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}}},
				{{"write", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "write_conflict_resolution",
			SeedType:  "fileops",
			OffsetRel: OffsetSame,
			LengthRel: OffsetSame,
		},
		{
			Name:        "write_read_concurrent",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}}},
				{{"read", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "read_visibility",
			SeedType:  "fileops",
			OffsetRel: OffsetSame,
			LengthRel: OffsetSame,
		},
		{
			Name:        "truncate_read_write_concurrent",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"truncate", []PathArgSpec{{PathSelf}}}},
				{{"read", []PathArgSpec{{PathSame}}}},
				{{"write", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "truncate_during_read",
			SeedType:  "fileops",
			OffsetRel: OffsetUnspecified,
			LengthRel: OffsetUnspecified,
		},
	}

	pp.FileopsPatterns = append(pp.FileopsPatterns,
		ConcurrentPattern{
			Name:        "fsync_concurrent",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"write", []PathArgSpec{{PathSelf}}},
					{"fsync", []PathArgSpec{}}},
				{{"read", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "fsync_visibility",
			SeedType:  "fileops",
			OffsetRel: OffsetUnspecified,
			LengthRel: OffsetUnspecified,
		},
	)
}

func (pp *PredefinedPatterns) initInodeopsPatterns() {
	pp.InodeopsPatterns = []ConcurrentPattern{
		{
			Name:        "concurrent_mkdir_same",
			ClientCount: 3,
			SharedRes:   "same_dir",
			Operations: [][]ConcurrentOp{
				{{"mkdir", []PathArgSpec{{PathSelf}}}},
				{{"mkdir", []PathArgSpec{{PathSame}}}},
				{{"mkdir", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "mkdir_atomicity",
			SeedType:  "inodeops",
		},
		{
			Name:        "create_unlink_same",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"creat", []PathArgSpec{{PathSelf}}}},
				{{"unlink", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "create_unlink_consistency",
			SeedType:  "inodeops",
		},
		{
			Name:        "mkdir_stat_concurrent",
			ClientCount: 2,
			SharedRes:   "same_dir",
			Operations: [][]ConcurrentOp{
				{{"mkdir", []PathArgSpec{{PathSelf}}}},
				{{"stat", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "mkdir_stat_consistency",
			SeedType:  "inodeops",
		},
		{
			Name:        "rename_open_concurrent",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"rename", []PathArgSpec{{PathSelfTwo}, {PathSelfTwo}}}},
				{{"open", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "rename_open_atomicity",
			SeedType:  "inodeops",
		},
		{
			Name:        "rename_rename_concurrent",
			ClientCount: 2,
			SharedRes:   "two_files",
			Operations: [][]ConcurrentOp{
				{{"rename", []PathArgSpec{{PathSelfTwo}, {PathSelfTwo}}}},
				{{"rename", []PathArgSpec{{PathSame}, {PathSibling}}}},
			},
			TestPoint: "rename_rename_conflict",
			SeedType:  "inodeops",
		},
		{
			Name:        "readdir_creat_concurrent",
			ClientCount: 2,
			SharedRes:   "same_dir",
			Operations: [][]ConcurrentOp{
				{{"getdents64", []PathArgSpec{{PathSelf}}}},
				{{"creat", []PathArgSpec{{PathChild}}}},
			},
			TestPoint: "readdir_creat_consistency",
			SeedType:  "inodeops",
		},
		{
			Name:        "chmod_getattr_concurrent",
			ClientCount: 2,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"chmod", []PathArgSpec{{PathSelf}}}},
				{{"stat", []PathArgSpec{{PathSame}}}},
			},
			TestPoint: "chmod_stat_consistency",
			SeedType:  "inodeops",
		},
		{
			Name:        "rmdir_mkdir_child_concurrent",
			ClientCount: 2,
			SharedRes:   "parent_child",
			Operations: [][]ConcurrentOp{
				{{"rmdir", []PathArgSpec{{PathSelf}}}},
				{{"mkdir", []PathArgSpec{{PathChild}}}},
			},
			TestPoint: "rmdir_mkdir_child_conflict",
			SeedType:  "inodeops",
		},
	}

	pp.InodeopsPatterns = append(pp.InodeopsPatterns,
		ConcurrentPattern{
			Name:        "multi_rename_same_file",
			ClientCount: 3,
			SharedRes:   "same_file",
			Operations: [][]ConcurrentOp{
				{{"rename", []PathArgSpec{{PathSelfTwo}, {PathSelfTwo}}}},
				{{"rename", []PathArgSpec{{PathSame}, {PathSibling}}}},
				{{"rename", []PathArgSpec{{PathSame}, {PathSibling}}}},
			},
			TestPoint: "multi_rename_conflict",
			SeedType:  "inodeops",
		},
	)
}

func (pp *PredefinedPatterns) GetRandomPattern(seedType string, r *rand.Rand) *ConcurrentPattern {
	var patterns []ConcurrentPattern
	switch seedType {
	case "fileops":
		patterns = pp.FileopsPatterns
	case "inodeops":
		patterns = pp.InodeopsPatterns
	default:
		return nil
	}

	if len(patterns) == 0 {
		return nil
	}
	return &patterns[r.Intn(len(patterns))]
}

func (pp *PredefinedPatterns) GetPatternsForNodeCount(seedType string, nodeCount int) []ConcurrentPattern {
	var patterns []ConcurrentPattern
	switch seedType {
	case "fileops":
		patterns = pp.FileopsPatterns
	case "inodeops":
		patterns = pp.InodeopsPatterns
	default:
		return nil
	}

	result := make([]ConcurrentPattern, 0)
	for _, p := range patterns {
		if p.ClientCount <= nodeCount {
			result = append(result, p)
		}
	}
	return result
}

type DistributedChoiceTable struct {
	mu sync.RWMutex

	RootCalls map[string]bool

	Variants map[string][]CallVariant

	Weights map[string]map[CallVariant]int

	// Explored marks (rootCall, variant) combos that have produced new
	// feedback signal; unexplored combos are preferred (direction 1).
	Explored map[string]map[CallVariant]bool
	// NoYield counts consecutive selections without new signal; combos
	// reaching the threshold are down-weighted (direction 2).
	NoYield map[string]map[CallVariant]int

	// TemporalWeights models the second layer of the choice strategy: for
	// each (rootCall, variant) combo, how likely its concurrent form vs its
	// causal (HB) form is to produce the corresponding DAG pair. Initialized
	// lazily to 50/50; updated by feedback from the actually produced pair
	// temporal (see UpdateTemporalWeight). Independent of the direction-1/2
	// mechanisms: HB pairs only update these weights, they never mark a combo
	// explored.
	TemporalWeights map[string]map[CallVariant]TemporalWeight

	seedType string
}

// TemporalWeight holds the selection weights of the two insertion forms of a
// combo: concurrent (time-aligned) and causal (after the root finishes).
type TemporalWeight struct {
	Concurrent int
	HB         int
}

const temporalInitWeight = 1

// Direction-2 parameters: after noYieldThreshold consecutive selections
// without yielding new signal, a combo loses noYieldDelta weight (only while
// its weight exceeds noYieldDelta, so the effective floor is noYieldDelta+1).
// maxComboWeight caps the MarkYield reward (defensive symmetric counterpart).
const (
	noYieldThreshold = 20
	noYieldDelta     = 5
	maxComboWeight   = 100
)

func NewDistributedChoiceTable(seedType string) *DistributedChoiceTable {
	dct := &DistributedChoiceTable{
		RootCalls:       make(map[string]bool),
		Variants:        make(map[string][]CallVariant),
		Weights:         make(map[string]map[CallVariant]int),
		Explored:        make(map[string]map[CallVariant]bool),
		NoYield:         make(map[string]map[CallVariant]int),
		TemporalWeights: make(map[string]map[CallVariant]TemporalWeight),
		seedType:        seedType,
	}

	dct.initDefaultConfig()
	return dct
}

func (dct *DistributedChoiceTable) initDefaultConfig() {
	var rootCallNames []string
	var variantCallNames []string

	if dct.seedType == "fileops" {
		rootCallNames = []string{"open", "read", "write", "pread64", "pwrite64", "fsync", "fdatasync", "truncate"}
		variantCallNames = []string{"open", "read", "write", "pread64", "pwrite64", "fsync", "fdatasync", "truncate", "stat"}
	} else {
		rootCallNames = []string{"mkdir", "rmdir", "creat", "unlink", "rename", "chmod", "truncate", "stat"}
		variantCallNames = []string{"mkdir", "rmdir", "creat", "unlink", "rename", "chmod", "truncate", "stat", "open", "getdents64", "write", "read", "pwrite64", "pread64"}
	}
	//TODO: root and variant call may need optimization
	for _, callName := range rootCallNames {
		dct.RootCalls[callName] = true
	}

	relations := []PathRelation{PathSame, PathParent, PathChild, PathSibling, PathNoRel}

	for _, rootCall := range rootCallNames {
		variants := make([]CallVariant, 0)
		weights := make(map[CallVariant]int)

		for _, variantCall := range variantCallNames {
			if variantCall == "rename" {
				for _, srcRel := range relations {
					for _, tgtRel := range relations {
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
				for _, rel := range relations {
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
		dct.Explored[rootCall] = make(map[CallVariant]bool)
		dct.NoYield[rootCall] = make(map[CallVariant]int)
	}
}

func (dct *DistributedChoiceTable) getInitialWeight(rootCall string, variant CallVariant) int {
	baseWeight := 10

	if variant.PathRelation == PathSame {
		baseWeight = 30
	} else if variant.PathRelation == PathSibling {
		baseWeight = 20
	} else if variant.PathRelation == PathParent || variant.PathRelation == PathChild {
		baseWeight = 15
	} else if variant.PathRelation == PathNoRel {
		baseWeight = 5
	}

	conflictPairs := map[string]map[string]int{
		"mkdir":    {"rmdir": 25, "creat": 20, "unlink": 15, "write": 15, "read": 15},
		"rmdir":    {"mkdir": 25, "creat": 20, "unlink": 15, "write": 15, "read": 15},
		"creat":    {"unlink": 30, "rmdir": 20, "mkdir": 20, "write": 25, "read": 25},
		"unlink":   {"creat": 30, "mkdir": 15, "rmdir": 15, "write": 30, "read": 20},
		"rename":   {"rename": 35, "unlink": 25, "creat": 20, "write": 20, "read": 20},
		"chmod":    {"read": 15, "write": 15, "truncate": 20},
		"truncate": {"write": 25, "read": 20, "chmod": 20},
		"stat":     {"write": 10, "read": 10},
		"open":     {"read": 25, "write": 25, "truncate": 20},
		"write":    {"read": 30, "write": 25, "truncate": 20, "unlink": 30, "creat": 25, "mkdir": 15, "rmdir": 15},
		"read":     {"write": 30, "truncate": 15, "unlink": 20, "creat": 25, "mkdir": 15, "rmdir": 15},
	}

	if conflictGroup, ok := conflictPairs[rootCall]; ok {
		if w, ok := conflictGroup[variant.CallName]; ok {
			baseWeight = w
		}
	}

	return baseWeight
}

func (dct *DistributedChoiceTable) ChooseVariant(rootCall string, r *rand.Rand) *CallVariant {
	dct.mu.Lock()
	defer dct.mu.Unlock()

	return dct.chooseVariant(rootCall, r, false)
}

func (dct *DistributedChoiceTable) ChooseVariantFiltered(rootCall string, r *rand.Rand, baseIsFile bool) *CallVariant {
	dct.mu.Lock()
	defer dct.mu.Unlock()

	return dct.chooseVariant(rootCall, r, baseIsFile)
}

// chooseVariant picks a variant for rootCall. Direction 1: combos that never
// produced signal (and are still within their exploration budget) are
// preferred. Direction 2: the picked combo's consecutive no-yield counter is
// bumped and it is down-weighted once the threshold is reached.
// The caller must hold dct.mu (write lock).
func (dct *DistributedChoiceTable) chooseVariant(rootCall string, r *rand.Rand, baseIsFile bool) *CallVariant {
	variants, ok := dct.Variants[rootCall]
	if !ok || len(variants) == 0 {
		return nil
	}

	weights := dct.Weights[rootCall]

	eligible := func(cv CallVariant) bool {
		if baseIsFile && isDirOnlyCall(cv.CallName) {
			return false
		}
		if baseIsFile && (cv.PathRelation == PathChild || cv.PathRelation == PathSibling) {
			return false
		}
		if !baseIsFile && isFileOnlyVariantRel(cv) {
			return false
		}
		return true
	}

	all := make([]CallVariant, 0, len(variants))
	unexplored := make([]CallVariant, 0)
	for _, cv := range variants {
		if !eligible(cv) {
			continue
		}
		all = append(all, cv)
		if !dct.Explored[rootCall][cv] && dct.NoYield[rootCall][cv] < noYieldThreshold {
			unexplored = append(unexplored, cv)
		}
	}
	cand := all
	if len(unexplored) > 0 {
		cand = unexplored
	}
	if len(cand) == 0 {
		return nil
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

	dct.noYieldTick(rootCall, *picked)
	return picked
}

// noYieldTick bumps the consecutive no-yield counter of a selected combo and
// down-weights it once the threshold is reached (direction 2). Combos above
// noYieldDelta lose noYieldDelta weight (gentle decay); combos in (1,
// noYieldDelta] are dropped straight to 1 on the next trigger (no frozen
// mid-range states); weight 1 is the floor. The caller must hold dct.mu
// (write lock).
func (dct *DistributedChoiceTable) noYieldTick(rootCall string, cv CallVariant) {
	if dct.NoYield[rootCall] == nil {
		return
	}
	n := dct.NoYield[rootCall][cv] + 1
	if n < noYieldThreshold {
		dct.NoYield[rootCall][cv] = n
		return
	}
	dct.NoYield[rootCall][cv] = 0
	if w, ok := dct.Weights[rootCall][cv]; ok {
		if w > noYieldDelta {
			dct.Weights[rootCall][cv] = w - noYieldDelta
		} else if w > 1 {
			dct.Weights[rootCall][cv] = 1
		}
	}
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

// MarkYield records that (rootCall, variant) produced new feedback signal:
// the combo is marked explored (direction 1), its consecutive no-yield
// counter is reset (direction 2), and its weight is rewarded by +1 (capped at
// maxComboWeight as a defensive symmetric counterpart of the down-weight
// floor; the natural bound is the combo's novel-pair space anyway).
func (dct *DistributedChoiceTable) MarkYield(rootCall string, variant CallVariant) {
	dct.mu.Lock()
	defer dct.mu.Unlock()
	if _, ok := dct.Weights[rootCall]; !ok {
		return
	}
	if _, ok := dct.Weights[rootCall][variant]; !ok {
		return
	}
	if dct.Explored[rootCall] != nil {
		dct.Explored[rootCall][variant] = true
	}
	if dct.NoYield[rootCall] != nil {
		dct.NoYield[rootCall][variant] = 0
	}
	if w := dct.Weights[rootCall][variant]; w < maxComboWeight {
		dct.Weights[rootCall][variant] = w + 1
	}
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

func isFileOnlyVariantRel(cv CallVariant) bool {
	return isFileOnlyCallByName(cv.CallName) &&
		(cv.PathRelation == PathSelf || cv.PathRelation == PathSame || cv.PathRelation == PathParent)
}

func isDirPath(ft *FileTree, path string) bool {
	node := ft.FindNode(path)
	return node != nil && (node.Type == NodeTypeDir || node.Type == NodeTypeEmptyDir)
}

// temporalWeightLocked returns the (lazily initialized) temporal weights of a
// combo; the caller must hold dct.mu.
func (dct *DistributedChoiceTable) temporalWeightLocked(rootCall string, variant CallVariant) *TemporalWeight {
	byRoot, ok := dct.TemporalWeights[rootCall]
	if !ok {
		byRoot = make(map[CallVariant]TemporalWeight)
		dct.TemporalWeights[rootCall] = byRoot
	}
	tw, ok := byRoot[variant]
	if !ok {
		tw = TemporalWeight{Concurrent: temporalInitWeight, HB: temporalInitWeight}
		byRoot[variant] = tw
	}
	return &tw
}

// ChooseTemporal picks the insertion form of a combo by its learned weights:
// concurrent (time-aligned, favoring CONCURRENT pairs) or causal (after the
// root finishes, favoring HB pairs). New combos start at 50/50.
func (dct *DistributedChoiceTable) ChooseTemporal(rootCall string, variant CallVariant, r *rand.Rand) TemporalRel {
	dct.mu.Lock()
	tw := dct.temporalWeightLocked(rootCall, variant)
	total := tw.Concurrent + tw.HB
	if total <= 0 {
		tw.Concurrent = temporalInitWeight
		tw.HB = temporalInitWeight
		total = tw.Concurrent + tw.HB
		dct.TemporalWeights[rootCall][variant] = *tw
	}
	if r.Intn(total) < tw.Concurrent {
		dct.mu.Unlock()
		return TemporalConcurrent
	}
	dct.mu.Unlock()
	return TemporalHB
}

// UpdateTemporalWeight rewards the form that actually produced a pair of the
// given temporal: a CONCURRENT pair rewards the concurrent form, an HB pair
// the causal form. Cross outcomes (the intended form produced the other
// temporal) are not rewarded — the weights thus learn which form reliably
// produces its corresponding pair.
func (dct *DistributedChoiceTable) UpdateTemporalWeight(rootCall string, variant CallVariant, actual TemporalRel) {
	dct.mu.Lock()
	defer dct.mu.Unlock()
	tw := dct.temporalWeightLocked(rootCall, variant)
	switch actual {
	case TemporalConcurrent:
		tw.Concurrent++
	case TemporalHB:
		tw.HB++
	}
	dct.TemporalWeights[rootCall][variant] = *tw
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
	PredefinedPatterns  *PredefinedPatterns
	DCTInodeops         *DistributedChoiceTable
	DCTFileops          *DistributedChoiceTable
	FileTree            *FileTree
	SeedType            string
	FileopsChoiceTable  *ChoiceTable
	InodeopsChoiceTable *ChoiceTable

	patternProbability float64
	// Per-VM TSC offsets for normalizing call timing across VMs when
	// aligning concurrent insertions by execution time.
	tscoffs []int64
}

func NewLayeredChoiceStrategy(seedType string, hmcfg *Hmdfs_config, target *Target) *LayeredChoiceStrategy {
	lcs := &LayeredChoiceStrategy{
		PredefinedPatterns:  NewPredefinedPatterns(),
		DCTInodeops:         NewDistributedChoiceTable("inodeops"),
		DCTFileops:          NewDistributedChoiceTable("fileops"),
		FileTree:            nil,
		SeedType:            seedType,
		FileopsChoiceTable:  nil,
		InodeopsChoiceTable: nil,
		patternProbability:  0.3,
	}

	if hmcfg != nil && hmcfg.FileTree != nil {
		lcs.FileTree = hmcfg.FileTree
	} else if hmcfg != nil {
		lcs.FileTree = NewFileTree()
		lcs.FileTree.InitFromHmdfsConfig(hmcfg)
	}

	if target != nil {
		lcs.FileopsChoiceTable = buildSubChoiceTable(target, FileopsSubCalls)
		lcs.InodeopsChoiceTable = buildSubChoiceTable(target, InodeopsSubCalls)
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

func (lcs *LayeredChoiceStrategy) ShouldUsePattern(r *rand.Rand) bool {
	return r.Float64() < lcs.patternProbability
}

func (lcs *LayeredChoiceStrategy) GetDCT() *DistributedChoiceTable {
	if lcs.SeedType == "fileops" {
		return lcs.DCTFileops
	}
	return lcs.DCTInodeops
}

func (lcs *LayeredChoiceStrategy) ChooseConcurrentCall(rootCallName string, r *rand.Rand) *CallVariant {
	dct := lcs.GetDCT()
	return dct.ChooseVariant(rootCallName, r)
}

func (lcs *LayeredChoiceStrategy) ChooseConcurrentCallFiltered(rootCallName string, r *rand.Rand, baseIsFile bool) *CallVariant {
	dct := lcs.GetDCT()
	return dct.ChooseVariantFiltered(rootCallName, r, baseIsFile)
}

// MarkYield propagates a yield signal (new DAG pair / new coverage) into the
// active DCT table: the combo is marked explored and its no-yield counter is
// reset.
func (lcs *LayeredChoiceStrategy) MarkYield(rootCallName string, variant CallVariant) {
	lcs.GetDCT().MarkYield(rootCallName, variant)
}

// UpdateTemporalWeight propagates the actually produced pair temporal into the
// temporal-form weights of the combo (second layer, see
// DistributedChoiceTable.UpdateTemporalWeight).
func (lcs *LayeredChoiceStrategy) UpdateTemporalWeight(rootCallName string, variant CallVariant, actual TemporalRel) {
	lcs.GetDCT().UpdateTemporalWeight(rootCallName, variant, actual)
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

func (lcs *LayeredChoiceStrategy) GetPathForVariant(basePath string, basePath2 string, variant CallVariant, r *rand.Rand, cid string, isTwoPath bool) string {
	return lcs.FileTree.GetPathByRelation(basePath, basePath2, variant.PathRelation, r, cid, isTwoPath)
}

func (lcs *LayeredChoiceStrategy) GetPathsForRenameVariant(basePath string, basePath2 string, pathRelation PathRelation, r *rand.Rand, cid string, isTwoPath bool) (string, string) {
	srcPath := lcs.FileTree.GetPathByRelation(basePath, basePath2, pathRelation, r, cid, isTwoPath)
	if srcPath == "" {
		// 该关系无匹配（无子/无兄弟/无父/无无关路径）——不产出（调用方跳过）——
		// 关系语义保持（空路径 rename 恒 ENOENT 且退化会改变路径关系，S16）。
		return "", ""
	}
	srcPathExt := filepath.Ext(srcPath)
	if strings.Contains(srcPathExt, "._renamed_") {
		siblingPath := srcPath[:len(srcPath)-len(srcPathExt)]
		siblingPath = siblingPath + "._renamed_" + randomSuffix(r)
		return srcPath, siblingPath
	}
	//退化为PathSame了
	return srcPath, srcPath + "._renamed_" + randomSuffix(r)
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

func NormalizeMergeViewPath(path string) string {
	path = strings.TrimPrefix(path, "./") // fsMd relative keys carry a leading "./"
	if !strings.HasPrefix(path, MergeViewPrefix) {
		return MergeViewPrefix + strings.TrimPrefix(path, "/")
	}
	return path
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
	if hmcfg.Persistence_dir == "" {
		return false
	}
	return strings.HasPrefix(path, hmcfg.Persistence_dir)
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
	CreateTypeFile   CreateCallType = iota
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

func extractOpenFlags(call *Call) uint64 {
	if len(call.Args) < 2 {
		return 0
	}
	if flagsArg, ok := call.Args[1].(*ConstArg); ok {
		return flagsArg.Val
	}
	return 0
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
		hmcfg.FileTree = NewFileTree()
	}

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
		paths = append(paths, NormalizeMergeViewPath(p))
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
		path = NormalizeMergeViewPath(path) // fsMd keys are "./<rel>" — align with the tree's merge_view/... keys
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
			if oldNode.OwnerCid != cid {
				// Ownership change: re-link NodesByCid unconditionally
				// (independent of type/size changes — a pure owner
				// re-resolution must stick).
				hmcfg.FileTree.removeNodeFromCidLists(oldNode)
				oldNode.OwnerCid = cid
				hmcfg.FileTree.NodesByCid[cid] = append(hmcfg.FileTree.NodesByCid[cid], oldNode)
			}
			if oldNode.OwnerCid != cid || oldNode.Type != nodeType {
				// Owner or type change: re-link Files/DirsByCid exactly once
				// (the node stays in the tree, so NodesByCid is untouched on
				// a pure type change).
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
