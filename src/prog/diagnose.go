package prog

import (
	"fmt"
	"strings"
)

// Mutation diagnostics: the fuzzer hooks a sink (its appendDiagLog) so
// prog-internal mutation events land in the collected dag.log. Used to
// localize which mutation sub-branch produced a broken program.
var diagSink func(format string, args ...interface{})

func SetDiagSink(fn func(format string, args ...interface{})) {
	diagSink = fn
}

// ProgDiag emits a mutation-diagnostic line through the registered sink (no-op
// when unset). Keep call sites cheap: single line per mutation entry.
func ProgDiag(format string, args ...interface{}) {
	if diagSink != nil {
		diagSink(format, args...)
	}
}

// DumpRefDiagnosis builds a read-only report of every cross-call ResultArg
// reference in the program, classifying each against the invariants the text
// serializer (serializer.call/allocVarID) relies on:
//   - vars for a call's Ret are registered only when len(Ret.uses) != 0;
//   - a reference resolves only against Rets serialized BEFORE the referrer.
//
// Violations of either are exactly what panics with "no result" at
// ResultArg.serialize. The report never mutates the program: Res pointers and
// uses maps are only read, so the panic-time state stays intact for
// post-mortem reproduction.
//
// Per-reference tags:
//   - OK           — provider precedes the referrer and the uses map is in sync
//   - DANGLING     — the provider call is not part of this program (ownerIdx=-1)
//   - FORWARD      — the provider call appears after the referrer (ownerIdx>=i)
//   - USES-MISSING — the provider Ret.uses map does not record this referrer
//     (owner will not register its var; serialize will panic on it)
func (p *Prog) DumpRefDiagnosis() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[ref-diag] calls=%d\n", len(p.Calls))

	type retInfo struct {
		idx  int
		name string
		ret  *ResultArg
	}
	owners := make(map[*ResultArg]retInfo)
	for i, c := range p.Calls {
		owner := retInfo{idx: i, name: c.Meta.Name}
		if c.Ret != nil {
			owners[c.Ret] = owner
			fmt.Fprintf(&b, "[ref-diag] call[%d] %s: ret-uses=%d\n", i, c.Meta.Name, len(c.Ret.uses))
		} else {
			fmt.Fprintf(&b, "[ref-diag] call[%d] %s: no-ret\n", i, c.Meta.Name)
		}
	}

	tagOf := func(refIdx, ownerIdx int, referrer *ResultArg, owner retInfo) string {
		switch {
		case ownerIdx < 0:
			return "DANGLING"
		case ownerIdx >= refIdx:
			return "FORWARD"
		case !owner.ret.uses[referrer]:
			return "USES-MISSING"
		default:
			return "OK"
		}
	}

	for i, c := range p.Calls {
		check := func(referrer *ResultArg) {
			if referrer.Res == nil {
				return
			}
			owner, ok := owners[referrer.Res]
			ownerIdx := -1
			ownerName := "<none>"
			if ok {
				ownerIdx = owner.idx
				ownerName = owner.name
			}
			tag := "DANGLING"
			if ok {
				tag = tagOf(i, ownerIdx, referrer, owner)
			}
			fmt.Fprintf(&b, "[ref-diag] call[%d] %s arg -> call[%d] %s [%s]\n",
				i, c.Meta.Name, ownerIdx, ownerName, tag)
		}
		for _, arg := range c.Args {
			ForeachSubArg(arg, func(a Arg, _ *ArgCtx) {
				if ra, ok := a.(*ResultArg); ok {
					check(ra)
				}
			})
		}
		if c.Ret != nil && c.Ret.Res != nil {
			// Defensive: a Ret must not itself reference another result.
			check(c.Ret)
		}
	}
	return b.String()
}

// HasBrokenRefs reports whether any cross-call ResultArg reference is
// dangling (the provider call is absent from the program) or forward (the
// provider call appears at index >= the referrer). Either form panics at
// Serialize ("no result") because serializer vars are registered in call
// order. Used as a pre-execution guard after mutations.
func (p *Prog) HasBrokenRefs() bool {
	owners := make(map[*ResultArg]int)
	for i, c := range p.Calls {
		if c.Ret != nil {
			owners[c.Ret] = i
		}
	}
	for i, c := range p.Calls {
		broken := false
		check := func(a Arg, _ *ArgCtx) {
			if broken {
				return
			}
			if ra, ok := a.(*ResultArg); ok && ra.Res != nil {
				ownerIdx, ok := owners[ra.Res]
				if !ok || ownerIdx >= i {
					broken = true
				}
			}
		}
		for _, arg := range c.Args {
			ForeachSubArg(arg, check)
			if broken {
				return true
			}
		}
		if c.Ret != nil && c.Ret.Res != nil {
			// Defensive: a Ret must not reference another result.
			ownerIdx, ok := owners[c.Ret.Res]
			if !ok || ownerIdx >= i {
				return true
			}
		}
	}
	return false
}
