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
