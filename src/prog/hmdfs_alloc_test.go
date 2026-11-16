package prog

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestHmdfsGenNoOverlap verifies that hmdfs program generators never produce
// overlapping data addresses across calls within a program. This guards the
// stateFromProg() fix: generators/mutators must allocate data on the target
// program's state, not on an independent newState() (whose allocator starts
// from address 0 and collides with existing data).

func hmdfsSmokeTarget(t *testing.T) *Target {
	target, err := GetTarget("linux", "amd64")
	if err != nil {
		t.Skipf("no linux/amd64 target data: %v", err)
	}
	return target
}

// hmdfsSmokeCids is shared by the smoke file tree and config so tree lookups
// by hmcfg.Cids[0] actually hit (a mismatch would silently disable the fresh
// FileTree branches in the tests).
func hmdfsSmokeCids() []string {
	return []string{
		"c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1",
		"c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2",
		"c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3",
	}
}

func hmdfsSmokeFileTree() *FileTree {
	cids := hmdfsSmokeCids()
	ft := NewFileTree()
	ft.InitFromHmdfsConfig(&Hmdfs_config{
		Init_dir: map[string][]string{
			cids[0]: {"merge_view/dirA", "merge_view/dirB"},
			cids[1]: {"merge_view/dirA", "merge_view/dirB"},
			cids[2]: {"merge_view/dirA", "merge_view/dirB"},
		},
		Init_file: map[string][]string{
			cids[0]: {"merge_view/dirA/a", "merge_view/dirA/b", "merge_view/dirB/c"},
			cids[1]: {"merge_view/dirA/a", "merge_view/dirA/b", "merge_view/dirB/c"},
			cids[2]: {"merge_view/dirA/a", "merge_view/dirA/b", "merge_view/dirB/c"},
		},
	})
	return ft
}

func hmdfsSmokeSpecialCalls(t *testing.T, target *Target) *SpecialCalls {
	sc := &SpecialCalls{}
	for id, syscall := range target.Syscalls {
		switch syscall.Name {
		case "syz_failure_sync":
			sc.SyncfailId = id
		case "syz_failure_recv":
			sc.RecvId = id
		case "syz_failure_send":
			sc.SendId = id
		case "syz_failure_barrier":
			sc.BarrierId = id
		case "syz_failure_up":
			sc.UpId = id
		case "syz_failure_down":
			sc.DownId = id
		case "syz_failure_net_up":
			sc.NetUpId = id
		case "syz_failure_net_down":
			sc.NetDownId = id
		case "syz_net_delay_add":
			sc.NetDelayAddId = id
		case "syz_net_delay_del":
			sc.NetDelayDelId = id
		case "sync":
			sc.SyncId = id
		case "fsync":
			sc.FsyncId = id
		case "fdatasync":
			sc.FdatasyncId = id
		case "lseek":
			sc.LseekId = id
		case "read":
			sc.ReadId = id
		case "readv":
			sc.ReadvId = id
		case "pread64":
			sc.Pread64Id = id
		case "preadv":
			sc.PreadvId = id
		case "write":
			sc.WriteId = id
		case "writev":
			sc.WritevId = id
		case "pwrite64":
			sc.Pwrite64Id = id
		case "pwritev":
			sc.PwritevId = id
		case "open":
			sc.OpenId = id
		case "close":
			sc.CloseId = id
		case "mkdir":
			sc.MkdirId = id
		case "rmdir":
			sc.RmdirId = id
		case "getdents64":
			sc.Getdents64Id = id
		case "creat":
			sc.CreatId = id
		case "unlink":
			sc.UnlinkId = id
		case "rename":
			sc.RenameId = id
		case "chmod":
			sc.ChmodId = id
		case "link":
			sc.LinkId = id
		case "symlink":
			sc.SymlinkId = id
		case "readlink":
			sc.ReadlinkId = id
		case "getxattr":
			sc.GetxattrId = id
		case "setxattr":
			sc.SetxattrId = id
		case "truncate":
			sc.TruncateId = id
		case "stat":
			sc.StatId = id
		case "nanosleep":
			sc.NanosleepId = id
		}
	}
	return sc
}

func hmdfsSmokeConfig(hmcfg *Hmdfs_config) {
	hmcfg.Node_num = 3
	hmcfg.Cids = hmdfsSmokeCids()
	hmcfg.InitIp = "192.168.0.5"
	hmcfg.FileTree = hmdfsSmokeFileTree()
}

// validateNoOverlap walks all pointer-arg data ranges and fails on any overlap.
func validateNoOverlap(t *testing.T, ps []*Prog) {
	t.Helper()
	type rng struct {
		start, end uint64
		what       string
	}
	var ranges []rng
	seen := make(map[uint64]bool)
	for pi, p := range ps {
		if p == nil {
			continue
		}
		for _, c := range p.Calls {
			ForeachArg(c, func(arg Arg, _ *ArgCtx) {
				ptr, ok := arg.(*PointerArg)
				if !ok || ptr.Res == nil {
					return
				}
				sz := ptr.Res.Size()
				if sz == 0 {
					return
				}
				start := uint64(ptr.Address)
				end := start + sz
				for _, r := range ranges {
					if start < r.end && r.start < end {
						t.Errorf("prog %v addr overlap: [0x%x,0x%x) %q vs [0x%x,0x%x) %q",
							pi, start, end, ptr.Res.Type().String(), r.start, r.end, r.what)
					}
				}
				ranges = append(ranges, rng{start, end, ptr.Res.Type().String()})
			})
			// Also detect duplicate raw addresses (even zero-size).
			ForeachArg(c, func(arg Arg, _ *ArgCtx) {
				ptr, ok := arg.(*PointerArg)
				if !ok || ptr.Res == nil {
					return
				}
				if ptr.Address == 0 && !seen[ptr.Address] {
					seen[ptr.Address] = true
				}
			})
		}
	}
}

func TestHmdfsGenNoOverlap(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := &Hmdfs_config{}
	hmdfsSmokeConfig(hmcfg)
	sc := hmdfsSmokeSpecialCalls(t, target)

	gens := []struct {
		name string
		gen  func() []*Prog
	}{
		{"stash", func() []*Prog {
			return target.GenerateProgsForHmdfsStash(rand.New(rand.NewSource(1)), sc, hmcfg)
		}},
		{"dcache", func() []*Prog {
			return target.GenerateProgsForHmdfsDcache(rand.New(rand.NewSource(2)), sc, hmcfg)
		}},
		{"fsops", func() []*Prog {
			return target.GenerateProgsForHmdfsFsops(rand.New(rand.NewSource(3)), sc, hmcfg)
		}},
	}
	for _, g := range gens {
		t.Run("gen_"+g.name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				ps := g.gen()
				validateNoOverlap(t, ps)
			}
		})
	}
}

func TestHmdfsMutateNoOverlap(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	hmcfg := &Hmdfs_config{}
	hmdfsSmokeConfig(hmcfg)
	sc := hmdfsSmokeSpecialCalls(t, target)

	gens := []struct {
		name string
		gen  func(rs rand.Source) []*Prog
		mut  func(ps []*Prog, rs rand.Source) bool
	}{
		{"stash",
			func(rs rand.Source) []*Prog { return target.GenerateProgsForHmdfsStash(rs, sc, hmcfg) },
			func(ps []*Prog, rs rand.Source) bool { return MutateStashProg(ps, rs, nil, sc, hmcfg) }},
		{"dcache",
			func(rs rand.Source) []*Prog { return target.GenerateProgsForHmdfsDcache(rs, sc, hmcfg) },
			func(ps []*Prog, rs rand.Source) bool { return MutateDcacheProg(ps, rs, nil, sc, hmcfg) }},
		{"fsops",
			func(rs rand.Source) []*Prog { return target.GenerateProgsForHmdfsFsops(rs, sc, hmcfg) },
			func(ps []*Prog, rs rand.Source) bool {
				lcs := NewLayeredChoiceStrategy(hmcfg, target)
				return MutateFsopsWithDCT(ps, rs, sc, hmcfg, lcs)
			}},
	}
	// The structured mutators consult the global coverage board; reset it so
	// the sweep is deterministic (static DCT path, no Direction 3 bias).
	ResetCoverageBoard()
	t.Cleanup(ResetCoverageBoard)
	for _, g := range gens {
		t.Run("mutate_"+g.name, func(t *testing.T) {
			for i := 0; i < 10; i++ {
				rs := rand.New(rand.NewSource(int64(100 + i)))
				ps := g.gen(rs)
				g.mut(ps, rs)
				validateNoOverlap(t, ps)
				for _, p := range ps {
					if p.HasBrokenRefs() {
						t.Fatalf("%s iter %d: broken refs after mutation (last %q):\n%s",
							g.name, i, LastMutation(), p.DumpRefDiagnosis())
					}
					assertSerializeSafe(t, fmt.Sprintf("%s iter %d", g.name, i), p)
				}
			}
		})
	}
}
