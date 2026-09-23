package prog

import "testing"

func TestCanonicalHmdfsName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"file0_conflict_dev1", "file0"},
		{"./file0_conflict_dev1", "./file0"},
		{"dir/x_conflict_dev12", "dir/x"},
		{"a_conflict_dev1.txt", "a.txt"},
		{"a.b_conflict_dev2.c", "a.b.c"},
		{"d_remote_directory", "d"},
		{"dir/d_remote_directory", "dir/d"},
		{"d_remote_directory/child", "d/child"},
		{"./d_remote_directory/child", "./d/child"},
		{"a/d_remote_directory/b", "a/d/b"},
		{"d_remote_directory/d_remote_directory", "d/d"},
		{"./d_remote_directory/child_conflict_dev2.txt", "./d/child.txt"},
		{"file0", "file0"},
		{"a_conflict_dev.txt", "a_conflict_dev.txt"},
		{"a_conflict_devx.txt", "a_conflict_devx.txt"},
		{"_conflict_dev1", "_conflict_dev1"},
		{"_remote_directory", "_remote_directory"},
		{"", ""},
	}
	for _, c := range cases {
		if got := CanonicalHmdfsName(c.in); got != c.want {
			t.Errorf("CanonicalHmdfsName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalizeFsMd(t *testing.T) {
	orig := map[string]FileMetadata{
		"file0":                              {Checksum: 1},
		"file0_conflict_dev1":                {Checksum: 2},
		"only_copy_conflict_dev1":            {Checksum: 3},
		"multi_conflict_dev2":                {Checksum: 4},
		"multi_conflict_dev1":                {Checksum: 5},
		"d_remote_directory":                 {Checksum: 6},
		"d/child":                            {Checksum: 7},
		"d_remote_directory/child":           {Checksum: 8},
		"d_remote_directory/x_conflict_dev3": {Checksum: 10},
		"e_remote_directory/leaf":            {Checksum: 9},
	}
	got := CanonicalizeFsMd(orig)
	if got["file0"].Checksum != 1 {
		t.Errorf("original entry must win: got checksum %v", got["file0"].Checksum)
	}
	if got["only_copy"].Checksum != 3 {
		t.Errorf("copy fallback must be used when original missing: got checksum %v", got["only_copy"].Checksum)
	}
	if got["multi"].Checksum != 5 {
		t.Errorf("smallest dev id must win: got checksum %v", got["multi"].Checksum)
	}
	if got["d"].Checksum != 6 {
		t.Errorf("directory suffix must be canonicalized: got checksum %v", got["d"].Checksum)
	}
	if got["d/child"].Checksum != 7 {
		t.Errorf("original below renamed dir must win: got checksum %v", got["d/child"].Checksum)
	}
	if got["d/x"].Checksum != 10 {
		t.Errorf("nested file conflict must canonicalize: got checksum %v", got["d/x"].Checksum)
	}
	if got["e/leaf"].Checksum != 9 {
		t.Errorf("copy-only subtree must fall back: got checksum %v", got["e/leaf"].Checksum)
	}
	if orig["file0_conflict_dev1"].Checksum != 2 {
		t.Errorf("input map must not be modified")
	}
	if CanonicalizeFsMd(nil) != nil {
		t.Errorf("nil map must return nil")
	}
}
