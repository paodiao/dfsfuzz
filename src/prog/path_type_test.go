package prog

import "testing"

func mkUnlinkCall(t *testing.T, target *Target, path string) *Call {
	t.Helper()
	meta := target.SyscallMap["unlink"]
	ptrType := meta.Args[0].Type.(*PtrType)
	data := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	ptr := MakePointerArg(ptrType, DirIn, 0, data)
	c := MakeCall(meta, nil)
	c.Args = []Arg{ptr}
	target.assignSizesCall(c)
	return c
}

func TestPathTypeBeforeSameProgram(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	ft := glusterfsPlanTree()
	if _, ok := target.SyscallMap["mkdir"]; !ok {
		t.Skip("target has no mkdir")
	}

	mkdir := mkMkdir(target, "dirA/newdir")
	p := &Prog{Target: target, Calls: []*Call{mkdir}}
	if isDir, known := pathTypeBefore([]*Prog{p}, 0, 1, 0, ft, "dirA/newdir"); !known || !isDir {
		t.Fatalf("fresh mkdir type = (%v,%v), want (true,true)", isDir, known)
	}

	creat := mkCreat(target, "dirA/newfile")
	p = &Prog{Target: target, Calls: []*Call{creat}}
	if isDir, known := pathTypeBefore([]*Prog{p}, 0, 1, 0, ft, "dirA/newfile"); !known || isDir {
		t.Fatalf("fresh creat type = (%v,%v), want (false,true)", isDir, known)
	}

	if isDir, known := pathTypeBefore([]*Prog{p}, 0, 0, 0, ft, "dirA"); !known || !isDir {
		t.Fatalf("baseline dir type = (%v,%v), want (true,true)", isDir, known)
	}

	unlink := mkUnlinkCall(t, target, "dirA/newdir")
	p = &Prog{Target: target, Calls: []*Call{mkdir, unlink}}
	if isDir, known := pathTypeBefore([]*Prog{p}, 0, 2, 0, ft, "dirA/newdir"); !known || isDir {
		t.Fatalf("deleted path type = (%v,%v), want (false,true)", isDir, known)
	}
}

func TestPathTypeBeforeCrossProgram(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	ft := glusterfsPlanTree()

	empty := &Prog{Target: target}
	dirCreator := &Prog{Target: target, Calls: []*Call{mkMkdir(target, "dirC/newdir")}}
	ps := []*Prog{empty, dirCreator}
	if isDir, known := pathTypeBefore(ps, 0, 0, 0, ft, "dirC/newdir"); !known || !isDir {
		t.Fatalf("cross-program mkdir type = (%v,%v), want (true,true)", isDir, known)
	}

	fileCreator := &Prog{Target: target, Calls: []*Call{mkCreat(target, "dirC/newfile")}}
	ps = []*Prog{empty, fileCreator}
	if isDir, known := pathTypeBefore(ps, 0, 0, 0, ft, "dirC/newfile"); !known || isDir {
		t.Fatalf("cross-program creat type = (%v,%v), want (false,true)", isDir, known)
	}

	rename := mkRenameCall(t, target, "dirC/newfile", "dirC/renamed", 0, 0)
	renamer := &Prog{Target: target, Calls: []*Call{fileCreator.Calls[0], rename}}
	ps = []*Prog{empty, renamer}
	if isDir, known := pathTypeBefore(ps, 0, 0, 0, ft, "dirC/renamed"); !known || isDir {
		t.Fatalf("cross-program rename type = (%v,%v), want (false,true)", isDir, known)
	}
}
