// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"monarch/pkg/log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Maximum length of generated binary blobs inserted into the program.
const maxBlobLen = uint64(100 << 10)

var (
	lastMutMu   sync.Mutex
	lastMutName string
)

// recordLastMutation remembers the most recent mutation entry point. Logged
// by the broken-ref guard on interception (localization without per-mutate
// log volume).
func recordLastMutation(name string) {
	lastMutMu.Lock()
	lastMutName = name
	lastMutMu.Unlock()
}

// LastMutation returns the most recent mutation entry point name.
func LastMutation() string {
	lastMutMu.Lock()
	defer lastMutMu.Unlock()
	return lastMutName
}

// Mutate program p.
//
// p:       The program to mutate.
// rs:      Random source.
// ncalls:  The allowed maximum calls in mutated program.
// ct:      ChoiceTable for syscalls.
// corpus:  The entire corpus, including original program p.
func (p *Prog) Mutate(rs rand.Source, ncalls int, ct *ChoiceTable, corpus [][]*Prog, sCalls *SpecialCalls,
	srvNum int, hasFail bool, enableC2san bool, hmcfg *Hmdfs_config) {
	recordLastMutation("standardMutate")
	r := newRand(p.Target, rs)
	r.hmcfg = hmcfg
	if ncalls < len(p.Calls) {
		ncalls = len(p.Calls)
	}
	ctx := &mutator{
		p:           p,
		r:           r,
		ncalls:      ncalls,
		ct:          ct,
		corpus:      corpus,
		sCalls:      sCalls,
		srvNum:      srvNum,
		enableC2san: enableC2san,
	}

	log.Logf(0, "mutate testcase hasFail=%v\n", hasFail)

	if hasFail {
		// 含故障的 prog：保留原有循环（splice 禁用以防破坏故障窗口，failpos 可用）。
		for stop, ok := false, false; !stop; stop = ok && len(p.Calls) != 0 && r.oneOf(3) {
			switch {
			case r.oneOf(5):
				//log.Logf(0, "----- squashAny()")
				// Not all calls have anything squashable,
				// so this has lower priority in reality.
				ok = ctx.squashAny()
			case r.nOutOf(1, 100):
				log.Logf(0, "----- splice()")
				ok = false
			case r.nOutOf(20, 31):
				log.Logf(0, "----- insertCall()")
				ok = ctx.insertCall()
			case r.nOutOf(10, 11):
				log.Logf(0, "----- mutateArg()")
				ok = ctx.mutateArg()
			case r.nOutOf(9, 10):
				log.Logf(0, "----- mutateFailPos()")
				ok = ctx.mutateFailPos()
			default:
				log.Logf(0, "----- removeCall()")
				ok = ctx.removeCall()
			}
		}
	} else {
		// 无故障的 prog：对齐上游 syzkaller——无 failpos 档，其预算全归
		// removeCall（default 拿剩余全部，removeCall ≈2.55%）。
		for stop, ok := false, false; !stop; stop = ok && len(p.Calls) != 0 && r.oneOf(3) {
			switch {
			case r.oneOf(5):
				//log.Logf(0, "----- squashAny()")
				// Not all calls have anything squashable,
				// so this has lower priority in reality.
				ok = ctx.squashAny()
			case r.nOutOf(1, 100):
				log.Logf(0, "----- splice()")
				ok = ctx.splice()
			case r.nOutOf(20, 31):
				log.Logf(0, "----- insertCall()")
				ok = ctx.insertCall()
			case r.nOutOf(10, 11):
				log.Logf(0, "----- mutateArg()")
				ok = ctx.mutateArg()
			default:
				log.Logf(0, "----- removeCall()")
				ok = ctx.removeCall()
			}
		}
	}
	p.sanitizeFix()
	p.debugValidate()
	if got := len(p.Calls); got < 1 || got > ncalls {
		panic(fmt.Sprintf("bad number of calls after mutation: %v, want [1, %v]", got, ncalls))
	}
}

// Internal state required for performing mutations -- currently this matches
// the arguments passed to Mutate().
type mutator struct {
	p           *Prog        // The program to mutate.
	r           *randGen     // The randGen instance.
	ncalls      int          // The allowed maximum calls in mutated program.
	ct          *ChoiceTable // ChoiceTable for syscalls.
	corpus      [][]*Prog    // The entire corpus, including original program p.
	initIp      string
	srvNum      int
	sCalls      *SpecialCalls
	enableC2san bool
}

// This function selects a random other program p0 out of the corpus, and
// mutates ctx.p as follows: preserve ctx.p's Calls up to a random index i
// (exclusive) concatenated with p0's calls from index i (inclusive).
func (ctx *mutator) splice() bool {
	p, r := ctx.p, ctx.r
	if len(ctx.corpus) == 0 || len(p.Calls) == 0 || len(p.Calls) >= ctx.ncalls {
		return false
	}
	//tao modified
	//p0 := ctx.corpus[r.Intn(len(ctx.corpus))]
	var p0 *Prog
	subTsNum := len(ctx.corpus[0]) - ctx.srvNum
	for {
		ps := ctx.corpus[r.Intn(len(ctx.corpus))]
		if !ps[0].HasCrashFail && !ps[0].HasNetFail {
			p0 = ps[r.Intn(subTsNum)+ctx.srvNum]
			break
		}
	}
	//log.Logf(0, "splice this program srvNum %d subTsNum %d:\n", ctx.srvNum, subTsNum)
	//logProgram(append(make([]*Prog, 0), p0))
	//tao end
	p0c := p0.Clone()
	idx := r.Intn(len(p.Calls))
	p.Calls = append(p.Calls[:idx], append(p0c.Calls, p.Calls[idx:]...)...)
	for i := len(p.Calls) - 1; i >= ctx.ncalls; i-- {
		p.RemoveCall(i)
	}
	return true
}

// Picks a random complex pointer and squashes its arguments into an ANY.
// Subsequently, if the ANY contains blobs, mutates a random blob.
func (ctx *mutator) squashAny() bool {
	p, r := ctx.p, ctx.r
	complexPtrs := p.complexPtrs()
	if len(complexPtrs) == 0 {
		return false
	}
	ptr := complexPtrs[r.Intn(len(complexPtrs))]
	if !p.Target.isAnyPtr(ptr.Type()) {
		p.Target.squashPtr(ptr)
	}
	var blobs []*DataArg
	var bases []*PointerArg
	ForeachSubArg(ptr, func(arg Arg, ctx *ArgCtx) {
		if data, ok := arg.(*DataArg); ok && arg.Dir() != DirOut {
			blobs = append(blobs, data)
			bases = append(bases, ctx.Base)
		}
	})
	if len(blobs) == 0 {
		return false
	}
	// TODO(dvyukov): we probably want special mutation for ANY.
	// E.g. merging adjacent ANYBLOBs (we don't create them,
	// but they can appear in future); or replacing ANYRES
	// with a blob (and merging it with adjacent blobs).
	idx := r.Intn(len(blobs))
	arg := blobs[idx]
	base := bases[idx]
	baseSize := base.Res.Size()
	arg.data = mutateData(r, arg.Data(), 0, maxBlobLen)
	// Update base pointer if size has increased.
	if baseSize < base.Res.Size() {
		s := analyze(ctx.ct, ctx.corpus, p, p.Calls[0])
		newArg := r.allocAddr(s, base.Type(), base.Dir(), base.Res.Size(), base.Res)
		*base = *newArg
	}
	return true
}

// Inserts a new call at a randomly chosen point (with bias towards the end of
// existing program). Does not insert a call if program already has ncalls.
func (ctx *mutator) insertCall() bool {
	p, r := ctx.p, ctx.r
	if len(p.Calls) >= ctx.ncalls {
		return false
	}
	idx := r.biasedRand(len(p.Calls)+1, 5)
	var c *Call
	if idx < len(p.Calls) {
		c = p.Calls[idx]
	}
	s := analyze(ctx.ct, ctx.corpus, p, c)
	calls := r.generateCall(s, p, idx, ctx.sCalls, ctx.enableC2san)
	if len(calls) == 0 {
		return false
	}
	p.insertBefore(c, calls)
	shiftSrvFailPos(p, idx, len(calls))
	for len(p.Calls) > ctx.ncalls {
		p.RemoveCall(idx)
		shiftSrvFailPos(p, idx, -1)
	}
	return true
}

// Removes a random call from program.
func (ctx *mutator) removeCall() bool {
	p, r := ctx.p, ctx.r
	if len(p.Calls) == 0 {
		return false
	}

	stop := false
	idx := 0
	cnt := 0
	for !stop {
		if cnt > 20 {
			return false
		}
		idx = r.Intn(len(p.Calls))
		if !strings.Contains(p.Calls[idx].Meta.Name, "syz_failure") {
			stop = true
		}
		cnt += 1
	}
	p.RemoveCall(idx)
	shiftSrvFailPos(p, idx, -1)
	return true
}

// Mutate an argument of a random call.
func (ctx *mutator) mutateArg() bool {
	p, r := ctx.p, ctx.r
	if len(p.Calls) == 0 {
		return false
	}

	stop := false
	idx := 0
	cnt := 0
	for !stop {
		idx = chooseCall(p, r)
		if idx < 0 || cnt > 20 {
			return false
		}
		if !strings.Contains(p.Calls[idx].Meta.Name, "syz_failure") {
			stop = true
		}
		cnt += 1
	}

	c := p.Calls[idx]
	updateSizes := true
	for stop, ok := false, false; !stop; stop = ok && r.oneOf(3) {
		ok = true
		ma := &mutationArgs{target: p.Target}
		ForeachArg(c, ma.collectArg)
		if len(ma.args) == 0 {
			return false
		}
		s := analyze(ctx.ct, ctx.corpus, p, c)
		arg, argCtx := ma.chooseArg(r.Rand)
		calls, ok1 := p.Target.mutateArg(r, s, arg, argCtx, &updateSizes)
		if !ok1 {
			ok = false
			continue
		}
		insertPos := idx
		p.insertBefore(c, calls)
		shiftSrvFailPos(p, insertPos, len(calls))
		idx += len(calls)
		for len(p.Calls) > ctx.ncalls {
			idx--
			p.RemoveCall(idx)
			shiftSrvFailPos(p, idx, -1)
		}
		if idx < 0 || idx >= len(p.Calls) || p.Calls[idx] != c {
			panic(fmt.Sprintf("wrong call index: idx=%v calls=%v p.Calls=%v ncalls=%v",
				idx, len(calls), len(p.Calls), ctx.ncalls))
		}
		if updateSizes {
			p.Target.assignSizesCall(c)
		}
	}
	return true
}

// Select a call based on the complexity of the arguments.
func chooseCall(p *Prog, r *randGen) int {
	var prioSum float64
	var callPriorities []float64
	for _, c := range p.Calls {
		var totalPrio float64
		ForeachArg(c, func(arg Arg, ctx *ArgCtx) {
			prio, stopRecursion := arg.Type().getMutationPrio(p.Target, arg, false)
			totalPrio += prio
			ctx.Stop = stopRecursion
		})
		prioSum += totalPrio
		callPriorities = append(callPriorities, prioSum)
	}
	if prioSum == 0 {
		return -1 // All calls are without arguments.
	}
	return sort.SearchFloat64s(callPriorities, prioSum*r.Float64())
}

func (target *Target) mutateArg(r *randGen, s *state, arg Arg, ctx ArgCtx, updateSizes *bool) ([]*Call, bool) {
	var baseSize uint64
	if ctx.Base != nil {
		baseSize = ctx.Base.Res.Size()
	}
	calls, retry, preserve := arg.Type().mutate(r, s, arg, ctx)
	if retry {
		return nil, false
	}
	if preserve {
		*updateSizes = false
	}
	// Update base pointer if size has increased.
	if base := ctx.Base; base != nil && baseSize < base.Res.Size() {
		newArg := r.allocAddr(s, base.Type(), base.Dir(), base.Res.Size(), base.Res)
		replaceArg(base, newArg)
	}
	return calls, true
}

func regenerate(r *randGen, s *state, arg Arg) (calls []*Call, retry, preserve bool) {
	var newArg Arg
	newArg, calls = r.generateArg(s, arg.Type(), arg.Dir())
	replaceArg(arg, newArg)
	return
}

func mutateInt(r *randGen, a *ConstArg, t *IntType) uint64 {
	switch {
	case r.nOutOf(1, 3):
		return a.Val + (uint64(r.Intn(4)) + 1)
	case r.nOutOf(1, 2):
		return a.Val - (uint64(r.Intn(4)) + 1)
	default:
		return a.Val ^ (1 << uint64(r.Intn(int(t.TypeBitSize()))))
	}
}

func mutateAlignedInt(r *randGen, a *ConstArg, t *IntType) uint64 {
	rangeEnd := t.RangeEnd
	if t.RangeBegin == 0 && int64(rangeEnd) == -1 {
		// Special [0:-1] range for all possible values.
		rangeEnd = uint64(1<<t.TypeBitSize() - 1)
	}
	index := (a.Val - t.RangeBegin) / t.Align
	misalignment := (a.Val - t.RangeBegin) % t.Align
	switch {
	case r.nOutOf(1, 3):
		index += uint64(r.Intn(4)) + 1
	case r.nOutOf(1, 2):
		index -= uint64(r.Intn(4)) + 1
	default:
		index ^= 1 << uint64(r.Intn(int(t.TypeBitSize())))
	}
	lastIndex := (rangeEnd - t.RangeBegin) / t.Align
	index %= lastIndex + 1
	return t.RangeBegin + index*t.Align + misalignment
}

func (t *IntType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	if r.bin() {
		return regenerate(r, s, arg)
	}
	a := arg.(*ConstArg)
	if t.Align == 0 {
		a.Val = mutateInt(r, a, t)
	} else {
		a.Val = mutateAlignedInt(r, a, t)
	}
	a.Val = truncateToBitSize(a.Val, t.TypeBitSize())
	return
}

func (t *FlagsType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	a := arg.(*ConstArg)
	for oldVal := a.Val; oldVal == a.Val; {
		a.Val = r.flags(t.Vals, t.BitMask, a.Val)
	}
	return
}

func (t *LenType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	if !r.mutateSize(arg.(*ConstArg), *ctx.Parent, ctx.Fields) {
		retry = true
		return
	}
	preserve = true
	return
}

func (t *ResourceType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	return regenerate(r, s, arg)
}

func (t *VmaType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	return regenerate(r, s, arg)
}

func (t *ProcType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	return regenerate(r, s, arg)
}

func (t *BufferType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	minLen, maxLen := uint64(0), maxBlobLen
	if t.Kind == BufferBlobRange {
		minLen, maxLen = t.RangeBegin, t.RangeEnd
	}
	a := arg.(*DataArg)
	if a.Dir() == DirOut {
		mutateBufferSize(r, a, minLen, maxLen)
		return
	}
	switch t.Kind {
	case BufferBlobRand, BufferBlobRange:
		data := append([]byte{}, a.Data()...)
		a.data = mutateData(r, data, minLen, maxLen)
	case BufferString:
		if len(t.Values) != 0 {
			a.data = r.randString(s, t)
		} else {
			if t.TypeSize != 0 {
				minLen, maxLen = t.TypeSize, t.TypeSize
			}
			data := append([]byte{}, a.Data()...)
			a.data = mutateData(r, data, minLen, maxLen)
		}
	case BufferFilename:
		a.data = []byte(r.filename(s, t))
	case BufferGlob:
		if len(t.Values) != 0 {
			a.data = r.randString(s, t)
		} else {
			a.data = []byte(r.filename(s, t))
		}
	case BufferText:
		data := append([]byte{}, a.Data()...)
		a.data = r.mutateText(t.Text, data)
	default:
		panic("unknown buffer kind")
	}
	return
}

func mutateBufferSize(r *randGen, arg *DataArg, minLen, maxLen uint64) {
	for oldSize := arg.Size(); oldSize == arg.Size(); {
		arg.size += uint64(r.Intn(33)) - 16
		if arg.size < minLen {
			arg.size = minLen
		}
		if arg.size > maxLen {
			arg.size = maxLen
		}
	}
}

func (t *ArrayType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	// TODO: swap elements of the array
	a := arg.(*GroupArg)
	count := uint64(0)
	switch t.Kind {
	case ArrayRandLen:
		if r.bin() {
			for count = uint64(len(a.Inner)); r.bin(); {
				count++
			}
		} else {
			for count == uint64(len(a.Inner)) {
				count = r.randArrayLen()
			}
		}
	case ArrayRangeLen:
		if t.RangeBegin == t.RangeEnd {
			panic("trying to mutate fixed length array")
		}
		for count == uint64(len(a.Inner)) {
			count = r.randRange(t.RangeBegin, t.RangeEnd)
		}
	}
	if count > uint64(len(a.Inner)) {
		for count > uint64(len(a.Inner)) {
			newArg, newCalls := r.generateArg(s, t.Elem, a.Dir())
			a.Inner = append(a.Inner, newArg)
			calls = append(calls, newCalls...)
			for _, c := range newCalls {
				s.analyze(c)
			}
		}
	} else if count < uint64(len(a.Inner)) {
		for _, arg := range a.Inner[count:] {
			removeArg(arg)
		}
		a.Inner = a.Inner[:count]
	}
	return
}

func (t *PtrType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	a := arg.(*PointerArg)
	if r.oneOf(1000) {
		removeArg(a.Res)
		index := r.rand(len(r.target.SpecialPointers))
		newArg := MakeSpecialPointerArg(t, a.Dir(), index)
		replaceArg(arg, newArg)
		return
	}
	newArg := r.allocAddr(s, t, a.Dir(), a.Res.Size(), a.Res)
	replaceArg(arg, newArg)
	return
}

func (t *StructType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	gen := r.target.SpecialTypes[t.Name()]
	if gen == nil {
		panic("bad arg returned by mutationArgs: StructType")
	}
	var newArg Arg
	newArg, calls = gen(&Gen{r, s}, t, arg.Dir(), arg)
	a := arg.(*GroupArg)
	for i, f := range newArg.(*GroupArg).Inner {
		replaceArg(a.Inner[i], f)
	}
	return
}

func (t *UnionType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	if gen := r.target.SpecialTypes[t.Name()]; gen != nil {
		var newArg Arg
		newArg, calls = gen(&Gen{r, s}, t, arg.Dir(), arg)
		replaceArg(arg, newArg)
		return
	}
	a := arg.(*UnionArg)
	index := r.Intn(len(t.Fields) - 1)
	if index >= a.Index {
		index++
	}
	optType, optDir := t.Fields[index].Type, t.Fields[index].Dir(a.Dir())
	removeArg(a.Option)
	var newOpt Arg
	newOpt, calls = r.generateArg(s, optType, optDir)
	replaceArg(arg, MakeUnionArg(t, a.Dir(), newOpt, index))
	return
}

func (t *CsumType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	panic("CsumType can't be mutated")
}

func (t *ConstType) mutate(r *randGen, s *state, arg Arg, ctx ArgCtx) (calls []*Call, retry, preserve bool) {
	panic("ConstType can't be mutated")
}

type mutationArgs struct {
	target        *Target
	ignoreSpecial bool
	prioSum       float64
	args          []mutationArg
	argsBuffer    [16]mutationArg
}

type mutationArg struct {
	arg      Arg
	ctx      ArgCtx
	priority float64
}

const (
	maxPriority = float64(10)
	minPriority = float64(1)
	dontMutate  = float64(0)
)

func (ma *mutationArgs) collectArg(arg Arg, ctx *ArgCtx) {
	ignoreSpecial := ma.ignoreSpecial
	ma.ignoreSpecial = false

	typ := arg.Type()
	prio, stopRecursion := typ.getMutationPrio(ma.target, arg, ignoreSpecial)
	ctx.Stop = stopRecursion

	if prio == dontMutate {
		return
	}

	_, isArrayTyp := typ.(*ArrayType)
	_, isBufferTyp := typ.(*BufferType)
	if !isBufferTyp && !isArrayTyp && arg.Dir() == DirOut || !typ.Varlen() && typ.Size() == 0 {
		return
	}

	if len(ma.args) == 0 {
		ma.args = ma.argsBuffer[:0]
	}
	ma.prioSum += prio
	ma.args = append(ma.args, mutationArg{arg, *ctx, ma.prioSum})
}

func (ma *mutationArgs) chooseArg(r *rand.Rand) (Arg, ArgCtx) {
	goal := ma.prioSum * r.Float64()
	chosenIdx := sort.Search(len(ma.args), func(i int) bool { return ma.args[i].priority >= goal })
	arg := ma.args[chosenIdx]
	return arg.arg, arg.ctx
}

// TODO: find a way to estimate optimal priority values.
// Assign a priority for each type. The boolean is the reference type and it has
// the minimum priority, since it has only two possible values.
func (t *IntType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	// For a integer without a range of values, the priority is based on
	// the number of bits occupied by the underlying type.
	plainPrio := math.Log2(float64(t.TypeBitSize())) + 0.1*maxPriority
	if t.Kind != IntRange {
		return plainPrio, false
	}

	size := t.RangeEnd - t.RangeBegin + 1
	if t.Align != 0 {
		if t.RangeBegin == 0 && int64(t.RangeEnd) == -1 {
			// Special [0:-1] range for all possible values.
			size = (1<<t.TypeBitSize()-1)/t.Align + 1
		} else {
			size = (t.RangeEnd-t.RangeBegin)/t.Align + 1
		}
	}
	switch {
	case size <= 15:
		// For a small range, we assume that it is effectively
		// similar with FlagsType and we need to try all possible values.
		prio = rangeSizePrio(size)
	case size <= 256:
		// We consider that a relevant range has at most 256
		// values (the number of values that can be represented on a byte).
		prio = maxPriority
	default:
		// Ranges larger than 256 are equivalent with a plain integer.
		prio = plainPrio
	}
	return prio, false
}

func (t *StructType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	if target.SpecialTypes[t.Name()] == nil || ignoreSpecial {
		return dontMutate, false
	}
	return maxPriority, true
}

func (t *UnionType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	if target.SpecialTypes[t.Name()] == nil && len(t.Fields) == 1 || ignoreSpecial {
		return dontMutate, false
	}
	// For a non-special type union with more than one option
	// we mutate the union itself and also the value of the current option.
	if target.SpecialTypes[t.Name()] == nil {
		return maxPriority, false
	}
	return maxPriority, true
}

func (t *FlagsType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	prio = rangeSizePrio(uint64(len(t.Vals)))
	if t.BitMask {
		// We want a higher priority because the mutation will include
		// more possible operations (bitwise operations).
		prio += 0.1 * maxPriority
	}
	return prio, false
}

// Assigns a priority based on the range size.
func rangeSizePrio(size uint64) (prio float64) {
	switch size {
	case 0:
		prio = dontMutate
	case 1:
		prio = minPriority
	default:
		// Priority proportional with the number of values. After a threshold, the priority is constant.
		// The threshold is 15 because most of the calls have <= 15 possible values for a flag.
		prio = math.Min(float64(size)/3+0.4*maxPriority, 0.9*maxPriority)
	}
	return prio
}

func (t *PtrType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	if arg.(*PointerArg).IsSpecial() {
		// TODO: we ought to mutate this, but we don't have code for this yet.
		return dontMutate, false
	}
	return 0.3 * maxPriority, false
}

func (t *ConstType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	return dontMutate, false
}

func (t *CsumType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	return dontMutate, false
}

func (t *ProcType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	return 0.5 * maxPriority, false
}

func (t *ResourceType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	return 0.5 * maxPriority, false
}

func (t *VmaType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	return 0.5 * maxPriority, false
}

func (t *LenType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	// Mutating LenType only produces "incorrect" results according to descriptions.
	return 0.1 * maxPriority, false
}

func (t *BufferType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	if arg.Dir() == DirOut && !t.Varlen() {
		return dontMutate, false
	}
	if t.Kind == BufferString && len(t.Values) == 1 {
		// These are effectively consts (and frequently file names).
		return dontMutate, false
	}
	return 0.8 * maxPriority, false
}

func (t *ArrayType) getMutationPrio(target *Target, arg Arg, ignoreSpecial bool) (prio float64, stopRecursion bool) {
	if t.Kind == ArrayRangeLen && t.RangeBegin == t.RangeEnd {
		return dontMutate, false
	}
	return maxPriority, false
}

func mutateData(r *randGen, data []byte, minLen, maxLen uint64) []byte {
	for stop := false; !stop; stop = stop && r.oneOf(3) {
		f := mutateDataFuncs[r.Intn(len(mutateDataFuncs))]
		data, stop = f(r, data, minLen, maxLen)
	}
	return data
}

// The maximum delta for integer mutations.
const maxDelta = 35

var mutateDataFuncs = [...]func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool){
	// TODO(dvyukov): duplicate part of data.
	// Flip bit in byte.
	func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool) {
		if len(data) == 0 {
			return data, false
		}
		byt := r.Intn(len(data))
		bit := r.Intn(8)
		data[byt] ^= 1 << uint(bit)
		return data, true
	},
	// Insert random bytes.
	func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool) {
		if len(data) == 0 || uint64(len(data)) >= maxLen {
			return data, false
		}
		n := r.Intn(16) + 1
		if r := int(maxLen) - len(data); n > r {
			n = r
		}
		pos := r.Intn(len(data))
		for i := 0; i < n; i++ {
			data = append(data, 0)
		}
		copy(data[pos+n:], data[pos:])
		for i := 0; i < n; i++ {
			data[pos+i] = byte(r.Int31())
		}
		if uint64(len(data)) > maxLen || r.bin() {
			data = data[:len(data)-n] // preserve original length
		}
		return data, true
	},
	// Remove bytes.
	func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool) {
		if len(data) == 0 {
			return data, false
		}
		n := r.Intn(16) + 1
		if n > len(data) {
			n = len(data)
		}
		pos := 0
		if n < len(data) {
			pos = r.Intn(len(data) - n)
		}
		copy(data[pos:], data[pos+n:])
		data = data[:len(data)-n]
		if uint64(len(data)) < minLen || r.bin() {
			for i := 0; i < n; i++ {
				data = append(data, 0) // preserve original length
			}
		}
		return data, true
	},
	// Append a bunch of bytes.
	func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool) {
		if uint64(len(data)) >= maxLen {
			return data, false
		}
		const max = 256
		n := max - r.biasedRand(max, 10)
		if r := int(maxLen) - len(data); n > r {
			n = r
		}
		for i := 0; i < n; i++ {
			data = append(data, byte(r.rand(256)))
		}
		return data, true
	},
	// Replace int8/int16/int32/int64 with a random value.
	func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool) {
		width := 1 << uint(r.Intn(4))
		if len(data) < width {
			return data, false
		}
		i := r.Intn(len(data) - width + 1)
		storeInt(data[i:], r.Uint64(), width)
		return data, true
	},
	// Add/subtract from an int8/int16/int32/int64.
	func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool) {
		width := 1 << uint(r.Intn(4))
		if len(data) < width {
			return data, false
		}
		i := r.Intn(len(data) - width + 1)
		v := loadInt(data[i:], width)
		delta := r.rand(2*maxDelta+1) - maxDelta
		if delta == 0 {
			delta = 1
		}
		if r.oneOf(10) {
			v = swapInt(v, width)
			v += delta
			v = swapInt(v, width)
		} else {
			v += delta
		}
		storeInt(data[i:], v, width)
		return data, true
	},
	// Set int8/int16/int32/int64 to an interesting value.
	func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool) {
		width := 1 << uint(r.Intn(4))
		if len(data) < width {
			return data, false
		}
		i := r.Intn(len(data) - width + 1)
		value := r.randInt64()
		if r.oneOf(10) {
			value = swap64(value)
		}
		storeInt(data[i:], value, width)
		return data, true
	},
}

func swap16(v uint16) uint16 {
	v0 := byte(v >> 0)
	v1 := byte(v >> 8)
	v = 0
	v |= uint16(v1) << 0
	v |= uint16(v0) << 8
	return v
}

func swap32(v uint32) uint32 {
	v0 := byte(v >> 0)
	v1 := byte(v >> 8)
	v2 := byte(v >> 16)
	v3 := byte(v >> 24)
	v = 0
	v |= uint32(v3) << 0
	v |= uint32(v2) << 8
	v |= uint32(v1) << 16
	v |= uint32(v0) << 24
	return v
}

func swap64(v uint64) uint64 {
	v0 := byte(v >> 0)
	v1 := byte(v >> 8)
	v2 := byte(v >> 16)
	v3 := byte(v >> 24)
	v4 := byte(v >> 32)
	v5 := byte(v >> 40)
	v6 := byte(v >> 48)
	v7 := byte(v >> 56)
	v = 0
	v |= uint64(v7) << 0
	v |= uint64(v6) << 8
	v |= uint64(v5) << 16
	v |= uint64(v4) << 24
	v |= uint64(v3) << 32
	v |= uint64(v2) << 40
	v |= uint64(v1) << 48
	v |= uint64(v0) << 56
	return v
}

func swapInt(v uint64, size int) uint64 {
	switch size {
	case 1:
		return v
	case 2:
		return uint64(swap16(uint16(v)))
	case 4:
		return uint64(swap32(uint32(v)))
	case 8:
		return swap64(v)
	default:
		panic(fmt.Sprintf("swapInt: bad size %v", size))
	}
}

func loadInt(data []byte, size int) uint64 {
	switch size {
	case 1:
		return uint64(data[0])
	case 2:
		return uint64(binary.LittleEndian.Uint16(data))
	case 4:
		return uint64(binary.LittleEndian.Uint32(data))
	case 8:
		return binary.LittleEndian.Uint64(data)
	default:
		panic(fmt.Sprintf("loadInt: bad size %v", size))
	}
}

func storeInt(data []byte, v uint64, size int) {
	switch size {
	case 1:
		data[0] = uint8(v)
	case 2:
		binary.LittleEndian.PutUint16(data, uint16(v))
	case 4:
		binary.LittleEndian.PutUint32(data, uint32(v))
	case 8:
		binary.LittleEndian.PutUint64(data, v)
	default:
		panic(fmt.Sprintf("storeInt: bad size %v", size))
	}
}

/*
	mutateFailPos()
*/

func (ctx *mutator) lastNNonFailCall(calls []*Call, idx int) (loc int) {
	for i := idx; i >= 0; i-- {
		if strings.Contains(calls[i].Meta.Name, "syz_failure") {
			return loc
		} else if ctx.r.nOutOf(8, 10) {
			return i
		}
	}
	loc = -1
	return loc
}

func (ctx *mutator) nextNNonFailCall(calls []*Call, idx int) int {
	for i := idx; i < len(calls); i++ {
		if !strings.Contains(calls[i].Meta.Name, "syz_failure") && ctx.r.nOutOf(6, 10) {
			return i
		}
	}
	return -1
}

func (ctx *mutator) mutateFailPos() bool {
	p, r := ctx.p, ctx.r
	if len(p.Calls) == 0 {
		return false
	}

	stop := false
	idx := 0
	cnt := 0
	insertPoint := 0
	for !stop {
		if cnt > 20 {
			return false
		}
		cnt += 1
		idx = r.Intn(len(p.Calls))
		if strings.Contains(p.Calls[idx].Meta.Name, "syz_failure_sync") {
			id := p.Calls[idx].Args[0].(*ConstArg).Val //TODO
			if id%2 == 0 {                             //failure start point
				//look up for an non-failure call upside
				insertPoint = ctx.lastNNonFailCall(p.Calls, idx)
				if insertPoint == -1 {
					continue
				}
				p.Calls = append(append(append(append(make([]*Call, 0), p.Calls[:insertPoint]...), p.Calls[idx]),
					p.Calls[insertPoint:idx]...), p.Calls[idx+1:]...)
				moveSrvFailPosEntry(p, idx, insertPoint)
				log.Logf(0, "insert call %v at pos %v\n", idx, insertPoint)
				stop = true
			} else {
				//look up for an non-failure call downside
				insertPoint = ctx.nextNNonFailCall(p.Calls, idx)
				if insertPoint == -1 {
					continue
				}
				p.Calls = append(p.Calls[:idx], append(append(append(make([]*Call, 0),
					p.Calls[idx+1:insertPoint+1]...), p.Calls[idx]), p.Calls[insertPoint+1:]...)...)
				moveSrvFailPosEntry(p, idx, insertPoint)
				log.Logf(0, "insert call %v at pos %v\n", idx, insertPoint)
				stop = true
			}
		}
	}
	return true
}

/*********************************** InsertFailures() *****************************************/
/*
	Example: recv(1); syz_down(); send(2); recv(3); syz_up(); send(4)
*/
func (ctx *mutator) genSrvFailCalls(startIdx *uint64, crashFailure bool, failInfo SrvFailInfo, ps []*Prog, idx int) {
	calls := make([]*Call, 0)
	calls = append(calls, ctx.genRecvCall(startIdx)...)
	calls = append(calls, ctx.genDownCall(crashFailure, failInfo, ps, idx)...)
	calls = append(calls, ctx.genSendCall(startIdx)...)
	calls = append(calls, ctx.genRecvCall(startIdx)...)
	calls = append(calls, ctx.genUpCall(crashFailure, failInfo)...)
	calls = append(calls, ctx.genSendCall(startIdx)...)
	ps[idx].insertEnd(calls)
}

func (ctx *mutator) genSyncCall(idxArg *uint64, cltArg int) (calls []*Call) {
	meta := ctx.r.target.Syscalls[ctx.sCalls.SyncfailId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: *idxArg}
	c.Args[1] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[1].Type.ref(), dir: DirIn}, Val: uint64(cltArg)}
	ctx.r.target.assignSizesCall(c)
	*idxArg = *idxArg + 1
	return append(calls, c)
}

func (ctx *mutator) genRecvCall(idxArg *uint64) (calls []*Call) {
	meta := ctx.r.target.Syscalls[ctx.sCalls.RecvId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: *idxArg}
	ctx.r.target.assignSizesCall(c)
	//*idxArg = *idxArg + 1
	return append(calls, c)
}

func (ctx *mutator) genSendCall(arg *uint64) (calls []*Call) {
	meta := ctx.r.target.Syscalls[ctx.sCalls.SendId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: *arg}
	ctx.r.target.assignSizesCall(c)
	*arg = *arg + 1
	return append(calls, c)
}

func genIptablesDropCmd(initIp string, nodes []int) string {
	bytes := strings.Split(initIp, ".")
	if len(bytes) != 4 {
		return ""
	}
	lastByte, err := strconv.Atoi(bytes[3])
	if err != nil {
		return ""
	}
	inputChanStr := ""
	outputChanStr := ""
	for _, node := range nodes {
		inputChanStr += fmt.Sprintf("iptables -A INPUT -s %s.%s.%s.%d -j DROP;",
			bytes[0], bytes[1], bytes[2], lastByte+node)
		outputChanStr += fmt.Sprintf("iptables -A OUTPUT -d %s.%s.%s.%d -j DROP;",
			bytes[0], bytes[1], bytes[2], lastByte+node)
	}
	return inputChanStr + outputChanStr
}

func (ctx *mutator) genNetCmd(failInfo SrvFailInfo) []byte {
	//PartNodes
	log.Logf(0, "part nodes: %v", failInfo.PartNodes)
	return []byte(genIptablesDropCmd(ctx.initIp, failInfo.PartNodes))
}

func (ctx *mutator) genDownCall(crashFailure bool, failInfo SrvFailInfo, ps []*Prog, idx int) []*Call {
	if crashFailure {
		meta := ctx.r.target.Syscalls[ctx.sCalls.DownId]
		calls := ctx.r.generateParticularCall(nil, meta)
		calls[len(calls)-1].IsFCall = true
		return calls
	} else {
		meta := ctx.r.target.Syscalls[ctx.sCalls.NetDownId]
		c := MakeCall(meta, nil)
		c.IsFCall = true
		// 必须在插入目标 prog（ps[idx]）的 state 上分配数据地址，
		// 否则独立 newState 从地址 0 分配，与目标 prog 数据重叠（L9）。
		s := stateFromProg(ps[idx])
		s.custData = append([]byte{}, ctx.genNetCmd(failInfo)...)
		c.Args, _ = ctx.r.generateArgs(s, meta.Args, DirIn)
		ctx.r.target.assignSizesCall(c)
		return append(make([]*Call, 0), c)
	}
}

func (ctx *mutator) genUpCall(crashFailure bool, failInfo SrvFailInfo) []*Call {
	callId := 0
	if crashFailure {
		callId = ctx.sCalls.UpId
	} else {
		callId = ctx.sCalls.NetUpId
	}
	meta := ctx.r.target.Syscalls[callId]
	calls := ctx.r.generateParticularCall(nil, meta)
	calls[len(calls)-1].IsFCall = true
	return calls
}

func (ctx *mutator) insertAtLast(cltSyncIdx *uint64, ps []*Prog, clt int) (loc int) {
	newCalls := ctx.genSyncCall(cltSyncIdx, clt)
	ps[clt].insertEnd(newCalls)
	return len(ps[clt].Calls) - 1
}

func (ctx *mutator) findAndInsert(cltSyncIdx *uint64, ps []*Prog, clt int, targetCall *Call) (loc int) {
	for idx, call := range ps[clt].Calls {
		if call == targetCall {
			newCalls := ctx.genSyncCall(cltSyncIdx, clt)
			ps[clt].insertBefore(ps[clt].Calls[idx], newCalls)
			loc = idx
			log.Logf(0, "findAndInsert at %v", loc)
			return loc
		}
	}
	log.Fatalf("findAndInsert failed: can't find the call")
	return
}

/*
Generate the insertable postion ranges for failure start sync and end sync.
1. The sync order of failures in all clients have to be the same.
2. Insert postions range from [0, callNum], the callNum-th postion means at the end of calls.
*/
func InsertablePos(ps1 []*Prog, clt int, srv int, srvNum int) ([]int, []int) {

	//func define: filter the insertable postions that are before failure calls
	filterFailureCalls := func(start int, end int, p *Prog) (posList []int) {
		i, callNum := start, len(p.Calls)
		for ; i <= end && i < callNum; i++ {
			//if !strings.Contains(p.Calls[i].Meta.Name, "syz_failure") {
			posList = append(posList, i)
			//}
		}
		if end >= callNum {
			posList = append(posList, callNum)
		}
		return posList
	}

	callNum := len(ps1[clt].Calls)
	startPosList, endPosList := make([]int, 0), make([]int, 0)

	//For the first client, we don't need to know the server failures orders
	if clt == srvNum {

		startPosList = filterFailureCalls(0, callNum, ps1[clt])
		endPosList = make([]int, len(startPosList))
		copy(endPosList, startPosList)

	} else {

		//func define: according to the server failure orders in the first client, get the insertion positions.
		getPosRange := func(srvFailOrder []int, p *Prog, curSrvIdx int) (posList []int) {

			srvFailPos := p.SrvFailPos
			CallNum := len(p.Calls)

			//func define
			arraySearch := func(srvFailPos [][]int, srv int) int {
				for _, item := range srvFailPos {
					if item[0] == srv {
						return item[1]
					}
				}
				return -1
			}

			posRange := []int{0, CallNum}
			for i := curSrvIdx; i >= 0; i-- {
				srv := srvFailOrder[i]
				ret := arraySearch(srvFailPos, srv)
				if ret != -1 {
					posRange[0] = ret + 1
					break
				}
			}

			for i := curSrvIdx; i < len(srvFailOrder); i++ {
				srv := srvFailOrder[i]
				ret := arraySearch(srvFailPos, srv)
				if ret != -1 {
					posRange[1] = ret
					break
				}
			}
			return filterFailureCalls(posRange[0], posRange[1], p)
		}

		//SrvFailOrder is descended ordered acrording to failure positions.
		for idx, s1 := range ps1[srvNum].SrvFailOrder {
			if s1 == srv*100+0*10+1 {
				startPosList = getPosRange(ps1[srvNum].SrvFailOrder, ps1[clt], idx)
			}
			if s1 == srv*100+0*10+2 {
				endPosList = getPosRange(ps1[srvNum].SrvFailOrder, ps1[clt], idx)
			}
		}
	}
	log.Logf(0, "filter Disconn Calls: %v %v, %v %v", clt, srv, startPosList, endPosList)
	return startPosList, endPosList
}

func (ctx *mutator) insertSync(start int, end int, cltSyncIdx *uint64, ps []*Prog, clt int, srv int) {
	loc1, loc2 := 0, 0
	log.Logf(0, "insertSync %v %v %v", start, end, len(ps[clt].Calls))

	if clt == 0 {
		log.Fatalf("clt is zero")
	}

	if start == -1 && end == -1 {

		loc1 = ctx.insertAtLast(cltSyncIdx, ps, clt)
		loc2 = ctx.insertAtLast(cltSyncIdx, ps, clt)

	} else {

		callNum := len(ps[clt].Calls)

		var endCall *Call
		if end < callNum {
			endCall = ps[clt].Calls[end]
		}

		if start < callNum {
			loc1 = ctx.findAndInsert(cltSyncIdx, ps, clt, ps[clt].Calls[start])
		} else {
			loc1 = ctx.insertAtLast(cltSyncIdx, ps, clt)
		}

		if end < callNum {
			loc2 = ctx.findAndInsert(cltSyncIdx, ps, clt, endCall)
		} else {
			loc2 = ctx.insertAtLast(cltSyncIdx, ps, clt)
		}
	}

	//The newly inserted sync syscalls effect the recorded locations of previous syncs, update them here.
	for idx, item := range ps[clt].SrvFailPos {
		loc := item[1]
		if loc >= start && loc >= end {
			loc += 2
		} else if loc >= start || loc >= end {
			loc += 1
		}
		ps[clt].SrvFailPos[idx][1] = loc
	}

	ps[clt].SrvFailPos = append(ps[clt].SrvFailPos, []int{srv*100 + 0*10 + 1, loc1}) //srv, failures, start/end
	ps[clt].SrvFailPos = append(ps[clt].SrvFailPos, []int{srv*100 + 0*10 + 2, loc2})
}

func LogProgram(ps []*Prog) {
	delimiter := []byte("---\n")
	var data []byte
	for _, p := range ps {
		data = append(data, p.Serialize()...)
		data = append(data, delimiter...)
	}
}

func (ctx *mutator) enumSyncPoint(ps1 []*Prog, clt int, srv int) (newPs [][]*Prog) {

	//If this client doesn't have syscalls, insert sync and return here
	if len(ps1[clt].Calls) == 0 {
		ps2 := Clones(ps1)
		cltSyncIdx := ps2[clt].SyncIdx
		ctx.insertSync(-1, -1, &cltSyncIdx, ps2, clt, srv)
		ps2[clt].SyncIdx = cltSyncIdx
		newPs = append(newPs, ps2)
		return newPs
	}

	/*
		Decide whether call1 is before call2, and there is only one normal syscall between them.
	*/
	isAdjacent := func(p *Prog, call1 int, call2 int) bool {
		if call1 > call2 {
			return false
		}
		/*
			cnt := 0
			for i:=call1; i<call2; i++ {
				name := p.Calls[i].Meta.Name
				length := len(name)
				if length > 11 && name[:11] == "syz_failure" {
					continue
				}
				cnt ++
			}
			if cnt == 1 {
				return true
			}
			return false
		*/
		return true
	}

	startPosList, endPosList := InsertablePos(ps1, clt, srv, ctx.srvNum)

	//Enumerate all possbile failure start and end synchronization points
	for _, call1 := range startPosList {
		for _, call2 := range endPosList {
			if isAdjacent(ps1[clt], call1, call2) {
				ps2 := Clones(ps1)
				cltSyncIdx := ps2[clt].SyncIdx
				ctx.insertSync(call1, call2, &cltSyncIdx, ps2, clt, srv)
				ps2[clt].SyncIdx = cltSyncIdx
				newPs = append(newPs, ps2)
			}
		}
	}
	return newPs
}

func extractOrder(srvFailPos [][]int) (srvs []int) {
	sort.SliceStable(srvFailPos, func(i, j int) bool {
		return srvFailPos[i][1] < srvFailPos[j][1]
	})

	//server orders according to start points
	for _, item := range srvFailPos {
		srvs = append(srvs, item[0])
	}
	log.Logf(0, "extractOrder: %v, %v", srvFailPos, srvs)
	return srvs
}

func InsertFailure(rs rand.Source, ncalls int, ct *ChoiceTable, ps []*Prog, srvComb []SrvFailInfo,
	ch chan []*Prog, sCalls *SpecialCalls,
	syncStartIdx uint64, crashFailure bool, initIp string, srvNum int, hmcfg *Hmdfs_config) {

	r := newRand(ps[0].Target, rs)
	r.hmcfg = hmcfg
	ctx := &mutator{
		r:      r,
		ncalls: ncalls,
		ct:     ct,
		sCalls: sCalls,
		initIp: initIp,
		srvNum: srvNum,
	}

	ps = Clones(ps)

	if crashFailure {
		ps[0].HasCrashFail = true
	} else {
		ps[0].HasNetFail = true
	}

	log.Logf(0, "InsertFailure: %v, %v", ps[0].HasCrashFail, ps[0].HasNetFail)

	srvSyncIdx := syncStartIdx
	for _, srvItem := range srvComb {
		srvIdx := srvItem.Srv
		ctx.genSrvFailCalls(&srvSyncIdx, crashFailure, srvItem, ps, srvIdx)
	}

	queue := make([][]*Prog, 0)
	queue = append(queue, ps)

	for clt := srvNum; clt < len(ps); clt++ {
		for _, srvItem := range srvComb {
			tmpQueue := make([][]*Prog, 0)
			for _, ps1 := range queue {
				//Extract the failures orders of servers
				if clt > srvNum && ps1[srvNum].SrvFailOrder == nil {
					ps1[srvNum].SrvFailOrder = extractOrder(ps1[srvNum].SrvFailPos)
				}
				//Enumerate the sync betwen one client and 1 failures of the server
				ret := ctx.enumSyncPoint(ps1, clt, srvItem.Srv)
				tmpQueue = append(tmpQueue, ret...)
			}
			queue = tmpQueue
			log.Logf(0, "clt %v srv %v queue %v", clt, srvItem.Srv, len(queue))
		}
	}
	log.Logf(0, "failure queue %v", len(queue))
	for _, ps1 := range queue {
		log.Logf(0, "send to channel: %v, %v", ps1[0].HasCrashFail, ps1[0].HasNetFail)
		//logProgram(ps1)
		ch <- ps1
	}
}

/******************************* RandomInsertFailure() ************************************/
func RandomInsertFailure(ps []*Prog, srvNum int, rs rand.Source, sCalls *SpecialCalls, initIp string, hmcfg *Hmdfs_config, nodecrash bool, netfailure bool) {

	if srvNum <= 0 {
		// No server nodes (e.g. hmdfs with server_num=0): server fault
		// injection is meaningless and RandSet(0, srvNum-1, ...) would panic.
		// Network failure calls are still inserted by the hmdfs generators.
		return
	}

	log.Logf(0, "RandomInsertFailure()\n")

	r := newRand(ps[0].Target, rs)
	r.hmcfg = hmcfg
	srvStartIdx := uint64(0)

	ctx := &mutator{
		r:      r,
		sCalls: sCalls,
		initIp: initIp,
		srvNum: srvNum,
	}

	randomSrvs := r.RandSet(0, srvNum-1, r.Intn(srvNum)+1)
	log.Logf(0, "failed servers: %v\n", randomSrvs)
	for i, srv := range randomSrvs {

		crashFail := r.nOutOf(1, 2)
		if !netfailure && nodecrash {
			crashFail = true
		} else if netfailure && !nodecrash {
			crashFail = false
		} else if !netfailure && !nodecrash {
			break
		}
		failInfo := SrvFailInfo{srv, make([]int, 0)}
		if !crashFail {
			failInfo.PartNodes = r.RandSetExcept(0, len(ps)-1, r.Intn(len(ps))+1, srv)
		}
		ctx.genSrvFailCalls(&srvStartIdx, crashFail, failInfo, ps, srv)

		for clt := srvNum; clt < len(ps); clt++ {
			cltSyncIdx := uint64(0)
			if i != 0 {
				cltSyncIdx = ps[clt].SyncIdx
			}

			//Extract the failures orders of servers
			if clt > srvNum && ps[srvNum].SrvFailOrder == nil {
				ps[srvNum].SrvFailOrder = extractOrder(ps[srvNum].SrvFailPos)
			}

			//func define
			randSelectCandidates := func(startPosList []int, endPosList []int) (int, int) {
				candidates := make([][]int, 0)
				for _, call1 := range startPosList {
					for _, call2 := range endPosList {
						if call1 <= call2 {
							candidates = append(candidates, []int{call1, call2})
						}
					}
				}
				if len(candidates) == 0 {
					log.Fatalf("random insert failure: there is no positions")
				}
				randIdx := r.Intn(len(candidates))
				startPos, endPos := candidates[randIdx][0], candidates[randIdx][1]
				return startPos, endPos
			}

			startPosList, endPosList := InsertablePos(ps, clt, srv, srvNum)
			startPos, endPos := randSelectCandidates(startPosList, endPosList)

			ctx.insertSync(startPos, endPos, &cltSyncIdx, ps, clt, srv)
			ps[clt].SyncIdx = cltSyncIdx
		}
		if crashFail {
			ps[0].HasCrashFail = true
		} else {
			ps[0].HasNetFail = true
		}
	}
}

/*************** Insert crashes to servers and clients for crash consistency bugs *****************/

func InsertSrvCrash(p *Prog, r *randGen, sCalls *SpecialCalls) {
	meta := r.target.Syscalls[sCalls.CrashServer]
	c := MakeCall(meta, nil)
	c.Args = make([]Arg, len(meta.Args))
	r.target.assignSizesCall(c)
	p.Calls = append(p.Calls, c)
}

func InsertCltCrash(p *Prog, willCrash int, r *randGen, sCalls *SpecialCalls) {
	meta := r.target.Syscalls[sCalls.CrashClient]
	c := MakeCall(meta, nil)
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: uint64(willCrash)}
	r.target.assignSizesCall(c)
	p.Calls = append(p.Calls, c)
}

func ProgCrashAll(ps []*Prog, servNum int, r *randGen, sCalls *SpecialCalls) []*Prog {
	ps1 := Clones(ps)
	ps1[0].C2test = true
	for idx, p := range ps1 {
		if idx < servNum {
			InsertSrvCrash(p, r, sCalls)
		} else {
			InsertCltCrash(p, 1, r, sCalls)
		}
	}
	return ps1
}

func ProgCrashRand(ps []*Prog, servNum int, r *randGen, sCalls *SpecialCalls) []*Prog {
	ps1 := Clones(ps)
	ps1[0].C2test = true
	//psLen := len(ps1)
	psIdx := r.RandSet(0, servNum-1, r.Intn(servNum)+1)
	for _, idx := range psIdx {
		if idx < servNum {
			InsertSrvCrash(ps1[idx], r, sCalls)
		}
	}

	for i := servNum; i < len(ps1); i++ {
		if IsIn(psIdx, i) {
			InsertCltCrash(ps1[i], 1, r, sCalls)
		} else {
			InsertCltCrash(ps1[i], 0, r, sCalls)
		}
	}
	return ps1
}

/******************************* Stash Seed Mutation ************************************/

const (
	StashMutFailPos = iota
	StashMutWriteParams
	StashMutSyncPos
	StashMutOpSequence
	StashMutTargetNodes
	StashMutTypeCount
)

func MutateStashProg(ps []*Prog, rs rand.Source, ct *ChoiceTable, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	if len(ps) == 0 {
		return false
	}

	r := newRand(ps[0].Target, rs)
	r.hmcfg = hmcfg

	mutType := r.Intn(StashMutTypeCount)

	switch mutType {
	case StashMutFailPos:
		return mutateStashFailPos(ps, r, sCalls)
	case StashMutWriteParams:
		return mutateStashWriteParams(ps, r, sCalls)
	case StashMutSyncPos:
		return mutateStashSyncPos(ps, r, sCalls)
	case StashMutOpSequence:
		return mutateStashOpSequence(ps, r, sCalls, ct)
	case StashMutTargetNodes:
		return mutateStashTargetNodes(ps, r, sCalls, hmcfg)
	}
	return false
}

func mutateStashFailPos(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for _, p := range ps {
		if !p.HasNetFail || len(p.GeneralFailPos) == 0 {
			continue
		}

		for _, failPos := range p.GeneralFailPos {
			if len(failPos) >= 4 {
				failStart := failPos[1]
				failEnd := failPos[3]

				if failStart <= 0 || failEnd <= failStart {
					continue
				}

				shift := r.Intn(3) - 1
				if shift == 0 {
					shift = 1
				}

				newFailStart := failStart + shift
				newFailEnd := failEnd + shift
				if newFailStart < 1 {
					newFailStart = 1
					newFailEnd = newFailStart + (failEnd - failStart)
				}

				if !moveFailCalls(p, failStart, newFailStart) {
					continue
				}
				failPos[1] = newFailStart
				failPos[3] = newFailEnd
				return true
			}
		}
	}
	return false
}

func moveFailCalls(p *Prog, oldPos, newPos int) bool {
	if oldPos == newPos || oldPos < 0 || newPos < 0 {
		return false
	}
	if oldPos+6 > len(p.Calls) || newPos+6 > len(p.Calls) {
		return false
	}

	failCalls := p.Calls[oldPos : oldPos+6]
	remainingBefore := p.Calls[:oldPos]
	remainingAfter := p.Calls[oldPos+6:]

	if newPos < oldPos {
		p.Calls = append(append(append([]*Call{}, remainingBefore[:newPos]...), failCalls...), append(remainingBefore[newPos:], remainingAfter...)...)
	} else {
		delta := newPos - oldPos
		p.Calls = append(append(append([]*Call{}, remainingBefore[:oldPos]...), remainingAfter[:delta]...), append(failCalls, remainingAfter[delta:]...)...)
	}
	return true
}

func mutateStashWriteParams(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for _, p := range ps {
		if !p.IsStash {
			continue
		}

		pwriteCalls := findCallsByName(p, "pwrite64")
		if len(pwriteCalls) == 0 {
			continue
		}

		callIdx := pwriteCalls[r.Intn(len(pwriteCalls))]
		call := p.Calls[callIdx]

		mutType := r.Intn(3)
		switch mutType {
		//TODO: 这里的问题就是设置的值的突变范围合不合理
		case 0:
			return mutateWriteOffset(ps, p, call, r)
		case 1:
			return mutateWriteLength(ps, p, call, r)
		case 2:
			return mutateWriteData(ps, p, call, r)
		}
	}
	return false
}

// syncStashReadVerification keeps the read-verification calls in sync with a
// mutated write: every pread64 that reads the old (offset, length) region of
// the same file is moved to the new region (count, out-buffer size and
// offset), so the write-fault-read loop stays closed. Reads of other regions
// and other files are untouched; an unknown writePath ("") skips the file
// check (legacy behaviour).
func syncStashReadVerification(ps []*Prog, writePath string, oldOff, oldLen, newOff, newLen uint64) {
	for _, p := range ps {
		for _, c := range p.Calls {
			if !strings.Contains(c.Meta.Name, "pread64") {
				continue
			}
			if len(c.Args) < 4 {
				continue
			}
			posArg, ok := c.Args[3].(*ConstArg)
			if !ok {
				continue
			}
			cntArg, ok := c.Args[2].(*ConstArg)
			if !ok {
				continue
			}
			if posArg.Val != oldOff || cntArg.Val != oldLen {
				continue
			}
			if writePath != "" {
				if cPath := openPathBehindFdCall(p, c); cPath != "" && cPath != writePath {
					continue
				}
			}
			posArg.Val = newOff
			cntArg.Val = newLen
			if ptrArg, ok := c.Args[1].(*PointerArg); ok {
				if dataArg, ok := ptrArg.Res.(*DataArg); ok && dataArg.Dir() == DirOut {
					dataArg.size = newLen
				}
			}
		}
	}
}

// reassignDataArg 重新分配 DataArg（改 data 后调用）——新地址按新长度分配。
// 消除 copyin 越界覆盖（原分配区按旧长度——data 变长后越界，L7）。
// 必须在主 prog 的 state（stateFromProg）上分配，否则独立 newState 从地址 0
// 重新分配，与主 prog 已有数据重叠（L9）。
// reassignDataArgAt replaces the DataArg behind call.Args[argIdx] with a
// freshly allocated buffer in p's address space. An in-place rewrite can
// outgrow the pointer's 64-byte-granule reservation and spill into the next
// argument's copyin region (the L6 class), so path/buffer rewrites must go
// through here.
func reassignDataArgAt(p *Prog, call *Call, argIdx int, r *randGen, newData []byte) bool {
	if p == nil || call == nil || argIdx < 0 || len(call.Args) <= argIdx {
		return false
	}
	ptrArg, ok := call.Args[argIdx].(*PointerArg)
	if !ok {
		return false
	}
	ptrType, ok := ptrArg.Type().(*PtrType)
	if !ok {
		return false
	}
	// Require a flat DataArg pointee (path/buffer rewrites always have one):
	// a non-data pointee must not be silently replaced by a data buffer.
	// Preserve the buffer's own direction: write buffers are DirIn, read
	// buffers (buffer[out]) are DirOut. MakePointerArg forces DirIn on the
	// pointer itself, so the old data arg is the only source of truth;
	// forcing DirIn here would flip a read buffer and drop its copyout.
	// Output buffers need MakeOutDataArg: MakeDataArg panics on DirOut.
	old, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return false
	}
	dir := old.Dir()
	s := stateFromProg(p)
	var newDataArg *DataArg
	if dir == DirOut {
		newDataArg = MakeOutDataArg(ptrType.Elem, DirOut, uint64(len(newData)))
	} else {
		newDataArg = MakeDataArg(ptrType.Elem, DirIn, newData)
	}
	newPtr := r.allocAddr(s, ptrType, DirIn, newDataArg.Size(), newDataArg)
	// Replace the pointer's fields (new address + flat DataArg pointee).
	// Assigning ptrArg.Res = newPtr would nest a PointerArg under the old
	// pointer: the pointee must be ptrType.Elem, not another pointer.
	*ptrArg = *newPtr
	return true
}

// reassignDataArg is the write-buffer helper (call.Args[1]).
func reassignDataArg(p *Prog, call *Call, r *randGen, newData []byte) bool {
	return reassignDataArgAt(p, call, 1, r, newData)
}

// openPathBehindFdCall returns the path of the open/creat call that produced an
// fd-taking call's fd, or "" when the producer is not in owner. Stash progs
// open each file once, so the path is the file identity that keeps writes and
// their verification reads on the same file.
func openPathBehindFdCall(owner *Prog, call *Call) string {
	if owner == nil || call == nil || len(call.Args) == 0 {
		return ""
	}
	fdArg, ok := call.Args[0].(*ResultArg)
	if !ok || fdArg.Res == nil {
		return ""
	}
	for _, c := range owner.Calls {
		if c == nil || c.Ret == nil {
			continue
		}
		name := c.Meta.Name
		if (strings.Contains(name, "open") || strings.Contains(name, "creat")) && c.Ret == fdArg.Res {
			return extractPathFromCall(c)
		}
	}
	return ""
}

func mutateWriteOffset(ps []*Prog, owner *Prog, call *Call, r *randGen) bool {
	if len(call.Args) < 4 {
		return false
	}

	posArg, ok := call.Args[3].(*ConstArg)
	if !ok {
		return false
	}
	cntArg, ok := call.Args[2].(*ConstArg)
	if !ok {
		return false
	}
	oldOff := posArg.Val
	oldLen := cntArg.Val

	delta := r.Intn(1025) - 512
	if delta == 0 {
		delta = 1
	}
	newVal := int64(posArg.Val) + int64(delta)
	if newVal < 0 {
		newVal = 0
	}
	posArg.Val = uint64(newVal)

	syncStashReadVerification(ps, openPathBehindFdCall(owner, call), oldOff, oldLen, posArg.Val, oldLen)
	return true
}

func mutateWriteLength(ps []*Prog, owner *Prog, call *Call, r *randGen) bool {
	if len(call.Args) < 4 {
		return false
	}

	countArg, ok := call.Args[2].(*ConstArg)
	if !ok {
		return false
	}
	posArg, ok := call.Args[3].(*ConstArg)
	if !ok {
		return false
	}
	oldOff := posArg.Val
	oldLen := countArg.Val

	delta := r.Intn(257) - 128
	if delta == 0 {
		delta = 1
	}
	newLen := int64(countArg.Val) + int64(delta)
	if newLen < 1 {
		newLen = 1
	}
	if newLen > 8192 {
		newLen = 8192
	}
	ptrArg, ok := call.Args[1].(*PointerArg)
	if !ok {
		return false
	}
	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return false
	}
	oldData := dataArg.Data()
	newData := make([]byte, newLen)
	copy(newData, oldData)
	for i := len(oldData); i < int(newLen); i++ {
		newData[i] = byte(r.Intn(256))
	}
	if !reassignDataArg(owner, call, r, newData) {
		return false
	}
	countArg.Val = uint64(newLen)

	syncStashReadVerification(ps, openPathBehindFdCall(owner, call), oldOff, oldLen, oldOff, countArg.Val)
	return true
}

func mutateWriteData(ps []*Prog, owner *Prog, call *Call, r *randGen) bool {
	if len(call.Args) < 4 {
		return false
	}

	ptrArg, ok := call.Args[1].(*PointerArg)
	if !ok {
		return false
	}

	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return false
	}

	countArg, ok := call.Args[2].(*ConstArg)
	if !ok {
		return false
	}

	posArg, ok := call.Args[3].(*ConstArg)
	if !ok {
		return false
	}

	data := dataArg.Data()
	if len(data) == 0 {
		return false
	}
	oldOff := posArg.Val
	oldLen := countArg.Val

	data = mutateData(r, data, 1, 8192)
	// Failure is unreachable here (Args[1] was checked to be a *PtrType
	// pointer above), so the in-place mutateData cannot leave a partial
	// mutation behind.
	if !reassignDataArg(owner, call, r, data) {
		return false
	}
	countArg.Val = uint64(len(data))

	syncStashReadVerification(ps, openPathBehindFdCall(owner, call), oldOff, oldLen, oldOff, countArg.Val) // 读验证同步（L7）
	return true
}

func mutateStashSyncPos(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for progIdx, p := range ps {
		if !p.IsStash {
			continue
		}

		syncCalls := findCallsByName(p, "syz_failure_sync")
		if len(syncCalls) == 0 {
			continue
		}

		type syncInfo struct {
			callIdx int
			syncId  int
		}
		var syncInfos []syncInfo
		for _, callIdx := range syncCalls {
			if callIdx >= len(p.Calls) {
				continue
			}
			syncId := extractSyncId(p.Calls[callIdx])
			if syncId >= 0 {
				syncInfos = append(syncInfos, syncInfo{callIdx: callIdx, syncId: syncId})
			}
		}

		if len(syncInfos) == 0 {
			continue
		}

		chosenIdx := r.Intn(len(syncInfos))
		chosen := syncInfos[chosenIdx]

		shift := r.Intn(3) - 1
		if shift == 0 {
			shift = 1
		}

		newCallIdx := chosen.callIdx + shift
		if newCallIdx < 1 || newCallIdx >= len(p.Calls)-1 {
			continue
		}
		/*if isSpecialCall(p.Calls[newCallIdx]) {
			continue
		}*/

		pairSyncId := -1
		if chosen.syncId%2 == 0 {
			pairSyncId = chosen.syncId + 1
		} else {
			pairSyncId = chosen.syncId - 1
		}

		var pairCallIdx int = -1
		for _, info := range syncInfos {
			if info.syncId == pairSyncId {
				pairCallIdx = info.callIdx
				break
			}
		}

		if pairCallIdx >= 0 {
			if chosen.syncId%2 == 0 && newCallIdx >= pairCallIdx {
				continue
			}
			if chosen.syncId%2 == 1 && newCallIdx <= pairCallIdx {
				continue
			}
		}

		call := p.Calls[chosen.callIdx]
		p.Calls = append(p.Calls[:chosen.callIdx], p.Calls[chosen.callIdx+1:]...)

		if shift < 0 {
			p.Calls = append(p.Calls[:newCallIdx], append([]*Call{call}, p.Calls[newCallIdx:]...)...)
		} else {
			p.Calls = append(p.Calls[:newCallIdx], append([]*Call{call}, p.Calls[newCallIdx:]...)...)
		}

		if len(ps) > 0 && len(ps[0].GeneralFailPos) > 0 {
			updateGeneralFailPos(ps[0], progIdx, chosen.syncId, newCallIdx)
		}

		return true
	}
	return false
}

func extractSyncId(call *Call) int {
	if len(call.Args) < 1 {
		return -1
	}
	constArg, ok := call.Args[0].(*ConstArg)
	if !ok {
		return -1
	}
	return int(constArg.Val)
}

func isSpecialCall(call *Call) bool {
	name := call.Meta.Name
	return strings.Contains(name, "syz_failure") || strings.Contains(name, "syz_failure_sync")
}

func updateGeneralFailPos(p *Prog, progIdx, syncId, newPos int) {
	targetIdx := progIdx*100 + syncId + 1

	for _, failPos := range p.GeneralFailPos {
		for i := 0; i < len(failPos)-1; i++ {
			if failPos[i] == targetIdx {
				failPos[i+1] = newPos
				return
			}
		}
	}
}

// RefreshGeneralFailPos 在通用突变（Prog.Mutate，含 hmdfs fallback）后，按被
// 突变 prog 中 syz_failure_sync 调用的实际位置重定位（只更新已存在键）
// GeneralFailPos 条目。表挂在 ps[0]；progIdx==0 的条目是失败窗口块（非 sync
// 调用），不在此处理。
// 约定：prog 内 syz_failure_sync 的 id 0/1 对应键 progIdx*100+1/+2（生成器每
// prog 恰好一对 start/end；重复 id 时后者覆盖）。
func RefreshGeneralFailPos(ps []*Prog, progIdx int) {
	if len(ps) == 0 || progIdx <= 0 || progIdx >= len(ps) {
		return
	}
	if len(ps[0].GeneralFailPos) == 0 {
		return
	}
	for i, c := range ps[progIdx].Calls {
		if c == nil || c.Meta == nil {
			continue
		}
		if strings.Contains(c.Meta.Name, "syz_failure_sync") {
			if id := extractSyncId(c); id >= 0 {
				updateGeneralFailPos(ps[0], progIdx, id, i)
			}
		}
	}
}

func mutateStashOpSequence(ps []*Prog, r *randGen, sCalls *SpecialCalls, ct *ChoiceTable) bool {
	for i, p := range ps {
		if !p.IsStash {
			continue
		}

		mutType := r.Intn(2)
		switch mutType {
		case 0:
			return insertStashCall(ps, p, r, sCalls, ct)
		case 1:
			return removeStashCall(ps, i, r)
		}
	}
	return false
}

func insertStashCall(ps []*Prog, p *Prog, r *randGen, sCalls *SpecialCalls, ct *ChoiceTable) bool {
	if len(p.Calls) == 0 {
		return false
	}

	type fdRange struct {
		openIdx  int
		closeIdx int
		fd       *ResultArg
	}
	var fdRanges []fdRange

	openCalls := findCallsByName(p, "open")
	closeCalls := findCallsByName(p, "close")

	for _, openIdx := range openCalls {
		if openIdx >= len(p.Calls) {
			continue
		}
		openCall := p.Calls[openIdx]
		fd := extractFdFromCall(openCall)
		if fd == nil {
			continue
		}
		// The close lookup must compare against the original open return
		// object (usesFd compares arg.Res): a fresh wrapper never matches and
		// silently extends the insertion window past the real close, letting
		// an fd call land after its close (EBADF).
		correspondingClose := -1
		for _, closeIdx := range closeCalls {
			if closeIdx > openIdx {
				closeCall := p.Calls[closeIdx]
				if usesFd(closeCall, fd) {
					correspondingClose = closeIdx
					break
				}
			}
		}

		if correspondingClose == -1 {
			correspondingClose = len(p.Calls)
		}

		fdRanges = append(fdRanges, fdRange{openIdx: openIdx, closeIdx: correspondingClose, fd: fd})
	}

	if len(fdRanges) == 0 {
		return false
	}

	chosenRange := fdRanges[r.Intn(len(fdRanges))]

	callTypes := []string{"pwrite64", "pread64", "fsync", "fdatasync"}
	callType := callTypes[r.Intn(len(callTypes))]

	var newCall *Call
	switch callType {
	case "pwrite64":
		newCall = genPwriteCallWithFd(p, r, sCalls, chosenRange.fd)
	case "pread64":
		newCall = genPreadCallWithFd(p, r, sCalls, chosenRange.fd)
	case "fsync":
		newCall = genFsyncCallWithFd(p, r, sCalls, chosenRange.fd)
	case "fdatasync":
		newCall = genFdatasyncCallWithFd(p, r, sCalls, chosenRange.fd)
	}

	if newCall == nil {
		return false
	}

	validStart := chosenRange.openIdx + 1
	validEnd := chosenRange.closeIdx
	if validStart >= validEnd {
		return false
	}

	// 故障窗口区间（p0 的 recv/down/send/recv/up/send 6 调用块）——插入不能
	// 落入窗口内部（窗口变长后 moveFailCalls 的 6 块移动会拆散 recv/send 对，
	// 否则死锁）；窗口前插入时同步更新表项。
	// GeneralFailPos 不参与序列化：corpus 往返后的 stash prog 表丢失，此时
	// 用 net_down 反推块位置（recv, net_down, send, recv, net_up, send）。
	failStart, failEnd := -1, -1
	if len(p.GeneralFailPos) > 0 && len(p.GeneralFailPos[0]) >= 4 {
		failStart = p.GeneralFailPos[0][1]
		failEnd = p.GeneralFailPos[0][3]
	} else if downs := findCallsByName(p, "syz_failure_net_down"); len(downs) > 0 {
		if nd := downs[0]; nd-1 >= 0 && nd+4 < len(p.Calls) &&
			strings.Contains(p.Calls[nd+3].Meta.Name, "net_up") {
			failStart, failEnd = nd-1, nd+4
		}
	}

	type interval struct{ start, end int }
	var slots []interval
	if failStart >= 0 {
		// The window interior (failStart, failEnd] is forbidden; positions
		// <= failStart stay insertable (the window shifts right afterwards).
		preEnd := failStart + 1
		if preEnd > validEnd {
			preEnd = validEnd
		}
		if validStart < preEnd {
			slots = append(slots, interval{validStart, preEnd})
		}
		postStart := failEnd + 1
		if postStart < validStart {
			postStart = validStart
		}
		if postStart < validEnd {
			slots = append(slots, interval{postStart, validEnd})
		}
		if len(slots) == 0 {
			return false
		}
	} else {
		slots = append(slots, interval{validStart, validEnd})
	}

	slot := slots[r.Intn(len(slots))]
	insertPos := slot.start + r.Intn(slot.end-slot.start)

	p.Calls = append(p.Calls[:insertPos], append([]*Call{newCall}, p.Calls[insertPos:]...)...)

	pidx := -1
	for i := range ps {
		if ps[i] == p {
			pidx = i
			break
		}
	}
	if pidx >= 0 {
		shiftGeneralFailPos(ps[0], pidx, insertPos, 1)
	}

	// A freshly inserted write has no read verification: add a matching
	// pread64 to another stash read prog to keep the write-fault-read loop
	// closed (see DAG_KNOWN_ISSUES.md #19).
	if callType == "pwrite64" && len(newCall.Args) >= 4 {
		if offArg, ok := newCall.Args[3].(*ConstArg); ok {
			if lenArg, ok := newCall.Args[2].(*ConstArg); ok {
				writerPath := ""
				if chosenRange.openIdx >= 0 && chosenRange.openIdx < len(p.Calls) {
					writerPath = extractPathFromCall(p.Calls[chosenRange.openIdx])
				}
				addStashReadVerification(ps, p, writerPath, r, sCalls, offArg.Val, lenArg.Val)
			}
		}
	}
	return true
}

// shiftGeneralFailPos 偏移 GeneralFailPos 表中 progIdx 节点的条目：
// 插入/删除位置 <= 记录位置时偏移 delta（与 updateBarrierPosTable 同模式）。
func shiftGeneralFailPos(p *Prog, progIdx, shiftPos, delta int) {
	if p == nil || len(p.GeneralFailPos) == 0 {
		return
	}
	startKey := progIdx*100 + 1
	endKey := progIdx*100 + 2
	for _, failPos := range p.GeneralFailPos {
		for i := 0; i < len(failPos)-1; i++ {
			if (failPos[i] == startKey || failPos[i] == endKey) &&
				failPos[i+1] > 0 && failPos[i+1] >= shiftPos {
				failPos[i+1] += delta
			}
		}
	}
}

// shiftSrvFailPos 偏移 p 自己的 SrvFailPos 条目：位置 >= shiftPos 的条目偏移
// delta（镜像 shiftGeneralFailPos，但表在被突变的 prog 上且位置可为 0，故无
// progIdx 与 >0 守卫）。通用 Mutate 的插入/删除调用处调用。
func shiftSrvFailPos(p *Prog, shiftPos, delta int) {
	if p == nil || len(p.SrvFailPos) == 0 || shiftPos < 0 {
		return
	}
	for _, item := range p.SrvFailPos {
		if len(item) >= 2 && item[1] >= shiftPos {
			item[1] += delta
		}
	}
}

// moveSrvFailPosEntry 记录一次调用搬移对 SrvFailPos 的影响：oldPos 处条目改为
// newPos，区间内其它条目相应平移（供通用 mutateFailPos 使用）。
func moveSrvFailPosEntry(p *Prog, oldPos, newPos int) {
	if p == nil || oldPos == newPos {
		return
	}
	for _, item := range p.SrvFailPos {
		if len(item) < 2 {
			continue
		}
		switch {
		case item[1] == oldPos:
			item[1] = newPos
		case oldPos < newPos && item[1] > oldPos && item[1] <= newPos:
			item[1]--
		case newPos < oldPos && item[1] >= newPos && item[1] < oldPos:
			item[1]++
		}
	}
}

// addStashReadVerification inserts a pread64 reading the (offset, length)
// region of a freshly inserted write into another stash read prog, so the
// write is actually verified. writerPath is the open path behind the write's
// own fd: multi-file stash seeds hold one open per file, so the verification
// must use the reader's open of the same file (the previous first-open pick
// could verify a file the write never touched).
func addStashReadVerification(ps []*Prog, writer *Prog, writerPath string, r *randGen, sCalls *SpecialCalls, offset, length uint64) {
	if writerPath == "" {
		return
	}
	var readers []*Prog
	var readersIdx []int
	for qIdx, q := range ps {
		if q == writer || !q.IsStash {
			continue
		}
		for _, c := range q.Calls {
			if strings.Contains(c.Meta.Name, "pread64") {
				readers = append(readers, q)
				readersIdx = append(readersIdx, qIdx)
				break
			}
		}
	}
	if len(readers) == 0 {
		return
	}
	chosen := r.Intn(len(readers))
	reader := readers[chosen]
	readerIdx := readersIdx[chosen]

	// Match the writer's file exactly: a mismatched fd would silently void
	// the verification, so no fallback to an arbitrary open.
	var fd *ResultArg
	for _, idx := range findCallsByName(reader, "open") {
		if idx >= len(reader.Calls) {
			continue
		}
		if extractPathFromCall(reader.Calls[idx]) != writerPath {
			continue
		}
		if f := extractFdFromCall(reader.Calls[idx]); f != nil {
			fd = f
			break
		}
	}
	if fd == nil {
		return
	}

	readCall := genPreadCallWithFdAt(reader, r, sCalls, fd, offset, length)
	if readCall == nil {
		return
	}
	// Insert right after the reader's last pread64 (its verification section).
	insertIdx := len(reader.Calls)
	for i := len(reader.Calls) - 1; i >= 0; i-- {
		if strings.Contains(reader.Calls[i].Meta.Name, "pread64") {
			insertIdx = i + 1
			break
		}
	}
	reader.Calls = append(reader.Calls[:insertIdx], append([]*Call{readCall}, reader.Calls[insertIdx:]...)...)
	shiftGeneralFailPos(ps[0], readerIdx, insertIdx, 1)
}

// genPreadCallWithFdAt is genPreadCallWithFd with an explicit region: it
// reads length bytes at offset through fd.
func genPreadCallWithFdAt(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg, offset, length uint64) *Call {
	meta := r.target.Syscalls[sCalls.Pread64Id]
	if meta == nil {
		return nil
	}
	s := stateFromProg(p)
	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)
	bufArg := MakeOutDataArg(bufPtrType.Elem, DirOut, length)
	bufPtr := r.allocAddr(s, bufPtrType, DirOut, bufArg.Size(), bufArg)
	args := make([]Arg, len(meta.Args))
	fdArg := MakeResultArg(meta.Args[0].Type.(*ResourceType), DirIn, fd, 0)
	args[0] = fdArg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, length)
	args[3] = MakeConstArg(posType, DirIn, offset)
	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	return c
}

func extractFdFromCall(call *Call) *ResultArg {
	if call.Ret == nil {
		return nil
	}
	return call.Ret
}

func usesFd(call *Call, openRet *ResultArg) bool {
	for _, arg := range call.Args {
		if resArg, ok := arg.(*ResultArg); ok {
			if resArg.Res == openRet {
				return true
			}
		}
	}
	return false
}

func genPwriteCallWithFd(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg) *Call {
	meta := r.target.Syscalls[sCalls.Pwrite64Id]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	bufSize := r.randBufLen()
	bufData := make([]byte, bufSize)
	for i := range bufData {
		bufData[i] = byte(r.Intn(256))
	}

	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)

	bufArg := MakeDataArg(bufPtrType.Elem, DirIn, bufData)
	bufPtr := r.allocAddr(s, bufPtrType, DirIn, bufArg.Size(), bufArg)

	args := make([]Arg, len(meta.Args))
	fdArg := MakeResultArg(meta.Args[0].Type.(*ResourceType), DirIn, fd, 0)
	args[0] = fdArg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, uint64(bufSize))
	args[3] = MakeConstArg(posType, DirIn, uint64(r.randInt(64)))

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

/******************************* Distributed Choice Based Mutation ************************************/

/******************************* Dynamic Group Mutation ************************************/

// Dynamic groups are computed lazily from the execution timeline instead of
// static group IDs assigned at generation time: pick an anchor call in ps[0],
// find the calls in the other progs whose last-execution windows overlap it
// (normalized to the global TSC domain), classify their path relations, and
// mutate the resulting set. Groups therefore always reflect the actual
// concurrency of the most recent execution and never go stale.

// pathRelBetween classifies the geometric path relation of concPath relative
// to anchorPath.
func pathRelBetween(anchorPath, concPath string) PathRelation {
	if anchorPath == "" || concPath == "" {
		return PathNoRel
	}
	if anchorPath == concPath {
		return PathSame
	}
	if strings.HasPrefix(concPath, anchorPath+"/") {
		return PathChild
	}
	if strings.HasPrefix(anchorPath, concPath+"/") {
		return PathParent
	}
	if GetParentDir(anchorPath) != "" && GetParentDir(anchorPath) == GetParentDir(concPath) {
		return PathSibling
	}
	return PathNoRel
}

// pathOfCall resolves the path of a call, backtracking open/creat for fd calls.
func pathOfCall(ps []*Prog, pos GroupPosition) string {
	call := ps[pos.ProgIdx].Calls[pos.CallIdx]
	if call == nil || call.Meta == nil {
		return ""
	}
	switch call.Meta.CallName {
	case "link", "symlink", "readlink":
		// Dual-path calls are planner-owned; their arg0/arg1 roles are not a
		// single FileTree coordinate and must not enter legacy path mutation.
		return ""
	}
	if p := extractPathFromCall(call); p != "" {
		return p
	}
	return getFdFilePathForCall(ps, pos)
}

// pickAnchor selects a random call in ps[0] that has a resolvable path and
// execution timing; wantData restricts the anchor to data-carrying calls
// (write/read/truncate), whose whole point is sharing offsets and lengths.
func pickAnchor(ps []*Prog, r *randGen, wantData bool) (GroupPosition, string, bool) {
	if len(ps) == 0 || len(ps[0].Calls) == 0 {
		return GroupPosition{}, "", false
	}
	for attempt := 0; attempt < 8; attempt++ {
		idx := r.Intn(len(ps[0].Calls))
		call := ps[0].Calls[idx]
		if wantData && !isWriteCall(call) && !isReadCall(call) && !isTruncateCall(call) {
			continue
		}
		pos := GroupPosition{ProgIdx: 0, CallIdx: idx}
		path := pathOfCall(ps, pos)
		if path == "" || call.CheckInfo == nil {
			continue
		}
		return pos, path, true
	}
	return GroupPosition{}, "", false
}

// ConcurrentCall is a call in another prog whose last-execution window
// overlapped the anchor's, with its path relation to the anchor.
type ConcurrentCall struct {
	Pos  GroupPosition
	Path string
	Rel  PathRelation
}

// findConcurrentCalls returns the calls in other progs that overlapped the
// anchor's last-execution window (windows normalized to the global TSC domain
// via tscoffs). Calls of the anchor's own prog are excluded: within one VM
// calls execute serially and never overlap.
func findConcurrentCalls(ps []*Prog, anchor GroupPosition, anchorPath string, tscoffs []int64) []ConcurrentCall {
	if len(ps) == 0 {
		return nil
	}
	anchorCall := ps[anchor.ProgIdx].Calls[anchor.CallIdx]
	if anchorCall.CheckInfo == nil {
		return nil
	}
	anchorS := int64(anchorCall.CheckInfo.Stime) - tscoffFor(tscoffs, anchor.ProgIdx)
	anchorE := int64(anchorCall.CheckInfo.Etime) - tscoffFor(tscoffs, anchor.ProgIdx)

	var res []ConcurrentCall
	for i := 1; i < len(ps); i++ {
		if i == anchor.ProgIdx {
			continue
		}
		p := ps[i]
		off := tscoffFor(tscoffs, i)
		for j, c := range p.Calls {
			if c.CheckInfo == nil {
				continue
			}
			s := int64(c.CheckInfo.Stime) - off
			e := int64(c.CheckInfo.Etime) - off
			if s < anchorE && anchorS < e {
				pos := GroupPosition{ProgIdx: i, CallIdx: j}
				path := pathOfCall(ps, pos)
				res = append(res, ConcurrentCall{Pos: pos, Path: path, Rel: pathRelBetween(anchorPath, path)})
			}
		}
	}
	return res
}

// findHBCalls returns the calls in other progs that started after the anchor
// finished: the immediate causal successors (one per prog, the earliest one),
// the counterpart of findConcurrentCalls on the causal side of the anchor's
// timeline. Like findConcurrentCalls this is a timing approximation of
// causality (DAG causality additionally requires path relevance and
// modifier/observer conditions), see DAG_KNOWN_ISSUES.md #18.
func findHBCalls(ps []*Prog, anchor GroupPosition, anchorPath string, tscoffs []int64) []ConcurrentCall {
	if len(ps) == 0 {
		return nil
	}
	anchorCall := ps[anchor.ProgIdx].Calls[anchor.CallIdx]
	if anchorCall.CheckInfo == nil {
		return nil
	}
	anchorE := int64(anchorCall.CheckInfo.Etime) - tscoffFor(tscoffs, anchor.ProgIdx)

	var res []ConcurrentCall
	for i := 1; i < len(ps); i++ {
		if i == anchor.ProgIdx {
			continue
		}
		p := ps[i]
		off := tscoffFor(tscoffs, i)
		best := -1
		var bestS int64
		for j, c := range p.Calls {
			if c.CheckInfo == nil {
				continue
			}
			s := int64(c.CheckInfo.Stime) - off
			if s >= anchorE && (best == -1 || s < bestS) {
				best, bestS = j, s
			}
		}
		if best >= 0 {
			pos := GroupPosition{ProgIdx: i, CallIdx: best}
			path := pathOfCall(ps, pos)
			res = append(res, ConcurrentCall{Pos: pos, Path: path, Rel: pathRelBetween(anchorPath, path)})
		}
	}
	return res
}

// findGroupCalls returns the unified group of an anchor: the concurrent calls
// (overlapping windows) plus the direct causal successors (starting after the
// anchor finished). All four dynamic group mutations operate on this unified
// set, so causal pairs participate in path migration, data sharing and
// deletion just like concurrent ones.
func findGroupCalls(ps []*Prog, anchor GroupPosition, anchorPath string, tscoffs []int64) []ConcurrentCall {
	seen := make(map[GroupPosition]bool)
	var res []ConcurrentCall
	merge := func(calls []ConcurrentCall) {
		for _, cc := range calls {
			if !seen[cc.Pos] {
				seen[cc.Pos] = true
				res = append(res, cc)
			}
		}
	}
	merge(findConcurrentCalls(ps, anchor, anchorPath, tscoffs))
	merge(findHBCalls(ps, anchor, anchorPath, tscoffs))
	return res
}

// pathOwnerCapClass classifies the path type an open/creat call can serve from
// its flags: O_DIRECTORY only opens directories, a write-capable access mode
// (O_WRONLY/O_RDWR) only opens files (VFS rejects write opens on dirs), and a
// read-only or non-open path owner accepts both.
func pathOwnerCapClass(target *Target, owner *Call) int {
	if owner == nil {
		return relTargetAny
	}
	switch cap := fdCapOf(target, owner); {
	case cap&fdCapDir != 0:
		return relTargetDir
	case cap&fdCapWrite != 0:
		return relTargetFile
	}
	return relTargetAny
}

func intersectPathConstraint(a, b int) (int, bool) {
	switch {
	case a == relTargetAny:
		return b, true
	case b == relTargetAny:
		return a, true
	case a == b:
		return a, true
	}
	return relTargetAny, false
}

// ownerPathConstraint intersects the path class allowed by the owner's own
// flags with every same-prog fd user's call constraint: a moved path must
// keep all of them valid (an O_RDONLY open used by read is file-only, a
// getdents64 user pins it to a directory, close/fsync impose nothing).
func ownerPathConstraint(p *Prog, owner *Call) (int, bool) {
	if p == nil || owner == nil {
		return relTargetAny, false
	}
	kind := pathOwnerCapClass(p.Target, owner)
	if owner.Ret == nil {
		return kind, true
	}
	for _, c := range p.Calls {
		if c == nil || c.Meta == nil {
			continue
		}
		uses := false
		ForeachArg(c, func(arg Arg, _ *ArgCtx) {
			if ra, ok := arg.(*ResultArg); ok && ra.Res == owner.Ret {
				uses = true
			}
		})
		if !uses {
			continue
		}
		k := callNameConstraint(c.Meta.Name)
		if strings.Contains(c.Meta.Name, "fsync") || strings.Contains(c.Meta.Name, "fdatasync") {
			// fsync accepts any regular-file or directory fd (capNeeded == 0).
			k = relTargetAny
		}
		var ok bool
		kind, ok = intersectPathConstraint(kind, k)
		if !ok {
			return relTargetAny, false
		}
	}
	return kind, true
}

// memberPathConstraint returns the path-type constraint for a group member:
// its own call constraint intersected with what the owned fd can serve (all
// of the owner's same-prog users included). ok=false means the classes
// conflict, so the member must not move.
func memberPathConstraint(ps []*Prog, opPos GroupPosition) (int, bool) {
	if opPos.ProgIdx < 0 || opPos.ProgIdx >= len(ps) {
		return relTargetAny, false
	}
	p := ps[opPos.ProgIdx]
	if opPos.CallIdx < 0 || opPos.CallIdx >= len(p.Calls) {
		return relTargetAny, false
	}
	op := p.Calls[opPos.CallIdx]
	opKind := callNameConstraint(op.Meta.Name)
	owner := op
	if rp := resolveFdTarget(ps, opPos); rp != opPos {
		if rp.ProgIdx < 0 || rp.ProgIdx >= len(ps) ||
			rp.CallIdx < 0 || rp.CallIdx >= len(ps[rp.ProgIdx].Calls) {
			return relTargetAny, false
		}
		owner = ps[rp.ProgIdx].Calls[rp.CallIdx]
	}
	ownerKind, ok := ownerPathConstraint(p, owner)
	if !ok {
		return relTargetAny, false
	}
	return intersectPathConstraint(opKind, ownerKind)
}

// pathSatisfiesType reports whether an existing tree path's node matches a
// member constraint (GetPathByRelation's PathSame ignores the constraint, so
// the result must be re-checked).
func pathSatisfiesType(ft *FileTree, path string, constraint int) bool {
	if ft == nil {
		return false
	}
	node := ft.FindNode(path)
	if node == nil {
		return false
	}
	return nodeMatchesConstraint(node, constraint)
}

// MutateGroupPathDynamic first tries the path-relation component around an
// anchor (PathSet), then falls back to the legacy dynamic concurrent set.
// PathSet migration keeps path relations across call order and is the broad
// path mutation; the legacy set is retained while the directed target arm is
// being migrated.
func MutateGroupPathDynamic(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	if lcs == nil || lcs.FileTree == nil {
		return false
	}
	if CoverageBoardReady() && r.Float64() < directedBiasProb {
		if mutatePathSetDirected(ps, lcs, r) {
			return true
		}
		if mutateGroupPathDirected(ps, lcs, r) {
			return true
		}
	}
	recordLastMutation("MutateGroupPathDynamic")
	anchor, anchorPath, ok := pickAnchor(ps, r, false)
	if !ok {
		return false
	}
	anchorPos := resolveFdTarget(ps, anchor)
	if set, ok := buildPathSet(ps, anchorPos, lcs.FileTree); ok {
		notePathSetBuilt(set, pathSetRoots(set), false)
		if relocatePathSet(ps, lcs, set, r) {
			recordLastMutation("MutatePathSet")
			return true
		}
	}
	conc := findGroupCalls(ps, anchor, anchorPath, lcs.tscoffs)
	if len(conc) == 0 {
		return false
	}

	anchorConstraint, ok := memberPathConstraint(ps, anchor)
	if !ok {
		return false
	}
	// Migration means a different path: the pick excludes the anchor's
	// current path so the weighted sampler cannot return a no-op edit.
	newBasePath := pickNewBasePath(lcs.FileTree, anchorConstraint, -1, r.Rand, "", pathOfCall(ps, anchor))
	if newBasePath == "" || !pathSatisfiesType(lcs.FileTree, newBasePath, anchorConstraint) {
		return false
	}

	// Merge the group entries by resolved owner: several ops can share one
	// open, and each owner's path is written once with the intersection of
	// all their constraints (last-write-wins would void a stricter user).
	type ownerGroup struct {
		pos  GroupPosition
		rel  PathRelation
		kind int
		ok   bool
	}
	ownerIdx := map[GroupPosition]int{}
	var owners []ownerGroup
	for _, cc := range conc {
		rp := resolveFdTarget(ps, cc.Pos)
		i, seen := ownerIdx[rp]
		if !seen {
			i = len(owners)
			ownerIdx[rp] = i
			owners = append(owners, ownerGroup{pos: rp, rel: cc.Rel, kind: relTargetAny, ok: true})
		}
		if !owners[i].ok {
			continue
		}
		kind, ok := memberPathConstraint(ps, cc.Pos)
		if !ok {
			owners[i].ok = false
			continue
		}
		owners[i].kind, owners[i].ok = intersectPathConstraint(owners[i].kind, kind)
	}

	type pathUpdate struct {
		pos     GroupPosition
		newPath string
	}
	updates := []pathUpdate{{pos: resolveFdTarget(ps, anchor), newPath: newBasePath}}
	for _, og := range owners {
		if !og.ok {
			continue
		}
		newPath := lcs.FileTree.GetPathByRelation(newBasePath, "", og.rel, r.Rand, false, og.kind)
		if newPath == "" || !pathSatisfiesType(lcs.FileTree, newPath, og.kind) {
			// Keep the member on its old path unless the new base itself is a
			// valid target (causal successors with no derivable relation).
			if !pathSatisfiesType(lcs.FileTree, newBasePath, og.kind) {
				continue
			}
			newPath = newBasePath
		}
		updates = append(updates, pathUpdate{pos: og.pos, newPath: newPath})
	}
	applied := false
	for _, u := range updates {
		if updateCallPathInProg(ps[u.pos.ProgIdx], u.pos.CallIdx, u.newPath, r) {
			applied = true
		}
	}
	if !applied {
		return false
	}
	return true
}

// pathAnchorBucket returns the anchor depth bucket used to realize a member at
// targetDepth for the relation (0 = unrealizable). The hints favor anchors
// that satisfy the member-bucket filter inside the picked bucket:
// Child T=2 uses depth-1 anchors (exact), T=3 uses depth-5+ anchors (every
// child is then in bucket 3); Parent T=1/2 use bucket-2 anchors, T=3 uses
// depth-6+ anchors (bucket 3). The member-bucket retry filter rejects the
// anchors that do not land the member in the target bucket.
func pathAnchorBucket(rel PathRelation, targetDepth int) int {
	switch rel {
	case PathChild:
		switch targetDepth {
		case 1:
			return 0 // no depth-0 parent exists
		case 2:
			return 1 // anchor depth 1 -> member depth 2
		default:
			return 3 // anchor depth >=5 -> member depth >=6 (all bucket 3)
		}
	case PathParent:
		switch targetDepth {
		case 1:
			return 2 // anchor depth 2 -> member depth 1
		case 2:
			return 2 // anchor depth 3/4 -> member depth 2/3
		default:
			return 3 // anchor depth >=6 -> member depth >=5
		}
	default: // Same / Sibling
		return targetDepth
	}
}

// mutateGroupPathDirected steers path mutation toward an uncovered
// (relation, depth-bucket) cell margin. The whole group moves with the anchor
// (cell DepthB belongs to B), but only the selected member is re-derived with
// the targeted relation; the other members keep their previous relation to
// the new anchor. The target is statistical: ExtractPairs orders a CC pair by
// program index, so the ps[0] anchor is normally A and the selected member B
// (where DepthB lives); an HB pair in the reverse direction is a blind spot.
func mutateGroupPathDirected(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	if lcs == nil || lcs.FileTree == nil || !CoverageBoardReady() {
		return false
	}
	anchor, anchorPath, ok := pickAnchor(ps, r, false)
	if !ok {
		return false
	}
	conc := findGroupCalls(ps, anchor, anchorPath, lcs.tscoffs)
	if len(conc) == 0 {
		return false
	}
	anchorCall := ps[anchor.ProgIdx].Calls[anchor.CallIdx]
	anchorName := anchorCall.Meta.CallName
	if anchorName == "" {
		return false
	}
	anchorKind, ok := memberPathConstraint(ps, anchor)
	if !ok {
		return false
	}

	type memberCand struct {
		pos    GroupPosition
		opName string
		kind   int
	}
	anchorPos := resolveFdTarget(ps, anchor)
	ownerIdx := map[GroupPosition]int{}
	ownerHasUser := map[GroupPosition]bool{}
	ownerPrefAny := map[GroupPosition]bool{}
	var owners []memberCand
	var ownerOK []bool
	var ownerRel []PathRelation
	for _, cc := range conc {
		rp := resolveFdTarget(ps, cc.Pos)
		if rp == anchorPos {
			continue
		}
		opName := ps[cc.Pos.ProgIdx].Calls[cc.Pos.CallIdx].Meta.CallName
		if opName == "" {
			continue
		}
		i, seen := ownerIdx[rp]
		if !seen {
			i = len(owners)
			ownerIdx[rp] = i
			owners = append(owners, memberCand{pos: rp, opName: opName, kind: relTargetAny})
			ownerOK = append(ownerOK, true)
			ownerRel = append(ownerRel, cc.Rel)
			ownerHasUser[rp] = cc.Pos != rp
			ownerPrefAny[rp] = callNameConstraint(opName) == relTargetAny
		} else if cc.Pos != rp && callNameConstraint(opName) != relTargetAny &&
			(!ownerHasUser[rp] || ownerPrefAny[rp]) {
			// Prefer a typed consumer op over the owner open and over an
			// untyped close for the combo key.
			owners[i].opName = opName
			ownerHasUser[rp] = true
			ownerPrefAny[rp] = false
		}
		if !ownerOK[i] {
			continue
		}
		kind, ok := memberPathConstraint(ps, cc.Pos)
		if !ok {
			ownerOK[i] = false
			continue
		}
		owners[i].kind, ownerOK[i] = intersectPathConstraint(owners[i].kind, kind)
	}
	var members []memberCand
	for i := range owners {
		if ownerOK[i] {
			members = append(members, owners[i])
		}
	}
	if len(members) == 0 {
		return false
	}

	// Candidate (member, relation) pairs, geometry-filtered: Child needs a
	// directory anchor, Parent needs a directory member (GetPathByRelation
	// returns "" otherwise, so such picks would only waste the round).
	rels := []PathRelation{PathSame, PathChild, PathParent, PathSibling}
	var pots []int
	var pickMember []memberCand
	var pickRel []PathRelation
	for _, m := range members {
		mKind := m.kind
		for _, rel := range rels {
			if rel == PathChild && anchorKind == relTargetFile {
				continue
			}
			if rel == PathParent && mKind == relTargetFile {
				continue
			}
			if rel == PathSame {
				// Same shares one path, so the member's constraint must accept
				// the anchor's path type (Sibling/Child/Parent generate the
				// member path under its own constraint instead).
				if anchorKind == relTargetFile && mKind == relTargetDir {
					continue
				}
				if anchorKind == relTargetDir && mKind == relTargetFile {
					continue
				}
			}
			pot := CoveragePotential(anchorName, m.opName, rel)
			if pot <= 0 {
				continue
			}
			pots = append(pots, pot)
			pickMember = append(pickMember, m)
			pickRel = append(pickRel, rel)
		}
	}
	idx := pickWeightedIndex(pots, r.Rand)
	if idx < 0 {
		return false
	}
	member, rel := pickMember[idx], pickRel[idx]

	// Depth target from the same (primary) ordering; bucket 0 is outside the
	// merge-view domain.
	depths := covBoard.uncoveredDepthPots(anchorName, member.opName, rel)
	depths[0] = 0
	targetDepth := pickWeightedIndex(depths[:], r.Rand)
	if targetDepth <= 0 {
		return false
	}
	anchorBucket := pathAnchorBucket(rel, targetDepth)
	if anchorBucket == 0 {
		return false
	}
	baseKind := anchorKind
	mKind := member.kind
	switch {
	case rel == PathChild && baseKind == relTargetAny:
		baseKind = relTargetDir
	case rel == PathSame && baseKind == relTargetAny && mKind != relTargetAny:
		// Same shares the path: an Any anchor is pinned to the member's type.
		baseKind = mKind
	}
	for attempt := 0; attempt < 8; attempt++ {
		newBase := pickNewBasePath(lcs.FileTree, baseKind, anchorBucket, r.Rand, "", pathOfCall(ps, anchorPos))
		if newBase == "" {
			return false
		}
		memberPath := lcs.FileTree.GetPathByRelation(newBase, "", rel, r.Rand, false, mKind)
		if memberPath == "" || DepthBucketOf(memberPath) != uint64(targetDepth) ||
			!pathSatisfiesType(lcs.FileTree, memberPath, mKind) {
			continue
		}
		if !updateCallPathInProg(ps[anchorPos.ProgIdx], anchorPos.CallIdx, newBase, r) {
			return false
		}
		// The anchor rewrite above is already a real edit; the member and the
		// owners stay best-effort (their derivation gates above skip
		// unrealizable ones).
		updateCallPathInProg(ps[member.pos.ProgIdx], member.pos.CallIdx, memberPath, r)
		for i := range owners {
			rp := owners[i].pos
			if rp == anchorPos || rp == member.pos || !ownerOK[i] {
				continue
			}
			constraint := owners[i].kind
			p := lcs.FileTree.GetPathByRelation(newBase, "", ownerRel[i], r.Rand, false, constraint)
			if p == "" || !pathSatisfiesType(lcs.FileTree, p, constraint) {
				if !pathSatisfiesType(lcs.FileTree, newBase, constraint) {
					continue
				}
				p = newBase
			}
			updateCallPathInProg(ps[rp.ProgIdx], rp.CallIdx, p, r)
		}
		recordLastMutation("MutateGroupPathDirected")
		return true
	}
	return false
}

// RemoveGroupDynamic deletes the fd-safe subset of the anchor's group
// (concurrent set + causal successors): open/creat calls whose fd users all
// fall inside the same removal set go too, while an anchor/provider with a
// surviving external user is kept. Returns false when nothing is removable.
func RemoveGroupDynamic(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	recordLastMutation("RemoveGroupDynamic")
	if lcs == nil {
		return false
	}
	anchor, anchorPath, ok := pickAnchor(ps, r, false)
	if !ok {
		return false
	}
	conc := findGroupCalls(ps, anchor, anchorPath, lcs.tscoffs)
	if len(conc) == 0 {
		return false
	}

	// Decide the removal set on call pointers before deleting anything: an
	// open/creat is removable only when every fd user of it is also in the
	// set, otherwise RemoveCall would rewrite the surviving users to a
	// constant fd 0 (a silently voided call).
	removeSet := make(map[*Call]bool, len(conc)+1)
	removeSet[ps[anchor.ProgIdx].Calls[anchor.CallIdx]] = true
	for _, cc := range conc {
		removeSet[ps[cc.Pos.ProgIdx].Calls[cc.Pos.CallIdx]] = true
	}
	owner := make(map[Arg]*Call)
	for _, p := range ps {
		for _, c := range p.Calls {
			if removeSet[c] {
				ForeachArg(c, func(a Arg, _ *ArgCtx) { owner[a] = c })
			}
		}
	}

	// Fixed point: a candidate may be dropped by the filter while one of its
	// fd users is also a candidate that gets dropped (the user is itself
	// non-removable), so re-check until the removal set is closed under the
	// "all users removed too" rule.
	removal := make(map[*Call]bool, len(removeSet))
	for c := range removeSet {
		removal[c] = true
	}
	for changed := true; changed; {
		changed = false
		for c := range removal {
			if !callRemovableInSet(c, removal, owner) {
				delete(removal, c)
				changed = true
			}
		}
	}

	byProg := make(map[int]map[int]bool)
	addPos := func(pos GroupPosition) {
		if byProg[pos.ProgIdx] == nil {
			byProg[pos.ProgIdx] = make(map[int]bool)
		}
		byProg[pos.ProgIdx][pos.CallIdx] = true
	}
	for _, cc := range conc {
		if removal[ps[cc.Pos.ProgIdx].Calls[cc.Pos.CallIdx]] {
			addPos(cc.Pos)
		}
	}
	if removal[ps[anchor.ProgIdx].Calls[anchor.CallIdx]] {
		addPos(anchor)
	}

	removed := 0
	for progIdx, callIdxs := range byProg {
		idxs := make([]int, 0, len(callIdxs))
		for idx := range callIdxs {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		// Delete from the end of each prog so earlier indices stay valid.
		for i := len(idxs) - 1; i >= 0; i-- {
			ps[progIdx].RemoveCall(idxs[i])
			removed++
		}
	}
	return removed > 0
}

// callRemovable reports whether the call can be removed without breaking an
// fd dependency: open/creat calls whose fd is still used are kept, as are the
// failure-injection pseudo calls.
func callRemovable(ps []*Prog, pos GroupPosition) bool {
	call := ps[pos.ProgIdx].Calls[pos.CallIdx]
	if strings.Contains(call.Meta.Name, "syz_failure") {
		return false
	}
	if strings.Contains(call.Meta.Name, "open") || strings.Contains(call.Meta.Name, "creat") {
		for _, fi := range AnalyzeProgFds(ps[pos.ProgIdx]) {
			if fi.Fd == call.Ret && fi.TotalUses > 0 {
				return false
			}
		}
	}
	return true
}

// callRemovableInSet is the group-removal counterpart of callRemovable: an
// open/creat may be removed together with the given removal set only when
// every fd user of its Ret is itself in the set. Removing it with surviving
// users would make RemoveCall rewrite them to a constant fd 0 (a silently
// voided call), not a dangling reference.
func callRemovableInSet(c *Call, removal map[*Call]bool, owner map[Arg]*Call) bool {
	if strings.Contains(c.Meta.Name, "syz_failure") {
		return false
	}
	if !strings.Contains(c.Meta.Name, "open") && !strings.Contains(c.Meta.Name, "creat") {
		return true
	}
	if c.Ret == nil {
		return true
	}
	for u := range c.Ret.uses {
		if o := owner[u]; o == nil || !removal[o] {
			return false
		}
	}
	return true
}

// RemoveReservoirDynamic removes calls across a random subset of the program
// group with a reservoir-style dynamic probability: the target number of
// removals adapts to the number of programs that have a removable call, and
// after each removal the probability for the remaining programs decreases
// (remain/(C-k)). Each program loses at most one fd-safe call. The traversal
// starts at a random program, so no position is systematically favored.
// Candidates include every non-empty program (including ps[0]) that has at
// least one fd-safe call; with pre-filtered candidates every probability hit
// removes exactly one call, so the reservoir draw yields exactly the target
// number of removals (>= 1 whenever a removable call exists).
func RemoveReservoirDynamic(ps []*Prog, r *randGen) bool {
	recordLastMutation("RemoveReservoirDynamic")

	// Pre-filter to programs that actually have a removable (fd-safe) call,
	// keeping their call indices. Without this, programs with no removable
	// call would consume probability draws without advancing the reservoir
	// (skewing the draw and occasionally yielding zero removals).
	type candProg struct {
		pi   int
		idxs []int
	}
	var cand []candProg
	for i := 0; i < len(ps); i++ {
		var idxs []int
		for ci := range ps[i].Calls {
			if callRemovable(ps, GroupPosition{ProgIdx: i, CallIdx: ci}) {
				idxs = append(idxs, ci)
			}
		}
		if len(idxs) > 0 {
			cand = append(cand, candProg{pi: i, idxs: idxs})
		}
	}
	C := len(cand)
	if C == 0 {
		return false
	}

	// Target removals: uniform in [ceil(C/3), C] (>= 1). C=3 -> {1,2,3}
	// (mean 2); the per-program cap of one call bounds remain naturally.
	lo := (C + 2) / 3
	if lo < 1 {
		lo = 1
	}
	remain := int(r.randRange(uint64(lo), uint64(C)))

	start := r.Intn(C)
	removed := 0
	for k := 0; k < C && remain > 0; k++ {
		cp := cand[(start+k)%C]
		// Reservoir probability remain/(C-k), drawn as an integer.
		if r.Intn(C-k) >= remain {
			continue
		}
		ps[cp.pi].RemoveCall(cp.idxs[r.Intn(len(cp.idxs))])
		remain--
		removed++
	}
	return removed > 0
}

// MutateGroupDataDynamic shares a random offset (and length for writes)
// across all read/write calls of the anchor's dynamic concurrent set, the
// deterministic counterpart of the probabilistic OffsetSame insertions. The
// anchor itself must be a read/write call, so the set always contains
// something to mutate.
func MutateGroupDataDynamic(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	if lcs == nil {
		return false
	}
	if CoverageBoardReady() && r.Float64() < directedBiasProb {
		if mutateGroupDataDirected(ps, lcs, r) {
			return true
		}
	}
	recordLastMutation("MutateGroupDataDynamic")
	anchor, anchorPath, ok := pickAnchor(ps, r, true)
	if !ok {
		atomic.AddUint64(&anchorMissCount, 1)
		return false
	}

	var positions []GroupPosition
	positions = append(positions, anchor)
	for _, cc := range findGroupCalls(ps, anchor, anchorPath, lcs.tscoffs) {
		positions = append(positions, cc.Pos)
	}

	sharedOffset := uint64(0)
	hasOffset := false
	sharedLength := uint64(0)
	hasLength := false
	fileSize := uint64(0)
	sizeKnown := false
	for _, pos := range positions {
		call := ps[pos.ProgIdx].Calls[pos.CallIdx]
		if !isWriteCall(call) && !isReadCall(call) && !isTruncateCall(call) {
			continue
		}
		if !sizeKnown && lcs.FileTree != nil {
			// Resolve the anchor file's size once: a real size 0 is a valid
			// outcome and must not be re-queried on later positions.
			fileSize = getFileSizeByPath(ps, pos, lcs.FileTree)
			sizeKnown = true
		}
		if !hasOffset && len(call.Args) >= 4 {
			sharedOffset = r.randHmdfsOffset(fileSize)
			hasOffset = true
		}
		if !hasLength && ((isWriteCall(call) && len(call.Args) >= 3) ||
			(isReadCall(call) && len(call.Args) >= 3) ||
			(isTruncateCall(call) && len(call.Args) >= 2)) {
			sharedLength = pickMutationLength(r, fileSize)
			hasLength = true
		}
	}
	if hasOffset && hasLength {
		// Keep the shared offset/length pair consistent with the offset
		// bucket semantics (tail writes cross EOF, mid stays inside).
		sharedLength = clampLenToBucket(fileSize, sharedOffset, sharedLength,
			bucketOfOffset(fileSize, sharedOffset))
	}
	applyGroupDataValues(ps, positions, sharedOffset, hasOffset, sharedLength, hasLength, r)
	return true
}

// applyGroupDataValues writes the shared offset/length pair into every data
// call of the group (offset only where the call carries one explicitly,
// length into the count/length argument) and rebuilds the derived sizes.
func applyGroupDataValues(ps []*Prog, positions []GroupPosition, sharedOffset uint64, hasOffset bool, sharedLength uint64, hasLength bool, r *randGen) {
	for _, pos := range positions {
		call := ps[pos.ProgIdx].Calls[pos.CallIdx]
		if !isWriteCall(call) && !isReadCall(call) && !isTruncateCall(call) {
			continue
		}
		if hasOffset && (isWriteCall(call) || isReadCall(call)) && len(call.Args) >= 4 {
			if posArg, ok := call.Args[3].(*ConstArg); ok {
				posArg.Val = sharedOffset
			}
		}
		if hasLength {
			switch {
			case isWriteCall(call) && len(call.Args) >= 3:
				if updateCallDataBuf(ps[pos.ProgIdx], call, sharedLength, r) {
					if countArg, ok := call.Args[2].(*ConstArg); ok {
						countArg.Val = sharedLength
					}
				}
			case isReadCall(call) && len(call.Args) >= 3:
				// read has no input payload: only the receive buffer is resized;
				// count (len[buf]) is recomputed by assignSizesCall below. A
				// failed resize is intentionally ignored: the old buffer is
				// kept and the count follows it.
				updateCallDataBuf(ps[pos.ProgIdx], call, sharedLength, r)
			case isTruncateCall(call) && len(call.Args) >= 2:
				if lenArg, ok := call.Args[1].(*ConstArg); ok {
					lenArg.Val = sharedLength
				}
			}
		}
		r.target.assignSizesCall(call)
	}
}

// mutateGroupDataDirected targets an uncovered OffB bucket: it collects the
// group's offset-capable call names (pread64/pwrite64 carry an explicit
// offset, truncate realizes the bucket through its length; read/write are
// sequential and cannot steer it), picks a bucket by uncovered potential and
// realizes the shared offset/length pair accordingly. The bucket choice is
// statistical: the group's roles (which call ends up as pair A/B) follow the
// execution order, so the mutation only biases the distribution.
func mutateGroupDataDirected(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	if lcs == nil || lcs.FileTree == nil || !CoverageBoardReady() {
		return false
	}
	anchor, anchorPath, ok := pickAnchor(ps, r, true)
	if !ok {
		return false
	}
	var positions []GroupPosition
	positions = append(positions, anchor)
	for _, cc := range findGroupCalls(ps, anchor, anchorPath, lcs.tscoffs) {
		positions = append(positions, cc.Pos)
	}

	var variants []string
	seenVar := make(map[string]bool)
	fileSize := uint64(0)
	sizeKnown := false
	hasExplicitOffset := false
	hasTruncate := false
	for _, pos := range positions {
		call := ps[pos.ProgIdx].Calls[pos.CallIdx]
		if !isWriteCall(call) && !isReadCall(call) && !isTruncateCall(call) {
			continue
		}
		if !sizeKnown {
			fileSize = getFileSizeByPath(ps, pos, lcs.FileTree)
			sizeKnown = true
		}
		name := call.Meta.CallName
		switch {
		case (isWriteCall(call) || isReadCall(call)) && len(call.Args) >= 4:
			hasExplicitOffset = true
			if name != "" && !seenVar[name] {
				seenVar[name] = true
				variants = append(variants, name)
			}
		case isTruncateCall(call) && len(call.Args) >= 2:
			hasTruncate = true
			if name != "" && !seenVar[name] {
				seenVar[name] = true
				variants = append(variants, name)
			}
		}
	}
	if len(variants) == 0 {
		return false
	}
	pots := covBoard.uncoveredOffPots(variants)
	bucket := pickWeightedIndex(pots[:], r.Rand)
	if bucket < offsetBucketZero || bucket > offsetBucketBeyond {
		return false
	}

	sharedOffset := uint64(0)
	hasOffset := false
	sharedLength := uint64(0)
	hasLength := false
	switch {
	case hasExplicitOffset:
		off, ok := realizeDirectedOffset(r, fileSize, uint64(bucket))
		if !ok {
			return false
		}
		sharedOffset, hasOffset = off, true
		sharedLength = clampLenToBucket(fileSize, off, pickMutationLength(r, fileSize), uint64(bucket))
		hasLength = true
	case hasTruncate:
		l, ok := realizeDirectedTruncateLen(r, fileSize, uint64(bucket))
		if !ok {
			return false
		}
		sharedLength, hasLength = l, true
	default:
		return false
	}
	applyGroupDataValues(ps, positions, sharedOffset, hasOffset, sharedLength, hasLength, r)
	recordLastMutation("MutateGroupDataDirected")
	return true
}

// realizeDirectedOffset samples an offset that lands in bucket on a file of
// the given size, verifying via bucketOfOffset. Single source for the 2b
// parameter realizer and the data-mutation arm. At size 0 the Beyond sampler
// can draw EOF itself (0, which is the Zero bucket), so that degenerate case
// is realized deterministically instead of relying on retries (degenerate
// sizes otherwise make a bucket unrealizable; the caller falls back).
func realizeDirectedOffset(r *randGen, size, bucket uint64) (uint64, bool) {
	if bucket == offsetBucketBeyond && size == 0 {
		return 4096, true
	}
	off := r.randOffsetInBucket(size, bucket)
	if bucketOfOffset(size, off) != bucket {
		return 0, false
	}
	return off, true
}

// realizeDirectedTruncateLen samples a truncate length that lands in bucket
// on a file of the given size (truncate carries no offset argument: its
// offset bucket is computed from the new length).
func realizeDirectedTruncateLen(r *randGen, size, bucket uint64) (uint64, bool) {
	for attempt := 0; attempt < 8; attempt++ {
		var l uint64
		switch bucket {
		case offsetBucketZero:
			l = 0
		case offsetBucketBeyond:
			l = size + 1 + uint64(r.Intn(4096))
		case offsetBucketMid:
			// No mid region when the file has no tail page (size 0 or below
			// one page). The tb==0 check also protects the size-1 subtraction
			// from unsigned underflow (which would wrap to MaxUint64 and let
			// the hi<1 guard below pass, ending in r.Intn(-1) panic).
			tb := tailBeginOf(size)
			if tb == 0 {
				return 0, false
			}
			hi := size - 1
			if tb < size {
				hi = tb - 1
			}
			if hi < 1 {
				return 0, false
			}
			l = 1 + uint64(r.Intn(int(hi)))
		case offsetBucketTail:
			// Sub-page files have no separate tail page: tailBeginOf is 0 and
			// every length in [1, size-1] classifies as Tail (bucketOfOffset),
			// so the region starts at 1 there. The lo >= size guard also
			// protects the size-lo subtraction from underflow.
			lo := tailBeginOf(size)
			if lo == 0 {
				lo = 1
			}
			if lo >= size {
				return 0, false
			}
			l = lo + uint64(r.Intn(int(size-lo)))
		default:
			return 0, false
		}
		if bucketOfOffset(size, l) == bucket {
			return l, true
		}
	}
	return 0, false
}

// mutateMixForSize returns the mutation mix weights for the fsops DCT
// mutator, banded by len(ps[0].Calls). Weights out of 100 are (insertion,
// remove-one, remove-group, path/data group mutation). The reservoir
// insertion was removed (2026-09-28): its budget folded back into the single
// insertion share, restoring the pre-merge distribution. The insertion share
// now dispatches entirely to the Phase 2b directed batch with the weighted
// 2a DCT insertion as its fallback (patterns are no longer used by the
// mutation path). remove-one is RemoveReservoirDynamic (cross-program
// subset deletion) and remove-group is RemoveGroupDynamic. The original
// syzkaller Mutate is NOT part of this mix: it remains the fallback used by
// mutateHmdfs when the typed mutator declines. The path/data weight
// (mutPathData) is split internally 70/30 in favor of the path mutator
// (dataMutateProb=30); a data miss degrades to the path mutator in the same
// round.
//
//	1-3   (tiny):   85/ 5/ 0/10
//	4-11  (grow):   60/10/ 5/25
//	12-16 (peak):   35/25/10/30
//	17-20 (shrink): 10/40/20/30
func mutateMixForSize(size int) (ins, removeOne, removeGroup, mutPathData int) {
	switch {
	case size < 4:
		return 85, 5, 0, 10
	case size < 12:
		return 60, 10, 5, 25
	case size < 17:
		return 35, 25, 10, 30
	default:
		return 10, 40, 20, 30
	}
}

// failShapeProb is the probability that a DCT insertion's root target is
// rewritten into a failure shape from the reachability whitelist (see
// applyFailureShape).
const failShapeProb = 0.25

var (
	failShapeHitCount  uint64
	failShapeSkipCount uint64
)

// FailShapeStats returns (hit, skip): root targets actually rewritten into a
// reachable failure shape, and attempts skipped because no constructible
// target existed (atomic; shared by all Procs).
func FailShapeStats() (uint64, uint64) {
	return atomic.LoadUint64(&failShapeHitCount), atomic.LoadUint64(&failShapeSkipCount)
}

// dataMutateProb is the probability (out of 100) that the group-mutation
// budget of MutateFsopsWithDCT goes to the data mutator instead of the path
// mutator: data only rewrites offset/length values while path changes file
// identity, so the split is biased 70/30 towards path. A data miss (no data
// anchor or no offset/length candidate) degrades to the path mutator in the
// same round; the miss count is exported for offline tuning.
const dataMutateProb = 30

var dataMutateMissCount uint64

// DataMutateMissCount returns the cumulative number of data-branch misses
// that were redirected to the path mutator (atomic; shared by all Procs).
func DataMutateMissCount() uint64 {
	return atomic.LoadUint64(&dataMutateMissCount)
}

var anchorMissCount uint64

// AnchorMissCount returns the cumulative number of data-branch misses caused
// by pickAnchor (no data-carrying call with timing info in ps[0]); it is a
// subset of DataMutateMissCount.
func AnchorMissCount() uint64 {
	return atomic.LoadUint64(&anchorMissCount)
}

var timingAlignMissCount uint64

// TimingAlignMissCount returns the cumulative number of temporal alignments
// that could not use timing (refTime < 0 or no CheckInfo anchor in the target
// prog) and fell back to index alignment. Freshly generated/grown progs and
// seeds whose corpus timeline was not restored both count here, so use it as
// a trend metric together with the fuzzer's timings=applied/total line.
// Atomic; shared by all Procs.
func TimingAlignMissCount() uint64 {
	return atomic.LoadUint64(&timingAlignMissCount)
}

// applyFailureShape rewrites the root target paths of an insertion into a
// shape whose error can reach the traced hmdfs function (source-verified
// reachability table):
//   - rmdir on a non-empty directory -> hmdfs_rmdir_merge negative ret;
//   - rename of a directory onto a non-empty directory -> hmdfs_rename_merge
//     negative ret (EPERM when the parents differ, lower ENOTEMPTY otherwise).
//
// VFS-level short-circuits (bad open flags, missing paths, fd errors, ...)
// are deliberately not attempted: they never fire the kretprobes.
func applyFailureShape(lcs *LayeredChoiceStrategy, rootCallName, basePath, basePath2 string, r *randGen) (string, string) {
	if lcs == nil || lcs.FileTree == nil {
		return basePath, basePath2
	}
	switch rootCallName {
	case "rmdir":
		if dir := pickNonEmptyDir(lcs.FileTree, r, basePath); dir != "" {
			atomic.AddUint64(&failShapeHitCount, 1)
			return dir, basePath2
		}
		atomic.AddUint64(&failShapeSkipCount, 1)
	case "rename":
		if dst := pickNonEmptyDir(lcs.FileTree, r, ""); dst != "" {
			if src := pickRenameSrcDir(lcs.FileTree, r, dst); src != "" {
				atomic.AddUint64(&failShapeHitCount, 1)
				return src, dst
			}
		}
		atomic.AddUint64(&failShapeSkipCount, 1)
	}
	return basePath, basePath2
}

// pickNonEmptyDir returns a plain directory with at least one child (the
// non-empty rmdir/rename target of the failure shapes), "" if none exists.
func pickNonEmptyDir(ft *FileTree, r *randGen, exclude string) string {
	ft.mu.RLock()
	var cands []*FileNode
	for _, dirs := range ft.DirsByCid {
		for _, n := range dirs {
			if n.Type != NodeTypeDir || len(n.Children) == 0 {
				continue
			}
			if n.FullPath == exclude {
				continue
			}
			cands = append(cands, n)
		}
	}
	ft.mu.RUnlock()
	if len(cands) == 0 {
		return ""
	}
	return cands[r.Intn(len(cands))].FullPath
}

// pickRenameSrcDir returns a directory sibling of dstPath (same parent — the
// hard hmdfs requirement: cross-parent renames return EPERM before the trace
// point, so they would never produce an event), "" if none exists. The source
// is a variant-side path, so a TmpDir sibling qualifies (nodeIsDir); the
// non-TmpDir rule applies to the destination only (see pickNonEmptyDir).
func pickRenameSrcDir(ft *FileTree, r *randGen, dstPath string) string {
	parent := GetParentDir(dstPath)
	if parent == "" {
		return ""
	}
	ft.mu.RLock()
	var cands []*FileNode
	for _, dirs := range ft.DirsByCid {
		for _, n := range dirs {
			if !nodeIsDir(n) {
				continue
			}
			if n.Parent == nil || n.Parent.FullPath != parent || n.FullPath == dstPath {
				continue
			}
			cands = append(cands, n)
		}
	}
	ft.mu.RUnlock()
	if len(cands) == 0 {
		return ""
	}
	return cands[r.Intn(len(cands))].FullPath
}

func MutateFsopsWithDCT(ps []*Prog, rs rand.Source, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy) bool {
	if len(ps) == 0 || lcs == nil {
		return false
	}

	r := newRand(ps[0].Target, rs)
	r.hmcfg = hmcfg

	ins, removeOne, removeGroup, _ := mutateMixForSize(len(ps[0].Calls))
	roll := r.Intn(100)
	switch {
	case roll < ins:
		if len(ps[0].Calls) >= RecommendedCalls {
			// At the size cap: fall back to removing calls instead of
			// wasting the round, so programs oscillate around the cap.
			return RemoveReservoirDynamic(ps, r)
		}
		// Pattern-based insertion was removed from the mutation path
		// (2026-09-28; the dormant insertCallFromPattern and its broken-ref
		// test were deleted later, P2-#12), and the reservoir insertion was
		// removed later the same day (its budget folded back into this
		// insertion share): the budget always dispatches as follows.
		//
		// Phase 2b dispatch: the outer roll splits the insertion budget
		// between an exact directed batch and the 2a-based regular insertion
		// (whose inner Direction 3 is disabled to avoid a double bias).
		// Anything that never executed is accounted by cause (noPlan /
		// insertFail / cfgErr), never as a cell miss, and falls back within
		// this same round.
		if CoverageBoardReady() && r.Float64() < directedBiasProb {
			batch, status := PickDirectedBatch(ps, lcs, hmcfg, r)
			switch {
			case batch != nil:
				if insertDirectedBatch(ps, r, sCalls, hmcfg, lcs, batch) {
					recordLastMutation("insertDirectedBatch")
					return true
				}
				NoteDirectedInsertFail()
			case status == directedPlanCfgErr:
				NoteDirectedCfgErr()
			default:
				NoteDirectedNoPlan()
			}
		}
		return insertDerivedCallFromDCT(ps, r, sCalls, hmcfg, lcs, false, false)
	case roll < ins+removeOne:
		return RemoveReservoirDynamic(ps, r)
	case roll < ins+removeOne+removeGroup:
		if RemoveGroupDynamic(ps, lcs, r) {
			return true
		}
		// The fd-safe filter can decline (every provider in the group has an
		// fd user outside it); keep the round productive by degrading to the
		// one-call reservoir removal.
		return RemoveReservoirDynamic(ps, r)
	default:
		// Path/data split of the group-mutation budget: path changes file
		// identity, data only offset/length values, so bias to path (70/30).
		// A data miss degrades to the path mutator in the same round instead
		// of dropping to the generic fallback.
		if r.Intn(100) >= dataMutateProb {
			return MutateGroupPathDynamic(ps, lcs, r)
		}
		if MutateGroupDataDynamic(ps, lcs, r) {
			return true
		}
		atomic.AddUint64(&dataMutateMissCount, 1)
		return MutateGroupPathDynamic(ps, lcs, r)
	}
}

type GroupPosition struct {
	ProgIdx int
	CallIdx int
}

// rootBaseConstraint returns the tree-type constraint of a root call that
// operates directly on its base path. ok=false for roots that rewrite the
// base path themselves (mkdir picks a container, creat appends a fresh name)
// or accept either type, so no check is needed.
func rootBaseConstraint(call string) (int, bool) {
	return rootBaseConstraintFor(hmdfsProfile, call)
}

func rootBaseConstraintFor(profile *DFSProfile, call string) (int, bool) {
	switch call {
	case "mkdir", "creat":
		return relTargetAny, false
	}
	switch kind := profile.TargetConstraint(call); kind {
	case relTargetFile, relTargetDir:
		return kind, true
	}
	return relTargetAny, false
}

// nodeKindMatches reports whether a tree node satisfies a root constraint.
func nodeKindMatches(n *FileNode, kind int) bool {
	switch kind {
	case relTargetFile:
		return n.Type == NodeTypeFile
	case relTargetDir:
		return n.Type == NodeTypeDir || n.Type == NodeTypeEmptyDir
	}
	return true
}

func resolveFdTarget(ps []*Prog, pos GroupPosition) GroupPosition {
	call := ps[pos.ProgIdx].Calls[pos.CallIdx]
	callName := call.Meta.CallName

	// Path-taking calls operate on their own path argument. getdents64 is
	// deliberately absent: its arg0 is an fd, so it resolves to the embedded
	// open (same as read/write) instead of being a silent no-op.
	if callName == "open" || callName == "creat" ||
		callName == "truncate" || callName == "chmod" ||
		callName == "mkdir" || callName == "rmdir" ||
		callName == "rename" || callName == "stat" ||
		callName == "unlink" || callName == "link" ||
		callName == "symlink" || callName == "readlink" ||
		callName == "getxattr" || callName == "setxattr" {
		return pos
	}

	if len(call.Args) < 1 {
		return pos
	}
	fdArg, ok := call.Args[0].(*ResultArg)
	if !ok || fdArg.Res == nil {
		return pos
	}

	p := ps[pos.ProgIdx]
	for i := pos.CallIdx - 1; i >= 0; i-- {
		prevCall := p.Calls[i]
		if (strings.Contains(prevCall.Meta.Name, "open") ||
			strings.Contains(prevCall.Meta.Name, "creat")) &&
			prevCall.Ret != nil && prevCall.Ret == fdArg.Res {
			return GroupPosition{ProgIdx: pos.ProgIdx, CallIdx: i}
		}
	}
	return pos
}

// pickNewBasePath picks a new anchor path for the path-mutation group. A
// bucket in [1,3] forces that depth bucket (one-hot, used by the directed
// arm); -1 keeps the DepthBucketWeights sampling.
func pickNewBasePath(ft *FileTree, kind int, bucket int, r *rand.Rand, excludeCid, excludePath string) string {
	var candidates []*FileNode
	switch kind {
	case relTargetFile:
		candidates = ft.GetAllFileNodesExcluding(excludeCid)
		if len(candidates) == 0 {
			node := ft.GetRandomFile(r, "")
			if node != nil {
				candidates = append(candidates, node)
			}
		}
	case relTargetDir:
		candidates = ft.GetAllNonTmpDirNodesExcluding(excludeCid)
		if len(candidates) == 0 {
			node := ft.GetRandomDir(r, "", true)
			if node != nil {
				candidates = append(candidates, node)
			}
		}
	default:
		candidates = append(candidates, ft.GetAllFileNodesExcluding(excludeCid)...)
		candidates = append(candidates, ft.GetAllNonTmpDirNodesExcluding(excludeCid)...)
		if len(candidates) == 0 {
			if node := ft.GetRandomFile(r, ""); node != nil {
				candidates = append(candidates, node)
			}
			if node := ft.GetRandomDir(r, "", true); node != nil {
				candidates = append(candidates, node)
			}
		}
	}
	if excludePath != "" {
		kept := candidates[:0]
		for _, n := range candidates {
			if n.FullPath != excludePath {
				kept = append(kept, n)
			}
		}
		candidates = kept
	}
	if len(candidates) == 0 {
		return ""
	}

	// Stage 1: pick a depth bucket by DepthBucketWeights (non-empty groups);
	// a forced bucket takes that group directly.
	var groups [4][]*FileNode
	for _, n := range candidates {
		b := int(DepthBucketOf(n.FullPath))
		if b < 1 || b > 3 {
			b = 1
		}
		groups[b] = append(groups[b], n)
	}
	pool := candidates
	if bucket >= 1 && bucket <= 3 {
		if len(groups[bucket]) == 0 {
			return ""
		}
		pool = groups[bucket]
	} else {
		total := 0.0
		var ws [4]float64
		for b := 1; b <= 3; b++ {
			if len(groups[b]) > 0 {
				ws[b] = DepthBucketWeights[b-1]
				total += ws[b]
			}
		}
		if total > 0 {
			x := r.Float64() * total
			cum := 0.0
			for b := 1; b <= 3; b++ {
				cum += ws[b]
				if ws[b] > 0 && x < cum {
					pool = groups[b]
					break
				}
			}
		}
	}

	// Stage 2: node-size/depth weight inside the chosen bucket.
	totalWeight := 0.0
	weights := make([]float64, len(pool))
	for i, node := range pool {
		w := calculateNodeWeight(node)
		weights[i] = w
		totalWeight += w
	}
	if totalWeight <= 0 {
		return pool[r.Intn(len(pool))].FullPath
	}
	x := r.Float64() * totalWeight
	cumSum := 0.0
	for i, w := range weights {
		cumSum += w
		if x < cumSum {
			return pool[i].FullPath
		}
	}
	return pool[len(pool)-1].FullPath
}

func getLengthBucket(l int) int {
	switch {
	case l < 64:
		return 0
	case l < 128:
		return 1
	case l < 256:
		return 2
	case l < 512:
		return 3
	case l < 1024:
		return 4
	case l < 2048:
		return 5
	case l < 4096:
		return 6
	default:
		return 7
	}
}

func getMaxComponentLength(fullPath string) int {
	maxLen := 0
	for _, comp := range strings.Split(fullPath, "/") {
		if len(comp) > maxLen {
			maxLen = len(comp)
		}
	}
	return maxLen
}

var totalLenWeights = [8]float64{3.0, 2.0, 1.5, 1.0, 1.0, 1.5, 2.0, 3.0}
var maxCompWeights = [8]float64{1.0, 1.0, 1.0, 1.5, 2.0, 2.0, 2.0, 2.0}

func calculateNodeWeight(node *FileNode) float64 {
	w := 1.0

	if node.Size <= 4096 {
		w *= 2.0
	}
	if node.Size > 1048576 {
		w *= 2.0
	}

	depth := strings.Count(node.FullPath, "/")
	if depth >= 4 {
		w *= 2.0
	} else if depth >= 2 {
		w *= 1.5
	}

	w *= totalLenWeights[getLengthBucket(node.PathLength)]
	w *= maxCompWeights[getLengthBucket(getMaxComponentLength(node.FullPath))]

	return w
}

// updateCallPathInProg rewrites the path-carrying argument of callIdx to
// newPath. Returns true when the source path was actually rewritten; the
// rename destination follow-up stays best-effort.
func updateCallPathInProg(p *Prog, callIdx int, newPath string, r *randGen) bool {
	if callIdx < 0 || callIdx >= len(p.Calls) {
		return false
	}
	call := p.Calls[callIdx]
	if !reassignDataArgAt(p, call, 0, r, []byte(newPath+"\x00")) {
		return false
	}
	r.target.assignSizesCall(call)

	// rename 的目标路径跟随源迁移（L25）：目标基于旧源派生
	// （源._renamed_xxx）——源迁移后目标同步重新派生（对齐 L9 的 rename 处理）。
	if strings.Contains(call.Meta.Name, "rename") {
		updateCallPathByArgIdx(p, call, 1, newPath+"._renamed_"+randomSuffix(r.Rand), r)
	}
	return true
}

var lengthBuckets = []struct{ len, weight int }{
	{0, 10},   // random [1,511]
	{512, 30}, // sector boundary
	{0, 10},   // random [513,2048]
	{0, 15},   // random [2049,4095]
	{4096, 30},
	{0, 15}, // random [4097,8191]
	{8192, 30},
	{0, 10}, // random [8193,16384]
	{0, 12}, // random [16385,65536]
}

var lenBucketRanges = [][2]int{
	{1, 511},
	{513, 2048},
	{2049, 4095},
	{4097, 8191},
	{8193, 16384},
	{16385, 65536},
}

// pickMutationLength picks the shared length of a data-group mutation. With a
// known file size it uses the size-relative / page-boundary / ranges / free
// layers; without one it keeps the legacy boundary-weighted table.
func pickMutationLength(r *randGen, size uint64) uint64 {
	if size == 0 {
		return pickMutationLengthLegacy(r)
	}
	switch r.rollByProbs(hmdfsMutLenProbs[:]) {
	case 0: // size-relative
		cands := [...]uint64{size - 1, size, size + 1, size / 2, 2 * size}
		v := cands[r.Intn(len(cands))]
		if v > lenBoundaryCap {
			v = lenBoundaryCap
		}
		if v == 0 {
			v = 1
		}
		return v
	case 1: // page boundaries
		return []uint64{512, 4096, 8192}[r.Intn(3)]
	case 2: // legacy random ranges
		rr := lenBucketRanges[r.Intn(len(lenBucketRanges))]
		return uint64(r.Intn(rr[1]-rr[0]+1) + rr[0])
	default: // free random
		return uint64(r.Intn(65536) + 1)
	}
}

func pickMutationLengthLegacy(r *randGen) uint64 {
	buckets := lengthBuckets
	totalWeight := 0
	for _, b := range buckets {
		totalWeight += b.weight
	}
	bucketIdx := 0
	x := r.Intn(totalWeight)
	cum := 0
	for i, b := range buckets {
		cum += b.weight
		if x < cum {
			bucketIdx = i
			break
		}
	}
	b := buckets[bucketIdx]
	if b.len != 0 {
		return uint64(b.len)
	}
	rangeIdx := 0
	for _, lb := range buckets[:bucketIdx] {
		if lb.len == 0 {
			rangeIdx++
		}
	}
	if rangeIdx < len(lenBucketRanges) {
		rr := lenBucketRanges[rangeIdx]
		return uint64(r.Intn(rr[1]-rr[0]+1) + rr[0])
	}
	return uint64(r.Intn(4096) + 1)
}

// updateCallDataBuf resizes the data/receive buffer at arg[1] to newLen.
// For writes (buffer[in]) the new tail is filled with random bytes; for reads
// (buffer[out]) the content is irrelevant (the kernel overwrites it), the
// resize itself is what drives the count len[buf] via assignSizesCall.
func updateCallDataBuf(p *Prog, call *Call, newLen uint64, r *randGen) bool {
	if len(call.Args) < 2 {
		return false
	}
	ptrArg, ok := call.Args[1].(*PointerArg)
	if !ok {
		return false
	}
	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return false
	}
	newData := make([]byte, newLen)
	if dataArg.Dir() != DirOut {
		// Data() panics on output buffers; reads have no payload to preserve
		// anyway (the kernel overwrites the receive buffer).
		oldData := dataArg.Data()
		copy(newData, oldData)
		for i := uint64(len(oldData)); i < newLen; i++ {
			newData[i] = byte(r.Intn(256))
		}
	}
	return reassignDataArg(p, call, r, newData)
}

// getFdFilePathForCall resolves the file/dir path a call operates on:
// path-taking calls report their own path argument; fd-taking calls
// (including getdents64, whose arg0 is an fd) backtrack to the open/creat
// that produced the fd. The extraction-first order is deliberate: a
// call-name whitelist here drifted from the actual argument shapes and made
// getdents64 silently unresolvable (extractPathFromCall returns "" for an
// fd arg), skipping the backtrack that resolveFdTarget already uses.
func getFdFilePathForCall(ps []*Prog, pos GroupPosition) string {
	call := ps[pos.ProgIdx].Calls[pos.CallIdx]

	if path := extractPathFromCall(call); path != "" {
		return path
	}

	if len(call.Args) < 1 {
		return ""
	}
	fdArg, ok := call.Args[0].(*ResultArg)
	if !ok || fdArg.Res == nil {
		return ""
	}

	p := ps[pos.ProgIdx]
	for i := pos.CallIdx - 1; i >= 0; i-- {
		prevCall := p.Calls[i]
		if (strings.Contains(prevCall.Meta.Name, "open") ||
			strings.Contains(prevCall.Meta.Name, "creat")) &&
			prevCall.Ret != nil && prevCall.Ret == fdArg.Res {
			return extractPathFromCall(prevCall)
		}
	}
	return ""
}

func getFileSizeByPath(ps []*Prog, pos GroupPosition, ft *FileTree) uint64 {
	path := getFdFilePathForCall(ps, pos)
	if path == "" || ft == nil {
		return 0
	}
	node := ft.FindNode(path)
	if node != nil {
		return node.Size
	}
	return 0
}

func findOpenFdForPath(p *Prog, filePath string, beforeIdx int, want uint8) *ResultArg {
	for i := 0; i < beforeIdx && i < len(p.Calls); i++ {
		call := p.Calls[i]
		if !strings.Contains(call.Meta.Name, "open") && !strings.Contains(call.Meta.Name, "creat") {
			continue
		}
		path := extractPathFromCall(call)
		if path != filePath {
			continue
		}
		if want != 0 && fdCapOf(p.Target, call)&want != want {
			// The fd cannot serve the call (e.g. a read-only fd for a write):
			// the kernel would fail it at the VFS layer without reaching hmdfs.
			continue
		}
		fd := extractFdFromCall(call)
		if fd == nil {
			continue
		}
		closed := false
		for j := i + 1; j < beforeIdx && j < len(p.Calls); j++ {
			if strings.Contains(p.Calls[j].Meta.Name, "close") && usesFd(p.Calls[j], fd) {
				closed = true
				break
			}
		}
		if !closed {
			return fd
		}
	}
	return nil
}

// freshPathKindsBefore scans the calls before the root insertion slot and
// records the type each in-program creation will produce (mkdir -> dir,
// creat -> file, rename destination -> the source's type, resolved through
// the hint first and the tree second). The FileTree is synced only after
// execution, so a fresh path invented earlier in the same program is not
// queryable there; this local hint restores the type gate for the same-batch
// cases without writing the tree. A path created by both kinds (pathological)
// is treated as unknown.
func freshPathKindsBefore(p *Prog, ft *FileTree, end int) map[string]int {
	if p == nil {
		return nil
	}
	if end > len(p.Calls) {
		end = len(p.Calls)
	}
	m := make(map[string]int)
	set := func(path string, kind int) {
		if path == "" {
			return
		}
		if prev, ok := m[path]; ok && prev != kind {
			delete(m, path)
			return
		}
		m[path] = kind
	}
	kindOf := func(path string) (int, bool) {
		if k, ok := m[path]; ok {
			return k, true
		}
		if ft == nil {
			return 0, false
		}
		if n := ft.FindNode(path); n != nil {
			if n.Type == NodeTypeFile {
				return relTargetFile, true
			}
			return relTargetDir, true
		}
		return 0, false
	}
	for i := 0; i < end; i++ {
		c := p.Calls[i]
		if c == nil || c.Meta == nil {
			continue
		}
		switch c.Meta.CallName {
		case "mkdir":
			set(extractPathFromCall(c), relTargetDir)
		case "creat":
			set(extractPathFromCall(c), relTargetFile)
		case "open":
			if created, known := openCallCreatesFile(p.Target, c); known && created {
				set(extractPathFromCall(c), relTargetFile)
			}
		case "link":
			set(extractPathFromCallByArgIdx(c, 1), relTargetFile)
		case "symlink":
			linkPath := extractPathFromCallByArgIdx(c, 1)
			target := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(c, 0))
			if k, ok := kindOf(target); ok {
				set(linkPath, k)
			}
		case "unlink", "rmdir":
			delete(m, extractPathFromCall(c))
		case "rename":
			src, dst := extractRenamePaths(c)
			if k, ok := kindOf(src); ok {
				set(dst, k)
			}
			delete(m, src)
		}
	}
	return m
}

// dctBaseCandidates returns the p0 paths a new DCT root may use. link/symlink
// targets are always usable; their fresh linkpaths become usable only when the
// creating call ran before insertPos and succeeded. readlink only contributes
// its linkpath. This keeps a not-yet-executed linkpath out of the root base.
func dctBaseCandidates(p *Prog, insertPos int) []string {
	if p == nil {
		return nil
	}
	var out []string
	for i, call := range p.Calls {
		if call == nil || call.Meta == nil {
			continue
		}
		before := i < insertPos
		switch call.Meta.CallName {
		case "rename":
			src, dst := extractRenamePaths(call)
			if src != "" {
				out = append(out, src)
			}
			if dst != "" {
				out = append(out, dst)
			}
		case "link":
			target := extractPathFromCallByArgIdx(call, 0)
			linkPath := extractPathFromCallByArgIdx(call, 1)
			if target != "" {
				out = append(out, target)
			}
			if before && callSucceeded(call) && linkPath != "" {
				out = append(out, linkPath)
			}
		case "symlink":
			linkPath := extractPathFromCallByArgIdx(call, 1)
			target := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(call, 0))
			if target != "" {
				out = append(out, target)
			}
			if before && callSucceeded(call) && linkPath != "" {
				out = append(out, linkPath)
			}
		case "readlink":
			linkPath := extractPathFromCallByArgIdx(call, 0)
			if before && callSucceeded(call) && linkPath != "" {
				out = append(out, linkPath)
			}
		default:
			if path := extractPathFromCall(call); path != "" {
				out = append(out, path)
			}
		}
	}
	return out
}

// dctCallPaths returns the canonical path roles of a call for DCT base
// candidate collection. Symlink payloads are relative to the linkpath parent
// and must be resolved before they can be used as a FileTree coordinate.
func dctCallPaths(call *Call) []string {
	if call == nil || call.Meta == nil {
		return nil
	}
	switch call.Meta.CallName {
	case "rename":
		src, dst := extractRenamePaths(call)
		return []string{src, dst}
	case "link":
		return []string{
			extractPathFromCallByArgIdx(call, 0),
			extractPathFromCallByArgIdx(call, 1),
		}
	case "symlink":
		linkPath := extractPathFromCallByArgIdx(call, 1)
		target := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(call, 0))
		return []string{target, linkPath}
	case "readlink":
		return []string{extractPathFromCallByArgIdx(call, 0)}
	}
	return []string{extractPathFromCall(call)}
}

// linkPathKind classifies a program-local linkpath binding.
type linkPathKind int

const (
	linkPathHardlink linkPathKind = iota + 1
	linkPathSymlink
)

type linkPathInfo struct {
	Kind        linkPathKind
	FollowIsDir bool
	Target      string
	CreatorIdx  int
}

// freshLinkPathsBefore scans link/symlink calls before end and records their
// program-local linkpaths. A linkpath is not a FileTree node; a later
// link/symlink target must not be one (v1 forbids nested links).
func freshLinkPathsBefore(p *Prog, ft *FileTree, end int) map[string]linkPathInfo {
	if p == nil {
		return nil
	}
	if end > len(p.Calls) {
		end = len(p.Calls)
	}
	m := make(map[string]linkPathInfo)
	followKind := func(path string) (bool, bool) {
		if info, ok := m[path]; ok {
			return info.FollowIsDir, true
		}
		if ft != nil {
			if n := ft.FindNode(path); n != nil {
				return n.Type != NodeTypeFile, true
			}
		}
		return false, false
	}
	for i := 0; i < end; i++ {
		c := p.Calls[i]
		if c == nil || c.Meta == nil {
			continue
		}
		switch c.Meta.CallName {
		case "link":
			linkPath := extractPathFromCallByArgIdx(c, 1)
			if linkPath != "" {
				m[linkPath] = linkPathInfo{
					Kind: linkPathHardlink, Target: extractPathFromCallByArgIdx(c, 0), CreatorIdx: i,
				}
			}
		case "symlink":
			linkPath := extractPathFromCallByArgIdx(c, 1)
			target := symlinkCanonicalTarget(linkPath, extractPathFromCallByArgIdx(c, 0))
			if linkPath != "" {
				isDir, _ := followKind(target)
				m[linkPath] = linkPathInfo{
					Kind: linkPathSymlink, FollowIsDir: isDir, Target: target, CreatorIdx: i,
				}
			}
		case "unlink", "rmdir", "rename":
			// Removal invalidates a local binding; rename moves it.
			paths := dctCallPaths(c)
			for _, path := range paths {
				delete(m, path)
			}
		}
	}
	return m
}

// basePathIsFileHinted is BasePathIsFile with the in-program creation hint
// taking precedence over the unknown-path fallback.
func basePathIsFileHinted(profile *DFSProfile, ft *FileTree, rootCallName, basePath string, freshKinds map[string]int) bool {
	if h, ok := freshKinds[basePath]; ok {
		return h == relTargetFile
	}
	return profile.BasePathIsFile(ft, rootCallName, basePath)
}

func insertCallFromDCT(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, allowDirected bool) bool {
	recordLastMutation("insertCallFromDCT")
	if len(ps) == 0 {
		return false
	}
	// Config guard: the planner classifies a nil lcs/FileTree/hmcfg or a
	// Cids/ps length mismatch as directedPlanCfgErr, yet the caller falls
	// back to this insertion. Without the guard the fallback would index
	// hmcfg.Cids[0] / Cids[nodeIdx] out of range and panic the Proc instead.
	if lcs == nil || lcs.FileTree == nil || hmcfg == nil || len(hmcfg.Cids) < len(ps) {
		return false
	}

	p0 := ps[0]
	cid0 := hmcfg.Cids[0]
	var basePath string
	var basePath2 string = ""

	// basePath 路线：多数沿用 p0 已有路径上下文（fd 可复用、跨组路径关联），
	// 部分直接 FileTree 新选（fd 必然新开、操作面更广）。生成与变异统一生效。
	useFreshPath := r.Rand.Float64() < 0.30
	insertPos := r.biasedRand(len(p0.Calls)+1, 5)
	var pathCandidates []string
	if !useFreshPath {
		pathCandidates = dctBaseCandidates(p0, insertPos)
		if len(pathCandidates) > 0 {
			basePath = pathCandidates[r.Intn(len(pathCandidates))]
		}
	}

	biasCall := -1
	if insertPos > 0 && insertPos <= len(p0.Calls) {
		biasCall = p0.Calls[r.Intn(insertPos)].Meta.ID
	}

	var subCt *ChoiceTable
	if lcs != nil {
		subCt = lcs.FsopsChoiceTable
	}
	rootCallName := r.chooseRootCallName(subCt, biasCall)
	if rootCallName == "" {
		return false
	}

	// In-program creations are absent from the FileTree (it is synced only
	// after execution), so a fresh path's type is unknown there. Scan the
	// calls before the root slot for the creating call (mkdir/creat) and use
	// its intent as a local hint: a hinted mismatch is treated like a tree
	// mismatch below, without writing the tree.
	freshKinds := freshPathKindsBefore(p0, lcs.FileTree, insertPos)
	freshLinks := freshLinkPathsBefore(p0, lcs.FileTree, insertPos)

	// The candidate-derived basePath was chosen before the root was known, so
	// a file-only/dir-only root may land on a mismatched type (EISDIR/ENOTDIR
	// at execution, a wasted round). Re-pick a compatible candidate; with none
	// the type-aware FileTree selection below takes over.
	if basePath != "" && lcs != nil && lcs.FileTree != nil {
		if kind, ok := rootBaseConstraintFor(lcs.profileOrDefault(), rootCallName); ok {
			node := lcs.FileTree.FindNode(basePath)
			mismatch := false
			if node != nil {
				mismatch = !nodeKindMatches(node, kind)
			} else if h, ok := freshKinds[basePath]; ok && h != kind {
				mismatch = true
			}
			if mismatch {
				basePath = ""
				var compatible []string
				for _, cand := range pathCandidates {
					if (rootCallName == "link" || rootCallName == "symlink") &&
						freshLinks[cand].Kind != 0 {
						continue // link/symlink targets cannot be linkpaths
					}
					if n := lcs.FileTree.FindNode(cand); n != nil {
						if nodeKindMatches(n, kind) {
							compatible = append(compatible, cand)
						}
						continue
					}
					if h, ok := freshKinds[cand]; ok && h == kind {
						compatible = append(compatible, cand)
					}
				}
				if len(compatible) > 0 {
					basePath = compatible[r.Intn(len(compatible))]
				}
			}
		}
	}

	if basePath != "" && (rootCallName == "link" || rootCallName == "symlink") &&
		freshLinks[basePath].Kind != 0 {
		// v1 forbids nested links: link/symlink targets must be real tree
		// nodes, not another program-local linkpath.
		basePath = ""
	}

	// The mount root is never a legal base for the destructive roots: rmdir
	// on it returns EBUSY before the hmdfs trace, and a rename/creat derives
	// sibling names outside the mount (merge_view._creat_*, merge_view._renamed_*).
	// The candidate path comes from p0, so keep the invariant explicit and
	// fall back to the type-aware non-root selection below.
	if basePath != "" && lcs != nil && lcs.FileTree != nil && basePath == lcs.FileTree.Root.FullPath {
		basePath = ""
	}

	if basePath == "" {
		switch lcs.profileOrDefault().TargetConstraint(rootCallName) {
		case relTargetDir:
			dirNode := lcs.FileTree.GetRandomDir(r.Rand, cid0, true)
			if dirNode == nil {
				return false
			}
			basePath = dirNode.FullPath
		case relTargetFile:
			fileNode := lcs.FileTree.GetRandomFile(r.Rand, cid0)
			if fileNode == nil {
				return false
			}
			basePath = fileNode.FullPath
		default:
			if r.bin() {
				fileNode := lcs.FileTree.GetRandomFile(r.Rand, cid0)
				if fileNode != nil {
					basePath = fileNode.FullPath
				}
			}
			if basePath == "" {
				dirNode := lcs.FileTree.GetRandomDir(r.Rand, cid0, true)
				if dirNode == nil {
					return false
				}
				basePath = dirNode.FullPath
			}
		}
	}

	// 与种子生成同款预处理：mkdir/creat root 改用新建路径——变异侧也能
	// 产生新建路径竞争（dirtype/双 creat 等形态，root 与 PathSame 变体
	// 通过 GetPathByRelation 的空值 fallback 对齐到同一新路径）。
	if rootCallName == "mkdir" {
		// mkdir 需在树内目录下建新目录（TmpDir 也算目录）：basePath 是文件
		// 则取其父目录，不在树内（被删/树外路径）则随机选一个目录。
		if lcs != nil && lcs.FileTree != nil {
			node := lcs.FileTree.FindNode(basePath)
			switch {
			case nodeIsDir(node):
				// 树内目录——就地拼（D/mut_dir_x）
			case node != nil:
				// 树内文件——取其父目录（在该文件所在目录建新目录）
				if parent := lcs.FileTree.GetParent(node); parent != nil {
					basePath = parent.FullPath
				}
			default:
				// 不在树内——随机选一个目录
				dir := lcs.FileTree.GetRandomDir(r.Rand, cid0, true)
				if dir == nil {
					return false // 树内无目录——跳过本次插入
				}
				basePath = dir.FullPath
			}
		}
		basePath = joinTestPath(basePath, "mut_dir_"+randomSuffix(r.Rand))
	} else if rootCallName == "creat" {
		// creat appends to the base name; keep it relative when the
		// GlusterFS test root is the empty prefix.
		basePath = strings.TrimSuffix(basePath, "/") + "._creat_" + randomSuffix(r.Rand) + ".txt"
	}

	var ExistingFd *ResultArg = nil
	useExistFd := false
	if IsFdRequiredCall(rootCallName) {
		ExistingFd = findOpenFdForPath(p0, basePath, insertPos, capNeeded(rootCallName))
		if ExistingFd != nil {
			useExistFd = true
		}
	}

	if rootCallName == "rename" {
		basePath2 = lcs.pickRenameDstFor(basePath, PathNoRel, r.Rand)
	}
	// Failure-shape whitelist: with failShapeProb, rewrite the root target
	// into a shape whose error actually reaches a traced hmdfs function.
	if r.Rand.Float64() < failShapeProb {
		basePath, basePath2 = applyFailureShape(lcs, rootCallName, basePath, basePath2, r)
	}
	rootFileSize := uint64(0)
	if lcs != nil {
		node := lcs.FileTree.FindNode(basePath)
		if node != nil {
			rootFileSize = node.Size
		}
	}
	rootFlags := uint64(2)
	if lcs != nil && lcs.FileTree != nil {
		if nodeIsDir(lcs.FileTree.FindNode(basePath)) || freshKinds[basePath] == relTargetDir {
			rootFlags = r.target.GetConst("O_DIRECTORY")
		}
	}
	rootPath, rootPath2 := basePath, basePath2
	rootLinkPath := ""
	if rootCallName == "link" || rootCallName == "symlink" {
		rootPath2 = basePath
		rootPath = r.freshLinkPath(basePath, rootCallName)
		if rootCallName == "symlink" {
			rootLinkPath = rootPath
		}
	}
	rootCalls := r.generateCallByName(stateFromProg(p0), sCalls, rootCallName, rootPath, rootPath2, cid0, ExistingFd, useExistFd, rootFileSize, rootFlags)
	if len(rootCalls) == 0 || hasNilCall(rootCalls) {
		// A nil call would panic at Serialize; a clean abort leaves ps
		// untouched and the caller falls back to the standard mutator.
		return false
	}

	if insertPos >= len(p0.Calls) {
		p0.Calls = append(p0.Calls, rootCalls...)
	} else {
		p0.Calls = append(p0.Calls[:insertPos], append(rootCalls, p0.Calls[insertPos:]...)...)
	}

	// Reference time for time-aligned concurrent insertions: 0 = program-start
	// alignment (root inserted at the beginning); the end of the call before
	// insertPos when it has timing info; -1 (no reference) otherwise, so
	// concurrent progs fall back to index alignment. Times are in the global
	// TSC domain. Inserting the root calls at insertPos does not shift
	// indices below insertPos, so p0.Calls[insertPos-1] is still the original
	// predecessor.
	refTime := int64(-1)
	if insertPos == 0 {
		refTime = 0
	} else if p0.Calls[insertPos-1].CheckInfo != nil {
		refTime = int64(p0.Calls[insertPos-1].CheckInfo.Etime) - lcs.tscoffFor(0)
	}

	// The variant picker and the fresh-target open flags judge the base by
	// basePathIsFile, whose ambiguous-root fallback treats an unknown path as
	// a file. When the base is an in-program fresh path, the local creation
	// hint knows better: use it so Same-relation variants follow the actual
	// type (file-only Same on a fresh dir would open EISDIR; dir-only Same on
	// a fresh dir would be rejected).
	baseIsFile := basePathIsFileHinted(lcs.profileOrDefault(), lcs.FileTree, rootCallName, basePath, freshKinds)

	for nodeIdx := 1; nodeIdx < len(ps); nodeIdx++ {
		useExistFd = true
		ExistingFd = nil
		p := ps[nodeIdx]
		cid := hmcfg.Cids[nodeIdx]

		variant := lcs.ChooseConcurrentCallFiltered(rootCallName, r.Rand, baseIsFile, basePath, allowDirected)
		if variant == nil {
			// No usable variant for this base: skip this node's insertion for
			// this round (ps is already full — no node is dropped).
			continue
		}
		temporal := lcs.GetDCT().ChooseTemporal(rootCallName, *variant, r.Rand)

		// One timing helper per variant, mirroring insertDirectedBatch: the HB
		// form places the variant at the first boundary after the root, the CC
		// form at the closest boundary. Both fail together (they need refTime
		// and a timing anchor), so calling them in sequence would only
		// double-count the timingAlignMiss metric.
		ConcurrentInsertPos := -1
		if temporal == TemporalHB {
			ConcurrentInsertPos = firstBoundaryAfter(p, refTime, lcs.tscoffFor(nodeIdx))
		} else {
			ConcurrentInsertPos = TimeAlignedInsertPos(p, refTime, lcs.tscoffFor(nodeIdx))
		}
		timingAligned := ConcurrentInsertPos >= 0
		if ConcurrentInsertPos < 0 {
			ConcurrentInsertPos = min(insertPos, len(p.Calls))
		}

		concurrentPath := ""

		concurrentPath2 := ""
		if variant.CallName == "readlink" {
			// readlink follows the root symlink's program-local linkpath; the
			// relation-derived path would be the symlink target instead.
			if rootLinkPath == "" {
				continue
			}
			concurrentPath = rootLinkPath
		}
		if variant.CallName == "mkdir" {
			// mkdir as a variant follows its geometric position: Child
			// creates a fresh child inside the base, Sibling a fresh sibling
			// inside the parent, NoRel inside an unrelated directory (the new
			// name is what makes these viable — an existing target would be
			// EEXIST forever). Same/Parent target the existing directory
			// itself and are handled by the generic relation path below.
			switch variant.PathRelation {
			case PathChild:
				concurrentPath = r.freshChildPath(basePath, "mkdir")
			case PathSibling:
				concurrentPath = r.freshChildPath(GetParentDir(basePath), "mkdir")
			case PathNoRel:
				if dir := lcs.FileTree.getRandomUnrelatedPath(basePath, r.Rand, relTargetDir); dir != "" {
					concurrentPath = r.freshChildPath(dir, "mkdir")
				}
			}
		}
		if variant.CallName == "rename" {
			concurrentPath, concurrentPath2 = lcs.GetPathsForRenameVariant(basePath, "", variant.PathRelation, r.Rand, false)
			if concurrentPath == "" {
				// Rename relation not generatable here (rare under relOK
				// filtering): skip this node's variant insertion for this
				// round (ps is full).
				continue
			}
		} else if concurrentPath == "" {
			constraint := lcs.profileOrDefault().TargetConstraint(variant.CallName)
			concurrentPath = lcs.FileTree.GetPathByRelation(basePath, "", variant.PathRelation, r.Rand, false, constraint)
			if concurrentPath == "" {
				// No generatable path of the right type: skip this node's
				// variant insertion for this round (ps is already full).
				continue
			}
		}
		if IsFdRequiredCall(variant.CallName) {
			ExistingFd = findOpenFdForPath(p, concurrentPath, ConcurrentInsertPos, capNeeded(variant.CallName))
			if ExistingFd == nil && timingAligned {
				// Time alignment may place the call before the open;
				// fall back to the index-aligned position for fd usability.
				ConcurrentInsertPos = min(insertPos, len(p.Calls))
				ExistingFd = findOpenFdForPath(p, concurrentPath, ConcurrentInsertPos, capNeeded(variant.CallName))
			}
			if ExistingFd == nil {
				useExistFd = false
			}
		}

		concurrentFileSize := uint64(0)
		concurrentFlags := uint64(2)
		if lcs != nil && lcs.FileTree != nil {
			node := lcs.FileTree.FindNode(concurrentPath)
			if node != nil {
				concurrentFileSize = node.Size
				if nodeIsDir(node) {
					concurrentFlags = r.target.GetConst("O_DIRECTORY")
				}
			} else if !baseIsFile {
				// Fresh target of a directory-intent root (e.g. mkdir): the
				// open variant must carry O_DIRECTORY or VFS rejects it
				// before the hmdfs trace point.
				concurrentFlags = r.target.GetConst("O_DIRECTORY")
			}
		}
		concurrentCalls := r.generateCallByName(stateFromProg(p), sCalls, variant.CallName, concurrentPath, concurrentPath2, cid, ExistingFd, useExistFd, concurrentFileSize, concurrentFlags)
		if len(concurrentCalls) == 0 || hasNilCall(concurrentCalls) {
			// A nil call would panic at Serialize; skip this node's variant
			// for the round (ps is already full — no node is dropped).
			continue
		}
		if ConcurrentInsertPos >= len(p.Calls) {
			p.Calls = append(p.Calls, concurrentCalls...)
		} else {
			p.Calls = append(p.Calls[:ConcurrentInsertPos], append(concurrentCalls, p.Calls[ConcurrentInsertPos:]...)...)
		}
	}

	return true
}

func progHaveFd(p *Prog) bool {
	if len(p.Calls) == 0 {
		return false
	}
	for i := 0; i < len(p.Calls); i++ {
		callName := p.Calls[i].Meta.Name
		if strings.Contains(callName, "open") {
			return true
		}
	}
	return false
}

// TimeAlignedInsertPos returns the insertion position j in p whose execution
// boundary time (the end of the previous call; 0 for j == 0) is closest to
// refTime. Times are in the global TSC domain (raw TSC minus the VM's TSC
// offset). refTime < 0 means "no reference": random behavior. Returns -1
// when refTime >= 0 but no call has timing info.
func insertDerivedCallFromDCT(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, allowDirected, insertAfterAnchor bool) bool {
	recordLastMutation("insertDerivedCallFromDCT")
	if len(ps) == 0 || lcs == nil || lcs.FileTree == nil || hmcfg == nil || len(hmcfg.Cids) < len(ps) {
		return false
	}
	p0 := ps[0]
	ft := lcs.FileTree
	derivation, ok := sampleSeedDerivation(p0, ft, lcs.CausalDerivationTable, r, !insertAfterAnchor)
	if !ok {
		return false
	}
	rootCallName := derivation.RootCall
	rp := derivation.RootPaths()
	basePath := rp.Source()
	basePath2 := rp.Dest()
	if basePath == "" {
		return false
	}
	insertPos := r.biasedRand(len(p0.Calls)+1, 5)
	if insertAfterAnchor {
		insertPos = derivation.Anchor.Pos.CallIdx + 1
	}
	if insertPos < 0 {
		insertPos = 0
	}
	if insertPos > len(p0.Calls) {
		insertPos = len(p0.Calls)
	}

	freshKinds := freshPathKindsBefore(p0, ft, insertPos)
	switch rootCallName {
	case "mkdir", "creat", "rename":
		if kind := seedFreshPathKind(derivation); kind != 0 {
			freshKinds[basePath2] = kind
			if rootCallName != "rename" {
				freshKinds[basePath] = kind
			}
		}
	}

	rootFileSize := directedNodeSize(lcs, basePath)
	rootFlags := directedNodeFlags(r, lcs, basePath)
	var rootFd *ResultArg
	rootUseFd := false
	if IsFdRequiredCall(rootCallName) {
		rootFd = findOpenFdForPath(p0, basePath, insertPos, capNeeded(rootCallName))
		rootUseFd = rootFd != nil
	}
	rootPath, rootPath2 := basePath, basePath2
	rootLinkPath := ""
	switch rp.Kind {
	case DualLink, DualSymlink:
		rootPath2 = rp.TargetPath
		rootPath = rp.LinkPath
		if rp.Kind == DualSymlink {
			rootLinkPath = rp.LinkPath
		}
	case DualRename:
		rootPath, rootPath2 = rp.PrePath, rp.PostPath
	}
	rootCalls := r.generateCallByName(stateFromProg(p0), sCalls, rootCallName,
		rootPath, rootPath2, hmcfg.Cids[0], rootFd, rootUseFd, rootFileSize, rootFlags)
	if len(rootCalls) == 0 || hasNilCall(rootCalls) {
		return false
	}
	if insertPos >= len(p0.Calls) {
		p0.Calls = append(p0.Calls, rootCalls...)
	} else {
		p0.Calls = append(p0.Calls[:insertPos], append(rootCalls, p0.Calls[insertPos:]...)...)
	}
	rootStart := insertPos
	rootEnd := rootStart + len(rootCalls)

	refTime := int64(-1)
	if insertAfterAnchor {
		if derivation.Anchor.Pos.CallIdx < len(p0.Calls) && p0.Calls[derivation.Anchor.Pos.CallIdx].CheckInfo != nil {
			refTime = int64(p0.Calls[derivation.Anchor.Pos.CallIdx].CheckInfo.Etime) - lcs.tscoffFor(0)
		}
	} else if rootStart == 0 {
		refTime = 0
	} else if p0.Calls[rootStart-1].CheckInfo != nil {
		refTime = int64(p0.Calls[rootStart-1].CheckInfo.Etime) - lcs.tscoffFor(0)
	}

	rootObservedPath := derivation.RootDest
	if lcs.profileOrDefault().Name == "glusterfs" {
		rootObservedPath = rp.ObservedPath()
	}
	baseIsFile := basePathIsFileHinted(lcs.profileOrDefault(), ft, rootCallName, rootObservedPath, freshKinds)

	for nodeIdx := 1; nodeIdx < len(ps); nodeIdx++ {
		p := ps[nodeIdx]
		cid := hmcfg.Cids[nodeIdx]
		variant := lcs.ChooseConcurrentCallFiltered(rootCallName, r.Rand, baseIsFile, rootObservedPath, allowDirected)
		if variant == nil {
			continue
		}
		temporal := lcs.GetDCT().ChooseTemporal(rootCallName, *variant, r.Rand)

		variantPos := rootStart
		if temporal == TemporalHB {
			variantPos = rootEnd
		}
		if !insertAfterAnchor {
			aligned := -1
			if temporal == TemporalHB {
				aligned = firstBoundaryAfter(p, refTime, lcs.tscoffFor(nodeIdx))
			} else {
				aligned = TimeAlignedInsertPos(p, refTime, lcs.tscoffFor(nodeIdx))
			}
			if aligned >= 0 {
				variantPos = aligned
			} else if temporal == TemporalHB {
				variantPos = rootEnd
			} else {
				variantPos = rootStart
			}
		}
		if variantPos < 0 {
			variantPos = 0
		}
		if variantPos > len(p.Calls) {
			variantPos = len(p.Calls)
		}

		variantPath := ""
		variantPath2 := ""
		if variant.CallName == "readlink" {
			if rootLinkPath == "" {
				continue
			}
			variantPath = rootLinkPath
		}
		switch variant.CallName {
		case "mkdir":
			switch variant.PathRelation {
			case PathChild:
				if baseIsFile {
					continue
				}
				variantPath = r.freshChildPath(rootObservedPath, "mkdir")
			case PathSibling:
				variantPath = r.freshChildPath(GetParentDir(rootObservedPath), "mkdir")
			case PathNoRel:
				if dir := ft.getRandomUnrelatedPath(rootObservedPath, r.Rand, relTargetDir); dir != "" {
					variantPath = r.freshChildPath(dir, "mkdir")
				}
			}
		case "rename":
			variantPath, variantPath2 = lcs.GetPathsForRenameVariant(rootObservedPath, "", variant.PathRelation, r.Rand, false)
		case "readlink":
			// Bound to rootLinkPath above; never derive from the target.
		default:
			constraint := lcs.profileOrDefault().TargetConstraint(variant.CallName)
			variantPath = ft.GetPathByRelation(rootObservedPath, "", variant.PathRelation, r.Rand, false, constraint)
		}
		if variant.CallName == "link" || variant.CallName == "symlink" {
			if variantPath == "" {
				continue
			}
			variantPath2 = variantPath
			variantPath = r.freshLinkPath(variantPath, variant.CallName)
		}
		// mkdir Same/Parent target existing tree paths (the root itself or its
		// parent), not a fresh child/sibling. Mirror the generic relation
		// fallback used by insertCallFromDCT instead of silently dropping the
		// DCT-selected variant.
		if variantPath == "" && variant.CallName == "mkdir" &&
			(variant.PathRelation == PathSame || variant.PathRelation == PathParent) {
			constraint := lcs.profileOrDefault().TargetConstraint(variant.CallName)
			variantPath = ft.GetPathByRelation(rootObservedPath, "", variant.PathRelation, r.Rand, false, constraint)
		}
		if variantPath == "" {
			continue
		}

		var variantFd *ResultArg
		variantUseFd := true
		if IsFdRequiredCall(variant.CallName) {
			variantFd = findOpenFdForPath(p, variantPath, variantPos, capNeeded(variant.CallName))
			if variantFd == nil {
				variantUseFd = false
			}
		}
		variantSize := directedNodeSize(lcs, variantPath)
		variantFlags := directedNodeFlags(r, lcs, variantPath)
		if !baseIsFile {
			if n := ft.FindNode(variantPath); n == nil {
				variantFlags = r.target.GetConst("O_DIRECTORY")
			}
		}
		calls := r.generateCallByName(stateFromProg(p), sCalls, variant.CallName,
			variantPath, variantPath2, cid, variantFd, variantUseFd, variantSize, variantFlags)
		if len(calls) == 0 || hasNilCall(calls) {
			continue
		}
		if variantPos >= len(p.Calls) {
			p.Calls = append(p.Calls, calls...)
		} else {
			p.Calls = append(p.Calls[:variantPos], append(calls, p.Calls[variantPos:]...)...)
		}
	}
	return true
}

func TimeAlignedInsertPos(p *Prog, refTime int64, tscoff int64) int {
	return timeAlignedInRange(p, refTime, tscoff, 0, len(p.Calls))
}

// firstBoundaryAfter returns the first call boundary at or after refTime: the
// causal-insertion counterpart of TimeAlignedInsertPos. Inserting a variant
// there makes it start after the root finishes, favoring HB pairs. Returns -1
// without any timing anchor (caller falls back to index alignment).
func firstBoundaryAfter(p *Prog, refTime int64, tscoff int64) int {
	if refTime < 0 {
		atomic.AddUint64(&timingAlignMissCount, 1)
		return -1
	}
	hasTiming := false
	for j := 0; j <= len(p.Calls); j++ {
		if j > 0 && p.Calls[j-1].CheckInfo != nil {
			hasTiming = true
			break
		}
	}
	if !hasTiming {
		atomic.AddUint64(&timingAlignMissCount, 1)
		return -1
	}
	for j := 0; j <= len(p.Calls); j++ {
		var t uint64
		if j == 0 {
			t = 0
		} else {
			ci := p.Calls[j-1].CheckInfo
			if ci == nil {
				continue
			}
			t = uint64(int64(ci.Etime) - tscoff)
		}
		if int64(t) >= refTime {
			return j
		}
	}
	return len(p.Calls)
}

// timeAlignedInRange is TimeAlignedInsertPos restricted to [start, end].
func timeAlignedInRange(p *Prog, refTime int64, tscoff int64, start, end int) int {
	if refTime < 0 || start > end {
		atomic.AddUint64(&timingAlignMissCount, 1)
		return -1
	}
	// Without any timing anchor in the range the boundary times are
	// meaningless; signal the caller to fall back to random behavior.
	hasTiming := false
	for j := start; j <= end; j++ {
		if j > 0 && p.Calls[j-1].CheckInfo != nil {
			hasTiming = true
			break
		}
	}
	if !hasTiming {
		atomic.AddUint64(&timingAlignMissCount, 1)
		return -1
	}
	best := -1
	var bestDist uint64
	for j := start; j <= end; j++ {
		var t uint64
		if j == 0 {
			t = 0
		} else {
			ci := p.Calls[j-1].CheckInfo
			if ci == nil {
				continue
			}
			t = uint64(int64(ci.Etime) - tscoff)
		}
		ref := uint64(refTime)
		var d uint64
		if t >= ref {
			d = t - ref
		} else {
			d = ref - t
		}
		if best == -1 || d < bestDist {
			best, bestDist = j, d
		}
	}
	return best
}

// ProgTimings extracts the per-call execution timeline of every prog as
// flattened [Stime, Etime] pairs (0/0 for calls without CheckInfo) for RPC
// transfer. CheckInfo is runtime-only and not part of the prog serialization,
// so this is how corpus seeds keep their timeline across fuzzer restarts.
func ProgTimings(ps []*Prog) [][]uint64 {
	timings := make([][]uint64, len(ps))
	for i, p := range ps {
		if p == nil {
			continue
		}
		t := make([]uint64, 2*len(p.Calls))
		for j, call := range p.Calls {
			if call == nil || call.CheckInfo == nil {
				continue
			}
			t[2*j] = call.CheckInfo.Stime
			t[2*j+1] = call.CheckInfo.Etime
		}
		timings[i] = t
	}
	return timings
}

// ApplyProgTimings restores the per-call CheckInfo timeline of progs from a
// ProgTimings payload (e.g. a corpus seed re-delivered after a fuzzer
// restart) and returns the number of calls that got a CheckInfo. Only calls
// with a non-zero Etime get one: 0/0 pairs mean "no timing observed" and stay
// nil, matching the no-anchor fallback. Calls that already carry a CheckInfo
// are left untouched, and a shape mismatch (different prog layout) is ignored
// safely.
func ApplyProgTimings(ps []*Prog, timings [][]uint64) int {
	restored := 0
	for i, p := range ps {
		if p == nil || i >= len(timings) {
			continue
		}
		t := timings[i]
		if len(t) != 2*len(p.Calls) {
			continue
		}
		for j, call := range p.Calls {
			if call == nil || call.CheckInfo != nil {
				continue
			}
			stime, etime := t[2*j], t[2*j+1]
			if etime == 0 {
				continue
			}
			call.CheckInfo = &FileMetadata{Stime: stime, Etime: etime}
			restored++
		}
	}
	return restored
}

func findInsertPosition(p *Prog, r *randGen, useFd int, sCalls *SpecialCalls, refTime int64, tscoff int64, want uint8, filePath string, ft *FileTree) (int, *ResultArg) {
	//useFd: 0 is false, 1 is file, 2 is dir; want filters the fd capability
	//(all requested bits must be present, 0 = any); filePath (when non-empty)
	//restricts the fd to an open of that very path so pattern semantics keep
	//operating on the same file/dir; ft classifies an open by the tree node
	//type when known (a plain open of a directory without O_DIRECTORY must not
	//be reused as a file fd).
	if useFd > 0 {
		if len(p.Calls) == 0 {
			return r.Intn(len(p.Calls) + 1), nil //没有适合的 fd，无法插入
		}

		type fdRange struct {
			openIdx  int
			closeIdx int
			fd       *ResultArg
		}
		var dirFdRanges []fdRange
		var fileFdRanges []fdRange
		var allFdRanges []fdRange

		openCalls := findCallsByName(p, "open")
		closeCalls := findCallsByName(p, "close")

		for _, openIdx := range openCalls {
			if openIdx >= len(p.Calls) {
				continue
			}
			openCall := p.Calls[openIdx]

			fd := extractFdFromCall(openCall)
			if fd == nil {
				continue
			}
			if want != 0 && fdCapOf(p.Target, openCall)&want != want {
				continue
			}
			if filePath != "" && extractPathFromCall(openCall) != filePath {
				continue
			}
			correspondingClose := -1
			for _, closeIdx := range closeCalls {
				if closeIdx > openIdx {
					closeCall := p.Calls[closeIdx]
					if usesFd(closeCall, fd) {
						correspondingClose = closeIdx
						break
					}
				}
			}

			if correspondingClose == -1 {
				correspondingClose = len(p.Calls)
			}
			isDir := isOpenDirectory(openCall)
			if ft != nil {
				if path := extractPathFromCall(openCall); path != "" {
					if node := ft.FindNode(path); node != nil {
						isDir = nodeIsDir(node)
					}
				}
			}
			if isDir {
				dirFdRanges = append(dirFdRanges, fdRange{openIdx: openIdx, closeIdx: correspondingClose, fd: fd})
			} else {
				fileFdRanges = append(fileFdRanges, fdRange{openIdx: openIdx, closeIdx: correspondingClose, fd: fd})
			}
			allFdRanges = append(allFdRanges, fdRange{openIdx: openIdx, closeIdx: correspondingClose, fd: fd})
		}
		if useFd == 1 {
			if len(fileFdRanges) == 0 {
				return r.Intn(len(p.Calls) + 1), nil
			}
			chosenRange := fileFdRanges[r.Intn(len(fileFdRanges))]
			startidx := chosenRange.openIdx + 1
			endidx := chosenRange.closeIdx
			resultidx := timeAlignedInRange(p, refTime, tscoff, startidx, endidx)
			if resultidx < 0 {
				resultidx = int(r.randRange(uint64(startidx), uint64(endidx)))
			}
			return int(resultidx), chosenRange.fd
		} else if useFd == 2 {
			if len(dirFdRanges) == 0 {
				return r.Intn(len(p.Calls) + 1), nil
			}
			chosenRange := dirFdRanges[r.Intn(len(dirFdRanges))]
			startidx := chosenRange.openIdx + 1
			endidx := chosenRange.closeIdx
			resultidx := timeAlignedInRange(p, refTime, tscoff, startidx, endidx)
			if resultidx < 0 {
				resultidx = int(r.randRange(uint64(startidx), uint64(endidx)))
			}
			return int(resultidx), chosenRange.fd
		} else if useFd == 3 {
			if len(allFdRanges) == 0 {
				return r.Intn(len(p.Calls) + 1), nil
			}
			chosenRange := allFdRanges[r.Intn(len(allFdRanges))]
			startidx := chosenRange.openIdx + 1
			endidx := chosenRange.closeIdx
			resultidx := timeAlignedInRange(p, refTime, tscoff, startidx, endidx)
			if resultidx < 0 {
				resultidx = int(r.randRange(uint64(startidx), uint64(endidx)))
			}
			return int(resultidx), chosenRange.fd
		} else {
			resultidx := TimeAlignedInsertPos(p, refTime, tscoff)
			if resultidx < 0 {
				resultidx = r.Intn(len(p.Calls) + 1)
			}
			return resultidx, nil
		}

	} else {
		if len(p.Calls) == 0 {
			return 0, nil
		}
		resultidx := TimeAlignedInsertPos(p, refTime, tscoff)
		if resultidx < 0 {
			resultidx = r.Intn(len(p.Calls) + 1)
		}
		return resultidx, nil
	}

}

func genPreadCallWithFd(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg) *Call {
	meta := r.target.Syscalls[sCalls.Pread64Id]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	bufSize := r.randBufLen()

	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)

	bufArg := MakeOutDataArg(bufPtrType.Elem, DirOut, bufSize)
	bufPtr := r.allocAddr(s, bufPtrType, DirOut, bufArg.Size(), bufArg)

	args := make([]Arg, len(meta.Args))
	fdArg := MakeResultArg(meta.Args[0].Type.(*ResourceType), DirIn, fd, 0)
	args[0] = fdArg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, uint64(bufSize))
	args[3] = MakeConstArg(posType, DirIn, uint64(r.randInt(64)))

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func genFsyncCallWithFd(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg) *Call {
	meta := r.target.Syscalls[sCalls.FsyncId]
	if meta == nil {
		return nil
	}

	args := make([]Arg, len(meta.Args))
	fdArg := MakeResultArg(meta.Args[0].Type.(*ResourceType), DirIn, fd, 0)
	args[0] = fdArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func genFdatasyncCallWithFd(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg) *Call {
	meta := r.target.Syscalls[sCalls.FdatasyncId]
	if meta == nil {
		return nil
	}

	args := make([]Arg, len(meta.Args))
	fdArg := MakeResultArg(meta.Args[0].Type.(*ResourceType), DirIn, fd, 0)
	args[0] = fdArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func removeStashCall(ps []*Prog, pidx int, r *randGen) bool {
	if pidx < 0 || pidx >= len(ps) {
		return false
	}
	p := ps[pidx]
	if len(p.Calls) <= 3 {
		return false
	}

	candidates := make([]int, 0)
	for i, call := range p.Calls {
		name := call.Meta.Name
		if strings.Contains(name, "pwrite64") || strings.Contains(name, "pread64") {
			candidates = append(candidates, i)
		}
	}

	if len(candidates) == 0 {
		return false
	}

	idx := candidates[r.Intn(len(candidates))]
	p.RemoveCall(idx)
	shiftGeneralFailPos(ps[0], pidx, idx, -1)
	return true
}

func mutateStashTargetNodes(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	for pidx, p := range ps {
		if !p.HasNetFail {
			continue
		}

		netDownCalls := findCallsByName(p, "syz_failure_net_down")
		if len(netDownCalls) == 0 {
			continue
		}

		callIdx := netDownCalls[0]
		call := p.Calls[callIdx]

		if len(call.Args) < 1 {
			continue
		}

		ptrArg, ok := call.Args[0].(*PointerArg)
		if !ok {
			continue
		}

		dataArg, ok := ptrArg.Res.(*DataArg)
		if !ok {
			continue
		}

		oldCmd := string(dataArg.Data())

		if hmcfg.Node_num <= 1 {
			return false
		}
		newNodes := r.RandSetExcept(0, hmcfg.Node_num-1, 1+r.Intn(hmcfg.Node_num-1), pidx)

		var cmdBuilder strings.Builder
		cmdBuilder.WriteString("iptables -F;iptables -X;")
		cmdBuilder.WriteString(genIptablesDropCmd(hmcfg.InitIp, newNodes))
		newCmd := cmdBuilder.String() + "\x00"

		if newCmd == oldCmd {
			return false
		}

		dataArg.data = []byte(newCmd)
		return true
	}
	return false
}

func findCallsByName(p *Prog, name string) []int {
	var indices []int
	for i, call := range p.Calls {
		if strings.Contains(call.Meta.Name, name) {
			indices = append(indices, i)
		}
	}
	return indices
}

func genFsyncCall(p *Prog, r *randGen, sCalls *SpecialCalls) *Call {
	meta := r.target.Syscalls[sCalls.FsyncId]
	if meta == nil {
		return nil
	}

	fd := findAvailableFd(p)
	if fd == nil {
		return nil
	}
	fsyncfdtype := meta.Args[0].Type.(*ResourceType)
	fdarg := MakeResultArg(fsyncfdtype, DirIn, fd, 0)
	args := make([]Arg, len(meta.Args))
	args[0] = fdarg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func genFdatasyncCall(p *Prog, r *randGen, sCalls *SpecialCalls) *Call {
	meta := r.target.Syscalls[sCalls.FdatasyncId]
	if meta == nil {
		return nil
	}

	fd := findAvailableFd(p)
	if fd == nil {
		return nil
	}
	fdatasyncfdtype := meta.Args[0].Type.(*ResourceType)
	fdarg := MakeResultArg(fdatasyncfdtype, DirIn, fd, 0)
	args := make([]Arg, len(meta.Args))
	args[0] = fdarg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func findAvailableFd(p *Prog) *ResultArg {
	for _, call := range p.Calls {
		if strings.Contains(call.Meta.Name, "open") && call.Ret != nil {
			return call.Ret
		}
	}
	return nil
}

/******************************* Dcache Seed Mutation ************************************/

const (
	DcacheMutOpType = iota
	DcacheMutPathName
	//DcacheMutDelay
	//DcacheMutFailPos
	DcacheMutSyncPos
	DcacheMutTypeCount
)

const (
	DcacheOpInsert = iota
	DcacheOpRemove
	DcacheOpSwap
)

func MutateDcacheProg(ps []*Prog, rs rand.Source, ct *ChoiceTable, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	if len(ps) == 0 {
		return false
	}

	r := newRand(ps[0].Target, rs)
	r.hmcfg = hmcfg

	mutType := r.Intn(DcacheMutTypeCount)

	switch mutType {
	case DcacheMutOpType:
		return mutateDcacheOpType(ps, r, sCalls, ct, hmcfg)
	case DcacheMutPathName:
		return mutateDcachePathName(ps, r, sCalls, hmcfg)
	/*case DcacheMutDelay:
	return mutateDcacheDelay(ps, r, sCalls)*/
	/*case DcacheMutFailPos:
	return mutateDcacheFailPos(ps, r, sCalls)*/
	case DcacheMutSyncPos:
		return mutateDcacheSyncPos(ps, r, sCalls)
	}
	return false
}

func mutateDcacheOpType(ps []*Prog, r *randGen, sCalls *SpecialCalls, ct *ChoiceTable, hmcfg *Hmdfs_config) bool {
	for pidx, p := range ps {
		if !p.IsDCache {
			continue
		}

		opType := r.Intn(3)
		switch opType {
		//TODO: insert 里 getdents64 的处理没有仔细检查，以及 swap 的重要性令人怀疑
		case DcacheOpInsert:
			return insertDcacheCall(ps, pidx, p, r, sCalls, hmcfg)
		case DcacheOpRemove:
			return removeDcacheCall(ps, pidx, p, r)
		case DcacheOpSwap:
			return swapDcacheCalls(p, r)
		}
	}
	return false
}

func insertDcacheCall(ps []*Prog, pidx int, p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	if len(p.Calls) == 0 {
		return false
	}

	callTypes := []string{"mkdir", "rmdir", "creat", "unlink", "getdents64", "rename"}
	callType := callTypes[r.Intn(len(callTypes))]

	if callType == "getdents64" {
		return insertGetdents64Call(ps, pidx, p, r, sCalls, hmcfg)
	}

	insertPos := r.Intn(len(p.Calls) + 1)

	var newCall *Call
	switch callType {
	case "mkdir":
		newCall = genMkdirCall(p, r, sCalls, hmcfg)
	case "rmdir":
		newCall = genRmdirCall(p, r, sCalls, hmcfg)
	case "creat":
		newCall = genCreatCall(p, r, sCalls, hmcfg)
	case "unlink":
		newCall = genUnlinkCall(p, r, sCalls, hmcfg)
	case "rename":
		newCall = genRenameCall(p, r, sCalls, hmcfg)
	}

	if newCall == nil {
		return false
	}

	if insertPos >= len(p.Calls) {
		p.Calls = append(p.Calls, newCall)
	} else {
		p.Calls = append(p.Calls[:insertPos], append([]*Call{newCall}, p.Calls[insertPos:]...)...)
	}
	return true
}

func insertGetdents64Call(ps []*Prog, pidx int, p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	// Self-contained insertion: open(O_DIRECTORY) + getdents64 + close on a
	// directory path of the dcache seed, so the insertion does not depend on
	// the seed carrying a directory fd (it never does; the old candidate
	// filter made this a permanent no-op). getdents64 is a test call here;
	// it exercises the dcache directory-read path; verification of directory
	// contents is left to stat/symsc (see DAG_KNOWN_ISSUES.md #19).
	// Directory operations (mkdir/rmdir) are preferred as candidates: opening
	// a file path with O_DIRECTORY fails and voids the test call.
	var dirCandidates []string
	seen := make(map[string]bool)
	collect := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		dirCandidates = append(dirCandidates, path)
	}
	for _, call := range p.Calls {
		if strings.Contains(call.Meta.Name, "mkdir") || strings.Contains(call.Meta.Name, "rmdir") {
			collect(extractPathFromCall(call))
		}
	}
	if len(dirCandidates) == 0 {
		for _, call := range p.Calls {
			collect(extractPathFromCall(call))
		}
	}
	if len(dirCandidates) == 0 {
		return false
	}
	dirPath := dirCandidates[r.Intn(len(dirCandidates))]

	openCall, fd := genOpenDirCallWithPath(p, r, sCalls, dirPath)
	if openCall == nil || fd == nil {
		return false
	}
	gdCall := genGetdents64CallWithFd(p, r, sCalls, fd)
	if gdCall == nil {
		return false
	}
	closeCall := genCloseCallWithFd(p, r, sCalls, fd)
	if closeCall == nil {
		return false
	}

	insertPos := r.Intn(len(p.Calls) + 1)
	newCalls := []*Call{openCall, gdCall, closeCall}
	p.Calls = append(p.Calls[:insertPos], append(newCalls, p.Calls[insertPos:]...)...)
	updateBarrierPosTable(ps, pidx, insertPos, 3)
	return true
}

// genOpenDirCallWithPath builds open(path, O_DIRECTORY) and returns the call
// and its result fd.
func genOpenDirCallWithPath(p *Prog, r *randGen, sCalls *SpecialCalls, dirPath string) (*Call, *ResultArg) {
	meta := r.target.Syscalls[sCalls.OpenId]
	if meta == nil {
		return nil, nil
	}
	s := stateFromProg(p)
	ptrType := meta.Args[0].Type.(*PtrType)
	flagsType := meta.Args[1].Type.(*FlagsType)
	modeType := meta.Args[2].Type.(*FlagsType)
	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(dirPath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)
	c := MakeCall(meta, nil)
	c.Args = []Arg{pathPtr, MakeConstArg(flagsType, DirIn, r.target.GetConst("O_DIRECTORY")), MakeConstArg(modeType, DirIn, 0)}
	r.target.assignSizesCall(c)
	return c, c.Ret
}

func genCloseCallWithFd(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg) *Call {
	meta := r.target.Syscalls[sCalls.CloseId]
	if meta == nil {
		return nil
	}
	fdType := meta.Args[0].Type.(*ResourceType)
	c := MakeCall(meta, nil)
	c.Args = []Arg{MakeResultArg(fdType, DirIn, fd, 0)}
	r.target.assignSizesCall(c)
	return c
}

func isOpenDirectory(call *Call) bool {
	if len(call.Args) < 2 {
		return false
	}

	flagArg, ok := call.Args[1].(*ConstArg)
	if !ok {
		return false
	}

	const O_DIRECTORY = 0x10000
	return (flagArg.Val & O_DIRECTORY) != 0
}

func genGetdents64CallWithFd(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg) *Call {
	meta := r.target.Syscalls[sCalls.Getdents64Id]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	// Fixed page-size buffer and count: the count must exceed zero to
	// trigger a real directory read, and a large value avoids the checker's
	// dirent-size approximation at truncation boundaries (see #19).
	const bufSize = 4096

	bufPtrType := meta.Args[1].Type.(*PtrType)
	lenType := meta.Args[2].Type.(*LenType)

	bufArg := MakeOutDataArg(bufPtrType.Elem, DirOut, bufSize)
	bufPtr := r.allocAddr(s, bufPtrType, DirOut, bufArg.Size(), bufArg)

	args := make([]Arg, len(meta.Args))
	fdType := meta.Args[0].Type.(*ResourceType)
	args[0] = MakeResultArg(fdType, DirIn, fd, 0) // 类型 = fd_dir（参数类型）——Res = open.Ret——validate 通过（L14）
	args[1] = bufPtr
	args[2] = MakeConstArg(lenType, DirIn, bufSize)

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func removeDcacheCall(ps []*Prog, pidx int, p *Prog, r *randGen) bool {
	if len(p.Calls) <= 2 {
		return false
	}

	candidates := make([]int, 0)
	for i, call := range p.Calls {
		name := call.Meta.Name
		if strings.Contains(name, "mkdir") || strings.Contains(name, "rmdir") ||
			strings.Contains(name, "creat") || strings.Contains(name, "unlink") ||
			strings.Contains(name, "getdents64") || strings.Contains(name, "rename") {
			candidates = append(candidates, i)
		}
	}

	if len(candidates) == 0 {
		return false
	}

	idx := candidates[r.Intn(len(candidates))]
	p.RemoveCall(idx)
	updateBarrierPosTable(ps, pidx, idx, -1)
	return true
}

func swapDcacheCalls(p *Prog, r *randGen) bool {
	if len(p.Calls) < 3 {
		return false
	}

	candidates := make([]int, 0)
	for i, call := range p.Calls {
		// open/close/getdents64 must not be swapped: moving them would break
		// the fd chain (open.Ret referenced before produced — serialization
		// panic "no copyout index"——L8) or the self-contained
		// open+getdents64+close blocks.
		if strings.Contains(call.Meta.Name, "syz_failure") ||
			strings.Contains(call.Meta.Name, "open") || strings.Contains(call.Meta.Name, "close") ||
			strings.Contains(call.Meta.Name, "getdents64") {
			continue
		}
		candidates = append(candidates, i)
	}

	if len(candidates) < 2 {
		return false
	}

	idx1 := candidates[r.Intn(len(candidates))]
	idx2 := candidates[r.Intn(len(candidates))]
	for idx2 == idx1 {
		idx2 = candidates[r.Intn(len(candidates))]
	}

	p.Calls[idx1], p.Calls[idx2] = p.Calls[idx2], p.Calls[idx1]
	return true
}

func mutateDcachePathName(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	var dcacheP *Prog
	for _, p := range ps {
		if p.IsDCache {
			dcacheP = p
			break
		}
	}
	if dcacheP == nil {
		return false
	}

	paths := make(map[string]bool)
	for _, call := range dcacheP.Calls {
		p := extractPathFromCall(call)
		if p != "" {
			paths[p] = true
		}
	}
	if len(paths) == 0 {
		return false
	}

	var pathList []string
	for p := range paths {
		pathList = append(pathList, p)
	}
	oldPath := pathList[r.Intn(len(pathList))]

	needDir := false
	for _, call := range dcacheP.Calls {
		if extractPathFromCall(call) != oldPath {
			continue
		}
		if strings.Contains(call.Meta.Name, "mkdir") ||
			strings.Contains(call.Meta.Name, "rmdir") ||
			strings.Contains(call.Meta.Name, "getdents64") {
			needDir = true
			break
		}
	}

	var newPath string
	if needDir {
		newPath = extractDirFromTree(hmcfg, dcacheP, r)
	} else {
		newPath = extractFileFromTree(hmcfg, dcacheP, r)
		if newPath == "" {
			return false // 无文件目标——不替换为目录（语义错误，L9）
		}
	}
	if newPath == "" || newPath == oldPath {
		return false
	}

	for _, p := range ps {
		for i, call := range p.Calls {
			if extractPathFromCall(call) == oldPath {
				updateCallPath(p, i, call, newPath, r)
			}
			// rename 的目标路径也检查（L9）
			if strings.Contains(call.Meta.Name, "rename") {
				if extractPathFromCallByArgIdx(call, 1) == oldPath {
					updateCallPathByArgIdx(p, call, 1, newPath, r)
				}
			}
		}
	}
	return true
}

func mutateDcacheDelay(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for pidx, p := range ps {
		if !p.IsDCache {
			continue
		}

		if len(p.Calls) == 0 {
			continue
		}

		insertPos := r.Intn(len(p.Calls) + 1)

		delayCall := genNanosleepCall(p, r, sCalls, true, "dcache")
		if delayCall == nil {
			continue
		}

		if insertPos >= len(p.Calls) {
			p.Calls = append(p.Calls, delayCall)
		} else {
			p.Calls = append(p.Calls[:insertPos], append([]*Call{delayCall}, p.Calls[insertPos:]...)...)
		}
		updateBarrierPosTable(ps, pidx, insertPos, 1)
		return true
	}
	return false
}

func mutateDcacheFailPos(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for _, p := range ps {
		if !p.HasNetFail || len(p.GeneralFailPos) == 0 {
			continue
		}

		for _, failPos := range p.GeneralFailPos {
			if len(failPos) >= 4 {
				failStart := failPos[1]
				failEnd := failPos[3]

				if failStart <= 0 || failEnd <= failStart {
					continue
				}

				shift := r.Intn(3) - 1
				if shift == 0 {
					shift = 1
				}

				newFailStart := failStart + shift
				newFailEnd := failEnd + shift
				if newFailStart < 1 {
					newFailStart = 1
					newFailEnd = newFailStart + (failEnd - failStart)
				}

				if !moveFailCalls(p, failStart, newFailStart) {
					continue
				}
				failPos[1] = newFailStart
				failPos[3] = newFailEnd
				return true
			}
		}
	}
	return false
}

func mutateDcacheSyncPos(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for progIdx, p := range ps {
		if !p.IsDCache {
			continue
		}

		syncCalls := findCallsByName(p, "syz_failure_barrier")
		if len(syncCalls) == 0 {
			continue
		}

		type syncInfo struct {
			callIdx int
			syncId  int
		}
		var syncInfos []syncInfo
		for _, callIdx := range syncCalls {
			if callIdx >= len(p.Calls) {
				continue
			}
			syncId := extractSyncId(p.Calls[callIdx])
			if syncId >= 0 {
				syncInfos = append(syncInfos, syncInfo{callIdx: callIdx, syncId: syncId})
			}
		}

		if len(syncInfos) == 0 {
			continue
		}

		chosenIdx := r.Intn(len(syncInfos))
		chosen := syncInfos[chosenIdx]

		shift := r.Intn(3) - 1
		if shift == 0 {
			shift = 1
		}

		newCallIdx := chosen.callIdx + shift
		if newCallIdx < 1 || newCallIdx >= len(p.Calls)-1 {
			continue
		}
		/*if isSpecialCall(p.Calls[newCallIdx]) {
			continue
		}*/

		pairSyncId := -1
		if chosen.syncId%2 == 0 {
			pairSyncId = chosen.syncId + 1
		} else {
			pairSyncId = chosen.syncId - 1
		}

		var pairCallIdx int = -1
		for _, info := range syncInfos {
			if info.syncId == pairSyncId {
				pairCallIdx = info.callIdx
				break
			}
		}

		if pairCallIdx >= 0 {
			if chosen.syncId%2 == 0 && newCallIdx >= pairCallIdx {
				continue
			}
			if chosen.syncId%2 == 1 && newCallIdx <= pairCallIdx {
				continue
			}
		}

		call := p.Calls[chosen.callIdx]
		p.Calls = append(p.Calls[:chosen.callIdx], p.Calls[chosen.callIdx+1:]...)

		if shift < 0 {
			p.Calls = append(p.Calls[:newCallIdx], append([]*Call{call}, p.Calls[newCallIdx:]...)...)
		} else {
			p.Calls = append(p.Calls[:newCallIdx], append([]*Call{call}, p.Calls[newCallIdx:]...)...)
		}

		if len(ps) > 0 && len(ps[0].GeneralFailPos) > 0 {
			updateGeneralFailPos(ps[0], progIdx, chosen.syncId, newCallIdx)
		}

		if len(ps) > 0 && len(ps[0].GeneralBarrierPos) > 0 && progIdx < len(ps[0].GeneralBarrierPos[0]) {
			ps[0].GeneralBarrierPos[0][progIdx] = newCallIdx
		}

		return true
	}
	return false
}

// updateBarrierPosTable maintains the GeneralBarrierPos real-time state
// record: when a call is inserted/removed at shiftPos (<= barrier position),
// the recorded barrier index shifts by delta.
func updateBarrierPosTable(ps []*Prog, pidx, shiftPos, delta int) {
	if len(ps) == 0 || len(ps[0].GeneralBarrierPos) == 0 ||
		pidx >= len(ps[0].GeneralBarrierPos[0]) {
		return
	}
	b := ps[0].GeneralBarrierPos[0][pidx]
	if shiftPos <= b {
		ps[0].GeneralBarrierPos[0][pidx] = b + delta
	}
}

func extractDirFromTree(hmcfg *Hmdfs_config, p *Prog, r *randGen) string {
	if hmcfg.FileTree != nil && r.bin() {
		node := hmcfg.FileTree.GetRandomDirExcluding(r.Rand, "")
		if node != nil {
			return node.FullPath
		}
	}
	dir := extractDirPath(p)
	if dir == "" {
		dir = defaultTestDir(hmcfg)
	}
	return dir
}

func extractFileFromTree(hmcfg *Hmdfs_config, p *Prog, r *randGen) string {
	if hmcfg.FileTree != nil && r.bin() {
		node := hmcfg.FileTree.GetRandomFileExcluding(r.Rand, "")
		if node != nil {
			return node.FullPath
		}
	}
	return ""
}

func genMkdirCall(p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) *Call {
	meta := r.target.Syscalls[sCalls.MkdirId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	parentDir := extractDirFromTree(hmcfg, p, r)
	newDir := joinTestPath(parentDir, "mut_dir_"+randomSuffix(r.Rand))

	return genPathCall(meta, newDir, r, s)
}

func genRmdirCall(p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) *Call {
	meta := r.target.Syscalls[sCalls.RmdirId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	var targetDir string
	if hmcfg.FileTree != nil && r.bin() {
		node := hmcfg.FileTree.GetRandomDirExcluding(r.Rand, "")
		if node != nil {
			targetDir = node.FullPath
		}
	}
	if targetDir == "" {
		testDir := extractDirPath(p)
		if testDir == "" {
			testDir = defaultTestDir(hmcfg)
		}
		targetDir = joinTestPath(testDir, "rmdir_"+randomSuffix(r.Rand))
	}

	return genPathCall(meta, targetDir, r, s)
}

func genCreatCall(p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) *Call {
	parentDir := extractDirFromTree(hmcfg, p, r)
	fileName := joinTestPath(parentDir, "creat_file_"+randomSuffix(r.Rand)+".txt")
	return genCreatCallWithPath(p, r, sCalls, fileName)
}

func genCreatCallWithPath(p *Prog, r *randGen, sCalls *SpecialCalls, filePath string) *Call {
	meta := r.target.Syscalls[sCalls.CreatId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	ptrType := meta.Args[0].Type.(*PtrType)
	modeType := meta.Args[1].Type.(*FlagsType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)
	modeArg := MakeConstArg(modeType, DirIn, 0o666)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	args[1] = modeArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func genUnlinkCall(p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) *Call {
	var targetFile string
	if hmcfg.FileTree != nil && r.bin() {
		node := hmcfg.FileTree.GetRandomFileExcluding(r.Rand, "")
		if node != nil {
			targetFile = node.FullPath
		}
	}
	if targetFile == "" {
		testDir := extractDirPath(p)
		if testDir == "" {
			testDir = defaultTestDir(hmcfg)
		}
		targetFile = joinTestPath(testDir, "unlink_file_"+randomSuffix(r.Rand)+".txt")
	}
	return genUnlinkCallWithPath(p, r, sCalls, targetFile)
}

func genUnlinkCallWithPath(p *Prog, r *randGen, sCalls *SpecialCalls, filePath string) *Call {
	meta := r.target.Syscalls[sCalls.UnlinkId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)
	return genPathCall(meta, filePath, r, s)
}

func genRenameCall(p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) *Call {
	meta := r.target.Syscalls[sCalls.RenameId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	var oldPath string
	if hmcfg.FileTree != nil && r.bin() {
		node := hmcfg.FileTree.GetRandomFileExcluding(r.Rand, "")
		if node != nil {
			oldPath = node.FullPath
		}
	}
	if oldPath == "" {
		testDir := extractDirPath(p)
		if testDir == "" {
			testDir = defaultTestDir(hmcfg)
		}
		oldPath = joinTestPath(testDir, "old_"+randomSuffix(r.Rand)+".txt")
	}

	newPath := oldPath[:strings.LastIndex(oldPath, "/")+1] + "new_" + randomSuffix(r.Rand) + ".txt"

	oldPtrType := meta.Args[0].Type.(*PtrType)
	newPtrType := meta.Args[1].Type.(*PtrType)

	oldPathArg := MakeDataArg(oldPtrType.Elem, DirIn, []byte(oldPath+"\x00"))
	oldPathPtr := r.allocAddr(s, oldPtrType, DirIn, oldPathArg.Size(), oldPathArg)

	newPathArg := MakeDataArg(newPtrType.Elem, DirIn, []byte(newPath+"\x00"))
	newPathPtr := r.allocAddr(s, newPtrType, DirIn, newPathArg.Size(), newPathArg)

	args := make([]Arg, len(meta.Args))
	args[0] = oldPathPtr
	args[1] = newPathPtr

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

type HmdfsTimeoutSpec struct {
	Name     string
	Sec      int64
	Nsec     int64
	Category string
}

var HmdfsTimeoutTable = []HmdfsTimeoutSpec{
	{Name: "dcache_timeout", Sec: 30, Nsec: 0, Category: "dcache"},
	{Name: "dcache_lifetime", Sec: 30, Nsec: 0, Category: "dcache"},
	{Name: "write_cache_timeout", Sec: 30, Nsec: 0, Category: "writeback"},
	{Name: "writeback_interval", Sec: 5, Nsec: 0, Category: "writeback"},
	{Name: "writeback_timelimit_def", Sec: 5, Nsec: 0, Category: "writeback"},
	{Name: "writeback_timelimit_max", Sec: 30, Nsec: 0, Category: "writeback"},
	{Name: "wb_timeout_def", Sec: 60, Nsec: 0, Category: "writeback"},
	{Name: "wb_timeout_max", Sec: 900, Nsec: 0, Category: "writeback"},
	{Name: "sync_wpage_retry", Sec: 2, Nsec: 0, Category: "writeback"},
	{Name: "tcp_recv_timeout", Sec: 2, Nsec: 0, Category: "connection"},
	{Name: "conn_release_wait", Sec: 3, Nsec: 0, Category: "connection"},
	{Name: "node_evt_cb_delay", Sec: 2, Nsec: 0, Category: "connection"},
	{Name: "share_item_timeout", Sec: 120, Nsec: 0, Category: "share"},
	{Name: "rekey_lifetime", Sec: 3600, Nsec: 0, Category: "connection"},
	{Name: "bandwidth_interval", Sec: 0, Nsec: 200000000, Category: "writeback"},
	{Name: "acquire_wfired_min", Sec: 0, Nsec: 10000, Category: "connection"},
	{Name: "acquire_wfired_max", Sec: 0, Nsec: 30000, Category: "connection"},
	{Name: "request_end_wait_min", Sec: 0, Nsec: 20000, Category: "connection"},
	{Name: "request_end_wait_max", Sec: 0, Nsec: 30000, Category: "connection"},
}

var HmdfsTimeoutBoundaryTable = []HmdfsTimeoutSpec{
	{Name: "just_before_dcache_timeout", Sec: 29, Nsec: 500000000, Category: "dcache"},
	{Name: "just_after_dcache_timeout", Sec: 30, Nsec: 500000000, Category: "dcache"},
	{Name: "just_before_wb_timelimit", Sec: 4, Nsec: 900000000, Category: "writeback"},
	{Name: "just_after_wb_timelimit", Sec: 5, Nsec: 100000000, Category: "writeback"},
	{Name: "just_before_wb_timeout", Sec: 59, Nsec: 500000000, Category: "writeback"},
	{Name: "just_after_wb_timeout", Sec: 60, Nsec: 500000000, Category: "writeback"},
	{Name: "just_before_write_cache_timeout", Sec: 29, Nsec: 500000000, Category: "writeback"},
	{Name: "just_after_write_cache_timeout", Sec: 30, Nsec: 500000000, Category: "writeback"},
	{Name: "just_before_tcp_recv_timeout", Sec: 1, Nsec: 900000000, Category: "connection"},
	{Name: "just_after_tcp_recv_timeout", Sec: 2, Nsec: 100000000, Category: "connection"},
	{Name: "just_before_sync_retry", Sec: 1, Nsec: 900000000, Category: "writeback"},
	{Name: "just_after_sync_retry", Sec: 2, Nsec: 100000000, Category: "writeback"},
	{Name: "just_before_conn_release", Sec: 2, Nsec: 900000000, Category: "connection"},
	{Name: "just_after_conn_release", Sec: 3, Nsec: 100000000, Category: "connection"},
}

func genNanosleepCall(p *Prog, r *randGen, sCalls *SpecialCalls, useHmdfsTimeout bool, category string) *Call {
	meta := r.target.Syscalls[sCalls.NanosleepId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	var sec int64
	var nsec int64

	if useHmdfsTimeout {
		sec, nsec = pickHmdfsTimeout(r, category)
	} else {
		sec = int64(r.Intn(5))
		nsec = int64(r.Intn(1000000000))
	}

	args, _ := r.generateArgs(s, meta.Args, DirIn)

	if len(args) >= 1 {
		if ptrArg, ok := args[0].(*PointerArg); ok {
			if structArg, ok := ptrArg.Res.(*GroupArg); ok {
				if len(structArg.Inner) >= 2 {
					// timespec 的 time_sec/time_nsec 是 ResourceType——
					// generateTimespec 产出 *ResultArg（Res=nil，Val 为值）。
					// 断言 *ConstArg 恒失败导致超时值写不进（S13）。
					if tvSec, ok := structArg.Inner[0].(*ResultArg); ok {
						tvSec.Val = uint64(sec)
					}
					if tvNsec, ok := structArg.Inner[1].(*ResultArg); ok {
						tvNsec.Val = uint64(nsec)
					}
				}
			}
		}
	}

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func pickHmdfsTimeout(r *randGen, category string) (int64, int64) {
	candidates := make([]HmdfsTimeoutSpec, 0)

	if category != "" {
		for _, spec := range HmdfsTimeoutTable {
			if spec.Category == category {
				candidates = append(candidates, spec)
			}
		}
		for _, spec := range HmdfsTimeoutBoundaryTable {
			if spec.Category == category {
				candidates = append(candidates, spec)
			}
		}
	}

	if len(candidates) == 0 {
		candidates = append(candidates, HmdfsTimeoutTable...)
		candidates = append(candidates, HmdfsTimeoutBoundaryTable...)
	}

	chosen := candidates[r.Intn(len(candidates))]

	jitterNsec := int64(r.Intn(200000001)) - 100000000
	if jitterNsec < 0 {
		jitterNsec = -jitterNsec
		if chosen.Nsec >= jitterNsec {
			return chosen.Sec, chosen.Nsec - jitterNsec
		}
		if chosen.Sec > 0 {
			return chosen.Sec - 1, 1000000000 - (jitterNsec - chosen.Nsec)
		}
		return 0, 0
	}
	newNsec := chosen.Nsec + jitterNsec
	if newNsec >= 1000000000 {
		return chosen.Sec + 1, newNsec - 1000000000
	}
	return chosen.Sec, newNsec
}

func genPathCall(meta *Syscall, path string, r *randGen, s *state) *Call {
	ptrType := meta.Args[0].Type.(*PtrType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(path+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr

	if len(meta.Args) > 1 {
		if modeType, ok := meta.Args[1].Type.(*FlagsType); ok {
			args[1] = MakeConstArg(modeType, DirIn, 0o755)
		}
	}

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func extractPathFromCall(call *Call) string {
	if len(call.Args) == 0 {
		return ""
	}

	ptrArg, ok := call.Args[0].(*PointerArg)
	if !ok {
		return ""
	}

	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return ""
	}

	path := string(dataArg.Data())
	path = strings.TrimSuffix(path, "\x00")
	return path
}

func updateCallPath(p *Prog, callIdx int, call *Call, newPath string, r *randGen) bool {
	if !reassignDataArgAt(p, call, 0, r, []byte(newPath+"\x00")) {
		return false
	}
	r.target.assignSizesCall(call)
	return true
}

func extractDirPath(p *Prog) string {
	for _, call := range p.Calls {
		path := extractPathFromCall(call)
		if path == "" {
			continue
		}

		name := call.Meta.Name
		if strings.Contains(name, "mkdir") || strings.Contains(name, "rmdir") {
			return path
		}

		if strings.Contains(name, "creat") || strings.Contains(name, "unlink") || strings.Contains(name, "open") {
			idx := strings.LastIndex(path, "/")
			if idx > 0 {
				return path[:idx]
			}
		}
	}
	return ""
}

func extractPathFromCallByArgIdx(call *Call, argIdx int) string {
	if len(call.Args) <= argIdx {
		return ""
	}

	ptrArg, ok := call.Args[argIdx].(*PointerArg)
	if !ok {
		return ""
	}

	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return ""
	}

	path := string(dataArg.Data())
	path = strings.TrimSuffix(path, "\x00")
	return path
}

func updateCallPathByArgIdx(p *Prog, call *Call, argIdx int, newPath string, r *randGen) bool {
	if !reassignDataArgAt(p, call, argIdx, r, []byte(newPath+"\x00")) {
		return false
	}
	r.target.assignSizesCall(call)
	return true
}
