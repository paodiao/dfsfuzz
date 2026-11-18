package prog

import "testing"

func mkRmdirCallForTest(t *testing.T, target *Target, path string) *Call {
	t.Helper()
	meta := target.SyscallMap["rmdir"]
	ptrType := meta.Args[0].Type.(*PtrType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr}
	target.assignSizesCall(c)
	return c
}

func TestProfilePathStateTracking(t *testing.T) {
	if hmdfsProfile.TracksPathState() {
		t.Fatal("HMDFS must keep the legacy baseline-first path state")
	}
	if !glusterfsProfile.TracksPathState() {
		t.Fatal("GlusterFS must track exact in-program path state")
	}
	future := *glusterfsProfile
	future.Name = "futuredfs"
	future.Plan.PathStateTracking = true
	if !future.TracksPathState() {
		t.Fatal("a link-capable profile must be able to opt into path state tracking")
	}
}

func TestFreshPathAbsentBefore(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	first := mkCreat(target, "dirA/fresh")
	first.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	unlink := mkUnlinkCall(t, target, "dirA/fresh")
	unlink.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	second := mkCreat(target, "dirA/fresh")
	second.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{first, unlink, second}}

	absent := freshPathAbsentBefore(p, 2, glusterfsProfile)
	if !absent["dirA/fresh"] {
		t.Fatal("successful unlink did not mark the path absent")
	}
	absent = freshPathAbsentBefore(p, 3, glusterfsProfile)
	if absent["dirA/fresh"] {
		t.Fatal("recreation did not clear the absent marker")
	}
	if got := freshPathAbsentBefore(p, 3, hmdfsProfile); got != nil {
		t.Fatalf("HMDFS absent map = %v, want nil legacy behavior", got)
	}
}

func TestBuildPathMembersSkipsDeletedLinkTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	ft := glusterfsPlanTree()
	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{unlink, link}}

	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
			t.Fatalf("deleted link target produced a member: %+v", m)
		}
	}
}

func TestBuildPathMembersSkipsRenamedAwayLinkTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := glusterfsPlanTree()
	rename := mkRenameCall(t, target, "dirA/a", "dirA/b", 100, 200)
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{rename, link}}

	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
			t.Fatalf("renamed-away link target produced a member: %+v", m)
		}
	}
}

func TestBuildPathMembersLinkRecreatedSameType(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	ft := glusterfsPlanTree()
	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	creat := mkCreat(target, "dirA/a")
	creat.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{unlink, creat, link}}

	var spec *pathSpec
	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 2}) {
			spec = &m.Paths[0]
		}
	}
	if spec == nil {
		t.Fatal("recreated link target produced no member")
	}
	if spec.Kind != pathFresh || spec.CurrentKind != relTargetFile ||
		!spec.HasCreator || spec.CreatorPos != (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
		t.Fatalf("recreated link target spec = %+v", *spec)
	}
	if !pathSetSpecValid(nil, GroupPosition{}, *spec, relTargetFile, ft) {
		t.Fatal("recreated link target rejected by pathSetSpecValid")
	}
}

func TestBuildPathMembersLinkRecreatedFromDir(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	if _, ok := target.SyscallMap["rmdir"]; !ok {
		t.Skip("target has no rmdir")
	}
	ft := glusterfsPlanTree()
	ft.AddNode("dirA/empty", NodeTypeEmptyDir, hmdfsSmokeCids()[0])
	rmdir := mkRmdirCallForTest(t, target, "dirA/empty")
	rmdir.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	creat := mkCreat(target, "dirA/empty")
	creat.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	link := mkLinkCall(t, target, "dirA/empty", "dirB/l1")
	link.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{rmdir, creat, link}}

	var creatSpec, linkSpec *pathSpec
	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		switch m.Pos.CallIdx {
		case 1:
			creatSpec = &m.Paths[0]
		case 2:
			linkSpec = &m.Paths[0]
		}
	}
	if creatSpec == nil || linkSpec == nil {
		t.Fatalf("recreated dir target members missing: creat=%+v link=%+v", creatSpec, linkSpec)
	}
	if creatSpec.Kind != pathFresh || !creatSpec.Recreated ||
		creatSpec.CurrentKind != relTargetFile {
		t.Fatalf("recreated creat spec = %+v", *creatSpec)
	}
	if linkSpec.Kind != pathFresh || linkSpec.CurrentKind != relTargetFile ||
		!linkSpec.HasCreator || linkSpec.CreatorPos != (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
		t.Fatalf("recreated link spec = %+v", *linkSpec)
	}
	if !pathSetSpecValid(nil, GroupPosition{}, *creatSpec, relTargetFile, ft) {
		t.Fatal("recreated creat spec rejected")
	}
	if !pathSetSpecValid(nil, GroupPosition{}, *linkSpec, relTargetFile, ft) {
		t.Fatal("recreated link spec rejected")
	}
	set := &pathSet{Members: []pathMember{
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 1}, Paths: []pathSpec{*creatSpec}, Constraint: relTargetFile},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 2}, Paths: []pathSpec{*linkSpec}, Constraint: relTargetFile},
	}}
	plan := &pathPlan{Updates: []pathUpdate{
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 1}, Specs: []pathSpec{*creatSpec}},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 2}, Specs: []pathSpec{*linkSpec}},
	}}
	if _, ok := validatePathSetPlan(nil, set, plan, ft, nil); !ok {
		t.Fatal("recreated dir-to-file link plan rejected")
	}
}

func TestPathSetSpecValidFreshLinkTargetCurrentKind(t *testing.T) {
	ft := glusterfsPlanTree()
	spec := pathSpec{
		Path: "dirA/a", Kind: pathFresh, Role: pathRoleLinkTarget,
		ArgIdx: 0, TypeConstraint: relTargetFile,
		CreatorPos: GroupPosition{ProgIdx: 0, CallIdx: 0}, CreatorArgIdx: 0,
		HasCreator: true, CurrentKind: relTargetFile,
	}
	if !pathSetSpecValid(nil, GroupPosition{}, spec, relTargetFile, ft) {
		t.Fatal("fresh link target with matching current kind rejected")
	}
	badKind := spec
	badKind.CurrentKind = relTargetDir
	if pathSetSpecValid(nil, GroupPosition{}, badKind, relTargetFile, ft) {
		t.Fatal("fresh link target with directory current kind accepted")
	}
	noCreator := spec
	noCreator.HasCreator = false
	if pathSetSpecValid(nil, GroupPosition{}, noCreator, relTargetFile, ft) {
		t.Fatal("fresh link target on an existing baseline node accepted without a creator")
	}
}

func TestFutureProfilePathStateTracking(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	ft := glusterfsPlanTree()
	future := *glusterfsProfile
	future.Name = "futuredfs"
	future.Plan.PathStateTracking = true
	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{unlink, link}}

	for _, m := range buildPathMembers([]*Prog{p}, ft, &future) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
			t.Fatalf("future profile accepted a deleted link target: %+v", m)
		}
	}
}
