package prog

import (
	"math/rand"
	"strings"
	"testing"
)

// TestGenerateSpecialCallByName covers the five GlusterFS-only v1 calls wired
// through generateCallByName. link/symlink/path arguments are canonical test
// paths; only the symlink on-wire payload is materialized relative to the
// linkpath parent.
func TestGenerateSpecialCallByName(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	sc := hmdfsSmokeSpecialCalls(t, target)
	s := newState(target, nil, nil)
	r := newRand(target, rand.NewSource(7))

	cases := []struct {
		name        string
		path        string
		path2       string
		wantArgs    map[int]string
		wantArg0Rel string
	}{
		{
			name:  "link",
			path:  "dir/newlink",
			path2: "dir/a",
			wantArgs: map[int]string{
				0: "dir/a",
				1: "dir/newlink",
			},
		},
		{
			name:  "symlink",
			path:  "dir/link",
			path2: "dir/a",
			wantArgs: map[int]string{
				0: "a", // payload relative to dir/
				1: "dir/link",
			},
			wantArg0Rel: "dir/a",
		},
		{
			name: "readlink",
			path: "dir/link",
			wantArgs: map[int]string{
				0: "dir/link",
			},
		},
		{
			name: "getxattr",
			path: "dir/a",
			wantArgs: map[int]string{
				0: "dir/a",
			},
		},
		{
			name: "setxattr",
			path: "dir/a",
			wantArgs: map[int]string{
				0: "dir/a",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := target.SyscallMap[tc.name]; !ok {
				t.Skipf("target has no %s", tc.name)
			}
			calls := r.generateCallByName(s, sc, tc.name, tc.path, tc.path2, "", nil, false, 0, 2)
			if len(calls) != 1 || calls[0] == nil {
				t.Fatalf("generateCallByName(%q) = %d calls, want 1", tc.name, len(calls))
			}
			c := calls[0]
			if c.Meta == nil || c.Meta.Name != tc.name {
				t.Fatalf("meta = %v, want %q", c.Meta, tc.name)
			}
			for idx, want := range tc.wantArgs {
				if got := extractPathFromCallByArgIdx(c, idx); got != want {
					t.Fatalf("arg%d = %q, want %q", idx, got, want)
				}
			}
			switch tc.name {
			case "getxattr":
				if name := extractPathFromCallByArgIdx(c, 1); !strings.HasPrefix(name, "user.") {
					t.Fatalf("xattr name = %q, want user.*", name)
				}
			case "setxattr":
				if name := extractPathFromCallByArgIdx(c, 1); !strings.HasPrefix(name, "user.") {
					t.Fatalf("xattr name = %q, want user.*", name)
				}
				if flags, ok := c.Args[4].(*ConstArg); !ok || flags.Val != 0 {
					t.Fatalf("setxattr flags = %+v, want 0", c.Args[4])
				}
			case "readlink":
				if size, ok := c.Args[2].(*ConstArg); !ok || size.Val == 0 {
					t.Fatalf("readlink buffer size = %+v, want > 0", c.Args[2])
				}
			}
			if tc.wantArg0Rel != "" {
				got := symlinkCanonicalTarget(tc.path, extractPathFromCallByArgIdx(c, 0))
				if got != tc.wantArg0Rel {
					t.Fatalf("payload round-trip = %q, want %q", got, tc.wantArg0Rel)
				}
			}
		})
	}
}

func TestSymlinkCanonicalTargetAndPayload(t *testing.T) {
	cases := []struct {
		linkPath string
		payload  string
		want     string
	}{
		{"dir/link", "a", "dir/a"},
		{"dir/sub/link", "../a", "dir/a"},
		{"q0", "q1", "q1"}, // top-level linkpath has no "." parent
		{"dir/link", "/eifuzz_root/dir/a", "dir/a"},
		{"dir/link", "/eifuzz_root", ""},
		{"dir/link", "/etc/passwd", ""},
		{"link", "../../x", ""},
		{"dir/link", "", ""},
	}
	for _, tc := range cases {
		if got := symlinkCanonicalTarget(tc.linkPath, tc.payload); got != tc.want {
			t.Fatalf("symlinkCanonicalTarget(%q, %q) = %q, want %q",
				tc.linkPath, tc.payload, got, tc.want)
		}
	}

	payloads := []struct {
		linkPath string
		target   string
		want     string
	}{
		{"dir/link", "dir/a", "a"},
		{"dir/link", "dir/sub/a", "sub/a"},
		{"dir/sub/link", "dir/a", "../a"},
		{"qlink", "q0", "q0"},
	}
	for _, tc := range payloads {
		got := symlinkPayload(tc.linkPath, tc.target)
		if got != tc.want {
			t.Fatalf("symlinkPayload(%q, %q) = %q, want %q",
				tc.linkPath, tc.target, got, tc.want)
		}
		if round := symlinkCanonicalTarget(tc.linkPath, got); round != tc.target {
			t.Fatalf("payload round-trip %q -> %q -> %q, want %q",
				tc.target, got, round, tc.target)
		}
	}
}

func TestFreshLinkPathParent(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	r := newRand(target, rand.NewSource(11))

	if got := r.freshLinkPath("q0", "link"); !strings.HasPrefix(got, "Eris_link_") ||
		strings.Contains(got, "/") {
		t.Fatalf("top-level fresh linkpath = %q, want Eris_link_*", got)
	}
	if got := r.freshLinkPath("dir/a", "symlink"); !strings.HasPrefix(got, "dir/Eris_symlink_") {
		t.Fatalf("fresh linkpath = %q, want dir/Eris_symlink_*", got)
	}
}

func mkLinkCall(t *testing.T, target *Target, oldPath, newPath string) *Call {
	t.Helper()
	meta := target.SyscallMap["link"]
	oldType := meta.Args[0].Type.(*PtrType)
	newType := meta.Args[1].Type.(*PtrType)
	oldArg := MakePointerArg(oldType, DirIn, 0,
		MakeDataArg(oldType.Elem, DirIn, []byte(oldPath+"\x00")))
	newArg := MakePointerArg(newType, DirIn, 0,
		MakeDataArg(newType.Elem, DirIn, []byte(newPath+"\x00")))
	c := MakeCall(meta, nil)
	c.Args = []Arg{oldArg, newArg}
	target.assignSizesCall(c)
	return c
}

func mkSymlinkCall(t *testing.T, target *Target, payload, linkPath string) *Call {
	t.Helper()
	meta := target.SyscallMap["symlink"]
	targetType := meta.Args[0].Type.(*PtrType)
	linkType := meta.Args[1].Type.(*PtrType)
	targetArg := MakePointerArg(targetType, DirIn, 0,
		MakeDataArg(targetType.Elem, DirIn, []byte(payload+"\x00")))
	linkArg := MakePointerArg(linkType, DirIn, 0,
		MakeDataArg(linkType.Elem, DirIn, []byte(linkPath+"\x00")))
	c := MakeCall(meta, nil)
	c.Args = []Arg{targetArg, linkArg}
	target.assignSizesCall(c)
	return c
}

// TestGlusterfsDagLinkSymlinkPaths pins the GlusterFS v1 vertex path roles:
// link uses Path=new/OldPath=source, symlink uses Path=linkpath and
// OldPath=canonical target.
func TestGlusterfsDagLinkSymlinkPaths(t *testing.T) {
	target := hmdfsSmokeTarget(t)
	if _, ok := target.SyscallMap["link"]; !ok {
		t.Skip("target has no link")
	}
	if _, ok := target.SyscallMap["symlink"]; !ok {
		t.Skip("target has no symlink")
	}

	link := mkLinkCall(t, target, "dir/a", "dir/newlink")
	oldPath, newPath := glusterfsLinkVertexPaths(link)
	if oldPath != "dir/a" || newPath != "dir/newlink" {
		t.Fatalf("link paths = %q/%q, want dir/a/dir/newlink", oldPath, newPath)
	}

	sym := mkSymlinkCall(t, target, "../dir/a", "dir/sub/link")
	linkPath, canonical := glusterfsSymlinkVertexPaths(sym)
	if linkPath != "dir/sub/link" || canonical != "dir/a" {
		t.Fatalf("symlink paths = %q/%q, want dir/sub/link/dir/a", linkPath, canonical)
	}
}
