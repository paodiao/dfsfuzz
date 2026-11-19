package prog

import (
	"math/rand"
	"testing"
)

// TestDepthBucketOfRelative pins the namespace-root-relative bucket mapping.
// HMDFS paths carry the merge_view alias and must keep their historical
// buckets; GlusterFS top-level paths now map to bucket 1 instead of the
// unreachable bucket 0.
func TestDepthBucketOfRelative(t *testing.T) {
	cases := []struct {
		path string
		want uint64
	}{
		{"", 0},
		{"merge_view", 0},
		{"merge_view/a", 1},
		{"merge_view/a/b", 2},
		{"merge_view/a/b/c", 2},
		{"merge_view/a/b/c/d", 2},
		{"merge_view/a/b/c/d/e", 3},
		{"dirA", 1},
		{"dirA/a", 2},
		{"dirA/a/b", 2},
		{"dirA/a/b/c", 2},
		{"dirA/a/b/c/d", 3},
		{"./dirA", 1},
		{"/dirA", 1},
	}
	for _, tc := range cases {
		if got := DepthBucketOf(tc.path); got != tc.want {
			t.Fatalf("DepthBucketOf(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}

	// The tree-aware variant is the collision-free escape hatch: with an
	// empty prefix, merge_view is a real top-level directory, not the root.
	if got := DepthBucketOfWithPrefix("dirA", ""); got != 1 {
		t.Fatalf("empty-prefix top-level bucket = %d, want 1", got)
	}
	if got := DepthBucketOfWithPrefix("merge_view/dirA", ""); got != 2 {
		t.Fatalf("empty-prefix merge_view child bucket = %d, want 2", got)
	}
	if got := DepthBucketOfWithPrefix("merge_view/dirA", MergeViewPrefix); got != 1 {
		t.Fatalf("HMDFS merge_view child bucket = %d, want 1", got)
	}
}

// TestGlusterfsTopLevelBucketSelection pins the B1 symptom: a GlusterFS tree
// whose only candidates are top-level nodes must still be selectable. Before
// the bucket fix those nodes landed in bucket 0, which pickByBucket skips.
func TestGlusterfsTopLevelBucketSelection(t *testing.T) {
	ft := NewFileTreeWithPrefix("")
	ft.AddNode("topfile", NodeTypeFile, "c1")
	ft.AddNode("topdir", NodeTypeDir, "c1")

	r := rand.New(rand.NewSource(1))
	file := ft.SelectFile(r, "c1", DepthBucketWeights)
	if file == nil || file.FullPath != "topfile" {
		t.Fatalf("SelectFile = %+v, want topfile", file)
	}
	dir := ft.SelectDir(r, "c1", true, DepthBucketWeights)
	if dir == nil || dir.FullPath != "topdir" {
		t.Fatalf("SelectDir = %+v, want topdir", dir)
	}
}

// TestGlusterfsCoverageDepths pins the coverage domain: top-level GlusterFS
// nodes populate bucket 1, so bucket 0 disappears from the run's domain.
func TestGlusterfsCoverageDepths(t *testing.T) {
	glusterfsCoverageConfig(t, false)
	if covBoard.depths[0] {
		t.Fatal("GlusterFS top-level nodes must not populate depth bucket 0")
	}
	if !covBoard.depths[1] {
		t.Fatal("GlusterFS top-level nodes must populate depth bucket 1")
	}
}
