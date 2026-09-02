package prog

import (
	"strings"
	"testing"
)

// buildRefDiagProg builds a two-call program: creat (produces an fd) + read
// (consumes it via a ResultArg reference registered through MakeResultArg).
func buildRefDiagProg(t *testing.T, target *Target) (*Prog, *ResultArg) {
	t.Helper()
	var creatMeta, readMeta *Syscall
	for _, c := range target.Syscalls {
		switch c.Name {
		case "creat":
			creatMeta = c
		case "read":
			readMeta = c
		}
	}
	if creatMeta == nil || readMeta == nil {
		t.Skip("no creat/read in linux/amd64 target")
	}

	creat := &Call{Meta: creatMeta}
	creat.Args = make([]Arg, len(creatMeta.Args))
	for i, a := range creatMeta.Args {
		if rt, ok := a.Type.(*ResourceType); ok {
			creat.Args[i] = MakeResultArg(rt, DirIn, nil, 0)
		} else {
			creat.Args[i] = a.Type.DefaultArg(DirIn)
		}
	}
	creatRet := MakeResultArg(creatMeta.Ret, DirOut, nil, 0)
	creat.Ret = creatRet

	read := &Call{Meta: readMeta}
	read.Args = make([]Arg, len(readMeta.Args))
	for i, a := range readMeta.Args {
		if rt, ok := a.Type.(*ResourceType); ok {
			read.Args[i] = MakeResultArg(rt, DirIn, creatRet, 0)
		} else {
			read.Args[i] = a.Type.DefaultArg(DirIn)
		}
	}

	p := &Prog{Target: target, Calls: []*Call{creat, read}}
	return p, creatRet
}

func TestDumpRefDiagnosisTags(t *testing.T) {
	target := hmdfsSmokeTarget(t)

	// Sanity: a healthy program only produces OK tags.
	p, creatRet := buildRefDiagProg(t, target)
	out := p.DumpRefDiagnosis()
	if strings.Contains(out, "DANGLING") || strings.Contains(out, "FORWARD") || strings.Contains(out, "USES-MISSING") {
		t.Errorf("healthy program misdiagnosed:\n%s", out)
	}
	if !strings.Contains(out, "[OK]") {
		t.Errorf("healthy program missing OK tag:\n%s", out)
	}
	if creatRet.uses == nil || len(creatRet.uses) != 1 {
		t.Errorf("creat.Ret.uses not registered by MakeResultArg: %v", creatRet.uses)
	}

	// DANGLING: drop the provider call by direct slice manipulation (bypassing
	// RemoveCall's reference cleanup), leaving read's fd pointing nowhere.
	p2, ret2 := buildRefDiagProg(t, target)
	p2.Calls = p2.Calls[1:]
	out2 := p2.DumpRefDiagnosis()
	if !strings.Contains(out2, "[DANGLING]") {
		t.Errorf("dangling reference not detected:\n%s", out2)
	}
	if ret2.Res != nil || len(ret2.uses) != 1 {
		t.Errorf("diagnosis mutated provider state: Res=%v uses=%v", ret2.Res, ret2.uses)
	}

	// FORWARD: put the consumer before the provider; serialize registers vars
	// in call order, so this is a "no result" at serialize time.
	p3, _ := buildRefDiagProg(t, target)
	p3.Calls[0], p3.Calls[1] = p3.Calls[1], p3.Calls[0]
	out3 := p3.DumpRefDiagnosis()
	if !strings.Contains(out3, "[FORWARD]") {
		t.Errorf("forward reference not detected:\n%s", out3)
	}

	// USES-MISSING: Res pointer kept, but the uses map entry wiped — the
	// owner will not register its var and serialize panics on this referrer.
	p4, ret4 := buildRefDiagProg(t, target)
	ret4.uses = nil
	out4 := p4.DumpRefDiagnosis()
	if !strings.Contains(out4, "[USES-MISSING]") {
		t.Errorf("uses-map desync not detected:\n%s", out4)
	}
	// State fidelity: Res pointer untouched by diagnosis.
	if p4.Calls[1].Args[0].(*ResultArg).Res != ret4 {
		t.Errorf("diagnosis mutated consumer Res pointer")
	}
}
