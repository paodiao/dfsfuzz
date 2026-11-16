package prog

import "strings"

// DFSProfile is the per-filesystem policy table. HMDFS keeps the existing
// behavior; GlusterFS v1 uses the explicit profile below. The struct is
// intentionally data-only so the fuzzer, generator and executor can select
// policy without scattering DFSName checks.
type DFSProfile struct {
	Name    string
	Calls   CallProfile
	Events  EventProfile
	Paths   PathPolicy
	Plan    PlanProfile
	Tree    TreeProfile
	Gen     GenProfile
	Check   CheckerProfile
	Trace   TraceProfile
	Feature FeatureProfile
}

type CallProfile struct {
	SubCalls        []string
	ChoiceCalls     []string
	DirOnlyCalls    []string
	FileOnlyCalls   []string
	FileOrDirCalls  []string
	FdRequiredCalls []string
	RootCalls       []string
	VariantCalls    []string
	CausalRootCalls []string
}

type EventProfile struct {
	SupportsSameInode bool
}

type PathPolicy struct {
	RootPrefix       string
	SameParentRename bool
	MaxDepth         int
	MaxClientNum     int
}

type PlanProfile struct {
	DCTRootCalls    []string
	DCTVariantCalls []string
	CausalRootCalls []string
}

type TreeProfile struct {
	Prefix string
}

type GenProfile struct {
	Stash      bool
	Dcache     bool
	Fsops      bool
	ClientOnly bool
}

type CheckerProfile struct {
	SymlinkPath     bool
	FullType        bool
	WeakNlink       bool
	DiagnosticMtime bool
	UserXattrsOnly  bool
}

type TraceProfile struct {
	Name string
}

type FeatureProfile struct {
	Stash          bool
	Dcache         bool
	Persistence    bool
	C2san          bool
	NetFailure     bool
	NodeCrash      bool
	ServerPrograms bool
}

var GlusterfsSubCalls = []string{
	"open", "read", "write", "close", "stat", "pread64", "pwrite64",
	"fsync", "fdatasync", "truncate", "getdents64", "rename", "mkdir",
	"rmdir", "creat", "unlink", "chmod", "link", "symlink", "readlink",
	"getxattr", "setxattr",
}

var GlusterfsRootCalls = []string{
	"open", "read", "write", "pread64", "pwrite64", "fsync", "fdatasync",
	"truncate", "mkdir", "rmdir", "creat", "unlink", "rename", "chmod",
	"stat", "getdents64", "link", "symlink", "setxattr",
}

var GlusterfsVariantCalls = append(append([]string{}, GlusterfsRootCalls...),
	"getxattr", "readlink")

var GlusterfsCausalRootCalls = []string{
	"open", "read", "write", "pread64", "pwrite64", "truncate", "stat",
	"chmod", "fsync", "fdatasync", "getdents64", "unlink", "mkdir",
	"creat", "rename", "link", "symlink", "setxattr",
}

var hmdfsProfile = &DFSProfile{
	Name: "hmdfs",
	Calls: CallProfile{
		SubCalls:        FsopsSubCalls,
		ChoiceCalls:     FsopsChoiceCalls,
		DirOnlyCalls:    DirOnlyCalls,
		FileOnlyCalls:   FileOnlyCalls,
		FileOrDirCalls:  FileOrDirCalls,
		FdRequiredCalls: FdRequiredCalls,
	},
	Events: EventProfile{SupportsSameInode: true},
	Paths:  PathPolicy{RootPrefix: MergeViewPrefix, SameParentRename: true},
	Tree:   TreeProfile{Prefix: MergeViewPrefix},
	Gen:    GenProfile{Stash: true, Dcache: true, Fsops: true, ClientOnly: false},
	Check:  CheckerProfile{SymlinkPath: true, FullType: false, WeakNlink: true, DiagnosticMtime: true, UserXattrsOnly: true},
	Trace:  TraceProfile{Name: "hmdfs"},
	Feature: FeatureProfile{
		Stash: true, Dcache: true, Persistence: true, C2san: false,
		NetFailure: true, NodeCrash: true, ServerPrograms: false,
	},
}

var glusterfsProfile = &DFSProfile{
	Name: "glusterfs",
	Calls: CallProfile{
		SubCalls:        GlusterfsSubCalls,
		ChoiceCalls:     GlusterfsSubCalls,
		DirOnlyCalls:    []string{"mkdir", "rmdir", "getdents64"},
		FileOnlyCalls:   []string{"creat", "truncate", "pread64", "pwrite64", "read", "write", "fsync", "fdatasync", "unlink"},
		FileOrDirCalls:  []string{"open", "stat", "chmod", "rename", "link", "symlink", "getxattr", "setxattr"},
		FdRequiredCalls: []string{"read", "write", "pread64", "pwrite64", "fsync", "fdatasync", "getdents64", "close"},
		RootCalls:       GlusterfsRootCalls,
		VariantCalls:    GlusterfsVariantCalls,
		CausalRootCalls: GlusterfsCausalRootCalls,
	},
	Events: EventProfile{SupportsSameInode: false},
	Paths:  PathPolicy{RootPrefix: "", SameParentRename: false, MaxClientNum: 3},
	Plan:   PlanProfile{DCTRootCalls: GlusterfsRootCalls, DCTVariantCalls: GlusterfsVariantCalls, CausalRootCalls: GlusterfsCausalRootCalls},
	Tree:   TreeProfile{Prefix: ""},
	Gen:    GenProfile{Stash: false, Dcache: false, Fsops: true, ClientOnly: true},
	Check:  CheckerProfile{SymlinkPath: true, FullType: true, WeakNlink: true, DiagnosticMtime: true, UserXattrsOnly: true},
	Trace:  TraceProfile{Name: "glusterfs"},
	Feature: FeatureProfile{
		Stash: false, Dcache: false, Persistence: false, C2san: false,
		NetFailure: false, NodeCrash: false, ServerPrograms: true,
	},
}

var dfsProfiles = map[string]*DFSProfile{
	"hmdfs":     hmdfsProfile,
	"glusterfs": glusterfsProfile,
}

func ProfileForName(name string) *DFSProfile {
	if p := dfsProfiles[name]; p != nil {
		return p
	}
	return nil
}

func (c *Hmdfs_config) ProfileOrDefault() *DFSProfile {
	if c != nil && c.Profile != nil {
		return c.Profile
	}
	if c != nil {
		if p := ProfileForName(c.DfsName); p != nil {
			return p
		}
	}
	return hmdfsProfile
}

func NewFileTreeForConfig(c *Hmdfs_config) *FileTree {
	prefix := MergeViewPrefix
	if c != nil {
		if c.Profile != nil {
			prefix = c.Profile.Tree.Prefix
		} else if c.RootPrefix != "" || c.DfsName == "glusterfs" {
			prefix = c.RootPrefix
		}
	}
	return NewFileTreeWithPrefix(prefix)
}

// NamespacePrefix returns the namespace root used by this config. An empty
// result is valid for GlusterFS; HMDFS and legacy zero-value configs use
// merge_view/.
func (c *Hmdfs_config) NamespacePrefix() string {
	if c == nil {
		return MergeViewPrefix
	}
	if c.Profile != nil {
		return c.Profile.Tree.Prefix
	}
	if c.DfsName == "glusterfs" {
		return ""
	}
	return MergeViewPrefix
}

// ClientView returns the client programs without mutating server slots.
func (c *Hmdfs_config) ClientView(ps []*Prog) []*Prog {
	if c == nil || c.Serv_num <= 0 || c.Serv_num >= len(ps) {
		return ps
	}
	return ps[c.Serv_num:]
}

// RootProg returns the first client program, or nil when malformed.
func (c *Hmdfs_config) RootProg(ps []*Prog) *Prog {
	view := c.ClientView(ps)
	if len(view) == 0 {
		return nil
	}
	return view[0]
}

func (c *Hmdfs_config) ClientProgIdx(clientIdx int) int {
	if c == nil {
		return clientIdx
	}
	return c.Serv_num + clientIdx
}

func (c *Hmdfs_config) ClientIdx(progIdx int) int {
	if c == nil {
		return progIdx
	}
	return progIdx - c.Serv_num
}

func (c *Hmdfs_config) IsDirOnlyCall(name string) bool {
	return profileCallListContains(c.ProfileOrDefault().Calls.DirOnlyCalls, name)
}

func (c *Hmdfs_config) IsFileOnlyCall(name string) bool {
	return profileCallListContains(c.ProfileOrDefault().Calls.FileOnlyCalls, name)
}

func (c *Hmdfs_config) IsFileOrDirCall(name string) bool {
	return profileCallListContains(c.ProfileOrDefault().Calls.FileOrDirCalls, name)
}

func profileCallListContains(list []string, name string) bool {
	for _, n := range list {
		if strings.Contains(name, n) {
			return true
		}
	}
	return false
}
