package prog

import "testing"

func TestPathEffectUnlinkFile(t *testing.T) {
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA/a", NodeTypeFile, "")
	e := plannedPathEffect(ft, "unlink", "dirA/a", "dirA/a", "dirA/a")
	if e.kind != pathEffectRemove || e.src != "dirA/a" {
		t.Fatalf("unlink effect = %+v, want Remove(dirA/a)", e)
	}
	for _, path := range []string{"dirA/a", "dirA/a/child"} {
		if e.allows(path) {
			t.Fatalf("removed path %q accepted", path)
		}
	}
	for _, path := range []string{"", "dirA", "dirA/b", "dirB/c"} {
		if !e.allows(path) {
			t.Fatalf("safe path %q rejected", path)
		}
	}
}

func TestPathEffectUnlinkDirectoryHasNoEffect(t *testing.T) {
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA", NodeTypeDir, "")
	if e := plannedPathEffect(ft, "unlink", "dirA", "dirA", "dirA"); e.active() {
		t.Fatalf("unlink on a directory produced %+v", e)
	}
}

func TestPathEffectRmdirEmptyVsNonEmpty(t *testing.T) {
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA/empty", NodeTypeEmptyDir, "")
	ft.AddNode("dirA/full", NodeTypeDir, "")
	ft.AddNode("dirA/full/kid", NodeTypeFile, "")

	empty := plannedPathEffect(ft, "rmdir", "dirA/empty", "dirA/empty", "dirA/empty")
	if empty.kind != pathEffectRemove || empty.allows("dirA/empty") {
		t.Fatalf("empty rmdir effect = %+v", empty)
	}
	nonEmpty := plannedPathEffect(ft, "rmdir", "dirA/full", "dirA/full", "dirA/full")
	if nonEmpty.active() {
		t.Fatalf("non-empty rmdir produced %+v", nonEmpty)
	}
}

func TestPathEffectRenameMove(t *testing.T) {
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("dirA/src/kid", NodeTypeFile, "")
	e := plannedPathEffect(ft, "rename", "dirA/dst", "dirA/src", "dirA/dst")
	if e.kind != pathEffectMove || e.src != "dirA/src" || e.dst != "dirA/dst" {
		t.Fatalf("rename effect = %+v", e)
	}
	for _, path := range []string{"dirA/src", "dirA/src/kid"} {
		if e.allows(path) {
			t.Fatalf("moved source path %q accepted", path)
		}
	}
	for _, path := range []string{"dirA", "dirA/dst", "dirA/sibling"} {
		if !e.allows(path) {
			t.Fatalf("safe rename path %q rejected", path)
		}
	}
}

func TestC2VariantAllowedAfterEffect(t *testing.T) {
	e := pathEffect{kind: pathEffectMove, src: "dirA/src", dst: "dirA/dst"}

	creatOK := directedCell{Combo: comboKey{Variant: "creat"}, RetB: RetSuccess}
	if !c2VariantAllowedAfterEffect(e, creatOK, "dirA/src", "", false, false) {
		t.Fatal("creat on the moved source path rejected")
	}
	creatEEXIST := directedCell{Combo: comboKey{Variant: "creat"}, RetB: RetEEXIST}
	if c2VariantAllowedAfterEffect(e, creatEEXIST, "dirA/src", "", false, false) {
		t.Fatal("creat EEXIST on the moved source path accepted")
	}
	stat := directedCell{Combo: comboKey{Variant: "stat"}, RetB: RetSuccess}
	if c2VariantAllowedAfterEffect(e, stat, "dirA/src", "", false, false) {
		t.Fatal("stat on the moved source path accepted")
	}
	// FD variants are not exempt: directed variants reopen the path in
	// another proc, so the open fails after the move.
	read := directedCell{Combo: comboKey{Variant: "read"}, RetB: RetSuccess}
	if c2VariantAllowedAfterEffect(e, read, "dirA/src", "", false, false) {
		t.Fatal("read on the moved source path accepted")
	}
	if !c2VariantAllowedAfterEffect(e, stat, "dirA/dst", "", false, false) {
		t.Fatal("stat on the move destination rejected")
	}
	freshB := directedCell{Combo: comboKey{Variant: "creat"}, RetB: RetSuccess}
	if c2VariantAllowedAfterEffect(e, freshB, "", "dirA/src", true, false) {
		t.Fatal("fresh-B under the moved source parent accepted")
	}
	if !c2VariantAllowedAfterEffect(e, freshB, "", "dirA/dst", true, false) {
		t.Fatal("fresh-B under the move destination parent rejected")
	}
}
