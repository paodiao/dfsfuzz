package prog

import "testing"

func TestCoverageBoardCallListsUseProfile(t *testing.T) {
	roots, variants := coverageBoardCallLists(glusterfsProfile)
	if len(roots) != len(GlusterfsRootCalls) {
		t.Fatalf("glusterfs roots = %d, want %d", len(roots), len(GlusterfsRootCalls))
	}
	if len(variants) != len(GlusterfsVariantCalls) {
		t.Fatalf("glusterfs variants = %d, want %d", len(variants), len(GlusterfsVariantCalls))
	}
	wantRoots := map[string]bool{"link": true, "symlink": true, "setxattr": true}
	for name := range wantRoots {
		found := false
		for _, r := range roots {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("glusterfs root list missing %q", name)
		}
	}
	wantVariants := map[string]bool{"link": true, "symlink": true, "readlink": true, "getxattr": true}
	for name := range wantVariants {
		found := false
		for _, v := range variants {
			if v == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("glusterfs variant list missing %q", name)
		}
	}

	roots, variants = coverageBoardCallLists(hmdfsProfile)
	if len(roots) != len(FsopsSubCalls) || len(variants) != len(FsopsSubCalls) {
		t.Fatalf("hmdfs lists = %d/%d, want %d/%d",
			len(roots), len(variants), len(FsopsSubCalls), len(FsopsSubCalls))
	}
}

func TestGlusterfsCallClassification(t *testing.T) {
	cfg := &Hmdfs_config{Profile: glusterfsProfile, DfsName: "glusterfs"}
	for _, name := range []string{"link", "symlink", "readlink"} {
		if !cfg.IsFileOnlyCall(name) {
			t.Fatalf("glusterfs %q is not file-only", name)
		}
		if got := callNameConstraint(name); got != relTargetFile {
			t.Fatalf("callNameConstraint(%q) = %d, want file", name, got)
		}
	}
	for _, name := range []string{"getxattr", "setxattr"} {
		if cfg.IsFileOnlyCall(name) || cfg.IsDirOnlyCall(name) {
			t.Fatalf("glusterfs %q should be file-or-dir", name)
		}
		if got := callNameConstraint(name); got != relTargetAny {
			t.Fatalf("callNameConstraint(%q) = %d, want any", name, got)
		}
	}
	if !cfg.IsDirOnlyCall("mkdir") || !cfg.IsDirOnlyCall("getdents64") {
		t.Fatalf("glusterfs directory-only classification regressed")
	}
}

func TestGlusterfsFuncClass(t *testing.T) {
	cases := map[string]uint32{
		"link":     FuncLink,
		"symlink":  FuncSymlink,
		"readlink": FuncReadlink,
		"getxattr": FuncGetxattr,
		"setxattr": FuncSetxattr,
	}
	for call, want := range cases {
		got, ok := callFuncClass(glusterfsProfile, call, false)
		if !ok || got != want {
			t.Fatalf("callFuncClass(glusterfs, %q) = %d,%v want %d,true", call, got, ok, want)
		}
		if _, ok := callFuncClass(hmdfsProfile, call, false); ok {
			t.Fatalf("callFuncClass(hmdfs, %q) unexpectedly known", call)
		}
	}
	if got, ok := callFuncClass(glusterfsProfile, "open", true); !ok || got != FuncDirOpen {
		t.Fatalf("glusterfs open(dir) = %d,%v", got, ok)
	}
	if got, ok := callFuncClass(glusterfsProfile, "open", false); !ok || got != FuncFileOpen {
		t.Fatalf("glusterfs open(file) = %d,%v", got, ok)
	}
}

func TestNormalizeGlusterfsClientProgs(t *testing.T) {
	mk := func() *Prog { return &Prog{Calls: []*Call{{}}} }
	ps := normalizeGlusterfsClientProgs([]*Prog{{}, {Calls: []*Call{{}}}}, 4, mk)
	if len(ps) != 4 {
		t.Fatalf("normalized count = %d, want 4", len(ps))
	}
	for i, p := range ps {
		if p == nil || len(p.Calls) == 0 {
			t.Fatalf("slot %d is empty after normalization", i)
		}
	}

	ps = normalizeGlusterfsClientProgs([]*Prog{
		{Calls: []*Call{{}}}, {Calls: []*Call{{}}}, {Calls: []*Call{{}}},
	}, 2, mk)
	if len(ps) != 2 {
		t.Fatalf("truncated count = %d, want 2", len(ps))
	}
}

func TestGlusterfsEmptyRootPath(t *testing.T) {
	cfg := &Hmdfs_config{Profile: glusterfsProfile, FileTree: NewFileTreeWithPrefix("")}
	if got := defaultTestDir(cfg); got != "" {
		t.Fatalf("defaultTestDir(glusterfs) = %q, want empty", got)
	}
	if got := joinTestPath("", "q0"); got != "q0" {
		t.Fatalf("joinTestPath(empty) = %q, want q0", got)
	}
	if got := joinTestPath("dir/", "q0"); got != "dir/q0" {
		t.Fatalf("joinTestPath(dir) = %q, want dir/q0", got)
	}
}

func TestGlusterfsOffsetAndRetModel(t *testing.T) {
	if isOffsetCall("readlink") {
		t.Fatalf("readlink must not carry offset buckets")
	}
	if !isOffsetCall("pread64") {
		t.Fatalf("pread64 must carry offset buckets")
	}
	rets := producibleRets("link")
	seen := map[RetBucket]bool{}
	for _, r := range rets {
		seen[r] = true
	}
	for _, want := range []RetBucket{RetSuccess, RetEEXIST, RetENOENT, RetFailure} {
		if !seen[want] {
			t.Fatalf("link ret model missing %v: %v", want, rets)
		}
	}
}
