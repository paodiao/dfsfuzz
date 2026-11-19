package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"monarch/pkg/glusterfsconfig"
	"monarch/prog"
)

// loadGlusterfsConfig initializes the client-only GlusterFS view. All clients
// share one manifest/namespace; Cids is repeated only for length-compatible
// APIs and never identifies a client.
func loadGlusterfsConfig(hc *prog.Hmdfs_config, setupParams string, fuzzingVMs, servNum int, initDir string) error {
	serverCnt, clientCnt, err := glusterfsconfig.ValidateV1(setupParams, servNum, fuzzingVMs)
	if err != nil {
		return err
	}

	hc.Serv_num = serverCnt
	hc.ClientNum = clientCnt
	hc.TotalNum = fuzzingVMs
	hc.RootProgIdx = serverCnt
	hc.Node_num = clientCnt
	hc.RootPrefix = ""
	hc.Cids = make([]string, clientCnt)
	for i := range hc.Cids {
		hc.Cids[i] = "shared"
	}

	hc.Init_file = make(map[string][]string)
	hc.Init_dir = make(map[string][]string)
	hc.Init_tmpdir = make(map[string][]string)
	hc.Init_empty_dir = make(map[string][]string)
	hc.File_in_persistence_dir = make(map[string][]string)
	hc.FileSize = make(map[string]map[string]uint64)
	hc.Node_idx_of_persistence = -1
	hc.Persistence_dir = ""

	if initDir == "" {
		return nil
	}

	prefix := hc.NamespacePrefix()
	join := func(rel string) string {
		rel = strings.TrimSpace(rel)
		rel = strings.TrimPrefix(rel, "./")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return ""
		}
		if prefix == "" {
			return rel
		}
		return strings.TrimSuffix(prefix, "/") + "/" + rel
	}

	readList := func(name string) ([]string, error) {
		data, err := os.ReadFile(initDir + "/" + name)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			out = append(out, join(strings.Fields(line)[0]))
		}
		return out, nil
	}

	files, err := readList("shared.file")
	if err != nil {
		return fmt.Errorf("read shared.file: %w", err)
	}
	hc.Init_file["shared"] = files
	hc.FileSize["shared"] = make(map[string]uint64)

	if data, err := os.ReadFile(initDir + "/shared.file"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 2 {
				continue
			}
			sz, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				hc.FileSize["shared"][join(fields[0])] = sz
			}
		}
	}

	if dirs, err := readList("shared.dir"); err == nil {
		hc.Init_dir["shared"] = dirs
	}
	if dirs, err := readList("shared.tmpdir"); err == nil {
		hc.Init_tmpdir["shared"] = dirs
	}

	if data, err := os.ReadFile(initDir + "/empty_dirs.info"); err == nil {
		var current string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "node_id:") {
				current = strings.TrimSpace(strings.TrimPrefix(line, "node_id:"))
				continue
			}
			if current == "shared" {
				hc.Init_empty_dir[current] = append(hc.Init_empty_dir[current], join(line))
			}
		}
	}

	return nil
}
