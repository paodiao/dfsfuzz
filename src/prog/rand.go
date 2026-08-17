// Copyright 2015/2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"monarch/pkg/ifuzz"
	"monarch/pkg/log"
)

const (
	// "Recommended" number of calls in programs that we try to aim at during fuzzing.
	RecommendedCalls = 30
	// "Recommended" max number of calls in programs.
	// If we receive longer programs from hub/corpus we discard them.
	MaxCalls = 35
)

type randGen struct {
	*rand.Rand
	target             *Target
	inGenerateResource bool
	recDepth           map[string]int
	hmcfg              *Hmdfs_config
	curIdx             int
}

func newRand(target *Target, rs rand.Source) *randGen {
	return &randGen{
		Rand:     rand.New(rs),
		target:   target,
		recDepth: make(map[string]int),
	}
}

func NewRand(target *Target, rs rand.Source) *randGen {
	return &randGen{
		Rand:     rand.New(rs),
		target:   target,
		recDepth: make(map[string]int),
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a < b {
		return b
	}
	return a
}

func (r *randGen) rand(n int) uint64 {
	return uint64(r.Intn(n))
}

func (r *randGen) randRange(begin, end uint64) uint64 {
	return begin + uint64(r.Intn(int(end-begin+1)))
}

func (r *randGen) bin() bool {
	return r.Intn(2) == 0
}

func (r *randGen) oneOf(n int) bool {
	return r.Intn(n) == 0
}

func (r *randGen) rand64() uint64 {
	v := uint64(r.Int63())
	if r.bin() {
		v |= 1 << 63
	}
	return v
}

var (
	// Some potentially interesting integers.
	specialInts = []uint64{
		0, 1, 31, 32, 63, 64, 127, 128,
		129, 255, 256, 257, 511, 512,
		1023, 1024, 1025, 2047, 2048, 4095, 4096,
		(1 << 15) - 1, (1 << 15), (1 << 15) + 1,
		(1 << 16) - 1, (1 << 16), (1 << 16) + 1,
		(1 << 31) - 1, (1 << 31), (1 << 31) + 1,
		(1 << 32) - 1, (1 << 32), (1 << 32) + 1,
	}
	// The indexes (exclusive) for the maximum specialInts values that fit in 1, 2, ... 8 bytes.
	specialIntIndex [9]int
)

func init() {
	sort.Slice(specialInts, func(i, j int) bool {
		return specialInts[i] < specialInts[j]
	})
	for i := range specialIntIndex {
		bitSize := uint64(8 * i)
		specialIntIndex[i] = sort.Search(len(specialInts), func(i int) bool {
			return specialInts[i]>>bitSize != 0
		})
	}
}

func (r *randGen) randInt64() uint64 {
	return r.randInt(64)
}

func (r *randGen) randInt(bits uint64) uint64 {
	v := r.rand64()
	switch {
	case r.nOutOf(100, 182):
		v %= 10
	case bits >= 8 && r.nOutOf(50, 82):
		v = specialInts[r.Intn(specialIntIndex[bits/8])]
	case r.nOutOf(10, 32):
		v %= 256
	case r.nOutOf(10, 22):
		v %= 4 << 10
	case r.nOutOf(10, 12):
		v %= 64 << 10
	default:
		v %= 1 << 31
	}
	switch {
	case r.nOutOf(100, 107):
	case r.nOutOf(5, 7):
		v = uint64(-int64(v))
	default:
		v <<= uint(r.Intn(int(bits)))
	}
	return truncateToBitSize(v, bits)
}

func truncateToBitSize(v, bitSize uint64) uint64 {
	if bitSize == 0 || bitSize > 64 {
		panic(fmt.Sprintf("invalid bitSize value: %d", bitSize))
	}
	return v & uint64(1<<bitSize-1)
}

func (r *randGen) randRangeInt(begin, end, bitSize, align uint64) uint64 {
	if r.oneOf(100) {
		return r.randInt(bitSize)
	}
	if align != 0 {
		if begin == 0 && int64(end) == -1 {
			// Special [0:-1] range for all possible values.
			end = uint64(1<<bitSize - 1)
		}
		endAlign := (end - begin) / align
		return begin + r.randRangeInt(0, endAlign, bitSize, 0)*align
	}
	return begin + (r.Uint64() % (end - begin + 1))
}

// biasedRand returns a random int in range [0..n),
// probability of n-1 is k times higher than probability of 0.
func (r *randGen) biasedRand(n, k int) int {
	nf, kf := float64(n), float64(k)
	rf := nf * (kf/2 + 1) * r.Float64()
	bf := (-1 + math.Sqrt(1+2*kf*rf/nf)) * nf / kf
	return int(bf)
}

func (r *randGen) randArrayLen() uint64 {
	const maxLen = 10
	// biasedRand produces: 10, 9, ..., 1, 0,
	// we want: 1, 2, ..., 9, 10, 0
	return uint64(maxLen-r.biasedRand(maxLen+1, 10)+1) % (maxLen + 1)
}

func (r *randGen) randBufLen() (n uint64) {
	switch {
	case r.nOutOf(50, 56):
		n = r.rand(512)
	case r.nOutOf(5, 6):
		n = 4 << 10
	}
	return
}

func (r *randGen) randPageCount() (n uint64) {
	switch {
	case r.nOutOf(100, 106):
		n = r.rand(4) + 1
	case r.nOutOf(5, 6):
		n = r.rand(20) + 1
	default:
		n = (r.rand(3) + 1) * r.target.NumPages / 4
	}
	return
}

// Change a flag value or generate a new one.
// If you are changing this function, run TestFlags and examine effect of results.
func (r *randGen) flags(vv []uint64, bitmask bool, oldVal uint64) uint64 {
	// Get these simpler cases out of the way first.
	// Once in a while we want to return completely random values,
	// or 0 which is frequently special.
	if r.oneOf(100) {
		return r.rand64()
	}
	if r.oneOf(50) {
		return 0
	}
	if !bitmask && oldVal != 0 && r.oneOf(100) {
		// Slightly increment/decrement the old value.
		// This is especially important during mutation when len(vv) == 1,
		// otherwise in that case we produce almost no randomness
		// (the value is always mutated to 0).
		inc := uint64(1)
		if r.bin() {
			inc = ^uint64(0)
		}
		v := oldVal + inc
		for r.bin() {
			v += inc
		}
		return v
	}
	if len(vv) == 1 {
		// This usually means that value or 0,
		// at least that's our best (and only) bet.
		if r.bin() {
			return 0
		}
		return vv[0]
	}
	if !bitmask && !r.oneOf(10) {
		// Enumeration, so just choose one of the values.
		return vv[r.rand(len(vv))]
	}
	if r.oneOf(len(vv) + 4) {
		return 0
	}
	// Flip rand bits. Do this for non-bitmask sometimes
	// because we may have detected bitmask incorrectly for complex cases
	// (e.g. part of the vlaue is bitmask and another is not).
	v := oldVal
	if v != 0 && r.oneOf(10) {
		v = 0 // Ignore the old value sometimes.
	}
	// We don't want to return 0 here, because we already given 0
	// fixed probability above (otherwise we get 0 too frequently).
	// Note: this loop can hang if all values are equal to 0. We don't generate such flags in the compiler now,
	// but it used to hang occasionally, so we keep the try < 10 logic b/c we don't have a local check for values.
	for try := 0; try < 10 && (v == 0 || r.nOutOf(2, 3)); try++ {
		flag := vv[r.rand(len(vv))]
		if r.oneOf(20) {
			// Try choosing adjacent bit values in case we forgot
			// to add all relevant flags to the descriptions.
			if r.bin() {
				flag >>= 1
			} else {
				flag <<= 1
			}
		}
		v ^= flag
	}
	return v
}

func (r *randGen) generateFileName(baseDir string) string {
	prefix := "Eris_"
	middle := r.randomString(10)
	suffix := r.timeStampMicro()
	fileType := r.randomString(3)
	return baseDir + "/" + prefix + middle + "_" + suffix + "." + fileType
}

func (r *randGen) generateDirName(baseDir string) string {
	prefix := "Eris_"
	middle := r.randomString(10)
	suffix := r.timeStampMicro()
	return baseDir + "/" + prefix + middle + "_" + suffix + ".d"
}

func (r *randGen) randomString(length int) string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		result[i] = letters[r.Intn(len(letters))]
	}
	return string(result)
}

func (r *randGen) timeStampMicro() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()/1000)
}

func (r *randGen) filename(s *state, typ *BufferType) string {
	fn := r.filenameImpl(s)
	if fn != "" && fn[len(fn)-1] == 0 {
		panic(fmt.Sprintf("zero-terminated filename: %q", fn))
	}
	if escapingFilename(fn) {
		panic(fmt.Sprintf("sandbox escaping file name %q, s.files are %v", fn, s.files))
	}
	if !typ.Varlen() {
		size := typ.Size()
		if uint64(len(fn)) < size {
			fn += string(make([]byte, size-uint64(len(fn))))
		}
		fn = fn[:size]
	} else if !typ.NoZ {
		fn += "\x00"
	}
	return fn
}

func escapingFilename(file string) bool {
	file = filepath.Clean(file)
	return len(file) >= 1 && file[0] == '/' ||
		len(file) >= 2 && file[0] == '.' && file[1] == '.'
}

func TransformDevicePathComplete(s string) (string, bool) {
	const (
		prefix    = "device_view/"
		cidLength = 64
	)

	// 1. check length
	if len(s) < len(prefix)+cidLength {
		return "merge_view/", false
	}

	// 2. check prefix
	if !strings.HasPrefix(s, prefix) {
		return s, false
	}

	// 3. cal position
	prefixLen := len(prefix)
	cidStart := prefixLen
	cidEnd := cidStart + cidLength

	// 4. extract cid(optional)
	// cid := s[cidStart:cidEnd]

	// 5. deal with the remaining part
	if len(s) == cidEnd {
		// string end
		return "merge_view/", false
	}

	if s[cidEnd] == '/' {
		if len(s) > cidEnd+1 {
			// with successor path
			remaining := s[cidEnd+1:]
			return "merge_view/" + remaining, true
		} else {
			// only slash
			return "merge_view/", false
		}
	}

	// cid without slash
	return "merge_view/", false
}

// var specialFiles = []string{"", "."}
var specialFiles = []string{""}

func (r *randGen) filenameImpl(s *state) string {
	if r.oneOf(100) {
		return specialFiles[r.Intn(len(specialFiles))]
	}
	if len(s.files) == 0 || r.oneOf(2) {
		// Generate a new name.
		dir := "."
		if r.hmcfg.DfsName == "hmdfs" {
			dir = "merge_view"
		}
		if r.oneOf(2) && len(s.files) != 0 {
			dir = r.randFromMap(s.files)
			if dir != "" && dir[len(dir)-1] == 0 {
				dir = dir[:len(dir)-1]
			}
			// Not generate filepath containing x/../b
			/*
				if r.oneOf(10) && filepath.Clean(dir)[0] != '.' {
					dir += "/.."
				}
			*/
		}
		for i := 0; ; i++ {
			//idx := rand.Intn(100)
			//f := fmt.Sprintf("%v/file%v", dir, idx)
			f := fmt.Sprintf("%v/file%v", dir, i)
			if !s.files[f] {
				return f
			}

		}
	}
	return r.randFromMap(s.files)
}

func (r *randGen) randFromMap(m map[string]bool) string {
	files := make([]string, 0, len(m))
	for f := range m {
		files = append(files, f)
	}
	sort.Strings(files)
	return files[r.Intn(len(files))]
}

func (r *randGen) randString(s *state, t *BufferType) []byte {
	if len(t.Values) != 0 {
		return []byte(t.Values[r.Intn(len(t.Values))])
	}
	if len(s.strings) != 0 && r.bin() {
		// Return an existing string.
		// TODO(dvyukov): make s.strings indexed by string SubKind.
		return []byte(r.randFromMap(s.strings))
	}
	punct := []byte{'!', '@', '#', '$', '%', '^', '&', '*', '(', ')', '-', '+', '\\',
		'/', ':', '.', ',', '-', '\'', '[', ']', '{', '}'}
	buf := new(bytes.Buffer)
	for r.nOutOf(3, 4) {
		if r.nOutOf(10, 11) {
			buf.Write([]byte{punct[r.Intn(len(punct))]})
		} else {
			buf.Write([]byte{byte(r.Intn(256))})
		}
	}
	if r.oneOf(100) == t.NoZ {
		buf.Write([]byte{0})
	}
	return buf.Bytes()
}

func (r *randGen) allocAddr(s *state, typ Type, dir Dir, size uint64, data Arg) *PointerArg {
	return MakePointerArg(typ, dir, s.ma.alloc(r, size, data.Type().Alignment()), data)
}

func (r *randGen) allocVMA(s *state, typ Type, dir Dir, numPages uint64) *PointerArg {
	page := s.va.alloc(r, numPages)
	return MakeVmaPointerArg(typ, dir, page*r.target.PageSize, numPages*r.target.PageSize)
}

func (r *randGen) createResource(s *state, res *ResourceType, dir Dir) (arg Arg, calls []*Call) {
	kind := res.Desc.Name
	// We may have no resources, but still be in createResource due to ANYRES.
	if len(r.target.resourceMap) != 0 && r.oneOf(1000) {
		// Spoof resource subkind.
		var all []string
		for kind1 := range r.target.resourceMap {
			if r.target.isCompatibleResource(res.Desc.Kind[0], kind1) {
				all = append(all, kind1)
			}
		}
		if len(all) == 0 {
			panic(fmt.Sprintf("got no spoof resources for %v in %v/%v",
				kind, r.target.OS, r.target.Arch))
		}
		sort.Strings(all)
		kind = all[r.Intn(len(all))]
	}
	// Find calls that produce the necessary resources.
	metas0 := r.target.resourceCtors[kind]
	// TODO: reduce priority of less specialized ctors.
	var metas []*Syscall
	for _, meta := range metas0 {
		if s.ct.Enabled(meta.ID) {
			metas = append(metas, meta)
		}
	}
	if len(metas) == 0 {
		return res.DefaultArg(dir), nil
	}

	// Now we have a set of candidate calls that can create the necessary resource.
	for i := 0; i < 1e3; i++ {
		// Generate one of them.
		meta := metas[r.Intn(len(metas))]
		calls := r.generateParticularCall(s, meta)
		s1 := newState(r.target, s.ct, nil)
		s1.analyze(calls[len(calls)-1])
		// Now see if we have what we want.
		var allres []*ResultArg
		for kind1, res1 := range s1.resources {
			if r.target.isCompatibleResource(kind, kind1) {
				allres = append(allres, res1...)
			}
		}
		sort.SliceStable(allres, func(i, j int) bool {
			return allres[i].Type().Name() < allres[j].Type().Name()
		})
		if len(allres) != 0 {
			// Bingo!
			arg := MakeResultArg(res, dir, allres[r.Intn(len(allres))], 0)
			return arg, calls
		}
		// Discard unsuccessful calls.
		// Note: s.ma/va have already noted allocations of the new objects
		// in discarded syscalls, ideally we should recreate state
		// by analyzing the program again.
		for _, c := range calls {
			ForeachArg(c, func(arg Arg, _ *ArgCtx) {
				if a, ok := arg.(*ResultArg); ok && a.Res != nil {
					delete(a.Res.uses, a)
				}
			})
		}
	}
	// Generally we can loop several times, e.g. when we choose a call that returns
	// the resource in an array, but then generateArg generated that array of zero length.
	// But we must succeed eventually.
	var ctors []string
	for _, meta := range metas {
		ctors = append(ctors, meta.Name)
	}
	panic(fmt.Sprintf("failed to create a resource %v with %v",
		res.Desc.Kind[0], strings.Join(ctors, ", ")))
}

func (r *randGen) generateText(kind TextKind) []byte {
	switch kind {
	case TextTarget:
		if cfg := createTargetIfuzzConfig(r.target); cfg != nil {
			return ifuzz.Generate(cfg, r.Rand)
		}
		fallthrough
	case TextArm64:
		// Just a stub, need something better.
		text := make([]byte, 50)
		for i := range text {
			text[i] = byte(r.Intn(256))
		}
		return text
	default:
		cfg := createIfuzzConfig(kind)
		return ifuzz.Generate(cfg, r.Rand)
	}
}

func (r *randGen) mutateText(kind TextKind, text []byte) []byte {
	switch kind {
	case TextTarget:
		if cfg := createTargetIfuzzConfig(r.target); cfg != nil {
			return ifuzz.Mutate(cfg, r.Rand, text)
		}
		fallthrough
	case TextArm64:
		return mutateData(r, text, 40, 60)
	default:
		cfg := createIfuzzConfig(kind)
		return ifuzz.Mutate(cfg, r.Rand, text)
	}
}

func createTargetIfuzzConfig(target *Target) *ifuzz.Config {
	cfg := &ifuzz.Config{
		Len:  10,
		Priv: false,
		Exec: true,
		MemRegions: []ifuzz.MemRegion{
			{Start: target.DataOffset, Size: target.NumPages * target.PageSize},
		},
	}
	for _, p := range target.SpecialPointers {
		cfg.MemRegions = append(cfg.MemRegions, ifuzz.MemRegion{
			Start: p & ^target.PageSize, Size: p & ^target.PageSize + target.PageSize,
		})
	}
	switch target.Arch {
	case "amd64":
		cfg.Mode = ifuzz.ModeLong64
		cfg.Arch = ifuzz.ArchX86
	case "386":
		cfg.Mode = ifuzz.ModeProt32
		cfg.Arch = ifuzz.ArchX86
	case "ppc64":
		cfg.Mode = ifuzz.ModeLong64
		cfg.Arch = ifuzz.ArchPowerPC
	default:
		return nil
	}
	return cfg
}

func createIfuzzConfig(kind TextKind) *ifuzz.Config {
	cfg := &ifuzz.Config{
		Len:  10,
		Priv: true,
		Exec: true,
		MemRegions: []ifuzz.MemRegion{
			{Start: 0 << 12, Size: 1 << 12},
			{Start: 1 << 12, Size: 1 << 12},
			{Start: 2 << 12, Size: 1 << 12},
			{Start: 3 << 12, Size: 1 << 12},
			{Start: 4 << 12, Size: 1 << 12},
			{Start: 5 << 12, Size: 1 << 12},
			{Start: 6 << 12, Size: 1 << 12},
			{Start: 7 << 12, Size: 1 << 12},
			{Start: 8 << 12, Size: 1 << 12},
			{Start: 9 << 12, Size: 1 << 12},
			{Start: 0xfec00000, Size: 0x100}, // ioapic
		},
	}
	switch kind {
	case TextX86Real:
		cfg.Mode = ifuzz.ModeReal16
		cfg.Arch = ifuzz.ArchX86
	case TextX86bit16:
		cfg.Mode = ifuzz.ModeProt16
		cfg.Arch = ifuzz.ArchX86
	case TextX86bit32:
		cfg.Mode = ifuzz.ModeProt32
		cfg.Arch = ifuzz.ArchX86
	case TextX86bit64:
		cfg.Mode = ifuzz.ModeLong64
		cfg.Arch = ifuzz.ArchX86
	case TextPpc64:
		cfg.Mode = ifuzz.ModeLong64
		cfg.Arch = ifuzz.ArchPowerPC
	default:
		panic("unknown text kind")
	}
	return cfg
}

// nOutOf returns true n out of outOf times.
func (r *randGen) nOutOf(n, outOf int) bool {
	if n <= 0 || n >= outOf {
		panic("bad probability")
	}
	v := r.Intn(outOf)
	return v < n
}

func (r *randGen) generateCall(s *state, p *Prog, insertionPoint int, sCalls *SpecialCalls,
	enableC2san bool) []*Call {
	if enableC2san && r.nOutOf(1, 5) {
		idx := 0
		switch {
		case r.nOutOf(1, 10):
			idx = sCalls.SyncId
		case r.nOutOf(4, 10):
			idx = sCalls.FdatasyncId
		default:
			idx = sCalls.FsyncId
		}
		syscall := r.target.Syscalls[idx]
		log.Logf(0, "----- C2san generateCall %d %v\n", idx, syscall.Name)
		return r.generateParticularCall(s, syscall)
	}

	biasCall := -1
	if insertionPoint > 0 {
		// Choosing the base call is based on the insertion point of the new calls sequence.
		biasCall = p.Calls[r.Intn(insertionPoint)].Meta.ID
	}
	idx := s.ct.choose(r.Rand, biasCall)
	syscall := r.target.Syscalls[idx]
	cnt := 0
	for strings.Contains(syscall.Name, "syz_failure") && cnt < 20 {
		idx = s.ct.choose(r.Rand, biasCall)
		syscall = r.target.Syscalls[idx]
		cnt++
	}
	if cnt >= 20 {
		return make([]*Call, 0)
	}
	log.Logf(0, "----- generateCall %d %v %v\n", idx, r.target.Syscalls[idx].Name, syscall.Name)
	meta := r.target.Syscalls[idx]
	return r.generateParticularCall(s, meta)
}

func (r *randGen) generateParticularCall(s *state, meta *Syscall) (calls []*Call) {
	if s == nil {
		s = newState(r.target, nil, nil) // 自动创建——消除 nil 依赖（L20）
	}
	if meta.Attrs.Disabled {
		panic(fmt.Sprintf("generating disabled call %v", meta.Name))
	}
	c := MakeCall(meta, nil)
	c.Args, calls = r.generateArgs(s, meta.Args, DirIn)
	r.target.assignSizesCall(c)
	return append(calls, c)
}

// GenerateAllSyzProg generates a program that contains all pseudo syz_ calls for testing.
func (target *Target) GenerateAllSyzProg(rs rand.Source) *Prog {
	p := &Prog{
		Target: target,
	}
	r := newRand(target, rs)
	s := newState(target, target.DefaultChoiceTable(), nil)
	handled := make(map[string]bool)
	for _, meta := range target.Syscalls {
		if !strings.HasPrefix(meta.CallName, "syz_") || handled[meta.CallName] || meta.Attrs.Disabled {
			continue
		}
		handled[meta.CallName] = true
		calls := r.generateParticularCall(s, meta)
		for _, c := range calls {
			s.analyze(c)
			p.Calls = append(p.Calls, c)
		}
	}
	if err := p.validate(); err != nil {
		panic(err)
	}
	return p
}

// DataMmapProg creates program that maps data segment.
// Also used for testing as the simplest program.
func (target *Target) DataMmapProg() *Prog {
	return &Prog{
		Target: target,
		Calls:  target.MakeDataMmap(),
	}
}

func (r *randGen) generateArgs(s *state, fields []Field, dir Dir) ([]Arg, []*Call) {
	var calls []*Call
	args := make([]Arg, len(fields))

	// Generate all args. Size args have the default value 0 for now.
	for i, field := range fields {
		arg, calls1 := r.generateArg(s, field.Type, field.Dir(dir))
		if arg == nil {
			panic(fmt.Sprintf("generated arg is nil for field '%v', fields: %+v", field.Type.Name(), fields))
		}
		args[i] = arg
		calls = append(calls, calls1...)
	}

	return args, calls
}

func (r *randGen) generateArg(s *state, typ Type, dir Dir) (arg Arg, calls []*Call) {
	return r.generateArgImpl(s, typ, dir, false)
}

func (r *randGen) generateArgImpl(s *state, typ Type, dir Dir, ignoreSpecial bool) (arg Arg, calls []*Call) {
	if dir == DirOut {
		// No need to generate something interesting for output scalar arguments.
		// But we still need to generate the argument itself so that it can be referenced
		// in subsequent calls. For the same reason we do generate pointer/array/struct
		// output arguments (their elements can be referenced in subsequent calls).
		switch typ.(type) {
		case *IntType, *FlagsType, *ConstType, *ProcType, *VmaType, *ResourceType:
			return typ.DefaultArg(dir), nil
		}
	}

	if typ.Optional() && r.oneOf(5) {
		if res, ok := typ.(*ResourceType); ok {
			v := res.Desc.Values[r.Intn(len(res.Desc.Values))]
			return MakeResultArg(typ, dir, nil, v), nil
		}
		return typ.DefaultArg(dir), nil
	}

	// Allow infinite recursion for optional pointers.
	if pt, ok := typ.(*PtrType); ok && typ.Optional() {
		switch pt.Elem.(type) {
		case *StructType, *ArrayType, *UnionType:
			name := pt.Elem.Name()
			r.recDepth[name]++
			defer func() {
				r.recDepth[name]--
				if r.recDepth[name] == 0 {
					delete(r.recDepth, name)
				}
			}()
			if r.recDepth[name] >= 3 {
				return MakeSpecialPointerArg(typ, dir, 0), nil
			}
		}
	}

	if !ignoreSpecial && dir != DirOut {
		switch typ.(type) {
		case *StructType, *UnionType:
			if gen := r.target.SpecialTypes[typ.Name()]; gen != nil {
				return gen(&Gen{r, s}, typ, dir, nil)
			}
		}
	}

	return typ.generate(r, s, dir)
}

func (a *ResourceType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	if !r.inGenerateResource {
		// Don't allow recursion for resourceCentric/createResource.
		// That can lead to generation of huge programs and may be very slow
		// (esp. if we are generating some failing attempts in createResource already).
		r.inGenerateResource = true
		defer func() { r.inGenerateResource = false }()

		if r.oneOf(4) {
			arg, calls = r.resourceCentric(s, a, dir)
			if arg != nil {
				return
			}
		}
		if r.oneOf(3) {
			arg, calls = r.createResource(s, a, dir)
			if arg != nil {
				return
			}
		}
	}
	if r.nOutOf(9, 10) {
		arg = r.existingResource(s, a, dir)
		if arg != nil {
			return
		}
	}
	special := a.SpecialValues()
	arg = MakeResultArg(a, dir, nil, special[r.Intn(len(special))])
	return
}

func (a *BufferType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	switch a.Kind {
	case BufferBlobRand, BufferBlobRange:
		sz := r.randBufLen()
		if a.Kind == BufferBlobRange {
			sz = r.randRange(a.RangeBegin, a.RangeEnd)
		}
		if dir == DirOut {
			return MakeOutDataArg(a, dir, sz), nil
		}
		data := make([]byte, sz)
		for i := range data {
			data[i] = byte(r.Intn(256))
		}
		return MakeDataArg(a, dir, data), nil
	case BufferString:
		data := r.randString(s, a)
		if dir == DirOut {
			return MakeOutDataArg(a, dir, uint64(len(data))), nil
		}
		return MakeDataArg(a, dir, data), nil
	case BufferFilename:
		if dir == DirOut {
			var sz uint64
			switch {
			case !a.Varlen():
				sz = a.Size()
			case r.nOutOf(1, 3):
				sz = r.rand(100)
			case r.nOutOf(1, 2):
				sz = 108 // UNIX_PATH_MAX
			default:
				sz = 4096 // PATH_MAX
			}
			return MakeOutDataArg(a, dir, sz), nil
		}
		if s.custData != nil {
			return MakeDataArg(a, dir, s.custData), nil
		} else {
			return MakeDataArg(a, dir, []byte(r.filename(s, a))), nil
		}
	case BufferGlob:
		return MakeDataArg(a, dir, r.randString(s, a)), nil
	case BufferText:
		if dir == DirOut {
			return MakeOutDataArg(a, dir, uint64(r.Intn(100))), nil
		}
		return MakeDataArg(a, dir, r.generateText(a.Text)), nil
	default:
		panic("unknown buffer kind")
	}
}

func (a *VmaType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	npages := r.randPageCount()
	if a.RangeBegin != 0 || a.RangeEnd != 0 {
		npages = a.RangeBegin + uint64(r.Intn(int(a.RangeEnd-a.RangeBegin+1)))
	}
	return r.allocVMA(s, a, dir, npages), nil
}

func (a *FlagsType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	return MakeConstArg(a, dir, r.flags(a.Vals, a.BitMask, 0)), nil
}

func (a *ConstType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	return MakeConstArg(a, dir, a.Val), nil
}

func (a *IntType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	bits := a.TypeBitSize()
	v := r.randInt(bits)
	switch a.Kind {
	case IntRange:
		v = r.randRangeInt(a.RangeBegin, a.RangeEnd, bits, a.Align)
	}
	return MakeConstArg(a, dir, v), nil
}

func (a *ProcType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	return MakeConstArg(a, dir, r.rand(int(a.ValuesPerProc))), nil
}

func (a *ArrayType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	var count uint64
	switch a.Kind {
	case ArrayRandLen:
		count = r.randArrayLen()
	case ArrayRangeLen:
		count = r.randRange(a.RangeBegin, a.RangeEnd)
	}
	var inner []Arg
	for i := uint64(0); i < count; i++ {
		arg1, calls1 := r.generateArg(s, a.Elem, dir)
		inner = append(inner, arg1)
		calls = append(calls, calls1...)
	}
	return MakeGroupArg(a, dir, inner), calls
}

func (a *StructType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	args, calls := r.generateArgs(s, a.Fields, dir)
	group := MakeGroupArg(a, dir, args)
	return group, calls
}

func (a *UnionType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	index := r.Intn(len(a.Fields))
	optType, optDir := a.Fields[index].Type, a.Fields[index].Dir(dir)
	opt, calls := r.generateArg(s, optType, optDir)
	return MakeUnionArg(a, dir, opt, index), calls
}

func (a *PtrType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	if r.oneOf(1000) {
		index := r.rand(len(r.target.SpecialPointers))
		return MakeSpecialPointerArg(a, dir, index), nil
	}
	inner, calls := r.generateArg(s, a.Elem, a.ElemDir)
	arg = r.allocAddr(s, a, dir, inner.Size(), inner)
	return arg, calls
}

func (a *LenType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	// Updated later in assignSizesCall.
	return MakeConstArg(a, dir, 0), nil
}

func (a *CsumType) generate(r *randGen, s *state, dir Dir) (arg Arg, calls []*Call) {
	// Filled at runtime by executor.
	return MakeConstArg(a, dir, 0), nil
}

func (r *randGen) existingResource(s *state, res *ResourceType, dir Dir) Arg {
	alltypes := make([][]*ResultArg, 0, len(s.resources))
	for _, res1 := range s.resources {
		alltypes = append(alltypes, res1)
	}
	sort.Slice(alltypes, func(i, j int) bool {
		return alltypes[i][0].Type().Name() < alltypes[j][0].Type().Name()
	})
	var allres []*ResultArg
	for _, res1 := range alltypes {
		name1 := res1[0].Type().Name()
		if r.target.isCompatibleResource(res.Desc.Name, name1) ||
			r.oneOf(50) && r.target.isCompatibleResource(res.Desc.Kind[0], name1) {
			allres = append(allres, res1...)
		}
	}
	if len(allres) == 0 {
		return nil
	}
	return MakeResultArg(res, dir, allres[r.Intn(len(allres))], 0)
}

// Finds a compatible resource with the type `t` and the calls that initialize that resource.
func (r *randGen) resourceCentric(s *state, t *ResourceType, dir Dir) (arg Arg, calls []*Call) {
	var p *Prog
	var resource *ResultArg
	for idx := range r.Perm(len(s.corpus)) {
		//tao modified
		//p = s.corpus[idx].Clone()
		p = s.corpus[idx][0].Clone()
		//tao end
		resources := getCompatibleResources(p, t.TypeName, r)
		if len(resources) > 0 {
			resource = resources[r.Intn(len(resources))]
			break
		}
	}

	// No compatible resource was found.
	if resource == nil {
		return nil, nil
	}

	// Set that stores the resources that appear in the same calls with the selected resource.
	relatedRes := map[*ResultArg]bool{resource: true}

	// Remove unrelated calls from the program.
	for idx := len(p.Calls) - 1; idx >= 0; idx-- {
		includeCall := false
		var newResources []*ResultArg
		ForeachArg(p.Calls[idx], func(arg Arg, _ *ArgCtx) {
			if a, ok := arg.(*ResultArg); ok {
				if a.Res != nil && !relatedRes[a.Res] {
					newResources = append(newResources, a.Res)
				}
				if relatedRes[a] || relatedRes[a.Res] {
					includeCall = true
				}
			}
		})
		if !includeCall {
			p.RemoveCall(idx)
		} else {
			for _, res := range newResources {
				relatedRes[res] = true
			}
		}
	}

	// Selects a biased random length of the returned calls (more calls could offer more
	// interesting programs). The values returned (n = len(calls): n, n-1, ..., 2.
	biasedLen := 2 + r.biasedRand(len(calls)-1, 10)

	// Removes the references that are not used anymore.
	for i := biasedLen; i < len(calls); i++ {
		p.RemoveCall(i)
	}

	return MakeResultArg(t, dir, resource, 0), p.Calls
}

func getCompatibleResources(p *Prog, resourceType string, r *randGen) (resources []*ResultArg) {
	for _, c := range p.Calls {
		ForeachArg(c, func(arg Arg, _ *ArgCtx) {
			// Collect only initialized resources (the ones that are already used in other calls).
			a, ok := arg.(*ResultArg)
			if !ok || len(a.uses) == 0 || a.Dir() != DirOut {
				return
			}
			if !r.target.isCompatibleResource(resourceType, a.Type().Name()) {
				return
			}
			resources = append(resources, a)
		})
	}
	return resources
}

func IsIn(set []int, num int) bool {
	for _, num1 := range set {
		if num1 == num {
			return true
		}
	}
	return false
}

// [startRange, endRange]
func (r *randGen) RandSet(startRange, endRange, setSize int) (set []int) {
	valRange := endRange - startRange + 1
	if setSize > valRange {
		panic(fmt.Sprintf("setSize > valRange: startRange %v, endRange %v, setSize %v", startRange, endRange, setSize))
	}
	for _, val := range rand.Perm(valRange) {
		if len(set) >= setSize {
			break
		}
		set = append(set, startRange+val)
	}
	sort.Ints(set)
	//log.Logf(0, "startRange %v, endRange %v, setSize %v, randVal %v", startRange, endRange, setSize, set)
	return set
}

func (r *randGen) RandSetExcept(startRange, endRange, setSize, except int) (set []int) {
	valRange := endRange - startRange + 1
	if setSize > valRange {
		panic(fmt.Sprintf("setSize > valRange: startRange %v, endRange %v, setSize %v", startRange, endRange, setSize))
	}
	for _, val := range rand.Perm(valRange) {
		if len(set) >= setSize {
			break
		}
		if startRange+val != except {
			set = append(set, startRange+val)
		}
	}
	sort.Ints(set)
	//log.Logf(0, "startRange %v, endRange %v, setSize %v, randVal %v", startRange, endRange, setSize, set)
	return set
}

func OutOfWrap(rs rand.Source, target *Target, n, outOf int) bool {
	r := newRand(target, rs)
	return r.nOutOf(n, outOf)
}

type WriteInfo struct {
	Offset uint64
	Length uint64
}

func findAvailableFdInCalls(calls []*Call) *ResultArg {
	for _, call := range calls {
		if strings.Contains(call.Meta.Name, "open") && call.Ret != nil {
			return call.Ret
		}
	}
	return nil
}

func (r *randGen) genNetDownCall(s *state, sCalls *SpecialCalls, targetNodes []int) *Call {
	meta := r.target.Syscalls[sCalls.NetDownId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	var cmdBuilder strings.Builder
	cmdBuilder.WriteString("iptables -F;iptables -X;")
	cmdBuilder.WriteString(genIptablesDropCmd(r.hmcfg.InitIp, targetNodes))
	custData := []byte(cmdBuilder.String())

	// 显式构造：按 custData 实际长度分配——不依赖 generateArgs 的随机分配
	// （原随机 filename 长度 < 命令长度——覆写后尺寸陈旧——copyin 越界覆盖，L6）。
	// 必须复用调用方传入的 state（同一 memAllocator）分配数据地址，
	// 否则独立 newState 从地址 0 重新分配，与主程序数据区重叠（L7/L9）。
	ptrType := meta.Args[0].Type.(*PtrType)
	dataArg := MakeDataArg(ptrType.Elem, DirIn, custData)
	ptrArg := r.allocAddr(s, ptrType, DirIn, dataArg.Size(), dataArg)
	c.Args = []Arg{ptrArg}
	r.target.assignSizesCall(c)
	return c
}

func (r *randGen) genNetUpCall(s *state, sCalls *SpecialCalls) *Call {
	meta := r.target.Syscalls[sCalls.NetUpId]
	calls := r.generateParticularCall(s, meta)
	calls[len(calls)-1].IsFCall = true
	return calls[len(calls)-1]
}

func genNetDelayCmd(r *randGen) []byte {
	baseMs := []int{50, 100, 200, 500}[r.Intn(4)]
	jitterMs := r.Intn(baseMs / 2)
	return []byte(fmt.Sprintf("tc qdisc add dev eth0 root netem delay %dms %dms", baseMs, jitterMs))
}

func (r *randGen) genNetDelayAddCall(s *state, sCalls *SpecialCalls) *Call {
	meta := r.target.Syscalls[sCalls.NetDelayAddId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	custData := genNetDelayCmd(r)
	c.Args, _ = r.generateArgs(s, meta.Args, DirIn)
	if len(c.Args) > 0 {
		if ptrArg, ok := c.Args[0].(*PointerArg); ok {
			if dataArg, ok := ptrArg.Res.(*DataArg); ok {
				dataArg.data = custData
			}
		}
	}
	r.target.assignSizesCall(c)
	return c
}

func (r *randGen) genNetDelayDelCall(sCalls *SpecialCalls) *Call {
	meta := r.target.Syscalls[sCalls.NetDelayDelId]
	calls := r.generateParticularCall(nil, meta)
	calls[len(calls)-1].IsFCall = true
	return calls[len(calls)-1]
}

func (r *randGen) genSyncCall(sCalls *SpecialCalls, syncIdx *uint64, nodeIdx int) *Call {
	meta := r.target.Syscalls[sCalls.SyncfailId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: *syncIdx}
	c.Args[1] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[1].Type.ref(), dir: DirIn}, Val: uint64(nodeIdx)}
	r.target.assignSizesCall(c)
	*syncIdx = *syncIdx + 1
	return c
}

func (r *randGen) genRecvCall(sCalls *SpecialCalls, syncIdx uint64) *Call {
	meta := r.target.Syscalls[sCalls.RecvId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: syncIdx}
	r.target.assignSizesCall(c)
	return c
}

func (r *randGen) genSendCall(sCalls *SpecialCalls, syncIdx *uint64) *Call {
	meta := r.target.Syscalls[sCalls.SendId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: *syncIdx}
	r.target.assignSizesCall(c)
	*syncIdx = *syncIdx + 1
	return c
}

func (r *randGen) genBarrierCall(sCalls *SpecialCalls, barrierIdx *uint64, nodeCount int) *Call {
	meta := r.target.Syscalls[sCalls.BarrierId]
	c := MakeCall(meta, nil)
	c.IsFCall = true
	c.Args = make([]Arg, len(meta.Args))
	c.Args[0] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[0].Type.ref(), dir: DirIn}, Val: *barrierIdx}
	c.Args[1] = &ConstArg{ArgCommon: ArgCommon{ref: meta.Args[1].Type.ref(), dir: DirIn}, Val: uint64(nodeCount)}
	r.target.assignSizesCall(c)
	*barrierIdx = *barrierIdx + 1
	return c
}

func (r *randGen) selectFileInOneNode(hmcfg *Hmdfs_config, localCid string) string {
	if hmcfg.FileTree != nil {
		node := hmcfg.FileTree.GetRandomFile(r.Rand, localCid)
		if node != nil {
			return node.FullPath
		}
	}
	return "merge_view/default_test_file_" + randomSuffix(r.Rand) + ".txt"
}

func (r *randGen) selectRemoteFile(hmcfg *Hmdfs_config, localCid string) string {
	if hmcfg.FileTree != nil {
		node := hmcfg.FileTree.GetRandomFileExcluding(r.Rand, localCid)
		if node != nil {
			return node.FullPath
		}
	}
	return "merge_view/default_test_file.txt"
}

func (r *randGen) generateWriteCallsWithoutNetCallForHmdfsStash(s *state, p *Prog, sCalls *SpecialCalls,
	filePath string, syncIdx *uint64, nodeIdx int, insertSync bool) ([]*Call, []WriteInfo, int, int) {

	var calls []*Call
	var writeInfos []WriteInfo

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	pwrite64meta := r.target.Syscalls[sCalls.Pwrite64Id]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, 2)
	openmode := MakeConstArg(openmodet, DirIn, 0o666)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	writefdt := pwrite64meta.Args[0].Type.(*ResourceType)
	openfd := MakeResultArg(writefdt, DirIn, copen.Ret, 0)

	syncStartIdx := -1
	syncEndIdx := -1

	for i := 0; i < 4; i++ {
		if insertSync && i == 2 {
			syncStartIdx = len(calls)
			calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
		}

		pwrite64bufptrt := pwrite64meta.Args[1].Type.(*PtrType)
		pwrite64countt := pwrite64meta.Args[2].Type.(*LenType)
		pwrite64postt := pwrite64meta.Args[3].Type.(*IntType)

		pwrite64args := make([]Arg, len(pwrite64meta.Args))

		var pwrite64sz uint64
		var pwrite64pos uint64
		fileSize := uint64(0)
		if r.hmcfg.FileTree != nil {
			node := r.hmcfg.FileTree.FindNode(filePath)
			if node != nil {
				fileSize = node.Size
			}
		}
		if fileSize > 0 && r.nOutOf(4, 5) {
			pwrite64pos = r.randRange(0, fileSize+4096)
			pwrite64sz = r.randRange(1, uint64(min(int(fileSize+4096), 8192)))
		} else {
			pwrite64sz = r.randBufLen()
			pwrite64pos = r.randInt(pwrite64postt.TypeBitSize())
		}
		pwrite64data := make([]byte, pwrite64sz)
		for j := range pwrite64data {
			pwrite64data[j] = byte(r.Intn(256))
		}

		writeInfos = append(writeInfos, WriteInfo{Offset: pwrite64pos, Length: uint64(pwrite64sz)})

		pwrite64buf := MakeDataArg(pwrite64bufptrt.Elem, DirIn, pwrite64data)
		pwrite64bufptr := r.allocAddr(s, pwrite64bufptrt, DirIn, pwrite64buf.Size(), pwrite64buf)
		pwrite64count := MakeConstArg(pwrite64countt, DirIn, uint64(pwrite64sz))
		pwrite64posarg := MakeConstArg(pwrite64postt, DirIn, pwrite64pos)

		pwrite64args[0] = openfd
		pwrite64args[1] = pwrite64bufptr
		pwrite64args[2] = pwrite64count
		pwrite64args[3] = pwrite64posarg
		cpwrite64 := MakeCall(pwrite64meta, nil)
		cpwrite64.Args = pwrite64args
		r.target.assignSizesCall(cpwrite64)
		s.analyze(cpwrite64)
		calls = append(calls, cpwrite64)
	}

	if insertSync {
		syncEndIdx = len(calls)
		calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
	}

	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, copen.Ret, 0)
	closeargs[0] = closefdarg
	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	calls = append(calls, cclose)

	return calls, writeInfos, syncStartIdx, syncEndIdx
}

func (r *randGen) generateWriteCallsWithNetCallForHmdfsStash(s *state, p *Prog, sCalls *SpecialCalls,
	filePath string, syncIdx *uint64, nodeIdx int, targetNodes []int) ([]*Call, []WriteInfo, int, int, int) {

	var calls []*Call
	var writeInfos []WriteInfo

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	writemeta := r.target.Syscalls[sCalls.WriteId]
	pwrite64meta := r.target.Syscalls[sCalls.Pwrite64Id]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, 2)
	openmode := MakeConstArg(openmodet, DirIn, 0o666)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	writefdt := writemeta.Args[0].Type.(*ResourceType)
	openfd := MakeResultArg(writefdt, DirIn, copen.Ret, 0)

	netInsertPos := r.Intn(4)

	failStartIdx := -1
	failEndIdx := -1

	for i := 0; i < 4; i++ {
		pwrite64bufptrt := pwrite64meta.Args[1].Type.(*PtrType)
		pwrite64countt := pwrite64meta.Args[2].Type.(*LenType)
		pwrite64postt := pwrite64meta.Args[3].Type.(*IntType)

		pwrite64args := make([]Arg, len(pwrite64meta.Args))

		var pwrite64sz uint64
		var pwrite64pos uint64
		fileSize := uint64(0)
		if r.hmcfg.FileTree != nil {
			node := r.hmcfg.FileTree.FindNode(filePath)
			if node != nil {
				fileSize = node.Size
			}
		}
		if fileSize > 0 && r.nOutOf(4, 5) {
			pwrite64pos = r.randRange(0, fileSize+4096)
			pwrite64sz = r.randRange(1, uint64(min(int(fileSize+4096), 8192)))
		} else {
			pwrite64sz = r.randBufLen()
			pwrite64pos = r.randInt(pwrite64postt.TypeBitSize())
		}
		pwrite64data := make([]byte, pwrite64sz)
		for j := range pwrite64data {
			pwrite64data[j] = byte(r.Intn(256))
		}

		writeInfos = append(writeInfos, WriteInfo{Offset: pwrite64pos, Length: uint64(pwrite64sz)})

		pwrite64buf := MakeDataArg(pwrite64bufptrt.Elem, DirIn, pwrite64data)
		pwrite64bufptr := r.allocAddr(s, pwrite64bufptrt, DirIn, pwrite64buf.Size(), pwrite64buf)
		pwrite64count := MakeConstArg(pwrite64countt, DirIn, uint64(pwrite64sz))
		pwrite64posarg := MakeConstArg(pwrite64postt, DirIn, pwrite64pos)

		pwrite64args[0] = openfd
		pwrite64args[1] = pwrite64bufptr
		pwrite64args[2] = pwrite64count
		pwrite64args[3] = pwrite64posarg
		cpwrite64 := MakeCall(pwrite64meta, nil)
		cpwrite64.Args = pwrite64args
		r.target.assignSizesCall(cpwrite64)
		s.analyze(cpwrite64)
		calls = append(calls, cpwrite64)

		if i == netInsertPos {
			failStartIdx = len(calls)
			calls = append(calls, r.genRecvCall(sCalls, *syncIdx))
			calls = append(calls, r.genNetDownCall(s, sCalls, targetNodes))
			calls = append(calls, r.genSendCall(sCalls, syncIdx))
			calls = append(calls, r.genRecvCall(sCalls, *syncIdx))
			calls = append(calls, r.genNetUpCall(s, sCalls))
			calls = append(calls, r.genSendCall(sCalls, syncIdx))
			failEndIdx = len(calls) - 1
		}
	}

	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, copen.Ret, 0)
	closeargs[0] = closefdarg
	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	calls = append(calls, cclose)

	return calls, writeInfos, failStartIdx, failEndIdx, netInsertPos
}

func (r *randGen) generateReadCallsForHmdfsStash(s *state, p *Prog, sCalls *SpecialCalls,
	filePath string, writeInfos []WriteInfo, syncIdx *uint64, nodeIdx int, insertSync bool, netInsertPos int) ([]*Call, int, int) {

	var calls []*Call

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	pread64meta := r.target.Syscalls[sCalls.Pread64Id]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, 0)
	openmode := MakeConstArg(openmodet, DirIn, 0o444)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	readfdt := pread64meta.Args[0].Type.(*ResourceType)
	openfd := MakeResultArg(readfdt, DirIn, copen.Ret, 0)

	syncStartIdx := -1
	syncEndIdx := -1

	pread64bufptrt := pread64meta.Args[1].Type.(*PtrType)
	pread64countt := pread64meta.Args[2].Type.(*LenType)
	pread64postt := pread64meta.Args[3].Type.(*IntType)

	for i, wi := range writeInfos {
		if insertSync && i == netInsertPos {
			syncStartIdx = len(calls)
			calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
		}

		pread64args := make([]Arg, len(pread64meta.Args))

		pread64buf := MakeOutDataArg(pread64bufptrt.Elem, DirOut, wi.Length)
		pread64bufptr := r.allocAddr(s, pread64bufptrt, DirIn, pread64buf.Size(), pread64buf)
		pread64count := MakeConstArg(pread64countt, DirIn, wi.Length)
		pread64posarg := MakeConstArg(pread64postt, DirIn, wi.Offset)

		pread64args[0] = openfd
		pread64args[1] = pread64bufptr
		pread64args[2] = pread64count
		pread64args[3] = pread64posarg
		cpread64 := MakeCall(pread64meta, nil)
		cpread64.Args = pread64args
		r.target.assignSizesCall(cpread64)
		s.analyze(cpread64)
		calls = append(calls, cpread64)

		if insertSync && i == netInsertPos {
			syncEndIdx = len(calls)
			calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
		}
	}

	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, copen.Ret, 0)
	closeargs[0] = closefdarg
	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	calls = append(calls, cclose)

	return calls, syncStartIdx, syncEndIdx
}

func (r *randGen) generateProgsForDcacheTimeout(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog
	barrierPos := -1
	barrierPosSlice := make([]int, 0)

	parentDir := r.selectRemoteDir(hmcfg, hmcfg.Cids[0])
	if parentDir == "" {
		parentDir = "merge_view"
	}

	testDir := ""
	if hmcfg.FileTree != nil && r.nOutOf(1, 2) {
		if node := hmcfg.FileTree.GetRandomEmptyDir(r.Rand, hmcfg.Cids[0]); node != nil {
			testDir = node.FullPath // useExisting 只选空目录——rmdir 必成功（L22）
		}
	}
	useExisting := testDir != ""
	if !useExisting {
		testDir = r.generateDirName(parentDir)
	}

	p0 := &Prog{Target: r.target, BarrierIdx: 0}
	if !useExisting {
		calls0 := r.generateMkdirCalls(s, sCalls, testDir)
		for _, c := range calls0 {
			p0.Calls = append(p0.Calls, c)
		}
	}

	calls1 := r.generateStatCalls(s, sCalls, testDir)
	for _, c := range calls1 {
		p0.Calls = append(p0.Calls, c)
	}

	calls2 := r.generateRmdirCalls(s, sCalls, testDir)
	calls2 = append(calls2, r.genBarrierCall(sCalls, &p0.BarrierIdx, hmcfg.Node_num))
	for _, c := range calls2 {
		p0.Calls = append(p0.Calls, c)
	}
	barrierPos = len(p0.Calls) - 1
	barrierPosSlice = append(barrierPosSlice, barrierPos)
	ps = append(ps, p0)

	for i := 1; i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target, BarrierIdx: 0}
		calls := r.generateStatCalls(s, sCalls, testDir)
		calls = append(calls, r.genBarrierCall(sCalls, &p.BarrierIdx, hmcfg.Node_num))
		barrierPos = len(calls) - 1
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}
		ps = append(ps, p)
		barrierPosSlice = append(barrierPosSlice, barrierPos)
	}

	if len(ps) > 0 {
		ps[0].GeneralBarrierPos = append(ps[0].GeneralBarrierPos, barrierPosSlice)
	}

	return ps
}

func (r *randGen) generateProgsForDcachePersistence(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	persistenceDir := hmcfg.Persistence_dir
	if persistenceDir == "" {
		return ps
	}

	ownerNode := hmcfg.Node_idx_of_persistence
	if ownerNode < 0 || ownerNode >= hmcfg.Node_num {
		return ps
	}

	var filesInDir []string
	if hmcfg.FileTree != nil {
		filesInDir = hmcfg.FileTree.GetFileEntriesUnderDir(persistenceDir)
	}

	opType := r.Intn(3)
	if len(filesInDir) == 0 {
		opType = 0
	}

	switch opType {
	case 0:
		ps = r.generatePersistenceCreateTest(s, sCalls, hmcfg, persistenceDir, ownerNode)
	case 1:
		ps = r.generatePersistenceDeleteTest(s, sCalls, hmcfg, persistenceDir, filesInDir, ownerNode)
	case 2:
		ps = r.generatePersistenceRenameTest(s, sCalls, hmcfg, persistenceDir, filesInDir, ownerNode)
	}

	return ps
}

func (r *randGen) generatePersistenceCreateTest(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, persistenceDir string, ownerNode int) []*Prog {
	ps := make([]*Prog, hmcfg.Node_num)

	newFileName := r.generateFileName(persistenceDir)

	p0 := &Prog{Target: r.target}
	calls0 := r.generateCreateFileCalls(s, sCalls, newFileName)
	for _, c := range calls0 {
		p0.Calls = append(p0.Calls, c)
	}
	ps[ownerNode] = p0

	for i := 0; i < hmcfg.Node_num; i++ {
		if i == ownerNode {
			continue
		}
		p := &Prog{Target: r.target}
		calls := r.generateStatCalls(s, sCalls, newFileName)
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}
		ps[i] = p
	}

	return ps
}

func (r *randGen) generatePersistenceDeleteTest(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, persistenceDir string, filesInDir []string, ownerNode int) []*Prog {
	ps := make([]*Prog, hmcfg.Node_num)

	if len(filesInDir) == 0 {
		return nil
	}

	targetFile := filesInDir[r.Intn(len(filesInDir))]

	p0 := &Prog{Target: r.target}
	calls0 := r.generateUnlinkCalls(s, sCalls, targetFile)
	for _, c := range calls0 {
		p0.Calls = append(p0.Calls, c)
	}
	ps[ownerNode] = p0

	for i := 0; i < hmcfg.Node_num; i++ {
		if i == ownerNode {
			continue
		}
		p := &Prog{Target: r.target}
		calls := r.generateStatCalls(s, sCalls, targetFile)
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}
		ps[i] = p
	}

	return ps
}

func (r *randGen) generatePersistenceRenameTest(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, persistenceDir string, filesInDir []string, ownerNode int) []*Prog {
	ps := make([]*Prog, hmcfg.Node_num)

	if len(filesInDir) == 0 {
		return nil
	}

	targetFile := filesInDir[r.Intn(len(filesInDir))]
	newFileName := r.generateFileName(persistenceDir)

	p0 := &Prog{Target: r.target}
	calls0 := r.generateRenameCalls(s, sCalls, targetFile, newFileName)
	for _, c := range calls0 {
		p0.Calls = append(p0.Calls, c)
	}
	ps[ownerNode] = p0

	for i := 0; i < hmcfg.Node_num; i++ {
		if i == ownerNode {
			continue
		}
		p := &Prog{Target: r.target}
		calls := r.generateStatCalls(s, sCalls, newFileName)
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}
		ps[i] = p
	}

	return ps
}

func (r *randGen) generateProgsForDropPush(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	parentDir := r.selectRemoteDir(hmcfg, hmcfg.Cids[0])
	if parentDir == "" {
		parentDir = "merge_view"
	}

	testFile := ""
	if hmcfg.FileTree != nil && r.nOutOf(1, 2) {
		candidates := hmcfg.FileTree.GetFileEntriesUnderDir(parentDir)
		if len(candidates) > 0 {
			testFile = candidates[r.Intn(len(candidates))]
		}
	}
	if testFile == "" {
		testFile = r.generateFileName(parentDir)
	}

	// dropPush 阶段链：create → stat → unlink → 验证 stat（验证 HMDFS 的
	// drop_push 失效广播：目录变更后其它节点的 stat 可见性）。ps[i] 与
	// executor i 一一对应，阶段数受 Node_num 约束，每种子保留至少一个
	// 推送验证：Node_num>=4 完整四阶段；==3 跳中间 stat 保 unlink 验证
	// （删除后陈旧可见性 bug 面更大）；==2 create+填充 stat（保 create
	// 验证）；==1 仅 create。
	p0 := &Prog{Target: r.target}
	calls0 := r.generateCreateFileCalls(s, sCalls, testFile)
	for _, c := range calls0 {
		p0.Calls = append(p0.Calls, c)
	}
	ps = append(ps, p0)

	if hmcfg.Node_num >= 4 {
		p1 := &Prog{Target: r.target}
		calls1 := r.generateStatCalls(s, sCalls, testFile)
		for _, c := range calls1 {
			p1.Calls = append(p1.Calls, c)
		}
		ps = append(ps, p1)
	}

	if hmcfg.Node_num >= 3 {
		p2 := &Prog{Target: r.target}
		calls2 := r.generateUnlinkCalls(s, sCalls, testFile)
		for _, c := range calls2 {
			p2.Calls = append(p2.Calls, c)
		}
		ps = append(ps, p2)
	}

	if hmcfg.Node_num >= 3 {
		p3 := &Prog{Target: r.target}
		calls3 := r.generateStatCalls(s, sCalls, testFile)
		for _, c := range calls3 {
			p3.Calls = append(p3.Calls, c)
		}
		ps = append(ps, p3)
	}

	for len(ps) < hmcfg.Node_num {
		p := &Prog{Target: r.target}
		calls := r.generateStatCalls(s, sCalls, testFile)
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateFileopsSetattrConcurrent(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, testDir string) []*Prog {
	var ps []*Prog

	testFile := ""
	if hmcfg.FileTree != nil && r.nOutOf(1, 2) {
		candidates := hmcfg.FileTree.GetFileEntriesUnderDir(testDir)
		if len(candidates) > 0 {
			testFile = candidates[r.Intn(len(candidates))]
		}
	}
	useExisting := testFile != ""
	if !useExisting {
		testFile = r.generateFileName(testDir)
	}

	p0 := &Prog{Target: r.target}
	if !useExisting {
		calls0 := r.generateCreateFileCalls(s, sCalls, testFile)
		for _, c := range calls0 {
			p0.Calls = append(p0.Calls, c)
		}
	}
	if len(p0.Calls) > 0 {
		ps = append(ps, p0)
	}

	writeData := make([]byte, r.randBufLen())
	for i := range writeData {
		writeData[i] = byte(r.Intn(256))
	}

	p1 := &Prog{Target: r.target}
	opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, testFile, 2)
	writecalls := r.generateWriteCallsSeq(s, sCalls, testFile, writeData, RetFd)
	closecall := r.generateCloseCall(s, sCalls, RetFd)
	p1.Calls = append(p1.Calls, opencalls...)
	p1.Calls = append(p1.Calls, writecalls...)
	p1.Calls = append(p1.Calls, closecall)
	ps = append(ps, p1)

	setattrType := r.Intn(2)
	p2 := &Prog{Target: r.target}
	switch setattrType {
	case 0:
		fileSize := uint64(0)
		if hmcfg.FileTree != nil {
			node := hmcfg.FileTree.FindNode(testFile)
			if node != nil {
				fileSize = node.Size
			}
		}
		truncCall := r.generateTruncateCallWithPath(s, sCalls, testFile, fileSize)
		p2.Calls = append(p2.Calls, truncCall)
	case 1:
		calls2 := r.generateChmodCalls(s, sCalls, testFile, 0o777)
		p2.Calls = append(p2.Calls, calls2...)
		/*case 2:
		calls2 := r.generateChownCalls(s, sCalls, testFile, 1000, 1000)
		for _, c := range calls2 {
			p2.Calls = append(p2.Calls, c)
		}*/
	}
	ps = append(ps, p2)

	for i := len(ps); i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		calls := r.generateStatCalls(s, sCalls, testFile)
		p.Calls = append(p.Calls, calls...)
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateConcurrentDirCreate(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, testDir string) []*Prog {
	var ps []*Prog
	var testfiles []string
	numCreators := 2
	if hmcfg.Node_num < 3 {
		numCreators = 1
	}

	for i := 0; i < numCreators; i++ {
		p := &Prog{Target: r.target}
		testFile := r.generateFileName(testDir)
		testfiles = append(testfiles, testFile)
		calls := r.generateCreateFileCalls(s, sCalls, testFile)
		p.Calls = append(p.Calls, calls...)
		ps = append(ps, p)
	}

	for i := numCreators; i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		for _, tf := range testfiles {
			calls := r.generateStatCalls(s, sCalls, tf)
			p.Calls = append(p.Calls, calls...)
		}
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateConcurrentDirDelete(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, testDir string) []*Prog {
	var ps []*Prog

	numDeleters := 2
	if hmcfg.Node_num < 3 {
		numDeleters = 1
	}

	var filesToDelete []string
	for i := 0; i < numDeleters; i++ {
		testFile := ""
		if hmcfg.FileTree != nil && r.nOutOf(1, 2) {
			candidates := hmcfg.FileTree.GetFileEntriesUnderDir(testDir)
			if len(candidates) > 0 {
				testFile = candidates[r.Intn(len(candidates))]
			}
		}
		useExisting := testFile != ""
		if !useExisting {
			testFile = r.generateFileName(testDir)
		}
		filesToDelete = append(filesToDelete, testFile)

		if !useExisting {
			p := &Prog{Target: r.target}
			calls := r.generateCreateFileCalls(s, sCalls, testFile)
			p.Calls = append(p.Calls, calls...)
			ps = append(ps, p)
		}
	}

	for _, file := range filesToDelete {
		//TODO: 有隐患，可能超出节点数量
		p := &Prog{Target: r.target}
		calls := r.generateUnlinkCalls(s, sCalls, file)
		p.Calls = append(p.Calls, calls...)
		ps = append(ps, p)
	}

	for i := len(ps); i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		for _, tf := range filesToDelete {
			calls := r.generateStatCalls(s, sCalls, tf)
			p.Calls = append(p.Calls, calls...)
		}
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateConcurrentDirMixed(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, testDir string) []*Prog {
	var ps []*Prog

	testFile1 := r.generateFileName(testDir)
	testFile2 := r.generateFileName(testDir)

	var testfiles []string
	testfiles = append(testfiles, testFile1, testFile2)

	p0 := &Prog{Target: r.target}
	calls0 := r.generateCreateFileCalls(s, sCalls, testFile2)
	p0.Calls = append(p0.Calls, calls0...)
	ps = append(ps, p0)

	p1 := &Prog{Target: r.target}
	calls1 := r.generateCreateFileCalls(s, sCalls, testFile1)
	p1.Calls = append(p1.Calls, calls1...)
	ps = append(ps, p1)

	p2 := &Prog{Target: r.target}
	calls2 := r.generateUnlinkCalls(s, sCalls, testFile2)
	p2.Calls = append(p2.Calls, calls2...)
	calls2 = r.generateUnlinkCalls(s, sCalls, testFile1)
	p2.Calls = append(p2.Calls, calls2...)
	ps = append(ps, p2)

	for i := len(ps); i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		for _, tf := range testfiles {
			calls := r.generateStatCalls(s, sCalls, tf)
			p.Calls = append(p.Calls, calls...)
		}
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateConcurrentInodeOps(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, testDir string) []*Prog {
	var ps []*Prog

	testFile := ""
	if hmcfg.FileTree != nil && r.nOutOf(1, 2) {
		candidates := hmcfg.FileTree.GetFileEntriesUnderDir(testDir)
		if len(candidates) > 0 {
			testFile = candidates[r.Intn(len(candidates))]
		}
	}
	useExisting := testFile != ""
	if !useExisting {
		testFile = r.generateFileName(testDir)
	}

	if !useExisting {
		p0 := &Prog{Target: r.target}
		calls0 := r.generateCreateFileCalls(s, sCalls, testFile)
		p0.Calls = append(p0.Calls, calls0...)
		ps = append(ps, p0)
	}

	for i := len(ps); i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		opType := r.Intn(3)
		switch opType {
		case 0:
			calls := r.generateStatCalls(s, sCalls, testFile)
			p.Calls = append(p.Calls, calls...)
		case 1:
			calls := r.generateChmodCalls(s, sCalls, testFile, 0o666|r.Intn(0o777+1))
			p.Calls = append(p.Calls, calls...)
		case 2:
			fileSize := uint64(0)
			if hmcfg.FileTree != nil {
				node := hmcfg.FileTree.FindNode(testFile)
				if node != nil {
					fileSize = node.Size
				}
			}
			truncCall := r.generateTruncateCallWithPath(s, sCalls, testFile, fileSize)
			p.Calls = append(p.Calls, truncCall)
			/*case 3:
			calls := r.generateChownCalls(s, sCalls, testFile, 1000+r.Intn(100), 1000+r.Intn(100))
			for _, c := range calls {
				p.Calls = append(p.Calls, c)
			}*/
		}
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateConcurrentRenameOps(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, testDir string) []*Prog {
	var ps []*Prog

	testFile1 := ""
	if hmcfg.FileTree != nil && r.nOutOf(1, 2) {
		candidates := hmcfg.FileTree.GetFileEntriesUnderDir(testDir)
		if len(candidates) > 0 {
			testFile1 = candidates[r.Intn(len(candidates))]
		}
	}
	useExisting1 := testFile1 != ""
	if !useExisting1 {
		testFile1 = r.generateFileName(testDir)
	}

	testFile2 := ""
	if hmcfg.FileTree != nil && r.nOutOf(1, 2) {
		candidates := hmcfg.FileTree.GetFileEntriesUnderDir(testDir)
		if len(candidates) > 0 {
			testFile2 = candidates[r.Intn(len(candidates))]
		}
	}
	useExisting2 := testFile2 != ""
	if !useExisting2 {
		testFile2 = r.generateFileName(testDir)
	}

	targetFile1 := r.generateFileName(testDir)
	targetFile2 := r.generateFileName(testDir)

	var testfiles []string
	testfiles = append(testfiles, testFile1, targetFile1, testFile2, targetFile2)

	if !useExisting1 {
		p0 := &Prog{Target: r.target}
		calls0 := r.generateCreateFileCalls(s, sCalls, testFile1)
		p0.Calls = append(p0.Calls, calls0...)
		ps = append(ps, p0)
	}

	if !useExisting2 {
		p1 := &Prog{Target: r.target}
		calls1 := r.generateCreateFileCalls(s, sCalls, testFile2)
		p1.Calls = append(p1.Calls, calls1...)
		ps = append(ps, p1)
	}

	p2 := &Prog{Target: r.target}
	calls2 := r.generateRenameCalls(s, sCalls, testFile1, targetFile1)
	p2.Calls = append(p2.Calls, calls2...)
	ps = append(ps, p2)

	if hmcfg.Node_num > 3 {
		p3 := &Prog{Target: r.target}
		calls3 := r.generateRenameCalls(s, sCalls, testFile2, targetFile2)
		p3.Calls = append(p3.Calls, calls3...)
		ps = append(ps, p3)
	}

	for i := len(ps); i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		for _, tf := range testfiles {
			calls := r.generateStatCalls(s, sCalls, tf)
			p.Calls = append(p.Calls, calls...)
		}
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateFromPredefinedPattern(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, seedType string) []*Prog {
	var ps []*Prog
	var writeinfos []*WriteInfo

	pattern := lcs.PredefinedPatterns.GetRandomPattern(seedType, r.Rand)
	if pattern == nil {
		return ps
	}

	if pattern.ClientCount > hmcfg.Node_num {
		return ps
	}

	basePath := ""
	if seedType == "fileops" {
		fileNode := lcs.FileTree.GetRandomFile(r.Rand, hmcfg.Cids[0])
		if fileNode == nil {
			return ps
		}
		basePath = fileNode.FullPath
	} else {
		dirNode := lcs.FileTree.GetRandomDir(r.Rand, hmcfg.Cids[0], false)
		if dirNode == nil {
			return ps
		}
		basePath = dirNode.FullPath
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

	for nodeIdx, ops := range pattern.Operations {
		if nodeIdx >= hmcfg.Node_num {
			break
		}

		p := &Prog{Target: r.target}
		cid := hmcfg.Cids[nodeIdx]

		for _, op := range ops {
			// 根调用（client 0）的创建/删除类操作预处理——并更新 basePath
			// 使后续 client 的 PathSame 共享根路径（并发冲突语义）。
			if nodeIdx == 0 {
				switch op.CallName {
				case "mkdir":
					basePath = basePath + "/mut_dir_" + randomSuffix(r.Rand)
				case "creat":
					basePath = basePath + "._creat_" + randomSuffix(r.Rand) + ".txt"
				case "rmdir":
					emptyDir := lcs.FileTree.GetRandomEmptyDir(r.Rand, cid)
					if emptyDir == nil {
						return ps
					}
					basePath = emptyDir.FullPath
				}
			}
			calls, wi, path2 := r.generateCallFromPatternOp(s, sCalls, op, basePath, cid, lcs, nil, false, pattern.OffsetRel, sharedOffset)
			for _, c := range calls {
				p.Calls = append(p.Calls, c)
			}
			writeinfos = append(writeinfos, wi...)
			if path2 != "" {
			}
		}

		if seedType == "inodeops" {
			p.IsInodeOps = true
		} else {
			p.IsFileOps = true
		}

		ps = append(ps, p)
	}

	for nodeIdx := len(pattern.Operations); nodeIdx < hmcfg.Node_num; nodeIdx++ {
		p := &Prog{Target: r.target}
		cid := hmcfg.Cids[nodeIdx]
		flags := uint64(2)
		if lcs.FileTree != nil {
			node := lcs.FileTree.FindNode(basePath)
			if node != nil && (node.Type == NodeTypeDir || node.Type == NodeTypeEmptyDir) {
				flags = r.target.GetConst("O_DIRECTORY")
			}
		}
		calls := r.generateVerificationCalls(s, sCalls, basePath, cid, seedType, writeinfos, flags)
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}

		if seedType == "inodeops" {
			p.IsInodeOps = true
		} else {
			p.IsFileOps = true
		}

		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateCallFromPatternOp(s *state, sCalls *SpecialCalls, op ConcurrentOp, basePath string, cid string, lcs *LayeredChoiceStrategy, ExistingFd *ResultArg, UseExistFd bool, offsetRel OffsetRelationType, sharedOffset uint64) ([]*Call, []*WriteInfo, string) {
	var calls []*Call
	var writeinfos []*WriteInfo

	path := basePath

	path2 := ""
	if len(op.PathArgs) > 1 {
		pathArg1 := op.PathArgs[1]
		path, path2 = lcs.GetPathsForRenameVariant(basePath, "", pathArg1.Relation, r.Rand, cid, false)
		if path == "" {
			return calls, writeinfos, path2 // 该关系无匹配——不生成该 op（S16）
		}
	} else if len(op.PathArgs) == 1 {
		pathArg := op.PathArgs[0]
		path = lcs.FileTree.GetPathByRelation(basePath, "", pathArg.Relation, r.Rand, cid, false)
		if path == "" {
			path = basePath
		}
	}

	openFlags := uint64(2)
	if lcs != nil && lcs.FileTree != nil {
		node := lcs.FileTree.FindNode(path)
		if node != nil && (node.Type == NodeTypeDir || node.Type == NodeTypeEmptyDir) {
			openFlags = r.target.GetConst("O_DIRECTORY")
		}
	}

	switch op.CallName {
	case "open":
		calls, _ = r.generateOpenCallSeq(s, sCalls, path, openFlags)
	case "close":
		if UseExistFd {
			if ExistingFd == nil {
				return calls, writeinfos, path2
			}
			closecall := r.generateCloseCall(s, sCalls, ExistingFd)
			calls = append(calls, closecall)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, openFlags)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, closecall)
		}
	case "read":
		if offsetRel != OffsetUnspecified {
			readSize := uint64(r.Intn(4096) + 1)
			offset := sharedOffset
			var fileSize uint64
			if lcs != nil {
				node := lcs.FileTree.FindNode(path)
				if node != nil {
					fileSize = node.Size
				}
			}
			if fileSize > 0 && r.nOutOf(4, 5) {
				if offsetRel == OffsetDifferent {
					offset = r.randRange(0, fileSize+4096)
				}
				if offset < fileSize {
					readSize = r.randRange(1, uint64(min(int(fileSize-offset+4096), 8192)))
				} else {
					readSize = r.randRange(1, 8192)
				}
			} else {
				if offsetRel == OffsetDifferent {
					offset = r.randInt(64)
				}
			}
			if UseExistFd {
				if ExistingFd == nil {
					return calls, writeinfos, path2
				}
				readcalls := r.generatePreadCallsSeq(s, sCalls, path, readSize, offset, ExistingFd)
				calls = append(calls, readcalls...)
			} else {
				opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, openFlags)
				readcalls := r.generatePreadCallsSeq(s, sCalls, path, readSize, offset, RetFd)
				calls = append(calls, opencalls...)
				calls = append(calls, readcalls...)
			}
		} else {
			if UseExistFd {
				if ExistingFd == nil {
					return calls, writeinfos, path2
				}
				readcalls := r.generateReadCallsSeq(s, sCalls, path, uint64(r.Intn(4096)+1), ExistingFd)
				calls = append(calls, readcalls...)
			} else {
				opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, openFlags)
				readcalls := r.generateReadCallsSeq(s, sCalls, path, uint64(r.Intn(4096)+1), RetFd)
				calls = append(calls, opencalls...)
				calls = append(calls, readcalls...)
			}
		}
	case "write":
		if offsetRel != OffsetUnspecified {
			writeLen := uint64(r.Intn(4096) + 1)
			offset := sharedOffset
			var fileSize uint64
			if lcs != nil {
				node := lcs.FileTree.FindNode(path)
				if node != nil {
					fileSize = node.Size
				}
			}
			if fileSize > 0 && r.nOutOf(4, 5) {
				if offsetRel == OffsetDifferent {
					offset = r.randRange(0, fileSize+4096)
				}
				writeLen = r.randRange(1, uint64(min(int(fileSize+4096), 8192)))
			} else {
				if offsetRel == OffsetDifferent {
					offset = r.randInt(64)
				}
			}
			data := make([]byte, writeLen)
			r.Read(data)
			writeinfos = append(writeinfos, &WriteInfo{Offset: offset, Length: writeLen})
			if UseExistFd {
				if ExistingFd == nil {
					return calls, writeinfos, path2
				}
				writecalls := r.generatePwriteCallsSeq(s, sCalls, path, data, offset, ExistingFd)
				calls = append(calls, writecalls...)
			} else {
				opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, openFlags)
				writecalls := r.generatePwriteCallsSeq(s, sCalls, path, data, offset, RetFd)
				calls = append(calls, opencalls...)
				calls = append(calls, writecalls...)
			}
		} else {
			if UseExistFd {
				if ExistingFd == nil {
					return calls, writeinfos, path2
				}
				data := make([]byte, r.Intn(4096)+1)
				r.Read(data)
				writecalls := r.generateWriteCallsSeq(s, sCalls, path, data, ExistingFd)
				calls = append(calls, writecalls...)
			} else {
				data := make([]byte, r.Intn(4096)+1)
				r.Read(data)
				opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, openFlags)
				writecalls := r.generateWriteCallsSeq(s, sCalls, path, data, RetFd)
				calls = append(calls, opencalls...)
				calls = append(calls, writecalls...)
			}
		}
	case "truncate":
		var fileSize uint64
		if lcs != nil {
			node := lcs.FileTree.FindNode(path)
			if node != nil {
				fileSize = node.Size
			}
		}
		calls = append(calls, r.generateTruncateCallWithPath(s, sCalls, path, fileSize))
	case "chmod":
		calls = append(calls, r.generateChmodCallWithPath(s, sCalls, path))
	case "mkdir":
		calls = append(calls, r.generateMkdirCallWithPath(s, sCalls, path))
	case "rmdir":
		calls = append(calls, r.generateRmdirCallWithPath(s, sCalls, path))
	case "creat":
		call, _ := r.generateCreatCallWithPath(s, sCalls, path)
		calls = append(calls, call)
	case "unlink":
		calls = append(calls, r.generateUnlinkCallWithPath(s, sCalls, path))
	case "rename":
		if path2 != "" {
			calls = append(calls, r.generateRenameCallWithPaths(s, sCalls, path, path2))
		} else {
			calls = append(calls, r.generateRenameCallWithPaths(s, sCalls, path, path+"._renamed_"+randomSuffix(r.Rand)))
		}
	case "stat":
		calls = append(calls, r.generateStatCallWithPath(s, sCalls, path))
	case "fsync":
		if UseExistFd {
			if ExistingFd == nil {
				return calls, writeinfos, path2
			}
			fsyncCall := r.generateFsyncCall(s, sCalls, ExistingFd)
			calls = append(calls, fsyncCall)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, openFlags)
			fsyncCall := r.generateFsyncCall(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, fsyncCall)
		}
	case "getdents64":
		if UseExistFd {
			if ExistingFd == nil {
				return calls, writeinfos, path2
			}
			getdents64Call := r.generateGetdents64CallWithFd(s, sCalls, ExistingFd)
			calls = append(calls, getdents64Call)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, openFlags)
			getdents64Call := r.generateGetdents64CallWithFd(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, getdents64Call)
		}
	}

	return calls, writeinfos, path2
}

func (r *randGen) generateVerificationCalls(s *state, sCalls *SpecialCalls, basePath string, cid string, seedType string, writeInfos []*WriteInfo, flags uint64) []*Call {
	var calls []*Call
	//TODO: 。。。果然没写好
	verificationType := r.Intn(3)

	switch verificationType {
	case 0:
		calls, _ = r.generateOpenCallSeq(s, sCalls, basePath, flags)
	case 1:
		opencalls1, RetFd := r.generateOpenCallSeq(s, sCalls, basePath, flags)
		readcalls1 := r.generatePreadCallsSeq(s, sCalls, basePath, uint64(r.Intn(1024)+1), uint64(r.Intn(1024)+1), RetFd)
		closecall1 := r.generateCloseCall(s, sCalls, RetFd)
		calls = append(calls, opencalls1...)
		calls = append(calls, readcalls1...)
		calls = append(calls, closecall1)
	case 2:
		calls = append(calls, r.generateStatCallWithPath(s, sCalls, basePath))
	}

	return calls
}

func (r *randGen) generateFromDistributedChoiceTable(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config, lcs *LayeredChoiceStrategy, seedType string) []*Prog {
	var ps []*Prog

	var basePath string

	var ct *ChoiceTable
	if seedType == "fileops" && lcs != nil {
		ct = lcs.FileopsChoiceTable
	} else if lcs != nil {
		ct = lcs.InodeopsChoiceTable
	}
	rootCallName := r.chooseRootCallName(ct, -1)
	if rootCallName == "" {
		return ps
	}

	if seedType == "fileops" {
		fileNode := lcs.FileTree.GetRandomFile(r.Rand, hmcfg.Cids[0])
		if fileNode == nil {
			return ps
		}
		basePath = fileNode.FullPath
	} else {
		if IsDirOnlyCall(rootCallName) {
			dirNode := lcs.FileTree.GetRandomDir(r.Rand, hmcfg.Cids[0], true)
			if dirNode == nil {
				return ps
			}
			basePath = dirNode.FullPath
		} else if IsFileOnlyCall(rootCallName) {
			fileNode := lcs.FileTree.GetRandomFile(r.Rand, hmcfg.Cids[0])
			if fileNode == nil {
				return ps
			}
			basePath = fileNode.FullPath
		} else {
			dirorfile := r.Intn(3)
			if dirorfile < 2 {
				dirNode := lcs.FileTree.GetRandomDir(r.Rand, hmcfg.Cids[0], true)
				if dirNode == nil {
					return ps
				}
				basePath = dirNode.FullPath
			} else {
				fileNode := lcs.FileTree.GetRandomFile(r.Rand, hmcfg.Cids[0])
				if fileNode == nil {
					return ps
				}
				basePath = fileNode.FullPath
			}
			/*dirNode := lcs.FileTree.GetRandomDir(r.Rand, hmcfg.Cids[0], true)
			if dirNode == nil {
				fileNode := lcs.FileTree.GetRandomFile(r.Rand, hmcfg.Cids[0])
				if fileNode == nil {
					return ps
				}
				basePath = fileNode.FullPath
			} else {
				basePath = dirNode.FullPath
			} */
		}
	}

	// 根调用创建/删除语义预处理：mkdir/creat 生成新名（创建成功路径）、
	// rmdir 选空目录（删除成功路径——ENOTEMPTY 否则必败）；变体保持
	// PathRelation 原语义（打已有路径——并发冲突测试价值）。
	if rootCallName == "mkdir" {
		basePath = basePath + "/mut_dir_" + randomSuffix(r.Rand)
	} else if rootCallName == "creat" {
		basePath = basePath + "._creat_" + randomSuffix(r.Rand) + ".txt"
	} else if rootCallName == "rmdir" {
		emptyDir := lcs.FileTree.GetRandomEmptyDir(r.Rand, hmcfg.Cids[0])
		if emptyDir == nil {
			return ps
		}
		basePath = emptyDir.FullPath
	}

	p0 := &Prog{Target: r.target}
	cid0 := hmcfg.Cids[0]
	rootFileSize := uint64(0)
	rootFlags := uint64(2)
	if lcs != nil {
		node := lcs.FileTree.FindNode(basePath)
		if node != nil {
			rootFileSize = node.Size
			if node.Type == NodeTypeDir || node.Type == NodeTypeEmptyDir {
				rootFlags = r.target.GetConst("O_DIRECTORY")
			}
		}
	}
	rootCalls := r.generateCallByName(s, sCalls, rootCallName, basePath, "", cid0, nil, false, rootFileSize, rootFlags)
	for _, c := range rootCalls {
		p0.Calls = append(p0.Calls, c)
	}

	if seedType == "inodeops" {
		p0.IsInodeOps = true
	} else {
		p0.IsFileOps = true
	}
	ps = append(ps, p0)

	for nodeIdx := 1; nodeIdx < hmcfg.Node_num; nodeIdx++ {
		p := &Prog{Target: r.target}
		cid := hmcfg.Cids[nodeIdx]

		variant := lcs.ChooseConcurrentCallFiltered(rootCallName, r.Rand, !isDirPath(lcs.FileTree, basePath))
		if variant == nil {
			continue
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
		concurrentCalls := r.generateCallByName(s, sCalls, variant.CallName, concurrentPath, concurrentPath2, cid, nil, false, concurrentFileSize, concurrentFlags)
		for _, c := range concurrentCalls {
			p.Calls = append(p.Calls, c)
		}

		if seedType == "inodeops" {
			p.IsInodeOps = true
		} else {
			p.IsFileOps = true
		}
		ps = append(ps, p)

	}

	return ps
}

func (r *randGen) chooseRootCallName(ct *ChoiceTable, bias int) string {
	if ct == nil {
		return ""
	}
	idx := ct.choose(r.Rand, bias)
	return r.target.Syscalls[idx].CallName
}

func (r *randGen) generateCallByName(s *state, sCalls *SpecialCalls, callName string, path string, path2 string, cid string, ExistingFd *ResultArg, UseExistFd bool, fileSize uint64, flags uint64) []*Call {
	var calls []*Call

	switch callName {
	case "open":
		calls, _ = r.generateOpenCallSeq(s, sCalls, path, flags)
	case "close":
		if UseExistFd {
			if ExistingFd == nil {
				return nil
			}
			closecall := r.generateCloseCall(s, sCalls, ExistingFd)
			calls = append(calls, closecall)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, flags)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, closecall)
		}
	case "read":
		readSize := uint64(r.Intn(1024) + 1)
		if fileSize > 0 && r.nOutOf(4, 5) {
			readSize = r.randRange(1, uint64(min(int(fileSize), 8192)))
		}
		if UseExistFd {
			if ExistingFd == nil {
				return nil
			}
			readcalls := r.generateReadCallsSeq(s, sCalls, path, readSize, ExistingFd)
			calls = append(calls, readcalls...)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, flags)
			readcalls := r.generateReadCallsSeq(s, sCalls, path, readSize, RetFd)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, readcalls...)
			calls = append(calls, closecall)
		}
	case "write":
		writeLen := r.Intn(1024) + 1
		if fileSize > 0 && r.nOutOf(4, 5) {
			writeLen = int(r.randRange(1, uint64(min(int(fileSize+4096), 8192))))
		}
		data := make([]byte, writeLen)
		r.Read(data)
		if UseExistFd {
			if ExistingFd == nil {
				return nil
			}
			writecalls := r.generateWriteCallsSeq(s, sCalls, path, data, ExistingFd)
			calls = append(calls, writecalls...)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, flags)
			writecalls := r.generateWriteCallsSeq(s, sCalls, path, data, RetFd)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, writecalls...)
			calls = append(calls, closecall)
		}
	case "pread64":
		if UseExistFd {
			if ExistingFd == nil {
				return nil
			}
			preadOffset := uint64(r.Intn(1024))
			preadSize := uint64(r.Intn(1024) + 1)
			if fileSize > 0 && r.nOutOf(4, 5) {
				preadOffset = r.randRange(0, fileSize+4096)
				if preadOffset < fileSize {
					preadSize = r.randRange(1, uint64(min(int(fileSize-preadOffset+4096), 8192)))
				} else {
					preadSize = r.randRange(1, 8192)
				}
			}
			preadcalls := r.generatePreadCallsSeq(s, sCalls, path, preadSize, preadOffset, ExistingFd)
			calls = append(calls, preadcalls...)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, flags)
			preadOffset := uint64(r.Intn(1024))
			preadSize := uint64(r.Intn(1024) + 1)
			if fileSize > 0 && r.nOutOf(4, 5) {
				preadOffset = r.randRange(0, fileSize+4096)
				if preadOffset < fileSize {
					preadSize = r.randRange(1, uint64(min(int(fileSize-preadOffset+4096), 8192)))
				} else {
					preadSize = r.randRange(1, 8192)
				}
			}
			preadcalls := r.generatePreadCallsSeq(s, sCalls, path, preadSize, preadOffset, RetFd)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, preadcalls...)
			calls = append(calls, closecall)
		}
	case "pwrite64":
		if UseExistFd {
			if ExistingFd == nil {
				return nil
			}
			pwriteOffset := uint64(r.Intn(1024))
			pwriteLen := r.Intn(1024) + 1
			if fileSize > 0 && r.nOutOf(4, 5) {
				pwriteOffset = r.randRange(0, fileSize+4096)
				pwriteLen = int(r.randRange(1, uint64(min(int(fileSize+4096), 8192))))
			}
			data := make([]byte, pwriteLen)
			r.Read(data)
			pwritecalls := r.generatePwriteCallsSeq(s, sCalls, path, data, pwriteOffset, ExistingFd)
			calls = append(calls, pwritecalls...)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, flags)
			pwriteOffset := uint64(r.Intn(1024))
			pwriteLen := r.Intn(1024) + 1
			if fileSize > 0 && r.nOutOf(4, 5) {
				pwriteOffset = r.randRange(0, fileSize+4096)
				pwriteLen = int(r.randRange(1, uint64(min(int(fileSize+4096), 8192))))
			}
			data := make([]byte, pwriteLen)
			r.Read(data)
			pwritecalls := r.generatePwriteCallsSeq(s, sCalls, path, data, pwriteOffset, RetFd)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			calls = append(calls, opencalls...)
			calls = append(calls, pwritecalls...)
			calls = append(calls, closecall)
		}
	case "truncate":
		calls = append(calls, r.generateTruncateCallWithPath(s, sCalls, path, fileSize))
	case "chmod":
		calls = append(calls, r.generateChmodCallWithPath(s, sCalls, path))
	case "mkdir":
		calls = append(calls, r.generateMkdirCallWithPath(s, sCalls, path))
	case "rmdir":
		calls = append(calls, r.generateRmdirCallWithPath(s, sCalls, path))
	case "creat":
		call, _ := r.generateCreatCallWithPath(s, sCalls, path)
		calls = append(calls, call)
	case "unlink":
		calls = append(calls, r.generateUnlinkCallWithPath(s, sCalls, path))
	case "rename":
		if path2 != "" {
			calls = append(calls, r.generateRenameCallWithPaths(s, sCalls, path, path2))
		} else {
			calls = append(calls, r.generateRenameCallWithPaths(s, sCalls, path, path+"._renamed_"+randomSuffix(r.Rand)))
		}
	case "stat":
		calls = append(calls, r.generateStatCallWithPath(s, sCalls, path))
	case "fsync":
		if UseExistFd {
			if ExistingFd == nil {
				return nil
			}
			fsyncCall := r.generateFsyncCall(s, sCalls, ExistingFd)
			calls = append(calls, fsyncCall)
		} else {
			openCalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, flags)
			calls = append(calls, openCalls...)
			if len(openCalls) > 0 {
				fsyncCall := r.generateFsyncCall(s, sCalls, RetFd)
				calls = append(calls, fsyncCall)
				closeCall := r.generateCloseCall(s, sCalls, RetFd)
				calls = append(calls, closeCall)
			}
		}
	case "getdents64":
		if UseExistFd {
			if ExistingFd == nil {
				return nil
			}
			getdents64Call := r.generateGetdents64CallWithFd(s, sCalls, ExistingFd)
			calls = append(calls, getdents64Call)
		} else {
			openCalls, RetFd := r.generateOpenCallSeq(s, sCalls, path, flags)
			calls = append(calls, openCalls...)
			if len(openCalls) > 0 {
				getdents64Call := r.generateGetdents64CallWithFd(s, sCalls, RetFd)
				calls = append(calls, getdents64Call)
				closeCall := r.generateCloseCall(s, sCalls, RetFd)
				calls = append(calls, closeCall)
			}
		}
	}

	return calls
}

func (r *randGen) generateOpenCallSeq(s *state, sCalls *SpecialCalls, filePath string, flags uint64) ([]*Call, *ResultArg) {
	var calls []*Call

	openmeta := r.target.Syscalls[sCalls.OpenId]
	//closemeta := r.target.Syscalls[sCalls.CloseId]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, flags)
	openmode := MakeConstArg(openmodet, DirIn, 0o666)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	return calls, copen.Ret
}

func (r *randGen) generatePreadCallsSeq(s *state, sCalls *SpecialCalls, filePath string, readSize uint64, offset uint64, RetFd *ResultArg) []*Call {
	var calls []*Call
	if RetFd == nil {
		return calls
	}
	//TODO: 感觉可以弄一套fd管理的函数和结构，比如可以标记open生成的fd在该程序下被几个调用引用，只剩close引用或者没有引用的时候就可以删了open了

	preadmeta := r.target.Syscalls[sCalls.Pread64Id]
	if preadmeta == nil {
		return calls
	}

	preadfdt := preadmeta.Args[0].Type.(*ResourceType)
	preadbufptrt := preadmeta.Args[1].Type.(*PtrType)
	preadlent := preadmeta.Args[2].Type.(*LenType)
	preadpost := preadmeta.Args[3].Type.(*IntType)
	preadargs := make([]Arg, len(preadmeta.Args))

	preadfdarg := MakeResultArg(preadfdt, DirIn, RetFd, 0)
	preadbuf := MakeOutDataArg(preadbufptrt.Elem, DirOut, readSize)
	preadbufptr := r.allocAddr(s, preadbufptrt, DirIn, preadbuf.Size(), preadbuf)
	preadlen := MakeConstArg(preadlent, DirIn, readSize)
	preadpos := MakeConstArg(preadpost, DirIn, offset)

	preadargs[0] = preadfdarg
	preadargs[1] = preadbufptr
	preadargs[2] = preadlen
	preadargs[3] = preadpos

	cpread := MakeCall(preadmeta, nil)
	cpread.Args = preadargs
	r.target.assignSizesCall(cpread)
	s.analyze(cpread)
	calls = append(calls, cpread)

	return calls
}

func (r *randGen) generatePwriteCallsSeq(s *state, sCalls *SpecialCalls, filePath string, data []byte, offset uint64, RetFd *ResultArg) []*Call {
	var calls []*Call

	if RetFd == nil {
		return calls
	}

	pwritemeta := r.target.Syscalls[sCalls.Pwrite64Id]
	if pwritemeta == nil {
		return calls
	}

	pwritefdt := pwritemeta.Args[0].Type.(*ResourceType)
	pwritebufptrt := pwritemeta.Args[1].Type.(*PtrType)
	pwritelent := pwritemeta.Args[2].Type.(*LenType)
	pwritepost := pwritemeta.Args[3].Type.(*IntType)
	pwriteargs := make([]Arg, len(pwritemeta.Args))

	pwritefdarg := MakeResultArg(pwritefdt, DirIn, RetFd, 0)
	pwritebuf := MakeDataArg(pwritebufptrt.Elem, DirIn, data)
	pwritebufptr := r.allocAddr(s, pwritebufptrt, DirIn, pwritebuf.Size(), pwritebuf)
	pwritelen := MakeConstArg(pwritelent, DirIn, uint64(len(data)))
	pwritepos := MakeConstArg(pwritepost, DirIn, offset)

	pwriteargs[0] = pwritefdarg
	pwriteargs[1] = pwritebufptr
	pwriteargs[2] = pwritelen
	pwriteargs[3] = pwritepos

	cpwrite := MakeCall(pwritemeta, nil)
	cpwrite.Args = pwriteargs
	r.target.assignSizesCall(cpwrite)
	s.analyze(cpwrite)
	calls = append(calls, cpwrite)

	return calls
}

func (r *randGen) generateCloseCall(s *state, sCalls *SpecialCalls, fdRet *ResultArg) *Call {
	closemeta := r.target.Syscalls[sCalls.CloseId]
	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, fdRet, 0)
	closeargs[0] = closefdarg

	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	return cclose
}

func (r *randGen) generateFsyncCall(s *state, sCalls *SpecialCalls, fdRet *ResultArg) *Call {
	fsyncmeta := r.target.Syscalls[sCalls.FsyncId]
	fsyncfdt := fsyncmeta.Args[0].Type.(*ResourceType)
	fsyncargs := make([]Arg, len(fsyncmeta.Args))
	fsyncfdarg := MakeResultArg(fsyncfdt, DirIn, fdRet, 0)
	fsyncargs[0] = fsyncfdarg

	cfsync := MakeCall(fsyncmeta, nil)
	cfsync.Args = fsyncargs
	r.target.assignSizesCall(cfsync)
	s.analyze(cfsync)
	return cfsync
}

func (r *randGen) generateTruncateCallWithPath(s *state, sCalls *SpecialCalls, filePath string, fileSize uint64) *Call {
	meta := r.target.Syscalls[sCalls.TruncateId]
	if meta == nil {
		return nil
	}

	ptrType := meta.Args[0].Type.(*PtrType)
	lenType := meta.Args[1].Type.(*IntType)

	var truncLen uint64
	if fileSize > 0 && r.nOutOf(4, 5) {
		truncLen = r.randRange(0, fileSize*2)
	} else {
		truncLen = uint64(r.Intn(8192))
	}

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)
	lenArg := MakeConstArg(lenType, DirIn, truncLen)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	args[1] = lenArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	s.analyze(c)
	return c
}

func (r *randGen) generateChmodCallWithPath(s *state, sCalls *SpecialCalls, filePath string) *Call {
	meta := r.target.Syscalls[sCalls.ChmodId]
	if meta == nil {
		return nil
	}

	ptrType := meta.Args[0].Type.(*PtrType)
	modeType := meta.Args[1].Type.(*FlagsType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)
	modeArg := MakeConstArg(modeType, DirIn, uint64(r.Intn(0o777)+1))

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	args[1] = modeArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	s.analyze(c)
	return c
}

func (r *randGen) generateMkdirCallWithPath(s *state, sCalls *SpecialCalls, dirPath string) *Call {
	meta := r.target.Syscalls[sCalls.MkdirId]
	if meta == nil {
		return nil
	}

	ptrType := meta.Args[0].Type.(*PtrType)
	modeType := meta.Args[1].Type.(*FlagsType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(dirPath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)
	modeArg := MakeConstArg(modeType, DirIn, 0o755)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	args[1] = modeArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	s.analyze(c)
	return c
}

func (r *randGen) generateRmdirCallWithPath(s *state, sCalls *SpecialCalls, dirPath string) *Call {
	meta := r.target.Syscalls[sCalls.RmdirId]
	if meta == nil {
		return nil
	}

	ptrType := meta.Args[0].Type.(*PtrType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(dirPath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	s.analyze(c)
	return c
}

func (r *randGen) generateCreatCallWithPath(s *state, sCalls *SpecialCalls, filePath string) (*Call, *ResultArg) {
	meta := r.target.Syscalls[sCalls.CreatId]
	if meta == nil {
		return nil, nil
	}
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
	s.analyze(c)
	return c, c.Ret
}

func (r *randGen) generateUnlinkCallWithPath(s *state, sCalls *SpecialCalls, filePath string) *Call {
	meta := r.target.Syscalls[sCalls.UnlinkId]
	if meta == nil {
		return nil
	}

	ptrType := meta.Args[0].Type.(*PtrType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	s.analyze(c)
	return c
}

func (r *randGen) generateRenameCallWithPaths(s *state, sCalls *SpecialCalls, oldPath string, newPath string) *Call {
	meta := r.target.Syscalls[sCalls.RenameId]
	if meta == nil {
		return nil
	}

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
	s.analyze(c)
	return c
}

func (r *randGen) generateStatCallWithPath(s *state, sCalls *SpecialCalls, filePath string) *Call {
	meta := r.target.Syscalls[sCalls.StatId]
	if meta == nil {
		return nil
	}

	ptrType := meta.Args[0].Type.(*PtrType)
	//statBufType := meta.Args[1].Type.(*PtrType)

	pathArg := MakeDataArg(ptrType.Elem, DirIn, []byte(filePath+"\x00"))
	pathPtr := r.allocAddr(s, ptrType, DirIn, pathArg.Size(), pathArg)

	statbufptr, _ := r.generateArg(s, meta.Args[1].Type, DirIn)
	cnt := 0
	for statbufptr.(*PointerArg).IsSpecial() && cnt < 20 {
		statbufptr, _ = r.generateArg(s, meta.Args[1].Type, DirIn)
		cnt++
	}

	args := make([]Arg, len(meta.Args))
	args[0] = pathPtr
	args[1] = statbufptr

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	s.analyze(c)
	return c
}

func (r *randGen) generateGetdents64CallWithFd(s *state, sCalls *SpecialCalls, fdRet *ResultArg) *Call {
	meta := r.target.Syscalls[sCalls.Getdents64Id]
	if meta == nil {
		return nil
	}

	fdType := meta.Args[0].Type.(*ResourceType)
	//bufPtrType := meta.Args[1].Type.(*PtrType)
	//countType := meta.Args[2].Type.(*LenType)

	fdArg := MakeResultArg(fdType, DirIn, fdRet, 0)
	/*buf := MakeOutDataArg(bufPtrType.Elem, DirOut, 8192)
	bufPtr := r.allocAddr(s, bufPtrType, DirIn, buf.Size(), buf)
	countArg := MakeConstArg(countType, DirIn, 8192)*/
	bufPtr, _ := r.generateArg(s, meta.Args[1].Type, DirIn)
	countArg, _ := r.generateArg(s, meta.Args[2].Type, DirIn)
	//TODO: 理论上getdents的参数生成也要做一些细化，但比较麻烦，先放在这吧.先用原本的生成

	args := make([]Arg, len(meta.Args))
	args[0] = fdArg
	args[1] = bufPtr
	args[2] = countArg

	c := MakeCall(meta, nil)
	c.Args = args
	r.target.assignSizesCall(c)
	s.analyze(c)
	return c
}

func (r *randGen) generateChmodCalls(s *state, sCalls *SpecialCalls, filePath string, mode int) []*Call {
	var calls []*Call

	chmodmeta := r.target.Syscalls[sCalls.ChmodId]
	chmodpathptrt := chmodmeta.Args[0].Type.(*PtrType)
	chmodmodet := chmodmeta.Args[1].Type.(*FlagsType)
	chmodargs := make([]Arg, len(chmodmeta.Args))

	chmodpath := MakeDataArg(chmodpathptrt.Elem, DirIn, []byte(filePath+"\x00"))
	chmodpathptr := r.allocAddr(s, chmodpathptrt, DirIn, chmodpath.Size(), chmodpath)
	chmodmode := MakeConstArg(chmodmodet, DirIn, uint64(mode))

	chmodargs[0] = chmodpathptr
	chmodargs[1] = chmodmode

	cchmod := MakeCall(chmodmeta, nil)
	cchmod.Args = chmodargs
	r.target.assignSizesCall(cchmod)
	s.analyze(cchmod)
	calls = append(calls, cchmod)

	return calls
}

/* func (r *randGen) generateChownCalls(s *state, sCalls *SpecialCalls, filePath string, uid, gid int) []*Call {
	var calls []*Call

	chownmeta := r.target.Syscalls[sCalls.ChownId]
	chownpathptrt := chownmeta.Args[0].Type.(*PtrType)
	chownuidt := chownmeta.Args[1].Type.(*ResourceType)
	chowngidt := chownmeta.Args[2].Type.(*ResourceType)
	chownargs := make([]Arg, len(chownmeta.Args))

	chownpath := MakeDataArg(chownpathptrt.Elem, DirIn, []byte(filePath+"\x00"))
	chownpathptr := r.allocAddr(s, chownpathptrt, DirIn, chownpath.Size(), chownpath)
	chownuid := MakeConstArg(chownuidt, DirIn, uint64(uid))
	chowngid := MakeConstArg(chowngidt, DirIn, uint64(gid))

	chownargs[0] = chownpathptr
	chownargs[1] = chownuid
	chownargs[2] = chowngid

	cchown := MakeCall(chownmeta, nil)
	cchown.Args = chownargs
	r.target.assignSizesCall(cchown)
	s.analyze(cchown)
	calls = append(calls, cchown)

	return calls
} */

func (r *randGen) generateMkdirCalls(s *state, sCalls *SpecialCalls, dirPath string) []*Call {
	var calls []*Call

	mkdirmeta := r.target.Syscalls[sCalls.MkdirId]
	mkdirpathptrt := mkdirmeta.Args[0].Type.(*PtrType)
	mkdirmodet := mkdirmeta.Args[1].Type.(*FlagsType)
	mkdirargs := make([]Arg, len(mkdirmeta.Args))

	mkdirpath := MakeDataArg(mkdirpathptrt.Elem, DirIn, []byte(dirPath+"\x00"))
	mkdirpathptr := r.allocAddr(s, mkdirpathptrt, DirIn, mkdirpath.Size(), mkdirpath)
	mkdirmode := MakeConstArg(mkdirmodet, DirIn, 0o755)
	mkdirargs[0] = mkdirpathptr
	mkdirargs[1] = mkdirmode

	cmkdir := MakeCall(mkdirmeta, nil)
	cmkdir.Args = mkdirargs
	r.target.assignSizesCall(cmkdir)
	s.analyze(cmkdir)
	calls = append(calls, cmkdir)

	return calls
}

func (r *randGen) generateRmdirCalls(s *state, sCalls *SpecialCalls, dirPath string) []*Call {
	var calls []*Call

	rmdirmeta := r.target.Syscalls[sCalls.RmdirId]
	rmdirpathptrt := rmdirmeta.Args[0].Type.(*PtrType)
	rmdirargs := make([]Arg, len(rmdirmeta.Args))

	rmdirpath := MakeDataArg(rmdirpathptrt.Elem, DirIn, []byte(dirPath+"\x00"))
	rmdirpathptr := r.allocAddr(s, rmdirpathptrt, DirIn, rmdirpath.Size(), rmdirpath)
	rmdirargs[0] = rmdirpathptr

	crmdir := MakeCall(rmdirmeta, nil)
	crmdir.Args = rmdirargs
	r.target.assignSizesCall(crmdir)
	s.analyze(crmdir)
	calls = append(calls, crmdir)

	return calls
}

func (r *randGen) generateCreateFileCalls(s *state, sCalls *SpecialCalls, filePath string) []*Call {
	var calls []*Call

	meta := r.target.Syscalls[sCalls.CreatId]
	if meta == nil {
		return nil
	}

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
	calls = append(calls, c)

	return calls
}

func (r *randGen) generateMultiFileWriteCallsForStash(s *state, p *Prog, sCalls *SpecialCalls,
	filePaths []string, syncIdx *uint64, nodeIdx int, targetNodes []int) ([]*Call, map[string][]WriteInfo, int, int, int) {

	var calls []*Call
	writeInfosMap := make(map[string][]WriteInfo)

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	pwrite64meta := r.target.Syscalls[sCalls.Pwrite64Id]

	netInsertPos := r.Intn(len(filePaths) * 2)
	failStartIdx := -1
	failEndIdx := -1

	fileFds := make(map[string]*ResultArg)
	fileRetFds := make(map[string]*ResultArg)
	for _, filePath := range filePaths {
		openptrt := openmeta.Args[0].Type.(*PtrType)
		openflagt := openmeta.Args[1].Type.(*FlagsType)
		openmodet := openmeta.Args[2].Type.(*FlagsType)
		openargs := make([]Arg, len(openmeta.Args))

		openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
		openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
		openflag := MakeConstArg(openflagt, DirIn, 2|64)
		openmode := MakeConstArg(openmodet, DirIn, 0o666)
		openargs[0] = openpathptr
		openargs[1] = openflag
		openargs[2] = openmode

		copen := MakeCall(openmeta, nil)
		copen.Args = openargs
		r.target.assignSizesCall(copen)
		s.analyze(copen)
		calls = append(calls, copen)

		writefdt := pwrite64meta.Args[0].Type.(*ResourceType)
		openfd := MakeResultArg(writefdt, DirIn, copen.Ret, 0)
		fileFds[filePath] = openfd
		fileRetFds[filePath] = copen.Ret

		writeInfosMap[filePath] = []WriteInfo{}
	}

	for writeIdx := 0; writeIdx < 2; writeIdx++ {
		for fileIdx, filePath := range filePaths {

			pwrite64bufptrt := pwrite64meta.Args[1].Type.(*PtrType)
			pwrite64countt := pwrite64meta.Args[2].Type.(*LenType)
			pwrite64postt := pwrite64meta.Args[3].Type.(*IntType)

			pwrite64args := make([]Arg, len(pwrite64meta.Args))

			var pwrite64sz uint64
			var pwrite64pos uint64
			fileSize := uint64(0)
			if r.hmcfg.FileTree != nil {
				node := r.hmcfg.FileTree.FindNode(filePath)
				if node != nil {
					fileSize = node.Size
				}
			}
			if fileSize > 0 && r.nOutOf(4, 5) {
				pwrite64pos = r.randRange(0, fileSize+4096)
				pwrite64sz = r.randRange(1, uint64(min(int(fileSize+4096), 8192)))
			} else {
				pwrite64sz = r.randBufLen()
				pwrite64pos = r.randInt(pwrite64postt.TypeBitSize())
			}
			pwrite64data := make([]byte, pwrite64sz)
			for j := range pwrite64data {
				pwrite64data[j] = byte(r.Intn(256))
			}

			writeInfosMap[filePath] = append(writeInfosMap[filePath], WriteInfo{Offset: pwrite64pos, Length: uint64(pwrite64sz)})

			pwrite64buf := MakeDataArg(pwrite64bufptrt.Elem, DirIn, pwrite64data)
			pwrite64bufptr := r.allocAddr(s, pwrite64bufptrt, DirIn, pwrite64buf.Size(), pwrite64buf)
			pwrite64count := MakeConstArg(pwrite64countt, DirIn, uint64(pwrite64sz))
			pwrite64posarg := MakeConstArg(pwrite64postt, DirIn, pwrite64pos)

			pwrite64args[0] = fileFds[filePath]
			pwrite64args[1] = pwrite64bufptr
			pwrite64args[2] = pwrite64count
			pwrite64args[3] = pwrite64posarg

			cpwrite64 := MakeCall(pwrite64meta, nil)
			cpwrite64.Args = pwrite64args
			r.target.assignSizesCall(cpwrite64)
			s.analyze(cpwrite64)
			calls = append(calls, cpwrite64)

			currentPos := writeIdx*len(filePaths) + fileIdx

			if currentPos == netInsertPos {
				failStartIdx = len(calls)
				calls = append(calls, r.genRecvCall(sCalls, *syncIdx))
				calls = append(calls, r.genNetDownCall(s, sCalls, targetNodes))
				calls = append(calls, r.genSendCall(sCalls, syncIdx))
				calls = append(calls, r.genRecvCall(sCalls, *syncIdx))
				calls = append(calls, r.genNetUpCall(s, sCalls))
				calls = append(calls, r.genSendCall(sCalls, syncIdx))
				failEndIdx = len(calls) - 1
			}
		}
	}

	for _, filePath := range filePaths {
		closefdt := closemeta.Args[0].Type.(*ResourceType)
		closeargs := make([]Arg, len(closemeta.Args))
		closefdarg := MakeResultArg(closefdt, DirIn, fileRetFds[filePath], 0)
		closeargs[0] = closefdarg
		cclose := MakeCall(closemeta, nil)
		cclose.Args = closeargs
		r.target.assignSizesCall(cclose)
		s.analyze(cclose)
		calls = append(calls, cclose)
	}

	return calls, writeInfosMap, failStartIdx, failEndIdx, netInsertPos
}

func (r *randGen) generateMultiFileReadCallsForStash(s *state, p *Prog, sCalls *SpecialCalls,
	filePaths []string, writeInfosMap map[string][]WriteInfo, syncIdx *uint64, nodeIdx int, insertSync bool, netInsertPos int) ([]*Call, int, int) {

	var calls []*Call

	syncStartIdx := -1
	syncEndIdx := -1

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	pread64meta := r.target.Syscalls[sCalls.Pread64Id]

	pread64bufptrt := pread64meta.Args[1].Type.(*PtrType)
	pread64countt := pread64meta.Args[2].Type.(*LenType)
	pread64postt := pread64meta.Args[3].Type.(*IntType)

	fileFds := make(map[string]*ResultArg)
	fileRetFds := make(map[string]*ResultArg)
	maxInfoLen := 0
	for _, filePath := range filePaths {
		openptrt := openmeta.Args[0].Type.(*PtrType)
		openflagt := openmeta.Args[1].Type.(*FlagsType)
		openmodet := openmeta.Args[2].Type.(*FlagsType)
		openargs := make([]Arg, len(openmeta.Args))

		openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
		openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
		openflag := MakeConstArg(openflagt, DirIn, 0)
		openmode := MakeConstArg(openmodet, DirIn, 0o444)
		openargs[0] = openpathptr
		openargs[1] = openflag
		openargs[2] = openmode

		copen := MakeCall(openmeta, nil)
		copen.Args = openargs
		r.target.assignSizesCall(copen)
		s.analyze(copen)
		calls = append(calls, copen)

		readfdt := pread64meta.Args[0].Type.(*ResourceType)
		openfd := MakeResultArg(readfdt, DirIn, copen.Ret, 0)
		fileFds[filePath] = openfd
		fileRetFds[filePath] = copen.Ret
	}

	for _, filePath := range filePaths {
		maxInfoLen = max(maxInfoLen, len(writeInfosMap[filePath]))
	}

	currentPos := -1 // ++ 后第一个操作 = 0——与写侧 0 基对齐（L3）
	for i := 0; i < maxInfoLen; i++ {
		for _, filePath := range filePaths {
			writeInfos := writeInfosMap[filePath]
			if i >= len(writeInfos) {
				continue
			}
			currentPos++

			if insertSync && currentPos == netInsertPos {
				syncStartIdx = len(calls)
				calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
			}
			wi := writeInfos[i]
			pread64args := make([]Arg, len(pread64meta.Args))

			pread64buf := MakeOutDataArg(pread64bufptrt.Elem, DirOut, wi.Length)
			pread64bufptr := r.allocAddr(s, pread64bufptrt, DirIn, pread64buf.Size(), pread64buf)
			pread64count := MakeConstArg(pread64countt, DirIn, wi.Length)
			pread64posarg := MakeConstArg(pread64postt, DirIn, wi.Offset)

			pread64args[0] = fileFds[filePath]
			pread64args[1] = pread64bufptr
			pread64args[2] = pread64count
			pread64args[3] = pread64posarg

			cpread64 := MakeCall(pread64meta, nil)
			cpread64.Args = pread64args
			r.target.assignSizesCall(cpread64)
			s.analyze(cpread64)
			calls = append(calls, cpread64)

			if insertSync && currentPos == netInsertPos {
				syncEndIdx = len(calls)
				calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
			}
		}
	}
	for _, filePath := range filePaths {
		closefdt := closemeta.Args[0].Type.(*ResourceType)
		closeargs := make([]Arg, len(closemeta.Args))
		closefdarg := MakeResultArg(closefdt, DirIn, fileRetFds[filePath], 0)
		closeargs[0] = closefdarg

		cclose := MakeCall(closemeta, nil)
		cclose.Args = closeargs
		r.target.assignSizesCall(cclose)
		s.analyze(cclose)
		calls = append(calls, cclose)
	}

	return calls, syncStartIdx, syncEndIdx
}

func (r *randGen) generateNormalWriteCallsForStash(s *state, sCalls *SpecialCalls, filePaths []string, syncIdx *uint64, nodeIdx int, insertSync bool, netInsertPos int) ([]*Call, map[string][]WriteInfo, int, int) {
	var calls []*Call
	writeInfosMap := make(map[string][]WriteInfo)

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	pwrite64meta := r.target.Syscalls[sCalls.Pwrite64Id]

	fileFds := make(map[string]*ResultArg)
	fileRetFds := make(map[string]*ResultArg)

	syncStartIdx := -1
	syncEndIdx := -1

	for _, filePath := range filePaths {
		openptrt := openmeta.Args[0].Type.(*PtrType)
		openflagt := openmeta.Args[1].Type.(*FlagsType)
		openmodet := openmeta.Args[2].Type.(*FlagsType)
		openargs := make([]Arg, len(openmeta.Args))

		openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
		openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
		openflag := MakeConstArg(openflagt, DirIn, 2|64)
		openmode := MakeConstArg(openmodet, DirIn, 0o666)
		openargs[0] = openpathptr
		openargs[1] = openflag
		openargs[2] = openmode

		copen := MakeCall(openmeta, nil)
		copen.Args = openargs
		r.target.assignSizesCall(copen)
		s.analyze(copen)
		calls = append(calls, copen)

		writefdt := pwrite64meta.Args[0].Type.(*ResourceType)
		openfd := MakeResultArg(writefdt, DirIn, copen.Ret, 0)
		fileFds[filePath] = openfd
		fileRetFds[filePath] = copen.Ret

		writeInfosMap[filePath] = []WriteInfo{}
	}

	currentPos := -1
	for i := 0; i < 2; i++ {
		for _, filePath := range filePaths {
			currentPos++
			if insertSync && currentPos == netInsertPos {
				syncStartIdx = len(calls)
				calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
			}

			pwrite64bufptrt := pwrite64meta.Args[1].Type.(*PtrType)
			pwrite64countt := pwrite64meta.Args[2].Type.(*LenType)
			pwrite64postt := pwrite64meta.Args[3].Type.(*IntType)

			pwrite64args := make([]Arg, len(pwrite64meta.Args))

			var pwrite64sz uint64
			var pwrite64pos uint64
			fileSize := uint64(0)
			if r.hmcfg.FileTree != nil {
				node := r.hmcfg.FileTree.FindNode(filePath)
				if node != nil {
					fileSize = node.Size
				}
			}
			if fileSize > 0 && r.nOutOf(4, 5) {
				pwrite64pos = r.randRange(0, fileSize+4096)
				pwrite64sz = r.randRange(1, uint64(min(int(fileSize+4096), 8192)))
			} else {
				pwrite64sz = r.randBufLen()
				pwrite64pos = r.randInt(pwrite64postt.TypeBitSize())
			}
			pwrite64data := make([]byte, pwrite64sz)
			for j := range pwrite64data {
				pwrite64data[j] = byte(r.Intn(256))
			}

			writeInfosMap[filePath] = append(writeInfosMap[filePath], WriteInfo{Offset: pwrite64pos, Length: uint64(pwrite64sz)})

			pwrite64buf := MakeDataArg(pwrite64bufptrt.Elem, DirIn, pwrite64data)
			pwrite64bufptr := r.allocAddr(s, pwrite64bufptrt, DirIn, pwrite64buf.Size(), pwrite64buf)
			pwrite64count := MakeConstArg(pwrite64countt, DirIn, uint64(pwrite64sz))
			pwrite64posarg := MakeConstArg(pwrite64postt, DirIn, pwrite64pos)

			pwrite64args[0] = fileFds[filePath]
			pwrite64args[1] = pwrite64bufptr
			pwrite64args[2] = pwrite64count
			pwrite64args[3] = pwrite64posarg

			cpwrite64 := MakeCall(pwrite64meta, nil)
			cpwrite64.Args = pwrite64args
			r.target.assignSizesCall(cpwrite64)
			s.analyze(cpwrite64)
			calls = append(calls, cpwrite64)
			if insertSync && currentPos == netInsertPos {
				syncEndIdx = len(calls)
				calls = append(calls, r.genSyncCall(sCalls, syncIdx, nodeIdx))
			}
		}
	}

	for _, filePath := range filePaths {
		closefdt := closemeta.Args[0].Type.(*ResourceType)
		closeargs := make([]Arg, len(closemeta.Args))
		closefdarg := MakeResultArg(closefdt, DirIn, fileRetFds[filePath], 0)
		closeargs[0] = closefdarg
		cclose := MakeCall(closemeta, nil)
		cclose.Args = closeargs
		r.target.assignSizesCall(cclose)
		s.analyze(cclose)
		calls = append(calls, cclose)
	}

	return calls, writeInfosMap, syncStartIdx, syncEndIdx
}

func (r *randGen) generateUnlinkCalls(s *state, sCalls *SpecialCalls, filePath string) []*Call {
	var calls []*Call

	unlinkmeta := r.target.Syscalls[sCalls.UnlinkId]
	unlinkpathptrt := unlinkmeta.Args[0].Type.(*PtrType)
	unlinkargs := make([]Arg, len(unlinkmeta.Args))

	unlinkpath := MakeDataArg(unlinkpathptrt.Elem, DirIn, []byte(filePath+"\x00"))
	unlinkpathptr := r.allocAddr(s, unlinkpathptrt, DirIn, unlinkpath.Size(), unlinkpath)
	unlinkargs[0] = unlinkpathptr

	cunlink := MakeCall(unlinkmeta, nil)
	cunlink.Args = unlinkargs
	r.target.assignSizesCall(cunlink)
	s.analyze(cunlink)
	calls = append(calls, cunlink)

	return calls
}

func (r *randGen) generateRenameCalls(s *state, sCalls *SpecialCalls, oldPath string, newPath string) []*Call {
	var calls []*Call

	renamemeta := r.target.Syscalls[sCalls.RenameId]
	olddirptrt := renamemeta.Args[0].Type.(*PtrType)
	newdirptrt := renamemeta.Args[1].Type.(*PtrType)
	renameargs := make([]Arg, len(renamemeta.Args))

	olddir := MakeDataArg(olddirptrt.Elem, DirIn, []byte(oldPath+"\x00"))
	olddirptr := r.allocAddr(s, olddirptrt, DirIn, olddir.Size(), olddir)
	newdir := MakeDataArg(newdirptrt.Elem, DirIn, []byte(newPath+"\x00"))
	newdirptr := r.allocAddr(s, newdirptrt, DirIn, newdir.Size(), newdir)
	renameargs[0] = olddirptr
	renameargs[1] = newdirptr

	crename := MakeCall(renamemeta, nil)
	crename.Args = renameargs
	r.target.assignSizesCall(crename)
	s.analyze(crename)
	calls = append(calls, crename)

	return calls
}

func (r *randGen) generateGetdents64Calls(s *state, sCalls *SpecialCalls, dirPath string) []*Call {
	var calls []*Call

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	getdents64meta := r.target.Syscalls[sCalls.Getdents64Id]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(dirPath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, r.target.GetConst("O_DIRECTORY"))
	openmode := MakeConstArg(openmodet, DirIn, 0)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	getdents64fdt := getdents64meta.Args[0].Type.(*ResourceType)
	getdents64bufptrt := getdents64meta.Args[1].Type.(*PtrType)
	getdents64lent := getdents64meta.Args[2].Type.(*LenType)
	getdents64buft := getdents64bufptrt.Elem.(*BufferType)
	// Fixed page-size buffer and count, same rationale as
	// genGetdents64CallWithFd (see DAG_KNOWN_ISSUES.md #19).
	const getdents64BufSize = 4096
	getdents64bufarg := MakeOutDataArg(getdents64buft, DirOut, getdents64BufSize)
	getdents64bufptr := r.allocAddr(s, getdents64bufptrt, DirIn, getdents64bufarg.Size(), getdents64bufarg)

	getdents64args := make([]Arg, len(getdents64meta.Args))
	getdents64fdarg := MakeResultArg(getdents64fdt, DirIn, copen.Ret, 0)
	getdents64lenarg := MakeConstArg(getdents64lent, DirIn, getdents64BufSize)
	getdents64args[0] = getdents64fdarg
	getdents64args[1] = getdents64bufptr
	getdents64args[2] = getdents64lenarg

	cgetdents64 := MakeCall(getdents64meta, nil)
	cgetdents64.Args = getdents64args
	r.target.assignSizesCall(cgetdents64)
	s.analyze(cgetdents64)
	calls = append(calls, cgetdents64)

	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, copen.Ret, 0)
	closeargs[0] = closefdarg

	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	calls = append(calls, cclose)

	return calls
}

func (r *randGen) generateStatCalls(s *state, sCalls *SpecialCalls, filePath string) []*Call {
	var calls []*Call

	statmeta := r.target.Syscalls[sCalls.StatId]
	statpathptrt := statmeta.Args[0].Type.(*PtrType)
	statargs := make([]Arg, len(statmeta.Args))

	statpath := MakeDataArg(statpathptrt.Elem, DirIn, []byte(filePath+"\x00"))
	statpathptr := r.allocAddr(s, statpathptrt, DirIn, statpath.Size(), statpath)

	statbufptr, _ := r.generateArg(s, statmeta.Args[1].Type, DirIn)
	cnt := 0
	for statbufptr.(*PointerArg).IsSpecial() && cnt < 20 {
		statbufptr, _ = r.generateArg(s, statmeta.Args[1].Type, DirIn)
		cnt++
	}

	statargs[0] = statpathptr
	statargs[1] = statbufptr

	cstat := MakeCall(statmeta, nil)
	cstat.Args = statargs
	r.target.assignSizesCall(cstat)
	s.analyze(cstat)
	calls = append(calls, cstat)

	return calls
}

func (r *randGen) selectRemoteDir(hmcfg *Hmdfs_config, localCid string) string {
	if hmcfg.FileTree != nil {
		node := hmcfg.FileTree.GetRandomDirExcluding(r.Rand, localCid)
		if node != nil {
			return node.FullPath
		}
	}
	return ""
}

func (r *randGen) randomSuffix() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = charset[r.Intn(len(charset))]
	}
	return string(b)
}

func (r *randGen) generateProgsForConcurrentRW(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	filePath := r.selectRemoteFile(hmcfg, hmcfg.Cids[0])
	if filePath == "" {
		filePath = "merge_view"
		filePath = r.generateFileName(filePath)
	}

	writeData := make([]byte, r.randBufLen())
	for i := range writeData {
		writeData[i] = byte(r.Intn(256))
	}

	p0 := &Prog{Target: r.target}
	opencalls0, RetFd := r.generateOpenCallSeq(s, sCalls, filePath, 2)
	writecalls0 := r.generateWriteCallsSeq(s, sCalls, filePath, writeData, RetFd)
	closecall0 := r.generateCloseCall(s, sCalls, RetFd)
	p0.Calls = append(p0.Calls, opencalls0...)
	p0.Calls = append(p0.Calls, writecalls0...)
	p0.Calls = append(p0.Calls, closecall0)
	ps = append(ps, p0)

	p1 := &Prog{Target: r.target}
	opencalls1, RetFd := r.generateOpenCallSeq(s, sCalls, filePath, 2)
	readcalls1 := r.generateReadCallsSeq(s, sCalls, filePath, uint64(len(writeData)), RetFd)
	closecall1 := r.generateCloseCall(s, sCalls, RetFd)
	p1.Calls = append(p1.Calls, opencalls1...)
	p1.Calls = append(p1.Calls, readcalls1...)
	p1.Calls = append(p1.Calls, closecall1)
	ps = append(ps, p1)

	for i := 2; i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		if i%2 == 0 {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, filePath, 2)
			writecalls := r.generateWriteCallsSeq(s, sCalls, filePath, writeData, RetFd)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			p.Calls = append(p.Calls, opencalls...)
			p.Calls = append(p.Calls, writecalls...)
			p.Calls = append(p.Calls, closecall)
		} else {
			opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, filePath, 2)
			readcalls := r.generateReadCallsSeq(s, sCalls, filePath, uint64(len(writeData)), RetFd)
			closecall := r.generateCloseCall(s, sCalls, RetFd)
			p.Calls = append(p.Calls, opencalls...)
			p.Calls = append(p.Calls, readcalls...)
			p.Calls = append(p.Calls, closecall)
		}
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateProgsForFsyncTest(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	filePath := r.selectRemoteFile(hmcfg, hmcfg.Cids[0])
	if filePath == "" {
		filePath = "merge_view"
		filePath = r.generateFileName(filePath)
	}

	writeData := make([]byte, r.randBufLen())
	for i := range writeData {
		writeData[i] = byte(r.Intn(256))
	}

	p0 := &Prog{Target: r.target}
	calls0 := r.generateWriteWithFsyncCalls(s, sCalls, filePath, writeData)
	p0.Calls = append(p0.Calls, calls0...)
	ps = append(ps, p0)

	for i := 1; i < hmcfg.Node_num; i++ {
		p := &Prog{Target: r.target}
		opencalls, RetFd := r.generateOpenCallSeq(s, sCalls, filePath, 2)
		readcalls := r.generateReadCallsSeq(s, sCalls, filePath, uint64(len(writeData)), RetFd)
		closecall := r.generateCloseCall(s, sCalls, RetFd)
		p.Calls = append(p.Calls, opencalls...)
		p.Calls = append(p.Calls, readcalls...)
		p.Calls = append(p.Calls, closecall)
		ps = append(ps, p)
	}

	return ps
}

func (r *randGen) generateProgsForAppendTest(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	filePath := r.selectRemoteFile(hmcfg, hmcfg.Cids[0])
	if filePath == "" {
		filePath = "merge_view"
		filePath = r.generateFileName(filePath)
	}

	appendLen := r.randBufLen()
	appendData := make([]byte, appendLen)
	for i := range appendData {
		appendData[i] = byte(r.Intn(256))
	}

	for i := 0; i < hmcfg.Node_num-1; i++ {
		p := &Prog{Target: r.target}
		calls := r.generateAppendCalls(s, sCalls, filePath, appendData)
		p.Calls = append(p.Calls, calls...)
		ps = append(ps, p)
	}

	pl := &Prog{Target: r.target}
	callsl := r.generateReadAppendCalls(s, sCalls, filePath, appendLen)
	pl.Calls = append(pl.Calls, callsl...)
	ps = append(ps, pl)

	return ps
}

func (r *randGen) generateWriteCallsSeq(s *state, sCalls *SpecialCalls, filePath string, data []byte, RetFd *ResultArg) []*Call {
	var calls []*Call
	writemeta := r.target.Syscalls[sCalls.WriteId]

	writefdt := writemeta.Args[0].Type.(*ResourceType)
	writebufptrt := writemeta.Args[1].Type.(*PtrType)
	writelent := writemeta.Args[2].Type.(*LenType)
	writeargs := make([]Arg, len(writemeta.Args))

	writefdarg := MakeResultArg(writefdt, DirIn, RetFd, 0)
	writebuf := MakeDataArg(writebufptrt.Elem, DirIn, data)
	writebufptr := r.allocAddr(s, writebufptrt, DirIn, writebuf.Size(), writebuf)
	writelen := MakeConstArg(writelent, DirIn, uint64(len(data)))

	writeargs[0] = writefdarg
	writeargs[1] = writebufptr
	writeargs[2] = writelen

	cwrite := MakeCall(writemeta, nil)
	cwrite.Args = writeargs
	r.target.assignSizesCall(cwrite)
	s.analyze(cwrite)
	calls = append(calls, cwrite)

	return calls
}

func (r *randGen) generateReadCallsSeq(s *state, sCalls *SpecialCalls, filePath string, readSize uint64, RetFd *ResultArg) []*Call {
	var calls []*Call
	readmeta := r.target.Syscalls[sCalls.ReadId]

	readfdt := readmeta.Args[0].Type.(*ResourceType)
	readbufptrt := readmeta.Args[1].Type.(*PtrType)
	readlent := readmeta.Args[2].Type.(*LenType)
	readargs := make([]Arg, len(readmeta.Args))

	readfdarg := MakeResultArg(readfdt, DirIn, RetFd, 0)
	readbuf := MakeOutDataArg(readbufptrt.Elem, DirOut, readSize)
	readbufptr := r.allocAddr(s, readbufptrt, DirIn, readbuf.Size(), readbuf)
	readlen := MakeConstArg(readlent, DirIn, readSize)

	readargs[0] = readfdarg
	readargs[1] = readbufptr
	readargs[2] = readlen

	cread := MakeCall(readmeta, nil)
	cread.Args = readargs
	r.target.assignSizesCall(cread)
	s.analyze(cread)
	calls = append(calls, cread)

	return calls
}

func (r *randGen) generateWriteWithFsyncCalls(s *state, sCalls *SpecialCalls, filePath string, data []byte) []*Call {
	var calls []*Call

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	writemeta := r.target.Syscalls[sCalls.WriteId]
	fsyncmeta := r.target.Syscalls[sCalls.FsyncId]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, 2|64)
	openmode := MakeConstArg(openmodet, DirIn, 0o666)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	writefdt := writemeta.Args[0].Type.(*ResourceType)
	writebufptrt := writemeta.Args[1].Type.(*PtrType)
	writelent := writemeta.Args[2].Type.(*LenType)
	writeargs := make([]Arg, len(writemeta.Args))

	writefdarg := MakeResultArg(writefdt, DirIn, copen.Ret, 0)
	writebuf := MakeDataArg(writebufptrt.Elem, DirIn, data)
	writebufptr := r.allocAddr(s, writebufptrt, DirIn, writebuf.Size(), writebuf)
	writelen := MakeConstArg(writelent, DirIn, uint64(len(data)))

	writeargs[0] = writefdarg
	writeargs[1] = writebufptr
	writeargs[2] = writelen

	cwrite := MakeCall(writemeta, nil)
	cwrite.Args = writeargs
	r.target.assignSizesCall(cwrite)
	s.analyze(cwrite)
	calls = append(calls, cwrite)

	fsyncfdt := fsyncmeta.Args[0].Type.(*ResourceType)
	fsyncargs := make([]Arg, len(fsyncmeta.Args))
	fsyncfdarg := MakeResultArg(fsyncfdt, DirIn, copen.Ret, 0)
	fsyncargs[0] = fsyncfdarg

	cfsync := MakeCall(fsyncmeta, nil)
	cfsync.Args = fsyncargs
	r.target.assignSizesCall(cfsync)
	s.analyze(cfsync)
	calls = append(calls, cfsync)

	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, copen.Ret, 0)
	closeargs[0] = closefdarg

	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	calls = append(calls, cclose)

	return calls
}

func (r *randGen) generateAppendCalls(s *state, sCalls *SpecialCalls, filePath string, data []byte) []*Call {
	var calls []*Call

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	writemeta := r.target.Syscalls[sCalls.WriteId]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, 2|64|1024)
	openmode := MakeConstArg(openmodet, DirIn, 0o666)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	writefdt := writemeta.Args[0].Type.(*ResourceType)
	writebufptrt := writemeta.Args[1].Type.(*PtrType)
	writelent := writemeta.Args[2].Type.(*LenType)
	writeargs := make([]Arg, len(writemeta.Args))

	writefdarg := MakeResultArg(writefdt, DirIn, copen.Ret, 0)
	writebuf := MakeDataArg(writebufptrt.Elem, DirIn, data)
	writebufptr := r.allocAddr(s, writebufptrt, DirIn, writebuf.Size(), writebuf)
	writelen := MakeConstArg(writelent, DirIn, uint64(len(data)))

	writeargs[0] = writefdarg
	writeargs[1] = writebufptr
	writeargs[2] = writelen

	cwrite := MakeCall(writemeta, nil)
	cwrite.Args = writeargs
	r.target.assignSizesCall(cwrite)
	s.analyze(cwrite)
	calls = append(calls, cwrite)

	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, copen.Ret, 0)
	closeargs[0] = closefdarg

	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	calls = append(calls, cclose)

	return calls
}

func (r *randGen) generateReadAppendCalls(s *state, sCalls *SpecialCalls, filePath string, offset uint64) []*Call {
	var calls []*Call

	openmeta := r.target.Syscalls[sCalls.OpenId]
	closemeta := r.target.Syscalls[sCalls.CloseId]
	readmeta := r.target.Syscalls[sCalls.ReadId]
	lseekmeta := r.target.Syscalls[sCalls.LseekId]

	openptrt := openmeta.Args[0].Type.(*PtrType)
	openflagt := openmeta.Args[1].Type.(*FlagsType)
	openmodet := openmeta.Args[2].Type.(*FlagsType)
	openargs := make([]Arg, len(openmeta.Args))

	openpath := MakeDataArg(openptrt.Elem, DirIn, []byte(filePath+"\x00"))
	openpathptr := r.allocAddr(s, openptrt, DirIn, openpath.Size(), openpath)
	openflag := MakeConstArg(openflagt, DirIn, 0)
	openmode := MakeConstArg(openmodet, DirIn, 0o666)
	openargs[0] = openpathptr
	openargs[1] = openflag
	openargs[2] = openmode

	copen := MakeCall(openmeta, nil)
	copen.Args = openargs
	r.target.assignSizesCall(copen)
	s.analyze(copen)
	calls = append(calls, copen)

	lseekfdt := lseekmeta.Args[0].Type.(*ResourceType)
	lseekofft := lseekmeta.Args[1].Type.(*IntType)
	lseekwhencet := lseekmeta.Args[2].Type.(*FlagsType)
	lseekargs := make([]Arg, len(lseekmeta.Args))

	lseekfdarg := MakeResultArg(lseekfdt, DirIn, copen.Ret, 0)
	negaoff := ^offset + 1
	lseekoff := MakeConstArg(lseekofft, DirIn, negaoff)
	lseekwhence := MakeConstArg(lseekwhencet, DirIn, 2)

	lseekargs[0] = lseekfdarg
	lseekargs[1] = lseekoff
	lseekargs[2] = lseekwhence

	clseek := MakeCall(lseekmeta, nil)
	clseek.Args = lseekargs
	r.target.assignSizesCall(clseek)
	s.analyze(clseek)
	calls = append(calls, clseek)

	readfdt := readmeta.Args[0].Type.(*ResourceType)
	readbufptrt := readmeta.Args[1].Type.(*PtrType)
	readlent := readmeta.Args[2].Type.(*LenType)
	readargs := make([]Arg, len(readmeta.Args))

	readfdarg := MakeResultArg(readfdt, DirIn, copen.Ret, 0)
	readbuf := MakeOutDataArg(readbufptrt.Elem, DirOut, offset)
	readbufptr := r.allocAddr(s, readbufptrt, DirIn, readbuf.Size(), readbuf)
	readlen := MakeConstArg(readlent, DirIn, offset)

	readargs[0] = readfdarg
	readargs[1] = readbufptr
	readargs[2] = readlen

	cread := MakeCall(readmeta, nil)
	cread.Args = readargs
	r.target.assignSizesCall(cread)
	s.analyze(cread)
	calls = append(calls, cread)

	closefdt := closemeta.Args[0].Type.(*ResourceType)
	closeargs := make([]Arg, len(closemeta.Args))
	closefdarg := MakeResultArg(closefdt, DirIn, copen.Ret, 0)
	closeargs[0] = closefdarg

	cclose := MakeCall(closemeta, nil)
	cclose.Args = closeargs
	r.target.assignSizesCall(cclose)
	s.analyze(cclose)
	calls = append(calls, cclose)

	return calls
}
