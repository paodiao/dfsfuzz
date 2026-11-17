package prog

import (
	"syscall"
	"testing"
)

func TestNormalizeFsMdPathProfilePrefix(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		in     string
		want   string
	}{
		{"hmdfs relative", MergeViewPrefix, "./dirA/a", "merge_view/dirA/a"},
		{"hmdfs already prefixed", MergeViewPrefix, "merge_view/dirA/a", "merge_view/dirA/a"},
		{"hmdfs leading slash", MergeViewPrefix, "/dirA/a", "merge_view/dirA/a"},
		{"glusterfs relative", "", "./dirA/a", "dirA/a"},
		{"glusterfs already relative", "", "dirA/a", "dirA/a"},
		{"glusterfs leading slash", "", "/dirA/a", "dirA/a"},
	}
	for _, tc := range cases {
		if got := NormalizeFsMdPath(tc.prefix, tc.in); got != tc.want {
			t.Fatalf("%s: NormalizeFsMdPath(%q, %q) = %q, want %q",
				tc.name, tc.prefix, tc.in, got, tc.want)
		}
	}
	if got := NormalizeMergeViewPath("./dirA/a"); got != "merge_view/dirA/a" {
		t.Fatalf("NormalizeMergeViewPath wrapper = %q", got)
	}
}

func TestSyncFileTreeFromFsMdGlusterfsPrefix(t *testing.T) {
	hmcfg := &Hmdfs_config{
		Profile:  glusterfsProfile,
		Cids:     []string{"c1", "c2", "c3"},
		Node_num: 3,
		Init_dir: map[string][]string{"c1": {"dirA"}},
		Init_file: map[string][]string{
			"c1": {"dirA/a"},
		},
	}
	hmcfg.FileTree = NewFileTreeWithPrefix("")
	hmcfg.FileTree.InitFromHmdfsConfig(hmcfg)

	fsMd := map[string]FileMetadata{
		"./dirA":   {StatMd: syscall.Stat_t{Mode: syscall.S_IFDIR | 0o755}},
		"./dirA/a": {StatMd: syscall.Stat_t{Mode: syscall.S_IFREG | 0o644, Size: 5}},
	}
	SyncFileTreeFromFsMd(fsMd, map[string]string{"dirA/a": "c2"}, hmcfg)

	node := hmcfg.FileTree.FindNode("dirA/a")
	if node == nil {
		t.Fatal("GlusterFS fsMd path was not synced into the empty-prefix tree")
	}
	if node.OwnerCid != "c2" {
		t.Fatalf("owner = %q, want c2", node.OwnerCid)
	}
	if hmcfg.FileTree.FindNode("merge_view/dirA/a") != nil {
		t.Fatal("GlusterFS fsMd path was incorrectly prefixed with merge_view")
	}
}
