package checker

import (
	"strings"
	"syscall"
	"testing"

	"monarch/prog"
)

func checkerTestTarget(t *testing.T) *prog.Target {
	t.Helper()
	target, err := prog.GetTarget("linux", "amd64")
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	return target
}

func checkerPathCall(t *testing.T, target *prog.Target, name, path string) *prog.Call {
	t.Helper()
	meta := target.SyscallMap[name]
	if meta == nil || len(meta.Args) == 0 {
		t.Fatalf("missing syscall %q", name)
	}
	ptr, ok := meta.Args[0].Type.(*prog.PtrType)
	if !ok {
		t.Fatalf("%s arg0 is not a pointer", name)
	}
	data := prog.MakeDataArg(ptr.Elem, prog.DirIn, []byte(path+"\x00"))
	return prog.MakeCall(meta, []prog.Arg{
		prog.MakePointerArg(ptr, prog.DirIn, 0, data),
	})
}

func checkerDataOnlyCall(t *testing.T, target *prog.Target, name, dataPath string) *prog.Call {
	t.Helper()
	meta := target.SyscallMap[name]
	if meta == nil || len(meta.Args) < 2 {
		t.Fatalf("missing syscall %q", name)
	}
	ptr, ok := meta.Args[1].Type.(*prog.PtrType)
	if !ok {
		t.Fatalf("%s arg1 is not a pointer", name)
	}
	data := prog.MakeDataArg(ptr.Elem, prog.DirIn, []byte(dataPath+"\x00"))
	return prog.MakeCall(meta, []prog.Arg{
		prog.MakeReturnArg(meta.Args[0].Type),
		data,
	})
}

func checkerSymlinkCall(t *testing.T, target *prog.Target, linkTarget, linkPath string) *prog.Call {
	t.Helper()
	meta := target.SyscallMap["symlink"]
	if meta == nil || len(meta.Args) < 2 {
		t.Fatal("missing symlink syscall")
	}
	targetPtr, ok := meta.Args[0].Type.(*prog.PtrType)
	if !ok {
		t.Fatal("symlink target arg is not a pointer")
	}
	linkPtr, ok := meta.Args[1].Type.(*prog.PtrType)
	if !ok {
		t.Fatal("symlink linkpath arg is not a pointer")
	}
	targetData := prog.MakeDataArg(targetPtr.Elem, prog.DirIn, []byte(linkTarget+"\x00"))
	linkData := prog.MakeDataArg(linkPtr.Elem, prog.DirIn, []byte(linkPath+"\x00"))
	return prog.MakeCall(meta, []prog.Arg{
		prog.MakePointerArg(targetPtr, prog.DirIn, 0, targetData),
		prog.MakePointerArg(linkPtr, prog.DirIn, 0, linkData),
	})
}

func checkerTree(prefix string, files, dirs []string) *prog.FileTree {
	ft := prog.NewFileTreeWithPrefix(prefix)
	cfg := &prog.Hmdfs_config{
		Init_file:      map[string][]string{"shared": files},
		Init_dir:       map[string][]string{"shared": dirs},
		Init_tmpdir:    map[string][]string{},
		Init_empty_dir: map[string][]string{},
		FileSize:       map[string]map[string]uint64{},
	}
	ft.InitFromHmdfsConfig(cfg)
	return ft
}

func checkerSubset(t *testing.T, ps []*prog.Prog, ft *prog.FileTree) map[string]bool {
	t.Helper()
	got := make(map[string]bool)
	for _, line := range strings.Split(initTreeSubset(ps, ft), "\n") {
		if line == "" {
			continue
		}
		got[strings.Split(line, "\t")[0]] = true
	}
	return got
}

func TestInitTreeSubsetGlusterfs(t *testing.T) {
	target := checkerTestTarget(t)
	ft := checkerTree("",
		[]string{"q0", "dir/a", "extra", "sym-target"},
		[]string{"dir"})

	ps := []*prog.Prog{{Calls: []*prog.Call{
		checkerPathCall(t, target, "open", "q0"),
		checkerPathCall(t, target, "stat", "dir/a"),
		checkerSymlinkCall(t, target, "sym-target", "l"),
		checkerDataOnlyCall(t, target, "write", "extra"),
	}}}
	got := checkerSubset(t, ps, ft)
	for _, want := range []string{"q0", "dir", "dir/a", "sym-target"} {
		if !got[want] {
			t.Fatalf("initTreeSubset missing %q: %v", want, got)
		}
	}
	if got["extra"] {
		t.Fatalf("write payload was mistaken for a path: %v", got)
	}
}

func TestInitTreeSubsetHMDFS(t *testing.T) {
	target := checkerTestTarget(t)
	ft := checkerTree("merge_view/",
		[]string{"merge_view/q0", "merge_view/dir/a", "merge_view/extra"},
		[]string{"merge_view/dir"})

	ps := []*prog.Prog{{Calls: []*prog.Call{
		checkerPathCall(t, target, "open", "merge_view/q0"),
		checkerPathCall(t, target, "stat", "merge_view/dir/a"),
		checkerDataOnlyCall(t, target, "write", "merge_view/extra"),
	}}}
	got := checkerSubset(t, ps, ft)
	for _, want := range []string{"q0", "dir", "dir/a"} {
		if !got[want] {
			t.Fatalf("initTreeSubset missing %q: %v", want, got)
		}
	}
	if got["extra"] {
		t.Fatalf("write payload was mistaken for a path: %v", got)
	}
}

func TestInitTreeSubsetFreshParentFallback(t *testing.T) {
	target := checkerTestTarget(t)
	ft := checkerTree("", nil, []string{"dir"})

	ps := []*prog.Prog{{Calls: []*prog.Call{
		checkerPathCall(t, target, "creat", "dir/fresh"),
		checkerSymlinkCall(t, target, "dir", "dir/newlink"),
	}}}
	got := checkerSubset(t, ps, ft)
	if !got["dir"] {
		t.Fatalf("fresh path did not seed its baseline parent: %v", got)
	}
	if got["dir/fresh"] || got["dir/newlink"] {
		t.Fatalf("fresh path leaked into initTreeSubset: %v", got)
	}
}

func TestSetEnvReplaces(t *testing.T) {
	got := setEnv([]string{"PATH=/bin", "GLUSTERFS_CSAN_LOG=/old", "X=1"},
		"GLUSTERFS_CSAN_LOG", "/new")
	want := []string{"PATH=/bin", "X=1", "GLUSTERFS_CSAN_LOG=/new"}
	if len(got) != len(want) {
		t.Fatalf("setEnv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("setEnv = %v, want %v", got, want)
		}
	}
}

func TestCompareFileMetaGlusterfs(t *testing.T) {
	file := func() prog.FileMetadata {
		return prog.FileMetadata{
			StatMd: syscall.Stat_t{
				Mode:  syscall.S_IFREG | 0644,
				Size:  10,
				Nlink: 1,
				Mtim:  syscall.Timespec{Sec: 100},
			},
			Checksum: 7,
			Xattr:    map[string]string{"user.test": "a"},
		}
	}
	m1 := file()
	m2 := file()
	if got := compareFileMeta("q0", m1, m2, "glusterfs"); len(got) != 0 {
		t.Fatalf("equal metadata reported: %v", got)
	}

	changedType := file()
	changedType.StatMd.Mode = syscall.S_IFLNK | 0777
	if got := compareFileMeta("q0", m1, changedType, "glusterfs"); len(got) == 0 {
		t.Fatal("full S_IFMT mismatch was not reported")
	}

	changedSymlink := file()
	changedSymlink.StatMd.Mode = syscall.S_IFLNK | 0777
	changedSymlink.SymlinkPath = "target-a"
	changedSymlink2 := changedSymlink
	changedSymlink2.SymlinkPath = "target-b"
	if got := compareFileMeta("q0", changedSymlink, changedSymlink2, "glusterfs"); len(got) == 0 {
		t.Fatal("symlink target mismatch was not reported")
	}

	extraXattr := file()
	extraXattr.Xattr = map[string]string{"user.test": "a", "user.extra": "b"}
	if got := compareFileMeta("q0", m1, extraXattr, "glusterfs"); len(got) == 0 {
		t.Fatal("extra user xattr was not reported")
	}

	diagOnly := file()
	diagOnly.StatMd.Nlink = 2
	diagOnly.StatMd.Mtim = syscall.Timespec{Sec: 200}
	if got := compareFileMeta("q0", m1, diagOnly, "glusterfs"); len(got) != 0 {
		t.Fatalf("B-class nlink/mtime became hard failures: %v", got)
	}
}
