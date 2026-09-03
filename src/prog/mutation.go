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
	srvNum int, hasFail bool, enableC2san bool, hmcfg *Hmdfs_config, idx int) {
	recordLastMutation("standardMutate")
	r := newRand(p.Target, rs)
	r.hmcfg = hmcfg
	r.curIdx = idx
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

	log.Logf(0, "mutate testcase with failures\n")

	for stop, ok := false, false; !stop; stop = ok && len(p.Calls) != 0 && r.oneOf(3) {
		switch {
		case r.oneOf(5):
			//log.Logf(0, "----- squashAny()")
			// Not all calls have anything squashable,
			// so this has lower priority in reality.
			ok = ctx.squashAny()
		case r.nOutOf(1, 100):
			log.Logf(0, "----- splice()")
			if hasFail {
				ok = false
			} else {
				ok = ctx.splice()
			}
		case r.nOutOf(20, 31):
			log.Logf(0, "----- insertCall()")
			ok = ctx.insertCall()
		case r.nOutOf(10, 11):
			log.Logf(0, "----- mutateArg()")
			ok = ctx.mutateArg()
		case r.nOutOf(9, 10):
			if hasFail {
				log.Logf(0, "----- mutateFailPos()")
				ok = ctx.mutateFailPos()
			} else {
				ok = false
			}
		default:
			log.Logf(0, "----- removeCall()")
			ok = ctx.removeCall()
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
	for len(p.Calls) > ctx.ncalls {
		p.RemoveCall(idx)
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
		p.insertBefore(c, calls)
		idx += len(calls)
		for len(p.Calls) > ctx.ncalls {
			idx--
			p.RemoveCall(idx)
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
			return mutateWriteOffset(ps, call, r)
		case 1:
			return mutateWriteLength(ps, call, r)
		case 2:
			return mutateWriteData(ps, call, r)
		}
	}
	return false
}

// syncStashReadVerification keeps the read-verification calls in sync with a
// mutated write: every pread64 that reads the old (offset, length) region is
// moved to the new region (count, out-buffer size and offset), so the
// write-fault-read loop stays closed. Reads of other regions are untouched.
func syncStashReadVerification(ps []*Prog, oldOff, oldLen, newOff, newLen uint64) {
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
func reassignDataArg(p *Prog, call *Call, r *randGen, newData []byte) bool {
	if len(call.Args) < 2 {
		return false
	}
	ptrArg, ok := call.Args[1].(*PointerArg)
	if !ok {
		return false
	}
	ptrType, ok := ptrArg.Type().(*PtrType)
	if !ok {
		return false
	}
	s := stateFromProg(p)
	newDataArg := MakeDataArg(ptrType.Elem, DirIn, newData)
	newPtr := r.allocAddr(s, ptrType, DirIn, newDataArg.Size(), newDataArg)
	ptrArg.Res = newPtr
	return true
}

func mutateWriteOffset(ps []*Prog, call *Call, r *randGen) bool {
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

	syncStashReadVerification(ps, oldOff, oldLen, posArg.Val, oldLen)
	return true
}

func mutateWriteLength(ps []*Prog, call *Call, r *randGen) bool {
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
	countArg.Val = uint64(newLen)

	if len(call.Args) >= 2 {
		if ptrArg, ok := call.Args[1].(*PointerArg); ok {
			if dataArg, ok := ptrArg.Res.(*DataArg); ok {
				oldData := dataArg.Data()
				newData := make([]byte, newLen)
				copy(newData, oldData)
				for i := len(oldData); i < int(newLen); i++ {
					newData[i] = byte(r.Intn(256))
				}
				dataArg.data = newData
				reassignDataArg(ps[0], call, r, newData) // 重分配——新地址新尺寸（L7）
			}
		}
	}

	syncStashReadVerification(ps, oldOff, oldLen, oldOff, countArg.Val)
	return true
}

func mutateWriteData(ps []*Prog, call *Call, r *randGen) bool {
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
	dataArg.data = data
	countArg.Val = uint64(len(data))
	reassignDataArg(ps[0], call, r, data) // 重分配——新地址新尺寸（L7）

	syncStashReadVerification(ps, oldOff, oldLen, oldOff, countArg.Val) // 读验证同步（L7）
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

func mutateStashOpSequence(ps []*Prog, r *randGen, sCalls *SpecialCalls, ct *ChoiceTable) bool {
	for _, p := range ps {
		if !p.IsStash {
			continue
		}

		mutType := r.Intn(2)
		switch mutType {
		case 0:
			return insertStashCall(ps, p, r, sCalls, ct)
		case 1:
			return removeStashCall(p, r)
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
		closemeta := r.target.Syscalls[sCalls.CloseId]
		closefdtype := closemeta.Args[0].Type.(*ResourceType)
		fdarg := MakeResultArg(closefdtype, DirIn, fd, 0)
		correspondingClose := -1
		for _, closeIdx := range closeCalls {
			if closeIdx > openIdx {
				closeCall := p.Calls[closeIdx]
				if usesFd(closeCall, fdarg) {
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
	// 否则死锁）；窗口前插入时同步更新表项
	failStart, failEnd := -1, -1
	if len(p.GeneralFailPos) > 0 && len(p.GeneralFailPos[0]) >= 4 {
		failStart = p.GeneralFailPos[0][1]
		failEnd = p.GeneralFailPos[0][3]
	}

	type interval struct{ start, end int }
	var slots []interval
	if failStart > validStart && failStart < validEnd {
		slots = append(slots, interval{validStart, failStart})
	}
	if failEnd+1 >= validStart && failEnd+1 < validEnd {
		slots = append(slots, interval{failEnd + 1, validEnd})
	}
	if len(slots) == 0 {
		slots = append(slots, interval{validStart, validEnd})
	}

	slot := slots[r.Intn(len(slots))]
	insertPos := slot.start + r.Intn(slot.end-slot.start)

	p.Calls = append(p.Calls[:insertPos], append([]*Call{newCall}, p.Calls[insertPos:]...)...)

	if failStart >= 0 && insertPos <= failStart {
		p.GeneralFailPos[0][1]++
		p.GeneralFailPos[0][3]++
	}

	// A freshly inserted write has no read verification: add a matching
	// pread64 to another stash read prog to keep the write-fault-read loop
	// closed (see DAG_KNOWN_ISSUES.md #19).
	if callType == "pwrite64" && len(newCall.Args) >= 4 {
		if offArg, ok := newCall.Args[3].(*ConstArg); ok {
			if lenArg, ok := newCall.Args[2].(*ConstArg); ok {
				addStashReadVerification(ps, p, r, sCalls, offArg.Val, lenArg.Val)
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

// addStashReadVerification inserts a pread64 reading the (offset, length)
// region of a freshly inserted write into another stash read prog, so the
// write is actually verified.
func addStashReadVerification(ps []*Prog, writer *Prog, r *randGen, sCalls *SpecialCalls, offset, length uint64) {
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

	// The reader's open on the same file as the writer's (multi-file stash
	// seeds have one open per file; mismatched fds would silently void the
	// verification). Falls back to any open fd when no path matches.
	writerPath := ""
	for _, idx := range findCallsByName(writer, "open") {
		if idx < len(writer.Calls) {
			if p := extractPathFromCall(writer.Calls[idx]); p != "" {
				writerPath = p
				break
			}
		}
	}
	var fd *ResultArg
	for _, idx := range findCallsByName(reader, "open") {
		if idx >= len(reader.Calls) {
			continue
		}
		if writerPath != "" && extractPathFromCall(reader.Calls[idx]) != writerPath {
			continue
		}
		if f := extractFdFromCall(reader.Calls[idx]); f != nil {
			fd = f
			break
		}
	}
	if fd == nil {
		for _, idx := range findCallsByName(reader, "open") {
			if idx >= len(reader.Calls) {
				continue
			}
			if f := extractFdFromCall(reader.Calls[idx]); f != nil {
				fd = f
				break
			}
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
	args[0] = fd
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
	args[0] = fd
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
	if p := extractPathFromCall(call); p != "" {
		return p
	}
	return getFdFilePathForCall(ps, pos)
}

// pickAnchor selects a random call in ps[0] that has a resolvable path and
// execution timing; wantReadWrite restricts the anchor to read/write calls
// (required by the data mutation, whose whole point is sharing offsets).
func pickAnchor(ps []*Prog, r *randGen, wantReadWrite bool) (GroupPosition, string, bool) {
	if len(ps) == 0 || len(ps[0].Calls) == 0 {
		return GroupPosition{}, "", false
	}
	for attempt := 0; attempt < 8; attempt++ {
		idx := r.Intn(len(ps[0].Calls))
		call := ps[0].Calls[idx]
		if wantReadWrite && !isWriteCall(call) && !isReadCall(call) {
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

// MutateGroupPathDynamic migrates the dynamic concurrent set of a random
// anchor call to a new base path, keeping the relative path relations between
// the anchor and its concurrent peers ("same pattern, new location"). Calls
// that did not overlap the anchor (the non-concurrent backbone) stay in place.
func MutateGroupPathDynamic(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	recordLastMutation("MutateGroupPathDynamic")
	if lcs == nil || lcs.FileTree == nil {
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

	seedType := "fileops"
	if isDirPath(lcs.FileTree, anchorPath) {
		seedType = "inodeops"
	}
	newBasePath := pickNewBasePath(lcs.FileTree, seedType, r.Rand, "")
	if newBasePath == "" {
		return false
	}

	type pathUpdate struct {
		pos     GroupPosition
		newPath string
	}
	updates := []pathUpdate{{pos: resolveFdTarget(ps, anchor), newPath: newBasePath}}
	for _, cc := range conc {
		newPath := lcs.FileTree.GetPathByRelation(newBasePath, "", cc.Rel, r.Rand, "", false)
		if newPath == "" {
			newPath = newBasePath
		}
		updates = append(updates, pathUpdate{pos: resolveFdTarget(ps, cc.Pos), newPath: newPath})
	}
	for _, u := range updates {
		updateCallPathInProg(ps[u.pos.ProgIdx], u.pos.CallIdx, u.newPath, r)
	}
	return true
}

// RemoveGroupDynamic deletes the anchor and its whole concurrent set.
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

	byProg := make(map[int][]int)
	for _, cc := range conc {
		byProg[cc.Pos.ProgIdx] = append(byProg[cc.Pos.ProgIdx], cc.Pos.CallIdx)
	}
	// Delete from the end of each prog so earlier indices stay valid.
	for progIdx, callIdxs := range byProg {
		sort.Ints(callIdxs)
		for i := len(callIdxs) - 1; i >= 0; i-- {
			ps[progIdx].RemoveCall(callIdxs[i])
		}
	}
	ps[anchor.ProgIdx].RemoveCall(anchor.CallIdx)
	return true
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

// RemoveOneInGroupDynamic removes one fd-safe call from the anchor's
// concurrent set.
func RemoveOneInGroupDynamic(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	recordLastMutation("RemoveOneInGroupDynamic")
	if lcs == nil {
		return false
	}
	anchor, anchorPath, ok := pickAnchor(ps, r, false)
	if !ok {
		return false
	}
	var candidates []GroupPosition
	for _, cc := range findGroupCalls(ps, anchor, anchorPath, lcs.tscoffs) {
		if callRemovable(ps, cc.Pos) {
			candidates = append(candidates, cc.Pos)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	pos := candidates[r.Intn(len(candidates))]
	ps[pos.ProgIdx].RemoveCall(pos.CallIdx)
	return true
}

// MutateGroupDataDynamic shares a random offset (and length for writes)
// across all read/write calls of the anchor's dynamic concurrent set, the
// deterministic counterpart of the probabilistic OffsetSame insertions. The
// anchor itself must be a read/write call, so the set always contains
// something to mutate.
func MutateGroupDataDynamic(ps []*Prog, lcs *LayeredChoiceStrategy, r *randGen) bool {
	recordLastMutation("MutateGroupDataDynamic")
	if lcs == nil {
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

	sharedOffset := uint64(0)
	hasOffset := false
	sharedLength := uint64(0)
	hasLength := false
	for _, pos := range positions {
		call := ps[pos.ProgIdx].Calls[pos.CallIdx]
		if !isWriteCall(call) && !isReadCall(call) {
			continue
		}
		if !hasOffset && len(call.Args) >= 4 {
			if lcs.FileTree != nil {
				if sz := getFileSizeByPath(ps, pos, lcs.FileTree); sz > 0 {
					sharedOffset = r.randRange(0, sz+4096)
				}
			}
			if sharedOffset == 0 {
				sharedOffset = r.randRange(0, 1024*1024)
			}
			hasOffset = true
		}
		if !hasLength && isWriteCall(call) && len(call.Args) >= 3 {
			sharedLength = pickMutationLength(r)
			hasLength = true
		}
	}
	if !hasOffset && !hasLength {
		return false
	}

	for _, pos := range positions {
		call := ps[pos.ProgIdx].Calls[pos.CallIdx]
		if !isWriteCall(call) && !isReadCall(call) {
			continue
		}
		if len(call.Args) >= 4 {
			if posArg, ok := call.Args[3].(*ConstArg); ok {
				posArg.Val = sharedOffset
			}
		}
		if isWriteCall(call) && len(call.Args) >= 3 {
			if countArg, ok := call.Args[2].(*ConstArg); ok {
				countArg.Val = sharedLength
				updateWriteDataBuf(ps[pos.ProgIdx], call, sharedLength, r)
			}
		}
		r.target.assignSizesCall(call)
	}
	return true
}

// mutateMixForSize returns the mutation mix weights for the inodeops/fileops
// DCT mutators, banded by len(ps[0].Calls): tiny programs grow (insertion
// heavy — they need to reach a size where modifier pairs exist at all),
// large ones shrink (removal heavy — bounded execution cost, bounded pair
// space). Weights are (insertion, remove-one, remove-group, path/data
// mutation) out of 100:
//
//	1-3   (tiny):   85/ 5/ 0/10 — gather the first modifier pairs
//	4-10  (grow):   60/10/ 5/25
//	11-15 (peak):   35/25/10/30
//	16-20 (shrink): 10/40/20/30
func mutateMixForSize(size int) (ins, removeOne, removeGroup, mutate int) {
	switch {
	case size < 4:
		return 85, 5, 0, 10
	case size < 11:
		return 60, 10, 5, 25
	case size < 16:
		return 35, 25, 10, 30
	default:
		return 10, 40, 20, 30
	}
}

func MutateInodeOpsWithDCT(ps []*Prog, rs rand.Source, ct *ChoiceTable, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy) bool {
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
			// At the size cap: fall back to removing a call instead of
			// wasting the round, so programs oscillate around the cap.
			return RemoveOneInGroupDynamic(ps, lcs, r)
		}
		if lcs.ShouldUsePattern(r.Rand) {
			return insertCallFromPattern(ps, r, sCalls, hmcfg, lcs, "inodeops")
		}
		return insertCallFromDCT(ps, r, ct, sCalls, hmcfg, lcs, "inodeops")
	case roll < ins+removeOne:
		return RemoveOneInGroupDynamic(ps, lcs, r)
	case roll < ins+removeOne+removeGroup:
		return RemoveGroupDynamic(ps, lcs, r)
	default:
		if r.bin() {
			return MutateGroupPathDynamic(ps, lcs, r)
		}
		return MutateGroupDataDynamic(ps, lcs, r)
	}
}

func MutateFileopsWithDCT(ps []*Prog, rs rand.Source, ct *ChoiceTable, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy) bool {
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
			// At the size cap: fall back to removing a call instead of
			// wasting the round, so programs oscillate around the cap.
			return RemoveOneInGroupDynamic(ps, lcs, r)
		}
		if lcs.ShouldUsePattern(r.Rand) {
			return insertCallFromPattern(ps, r, sCalls, hmcfg, lcs, "fileops")
		}
		return insertCallFromDCT(ps, r, ct, sCalls, hmcfg, lcs, "fileops")
	case roll < ins+removeOne:
		return RemoveOneInGroupDynamic(ps, lcs, r)
	case roll < ins+removeOne+removeGroup:
		return RemoveGroupDynamic(ps, lcs, r)
	default:
		if r.bin() {
			return MutateGroupPathDynamic(ps, lcs, r)
		}
		return MutateGroupDataDynamic(ps, lcs, r)
	}
}

func insertCallFromPattern(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, seedType string) bool {
	recordLastMutation("insertCallFromPattern:" + seedType)
	pattern := lcs.PredefinedPatterns.GetRandomPattern(seedType, r.Rand)
	if pattern == nil || pattern.ClientCount > len(ps) {
		return false
	}

	var basePath string
	p0 := ps[0]
	var pathCandidates []string
	for _, call := range p0.Calls {
		if call.Meta.Name == "rename" {
			p1, p2 := extractRenamePaths(call)
			if p1 != "" {
				pathCandidates = append(pathCandidates, p1)
			}
			if p2 != "" {
				pathCandidates = append(pathCandidates, p2)
			}
		} else {
			p := extractPathFromCall(call)
			if p != "" {
				pathCandidates = append(pathCandidates, p)
			}
		}
	}
	if len(pathCandidates) > 0 {
		basePath = pathCandidates[r.Intn(len(pathCandidates))]
	}

	if basePath == "" {
		//TODO: 目前是种子本身有操作文件就使用已操作文件，否则就选一个新的
		if seedType == "fileops" {
			fileNode := lcs.FileTree.GetRandomFile(r.Rand, hmcfg.Cids[0])
			if fileNode == nil {
				return false
			}
			basePath = fileNode.FullPath
		} else {
			dirNode := lcs.FileTree.GetRandomDir(r.Rand, hmcfg.Cids[0], true)
			//TODO: 对于 inodeops 其实不一定要是 dir 吧，只不过有一些操作只能对 dir 操作
			if dirNode == nil {
				return false
			}
			basePath = dirNode.FullPath
		}
	}

	sharedOffset := uint64(0)
	if pattern.OffsetRel == OffsetSame {
		node := lcs.FileTree.FindNode(basePath)
		if node != nil && node.Size > 0 && r.nOutOf(4, 5) {
			sharedOffset = r.randRange(0, node.Size+4096)
		} else {
			sharedOffset = r.randInt(64)
		}
	}

	referenceInsertPos := -1
	refTime := int64(-1) // 参考时间（全局 TSC 域），-1 表示尚无参考

	for nodeIdx, ops := range pattern.Operations {
		if nodeIdx >= len(ps) {
			break
		}

		p := ps[nodeIdx]
		cid := hmcfg.Cids[nodeIdx]

		var insertPos int
		var ExistingFd *ResultArg
		var useExistingFd bool
		first := referenceInsertPos < 0
		if seedType == "fileops" {
			insertPos, ExistingFd = findInsertPosition(p, r, 1, sCalls, refTime, lcs.tscoffFor(nodeIdx))
			useExistingFd = true
		} else {
			insertPos, ExistingFd = findInsertPosition(p, r, 2, sCalls, refTime, lcs.tscoffFor(nodeIdx))
			useExistingFd = true
		}
		if ExistingFd == nil {
			insertPos, _ = findInsertPosition(p, r, 0, sCalls, refTime, lcs.tscoffFor(nodeIdx))
			useExistingFd = false
		}
		if first {
			referenceInsertPos = insertPos
			// Reference time for the other progs: 0 = program-start
			// alignment (root inserted at the beginning); the end of the
			// call before insertPos when it has timing info; -1 (no
			// reference) otherwise, so others fall back to index alignment.
			refTime = int64(-1)
			if insertPos == 0 {
				refTime = 0
			} else if p.Calls[insertPos-1].CheckInfo != nil {
				refTime = int64(p.Calls[insertPos-1].CheckInfo.Etime) - lcs.tscoffFor(nodeIdx)
			}
		}
		for _, op := range ops {
			calls, _, _ := r.generateCallFromPatternOp(stateFromProg(p), sCalls, op, basePath, cid, lcs, ExistingFd, useExistingFd, pattern.OffsetRel, sharedOffset)
			for _, c := range calls {
				if insertPos >= len(p.Calls) {
					p.Calls = append(p.Calls, c)
				} else {
					p.Calls = append(p.Calls[:insertPos], append([]*Call{c}, p.Calls[insertPos:]...)...)
				}
				insertPos++
			}
		}
	}

	// Fill remaining nodes with verification calls at the time-aligned position
	for nodeIdx := len(pattern.Operations); nodeIdx < len(ps); nodeIdx++ {
		p := ps[nodeIdx]
		cid := hmcfg.Cids[nodeIdx]
		verInsertPos := TimeAlignedInsertPos(p, refTime, lcs.tscoffFor(nodeIdx))
		if verInsertPos < 0 {
			verInsertPos = referenceInsertPos
		}
		if verInsertPos < 0 || verInsertPos > len(p.Calls) {
			verInsertPos = len(p.Calls)
		}
		flags := uint64(2)
		if lcs.FileTree != nil {
			node := lcs.FileTree.FindNode(basePath)
			if node != nil && (node.Type == NodeTypeDir || node.Type == NodeTypeEmptyDir) {
				flags = r.target.GetConst("O_DIRECTORY")
			}
		}
		calls := r.generateVerificationCalls(stateFromProg(p), sCalls, basePath, cid, seedType, nil, flags)
		for _, c := range calls {
			if verInsertPos >= len(p.Calls) {
				p.Calls = append(p.Calls, c)
			} else {
				p.Calls = append(p.Calls[:verInsertPos], append([]*Call{c}, p.Calls[verInsertPos:]...)...)
			}
			verInsertPos++
		}
	}

	//TODO: 没有记录插入索引

	return true
}

type GroupPosition struct {
	ProgIdx int
	CallIdx int
}

func resolveFdTarget(ps []*Prog, pos GroupPosition) GroupPosition {
	call := ps[pos.ProgIdx].Calls[pos.CallIdx]
	callName := call.Meta.CallName

	if callName == "open" || callName == "creat" ||
		callName == "truncate" || callName == "chmod" ||
		callName == "mkdir" || callName == "rmdir" ||
		callName == "rename" || callName == "stat" ||
		callName == "unlink" || callName == "getdents64" {
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

func pickNewBasePath(ft *FileTree, seedType string, r *rand.Rand, excludeCid string) string {
	var candidates []*FileNode
	if seedType == "fileops" {
		candidates = ft.GetAllFileNodesExcluding(excludeCid)
		if len(candidates) == 0 {
			node := ft.GetRandomFile(r, "")
			if node != nil {
				candidates = append(candidates, node)
			}
		}
	} else {
		candidates = ft.GetAllNonTmpDirNodesExcluding(excludeCid)
		if len(candidates) == 0 {
			node := ft.GetRandomDir(r, "", true)
			if node != nil {
				candidates = append(candidates, node)
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	totalWeight := 0.0
	weights := make([]float64, len(candidates))
	for i, node := range candidates {
		w := calculateNodeWeight(node)
		weights[i] = w
		totalWeight += w
	}

	x := r.Float64() * totalWeight
	cumSum := 0.0
	for i, w := range weights {
		cumSum += w
		if x < cumSum {
			return candidates[i].FullPath
		}
	}
	return candidates[len(candidates)-1].FullPath
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

func updateCallPathInProg(p *Prog, callIdx int, newPath string, r *randGen) {
	if callIdx < 0 || callIdx >= len(p.Calls) {
		return
	}
	call := p.Calls[callIdx]
	if len(call.Args) == 0 {
		return
	}
	ptrArg, ok := call.Args[0].(*PointerArg)
	if !ok {
		return
	}
	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return
	}
	dataArg.data = []byte(newPath + "\x00")
	r.target.assignSizesCall(call)

	// rename 的目标路径跟随源迁移（L25）：目标基于旧源派生
	// （源._renamed_xxx）——源迁移后目标同步重新派生（对齐 L9 的 rename 处理）。
	if strings.Contains(call.Meta.Name, "rename") {
		updateCallPathByArgIdx(call, 1, newPath+"._renamed_"+randomSuffix(r.Rand), r)
	}
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

func pickMutationLength(r *randGen) uint64 {
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

func updateWriteDataBuf(p *Prog, call *Call, newLen uint64, r *randGen) {
	if len(call.Args) < 2 {
		return
	}
	ptrArg, ok := call.Args[1].(*PointerArg)
	if !ok {
		return
	}
	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return
	}
	oldData := dataArg.Data()
	newData := make([]byte, newLen)
	copy(newData, oldData)
	for i := uint64(len(oldData)); i < newLen; i++ {
		newData[i] = byte(r.Intn(256))
	}
	reassignDataArg(p, call, r, newData)
}

func getFdFilePathForCall(ps []*Prog, pos GroupPosition) string {
	call := ps[pos.ProgIdx].Calls[pos.CallIdx]
	callName := call.Meta.CallName

	if callName == "open" || callName == "creat" ||
		callName == "truncate" || callName == "chmod" ||
		callName == "mkdir" || callName == "rmdir" ||
		callName == "rename" || callName == "stat" ||
		callName == "unlink" || callName == "getdents64" {
		return extractPathFromCall(call)
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

func findOpenFdForPath(p *Prog, filePath string, beforeIdx int) *ResultArg {
	for i := 0; i < beforeIdx && i < len(p.Calls); i++ {
		call := p.Calls[i]
		if !strings.Contains(call.Meta.Name, "open") && !strings.Contains(call.Meta.Name, "creat") {
			continue
		}
		path := extractPathFromCall(call)
		if path != filePath {
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

func insertCallFromDCT(ps []*Prog, r *randGen, ct *ChoiceTable, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, seedType string) bool {
	recordLastMutation("insertCallFromDCT:" + seedType)
	if len(ps) == 0 {
		return false
	}

	p0 := ps[0]
	cid0 := hmcfg.Cids[0]
	var basePath string
	var basePath2 string = ""

	// Collect all file paths from p0's calls, pick one randomly
	var pathCandidates []string
	for _, call := range p0.Calls {
		if call.Meta.Name == "rename" {
			p1, p2 := extractRenamePaths(call)
			if p1 != "" {
				pathCandidates = append(pathCandidates, p1)
			}
			if p2 != "" {
				pathCandidates = append(pathCandidates, p2)
			}
		} else {
			p := extractPathFromCall(call)
			if p != "" {
				pathCandidates = append(pathCandidates, p)
			}
		}
	}
	if len(pathCandidates) > 0 {
		basePath = pathCandidates[r.Intn(len(pathCandidates))]
	}

	insertPos := r.biasedRand(len(p0.Calls)+1, 5)

	biasCall := -1
	if insertPos > 0 && insertPos <= len(p0.Calls) {
		biasCall = p0.Calls[r.Intn(insertPos)].Meta.ID
	}

	var subCt *ChoiceTable
	if seedType == "fileops" && lcs != nil {
		subCt = lcs.FileopsChoiceTable
	} else if lcs != nil {
		subCt = lcs.InodeopsChoiceTable
	}
	rootCallName := r.chooseRootCallName(subCt, biasCall)
	if rootCallName == "" {
		return false
	}

	if basePath == "" {
		if seedType == "fileops" {
			fileNode := lcs.FileTree.GetRandomFile(r.Rand, cid0)
			if fileNode == nil {
				return false
			}
			basePath = fileNode.FullPath
		} else {
			if IsDirOnlyCall(rootCallName) {
				dirNode := lcs.FileTree.GetRandomDir(r.Rand, cid0, true)
				if dirNode == nil {
					return false
				}
				basePath = dirNode.FullPath
			} else {
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
	}

	var ExistingFd *ResultArg = nil
	useExistFd := false
	if IsFdRequiredCall(rootCallName) {
		ExistingFd = findOpenFdForPath(p0, basePath, insertPos)
		if ExistingFd != nil {
			useExistFd = true
		}
	}

	if rootCallName == "rename" {
		basePath2 = basePath + "._renamed" + randomSuffix(r.Rand)
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
		node := lcs.FileTree.FindNode(basePath)
		if node != nil && (node.Type == NodeTypeDir || node.Type == NodeTypeEmptyDir) {
			rootFlags = r.target.GetConst("O_DIRECTORY")
		}
	}
	rootCalls := r.generateCallByName(stateFromProg(p0), sCalls, rootCallName, basePath, basePath2, cid0, ExistingFd, useExistFd, rootFileSize, rootFlags)

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

	for nodeIdx := 1; nodeIdx < len(ps); nodeIdx++ {
		useExistFd = true
		ExistingFd = nil
		p := ps[nodeIdx]
		cid := hmcfg.Cids[nodeIdx]

		variant := lcs.ChooseConcurrentCallFiltered(rootCallName, r.Rand, !isDirPath(lcs.FileTree, basePath))
		if variant == nil {
			continue
			//TODO: continue 对吗？
		}
		temporal := lcs.GetDCT().ChooseTemporal(rootCallName, *variant, r.Rand)

		alignedPos := TimeAlignedInsertPos(p, refTime, lcs.tscoffFor(nodeIdx))
		ConcurrentInsertPos := alignedPos
		if temporal == TemporalHB {
			// Causal form: insert where the call most likely starts after
			// the root finishes (favoring HB pairs).
			if hbPos := firstBoundaryAfter(p, refTime, lcs.tscoffFor(nodeIdx)); hbPos >= 0 {
				ConcurrentInsertPos = hbPos
			}
		}
		if ConcurrentInsertPos < 0 {
			ConcurrentInsertPos = min(insertPos, len(p.Calls))
		}

		concurrentPath := ""

		concurrentPath2 := ""
		if variant.CallName == "rename" {
			concurrentPath, concurrentPath2 = lcs.GetPathsForRenameVariant(basePath, "", variant.PathRelation, r.Rand, cid, false)
			if concurrentPath == "" {
				continue // 该关系无匹配——跳过此节点（S16）
			}
		} else {
			concurrentPath = lcs.FileTree.GetPathByRelation(basePath, "", variant.PathRelation, r.Rand, cid, false)
			if concurrentPath == "" {
				concurrentPath = basePath
			}
		}
		if IsFdRequiredCall(variant.CallName) {
			ExistingFd = findOpenFdForPath(p, concurrentPath, ConcurrentInsertPos)
			if ExistingFd == nil && alignedPos >= 0 {
				// Time alignment may place the call before the open;
				// fall back to the index-aligned position for fd usability.
				ConcurrentInsertPos = min(insertPos, len(p.Calls))
				ExistingFd = findOpenFdForPath(p, concurrentPath, ConcurrentInsertPos)
			}
			if ExistingFd == nil {
				useExistFd = false
			}
		}

		concurrentFileSize := uint64(0)
		concurrentFlags := uint64(2)
		if lcs != nil {
			node := lcs.FileTree.FindNode(concurrentPath)
			if node != nil {
				concurrentFileSize = node.Size
				if node.Type == NodeTypeDir || node.Type == NodeTypeEmptyDir {
					concurrentFlags = r.target.GetConst("O_DIRECTORY")
				}
			}
		}
		concurrentCalls := r.generateCallByName(stateFromProg(p), sCalls, variant.CallName, concurrentPath, concurrentPath2, cid, ExistingFd, useExistFd, concurrentFileSize, concurrentFlags)

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
func TimeAlignedInsertPos(p *Prog, refTime int64, tscoff int64) int {
	return timeAlignedInRange(p, refTime, tscoff, 0, len(p.Calls))
}

// firstBoundaryAfter returns the first call boundary at or after refTime: the
// causal-insertion counterpart of TimeAlignedInsertPos. Inserting a variant
// there makes it start after the root finishes, favoring HB pairs. Returns -1
// without any timing anchor (caller falls back to index alignment).
func firstBoundaryAfter(p *Prog, refTime int64, tscoff int64) int {
	if refTime < 0 {
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

func findInsertPosition(p *Prog, r *randGen, useFd int, sCalls *SpecialCalls, refTime int64, tscoff int64) (int, *ResultArg) {
	//useFd: 0 is false, 1 is file, 2 is dir
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
			if isOpenDirectory(openCall) {
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

func findInsertPositionByPath(p *Prog, r *randGen, filePath string, sCalls *SpecialCalls) (int, *ResultArg) {
	if len(p.Calls) == 0 {
		return 0, nil
	}

	openIdx := -1
	var fd *ResultArg

	for i, call := range p.Calls {
		if !strings.Contains(call.Meta.Name, "open") {
			continue
		}

		path := extractPathFromCall(call)
		if path == filePath {
			openIdx = i
			fd = extractFdFromCall(call)
			break
		}
	}

	if openIdx == -1 || fd == nil {
		return r.Intn(len(p.Calls) + 1), nil
	}

	closeIdx := len(p.Calls)
	closeCalls := findCallsByName(p, "close")

	for _, idx := range closeCalls {
		if idx > openIdx {
			closeCall := p.Calls[idx]
			if usesFd(closeCall, fd) {
				closeIdx = idx
				break
			}
		}
	}

	startIdx := openIdx + 1
	endIdx := closeIdx

	if startIdx >= endIdx {
		return openIdx + 1, fd
	}

	resultIdx := r.randRange(uint64(startIdx), uint64(endIdx))
	return int(resultIdx), fd
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
	args[0] = fd
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
	args[0] = fd

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
	args[0] = fd

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func removeStashCall(p *Prog, r *randGen) bool {
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
		newCmd := cmdBuilder.String()

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

func genPwriteCall(p *Prog, r *randGen, sCalls *SpecialCalls) *Call {
	meta := r.target.Syscalls[sCalls.Pwrite64Id]
	if meta == nil {
		return nil
	}

	fd := findAvailableFd(p)
	if fd == nil {
		return nil
	}

	s := stateFromProg(p)

	bufSize := r.randBufLen()
	bufData := make([]byte, bufSize)
	for i := range bufData {
		bufData[i] = byte(r.Intn(256))
	}

	args := make([]Arg, len(meta.Args))

	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)

	bufArg := MakeDataArg(bufPtrType.Elem, DirIn, bufData)
	bufPtr := r.allocAddr(s, bufPtrType, DirIn, bufArg.Size(), bufArg)

	pwritefdtype := meta.Args[0].Type.(*ResourceType)
	fdarg := MakeResultArg(pwritefdtype, DirIn, fd, 0)

	args[0] = fdarg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, uint64(bufSize))
	args[3] = MakeConstArg(posType, DirIn, uint64(r.randInt(64)))

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func genPreadCall(p *Prog, r *randGen, sCalls *SpecialCalls) *Call {
	meta := r.target.Syscalls[sCalls.Pread64Id]
	if meta == nil {
		return nil
	}

	fd := findAvailableFd(p)
	if fd == nil {
		return nil
	}

	s := stateFromProg(p)

	bufSize := r.randBufLen()

	args := make([]Arg, len(meta.Args))

	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)

	bufArg := MakeOutDataArg(bufPtrType.Elem, DirOut, bufSize)
	bufPtr := r.allocAddr(s, bufPtrType, DirOut, bufArg.Size(), bufArg)

	preadfdtype := meta.Args[0].Type.(*ResourceType)
	fdarg := MakeResultArg(preadfdtype, DirIn, fd, 0)

	args[0] = fdarg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, uint64(bufSize))
	args[3] = MakeConstArg(posType, DirIn, uint64(r.randInt(64)))

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
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
					updateCallPathByArgIdx(call, 1, newPath, r)
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
		dir = "merge_view"
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
	newDir := fmt.Sprintf("%s/mut_dir_%s", parentDir, randomSuffix(r.Rand))

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
			testDir = "merge_view/test_dir"
		}
		targetDir = fmt.Sprintf("%s/rmdir_%s", testDir, randomSuffix(r.Rand))
	}

	return genPathCall(meta, targetDir, r, s)
}

func genCreatCall(p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) *Call {
	parentDir := extractDirFromTree(hmcfg, p, r)
	fileName := fmt.Sprintf("%s/creat_file_%s.txt", parentDir, randomSuffix(r.Rand))
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
			testDir = "merge_view/test_dir"
		}
		targetFile = fmt.Sprintf("%s/unlink_file_%s.txt", testDir, randomSuffix(r.Rand))
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
			testDir = "merge_view/test_dir"
		}
		oldPath = fmt.Sprintf("%s/old_%s.txt", testDir, randomSuffix(r.Rand))
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
	if len(call.Args) == 0 {
		return false
	}

	ptrArg, ok := call.Args[0].(*PointerArg)
	if !ok {
		return false
	}

	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return false
	}

	dataArg.data = []byte(newPath + "\x00")
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
		if strings.Contains(name, "getdents64") || strings.Contains(name, "mkdir") || strings.Contains(name, "rmdir") {
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

/******************************* InodeOps Seed Mutation ************************************/

const (
	InodeOpsMutOpType = iota
	InodeOpsMutParams
	InodeOpsMutSequence
	InodeOpsMutAddRemove
	InodeOpsMutTypeCount
)

func MutateInodeOpsProg(ps []*Prog, rs rand.Source, ct *ChoiceTable, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	if len(ps) == 0 {
		return false
	}

	r := newRand(ps[0].Target, rs)
	r.hmcfg = hmcfg

	mutType := r.Intn(InodeOpsMutTypeCount)

	switch mutType {
	case InodeOpsMutOpType:
		return mutateInodeOpsOpType(ps, r, sCalls, hmcfg)
	case InodeOpsMutParams:
		return mutateInodeOpsParams(ps, r, sCalls)
	case InodeOpsMutSequence:
		//TODO: 其实是 swap，重要性存疑
		return mutateInodeOpsSequence(ps, r)
	case InodeOpsMutAddRemove:
		//TODO: 替换为基于distributed choice
		return mutateInodeOpsAddRemove(ps, r, sCalls, hmcfg)
	}
	return false
}

func mutateInodeOpsOpType(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	for _, p := range ps {
		if !p.IsInodeOps {
			continue
		}

		opTypes := []string{"chmod", "truncate", "rename", "unlink", "mkdir", "rmdir"}
		targetTypes := []string{"chmod", "truncate", "rename", "unlink", "mkdir", "rmdir"}

		for i, call := range p.Calls {
			callName := call.Meta.Name
			for _, opType := range opTypes {
				if strings.Contains(callName, opType) {
					newOpType := targetTypes[r.Intn(len(targetTypes))]
					if newOpType != opType {
						return replaceInodeOpsCall(p, i, call, newOpType, r, sCalls, hmcfg)
					}
				}
			}
		}
	}
	return false
}

func replaceInodeOpsCall(p *Prog, idx int, oldCall *Call, newOpType string, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	path := extractPathFromCall(oldCall)
	if path == "" {
		return false
	}

	var newCall *Call
	switch newOpType {
	case "chmod":
		newCall = genChmodCallWithMode(p, r, sCalls, path, r.Intn(0o777+1))
	/*case "chown":
	newCall = genChownCall(p, r, sCalls, path)*/
	case "truncate":
		fileSize := uint64(0)
		if hmcfg != nil && hmcfg.FileTree != nil {
			node := hmcfg.FileTree.FindNode(path)
			if node != nil {
				fileSize = node.Size
			}
		}
		newCall = genTruncateCall(p, r, sCalls, path, fileSize)
	case "rename":
		newPath := path + "_renamed_" + randomSuffix(r.Rand)
		newCall = genRenameCallWithPaths(p, r, sCalls, path, newPath)
	case "unlink":
		newCall = genUnlinkCallWithPath(p, r, sCalls, path)
	case "mkdir":
		newPath := path + "_newdir_" + randomSuffix(r.Rand)
		newCall = genMkdirCallWithPath(p, r, sCalls, newPath)
	case "rmdir":
		newCall = genRmdirCallWithPath(p, r, sCalls, path)
	}

	if newCall == nil {
		return false
	}

	p.Calls[idx] = newCall
	return true
}

func mutateInodeOpsParams(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for _, p := range ps {
		if !p.IsInodeOps {
			continue
		}

		for _, call := range p.Calls {
			callName := call.Meta.Name

			if strings.Contains(callName, "chmod") {
				return mutateChmodMode(call, r)
			}
			/*if strings.Contains(callName, "chown") {
				return mutateChownIds(call, r)
			}*/
			if strings.Contains(callName, "truncate") {
				//TODO: 依然length取什么范围的问题
				return mutateTruncateLength(call, r)
			}
			if strings.Contains(callName, "rename") {
				return mutateRenamePaths(call, r)
			}
		}
	}
	return false
}

func mutateChmodMode(call *Call, r *randGen) bool {
	if len(call.Args) < 2 {
		return false
	}

	modeArg, ok := call.Args[1].(*ConstArg)
	if !ok {
		return false
	}

	oldMode := modeArg.Val
	newMode := oldMode ^ (1 << uint64(r.Intn(9)))
	if newMode == oldMode {
		newMode = 0o755
	}
	modeArg.Val = newMode
	return true
}

/*func mutateChownIds(call *Call, r *randGen) bool {
	if len(call.Args) < 3 {
		return false
	}

	uidArg, ok1 := call.Args[1].(*ConstArg)
	gidArg, ok2 := call.Args[2].(*ConstArg)
	if !ok1 || !ok2 {
		return false
	}

	delta := r.Intn(2001) - 1000
	if delta < 0 {
		minusdelta := -delta
		uidArg.Val = uidArg.Val - uint64(minusdelta)
		gidArg.Val = gidArg.Val - uint64(minusdelta)
	} else {
		uidArg.Val = uidArg.Val + uint64(delta)
		gidArg.Val = gidArg.Val + uint64(delta)
	}
	return true
}*/

func mutateTruncateLength(call *Call, r *randGen) bool {
	if len(call.Args) < 2 {
		return false
	}

	lengthArg, ok := call.Args[1].(*ConstArg)
	if !ok {
		return false
	}

	delta := r.Intn(2049) - 1024
	newLength := int64(lengthArg.Val) + int64(delta)
	if newLength < 0 {
		newLength = 0
	}
	lengthArg.Val = uint64(newLength)
	return true
}

func mutateRenamePaths(call *Call, r *randGen) bool {
	if len(call.Args) < 2 {
		return false
	}

	oldPath := extractPathFromCallByArgIdx(call, 0)
	if oldPath == "" {
		return false
	}

	newPath := oldPath + "_mutated_" + randomSuffix(r.Rand)
	return updateCallPathByArgIdx(call, 1, newPath, r)
}

func mutateInodeOpsSequence(ps []*Prog, r *randGen) bool {
	for _, p := range ps {
		if !p.IsInodeOps || len(p.Calls) < 3 {
			continue
		}

		candidates := make([]int, 0)
		for i, call := range p.Calls {
			if strings.Contains(call.Meta.Name, "syz_failure") ||
				strings.Contains(call.Meta.Name, "open") || strings.Contains(call.Meta.Name, "close") {
				continue
			}
			candidates = append(candidates, i)
		}

		if len(candidates) < 2 {
			continue
		}

		idx1 := candidates[r.Intn(len(candidates))]
		idx2 := candidates[r.Intn(len(candidates))]
		for idx2 == idx1 {
			idx2 = candidates[r.Intn(len(candidates))]
		}

		p.Calls[idx1], p.Calls[idx2] = p.Calls[idx2], p.Calls[idx1]
		return true
	}
	return false
}

func mutateInodeOpsAddRemove(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	for _, p := range ps {
		if !p.IsInodeOps {
			continue
		}

		if r.bin() {
			return insertInodeOpsCall(p, r, sCalls, hmcfg)
		} else {
			return removeInodeOpsCall(p, r)
		}
	}
	return false
}

func insertInodeOpsCall(p *Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	if len(p.Calls) == 0 {
		return false
	}

	insertPos := r.Intn(len(p.Calls) + 1)

	parentDir := extractDirFromTree(hmcfg, p, r)

	callTypes := []string{"chmod", "truncate", "rename", "unlink", "mkdir"}
	callType := callTypes[r.Intn(len(callTypes))]

	var newCall *Call
	switch callType {
	case "chmod":
		testFile := extractFileFromTree(hmcfg, p, r)
		if testFile == "" {
			testFile = parentDir + "/chmod_test_" + randomSuffix(r.Rand) + ".txt"
		}
		newCall = genChmodCallWithMode(p, r, sCalls, testFile, r.Intn(0o777+1))
	case "truncate":
		testFile := extractFileFromTree(hmcfg, p, r)
		if testFile == "" {
			testFile = parentDir + "/truncate_test_" + randomSuffix(r.Rand) + ".txt"
		}
		fileSize := uint64(0)
		if hmcfg.FileTree != nil {
			node := hmcfg.FileTree.FindNode(testFile)
			if node != nil {
				fileSize = node.Size
			}
		}
		newCall = genTruncateCall(p, r, sCalls, testFile, fileSize)
	case "rename":
		oldPath := extractFileFromTree(hmcfg, p, r)
		if oldPath == "" {
			oldPath = parentDir + "/rename_old_" + randomSuffix(r.Rand) + ".txt"
		}
		newPath := oldPath[:strings.LastIndex(oldPath, "/")+1] + "rename_new_" + randomSuffix(r.Rand) + ".txt"
		newCall = genRenameCallWithPaths(p, r, sCalls, oldPath, newPath)
	case "unlink":
		testFile := extractFileFromTree(hmcfg, p, r)
		if testFile == "" {
			testFile = parentDir + "/unlink_test_" + randomSuffix(r.Rand) + ".txt"
		}
		newCall = genUnlinkCallWithPath(p, r, sCalls, testFile)
	case "mkdir":
		newDir := parentDir + "/mkdir_test_" + randomSuffix(r.Rand)
		newCall = genMkdirCallWithPath(p, r, sCalls, newDir)
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

func removeInodeOpsCall(p *Prog, r *randGen) bool {
	if len(p.Calls) <= 2 {
		return false
	}

	candidates := make([]int, 0)
	for i, call := range p.Calls {
		name := call.Meta.Name
		if strings.Contains(name, "chmod") || strings.Contains(name, "chown") ||
			strings.Contains(name, "truncate") || strings.Contains(name, "rename") ||
			strings.Contains(name, "unlink") || strings.Contains(name, "mkdir") ||
			strings.Contains(name, "rmdir") {
			candidates = append(candidates, i)
		}
	}

	if len(candidates) == 0 {
		return false
	}

	idx := candidates[r.Intn(len(candidates))]
	p.RemoveCall(idx)
	return true
}

func genChmodCallWithMode(p *Prog, r *randGen, sCalls *SpecialCalls, filePath string, mode int) *Call {
	meta := r.target.Syscalls[sCalls.ChmodId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	ptrType := meta.Args[0].Type.(*PtrType)
	modeType := meta.Args[1].Type.(*FlagsType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)
	modeArg := MakeConstArg(modeType, DirIn, uint64(mode))

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	args[1] = modeArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

/*func genChownCall(p *Prog, r *randGen, sCalls *SpecialCalls, filePath string) *Call {
	meta := r.target.Syscalls[sCalls.ChownId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	ptrType := meta.Args[0].Type.(*PtrType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)

	uid := r.Intn(65536)
	gid := r.Intn(65536)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	if len(args) > 1 {
		args[1] = MakeConstArg(meta.Args[1].Type.(*IntType), DirIn, uint64(uid))
	}
	if len(args) > 2 {
		args[2] = MakeConstArg(meta.Args[2].Type.(*IntType), DirIn, uint64(gid))
	}

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}*/

func genTruncateCall(p *Prog, r *randGen, sCalls *SpecialCalls, filePath string, fileSize uint64) *Call {
	meta := r.target.Syscalls[sCalls.TruncateId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	ptrType := meta.Args[0].Type.(*PtrType)

	var length int
	if fileSize > 0 && r.nOutOf(4, 5) {
		length = int(r.randRange(0, fileSize*2))
	} else {
		length = r.Intn(4096)
	}

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	if len(args) > 1 {
		args[1] = MakeConstArg(meta.Args[1].Type.(*IntType), DirIn, uint64(length))
	}

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func genRenameCallWithPaths(p *Prog, r *randGen, sCalls *SpecialCalls, oldPath, newPath string) *Call {
	meta := r.target.Syscalls[sCalls.RenameId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

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

func genMkdirCallWithPath(p *Prog, r *randGen, sCalls *SpecialCalls, dirPath string) *Call {
	meta := r.target.Syscalls[sCalls.MkdirId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)
	return genPathCall(meta, dirPath, r, s)
}

func genRmdirCallWithPath(p *Prog, r *randGen, sCalls *SpecialCalls, dirPath string) *Call {
	meta := r.target.Syscalls[sCalls.RmdirId]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)
	return genPathCall(meta, dirPath, r, s)
}

/******************************* FileOps Seed Mutation ************************************/

const (
	FileOpsMutOffset = iota
	FileOpsMutLength
	FileOpsMutData
	FileOpsMutConcurrent
	FileOpsMutFsync
	FileOpsMutTypeCount
)

func MutateFileopsProg(ps []*Prog, rs rand.Source, ct *ChoiceTable, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	if len(ps) == 0 {
		return false
	}

	r := newRand(ps[0].Target, rs)
	r.hmcfg = hmcfg

	mutType := r.Intn(FileOpsMutTypeCount)

	switch mutType {
	case FileOpsMutOffset:
		return mutateFileopsOffset(ps, r)
	case FileOpsMutLength:
		return mutateFileopsLength(ps, r)
	case FileOpsMutData:
		return mutateFileopsData(ps, r)
	case FileOpsMutConcurrent:
		return mutateFileopsConcurrent(ps, r, sCalls, hmcfg)
	case FileOpsMutFsync:
		return mutateFileopsFsync(ps, r, sCalls)
	}
	return false
}

func mutateFileopsOffset(ps []*Prog, r *randGen) bool {
	for _, p := range ps {
		if !p.IsFileOps {
			continue
		}

		pwriteCalls := findCallsByName(p, "pwrite64")
		preadCalls := findCallsByName(p, "pread64")

		allCalls := append(pwriteCalls, preadCalls...)
		if len(allCalls) == 0 {
			continue
		}

		callIdx := allCalls[r.Intn(len(allCalls))]
		call := p.Calls[callIdx]

		if len(call.Args) < 4 {
			continue
		}

		posArg, ok := call.Args[3].(*ConstArg)
		if !ok {
			continue
		}

		delta := r.Intn(2049) - 1024
		if delta == 0 {
			delta = 1
		}
		newVal := int64(posArg.Val) + int64(delta)
		if newVal < 0 {
			newVal = 0
		}
		posArg.Val = uint64(newVal)
		return true
	}
	return false
}

func mutateFileopsLength(ps []*Prog, r *randGen) bool {
	for _, p := range ps {
		if !p.IsFileOps {
			continue
		}

		pwriteCalls := findCallsByName(p, "pwrite64")
		preadCalls := findCallsByName(p, "pread64")

		allCalls := append(pwriteCalls, preadCalls...)
		if len(allCalls) == 0 {
			continue
		}

		callIdx := allCalls[r.Intn(len(allCalls))]
		call := p.Calls[callIdx]

		if len(call.Args) < 3 {
			continue
		}

		countArg, ok := call.Args[2].(*ConstArg)
		if !ok {
			continue
		}

		delta := r.Intn(1025) - 512
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
		countArg.Val = uint64(newLen)

		if len(call.Args) >= 2 {
			if ptrArg, ok := call.Args[1].(*PointerArg); ok {
				if dataArg, ok := ptrArg.Res.(*DataArg); ok {
					oldData := dataArg.Data()
					newData := make([]byte, newLen)
					copy(newData, oldData)
					for i := len(oldData); i < int(newLen); i++ {
						newData[i] = byte(r.Intn(256))
					}
					dataArg.data = newData
				}
			}
		}
		return true
	}
	return false
}

func mutateFileopsData(ps []*Prog, r *randGen) bool {
	for _, p := range ps {
		if !p.IsFileOps {
			continue
		}

		pwriteCalls := findCallsByName(p, "pwrite64")
		if len(pwriteCalls) == 0 {
			continue
		}

		callIdx := pwriteCalls[r.Intn(len(pwriteCalls))]
		call := p.Calls[callIdx]

		if len(call.Args) < 2 {
			continue
		}

		ptrArg, ok := call.Args[1].(*PointerArg)
		if !ok {
			continue
		}

		dataArg, ok := ptrArg.Res.(*DataArg)
		if !ok {
			continue
		}

		data := dataArg.Data()
		olddata := dataArg.Data()
		if len(data) == 0 {
			continue
		}

		data = mutateData(r, data, 1, 8192)
		dataArg.data = data

		countArg, ok := call.Args[2].(*ConstArg)
		if !ok {
			dataArg.data = olddata
			return false
		}
		countArg.Val = uint64(len(data))
		return true
	}
	return false
}

func mutateFileopsConcurrent(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	if len(ps) < 2 {
		return false
	}

	for _, p := range ps {
		if !p.IsFileOps {
			continue
		}

		mutType := r.Intn(3)
		switch mutType {
		//TODO: 这里插入并发读和写的约束太松弛了，实际上并没有很并发，考虑替换为基于 distributed choice。然后这里 distributed choice 是否要把 offset、length 等元数据也纳入考虑？
		case 0:
			return insertConcurrentRead(ps, r, sCalls, hmcfg)
		case 1:
			return insertConcurrentWrite(ps, r, sCalls, hmcfg)
		case 2:
			return insertOverlappingWrite(ps, r, sCalls, hmcfg)
		}
	}
	return false
}

func insertOverlappingWrite(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	for srcNodeIdx, srcP := range ps {
		if !srcP.IsFileOps || len(srcP.Calls) == 0 {
			continue
		}

		pwriteCalls := findCallsByName(srcP, "pwrite64")
		if len(pwriteCalls) == 0 {
			continue
		}

		srcCallIdx := pwriteCalls[r.Intn(len(pwriteCalls))]
		srcCall := srcP.Calls[srcCallIdx]

		// src 文件 = pwrite 实际写的文件（多 open 时第一个 open 路径可能
		// 不一致——overlap 相对错误文件计算，L24）。
		srcFilePath := extractFilePath(srcP)
		if p := resolveFdToPath(srcP, srcCall); p != "" {
			srcFilePath = p
		}
		if srcFilePath == "" {
			continue
		}

		srcOffset := extractPwriteOffset(srcCall)
		srcLength := extractPwriteLength(srcCall)
		if srcLength == 0 {
			srcLength = 100
		}

		for dstNodeIdx, dstP := range ps {
			if dstNodeIdx == srcNodeIdx || !dstP.IsFileOps {
				continue
			}

			overlapOffset := srcOffset + int64(r.Intn(int(srcLength)))
			if overlapOffset < 0 {
				overlapOffset = srcOffset
			}

			insertPos := r.Intn(len(dstP.Calls) + 1)
			fd := findOpenFdForPath(dstP, srcFilePath, insertPos)
			if fd == nil {
				continue
			}

			overlapWrite := genPwriteCallWithOffsetAndLength(dstP, r, sCalls, fd, overlapOffset, int64(r.Intn(100)+1))
			if overlapWrite == nil {
				continue
			}
			//TODO: 这里没有让覆盖写尽可能并发
			if insertPos >= len(dstP.Calls) {
				dstP.Calls = append(dstP.Calls, overlapWrite)
			} else {
				dstP.Calls = append(dstP.Calls[:insertPos], append([]*Call{overlapWrite}, dstP.Calls[insertPos:]...)...)
			}
			return true
		}
	}
	return false
}

func extractPwriteOffset(call *Call) int64 {
	if len(call.Args) < 4 {
		return 0
	}
	posArg, ok := call.Args[3].(*ConstArg)
	if !ok {
		return 0
	}
	return int64(posArg.Val)
}

func extractPwriteLength(call *Call) int64 {
	if len(call.Args) < 3 {
		return 0
	}
	countArg, ok := call.Args[2].(*ConstArg)
	if !ok {
		return 0
	}
	return int64(countArg.Val)
}

func genPwriteCallWithOffsetAndLength(p *Prog, r *randGen, sCalls *SpecialCalls, fd *ResultArg, offset, length int64) *Call {
	meta := r.target.Syscalls[sCalls.Pwrite64Id]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	bufData := make([]byte, length)
	for i := range bufData {
		bufData[i] = byte(r.Intn(256))
	}

	bufPtrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)

	bufArg := MakeDataArg(bufPtrType.Elem, DirIn, bufData)
	bufPtr := r.allocAddr(s, bufPtrType, DirIn, bufArg.Size(), bufArg)

	if fd == nil {
		return nil
	}

	pwritefdtype := meta.Args[0].Type.(*ResourceType)
	fdarg := MakeResultArg(pwritefdtype, DirIn, fd, 0)

	args := make([]Arg, len(meta.Args))
	args[0] = fdarg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, uint64(length))
	args[3] = MakeConstArg(posType, DirIn, uint64(offset))

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func findFdForPath(p *Prog, filePath string) *ResultArg {
	for _, call := range p.Calls {
		if !strings.Contains(call.Meta.Name, "open") {
			continue
		}

		path := extractPathFromCall(call)
		if path == filePath {
			return extractFdFromCall(call)
		}
	}
	return nil
}

func insertConcurrentRead(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	for i, p := range ps {
		if !p.IsFileOps || len(p.Calls) == 0 {
			continue
		}

		filePath := extractFilePath(p)
		if filePath == "" {
			continue
		}

		for j, otherP := range ps {
			if i == j || !otherP.IsFileOps {
				continue
			}

			if len(otherP.Calls) == 0 {
				continue
			}

			insertPos := r.Intn(len(otherP.Calls) + 1)
			fd := findOpenFdForPath(otherP, filePath, insertPos)
			if fd == nil {
				continue
			}
			readCall := genPreadCallWithFd(otherP, r, sCalls, fd)

			if readCall == nil {
				continue
			}

			if insertPos >= len(otherP.Calls) {
				otherP.Calls = append(otherP.Calls, readCall)
			} else {
				otherP.Calls = append(otherP.Calls[:insertPos], append([]*Call{readCall}, otherP.Calls[insertPos:]...)...)
			}
			return true
		}
	}
	return false
}

func insertConcurrentWrite(ps []*Prog, r *randGen, sCalls *SpecialCalls, hmcfg *Hmdfs_config) bool {
	for i, p := range ps {
		if !p.IsFileOps || len(p.Calls) == 0 {
			continue
		}

		filePath := extractFilePath(p)
		if filePath == "" {
			continue
		}

		for j, otherP := range ps {
			if i == j || !otherP.IsFileOps {
				continue
			}

			if len(otherP.Calls) == 0 {
				continue
			}

			insertPos := r.Intn(len(otherP.Calls) + 1)
			fd := findOpenFdForPath(otherP, filePath, insertPos)
			if fd == nil {
				continue
			}
			writeCall := genPwriteCallWithFd(otherP, r, sCalls, fd)

			if writeCall == nil {
				continue
			}

			if insertPos >= len(otherP.Calls) {
				otherP.Calls = append(otherP.Calls, writeCall)
			} else {
				otherP.Calls = append(otherP.Calls[:insertPos], append([]*Call{writeCall}, otherP.Calls[insertPos:]...)...)
			}
			return true
		}
	}
	return false
}

func mutateFileopsFsync(ps []*Prog, r *randGen, sCalls *SpecialCalls) bool {
	for _, p := range ps {
		if !p.IsFileOps {
			continue
		}
		//TODO: 这里所有的 fsync 和 fdatasync 的问题都是一样的，只插入一个，也不突变位置，肯定不好
		fsyncCalls := findCallsByName(p, "fsync")
		fdatasyncCalls := findCallsByName(p, "fdatasync")

		if len(fsyncCalls) > 0 && len(fdatasyncCalls) > 0 {
			continue
		}

		openCalls := findCallsByName(p, "open")
		if len(openCalls) == 0 {
			continue
		}

		// 插入位置约束在第一个 open 之后——fsync 的 fd（findAvailableFd 同款）
		// 在 insertPos 前有效（避免 use-before-produce，S14）。
		openIdx := openCalls[0]
		insertPos := openIdx + 1 + r.Intn(len(p.Calls)-openIdx)

		var fsyncCall *Call
		if r.bin() {
			fsyncCall = genFsyncCall(p, r, sCalls)
		} else {
			fsyncCall = genFdatasyncCall(p, r, sCalls)
		}

		if fsyncCall == nil {
			continue
		}

		if insertPos >= len(p.Calls) {
			p.Calls = append(p.Calls, fsyncCall)
		} else {
			p.Calls = append(p.Calls[:insertPos], append([]*Call{fsyncCall}, p.Calls[insertPos:]...)...)
		}
		return true
	}
	return false
}

func findReadWriteCalls(p *Prog) []int {
	var indices []int
	for i, call := range p.Calls {
		name := call.Meta.Name
		if strings.Contains(name, "pwrite64") || strings.Contains(name, "pread64") ||
			strings.Contains(name, "write") || strings.Contains(name, "read") {
			indices = append(indices, i)
		}
	}
	return indices
}

func extractFilePath(p *Prog) string {
	for _, call := range p.Calls {
		name := call.Meta.Name
		if strings.Contains(name, "open") || strings.Contains(name, "creat") {
			return extractPathFromCall(call)
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

func updateCallPathByArgIdx(call *Call, argIdx int, newPath string, r *randGen) bool {
	if len(call.Args) <= argIdx {
		return false
	}

	ptrArg, ok := call.Args[argIdx].(*PointerArg)
	if !ok {
		return false
	}

	dataArg, ok := ptrArg.Res.(*DataArg)
	if !ok {
		return false
	}

	dataArg.data = []byte(newPath + "\x00")
	r.target.assignSizesCall(call)
	return true
}

func genPreadCallWithPath(p *Prog, r *randGen, sCalls *SpecialCalls, filePath string) *Call {
	meta := r.target.Syscalls[sCalls.Pread64Id]
	if meta == nil {
		return nil
	}

	s := stateFromProg(p)

	bufSize := r.randBufLen()

	ptrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)

	bufArg := MakeOutDataArg(ptrType.Elem, DirOut, bufSize)
	bufPtr := r.allocAddr(s, ptrType, DirOut, bufArg.Size(), bufArg)

	args := make([]Arg, len(meta.Args))
	fd := findFdForPath(p, filePath)
	if fd == nil {
		return nil
	}
	preadfdtype := meta.Args[0].Type.(*ResourceType)
	fdarg := MakeResultArg(preadfdtype, DirIn, fd, 0)
	args[0] = fdarg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, uint64(bufSize))
	args[3] = MakeConstArg(posType, DirIn, uint64(r.randInt(64)))

	if args[0] == nil {
		return nil
	}

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}

func genPwriteCallWithPath(p *Prog, r *randGen, sCalls *SpecialCalls, filePath string) *Call {
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

	ptrType := meta.Args[1].Type.(*PtrType)
	countType := meta.Args[2].Type.(*LenType)
	posType := meta.Args[3].Type.(*IntType)

	bufArg := MakeDataArg(ptrType.Elem, DirIn, bufData)
	bufPtr := r.allocAddr(s, ptrType, DirIn, bufArg.Size(), bufArg)

	args := make([]Arg, len(meta.Args))
	fd := findFdForPath(p, filePath)
	if fd == nil {
		return nil
	}
	pwritefdtype := meta.Args[0].Type.(*ResourceType)
	fdarg := MakeResultArg(pwritefdtype, DirIn, fd, 0)
	args[0] = fdarg
	args[1] = bufPtr
	args[2] = MakeConstArg(countType, DirIn, uint64(bufSize))
	args[3] = MakeConstArg(posType, DirIn, uint64(r.randInt(64)))

	if args[0] == nil {
		return nil
	}

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)

	return c
}
