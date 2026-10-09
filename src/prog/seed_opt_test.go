package prog

import (
	"fmt"
	"math/rand"
	"strings"
	"syscall"
	"testing"
)

// Tests for the seed-construction optimization batch: bias safety on the
// merged choice tables, depth-bucket selection,
// the initial-weight formula and the failure-shape whitelist. They require a
// linux target (GetTarget("linux", "amd64")) and are meant to run on Linux.

func seedOptFindSyscallID(target *Target, name string) int {
	for id, sc := range target.Syscalls {
		if sc.Name == name {
			return id
		}
	}
	return -1
}

func seedOptLCS(t *testing.T) (*Target, *Hmdfs_config, *SpecialCalls, *LayeredChoiceStrategy) {
	t.Helper()
	target := hmdfsSmokeTarget(t)
	hmcfg := &Hmdfs_config{}
	hmdfsSmokeConfig(hmcfg)
	sc := hmdfsSmokeSpecialCalls(t, target)
	lcs := NewLayeredChoiceStrategy(hmcfg, target)
	return target, hmcfg, sc, lcs
}

// TestChooseRootCallNameBiasSafety pins the merged-table close regression:
// ChoiceTable.choose panics on a disabled bias, so biases sampled from
// arbitrary program calls (close, syz_*, variants) must be sanitized.
func TestChooseRootCallNameBiasSafety(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	r := newRand(target, rand.NewSource(1))

	closeID := seedOptFindSyscallID(target, "close")
	if closeID < 0 {
		t.Skip("target has no close")
	}
	// close is enabled in FsopsChoiceTable: safe, and never a root result.
	for i := 0; i < 200; i++ {
		if name := r.chooseRootCallName(lcs.FsopsChoiceTable, closeID); name == "close" {
			t.Fatalf("close returned as root")
		}
	}
	// Calls outside any sub table must be sanitized as well. (The former
	// close-disabled second table went away with the reservoir insertion.)
	for _, name := range []string{"syz_failure_sync", "open$dir"} {
		id := seedOptFindSyscallID(target, name)
		if id < 0 {
			continue
		}
		for i := 0; i < 200; i++ {
			r.chooseRootCallName(lcs.FsopsChoiceTable, id)
		}
	}
}

// TestInsertCallFromDCTCloseBiasNoPanic runs the DCT insertion path on programs
// whose calls include a close (the bias source that used to panic).
func TestInsertCallFromDCTCloseBiasNoPanic(t *testing.T) {
	target, hmcfg, sc, lcs := seedOptLCS(t)
	// Direction 3 reads the global board; reset it so the sweep is
	// deterministic and exercises the static DCT path only.
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)

	mkBase := func() []*Prog {
		ps := make([]*Prog, 0, 3)
		for i := 0; i < 3; i++ {
			o := mkOpen(target, "merge_view/dirA/a")
			c := mkClose(target, o)
			ps = append(ps, &Prog{Target: target, Calls: []*Call{o, c}})
		}
		return ps
	}
	okCount := 0
	for seed := int64(0); seed < 300; seed++ {
		ps := mkBase()
		r := newRand(target, rand.NewSource(seed))
		if insertCallFromDCT(ps, r, sc, hmcfg, lcs, true) {
			okCount++
		}
		for _, p := range ps {
			if p.HasBrokenRefs() {
				t.Fatalf("seed %d: broken refs after insertCallFromDCT (last mutation %q):\n%s",
					seed, LastMutation(), p.DumpRefDiagnosis())
			}
			assertSerializeSafe(t, fmt.Sprintf("seed %d", seed), p)
		}
	}
	if okCount == 0 {
		t.Fatal("insertCallFromDCT never succeeded across 300 seeds")
	}
	t.Logf("insertCallFromDCT succeeded for %d/300 seeds", okCount)
}

// TestRootBaseConstraint pins the F1/F2 root-kind table: which roots carry a
// tree-type constraint on their base path, and which node types satisfy it.
func TestRootBaseConstraint(t *testing.T) {
	cases := []struct {
		call string
		kind int
		ok   bool
	}{
		{"mkdir", relTargetAny, false},
		{"creat", relTargetAny, false},
		{"truncate", relTargetFile, true},
		{"unlink", relTargetFile, true},
		{"read", relTargetFile, true},
		{"write", relTargetFile, true},
		{"pread64", relTargetFile, true},
		{"pwrite64", relTargetFile, true},
		{"fsync", relTargetFile, true},
		{"fdatasync", relTargetFile, true},
		{"rmdir", relTargetDir, true},
		{"getdents64", relTargetDir, true},
		{"open", relTargetAny, false},
		{"stat", relTargetAny, false},
		{"chmod", relTargetAny, false},
		{"rename", relTargetAny, false},
	}
	for _, c := range cases {
		kind, ok := rootBaseConstraint(c.call)
		if kind != c.kind || ok != c.ok {
			t.Errorf("rootBaseConstraint(%q) = (%v, %v), want (%v, %v)",
				c.call, kind, ok, c.kind, c.ok)
		}
	}

	nodes := []struct {
		name string
		n    *FileNode
		file bool
		dir  bool
	}{
		{"file", &FileNode{Type: NodeTypeFile}, true, false},
		{"dir", &FileNode{Type: NodeTypeDir}, false, true},
		{"emptydir", &FileNode{Type: NodeTypeEmptyDir}, false, true},
		// insertCallFromDCT's local dir convention excludes TmpDir (matches
		// the O_DIRECTORY/root-container checks in the same file).
		{"tmpdir", &FileNode{Type: NodeTypeTmpDir}, false, false},
	}
	for _, c := range nodes {
		if got := nodeKindMatches(c.n, relTargetFile); got != c.file {
			t.Errorf("nodeKindMatches(%s, file) = %v, want %v", c.name, got, c.file)
		}
		if got := nodeKindMatches(c.n, relTargetDir); got != c.dir {
			t.Errorf("nodeKindMatches(%s, dir) = %v, want %v", c.name, got, c.dir)
		}
		if !nodeKindMatches(c.n, relTargetAny) {
			t.Errorf("nodeKindMatches(%s, any) = false, want true", c.name)
		}
	}
}

// TestNodeIsDir pins the physical directory predicate used by the open-flag
// and fd-classification layers: anything that is not a file (Dir/EmptyDir/
// TmpDir) is a directory. TmpDir is deliberately included here even though
// the root-base policy (nodeKindMatches) excludes it.
func TestNodeIsDir(t *testing.T) {
	cases := []struct {
		name string
		n    *FileNode
		want bool
	}{
		{"nil", nil, false},
		{"file", &FileNode{Type: NodeTypeFile}, false},
		{"dir", &FileNode{Type: NodeTypeDir}, true},
		{"emptydir", &FileNode{Type: NodeTypeEmptyDir}, true},
		{"tmpdir", &FileNode{Type: NodeTypeTmpDir}, true},
	}
	for _, c := range cases {
		if got := nodeIsDir(c.n); got != c.want {
			t.Errorf("nodeIsDir(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPatternOpenFlagsTmpDir pins nodeIsDir in the pattern op flag layer: an
// open op on a TmpDir (a real directory) must carry O_DIRECTORY like Dir and
// EmptyDir, otherwise the open fails EISDIR at the VFS before the hmdfs trace.
func TestPatternOpenFlagsTmpDir(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	sc := hmdfsSmokeSpecialCalls(t, target)
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir:    map[string][]string{"c1": {"merge_view/dirA"}},
		Init_file:   map[string][]string{"c1": {"merge_view/dirA/a"}},
		Init_tmpdir: map[string][]string{"c1": {"merge_view/tmpdir"}},
	})
	lcs := &LayeredChoiceStrategy{FileTree: ft}
	r := newRand(target, rand.NewSource(1))
	s := newState(target, nil, nil)
	op := ConcurrentOp{CallName: "open", PathArgs: []PathArgSpec{{PathSelf}}}
	for _, tc := range []struct {
		path string
		dir  bool
	}{
		{"merge_view/tmpdir", true},
		{"merge_view/dirA", true},
		{"merge_view/dirA/a", false},
	} {
		calls, _, _ := r.generateCallFromPatternOp(s, sc, op, tc.path, "c1", lcs, nil, false, OffsetUnspecified, 0)
		if len(calls) == 0 || calls[0].Meta.CallName != "open" {
			t.Fatalf("%s: got %d calls, want an open", tc.path, len(calls))
		}
		if got := isOpenDirectory(calls[0]); got != tc.dir {
			t.Errorf("%s: open O_DIRECTORY = %v, want %v", tc.path, got, tc.dir)
		}
	}
}

// TestInsertCallFromDCTRootKindRepick pins F2: a file-only root must not keep
// a directory candidate as its base. The base program holds a single path
// candidate (a directory), so without the re-pick a file-only root would land
// on it (EISDIR at execution); with F2 the base falls back to the type-aware
// FileTree selection and no inserted file-only call targets a non-file node.
func TestInsertCallFromDCTRootKindRepick(t *testing.T) {
	target, hmcfg, sc, lcs := seedOptLCS(t)
	// Direction 3 would add board-dependent randomization; keep the static
	// DCT path so the sweep is deterministic.
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)

	const baseDir = "merge_view/dirA"
	if n := lcs.FileTree.FindNode(baseDir); n == nil || n.Type != NodeTypeDir {
		t.Fatalf("smoke tree lost dir %q", baseDir)
	}

	s := newState(target, nil, nil)
	insertedFileOnly := 0
	for seed := int64(0); seed < 300; seed++ {
		r := newRand(target, rand.NewSource(seed))
		base := r.fallbackStatNode(s, sc, baseDir)
		if base == nil {
			t.Fatalf("seed %d: stat fallback unavailable", seed)
		}
		orig := base.Calls[0]
		ps := []*Prog{base}
		insertCallFromDCT(ps, r, sc, hmcfg, lcs, true)
		for _, c := range ps[0].Calls {
			if c == orig || c == nil || !isFileOnlyCallByName(c.Meta.Name) {
				continue
			}
			p := extractPathFromCall(c)
			if p == "" {
				continue
			}
			insertedFileOnly++
			if n := lcs.FileTree.FindNode(p); n != nil && n.Type != NodeTypeFile {
				t.Fatalf("seed %d: file-only call %q targeted non-file node %q (root re-pick failed)",
					seed, c.Meta.Name, p)
			}
		}
	}
	if insertedFileOnly == 0 {
		t.Fatal("no inserted file-only call across 300 seeds: property test is vacuous")
	}
}

// TestFreshPathKindsBefore pins the in-program creation hint used by the root
// type re-pick: mkdir paths are directories, creat paths are files, a rename
// destination inherits the source's type (through the hint first, the tree
// second), only the calls before the root slot count, and a path created by
// both kinds is treated as unknown.
func TestFreshPathKindsBefore(t *testing.T) {
	target, hmcfg, sc, lcs := seedOptLCS(t)
	r := newRand(target, rand.NewSource(1))
	s := newState(target, nil, nil)
	cid := hmcfg.Cids[0]

	const dirPath = "merge_view/dirA/mut_dir_hint"
	const filePath = "merge_view/dirA/fresh_hint.txt"
	mk := r.generateCallByName(s, sc, "mkdir", dirPath, "", cid, nil, false, 0, 0)
	cr := r.generateCallByName(s, sc, "creat", filePath, "", cid, nil, false, 0, 0)
	if len(mk) == 0 || len(cr) == 0 {
		t.Fatalf("fixture generation failed: mkdir=%d creat=%d", len(mk), len(cr))
	}
	p := &Prog{Target: target, Calls: append(append([]*Call{}, mk...), cr...)}

	if got := freshPathKindsBefore(p, lcs.FileTree, 0); len(got) != 0 {
		t.Fatalf("end=0: got %v, want empty", got)
	}
	if got := freshPathKindsBefore(p, lcs.FileTree, 1); len(got) != 1 || got[dirPath] != relTargetDir {
		t.Fatalf("end=1: got %v, want {%s: dir}", got, dirPath)
	}
	got := freshPathKindsBefore(p, lcs.FileTree, len(p.Calls))
	if got[dirPath] != relTargetDir || got[filePath] != relTargetFile {
		t.Fatalf("full scan mismatch: %v", got)
	}

	// A second creat on the mkdir path makes the type ambiguous: unknown.
	conflict := r.generateCallByName(s, sc, "creat", dirPath, "", cid, nil, false, 0, 0)
	if len(conflict) == 0 {
		t.Fatal("fixture conflict creat generation failed")
	}
	pc := &Prog{Target: target, Calls: append(append([]*Call{}, mk...), conflict...)}
	if k, ok := freshPathKindsBefore(pc, lcs.FileTree, len(pc.Calls))[dirPath]; ok {
		t.Fatalf("conflicting creators must be unknown, got kind %d", k)
	}

	// rename destinations inherit the source type: a tree file source, then a
	// chained fresh source; an unknown source leaves the destination out.
	const renameSrc = "merge_view/dirA/a"
	const renameDst1 = "merge_view/dirA/a._renamed_hint1"
	const renameDst2 = "merge_view/dirA/a._renamed_hint1._renamed_hint2"
	const unknownDst = "merge_view/no_such_src._renamed_hint"
	rn1 := r.generateRenameCallWithPaths(s, sc, renameSrc, renameDst1)
	rn2 := r.generateRenameCallWithPaths(s, sc, renameDst1, renameDst2)
	rn3 := r.generateRenameCallWithPaths(s, sc, "merge_view/no_such_src", unknownDst)
	if rn1 == nil || rn2 == nil || rn3 == nil {
		t.Fatal("fixture rename generation failed")
	}
	pr := &Prog{Target: target, Calls: []*Call{rn1, rn2, rn3}}
	gotR := freshPathKindsBefore(pr, lcs.FileTree, len(pr.Calls))
	if gotR[renameDst1] != relTargetFile || gotR[renameDst2] != relTargetFile {
		t.Fatalf("rename destination type missing: %v", gotR)
	}
	if _, ok := gotR[unknownDst]; ok {
		t.Fatalf("unknown rename source must leave the destination out: %v", gotR)
	}
}

// TestBasePathIsFileHinted pins the hint override used by the variant picker
// and the fresh-target open flags: a hinted type wins over the ambiguous-root
// fallback, which otherwise treats an unknown path as a file.
func TestBasePathIsFileHinted(t *testing.T) {
	_, _, _, lcs := seedOptLCS(t)
	const fresh = "merge_view/dirA/fresh_unknown"
	if lcs.FileTree.FindNode(fresh) != nil {
		t.Fatalf("fixture path %q unexpectedly in the tree", fresh)
	}
	if got := basePathIsFileHinted(lcs.FileTree, "open", fresh, map[string]int{fresh: relTargetDir}); got {
		t.Fatal("hinted directory must not be treated as a file")
	}
	if got := basePathIsFileHinted(lcs.FileTree, "open", fresh, map[string]int{fresh: relTargetFile}); !got {
		t.Fatal("hinted file must be treated as a file")
	}
	if got := basePathIsFileHinted(lcs.FileTree, "open", fresh, nil); !got {
		t.Fatal("no hint: ambiguous root must keep the unknown-path file fallback")
	}
	if got := basePathIsFileHinted(lcs.FileTree, "mkdir", fresh, nil); got {
		t.Fatal("no hint: directory-intent root must stay a directory")
	}
}

// TestPickByBucketWeights checks that a single non-zero bucket weight selects
// only files of that depth bucket, and that an empty selected bucket
// renormalizes to the non-empty ones.
func TestPickByBucketWeights(t *testing.T) {
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{"c1": {
			"merge_view/d1",
			"merge_view/d1/d2/d3",
			"merge_view/d1/d2/d3/d4/d5/d6",
		}},
		Init_file: map[string][]string{"c1": {
			"merge_view/f1",
			"merge_view/d1/d2/f3",
			"merge_view/d1/d2/d3/d4/d5/f6",
		}},
	})
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 100; i++ {
		n := ft.SelectFile(r, "c1", [3]float64{1, 0, 0})
		if n == nil || DepthBucketOf(n.FullPath) != 1 {
			t.Fatalf("weights {1,0,0} picked bucket != 1: %v", n)
		}
	}
	for i := 0; i < 100; i++ {
		n := ft.SelectFile(r, "c1", [3]float64{0, 1, 0})
		if n == nil || DepthBucketOf(n.FullPath) != 2 {
			t.Fatalf("weights {0,1,0} picked bucket != 2: %v", n)
		}
	}
	for i := 0; i < 100; i++ {
		n := ft.SelectFile(r, "c1", [3]float64{0, 0, 1})
		if n == nil || DepthBucketOf(n.FullPath) != 3 {
			t.Fatalf("weights {0,0,1} picked bucket != 3: %v", n)
		}
	}

	// Only a bucket-2 file exists: weight on bucket 1 must renormalize.
	ft2 := NewFileTree()
	ft2.InitFromHmdfsConfig(&Hmdfs_config{
		Init_file: map[string][]string{"c1": {"merge_view/a/b/f"}},
	})
	n := ft2.SelectFile(r, "c1", [3]float64{1, 0, 0})
	if n == nil || DepthBucketOf(n.FullPath) != 2 {
		t.Fatalf("empty selected bucket did not renormalize: %v", n)
	}
}

// TestInitialWeightFormula pins representative values of the formula-based
// initial DCT weights (relBase x conflictCoeff, clamped to [5,45]).
func TestInitialWeightFormula(t *testing.T) {
	dct := NewDistributedChoiceTable()
	cases := []struct {
		root, variant string
		rel           PathRelation
		want          int
	}{
		{"mkdir", "rmdir", PathSame, 45},             // ns x ns 150%, Same 30
		{"stat", "getdents64", PathNoRel, 5},         // obs x obs 50%, NoRel 10
		{"write", "truncate", PathSame, 45},          // dm x dm (special) 150%, Same 30
		{"mkdir", "stat", PathSame, 30},              // ns x obs 100%, Same 30
		{"rename", "stat", PathNoRel, 10},            // ns x obs 100%, NoRel 10
		{"getdents64", "getdents64", PathParent, 30}, // same-call 150%, Parent 20
		{"open", "write", PathSibling, 24},           // open-involving 120%, Sibling 20
	}
	for _, c := range cases {
		got := dct.getInitialWeight(c.root, CallVariant{CallName: c.variant, PathRelation: c.rel})
		if got != c.want {
			t.Errorf("getInitialWeight(%s,%s,%v) = %d, want %d", c.root, c.variant, c.rel, got, c.want)
		}
	}
}

// TestSyncFileTreeOwnerChangeKeepsTypeList pins the SyncFileTreeFromFsMd
// owner-only bug: re-owning a node must re-link it into the new owner's
// FilesByCid/DirsByCid list. The old code tested oldNode.OwnerCid after
// overwriting it, so a pure owner change dropped the node from every type
// list (invisible to list-based selectors); the condition is now captured
// before the assignment.
func TestSyncFileTreeOwnerChangeKeepsTypeList(t *testing.T) {
	hmcfg := &Hmdfs_config{
		Cids:     []string{"c1", "c2", "c3"},
		Node_num: 3,
		Init_dir: map[string][]string{"c1": {"merge_view/dirA"}},
		Init_file: map[string][]string{
			"c1": {"merge_view/dirA/a"},
		},
	}
	hmcfg.FileTree = NewFileTree()
	hmcfg.FileTree.InitFromHmdfsConfig(hmcfg)

	node := hmcfg.FileTree.FindNode("merge_view/dirA/a")
	if node == nil || node.OwnerCid != "c1" {
		t.Fatalf("setup: node not in tree with owner c1")
	}

	fsMd := map[string]FileMetadata{
		"./dirA":   {StatMd: syscall.Stat_t{Mode: syscall.S_IFDIR | 0o755}},
		"./dirA/a": {StatMd: syscall.Stat_t{Mode: syscall.S_IFREG | 0o644, Size: 5}},
	}
	ownerMap := map[string]string{"merge_view/dirA/a": "c2"}
	SyncFileTreeFromFsMd(fsMd, ownerMap, hmcfg)

	node = hmcfg.FileTree.FindNode("merge_view/dirA/a")
	if node == nil {
		t.Fatalf("node lost after sync")
	}
	if node.OwnerCid != "c2" {
		t.Fatalf("owner not updated: %q", node.OwnerCid)
	}
	if !containsNode(hmcfg.FileTree.FilesByCid["c2"], node) {
		t.Fatalf("node missing from FilesByCid[c2] after owner-only change")
	}
	if containsNode(hmcfg.FileTree.FilesByCid["c1"], node) {
		t.Fatalf("node still listed under the old owner c1")
	}
	if got := hmcfg.FileTree.GetFileEntriesUnderDir("merge_view/dirA"); len(got) != 1 || got[0] != "merge_view/dirA/a" {
		t.Fatalf("GetFileEntriesUnderDir after sync = %v", got)
	}
	if n := hmcfg.FileTree.SelectFile(rand.New(rand.NewSource(1)), "c2", [3]float64{0, 1, 0}); n == nil || n.FullPath != "merge_view/dirA/a" {
		t.Fatalf("SelectFile(c2) after re-own = %v", n)
	}
}

// TestApplyFailureShape checks the reachable-failure constructions: rmdir on a
// non-empty dir, and a dir -> non-empty dir rename with no ancestor relation;
// other roots stay untouched.
func TestApplyFailureShape(t *testing.T) {
	target, _, _, lcs := seedOptLCS(t)
	r := newRand(target, rand.NewSource(3))

	for i := 0; i < 100; i++ {
		p, p2 := applyFailureShape(lcs, "rmdir", "merge_view/whatever", "", r)
		node := lcs.FileTree.FindNode(p)
		if node == nil || node.Type != NodeTypeDir || len(node.Children) == 0 {
			t.Fatalf("rmdir shape target not a non-empty dir: %q", p)
		}
		if p2 != "" {
			t.Fatalf("rmdir shape changed basePath2: %q", p2)
		}
	}
	for i := 0; i < 100; i++ {
		src, dst := applyFailureShape(lcs, "rename", "merge_view/x", "merge_view/y", r)
		sn := lcs.FileTree.FindNode(src)
		dn := lcs.FileTree.FindNode(dst)
		if sn == nil || dn == nil || dn.Type != NodeTypeDir || len(dn.Children) == 0 {
			t.Fatalf("rename shape dst not a non-empty dir: src=%q dst=%q", src, dst)
		}
		if src == dst || strings.HasPrefix(dst, src+"/") || strings.HasPrefix(src, dst+"/") {
			t.Fatalf("rename shape has ancestor relation: src=%q dst=%q", src, dst)
		}
	}
	if a, b := applyFailureShape(lcs, "stat", "merge_view/p", "", r); a != "merge_view/p" || b != "" {
		t.Fatalf("non-whitelisted root was rewritten: %q %q", a, b)
	}
}

// TestPathRelationExcludesMountRoot pins the root-parent guard: a depth-1
// base's Parent relation would be the merge-view mount root, which VFS
// rejects (EBUSY on rename/rmdir). The guard must not spill over to the
// Sibling/NoRel relations, which stay valid for fresh depth-1 paths.
func TestPathRelationExcludesMountRoot(t *testing.T) {
	lcs := &LayeredChoiceStrategy{FileTree: hmdfsSmokeFileTree()}
	r := rand.New(rand.NewSource(1))

	if got := lcs.FileTree.GetPathByRelation("merge_view/dirA", "", PathParent, r, false, relTargetAny); got != "" {
		t.Errorf("existing depth-1 Parent = %q, want empty", got)
	}
	if got := lcs.FileTree.GetPathByRelation("merge_view/dirA", "", PathParent, r, false, relTargetDir); got != "" {
		t.Errorf("existing depth-1 dir Parent = %q, want empty", got)
	}
	if src, dst := lcs.GetPathsForRenameVariant("merge_view/dirA", "", PathParent, r, false); src != "" || dst != "" {
		t.Errorf("depth-1 rename Parent = %q/%q, want empty", src, dst)
	}
	if got := lcs.FileTree.GetPathByRelation("merge_view/dirA/a", "", PathParent, r, false, relTargetDir); got != "merge_view/dirA" {
		t.Errorf("depth-2 Parent = %q, want merge_view/dirA", got)
	}

	// Fresh depth-1 path (parent is the root): Parent is illegal, but Sibling
	// (the other depth-1 nodes) must stay available.
	if got := lcs.FileTree.GetPathByRelation("merge_view/fresh_x", "", PathParent, r, false, relTargetAny); got != "" {
		t.Errorf("fresh depth-1 Parent = %q, want empty", got)
	}
	if got := lcs.FileTree.GetPathByRelation("merge_view/fresh_x", "", PathSibling, r, false, relTargetAny); got == "" {
		t.Errorf("fresh depth-1 Sibling must stay available")
	}

	relOK, _ := lcs.availableRelationsForBase("merge_view/dirA")
	if relOK[relTargetAny][PathParent] || relOK[relTargetDir][PathParent] {
		t.Errorf("availableRelationsForBase keeps Parent at a depth-1 base")
	}
	relOKFresh, _ := lcs.availableRelationsForBase("merge_view/fresh_x")
	if relOKFresh[relTargetAny][PathParent] || relOKFresh[relTargetDir][PathParent] {
		t.Errorf("availableRelationsForBase keeps Parent for a fresh depth-1 base")
	}
	if !relOKFresh[relTargetAny][PathSibling] {
		t.Errorf("availableRelationsForBase drops Sibling for a fresh depth-1 base")
	}
}

// TestPatternRenamePathArgs pins the rename spec shape: a rename op carries
// two path slots, [0] the source relation (resolved against the pattern base)
// and [1] the destination relation (resolved against the source, restricted
// to SelfTwo/Sibling). It also pins the per-pattern intent: multi_rename's
// root client keeps the SelfTwo pair token (which resolves to the base) while
// the two followers share the same source via Same; rename_rename has a
// Sibling source (two distinct files).
func TestPatternRenamePathArgs(t *testing.T) {
	renames, sameFileOps, twoFileSrc := 0, 0, 0
	var multiSrc []PathRelation
	for _, pattern := range NewPredefinedPatterns().Patterns {
		for _, ops := range pattern.Operations {
			for _, op := range ops {
				if op.CallName != "rename" {
					continue
				}
				renames++
				if len(op.PathArgs) != 2 {
					t.Fatalf("pattern %s: rename has %d path args, want 2 (src, dst)",
						pattern.Name, len(op.PathArgs))
				}
				if dst := op.PathArgs[1].Relation; dst != PathSelfTwo && dst != PathSibling && dst != PathSame {
					t.Errorf("pattern %s: rename dst relation = %v, want SelfTwo, Sibling or Same", pattern.Name, dst)
				}
				switch pattern.Name {
				case "multi_rename_same_file":
					sameFileOps++
					multiSrc = append(multiSrc, op.PathArgs[0].Relation)
				case "rename_rename_concurrent":
					if op.PathArgs[0].Relation == PathSibling {
						twoFileSrc++
						if op.PathArgs[1].Relation != PathSame {
							t.Errorf("rename_rename_concurrent: sibling-source rename dst = %v, want Same (deterministic crossing onto the base)",
								op.PathArgs[1].Relation)
						}
					}
				}
			}
		}
	}
	if renames == 0 {
		t.Fatal("no rename ops in predefined patterns")
	}
	if sameFileOps == 0 {
		t.Error("multi_rename_same_file has no rename ops")
	}
	wantMultiSrc := []PathRelation{PathSelfTwo, PathSame, PathSame}
	if len(multiSrc) != len(wantMultiSrc) {
		t.Fatalf("multi_rename_same_file src tokens = %v, want %v", multiSrc, wantMultiSrc)
	}
	for i, rel := range wantMultiSrc {
		if multiSrc[i] != rel {
			t.Fatalf("multi_rename_same_file src tokens = %v, want %v", multiSrc, wantMultiSrc)
		}
	}
	if twoFileSrc == 0 {
		t.Error("rename_rename_concurrent has no sibling-source rename (two_files differentiation missing)")
	}
}

// TestPatternClientCountMatchesOps pins the declarative client count to the
// number of per-client operation groups. The Node_num gate uses
// len(Operations), so a stale ClientCount would otherwise go unnoticed (a
// pattern with more groups than Node_num must be skipped whole, never
// generated with its trailing groups silently dropped).
func TestPatternClientCountMatchesOps(t *testing.T) {
	for _, pattern := range NewPredefinedPatterns().Patterns {
		if pattern.ClientCount != len(pattern.Operations) {
			t.Errorf("pattern %s: ClientCount=%d, len(Operations)=%d",
				pattern.Name, pattern.ClientCount, len(pattern.Operations))
		}
	}
}

// TestPatternRenameKeepsFreshDst pins the SelfTwo pair token and the fresh
// destination: {SelfTwo},{SelfTwo} resolves [0] to the base and [1] to the
// pair's derived second path — a fresh same-parent name even on a base with no
// existing siblings (never a stat keeper).
func TestPatternRenameKeepsFreshDst(t *testing.T) {
	target, hmcfg, sc, _ := seedOptLCS(t)
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{Init_dir: map[string][]string{"c1": {"merge_view/only"}}})
	lcs := &LayeredChoiceStrategy{FileTree: ft}
	s := newState(target, nil, nil)

	op := ConcurrentOp{CallName: "rename", PathArgs: []PathArgSpec{{PathSelfTwo}, {PathSelfTwo}}}
	const base = "merge_view/only"
	for seed := int64(0); seed < 20; seed++ {
		r := newRand(target, rand.NewSource(seed))
		calls, _, _ := r.generateCallFromPatternOp(s, sc, op, base, hmcfg.Cids[0], lcs, nil, false, OffsetUnspecified, 0)
		if len(calls) == 0 || calls[0].Meta.CallName != "rename" {
			t.Fatalf("seed %d: got %d calls, want a rename", seed, len(calls))
		}
		oldPath := extractPathFromCallByArgIdx(calls[0], 0)
		newPath := extractPathFromCallByArgIdx(calls[0], 1)
		if oldPath != base {
			t.Errorf("seed %d: src = %q, want %q", seed, oldPath, base)
		}
		if newPath == "" || GetParentDir(newPath) != GetParentDir(base) {
			t.Errorf("seed %d: dst = %q, want a fresh same-parent name", seed, newPath)
		}
	}
}

// TestPatternRenameTwoSlotSemantics pins the two-slot resolution: [0] against
// the pattern base, [1] against the resolved source. Same/Sibling sources and
// the SelfTwo/Sibling/Same destinations are exercised, plus the feasibility
// table (Same destination = the base, only realizable from a sibling source).
func TestPatternRenameTwoSlotSemantics(t *testing.T) {
	target, hmcfg, sc, lcs := seedOptLCS(t)
	s := newState(target, nil, nil)
	const base = "merge_view/dirA"

	// [0]=Same, [1]=Sibling: src=base, dst same-parent (existing sibling or
	// fresh via pickRenameDst).
	sameSib := ConcurrentOp{CallName: "rename", PathArgs: []PathArgSpec{{PathSame}, {PathSibling}}}
	for seed := int64(0); seed < 20; seed++ {
		r := newRand(target, rand.NewSource(seed))
		calls, _, _ := r.generateCallFromPatternOp(s, sc, sameSib, base, hmcfg.Cids[0], lcs, nil, false, OffsetUnspecified, 0)
		if len(calls) == 0 || calls[0].Meta.CallName != "rename" {
			t.Fatalf("Same/Sibling seed %d: got %d calls, want a rename", seed, len(calls))
		}
		if oldPath := extractPathFromCallByArgIdx(calls[0], 0); oldPath != base {
			t.Errorf("Same/Sibling seed %d: src = %q, want %q", seed, oldPath, base)
		}
		newPath := extractPathFromCallByArgIdx(calls[0], 1)
		if newPath == "" || GetParentDir(newPath) != GetParentDir(base) {
			t.Errorf("Same/Sibling seed %d: dst = %q, want a same-parent name", seed, newPath)
		}
	}

	// [0]=Sibling, [1]=Sibling: src is an existing sibling of base, dst stays
	// in that sibling's parent (the two_files form).
	twoFile := ConcurrentOp{CallName: "rename", PathArgs: []PathArgSpec{{PathSibling}, {PathSibling}}}
	r := newRand(target, rand.NewSource(1))
	calls, _, _ := r.generateCallFromPatternOp(s, sc, twoFile, base, hmcfg.Cids[0], lcs, nil, false, OffsetUnspecified, 0)
	if len(calls) == 0 || calls[0].Meta.CallName != "rename" {
		t.Fatalf("Sibling/Sibling: got %d calls, want a rename", len(calls))
	}
	oldPath := extractPathFromCallByArgIdx(calls[0], 0)
	newPath := extractPathFromCallByArgIdx(calls[0], 1)
	if oldPath == "" || oldPath == base || GetParentDir(oldPath) != GetParentDir(base) {
		t.Errorf("Sibling/Sibling: src = %q, want an existing same-parent sibling", oldPath)
	}
	if newPath == "" || GetParentDir(newPath) != GetParentDir(oldPath) {
		t.Errorf("Sibling/Sibling: dst = %q, want a same-parent name of the source", newPath)
	}

	// [0]=Sibling, [1]=Same: deterministic crossing onto the base — src is a
	// same-class non-TmpDir sibling (dirB), dst is the base itself.
	ontoBase := ConcurrentOp{CallName: "rename", PathArgs: []PathArgSpec{{PathSibling}, {PathSame}}}
	r3 := newRand(target, rand.NewSource(3))
	bcalls, _, _ := r3.generateCallFromPatternOp(s, sc, ontoBase, base, hmcfg.Cids[0], lcs, nil, false, OffsetUnspecified, 0)
	if len(bcalls) == 0 || bcalls[0].Meta.CallName != "rename" {
		t.Fatalf("Sibling/Same: got %d calls, want a rename", len(bcalls))
	}
	if old := extractPathFromCallByArgIdx(bcalls[0], 0); old != "merge_view/dirB" {
		t.Errorf("Sibling/Same: src = %q, want the same-class sibling merge_view/dirB", old)
	}
	if dst := extractPathFromCallByArgIdx(bcalls[0], 1); dst != base {
		t.Errorf("Sibling/Same: dst = %q, want the base %q", dst, base)
	}

	// Feasibility: dst must be SelfTwo/Sibling, or Same from a sibling source;
	// a structural source needs availability (dirA has the dirB sibling).
	feas := func(args ...PathArgSpec) bool {
		return patternOpsFeasible(lcs, &ConcurrentPattern{Operations: [][]ConcurrentOp{{
			{CallName: "rename", PathArgs: args}}}}, base)
	}
	if !feas(PathArgSpec{PathSame}, PathArgSpec{PathSelfTwo}) {
		t.Error("Same/SelfTwo wrongly infeasible")
	}
	if !feas(PathArgSpec{PathSame}, PathArgSpec{PathSibling}) {
		t.Error("Same/Sibling wrongly infeasible")
	}
	if !feas(PathArgSpec{PathSibling}, PathArgSpec{PathSibling}) {
		t.Error("Sibling/Sibling wrongly infeasible (dirA has a sibling)")
	}
	if !feas(PathArgSpec{PathSibling}, PathArgSpec{PathSame}) {
		t.Error("Sibling/Same wrongly infeasible (dirA has a same-class sibling)")
	}
	for _, src := range []PathRelation{PathSame, PathSelfTwo, PathSelf, PathChild, PathParent, PathNoRel} {
		if feas(PathArgSpec{src}, PathArgSpec{PathSame}) {
			t.Errorf("%v/Same wrongly feasible (source is not a sibling of the base)", src)
		}
	}
	for _, dst := range []PathRelation{PathSelf, PathParent, PathChild, PathNoRel} {
		if feas(PathArgSpec{PathSame}, PathArgSpec{dst}) {
			t.Errorf("dst relation %v wrongly feasible", dst)
		}
	}

	// A sibling-less base still generates Same/Sibling with a fresh dst, but
	// a Sibling source becomes infeasible.
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{Init_dir: map[string][]string{"c1": {"merge_view/only"}}})
	onlyLCS := &LayeredChoiceStrategy{FileTree: ft}
	r2 := newRand(target, rand.NewSource(2))
	only, _, _ := r2.generateCallFromPatternOp(s, sc, sameSib, "merge_view/only", hmcfg.Cids[0], onlyLCS, nil, false, OffsetUnspecified, 0)
	if len(only) == 0 || only[0].Meta.CallName != "rename" {
		t.Fatalf("sibling-less Same/Sibling: got %d calls, want a rename", len(only))
	}
	if dst := extractPathFromCallByArgIdx(only[0], 1); dst == "" || GetParentDir(dst) != "merge_view" {
		t.Errorf("sibling-less Same/Sibling: dst = %q, want a fresh same-parent name", dst)
	}
	if patternOpsFeasible(onlyLCS, &ConcurrentPattern{Operations: [][]ConcurrentOp{{
		{CallName: "rename", PathArgs: []PathArgSpec{{PathSibling}, {PathSibling}}}}}}, "merge_view/only") {
		t.Error("Sibling source wrongly feasible without siblings")
	}
	if patternOpsFeasible(onlyLCS, &ConcurrentPattern{Operations: [][]ConcurrentOp{{
		{CallName: "rename", PathArgs: []PathArgSpec{{PathSibling}, {PathSame}}}}}}, "merge_view/only") {
		t.Error("Sibling/Same wrongly feasible without siblings")
	}

	// A base whose only same-class sibling is a TmpDir still generates the
	// crossing: the Base source follows the variant-side rule (TmpDir
	// included), unlike the E-destination helper.
	ftTmp := NewFileTree()
	ftTmp.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir:    map[string][]string{"c1": {"merge_view/base"}},
		Init_tmpdir: map[string][]string{"c1": {"merge_view/twin"}},
	})
	tmpLCS := &LayeredChoiceStrategy{FileTree: ftTmp}
	if !patternOpsFeasible(tmpLCS, &ConcurrentPattern{Operations: [][]ConcurrentOp{{
		{CallName: "rename", PathArgs: []PathArgSpec{{PathSibling}, {PathSame}}}}}}, "merge_view/base") {
		t.Error("Sibling/Same with a TmpDir-only sibling wrongly infeasible")
	}
	r4 := newRand(target, rand.NewSource(4))
	tcalls, _, _ := r4.generateCallFromPatternOp(s, sc, ontoBase, "merge_view/base", hmcfg.Cids[0], tmpLCS, nil, false, OffsetUnspecified, 0)
	if len(tcalls) == 0 || tcalls[0].Meta.CallName != "rename" {
		t.Fatalf("TmpDir sibling Sibling/Same: got %d calls, want a rename", len(tcalls))
	}
	if old := extractPathFromCallByArgIdx(tcalls[0], 0); old != "merge_view/twin" {
		t.Errorf("TmpDir sibling Sibling/Same: src = %q, want merge_view/twin", old)
	}
	if dst := extractPathFromCallByArgIdx(tcalls[0], 1); dst != "merge_view/base" {
		t.Errorf("TmpDir sibling Sibling/Same: dst = %q, want merge_view/base", dst)
	}
}

// TestLegacyGeneratorNodeBudget pins that the legacy fallback generators never
// return more programs than Node_num (an over-long batch crashes the fuzzer in
// ipc.Env.Exec/parseOutputs), including the degenerate tiny-Node_num shapes.
func TestLegacyGeneratorNodeBudget(t *testing.T) {
	target, hmcfg, sc, _ := seedOptLCS(t)
	s := newState(target, nil, nil)

	gens := []struct {
		name string
		run  func(r *randGen, cfg *Hmdfs_config, testDir string) []*Prog
	}{
		{"dirDelete", func(r *randGen, cfg *Hmdfs_config, d string) []*Prog {
			return r.generateConcurrentDirDelete(s, sc, cfg, d)
		}},
		{"dirMixed", func(r *randGen, cfg *Hmdfs_config, d string) []*Prog {
			return r.generateConcurrentDirMixed(s, sc, cfg, d)
		}},
		{"renameOps", func(r *randGen, cfg *Hmdfs_config, d string) []*Prog {
			return r.generateConcurrentRenameOps(s, sc, cfg, d)
		}},
		{"fileopsSetattr", func(r *randGen, cfg *Hmdfs_config, d string) []*Prog {
			return r.generateFileopsSetattrConcurrent(s, sc, cfg, d)
		}},
	}

	for _, treeName := range []string{"smoke", "empty"} {
		for _, nodeNum := range []int{2, 3} {
			cfg := &Hmdfs_config{Node_num: nodeNum, Cids: hmcfg.Cids, InitIp: hmcfg.InitIp, FileTree: hmdfsSmokeFileTree()}
			if treeName == "empty" {
				cfg = &Hmdfs_config{Node_num: nodeNum, Cids: hmcfg.Cids, InitIp: hmcfg.InitIp, FileTree: NewFileTree()}
			}
			for _, g := range gens {
				for seed := int64(0); seed < 30; seed++ {
					r := newRand(target, rand.NewSource(seed))
					ps := g.run(r, cfg, "merge_view/dirA")
					if len(ps) > nodeNum {
						t.Fatalf("%s/%s Node_num=%d seed=%d: %d programs (budget exceeded)",
							g.name, treeName, nodeNum, seed, len(ps))
					}
					if len(ps) == 0 {
						t.Fatalf("%s/%s Node_num=%d seed=%d: empty batch", g.name, treeName, nodeNum, seed)
					}
				}
			}
		}
	}

	// Budget-exhausted shapes: @3 with no existing files dirDelete emits one
	// create and two unlinks of the same file (double-unlink race).
	cfg3 := &Hmdfs_config{Node_num: 3, Cids: hmcfg.Cids, FileTree: NewFileTree()}
	r3 := newRand(target, rand.NewSource(1))
	ps3 := r3.generateConcurrentDirDelete(s, sc, cfg3, "merge_view/dirA")
	if len(ps3) != 3 {
		t.Fatalf("dirDelete@3 empty tree: %d programs, want 3", len(ps3))
	}
	creates, unlinks := 0, 0
	for _, p := range ps3 {
		for _, c := range p.Calls {
			switch c.Meta.CallName {
			case "creat":
				creates++
			case "unlink":
				unlinks++
			}
		}
	}
	if creates != 1 || unlinks != 2 {
		t.Errorf("dirDelete@3 empty tree: creates=%d unlinks=%d, want 1/2", creates, unlinks)
	}

	// @2 fileopsSetattr folds the create into the writer program.
	cfg2 := &Hmdfs_config{Node_num: 2, Cids: hmcfg.Cids, FileTree: NewFileTree()}
	r2 := newRand(target, rand.NewSource(1))
	ps2 := r2.generateFileopsSetattrConcurrent(s, sc, cfg2, "merge_view/dirA")
	if len(ps2) != 2 {
		t.Fatalf("fileopsSetattr@2 empty tree: %d programs, want 2", len(ps2))
	}
	if len(ps2[0].Calls) == 0 || ps2[0].Calls[0].Meta.CallName != "creat" {
		t.Errorf("fileopsSetattr@2: writer program does not start with the folded create")
	}

	// @2 dirMixed folds the two unlinks into the second creator program.
	r4 := newRand(target, rand.NewSource(1))
	ps4 := r4.generateConcurrentDirMixed(s, sc, cfg2, "merge_view/dirA")
	if len(ps4) != 2 {
		t.Fatalf("dirMixed@2: %d programs, want 2", len(ps4))
	}
	mixedUnlinks := 0
	for _, c := range ps4[1].Calls {
		if c.Meta.CallName == "unlink" {
			mixedUnlinks++
		}
	}
	if mixedUnlinks != 2 {
		t.Errorf("dirMixed@2: folded unlinks = %d, want 2", mixedUnlinks)
	}
}

// TestGenerateProgsForHmdfsFsopsNodeBudget pins the end-to-end seed budget:
// no generation path (DCT, pattern, legacy fallback) may exceed Node_num.
func TestGenerateProgsForHmdfsFsopsNodeBudget(t *testing.T) {
	target, hmcfg, sc, _ := seedOptLCS(t)
	for _, nodeNum := range []int{2, 3} {
		cfg := &Hmdfs_config{Node_num: nodeNum, Cids: hmcfg.Cids, InitIp: hmcfg.InitIp,
			FileTree: hmdfsSmokeFileTree()}
		nonEmpty := 0
		for seed := int64(0); seed < 60; seed++ {
			ps := target.GenerateProgsForHmdfsFsops(rand.New(rand.NewSource(seed)), sc, cfg)
			if len(ps) > nodeNum {
				t.Fatalf("Node_num=%d seed=%d: %d programs (budget exceeded)", nodeNum, seed, len(ps))
			}
			if len(ps) > 0 {
				nonEmpty++
			}
		}
		if nonEmpty == 0 {
			t.Fatalf("Node_num=%d: no non-empty seed across 60 seeds", nodeNum)
		}
	}
}
