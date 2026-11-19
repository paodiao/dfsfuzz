package prog

import "testing"

func TestBuildPathMembersFreshRenameSource(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	for _, name := range []string{"creat", "rename"} {
		if _, ok := target.SyscallMap[name]; !ok {
			t.Skipf("target has no %s", name)
		}
	}
	ft := glusterfsPlanTree()
	creat := mkCreat(target, "dirA/new")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	rn := mkRenameCall(t, target, "dirA/new", "dirA/moved", 120, 130)
	rn.CheckInfo.Retv = 0
	ps := []*Prog{{Target: target, Calls: []*Call{creat, rn}}}

	var srcSpec *pathSpec
	for _, m := range buildPathMembers(ps, ft, glusterfsProfile) {
		if m.Pos.CallIdx == 1 && len(m.Paths) > 0 {
			srcSpec = &m.Paths[0]
		}
	}
	if srcSpec == nil {
		t.Fatal("rename member missing")
	}
	if srcSpec.Kind != pathFresh || !srcSpec.HasCreator ||
		srcSpec.CreatorPos != (GroupPosition{ProgIdx: 0, CallIdx: 0}) ||
		srcSpec.CurrentKind != relTargetFile {
		t.Fatalf("fresh rename source spec = %+v", *srcSpec)
	}
	if !pathSetSpecValid(ps, GroupPosition{ProgIdx: 0, CallIdx: 1},
		*srcSpec, relTargetFile, ft) {
		t.Fatal("fresh rename source spec rejected")
	}
}

func TestValidatePathSetPlanFreshRenameSource(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	for _, name := range []string{"creat", "rename"} {
		if _, ok := target.SyscallMap[name]; !ok {
			t.Skipf("target has no %s", name)
		}
	}
	ft := glusterfsPlanTree()
	creat := mkCreat(target, "dirA/new")
	creat.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	rn := mkRenameCall(t, target, "dirA/new", "dirA/moved", 120, 130)
	rn.CheckInfo.Retv = 0
	ps := []*Prog{{Target: target, Calls: []*Call{creat, rn}}}

	members := buildPathMembers(ps, ft, glusterfsProfile)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	set := &pathSet{Anchor: GroupPosition{ProgIdx: 0, CallIdx: 1}, Members: members}
	plan := &pathPlan{}
	for _, m := range members {
		plan.Updates = append(plan.Updates, pathUpdate{
			Pos:   m.Pos,
			Specs: append([]pathSpec{}, m.Paths...),
		})
	}
	if _, ok := validatePathSetPlan(ps, set, plan, ft, nil); !ok {
		t.Fatal("fresh rename source plan rejected")
	}

	// The creator and the source must move together.
	bad := &pathPlan{}
	for i, m := range members {
		specs := append([]pathSpec{}, m.Paths...)
		if i == 0 {
			specs[0].Path = "dirA/other"
		}
		bad.Updates = append(bad.Updates, pathUpdate{Pos: m.Pos, Specs: specs})
	}
	if _, ok := validatePathSetPlan(ps, set, bad, ft, nil); ok {
		t.Fatal("plan with a detached rename source creator accepted")
	}
}
