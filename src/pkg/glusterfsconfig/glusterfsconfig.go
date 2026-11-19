// Package glusterfsconfig validates the fixed GlusterFS v1 deployment shape.
package glusterfsconfig

import (
	"fmt"
	"strings"
)

const (
	// V1Servers is the only supported server count for GlusterFS v1.
	V1Servers = 3
	// V1Clients is the only supported client count for GlusterFS v1.
	V1Clients = 3
	// V1FuzzingVMs is the only supported fuzzing_vms value for GlusterFS v1.
	V1FuzzingVMs = 6

	v1SetupParams = "3 3 'replica 3'"
)

// ValidateV1 checks the fixed GlusterFS v1 setup shape: three servers, three
// clients, six fuzzing VMs, and replica 3. The setup params must normalize to
// "3 3 'replica 3'" (single or double quotes around the mode are accepted).
func ValidateV1(params string, servNum, fuzzingVMs int) (servers, clients int, err error) {
	norm := strings.Join(strings.Fields(strings.ReplaceAll(params, "\"", "'")), " ")
	if norm != v1SetupParams {
		return 0, 0, fmt.Errorf("bad glusterfs params format: %q (want %q)", params, v1SetupParams)
	}
	if servNum != V1Servers {
		return 0, 0, fmt.Errorf("glusterfs v1 requires server_num %d (got %d)", V1Servers, servNum)
	}
	if fuzzingVMs != V1FuzzingVMs {
		return 0, 0, fmt.Errorf("glusterfs v1 requires fuzzing_vms %d (got %d)", V1FuzzingVMs, fuzzingVMs)
	}
	return V1Servers, V1Clients, nil
}
