package prog

import (
	"math/rand"
	"testing"
)

// TestGlusterfsRootCallPartition pins the model-root vs directed-root split:
// every directed root must exist in the model/DCT root list, and the only
// current model-only root is rmdir. Adding a directed root later must update
// this test together with the planner and coverage tests.
func TestGlusterfsRootCallPartition(t *testing.T) {
	roots, _ := coverageBoardCallLists(glusterfsProfile)
	directed := glusterfsProfile.DirectedRootCalls()
	for _, root := range directed {
		if !profileCallListContains(roots, root) {
			t.Fatalf("directed root %q missing from model roots %v", root, roots)
		}
	}
	var modelOnly []string
	for _, root := range roots {
		if !profileCallListContains(directed, root) {
			modelOnly = append(modelOnly, root)
		}
	}
	if len(modelOnly) != 1 || modelOnly[0] != "rmdir" {
		t.Fatalf("model-only roots = %v, want [rmdir]", modelOnly)
	}
}

func TestCoverageBoardCallListsUseProfile(t *testing.T) {
	roots, variants := coverageBoardCallLists(glusterfsProfile)
	if len(roots) != len(GlusterfsRootCalls) {
		t.Fatalf("glusterfs roots = %d, want %d", len(roots), len(GlusterfsRootCalls))
	}
	if len(variants) != len(GlusterfsVariantCalls) {
		t.Fatalf("glusterfs variants = %d, want %d", len(variants), len(GlusterfsVariantCalls))
	}
	wantRoots := map[string]bool{"link": true, "symlink": true, "setxattr": true}
	for name := range wantRoots {
		found := false
		for _, r := range roots {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("glusterfs root list missing %q", name)
		}
	}
	wantVariants := map[string]bool{"link": true, "symlink": true, "readlink": true, "getxattr": true}
	for name := range wantVariants {
		found := false
		for _, v := range variants {
			if v == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("glusterfs variant list missing %q", name)
		}
	}

	roots, variants = coverageBoardCallLists(hmdfsProfile)
	if len(roots) != len(FsopsSubCalls) || len(variants) != len(FsopsSubCalls) {
		t.Fatalf("hmdfs lists = %d/%d, want %d/%d",
			len(roots), len(variants), len(FsopsSubCalls), len(FsopsSubCalls))
	}
}

func TestGlusterfsCallClassification(t *testing.T) {
	cfg := &Hmdfs_config{Profile: glusterfsProfile, DfsName: "glusterfs"}
	for _, name := range []string{"link", "symlink", "readlink"} {
		if !cfg.IsFileOnlyCall(name) {
			t.Fatalf("glusterfs %q is not file-only", name)
		}
		if got := callNameConstraint(name); got != relTargetFile {
			t.Fatalf("callNameConstraint(%q) = %d, want file", name, got)
		}
	}
	for _, name := range []string{"getxattr", "setxattr"} {
		if cfg.IsFileOnlyCall(name) || cfg.IsDirOnlyCall(name) {
			t.Fatalf("glusterfs %q should be file-or-dir", name)
		}
		if got := callNameConstraint(name); got != relTargetAny {
			t.Fatalf("callNameConstraint(%q) = %d, want any", name, got)
		}
	}
	if !cfg.IsDirOnlyCall("mkdir") || !cfg.IsDirOnlyCall("getdents64") {
		t.Fatalf("glusterfs directory-only classification regressed")
	}
}

func TestGlusterfsFuncClass(t *testing.T) {
	cases := map[string]uint32{
		"link":     FuncLink,
		"symlink":  FuncSymlink,
		"readlink": FuncReadlink,
		"getxattr": FuncGetxattr,
		"setxattr": FuncSetxattr,
	}
	for call, want := range cases {
		got, ok := callFuncClass(glusterfsProfile, call, false)
		if !ok || got != want {
			t.Fatalf("callFuncClass(glusterfs, %q) = %d,%v want %d,true", call, got, ok, want)
		}
		if _, ok := callFuncClass(hmdfsProfile, call, false); ok {
			t.Fatalf("callFuncClass(hmdfs, %q) unexpectedly known", call)
		}
	}
	if got, ok := callFuncClass(glusterfsProfile, "open", true); !ok || got != FuncDirOpen {
		t.Fatalf("glusterfs open(dir) = %d,%v", got, ok)
	}
	if got, ok := callFuncClass(glusterfsProfile, "open", false); !ok || got != FuncFileOpen {
		t.Fatalf("glusterfs open(file) = %d,%v", got, ok)
	}
}

func TestNormalizeGlusterfsClientProgs(t *testing.T) {
	mk := func() *Prog { return &Prog{Calls: []*Call{{}}} }
	ps := normalizeGlusterfsClientProgs([]*Prog{{}, {Calls: []*Call{{}}}}, 4, mk)
	if len(ps) != 4 {
		t.Fatalf("normalized count = %d, want 4", len(ps))
	}
	for i, p := range ps {
		if p == nil || len(p.Calls) == 0 {
			t.Fatalf("slot %d is empty after normalization", i)
		}
	}

	ps = normalizeGlusterfsClientProgs([]*Prog{
		{Calls: []*Call{{}}}, {Calls: []*Call{{}}}, {Calls: []*Call{{}}},
	}, 2, mk)
	if len(ps) != 2 {
		t.Fatalf("truncated count = %d, want 2", len(ps))
	}
}

func TestGlusterfsEmptyRootPath(t *testing.T) {
	cfg := &Hmdfs_config{Profile: glusterfsProfile, FileTree: NewFileTreeWithPrefix("")}
	if got := defaultTestDir(cfg); got != "" {
		t.Fatalf("defaultTestDir(glusterfs) = %q, want empty", got)
	}
	if got := joinTestPath("", "q0"); got != "q0" {
		t.Fatalf("joinTestPath(empty) = %q, want q0", got)
	}
	if got := joinTestPath("dir/", "q0"); got != "dir/q0" {
		t.Fatalf("joinTestPath(dir) = %q, want dir/q0", got)
	}
}

func TestGlusterfsOffsetAndRetModel(t *testing.T) {
	if isOffsetCall("readlink") {
		t.Fatalf("readlink must not carry offset buckets")
	}
	if !isOffsetCall("pread64") {
		t.Fatalf("pread64 must carry offset buckets")
	}
	rets := producibleRets("link")
	seen := map[RetBucket]bool{}
	for _, r := range rets {
		seen[r] = true
	}
	for _, want := range []RetBucket{RetSuccess, RetEEXIST, RetENOENT, RetFailure} {
		if !seen[want] {
			t.Fatalf("link ret model missing %v: %v", want, rets)
		}
	}
}

func TestGlusterfsProfileTargetConstraint(t *testing.T) {
	cases := map[string]int{
		"link":     relTargetFile,
		"symlink":  relTargetAny,
		"readlink": relTargetAny,
		"getxattr": relTargetAny,
		"setxattr": relTargetAny,
		"mkdir":    relTargetDir,
		"rmdir":    relTargetDir,
		"creat":    relTargetFile,
	}
	for call, want := range cases {
		if got := glusterfsProfile.TargetConstraint(call); got != want {
			t.Fatalf("glusterfs TargetConstraint(%q) = %d, want %d", call, got, want)
		}
	}

	_, _, _, lcs := seedOptLCS(t)
	filePath, dirPath := "merge_view/dirA/a", "merge_view/dirA"
	if glusterfsProfile.BasePathIsFile(lcs.FileTree, "link", filePath) != true {
		t.Fatal("link target must be file-intent")
	}
	if glusterfsProfile.BasePathIsFile(lcs.FileTree, "symlink", dirPath) {
		t.Fatal("symlink target may be a directory")
	}
	if !glusterfsProfile.BasePathIsFile(lcs.FileTree, "symlink", filePath) {
		t.Fatal("symlink target may be a file")
	}
	if glusterfsProfile.BasePathIsFile(lcs.FileTree, "rmdir", dirPath) {
		t.Fatal("rmdir must be dir-intent")
	}
	if !glusterfsProfile.BasePathIsFile(lcs.FileTree, "fsync", dirPath) {
		t.Fatal("glusterfs fsync is file-intent")
	}
	// HMDFS keeps the historical fallback: fsync has no type intent and the
	// tree decides.
	if hmdfsProfile.BasePathIsFile(lcs.FileTree, "fsync", dirPath) {
		t.Fatal("hmdfs fsync on a directory must fall back to the tree type")
	}
}

func TestGlusterfsDCTReadlinkFollower(t *testing.T) {
	dct := BuildDefaultDCT(glusterfsProfile)
	if dct.Profile != glusterfsProfile {
		t.Fatal("DCT did not retain its profile")
	}
	if len(dct.RootCalls) != len(GlusterfsRootCalls) {
		t.Fatalf("glusterfs roots = %d, want %d", len(dct.RootCalls), len(GlusterfsRootCalls))
	}
	for _, forbidden := range []string{"close", "readlink", "getxattr"} {
		if dct.RootCalls[forbidden] {
			t.Fatalf("%q must not be a GlusterFS root", forbidden)
		}
	}
	for _, root := range []string{"open", "link", "symlink"} {
		for _, cv := range dct.Variants[root] {
			if cv.CallName == "readlink" && root != "symlink" {
				t.Fatalf("readlink variant leaked under root %q", root)
			}
		}
	}
	for _, cv := range dct.Variants["symlink"] {
		if cv.CallName == "readlink" && cv.PathRelation != PathSame {
			t.Fatalf("readlink relation = %v, want Same", cv.PathRelation)
		}
	}

	// The runtime gate is independent of the table: even a stale/hand-built
	// DCT cannot produce readlink under a non-symlink root.
	stale := &DistributedChoiceTable{
		Profile:   glusterfsProfile,
		RootCalls: map[string]bool{"open": true},
		Variants: map[string][]CallVariant{
			"open": {{CallName: "readlink", PathRelation: PathSame}},
		},
		Weights: map[string]map[CallVariant]int{
			"open": {{CallName: "readlink", PathRelation: PathSame}: 30},
		},
	}
	if v := stale.ChooseVariant("open", rand.New(rand.NewSource(1))); v != nil {
		t.Fatalf("stale DCT produced variant %+v under open", v)
	}
}

func TestGlusterfsCoverageProfileRules(t *testing.T) {
	cases := []struct {
		call string
		want []RetBucket
	}{
		{"link", []RetBucket{RetSuccess}},
		{"symlink", []RetBucket{RetSuccess}},
		{"readlink", []RetBucket{RetSuccess}},
		{"getxattr", []RetBucket{RetSuccess, RetFailure}},
		{"setxattr", []RetBucket{RetSuccess, RetEEXIST}},
		{"rmdir", []RetBucket{RetSuccess, RetFailure}},
		{"rename", []RetBucket{RetSuccess, RetFailure}},
		{"unlink", []RetBucket{RetSuccess}},
	}
	for _, tc := range cases {
		got := producibleRetsFor(glusterfsProfile, tc.call)
		if len(got) != len(tc.want) {
			t.Fatalf("producibleRets(%q) = %v, want %v", tc.call, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("producibleRets(%q) = %v, want %v", tc.call, got, tc.want)
			}
		}
	}
	if got := producibleTypesFor(glusterfsProfile, "link"); len(got) != 1 || got[0] {
		t.Fatalf("link types = %v, want file only", got)
	}
	if got := producibleTypesFor(glusterfsProfile, "symlink"); len(got) != 2 {
		t.Fatalf("symlink types = %v, want file+dir", got)
	}
	if got := producibleTypesFor(glusterfsProfile, "readlink"); len(got) != 1 || got[0] {
		t.Fatalf("readlink types = %v, want non-dir", got)
	}
	if comboStaticallyUnreachableFor(glusterfsProfile, "symlink", "link", PathSame) {
		t.Fatal("symlink(dir) x link(Same) must stay reachable")
	}
	if !comboStaticallyUnreachableFor(glusterfsProfile, "link", "mkdir", PathSame) {
		t.Fatal("link(file) x mkdir(Same) must be statically unreachable")
	}
}

func TestGlusterfsReadlinkCoverageHBOnly(t *testing.T) {
	ft := glusterfsPlanTree()
	hmcfg := &Hmdfs_config{Profile: glusterfsProfile, DfsName: "glusterfs",
		Node_num: 3, Cids: hmdfsSmokeCids(), FileTree: ft}
	ResetCoverageBoard()
	InitCoverageBoard(hmcfg)
	t.Cleanup(ResetCoverageBoard)
	covBoard.mu.RLock()
	got := covBoard.computeProducibleLocked(comboKey{
		Root: "symlink", Variant: "readlink", Rel: PathSame,
	})
	covBoard.mu.RUnlock()
	if got[0] <= 0 {
		t.Fatalf("readlink HB producible = %d, want > 0", got[0])
	}
	if got[1] != 0 {
		t.Fatalf("readlink CC producible = %d, want 0", got[1])
	}
}

func TestGlusterfsConflictWeights(t *testing.T) {
	cases := []struct {
		root, variant string
		want          int
	}{
		{"link", "link", 150},
		{"link", "open", 120},
		{"link", "read", 100},      // namespace x observer
		{"setxattr", "write", 120}, // data-mod x data-mod
		{"readlink", "getxattr", 50},
	}
	for _, tc := range cases {
		if got := glusterfsProfile.ConflictCoeffOf(tc.root, tc.variant); got != tc.want {
			t.Fatalf("ConflictCoeffOf(%q,%q) = %d, want %d",
				tc.root, tc.variant, got, tc.want)
		}
	}
	if got := conflictCoeffOf("fsync", "read"); got != 120 {
		t.Fatalf("hmdfs fsync/read = %d, want 120", got)
	}
	if got := conflictCoeffOf("fsync", "fsync"); got != 150 {
		t.Fatalf("hmdfs fsync/fsync = %d, want 150", got)
	}
}

func TestGlusterfsResolveFdTargetPathCalls(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	link := mkLinkCall(t, target, "a", "b")
	p := &Prog{Target: target, Calls: []*Call{link}}
	pos := GroupPosition{ProgIdx: 0, CallIdx: 0}
	if got := resolveFdTarget([]*Prog{p}, pos); got != pos {
		t.Fatalf("resolveFdTarget(link) = %+v, want %+v", got, pos)
	}
}
