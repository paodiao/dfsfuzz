package main

import (
	"strings"
	"testing"

	"monarch/prog"
)

func TestLoadGlusterfsConfigRequiresThreeClients(t *testing.T) {
	okCases := []struct {
		params  string
		servNum int
		vms     int
	}{
		{"3 3", 3, 6},
		{"3 3 'replica 3'", 3, 6},
	}
	for _, tc := range okCases {
		hc := &prog.Hmdfs_config{}
		if err := loadGlusterfsConfig(hc, tc.params, tc.vms, tc.servNum, ""); err != nil {
			t.Fatalf("params %q: unexpected error: %v", tc.params, err)
		}
		if hc.ClientNum != 3 || hc.Node_num != 3 || hc.Serv_num != 3 {
			t.Fatalf("params %q: ClientNum/Node_num/Serv_num = %d/%d/%d, want 3/3/3",
				tc.params, hc.ClientNum, hc.Node_num, hc.Serv_num)
		}
		if hc.RootProgIdx != 3 || len(hc.Cids) != 3 {
			t.Fatalf("params %q: RootProgIdx/Cids = %d/%d, want 3/3",
				tc.params, hc.RootProgIdx, len(hc.Cids))
		}
		for _, cid := range hc.Cids {
			if cid != "shared" {
				t.Fatalf("params %q: cid %q, want shared", tc.params, cid)
			}
		}
	}

	rejectCases := []struct {
		params  string
		servNum int
		vms     int
		wantErr string
	}{
		{"3 0", 3, 3, "exactly 3 clients"},
		{"3 1", 3, 4, "exactly 3 clients"},
		{"3 2", 3, 5, "exactly 3 clients"},
		{"3 4", 3, 7, "exactly 3 clients"},
		{"2 3", 3, 5, "does not match"},
		{"3 3", 3, 5, "fuzzing_vms"},
	}
	for _, tc := range rejectCases {
		hc := &prog.Hmdfs_config{}
		err := loadGlusterfsConfig(hc, tc.params, tc.vms, tc.servNum, "")
		if err == nil {
			t.Fatalf("params %q: expected error, got nil", tc.params)
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("params %q: error %q does not contain %q", tc.params, err, tc.wantErr)
		}
	}
}
