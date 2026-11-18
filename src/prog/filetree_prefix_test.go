package prog

import "testing"

func TestFileTreeEmptyPrefix(t *testing.T) {
	ft := NewFileTreeWithPrefix("")
	if got := ft.RootPath(); got != "" {
		t.Fatalf("RootPath=%q, want empty", got)
	}
	if got := ft.JoinRoot("a/b"); got != "a/b" {
		t.Fatalf("JoinRoot=%q, want a/b", got)
	}
	if node := ft.AddNode("a/b", NodeTypeFile, "shared"); node == nil || node.FullPath != "a/b" {
		t.Fatalf("AddNode failed: %+v", node)
	}
	if node := ft.FindNode("a/b"); node == nil || node.Type != NodeTypeFile {
		t.Fatalf("FindNode(a/b)=%+v", node)
	}
	if got := ft.DepthBucketOf("a/b"); got != 2 {
		t.Fatalf("DepthBucketOf(a/b)=%d, want 2", got)
	}
	if !ft.RenameNode("a/b", "a/c") {
		t.Fatal("RenameNode failed")
	}
	if ft.FindNode("a/b") != nil || ft.FindNode("a/c") == nil {
		t.Fatal("RenameNode did not move node")
	}
}

func TestNewLayeredChoiceStrategyFileTreePrefix(t *testing.T) {
	cids := []string{"c1"}
	gl := &Hmdfs_config{
		Profile: glusterfsProfile, Cids: cids, Node_num: 1,
		Init_dir:  map[string][]string{"c1": {"dirA"}},
		Init_file: map[string][]string{"c1": {"dirA/a"}},
	}
	glcs := NewLayeredChoiceStrategy(gl, nil)
	if glcs.FileTree == nil || glcs.FileTree.Prefix != "" {
		t.Fatalf("glusterfs tree prefix = %+v, want empty", glcs.FileTree)
	}
	if glcs.FileTree.FindNode("dirA/a") == nil {
		t.Fatal("glusterfs tree missing dirA/a")
	}
	if glcs.FileTree.FindNode("merge_view/dirA/a") != nil {
		t.Fatal("glusterfs tree was built with the merge_view prefix")
	}

	hm := &Hmdfs_config{
		Profile: hmdfsProfile, Cids: cids, Node_num: 1,
		Init_dir:  map[string][]string{"c1": {"merge_view/dirA"}},
		Init_file: map[string][]string{"c1": {"merge_view/dirA/a"}},
	}
	hlcs := NewLayeredChoiceStrategy(hm, nil)
	if hlcs.FileTree == nil || hlcs.FileTree.Prefix != MergeViewPrefix {
		t.Fatalf("hmdfs tree prefix = %+v, want %q", hlcs.FileTree, MergeViewPrefix)
	}
	if hlcs.FileTree.FindNode("merge_view/dirA/a") == nil {
		t.Fatal("hmdfs tree missing merge_view/dirA/a")
	}
}

func TestFileTreeDefaultPrefix(t *testing.T) {
	ft := NewFileTree()
	if got := ft.RootPath(); got != "merge_view" {
		t.Fatalf("RootPath=%q, want merge_view", got)
	}
	if got := ft.JoinRoot("file0"); got != "merge_view/file0" {
		t.Fatalf("JoinRoot=%q", got)
	}
	if node := ft.AddNode("merge_view/file0", NodeTypeFile, "shared"); node == nil || node.FullPath != "merge_view/file0" {
		t.Fatalf("AddNode failed: %+v", node)
	}
	if node := ft.FindNode("file0"); node == nil || node.FullPath != "merge_view/file0" {
		t.Fatalf("FindNode(file0)=%+v", node)
	}
}
