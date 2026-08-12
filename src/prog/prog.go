// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"fmt"
	"monarch/pkg/log"
	"reflect"
	"strings"
	"syscall"
)

type ProgSlice []*Prog

type Conn struct {
	From int
	To   int
}

type Prog struct {
	Target   *Target
	Calls    []*Call
	Comments []string
	//IsForSrv     bool
	HasCrashFail      bool
	HasNetFail        bool
	IsStash           bool
	IsDCache          bool
	IsFileOps         bool
	IsInodeOps        bool
	SyncIdx           uint64
	BarrierIdx        uint64
	SrvFailPos        [][]int
	SrvFailOrder      []int
	GeneralFailPos    [][]int
	GeneralBarrierPos [][]int
	C2test            bool
	//SrvClusters	 [][]int
	//ConnClusters [][]Conn
	//InsertedRebootFailure bool
	//InsertedNetFailure bool
}

type FileMetadata struct {
	Stime       uint64
	Etime       uint64
	Retv        int64
	StatMd      syscall.Stat_t
	Xattr       map[string]string
	Checksum    uint32
	SymlinkPath string
	Dents       string
	ProcId      int
}

// HmdfsTraceEvent is a per-function-return event captured by the eBPF
// kretprobe tracer on hmdfs merge-view VFS operations.
// Fields match the C structure layout written by hmdfs_trace.cc.
type HmdfsTraceEvent struct {
	Timestamp uint64
	FuncID    uint32
	Ret       int32
	Ino       uint64
	Args      [6]uint64
	ProgIdx   int // VM/prog index that produced this event
	Off       uint64
}

// HmdfsFuncName maps BPF func_id constants to human-readable operation names.
var HmdfsFuncName = map[uint32]string{
	0:  "MKDIR",
	1:  "CREATE",
	2:  "RMDIR",
	3:  "UNLINK",
	4:  "RENAME",
	5:  "WRITE",
	6:  "READ",
	7:  "FILE_OPEN",
	8:  "DIR_OPEN",
	9:  "GETATTR",
	10: "SETATTR",
	11: "FSYNC",
	12: "RELEASE",
	13: "LOOKUP",
	14: "ITERATE",
	15: "WRITEPAGE_CB",
}

// These properties are parsed and serialized according to the tag and the type
// of the corresponding fields.
// IMPORTANT: keep the exact values of "key" tag for existing props unchanged,
// otherwise the backwards compatibility would be broken.
type CallProps struct {
	FailNth int `key:"fail_nth"`
}

type SpecialCalls struct {
	RecvId        int
	SendId        int
	BarrierId     int
	SyncfailId    int
	UpId          int
	DownId        int
	NetUpId       int
	NetDownId     int
	NetDelayAddId int
	NetDelayDelId int
	SyncId        int
	FsyncId       int
	LseekId       int
	ReadId        int
	ReadvId       int
	// PreadId     int
	PreadvId  int
	Pread64Id int
	WriteId   int
	WritevId  int
	// PwriteId    int
	PwritevId    int
	Pwrite64Id   int
	OpenId       int
	CloseId      int
	MkdirId      int
	RmdirId      int
	Getdents64Id int
	CreatId      int
	UnlinkId     int
	RenameId     int
	ChmodId      int
	//ChownId     int
	TruncateId  int
	StatId      int
	FdatasyncId int
	NanosleepId int
	CrashClient int
	CrashServer int
}

type Call struct {
	Meta    *Syscall
	Args    []Arg
	Ret     *ResultArg
	Props   CallProps
	Comment string
	//Conns   []Conn
	IsFCall   bool
	CheckInfo *FileMetadata
	ProcId    int
	/*
	   Timestamp1 int
	   Timestamp2 int
	   Uid        int
	   Datasum    int
	   //Stat
	   Attr       string
	*/
	Argstr   string
	Argbytes [6][]byte
}

func MakeCall(meta *Syscall, args []Arg) *Call {
	return &Call{
		Meta: meta,
		Args: args,
		Ret:  MakeReturnArg(meta.Ret),
	}
}

type FdInfo struct {
	Fd              *ResultArg
	FilePath        string
	OpenCall        *Call
	OpenIdx         int
	CloseIdx        int // -1 = not closed
	ReadCount       int
	WriteCount      int
	FsyncCount      int
	Getdents64Count int
	OtherCount      int
	TotalUses       int
	IsOpenDir       bool
}

func isReadCall(c *Call) bool {
	return strings.Contains(c.Meta.Name, "read") || strings.Contains(c.Meta.Name, "pread64")
}

func isWriteCall(c *Call) bool {
	return strings.Contains(c.Meta.Name, "write") || strings.Contains(c.Meta.Name, "pwrite64")
}

func isFsyncCall(c *Call) bool {
	return strings.Contains(c.Meta.Name, "fsync") || strings.Contains(c.Meta.Name, "fdatasync")
}

func AnalyzeProgFds(p *Prog) []FdInfo {
	type fdKey struct {
		fd *ResultArg
	}
	fdMap := make(map[*ResultArg]*FdInfo)
	var result []FdInfo

	// First pass: find all open/creat calls
	for i, call := range p.Calls {
		if !strings.Contains(call.Meta.Name, "open") && !strings.Contains(call.Meta.Name, "creat") {
			continue
		}
		if call.Ret == nil {
			continue
		}
		path := extractPathFromCall(call)
		result = append(result, FdInfo{
			Fd:       call.Ret,
			FilePath: path,
			OpenCall: call,
			OpenIdx:  i,
			CloseIdx: -1,
		})
		info := &result[len(result)-1]
		if len(call.Args) >= 2 {
			if flagArg, ok := call.Args[1].(*ConstArg); ok {
				info.IsOpenDir = (flagArg.Val & 0x10000) != 0
			}
		}
		fdMap[call.Ret] = info
	}

	// Second pass: track fd usage
	for i, call := range p.Calls {
		if len(call.Args) < 1 {
			continue
		}
		resArg, ok := call.Args[0].(*ResultArg)
		if !ok || resArg.Res == nil {
			continue
		}
		info, found := fdMap[resArg.Res]
		if !found {
			continue
		}
		switch {
		case strings.Contains(call.Meta.Name, "close"):
			info.CloseIdx = i
			info.TotalUses++
		case isReadCall(call):
			info.ReadCount++
			info.TotalUses++
		case isWriteCall(call):
			info.WriteCount++
			info.TotalUses++
		case isFsyncCall(call):
			info.FsyncCount++
			info.TotalUses++
		case strings.Contains(call.Meta.Name, "getdents64"):
			info.Getdents64Count++
			info.TotalUses++
		default:
			info.OtherCount++
			info.TotalUses++
		}
	}

	return result
}

type Arg interface {
	Type() Type
	Dir() Dir
	Size() uint64

	validate(ctx *validCtx) error
	serialize(ctx *serializer)
}

type ArgCommon struct {
	ref Ref
	dir Dir
}

/*
func GetSyncId(call *Call) uint64 {
	return call.Args[0].(*ConstArg).Val
}
*/

func (arg ArgCommon) Type() Type {
	if arg.ref == 0 {
		panic("broken type ref")
	}
	return typeRefs.Load().([]Type)[arg.ref]
}

func (arg *ArgCommon) Dir() Dir {
	return arg.dir
}

// Used for ConstType, IntType, FlagsType, LenType, ProcType and CsumType.
type ConstArg struct {
	ArgCommon
	Val uint64
}

func MakeConstArg(t Type, dir Dir, v uint64) *ConstArg {
	return &ConstArg{ArgCommon: ArgCommon{ref: t.ref(), dir: dir}, Val: v}
}

func (arg *ConstArg) Size() uint64 {
	return arg.Type().Size()
}

// Value returns value and pid stride.
func (arg *ConstArg) Value() (uint64, uint64) {
	switch typ := (*arg).Type().(type) {
	case *IntType:
		return arg.Val, 0
	case *ConstType:
		return arg.Val, 0
	case *FlagsType:
		return arg.Val, 0
	case *LenType:
		return arg.Val, 0
	case *ResourceType:
		return arg.Val, 0
	case *CsumType:
		// Checksums are computed dynamically in executor.
		return 0, 0
	case *ProcType:
		if arg.Val == procDefaultValue {
			return 0, 0
		}
		return typ.ValuesStart + arg.Val, typ.ValuesPerProc
	default:
		panic(fmt.Sprintf("unknown ConstArg type %#v", typ))
	}
}

// Used for PtrType and VmaType.
type PointerArg struct {
	ArgCommon
	Address uint64
	VmaSize uint64 // size of the referenced region for vma args
	Res     Arg    // pointee (nil for vma)
}

func MakePointerArg(t Type, dir Dir, addr uint64, data Arg) *PointerArg {
	if data == nil {
		panic("nil pointer data arg")
	}
	return &PointerArg{
		ArgCommon: ArgCommon{ref: t.ref(), dir: DirIn}, // pointers are always in
		Address:   addr,
		Res:       data,
	}
}

func MakeVmaPointerArg(t Type, dir Dir, addr, size uint64) *PointerArg {
	if addr%1024 != 0 {
		panic("unaligned vma address")
	}
	return &PointerArg{
		ArgCommon: ArgCommon{ref: t.ref(), dir: dir},
		Address:   addr,
		VmaSize:   size,
	}
}

func MakeSpecialPointerArg(t Type, dir Dir, index uint64) *PointerArg {
	if index >= maxSpecialPointers {
		panic("bad special pointer index")
	}
	if _, ok := t.(*PtrType); ok {
		dir = DirIn // pointers are always in
	}
	return &PointerArg{
		ArgCommon: ArgCommon{ref: t.ref(), dir: dir},
		Address:   -index,
	}
}

func (arg *PointerArg) Size() uint64 {
	return arg.Type().Size()
}

func (arg *PointerArg) IsSpecial() bool {
	return arg.VmaSize == 0 && arg.Res == nil && -arg.Address < maxSpecialPointers
}

func (target *Target) PhysicalAddr(arg *PointerArg) uint64 {
	if arg.IsSpecial() {
		return target.SpecialPointers[-arg.Address]
	}
	return target.DataOffset + arg.Address
}

// Used for BufferType.
type DataArg struct {
	ArgCommon
	data []byte // for in/inout args
	size uint64 // for out Args
}

func MakeDataArg(t Type, dir Dir, data []byte) *DataArg {
	if dir == DirOut {
		panic("non-empty output data arg")
	}
	return &DataArg{ArgCommon: ArgCommon{ref: t.ref(), dir: dir}, data: append([]byte{}, data...)}
}

func MakeOutDataArg(t Type, dir Dir, size uint64) *DataArg {
	if dir != DirOut {
		panic("empty input data arg")
	}
	return &DataArg{ArgCommon: ArgCommon{ref: t.ref(), dir: dir}, size: size}
}

func (arg *DataArg) Size() uint64 {
	if len(arg.data) != 0 {
		return uint64(len(arg.data))
	}
	return arg.size
}

func (arg *DataArg) Data() []byte {
	if arg.Dir() == DirOut {
		panic("getting data of output data arg")
	}
	return arg.data
}

func (arg *DataArg) SetData(data []byte) {
	if arg.Dir() == DirOut {
		panic("setting data of output data arg")
	}
	arg.data = append([]byte{}, data...)
}

// Used for StructType and ArrayType.
// Logical group of args (struct or array).
type GroupArg struct {
	ArgCommon
	Inner []Arg
}

func MakeGroupArg(t Type, dir Dir, inner []Arg) *GroupArg {
	return &GroupArg{ArgCommon: ArgCommon{ref: t.ref(), dir: dir}, Inner: inner}
}

func (arg *GroupArg) Size() uint64 {
	typ0 := arg.Type()
	if !typ0.Varlen() {
		return typ0.Size()
	}
	switch typ := typ0.(type) {
	case *StructType:
		var size uint64
		for _, fld := range arg.Inner {
			size += fld.Size()
		}
		if typ.AlignAttr != 0 && size%typ.AlignAttr != 0 {
			size += typ.AlignAttr - size%typ.AlignAttr
		}
		return size
	case *ArrayType:
		var size uint64
		for _, elem := range arg.Inner {
			size += elem.Size()
		}
		return size
	default:
		panic(fmt.Sprintf("bad group arg type %v", typ))
	}
}

func (arg *GroupArg) fixedInnerSize() bool {
	switch typ := arg.Type().(type) {
	case *StructType:
		return true
	case *ArrayType:
		return typ.Kind == ArrayRangeLen && typ.RangeBegin == typ.RangeEnd
	default:
		panic(fmt.Sprintf("bad group arg type %v", typ))
	}
}

// Used for UnionType.
type UnionArg struct {
	ArgCommon
	Option Arg
	Index  int // Index of the selected option in the union type.
}

func MakeUnionArg(t Type, dir Dir, opt Arg, index int) *UnionArg {
	return &UnionArg{ArgCommon: ArgCommon{ref: t.ref(), dir: dir}, Option: opt, Index: index}
}

func (arg *UnionArg) Size() uint64 {
	if !arg.Type().Varlen() {
		return arg.Type().Size()
	}
	return arg.Option.Size()
}

// Used for ResourceType.
// This is the only argument that can be used as syscall return value.
// Either holds constant value or reference another ResultArg.
type ResultArg struct {
	ArgCommon
	Res   *ResultArg          // reference to arg which we use
	OpDiv uint64              // divide result (executed before OpAdd)
	OpAdd uint64              // add to result
	Val   uint64              // value used if Res is nil
	uses  map[*ResultArg]bool // args that use this arg
}

func MakeResultArg(t Type, dir Dir, r *ResultArg, v uint64) *ResultArg {
	arg := &ResultArg{ArgCommon: ArgCommon{ref: t.ref(), dir: dir}, Res: r, Val: v}
	if r == nil {
		return arg
	}
	if r.uses == nil {
		r.uses = make(map[*ResultArg]bool)
	}
	r.uses[arg] = true
	return arg
}

func MakeReturnArg(t Type) *ResultArg {
	if t == nil {
		return nil
	}
	return &ResultArg{ArgCommon: ArgCommon{ref: t.ref(), dir: DirOut}}
}

func (arg *ResultArg) Size() uint64 {
	return arg.Type().Size()
}

// Returns inner arg for pointer args.
func InnerArg(arg Arg) Arg {
	if _, ok := arg.Type().(*PtrType); ok {
		res := arg.(*PointerArg).Res
		if res == nil {
			return nil
		}
		return InnerArg(res)
	}
	return arg // Not a pointer.
}

func isDefault(arg Arg) bool {
	return arg.Type().isDefaultArg(arg)
}

func (p *Prog) insertBefore(c *Call, calls []*Call) {
	idx := 0
	for ; idx < len(p.Calls); idx++ {
		if p.Calls[idx] == c {
			break
		}
	}
	var newCalls []*Call
	newCalls = append(newCalls, p.Calls[:idx]...)
	newCalls = append(newCalls, calls...)
	if idx < len(p.Calls) {
		newCalls = append(newCalls, p.Calls[idx])
		newCalls = append(newCalls, p.Calls[idx+1:]...)
	}
	p.Calls = newCalls
}

func (p *Prog) insertAfter(c *Call, calls []*Call) {
	idx := 0
	for ; idx < len(p.Calls); idx++ {
		if p.Calls[idx] == c {
			break
		}
	}
	var newCalls []*Call
	newCalls = append(newCalls, p.Calls[:idx]...)
	if idx < len(p.Calls) {
		newCalls = append(newCalls, p.Calls[idx])
		newCalls = append(newCalls, calls...)
		newCalls = append(newCalls, p.Calls[idx+1:]...)
	}
	p.Calls = newCalls
}

func (p *Prog) insertEnd(calls []*Call) {
	p.Calls = append(p.Calls, calls...)
}

// replaceArg replaces arg with arg1 in a program.
func replaceArg(arg, arg1 Arg) {
	switch a := arg.(type) {
	case *ConstArg:
		*a = *arg1.(*ConstArg)
	case *ResultArg:
		replaceResultArg(a, arg1.(*ResultArg))
	case *PointerArg:
		*a = *arg1.(*PointerArg)
	case *UnionArg:
		*a = *arg1.(*UnionArg)
	case *DataArg:
		*a = *arg1.(*DataArg)
	case *GroupArg:
		a1 := arg1.(*GroupArg)
		if len(a.Inner) != len(a1.Inner) {
			panic(fmt.Sprintf("replaceArg: group fields don't match: %v/%v",
				len(a.Inner), len(a1.Inner)))
		}
		a.ArgCommon = a1.ArgCommon
		for i := range a.Inner {
			replaceArg(a.Inner[i], a1.Inner[i])
		}
	default:
		panic(fmt.Sprintf("replaceArg: bad arg kind %#v", arg))
	}
}

func replaceResultArg(arg, arg1 *ResultArg) {
	// Remove link from `a.Res` to `arg`.
	if arg.Res != nil {
		delete(arg.Res.uses, arg)
	}
	// Copy all fields from `arg1` to `arg` except for the list of args that use `arg`.
	uses := arg.uses
	*arg = *arg1
	arg.uses = uses
	// Make the link in `arg.Res` (which is now `Res` of `arg1`) to point to `arg` instead of `arg1`.
	if arg.Res != nil {
		resUses := arg.Res.uses
		delete(resUses, arg1)
		resUses[arg] = true
	}
}

// removeArg removes all references to/from arg0 from a program.
func removeArg(arg0 Arg) {
	ForeachSubArg(arg0, func(arg Arg, ctx *ArgCtx) {
		a, ok := arg.(*ResultArg)
		if !ok {
			return
		}
		if a.Res != nil {
			uses := a.Res.uses
			if !uses[a] {
				panic("broken tree")
			}
			delete(uses, a)
		}
		for arg1 := range a.uses {
			arg2 := arg1.Type().DefaultArg(arg1.Dir()).(*ResultArg)
			replaceResultArg(arg1, arg2)
		}
	})
}

// removeCall removes call idx from p.
func (p *Prog) RemoveCall(idx int) {
	c := p.Calls[idx]
	for _, arg := range c.Args {
		removeArg(arg)
	}
	if c.Ret != nil {
		removeArg(c.Ret)
	}
	copy(p.Calls[idx:], p.Calls[idx+1:])
	p.Calls = p.Calls[:len(p.Calls)-1]
}

func (p *Prog) sanitizeFix() {
	if err := p.sanitize(true); err != nil {
		panic(err)
	}
}

func (p *Prog) sanitize(fix bool) error {
	for _, c := range p.Calls {
		if err := p.Target.sanitize(c, fix); err != nil {
			return err
		}
	}
	return nil
}

// TODO: This method might be more generic - it can be applied to any struct.
func (props *CallProps) ForeachProp(f func(fieldName, key string, value reflect.Value)) {
	valueObj := reflect.ValueOf(props).Elem()
	typeObj := valueObj.Type()
	for i := 0; i < valueObj.NumField(); i++ {
		fieldValue := valueObj.Field(i)
		fieldType := typeObj.Field(i)
		f(fieldType.Name, fieldType.Tag.Get("key"), fieldValue)
	}
}

func CmpProgs(ps []*Prog, srvNum int) {

	Equal := func(a, b []int) bool {
		if len(a) != len(b) {
			return false
		}
		for i, v := range a {
			if v != b[i] {
				return false
			}
		}
		return true
	}

	log.Logf(0, "before return in CmpProgs, HasNetFail: %v, HasCrashFail: %v", ps[0].HasNetFail, ps[0].HasCrashFail)

	if !ps[0].HasNetFail && !ps[0].HasCrashFail {
		return
	}

	lastProgOrder := make([]int, 0)
	lastProgOrder = nil
	for i := srvNum; i < len(ps); i++ {
		curOrder := make([]int, 0)
		for _, call := range ps[i].Calls {
			if call.IsFCall {
				curOrder = append(curOrder, int(call.Args[0].(*ConstArg).Val))
			}
		}

		if lastProgOrder == nil {
			lastProgOrder = curOrder
			continue
		}

		if !Equal(lastProgOrder, curOrder) {
			log.Fatalf("orders are not the same: %v, %v", lastProgOrder, curOrder)
		} else {
			log.Logf(0, "orders: lastProgOrder %v, curOrder %v", lastProgOrder, curOrder)
		}
	}

}
