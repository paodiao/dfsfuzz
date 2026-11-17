package prog

import "testing"

// TestNormalizePathSetPathProfilePrefix pins the GlusterFS empty-prefix case:
// PathSet specs must stay in the FileTree namespace, not inherit HMDFS's
// merge_view/ prepend.
func TestNormalizePathSetPathProfilePrefix(t *testing.T) {
	gl := NewFileTreeWithPrefix("")
	hm := NewFileTreeWithPrefix(MergeViewPrefix)
	cases := []struct {
		name string
		ft   *FileTree
		in   string
		want string
	}{
		{"glusterfs plain", gl, "q0/f", "q0/f"},
		{"glusterfs dot", gl, "./q0/f", "q0/f"},
		{"glusterfs slash", gl, "/q0/f", "q0/f"},
		{"hmdfs plain", hm, "q0/f", "merge_view/q0/f"},
		{"hmdfs prefixed", hm, "merge_view/q0/f", "merge_view/q0/f"},
		{"hmdfs dot", hm, "./merge_view/q0/f", "merge_view/q0/f"},
	}
	for _, tc := range cases {
		if got := normalizePathSetPath(tc.ft, tc.in); got != tc.want {
			t.Fatalf("%s: normalize(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
