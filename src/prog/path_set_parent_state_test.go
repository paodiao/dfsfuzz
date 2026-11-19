package prog

import "testing"

func TestPathSetFreshParentCreatedSuccess(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["mkdir"]; !ok {
		t.Skip("target has no mkdir")
	}
	ft := glusterfsPlanTree()
	const parent = "dirA/newdir"

	mkdir := mkMkdir(target, parent)
	mkdir.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	ps := []*Prog{{Target: target, Calls: []*Call{mkdir}}}
	pos := GroupPosition{ProgIdx: 0, CallIdx: 1}
	if !pathSetFreshParentCreated(ps, ft, pos, parent) {
		t.Fatal("successful mkdir not accepted as fresh parent")
	}

	mkdir.CheckInfo.Retv = -1
	if pathSetFreshParentCreated(ps, ft, pos, parent) {
		t.Fatal("failed mkdir accepted as fresh parent")
	}

	mkdir.CheckInfo = nil
	if !pathSetFreshParentCreated(ps, ft, pos, parent) {
		t.Fatal("nil CheckInfo mkdir lost the assume-success fallback")
	}
}

func TestPathSetPlanParentMkdirSuccess(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["mkdir"]; !ok {
		t.Skip("target has no mkdir")
	}
	if _, ok := target.SyscallMap["open"]; !ok {
		t.Skip("target has no open")
	}
	ft := glusterfsPlanTree()
	const parent = "dirA/newdir"

	mkdir := mkMkdir(target, parent)
	mkdir.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	consumer := mkOpen(target, "dirA/newdir/file")
	consumer.CheckInfo = &FileMetadata{Stime: 200, Etime: 210}
	ps := []*Prog{{Target: target, Calls: []*Call{mkdir, consumer}}}
	mapped := []pathPlanMappedSpec{{
		pos:  GroupPosition{ProgIdx: 0, CallIdx: 0},
		spec: pathSpec{Path: parent, Kind: pathFresh, Role: pathRoleMkdir},
	}}
	consumerPos := GroupPosition{ProgIdx: 0, CallIdx: 1}
	var quality pathPlanQuality
	if !pathSetPlanParentValid(ps, mapped, ft, nil, consumerPos, parent, &quality) {
		t.Fatal("successful same-program mkdir rejected as parent")
	}

	mkdir.CheckInfo.Retv = -1
	if pathSetPlanParentValid(ps, mapped, ft, nil, consumerPos, parent, &quality) {
		t.Fatal("failed same-program mkdir accepted as parent")
	}

	mkdir.CheckInfo = nil
	if !pathSetPlanParentValid(ps, mapped, ft, nil, consumerPos, parent, &quality) {
		t.Fatal("nil CheckInfo same-program mkdir lost the assume-success fallback")
	}
}

func TestPathSetPlanParentCrossProgramMkdirSuccess(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["mkdir"]; !ok {
		t.Skip("target has no mkdir")
	}
	if _, ok := target.SyscallMap["open"]; !ok {
		t.Skip("target has no open")
	}
	ft := glusterfsPlanTree()
	const parent = "dirA/newdir"

	mkdir := mkMkdir(target, parent)
	mkdir.CheckInfo = &FileMetadata{Stime: 100, Etime: 110, Retv: 0}
	consumer := mkOpen(target, "dirA/newdir/file")
	consumer.CheckInfo = &FileMetadata{Stime: 200, Etime: 210}
	ps := []*Prog{
		{Target: target, Calls: []*Call{mkdir}},
		{Target: target, Calls: []*Call{consumer}},
	}
	mapped := []pathPlanMappedSpec{{
		pos:  GroupPosition{ProgIdx: 0, CallIdx: 0},
		spec: pathSpec{Path: parent, Kind: pathFresh, Role: pathRoleMkdir},
	}}
	consumerPos := GroupPosition{ProgIdx: 1, CallIdx: 0}
	tscoffs := []int64{0, 0}
	var quality pathPlanQuality
	if !pathSetPlanParentValid(ps, mapped, ft, tscoffs, consumerPos, parent, &quality) ||
		!quality.SoftParent {
		t.Fatal("successful cross-program mkdir not accepted as soft parent")
	}

	quality = pathPlanQuality{}
	mkdir.CheckInfo.Retv = -1
	if pathSetPlanParentValid(ps, mapped, ft, tscoffs, consumerPos, parent, &quality) {
		t.Fatal("failed cross-program mkdir accepted as soft parent")
	}

	quality = pathPlanQuality{}
	mkdir.CheckInfo = nil
	if pathSetPlanParentValid(ps, mapped, ft, tscoffs, consumerPos, parent, &quality) {
		t.Fatal("nil CheckInfo cross-program mkdir accepted without timing")
	}
}
