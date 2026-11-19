package prog

import (
	"math/rand"
	"testing"
)

func pathSetTestProg(target *Target, paths ...string) []*Prog {
	p := &Prog{Target: target}
	for _, path := range paths {
		p.Calls = append(p.Calls, mkOpenWithFlags(target, path, 0))
	}
	return []*Prog{p}
}

func TestPathSetCommonPrefix(t *testing.T) {
	got := pathSetCommonPrefix([]string{
		"merge_view/d1/a",
		"merge_view/d1/b",
		"merge_view/d1/c",
	})
	if got != "merge_view/d1" {
		t.Fatalf("common prefix = %q, want merge_view/d1", got)
	}
}

func TestPathSetRootSiblings(t *testing.T) {
	target := smokeTarget(t)
	ft := smokeFileTree()
	ps := pathSetTestProg(target,
		"merge_view/a",
		"merge_view/b",
		"merge_view/d1",
		"merge_view/d1/x",
	)
	set, ok := buildPathSet(ps, GroupPosition{ProgIdx: 0, CallIdx: 3}, ft)
	if !ok {
		t.Fatal("buildPathSet failed")
	}
	if len(set.Members) != 4 {
		t.Fatalf("set size = %d, want 4", len(set.Members))
	}
}

func TestPathSetNoDirectoryBridge(t *testing.T) {
	target := smokeTarget(t)
	ft := smokeFileTree()
	ps := pathSetTestProg(target,
		"merge_view/a",
		"merge_view/b",
		"merge_view/d1/x",
	)
	set, ok := buildPathSet(ps, GroupPosition{ProgIdx: 0, CallIdx: 2}, ft)
	if !ok {
		t.Fatal("buildPathSet failed")
	}
	if len(set.Members) != 1 {
		t.Fatalf("set size = %d, want 1", len(set.Members))
	}
}

func TestPathSetBaseKindDirectory(t *testing.T) {
	ft := smokeFileTree()
	set := &pathSet{Members: []pathMember{
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 0}, Paths: []pathSpec{{Path: "merge_view/dirA/a"}}},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 1}, Paths: []pathSpec{{Path: "merge_view/dirA/b"}}},
	}}
	if got := pathSetBaseKind(set, ft, relTargetFile); got != relTargetDir {
		t.Fatalf("base kind = %d, want dir", got)
	}
}

func TestPathSetRenameArgIdx(t *testing.T) {
	target := smokeTarget(t)
	ft := smokeFileTree()
	call := mkRenameCall(t, target, "merge_view/dirA/a", "merge_view/dirA/c", 0, 0)
	ps := []*Prog{{Target: target, Calls: []*Call{call}}}
	members := buildPathMembers(ps, ft)
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	if len(members[0].Paths) != 2 {
		t.Fatalf("rename specs = %d, want 2", len(members[0].Paths))
	}
	if members[0].Paths[0].ArgIdx != 0 || members[0].Paths[0].Role != pathRoleRenameSrc {
		t.Fatalf("source spec = %+v", members[0].Paths[0])
	}
	if members[0].Paths[1].ArgIdx != 1 || members[0].Paths[1].Role != pathRoleRenameDst {
		t.Fatalf("destination spec = %+v", members[0].Paths[1])
	}
}

func TestPathSetFreshParentCreatedOnlyMkdir(t *testing.T) {
	target := smokeTarget(t)
	ps := []*Prog{{Target: target, Calls: []*Call{
		mkCreat(target, "merge_view/dirA/newfile"),
	}}}
	if pathSetFreshParentCreated(ps, NewFileTree(), GroupPosition{ProgIdx: 0, CallIdx: 1}, "merge_view/dirA/newfile") {
		t.Fatal("creat must not count as a directory creator")
	}
}

func TestMutatePathSetDirected(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Root == "open" && ck.Variant == "truncate" &&
			ck.Rel == PathChild && c.DepthB == 2
	})
	lcs := &LayeredChoiceStrategy{FileTree: smokeFileTree()}
	done := false
	for seed := int64(0); seed < 50 && !done; seed++ {
		openA := mkOpen(target, "merge_view/dirA/a")
		openA.Args[1].(*ConstArg).Val = 0
		openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		tr := mkTruncate(target, "merge_view/dirA/a/b", 1000)
		tr.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
		ps := []*Prog{
			{Target: target, Calls: []*Call{openA}},
			{Target: target, Calls: []*Call{tr}},
		}
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if !mutatePathSetDirected(ps, lcs, r) {
			continue
		}
		anchorPath := callPath(ps[0].Calls[0])
		memberPath := callPath(ps[1].Calls[0])
		if GetParentDir(memberPath) != anchorPath {
			t.Fatalf("member path %q is not a child of anchor %q", memberPath, anchorPath)
		}
		if DepthBucketOf(memberPath) != 2 {
			t.Fatalf("member path %q is not in bucket 2", memberPath)
		}
		done = true
	}
	if !done {
		t.Fatal("no seed produced a directed PathSet mutation")
	}
}

func TestPathSetForestParentBuckets(t *testing.T) {
	got := pathSetForestParentBuckets(3)
	want := []int{3, 2, 1, -1}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buckets = %v, want %v", got, want)
		}
	}
}

func TestPathSetTargetLevel(t *testing.T) {
	target := &pathTarget{Rel: PathSibling, DepthBucket: 2}
	if got := pathSetTargetLevel(target, "merge_view/d/x", "merge_view/d/y"); got != 0 {
		t.Fatalf("exact level = %d, want 0", got)
	}
	if got := pathSetTargetLevel(target, "merge_view/d/x", "merge_view/d/x"); got != 2 {
		t.Fatalf("degraded level = %d, want 2", got)
	}
}

func TestMutatePathSetDirectedMultiRoot(t *testing.T) {
	target := smokeTarget(t)
	newTestCoverageBoard(t)
	coverBoardCellsExcept(t, func(ck comboKey, c cellKey) bool {
		return ck.Root == "open" && ck.Variant == "truncate" &&
			ck.Rel == PathSibling && c.DepthB == 2
	})
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir:  map[string][]string{"c1": {"merge_view/dirA"}},
		Init_file: map[string][]string{"c1": {"merge_view/dirA/a", "merge_view/dirA/b"}},
	})
	lcs := &LayeredChoiceStrategy{FileTree: ft}
	resetPathSetTargetStats()
	done := false
	for seed := int64(0); seed < 50 && !done; seed++ {
		openA := mkOpenWithFlags(target, "merge_view/a", 0)
		openA.CheckInfo = &FileMetadata{Stime: 100, Etime: 200}
		tr := mkTruncate(target, "merge_view/b", 1000)
		tr.CheckInfo = &FileMetadata{Stime: 150, Etime: 250}
		ps := []*Prog{
			{Target: target, Calls: []*Call{openA}},
			{Target: target, Calls: []*Call{tr}},
		}
		r := &randGen{Rand: rand.New(rand.NewSource(seed))}
		if !mutatePathSetDirected(ps, lcs, r) {
			continue
		}
		anchorPath := callPath(ps[0].Calls[0])
		memberPath := callPath(ps[1].Calls[0])
		if GetParentDir(anchorPath) != GetParentDir(memberPath) {
			t.Fatalf("paths are not siblings: %q vs %q", anchorPath, memberPath)
		}
		if DepthBucketOf(memberPath) != 2 {
			t.Fatalf("member path %q is not in bucket 2", memberPath)
		}
		if exact, _, _, _, _, _, applyFail := PathSetTargetStats(); exact != 1 || applyFail != 0 {
			t.Fatalf("stats exact=%d applyFail=%d, want exact=1 applyFail=0", exact, applyFail)
		}
		diag := PathSetDiagnosticsSnapshot()
		if diag.Builds == 0 || diag.DirectedAttempts == 0 || diag.DirectedMultiRoot == 0 {
			t.Fatalf("diagnostics builds=%d directed=%d multi=%d",
				diag.Builds, diag.DirectedAttempts, diag.DirectedMultiRoot)
		}
		if diag.CandidateCount == 0 || diag.ExternalParentCandidates+diag.SelectedDirCandidates == 0 {
			t.Fatalf("diagnostics candidates=%d external=%d selected=%d",
				diag.CandidateCount, diag.ExternalParentCandidates, diag.SelectedDirCandidates)
		}
		done = true
	}
	if !done {
		t.Fatal("no seed produced a multi-root directed PathSet mutation")
	}
}

func TestPathSetDirectedMultiRootCandidates(t *testing.T) {
	target := smokeTarget(t)
	ft := smokeFileTree()
	ps := pathSetTestProg(target, "merge_view/a", "merge_view/b")
	set, ok := buildPathSet(ps, GroupPosition{ProgIdx: 0, CallIdx: 0}, ft)
	if !ok {
		t.Fatal("buildPathSet failed")
	}
	r := smokeRand(target)
	cands := pathSetDirectedCandidates(ps, set, &pathTarget{
		Anchor:      set.Anchor,
		Member:      GroupPosition{ProgIdx: 0, CallIdx: 1},
		Rel:         PathSibling,
		DepthBucket: 2,
	}, ft, r)
	if len(cands) == 0 {
		t.Fatal("multi-root directed candidates are empty")
	}
}

func TestPathSetRenameDstType(t *testing.T) {
	target := smokeTarget(t)
	ft := smokeFileTree()
	ps := []*Prog{{Target: target, Calls: []*Call{
		mkRenameCall(t, target, "merge_view/dirA/a", "merge_view/dirA/b", 0, 0),
	}}}
	pos := GroupPosition{ProgIdx: 0, CallIdx: 0}
	fileSpec := pathSpec{
		Path: "merge_view/dirA/a", Kind: pathExisting,
		Role: pathRoleRenameDst, ArgIdx: 1, TypeConstraint: relTargetFile,
	}
	if !pathSetSpecValid(ps, pos, fileSpec, relTargetAny, ft) {
		t.Fatal("file rename destination should accept a file")
	}
	dirSpec := fileSpec
	dirSpec.Path = "merge_view/dirA"
	if pathSetSpecValid(ps, pos, dirSpec, relTargetAny, ft) {
		t.Fatal("file rename destination must reject a directory")
	}
}

func TestMapPathSetPath(t *testing.T) {
	if got := mapPathSetPath(MergeViewPrefix, "merge_view/d1/a", "merge_view/d1", "", nil); got != "merge_view/d1/a" {
		t.Fatalf("single-root map = %q", got)
	}
	if got := mapPathSetPath(MergeViewPrefix, "merge_view/d1", "merge_view/d1", "", nil); got != "merge_view/d1" {
		t.Fatalf("single-root exact map = %q", got)
	}
	got := mapPathSetPath(MergeViewPrefix, "merge_view/d1/x", "", "merge_view/p", []string{"a"})
	if got != "merge_view/p/d1/x" {
		t.Fatalf("forest map = %q", got)
	}
}

func TestMapPathSetBasePath(t *testing.T) {
	cases := []struct {
		path, oldBase, newBase, want string
	}{
		{"dirA/a", "dirA", "dirB", "dirB/a"},
		{"dirA", "dirA", "dirB", "dirB"},
		{"dirA/sub/a", "dirA/sub", "dirA", "dirA/a"},
		{"dirA/a", "dirA", "dirA", ""},
		{"dirA/a", "dirA", "", ""},
		{"dirB/a", "dirA", "dirB", ""},
	}
	for _, tc := range cases {
		if got := mapPathSetBasePath(tc.path, tc.oldBase, tc.newBase); got != tc.want {
			t.Fatalf("mapPathSetBasePath(%q,%q,%q) = %q, want %q",
				tc.path, tc.oldBase, tc.newBase, got, tc.want)
		}
	}
}

func TestPathSetBaseCandidateOK(t *testing.T) {
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA", NodeTypeDir, "c1")
	ft.AddNode("dirA/sub", NodeTypeDir, "c1")

	if !pathSetBaseCandidateOK(ft, "dirA", "dirB") {
		t.Fatal("unrelated newBase rejected")
	}
	if !pathSetBaseCandidateOK(ft, "dirA/sub", "dirA") {
		t.Fatal("ancestor newBase rejected")
	}
	if pathSetBaseCandidateOK(ft, "dirA", "dirA") {
		t.Fatal("no-op newBase accepted")
	}
	if pathSetBaseCandidateOK(ft, "dirA", "dirA/sub") {
		t.Fatal("descendant newBase accepted")
	}
	if pathSetBaseCandidateOK(ft, "dirA", ft.Root.FullPath) {
		t.Fatal("namespace root accepted as a newBase")
	}
}

func TestPathPlanChangesPaths(t *testing.T) {
	set := &pathSet{Members: []pathMember{{
		Pos:   GroupPosition{ProgIdx: 0, CallIdx: 0},
		Paths: []pathSpec{{Path: "dirA/a"}},
	}}}
	plan := &pathPlan{Updates: []pathUpdate{{
		Pos:   GroupPosition{ProgIdx: 0, CallIdx: 0},
		Specs: []pathSpec{{Path: "dirA/a"}},
	}}}
	if pathPlanChangesPaths(set, plan) {
		t.Fatal("no-op plan reported as a change")
	}
	plan.Updates[0].Specs[0].Path = "dirB/a"
	if !pathPlanChangesPaths(set, plan) {
		t.Fatal("changed plan reported as a no-op")
	}
}

func TestBuildPathPlanBroadMapping(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA", NodeTypeDir, "c1")
	ft.AddNode("dirA/a", NodeTypeDir, "c1")
	ft.AddNode("dirA/b", NodeTypeDir, "c1")
	ft.AddNode("dirB", NodeTypeDir, "c1")
	ft.AddNode("dirB/a", NodeTypeDir, "c1")
	ft.AddNode("dirB/b", NodeTypeDir, "c1")
	p := &Prog{Target: target, Calls: []*Call{
		mkMkdir(target, "dirA/a"),
		mkMkdir(target, "dirA/b"),
	}}
	set := &pathSet{
		Anchor: GroupPosition{ProgIdx: 0, CallIdx: 0},
		Members: []pathMember{
			{Pos: GroupPosition{ProgIdx: 0, CallIdx: 0}, Constraint: relTargetDir,
				Paths: []pathSpec{{Path: "dirA/a", Kind: pathExisting, Role: pathRoleMkdir,
					ArgIdx: 0, TypeConstraint: relTargetDir}}},
			{Pos: GroupPosition{ProgIdx: 0, CallIdx: 1}, Constraint: relTargetDir,
				Paths: []pathSpec{{Path: "dirA/b", Kind: pathExisting, Role: pathRoleMkdir,
					ArgIdx: 0, TypeConstraint: relTargetDir}}},
		},
	}
	plan, ok := buildPathPlan([]*Prog{p}, set, "dirA", "dirB", "", nil, ft,
		newRand(target, rand.NewSource(5)))
	if !ok {
		t.Fatal("buildPathPlan failed")
	}
	if got := plan.Updates[0].Specs[0].Path; got != "dirB/a" {
		t.Fatalf("mapped path = %q, want dirB/a", got)
	}
	if got := plan.Updates[1].Specs[0].Path; got != "dirB/b" {
		t.Fatalf("mapped path = %q, want dirB/b", got)
	}
	if !pathPlanChangesPaths(set, plan) {
		t.Fatal("broad mapping reported as a no-op")
	}
}

func TestPathSetTargetLevelGlusterfsTopLevel(t *testing.T) {
	target := &pathTarget{Rel: PathSibling, DepthBucket: 1}
	if got := pathSetTargetLevel(target, "dirA", "dirB"); got != 0 {
		t.Fatalf("top-level GlusterFS target level = %d, want 0", got)
	}
}
