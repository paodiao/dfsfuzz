package glusterfsconfig

import "testing"

func TestValidateV1(t *testing.T) {
	ok := []string{
		"3 3 'replica 3'",
		"3 3 \"replica 3\"",
		" 3   3   'replica 3' ",
	}
	for _, params := range ok {
		servers, clients, err := ValidateV1(params, 3, 6)
		if err != nil {
			t.Fatalf("params %q: unexpected error: %v", params, err)
		}
		if servers != 3 || clients != 3 {
			t.Fatalf("params %q: servers/clients = %d/%d, want 3/3", params, servers, clients)
		}
	}

	bad := []struct {
		params  string
		servNum int
		vms     int
	}{
		{"3 3", 3, 6},
		{"3 3 replica 3", 3, 6},
		{"3 2 'replica 3'", 3, 6},
		{"3 3 'replica 2'", 3, 6},
		{"3 3 'replica 3'", 2, 6},
		{"3 3 'replica 3'", 3, 5},
	}
	for _, tc := range bad {
		if _, _, err := ValidateV1(tc.params, tc.servNum, tc.vms); err == nil {
			t.Fatalf("params %q servNum=%d vms=%d: expected error", tc.params, tc.servNum, tc.vms)
		}
	}
}
