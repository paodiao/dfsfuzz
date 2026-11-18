package prog

import "testing"

func TestFreshLinkPathsRenameDirectoryMovesBindings(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := glusterfsPlanTree()
	link := mkLinkCall(t, target, "dirA/a", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "dirA", "dirX", 210, 260)
	p := &Prog{Target: target, Calls: []*Call{link, rn}}

	links := freshLinkPathsBefore(p, ft, 2, glusterfsProfile)
	if _, ok := links["dirA/l1"]; ok {
		t.Fatal("renamed directory left the old link binding")
	}
	info, ok := links["dirX/l1"]
	if !ok || info.Kind != linkPathHardlink || info.Target != "dirX/a" {
		t.Fatalf("renamed link binding = %+v (found=%v), want dirX/a hardlink", info, ok)
	}
}

func TestFreshLinkPathsRenameDirectoryRejectsNestedLink(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	for _, tc := range []struct {
		name  string
		first *Call
	}{
		{"hardlink", mkLinkCall(t, target, "dirA/a", "dirA/l1")},
		{"symlink", mkSymlinkCall(t, target, "a", "dirA/l1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ft := glusterfsPlanTree()
			tc.first.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
			rn := mkRenameCall(t, target, "dirA", "dirX", 210, 260)
			second := mkLinkCall(t, target, "dirX/l1", "dirX/l2")
			second.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
			p := &Prog{Target: target, Calls: []*Call{tc.first, rn, second}}

			for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
				if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 2}) {
					t.Fatalf("nested %s target under renamed directory produced a member: %+v", tc.name, m)
				}
			}
		})
	}
}

func TestFreshPathKindsRenameDirectory(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := glusterfsPlanTree()
	creat := mkCreat(target, "dirA/fresh")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "dirA", "dirX", 210, 260)
	p := &Prog{Target: target, Calls: []*Call{creat, rn}}

	kinds := freshPathKindsBefore(p, ft, 2, glusterfsProfile)
	if kinds["dirX"] != relTargetDir || kinds["dirX/fresh"] != relTargetFile {
		t.Fatalf("renamed fresh kinds = %v, want dirX dir and dirX/fresh file", kinds)
	}
	if _, ok := kinds["dirA/fresh"]; ok {
		t.Fatalf("old fresh kind survived directory rename: %v", kinds)
	}
}

func TestFreshPathCreatorRenameDirectory(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := glusterfsPlanTree()
	creat := mkCreat(target, "dirA/fresh")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "dirA", "dirX", 210, 260)
	link := mkLinkCall(t, target, "dirX/fresh", "dirX/l1")
	link.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{creat, rn, link}}

	var spec *pathSpec
	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 2}) {
			spec = &m.Paths[0]
		}
	}
	if spec == nil {
		t.Fatal("renamed creator produced no link target member")
	}
	if spec.Path != "dirX/fresh" || spec.Kind != pathFresh ||
		!spec.HasCreator || spec.CreatorPos != (GroupPosition{ProgIdx: 0, CallIdx: 0}) {
		t.Fatalf("renamed creator link spec = %+v", *spec)
	}
}

func TestFreshPathAbsentRenameDirectory(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["unlink"]; !ok {
		t.Skip("target has no unlink")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	unlink := mkUnlinkCall(t, target, "dirA/a")
	unlink.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "dirA", "dirX", 210, 260)
	p := &Prog{Target: target, Calls: []*Call{unlink, rn}}

	absent := freshPathAbsentBefore(p, 2, glusterfsProfile)
	if !absent["dirA"] || !absent["dirX/a"] {
		t.Fatalf("renamed tombstones = %v, want dirA and dirX/a", absent)
	}
}

func TestFreshRenameDirectoryHMDFSLegacy(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	if _, ok := target.SyscallMap["rename"]; !ok {
		t.Skip("target has no rename")
	}
	ft := smokeFileTree()
	creat := mkCreat(target, "merge_view/dirA/fresh")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	rn := mkRenameCall(t, target, "merge_view/dirA", "merge_view/dirX", 210, 260)
	p := &Prog{Target: target, Calls: []*Call{creat, rn}}

	kinds := freshPathKindsBefore(p, ft, 2, hmdfsProfile)
	if kinds["merge_view/dirX"] != relTargetDir {
		t.Fatalf("HMDFS rename destination kind = %v", kinds)
	}
	if _, ok := kinds["merge_view/dirX/fresh"]; ok {
		t.Fatalf("HMDFS legacy unexpectedly remapped descendant kind: %v", kinds)
	}
}
