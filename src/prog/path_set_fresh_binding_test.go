package prog

import "testing"

func freshLinkBindingFixture() (*pathSet, *pathPlan) {
	set := &pathSet{Members: []pathMember{
		{
			Pos: GroupPosition{ProgIdx: 0, CallIdx: 0},
			Paths: []pathSpec{{
				Path: "dirA/fresh.txt", Kind: pathFresh, Role: pathRoleCreat,
				ArgIdx: 0, TypeConstraint: relTargetFile,
			}},
			Constraint: relTargetFile,
		},
		{
			Pos: GroupPosition{ProgIdx: 0, CallIdx: 1},
			Paths: []pathSpec{{
				Path: "dirA/fresh.txt", Kind: pathFresh, Role: pathRoleLinkTarget,
				ArgIdx: 0, TypeConstraint: relTargetFile,
				CreatorPos:    GroupPosition{ProgIdx: 0, CallIdx: 0},
				CreatorArgIdx: 0, HasCreator: true,
			}},
			Constraint: relTargetFile,
		},
	}}
	plan := &pathPlan{Updates: []pathUpdate{
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 0}, Specs: []pathSpec{{
			Path: "dirB/x.txt", Kind: pathFresh, Role: pathRoleCreat,
			ArgIdx: 0, TypeConstraint: relTargetFile,
		}}},
		{Pos: GroupPosition{ProgIdx: 0, CallIdx: 1}, Specs: []pathSpec{{
			Path: "dirB/y.txt", Kind: pathFresh, Role: pathRoleLinkTarget,
			ArgIdx: 0, TypeConstraint: relTargetFile,
			CreatorPos:    GroupPosition{ProgIdx: 0, CallIdx: 0},
			CreatorArgIdx: 0, HasCreator: true,
		}}},
	}}
	return set, plan
}

func TestValidatePathSetPlanRejectsDivergentFreshLinkTarget(t *testing.T) {
	ft := glusterfsPlanTree()
	set, plan := freshLinkBindingFixture()
	if _, ok := validatePathSetPlan(nil, set, plan, ft, nil); ok {
		t.Fatal("divergent fresh link target accepted: creator and target map to different paths")
	}
}

func TestValidatePathSetPlanAcceptsAlignedFreshLinkTarget(t *testing.T) {
	ft := glusterfsPlanTree()
	set, plan := freshLinkBindingFixture()
	plan.Updates[1].Specs[0].Path = plan.Updates[0].Specs[0].Path
	if _, ok := validatePathSetPlan(nil, set, plan, ft, nil); !ok {
		t.Fatal("aligned fresh link target rejected")
	}
}

func TestAlignFreshLinkTargets(t *testing.T) {
	set, plan := freshLinkBindingFixture()
	alignFreshLinkTargets(set, plan.Updates)
	if got := plan.Updates[1].Specs[0].Path; got != "dirB/x.txt" {
		t.Fatalf("aligned link target = %q, want creator path dirB/x.txt", got)
	}
}

func TestBuildPathMembersFreshLinkTargetCreatorBinding(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	ft := glusterfsPlanTree()
	creator := mkCreat(target, "dirA/fresh.txt")
	creator.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	link := mkLinkCall(t, target, "dirA/fresh.txt", "dirA/l1")
	link.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{creator, link}}

	var spec *pathSpec
	for _, m := range buildPathMembers([]*Prog{p}, ft, glusterfsProfile) {
		if m.Pos == (GroupPosition{ProgIdx: 0, CallIdx: 1}) {
			spec = &m.Paths[0]
		}
	}
	if spec == nil {
		t.Fatal("no fresh link target member")
	}
	if !spec.HasCreator || spec.CreatorArgIdx != 0 ||
		spec.CreatorPos != (GroupPosition{ProgIdx: 0, CallIdx: 0}) {
		t.Fatalf("fresh link creator binding = %+v", *spec)
	}
}

func TestFreshPathCreatorBeforeLatestSuccess(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["creat"]; !ok {
		t.Skip("target has no creat")
	}
	first := mkCreat(target, "dirA/fresh.txt")
	first.CheckInfo = &FileMetadata{Stime: 100, Etime: 200, Retv: 0}
	failed := mkCreat(target, "dirA/fresh.txt")
	failed.CheckInfo = &FileMetadata{Stime: 210, Etime: 260, Retv: 1}
	last := mkCreat(target, "dirA/fresh.txt")
	last.CheckInfo = &FileMetadata{Stime: 270, Etime: 320, Retv: 0}
	p := &Prog{Target: target, Calls: []*Call{first, failed, last}}

	pos, arg, ok := freshPathCreatorBefore(p, 3, "dirA/fresh.txt", glusterfsProfile)
	if !ok || pos.CallIdx != 2 || arg != 0 {
		t.Fatalf("latest creator = %+v arg=%d ok=%v, want call 2 arg0", pos, arg, ok)
	}
	pos, arg, ok = freshPathCreatorBefore(p, 2, "dirA/fresh.txt", glusterfsProfile)
	if !ok || pos.CallIdx != 0 || arg != 0 {
		t.Fatalf("prior creator = %+v arg=%d ok=%v, want call 0 arg0", pos, arg, ok)
	}
}
