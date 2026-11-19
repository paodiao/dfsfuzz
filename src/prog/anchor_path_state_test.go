package prog

import "testing"

func mkRmdirCall(t *testing.T, target *Target, path string) *Call {
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

// TestClassifyAnchorPathRejectsDeletedPath pins the B5 path-state guard: an
// anchor on a path removed by an earlier successful call is not Existing.
func TestClassifyAnchorPathRejectsDeletedPath(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	ft := glusterfsPlanTree()
	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	open := mkOpen(target, "dirA/a")
	open.CheckInfo = &FileMetadata{Stime: 120, Etime: 130}
	p := &Prog{Target: target, Calls: []*Call{unlink, open}}

	pos := GroupPosition{ProgIdx: 0, CallIdx: 1}
	if _, ok := classifyAnchorPath(p, ft, pos, "dirA/a", true, glusterfsProfile); ok {
		t.Fatal("anchor on a path deleted earlier was accepted")
	}
	// HMDFS keeps the historical baseline-only behavior.
	if _, ok := classifyAnchorPath(p, ft, pos, "dirA/a", true); !ok {
		t.Fatal("HMDFS baseline behavior changed")
	}
}

// TestClassifyAnchorPathAcceptsCreatorAnchor ensures the deletion guard does
// not reject an anchor that creates the path itself.
func TestClassifyAnchorPathAcceptsCreatorAnchor(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	ft := glusterfsPlanTree()
	oCreate, ok := target.LookupConst("O_CREAT")
	if !ok {
		t.Skip("target has no O_CREAT")
	}
	open := mkOpenWithFlags(target, "dirA/new", oCreate|2)
	open.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 3}
	p := &Prog{Target: target, Calls: []*Call{open}}

	info, ok := classifyAnchorPath(p, ft, GroupPosition{ProgIdx: 0, CallIdx: 0},
		"dirA/new", true, glusterfsProfile)
	if !ok || info.Kind != AnchorPathFreshFile || info.IsDir {
		t.Fatalf("creator anchor = %+v, ok=%v", info, ok)
	}
}

// TestClassifyAnchorPathRecreatedType pins that the latest creator wins after
// a deletion: the baseline directory was replaced by a file, so the anchor
// must not fall back to the stale baseline type.
func TestClassifyAnchorPathRecreatedType(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["rmdir"]; !ok {
		t.Skip("target has no rmdir")
	}
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA", NodeTypeDir, "c1")
	ft.AddNode("dirA/a", NodeTypeDir, "c1")

	rm := mkRmdirCall(t, target, "dirA/a")
	rm.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	creat := mkCreat(target, "dirA/a")
	creat.CheckInfo = &FileMetadata{Stime: 120, Etime: 130, Retv: 0}
	open := mkOpen(target, "dirA/a")
	open.CheckInfo = &FileMetadata{Stime: 140, Etime: 150}
	p := &Prog{Target: target, Calls: []*Call{rm, creat, open}}

	info, ok := classifyAnchorPath(p, ft, GroupPosition{ProgIdx: 0, CallIdx: 2},
		"dirA/a", true, glusterfsProfile)
	if !ok || info.Kind != AnchorPathFreshFile || info.IsDir || info.CreatorIdx != 1 {
		t.Fatalf("recreated anchor = %+v, ok=%v", info, ok)
	}
}

// TestBuildPathMembersSkipsDeletedRenameSource pins the PathSet-side B5 guard:
// a rename whose source was removed earlier cannot execute and must not become
// a PathSet member.
func TestBuildPathMembersSkipsLinkPath(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["stat"]; !ok {
		t.Skip("target has no stat")
	}
	ft := glusterfsPlanTree()
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	stat := mkPathCall(t, target, "stat", "dirA/l1", 120, 130)
	stat.CheckInfo.Retv = 0
	p := &Prog{Target: target, Calls: []*Call{link, stat}}

	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos.CallIdx == 1 {
			t.Fatalf("linkpath produced a generic member: %+v", m)
		}
	}
}

func TestBuildPathMembersSkipsDeletedRenameSource(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	ft := glusterfsPlanTree()
	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	rename := mkRenameCall(t, target, "dirA/a", "dirA/b", 120, 130)
	p := &Prog{Target: target, Calls: []*Call{unlink, rename}}

	members := buildPathMembers([]*Prog{p}, ft, glusterfsProfile)
	for _, m := range members {
		if m.Pos.CallIdx == 1 {
			t.Fatalf("rename member with deleted source was built: %+v", m)
		}
	}
}

func TestFindAnchorCreatorLegacyFirstCreator(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	ft := glusterfsPlanTree()
	first := mkCreat(target, "dirA/fresh")
	first.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	second := mkCreat(target, "dirA/fresh")
	second.CheckInfo = &FileMetadata{Stime: 120, Etime: 130, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{first, second}}

	// HMDFS legacy (no profile): the first creator wins.
	kind, isDir, idx, ok := findAnchorCreator(p, ft, "dirA/fresh", 1, true)
	if !ok || kind != AnchorPathFreshFile || isDir || idx != 0 {
		t.Fatalf("HMDFS creator = kind %v isDir %v idx %d ok %v, want FreshFile idx 0",
			kind, isDir, idx, ok)
	}
	// GlusterFS: the latest creator wins.
	kind, isDir, idx, ok = findAnchorCreator(p, ft, "dirA/fresh", 1, true, glusterfsProfile)
	if !ok || kind != AnchorPathFreshFile || isDir || idx != 1 {
		t.Fatalf("GlusterFS creator = kind %v isDir %v idx %d ok %v, want FreshFile idx 1",
			kind, isDir, idx, ok)
	}
}

func TestFindAnchorCreatorSkipsFailedLatestCreator(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["mkdir"]; !ok {
		t.Skip("target has no mkdir")
	}
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	ft := glusterfsPlanTree()
	creat := mkCreat(target, "dirA/fresh")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	mkdir := mkMkdir(target, "dirA/fresh")
	mkdir.CheckInfo = &FileMetadata{Stime: 120, Etime: 130, Retv: -1}
	p := &Prog{Target: target, Calls: []*Call{creat, mkdir}}

	for _, tc := range []struct {
		name    string
		profile *DFSProfile
	}{
		{"hmdfs", nil},
		{"glusterfs", glusterfsProfile},
	} {
		var profiles []*DFSProfile
		if tc.profile != nil {
			profiles = append(profiles, tc.profile)
		}
		kind, isDir, idx, ok := findAnchorCreator(p, ft, "dirA/fresh", 1, false, profiles...)
		if !ok || kind != AnchorPathFreshFile || isDir || idx != 0 {
			t.Fatalf("%s creator = kind %v isDir %v idx %d ok %v, want FreshFile idx 0",
				tc.name, kind, isDir, idx, ok)
		}
	}
}

func TestClassifyAnchorPathRejectsDeletedLinkTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	ft := glusterfsPlanTree()
	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 120, Etime: 130, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{unlink, link}}

	if _, ok := classifyAnchorPath(p, ft, GroupPosition{ProgIdx: 0, CallIdx: 1},
		"dirA/l1", true, glusterfsProfile); ok {
		t.Fatal("linkpath anchor with a deleted target was accepted")
	}
}

func TestClassifyAnchorPathRejectsDirLinkTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["mkdir"]; !ok {
		t.Skip("target has no mkdir")
	}
	ft := glusterfsPlanTree()
	mkdir := mkMkdir(target, "dirA/newdir")
	mkdir.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	link := mkLinkCall(t, target, "dirA/newdir", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 120, Etime: 130, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{mkdir, link}}

	if _, ok := classifyAnchorPath(p, ft, GroupPosition{ProgIdx: 0, CallIdx: 1},
		"dirA/l1", true, glusterfsProfile); ok {
		t.Fatal("linkpath anchor with a directory target was accepted")
	}
}

func TestClassifyAnchorPathAcceptsFileLinkTarget(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	ft := glusterfsPlanTree()
	creat := mkCreat(target, "dirA/target")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	link := mkLinkCall(t, target, "dirA/target", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 120, Etime: 130, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{creat, link}}

	info, ok := classifyAnchorPath(p, ft, GroupPosition{ProgIdx: 0, CallIdx: 1},
		"dirA/l1", true, glusterfsProfile)
	if !ok || info.Kind != AnchorPathFreshFile || info.IsDir {
		t.Fatalf("linkpath anchor with a fresh file target = %+v, ok=%v", info, ok)
	}
}

func TestFreshPathAbsentPrefixedProfile(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	future := *glusterfsProfile
	future.Name = "futuredfs"
	future.Plan.PathStateTracking = true
	future.Tree.Prefix = MergeViewPrefix

	ft := NewFileTree()
	ft.AddNode("merge_view/dirA", NodeTypeDir, "c1")
	ft.AddNode("merge_view/dirA/a", NodeTypeFile, "c1")

	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	open := mkOpen(target, "dirA/a")
	open.CheckInfo = &FileMetadata{Stime: 120, Etime: 130}
	p := &Prog{Target: target, Calls: []*Call{unlink, open}}

	absent := freshPathAbsentBefore(p, 1, &future)
	if !absent["merge_view/dirA/a"] {
		t.Fatalf("absent = %v, want prefixed merge_view/dirA/a", absent)
	}
	if absent["dirA/a"] {
		t.Fatalf("absent kept the bare key: %v", absent)
	}
	if _, ok := classifyAnchorPath(p, ft, GroupPosition{ProgIdx: 0, CallIdx: 1},
		"dirA/a", true, &future); ok {
		t.Fatal("prefixed tracked profile accepted an anchor deleted earlier")
	}
}
