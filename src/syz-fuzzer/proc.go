// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"

	//"runtime/debug"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	//"sync"
	//"io/ioutil"
	"strconv"
	"strings"

	"monarch/checker"
	"monarch/pkg/cover"
	"monarch/pkg/hash"
	"monarch/pkg/ipc"
	"monarch/pkg/log"
	"monarch/pkg/osutil"
	"monarch/pkg/rpctype"
	"monarch/pkg/signal"
	"monarch/prog"

	"gonum.org/v1/gonum/stat/combin"
)

// Proc represents a single fuzzing process (executor).
type Proc struct {
	fuzzer            *Fuzzer
	pid               int
	env               *ipc.Env
	rnd               *rand.Rand
	execOpts          *ipc.ExecOpts
	execOptsCover     *ipc.ExecOpts
	execOptsComps     *ipc.ExecOpts
	execOptsNoCollide *ipc.ExecOpts
	execOptsLight     *ipc.ExecOpts
	freqCov           int32
	cltTick           chan bool
	hmcfg             prog.Hmdfs_config
	lcsFsops          *prog.LayeredChoiceStrategy
	tscoffs           []int64 // per-proc copy; only the owning goroutine reads/writes it
	lastDCTBoardLog   time.Time
}

func newProc(fuzzer *Fuzzer, pid int) (*Proc, error) {
	env, err := ipc.MakeEnv(fuzzer.config, pid, fuzzer.config.InitShmId)
	if err != nil {
		return nil, err
	}
	rnd := rand.New(rand.NewSource(time.Now().UnixNano() + int64(pid)*1e12))
	execOptsNoCollide := *fuzzer.execOpts
	execOptsNoCollide.Flags &= ^ipc.FlagCollide
	execOptsCover := execOptsNoCollide
	execOptsCover.Flags |= ipc.FlagCollectCover
	execOptsComps := execOptsNoCollide
	execOptsComps.Flags |= ipc.FlagCollectComps
	execOptsLight := execOptsCover
	execOptsLight.Flags |= ipc.FlagSkipFsMd

	freqCov := int32(10)
	cltTick := make(chan bool)

	go func() {
		ticker := time.NewTicker(3 * time.Minute).C
		for {
			select {
			case <-ticker:
				atomic.StoreInt32(&freqCov, 0)
			case <-cltTick:
				atomic.AddInt32(&freqCov, 1)
			}
		}
	}()
	var hc prog.Hmdfs_config
	hc.DfsName = fuzzer.config.DFSName
	hc.Profile = prog.ProfileForName(hc.DfsName)
	hc.TotalNum = fuzzer.config.FuzzingVMs
	hc.Serv_num = fuzzer.config.ServNum
	hc.RootProgIdx = hc.Serv_num
	hc.ClientNum = hc.TotalNum - hc.Serv_num
	hc.InitIp = fuzzer.config.InitIp
	if hc.DfsName == "hmdfs" {
		parts := strings.Split(fuzzer.config.DfsSetupParams, " ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad hmdfs params format: %v", fuzzer.config.DfsSetupParams)
		}
		nodeNumStr := parts[0]
		nodeNum, err := strconv.Atoi(nodeNumStr)
		if err != nil {
			fmt.Printf("parse hmdfs node num fail: %v\n", err)
			return nil, err
		}
		hc.Node_num = nodeNum
		idsStr := parts[1]
		cids := strings.Split(idsStr, ";")

		if nodeNum != len(cids) {
			return nil, fmt.Errorf("node num(%d) cannot match cid num(%d)", nodeNum, len(cids))
		}
		if nodeNum != fuzzer.config.FuzzingVMs {
			return nil, fmt.Errorf("node num(%d) does not match fuzzing_vms(%d)", nodeNum, fuzzer.config.FuzzingVMs)
		}
		// Degenerate configs would make the execution batch exceed the
		// executor count (e.g. generateProgsForConcurrentRW always emits two
		// programs), which the length guards then reject every round. Fail at
		// startup instead: hmdfs needs at least one server and one client.
		if nodeNum < 2 {
			return nil, fmt.Errorf("hmdfs requires at least 2 nodes (server + client), got %d", nodeNum)
		}

		for i, id := range cids {
			cids[i] = strings.TrimSpace(id)
		}
		hc.Cids = cids
		hc.Serv_num = fuzzer.config.ServNum
		hc.InitIp = fuzzer.config.InitIp

		// Load initial files and directories from InitDir
		hc.Init_file = make(map[string][]string)
		hc.Init_dir = make(map[string][]string)
		hc.Init_tmpdir = make(map[string][]string)
		hc.Init_empty_dir = make(map[string][]string)
		hc.File_in_persistence_dir = make(map[string][]string)
		hc.FileSize = make(map[string]map[string]uint64)

		initDir := fuzzer.config.InitialDir
		if initDir != "" {
			// Check if InitDir exists
			if _, err := os.Stat(initDir); os.IsNotExist(err) {
				log.Logf(0, "InitDir does not exist: %v", initDir)
			} else {
				for _, cid := range cids {
					// Load file list
					filePath := initDir + "/" + cid + ".file"
					if data, err := os.ReadFile(filePath); err != nil {
						log.Logf(0, "Failed to read file %s: %v", filePath, err)
					} else {
						lines := strings.Split(string(data), "\n")
						var files []string
						fileSizes := make(map[string]uint64)
						for _, line := range lines {
							line = strings.TrimSpace(line)
							if line == "" {
								continue
							}
							parts := strings.Split(line, " ")
							relPath := parts[0]
							mergePath := "merge_view/" + relPath
							files = append(files, mergePath)
							if len(parts) >= 2 {
								if sz, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
									fileSizes[mergePath] = sz
								}
							}
						}
						hc.Init_file[cid] = files
						hc.FileSize[cid] = fileSizes
						log.Logf(0, "Loaded %d initial files for node %s", len(files), cid)
						appendDiagLog("loaded init: %d initial files for node %s", len(files), cid)
					}

					// Load directory list
					dirPath := initDir + "/" + cid + ".dir"
					if data, err := os.ReadFile(dirPath); err != nil {
						log.Logf(0, "Failed to read file %s: %v", dirPath, err)
					} else {
						lines := strings.Split(string(data), "\n")
						var dirs []string
						for _, line := range lines {
							line = strings.TrimSpace(line)
							if line != "" {
								dirs = append(dirs, "merge_view/"+line)
							}
						}
						hc.Init_dir[cid] = dirs
						log.Logf(0, "Loaded %d initial directories for node %s", len(dirs), cid)
						appendDiagLog("loaded init: %d initial directories for node %s", len(dirs), cid)
					}

					// Load tmpdir list
					tmpdirPath := initDir + "/" + cid + ".tmpdir"
					if data, err := os.ReadFile(tmpdirPath); err != nil {
						log.Logf(0, "Failed to read file %s: %v", tmpdirPath, err)
					} else {
						lines := strings.Split(string(data), "\n")
						var tmpdirs []string
						for _, line := range lines {
							line = strings.TrimSpace(line)
							if line != "" {
								tmpdirs = append(tmpdirs, "merge_view/"+line)
							}
						}
						hc.Init_tmpdir[cid] = tmpdirs
						log.Logf(0, "Loaded %d initial tmpdirs for node %s", len(tmpdirs), cid)
						appendDiagLog("loaded init: %d initial tmpdirs for node %s", len(tmpdirs), cid)
					}
				}

				// Load empty directory list
				emptyDirsPath := initDir + "/empty_dirs.info"
				if emptyData, err := os.ReadFile(emptyDirsPath); err != nil {
					log.Logf(0, "Failed to read empty_dirs.info: %v", err)
				} else {
					var currentCid string
					lines := strings.Split(string(emptyData), "\n")
					for _, line := range lines {
						trimmed := strings.TrimSpace(line)
						if trimmed == "" {
							continue
						}
						if strings.HasPrefix(trimmed, "node_id:") {
							currentCid = strings.TrimSpace(strings.TrimPrefix(trimmed, "node_id:"))
						} else {
							dirPath := strings.TrimSpace(trimmed)
							hc.Init_empty_dir[currentCid] = append(hc.Init_empty_dir[currentCid], "merge_view/"+dirPath)
						}
					}
					totalEmpty := 0
					for _, dirs := range hc.Init_empty_dir {
						totalEmpty += len(dirs)
					}
					log.Logf(0, "Loaded %d empty directories across %d nodes", totalEmpty, len(hc.Init_empty_dir))
				}

				// Load persistence directory configuration
				persistPath := initDir + "/large_dir.info"
				if persistData, err := os.ReadFile(persistPath); err != nil {
					log.Logf(0, "Failed to read large_dir.info: %v", err)
				} else {
					var persistNodeId string
					var persistDir string
					var inFilesSection bool
					var persistFiles []string
					lines := strings.Split(string(persistData), "\n")
					for _, line := range lines {
						trimmed := strings.TrimSpace(line)
						if trimmed == "" {
							continue
						}
						if strings.HasPrefix(trimmed, "node_id:") {
							persistNodeId = strings.TrimSpace(strings.TrimPrefix(trimmed, "node_id:"))
						} else if strings.HasPrefix(trimmed, "dir_path:") {
							persistDir = strings.TrimSpace(strings.TrimPrefix(trimmed, "dir_path:"))
						} else if trimmed == "files:" {
							inFilesSection = true
						} else if inFilesSection {
							parts := strings.Split(trimmed, " ")
							filePath := strings.TrimSpace(parts[0])
							mergePath := "merge_view/" + filePath
							persistFiles = append(persistFiles, mergePath)
							if len(parts) >= 2 {
								if sz, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
									if hc.FileSize[persistNodeId] == nil {
										hc.FileSize[persistNodeId] = make(map[string]uint64)
									}
									hc.FileSize[persistNodeId][mergePath] = sz
								}
							}
						}
					}
					if persistNodeId != "" {
						for idx, cid := range cids {
							if cid == persistNodeId {
								hc.Node_idx_of_persistence = idx
								break
							}
						}
					}
					if persistDir != "" {
						hc.Persistence_dir = "merge_view/" + persistDir
					}
					if persistNodeId != "" && len(persistFiles) > 0 {
						hc.File_in_persistence_dir[persistNodeId] = persistFiles
					}
					log.Logf(0, "Loaded persistence config: node_idx=%d, dir=%s, files=%d",
						hc.Node_idx_of_persistence, hc.Persistence_dir, len(persistFiles))
					appendDiagLog("loaded persistence config: node_idx=%d dir=%s files=%d persistNodeId=%q persistDirRaw=%v persistLen=%d",
						hc.Node_idx_of_persistence, hc.Persistence_dir, len(persistFiles),
						persistNodeId, persistDir != "", len(persistDir))
				}
			}
		} else {
			log.Logf(0, "InitDir is not configured, skipping initial files loading")
		}
	} else if hc.DfsName == "glusterfs" {
		if err := loadGlusterfsConfig(&hc, fuzzer.config.DfsSetupParams,
			fuzzer.config.FuzzingVMs, fuzzer.config.ServNum, fuzzer.config.InitialDir); err != nil {
			return nil, err
		}
	}

	// The execution protocol is one program per executor; the manager builds
	// exactly FuzzingVMs executor commands and hc.Node_num is validated
	// against FuzzingVMs above. A mismatch here is a config/loader corruption
	// that would panic on the first batch, so fail cleanly at startup.
	if hc.DfsName == "hmdfs" && len(fuzzer.config.Executor) != hc.Node_num {
		return nil, fmt.Errorf("executor count %d does not match Node_num %d",
			len(fuzzer.config.Executor), hc.Node_num)
	}
	if hc.DfsName == "glusterfs" {
		if len(fuzzer.config.Executor) != hc.TotalNum {
			return nil, fmt.Errorf("executor count %d does not match TotalNum %d",
				len(fuzzer.config.Executor), hc.TotalNum)
		}
		if hc.Serv_num <= 0 || hc.ClientNum <= 0 {
			return nil, fmt.Errorf("glusterfs needs server and client programs")
		}
	}

	hc.FileTree = prog.NewFileTreeForConfig(&hc)
	hc.FileTree.InitFromHmdfsConfig(&hc)

	// Per-proc TSC offsets: seeded from the immutable fuzzer config slice and
	// refreshed in place from this proc's own exec report (see executeRaw).
	procTsc := append([]int64(nil), fuzzer.tscoffs...)
	var lcsFsops *prog.LayeredChoiceStrategy
	if hc.DfsName == "hmdfs" || hc.DfsName == "glusterfs" {
		lcsFsops = prog.NewLayeredChoiceStrategy(&hc, fuzzer.target)
		lcsFsops.SetTscOffsets(procTsc)
		prog.InitCoverageBoard(&hc)
		appendDiagLog("tscoffs=%v", procTsc)
	}

	proc := &Proc{
		fuzzer:            fuzzer,
		pid:               pid,
		env:               env,
		rnd:               rnd,
		execOpts:          fuzzer.execOpts,
		execOptsCover:     &execOptsCover,
		execOptsComps:     &execOptsComps,
		execOptsNoCollide: &execOptsNoCollide,
		execOptsLight:     &execOptsLight,
		freqCov:           freqCov,
		cltTick:           cltTick,
		hmcfg:             hc,
		lcsFsops:          lcsFsops,
		tscoffs:           procTsc,
	}
	return proc, nil
}

func (proc *Proc) loop() {
	generatePeriod := 20
	if proc.fuzzer.config.Flags&ipc.FlagSignal == 0 {
		// If we don't have real coverage signal, generate programs more frequently
		// because fallback signal is weak.
		generatePeriod = 2
	}
	for i := 0; ; i++ {
		item := proc.fuzzer.workQueue.dequeue()
		wq := proc.fuzzer.workQueue
		wq.mu.RLock()
		appendDiagLog("loop: work triage=%d triageCand=%d cand=%d smash=%d fsmash=%d",
			len(wq.triage), len(wq.triageCandidate), len(wq.candidate), len(wq.smash), len(wq.fsmash))
		wq.mu.RUnlock()
		if item != nil {
			switch item := item.(type) {
			case *WorkTriage:
				proc.triageInput(item)
			case *WorkCandidate:
				proc.execute(proc.execOpts, item.ps, item.flags, StatCandidate)
			case *WorkSmash:
				proc.smashInput(item)
			default:
				log.Fatalf("unknown work type: %#v", item)
			}
			continue
		}

		ct := proc.fuzzer.choiceTable
		fuzzerSnapshot := proc.fuzzer.snapshot()
		if len(fuzzerSnapshot.corpus) == 0 || i%generatePeriod == 0 {
			// Generate a new prog.
			//tao modified
			var ps []*prog.Prog
			if proc.fuzzer.config.DFSName == "hmdfs" {
				// Seed mix: stash 10% (fault-window orchestration) and the
				// merged fsops type 90% (single DCT/pattern generator).
				if proc.rnd.Intn(10) < 1 {
					ps = proc.fuzzer.target.GenerateProgsForHmdfsStash(proc.rnd, proc.fuzzer.sCalls, &proc.hmcfg)
				} else {
					ps = proc.fuzzer.target.GenerateProgsForHmdfsFsops(proc.rnd, proc.fuzzer.sCalls, &proc.hmcfg)
				}
				proc.execute(proc.execOpts, ps, ProgNormal, StatGenerate)
			} else if proc.fuzzer.config.DFSName == "glusterfs" {
				for idx := 0; idx < proc.fuzzer.config.ServNum; idx++ {
					p, _ := proc.fuzzer.target.Generate(proc.rnd, 0, nil, nil, true,
						proc.fuzzer.sCalls, proc.fuzzer.config.EnableC2san, &proc.hmcfg)
					ps = append(ps, p)
				}
				ps = append(ps, proc.fuzzer.target.GenerateProgsForDfsFsops(
					proc.rnd, proc.fuzzer.sCalls, &proc.hmcfg)...)
				proc.execute(proc.execOpts, ps, ProgNormal, StatGenerate)
			} else {
				//Generate empty subtestcase for servers
				for idx := 0; idx < proc.fuzzer.config.ServNum; idx++ {
					p, _ := proc.fuzzer.target.Generate(proc.rnd, 0, nil, nil, true, proc.fuzzer.sCalls,
						proc.fuzzer.config.EnableC2san, &proc.hmcfg)
					ps = append(ps, p)
				}
				//Generate subtestcases for clients
				subTsNum := proc.fuzzer.config.FuzzingVMs - proc.fuzzer.config.ServNum
				var files map[string]bool
				for idx := 0; idx < subTsNum; {
					repeatNum := proc.rnd.Intn(subTsNum-idx) + 1
					p, newFiles := proc.fuzzer.target.Generate(proc.rnd, prog.RecommendedCalls, ct, files, false,
						proc.fuzzer.sCalls, proc.fuzzer.config.EnableC2san, &proc.hmcfg)
					log.Logf(0, "%v", newFiles)
					files = newFiles
					ps = append(ps, p)
					for i := 0; i < repeatNum-1; i++ {
						cpyProg := p.Clone()
						ps = append(ps, cpyProg)
					}
					idx += repeatNum
				}
				//tao end
				log.Logf(1, "#%v: generated", proc.pid)
				proc.execute(proc.execOpts, ps, ProgNormal, StatGenerate)
			}

		} else {
			if proc.fuzzer.config.DFSName == "hmdfs" {
				// Mutate an existing hmdfs testcase via the DCT-based
				// structured mutation (falling back to standard Mutate).
				seedPS := fuzzerSnapshot.chooseProgram(proc.rnd)
				ps := prog.Clones(seedPS)
				proc.mutateHmdfs(ps, fuzzerSnapshot.corpus)
				log.Logf(1, "#%v: mutated", proc.pid)
				proc.execute(proc.execOpts, ps, ProgNormal, StatFuzz)
			} else if proc.fuzzer.config.DFSName == "glusterfs" {
				seedPS := fuzzerSnapshot.chooseProgram(proc.rnd)
				ps := prog.Clones(seedPS)
				proc.mutateDfs(ps, fuzzerSnapshot.corpus)
				log.Logf(1, "#%v: mutated", proc.pid)
				proc.execute(proc.execOpts, ps, ProgNormal, StatFuzz)
			} else {
				// Mutate an existing prog.
				//tao modified
				var ps []*prog.Prog
				seedPS := fuzzerSnapshot.chooseProgram(proc.rnd)
				if !seedPS[0].HasCrashFail && !seedPS[0].HasNetFail &&
					(proc.fuzzer.config.NetFailure || proc.fuzzer.config.NodeCrash) &&
					prog.OutOfWrap(proc.rnd, seedPS[0].Target, 1, 5) {
					ps = prog.Clones(seedPS)
					prog.RandomInsertFailure(ps, proc.fuzzer.config.ServNum, proc.rnd, proc.fuzzer.sCalls,
						proc.fuzzer.config.InitIp, &proc.hmcfg, proc.fuzzer.config.NodeCrash, proc.fuzzer.config.NetFailure)
					log.Logf(1, "#%v: random insert failure", proc.pid)
					proc.execute(proc.execOptsCover, ps, ProgNormal, StatFailureEnum)
				} else {
					for idx, tmp_p := range seedPS {
						p := tmp_p.Clone()
						if idx >= proc.fuzzer.config.ServNum {
							p.Mutate(proc.rnd, prog.RecommendedCalls, ct, fuzzerSnapshot.corpus,
								proc.fuzzer.sCalls, proc.fuzzer.config.ServNum,
								seedPS[0].HasCrashFail || seedPS[0].HasNetFail, proc.fuzzer.config.EnableC2san, &proc.hmcfg)
						} else {
						}
						ps = append(ps, p)
					}
					log.Logf(1, "#%v: mutated", proc.pid)
					proc.execute(proc.execOpts, ps, ProgNormal, StatFuzz)
				}
				//tao end
			}
		}
	}
}

type triageTargetState struct {
	target    TriageTarget
	pending   signal.Signal
	cliSignal signal.Signal
	srvSignal signal.Signal
	cliCover  cover.Cover
	srvCover  cover.Cover
}

func makeTriageTarget(info ipc.CallInfo, callIndex, subNum int) TriageTarget {
	// info.Signal points to the output shmem region, detach it before queueing.
	info.Signal = append([]uint32{}, info.Signal...)
	info.Cover = nil
	return TriageTarget{call: callIndex, subNum: subNum, info: info}
}

func dedupTriageTargets(targets []TriageTarget) []TriageTarget {
	if len(targets) < 2 {
		return targets
	}
	seen := make(map[[2]int]bool, len(targets))
	out := targets[:0]
	for _, target := range targets {
		key := [2]int{target.subNum, target.call}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, target)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].subNum != out[j].subNum {
			return out[i].subNum < out[j].subNum
		}
		return out[i].call < out[j].call
	})
	return out
}

func serializeTriageProgram(ps []*prog.Prog) ([][]byte, hash.Sig) {
	data := make([][]byte, 0, len(ps))
	var dataForHash []byte
	for _, p := range ps {
		ser := p.Serialize()
		data = append(data, ser)
		dataForHash = append(dataForHash, ser...)
	}
	return data, hash.Hash(dataForHash)
}

func collectTriageTargetResult(ps []*prog.Prog, infos []*ipc.ProgInfo, target TriageTarget,
	triageClient bool, srvNum int) (triageTargetState, bool) {
	if target.subNum < 0 || target.subNum >= len(infos) || infos[target.subNum] == nil {
		return triageTargetState{}, false
	}
	thisSignal, thisCover := getSignalAndCover(ps[target.subNum], infos[target.subNum], target.call)
	res := triageTargetState{target: target}
	if triageClient {
		res.cliSignal.Merge(thisSignal)
		res.cliCover.Merge(thisCover)
		for idx := 0; idx < srvNum && idx < len(infos); idx++ {
			if infos[idx] == nil {
				continue
			}
			sig, cov := getSignalAndCover(ps[idx], infos[idx], -1)
			res.srvSignal.Merge(sig)
			res.srvCover.Merge(cov)
		}
	} else {
		res.srvSignal.Merge(thisSignal)
		res.srvCover.Merge(thisCover)
		for idx := srvNum; idx < len(infos); idx++ {
			if infos[idx] == nil {
				continue
			}
			sig, cov := getAllSignalAndCover(ps[idx], infos[idx])
			res.cliSignal.Merge(sig)
			res.cliCover.Merge(cov)
		}
	}
	return res, true
}

func (proc *Proc) enqueueTriageTargets(ps []*prog.Prog, flags ProgTypes, targets []TriageTarget,
	triageClient bool) {
	targets = dedupTriageTargets(targets)
	if len(targets) == 0 {
		return
	}
	proc.fuzzer.workQueue.enqueue(&WorkTriage{
		ps:           prog.Clones(ps),
		targets:      targets,
		flags:        flags,
		triageClient: triageClient,
	})
}

func (proc *Proc) directDagTriage(ps []*prog.Prog, infos []*ipc.ProgInfo, subNum int, flags ProgTypes) {
	atomic.AddUint64(&proc.fuzzer.dagCorpusCount, 1)

	var cliSignal, srvSignal signal.Signal
	var cliCover, srvCover cover.Cover
	targetSignal, targetCover := getSignalAndCover(ps[subNum], infos[subNum], -1)
	cliSignal.Merge(targetSignal)
	cliCover.Merge(targetCover)
	srvNum := proc.fuzzer.config.ServNum
	for idx := 0; idx < srvNum && idx < len(infos); idx++ {
		if infos[idx] == nil {
			continue
		}
		sig, cov := getSignalAndCover(ps[idx], infos[idx], -1)
		srvSignal.Merge(sig)
		srvCover.Merge(cov)
	}

	data, sig := serializeTriageProgram(ps)
	proc.fuzzer.sendInputToManager(rpctype.RPCInput{
		Call:      rpctype.DagCall,
		Prog:      data,
		CliSignal: cliSignal.Serialize(),
		SrvSignal: srvSignal.Serialize(),
		SrvCover:  srvCover.Serialize(),
		CliCover:  cliCover.Serialize(),
		Timings:   prog.ProgTimings(ps),
	})

	added := proc.fuzzer.addInputToCorpus(ps, cliSignal, srvSignal, sig)
	if !added {
		return
	}
	atomic.AddUint64(&proc.fuzzer.dagCorpusEntries, 1)
	if flags&ProgSmashed == 0 {
		proc.fuzzer.workQueue.enqueue(&WorkSmash{ps, -1, subNum})
	}
}

func (proc *Proc) triageInput(item *WorkTriage) {
	log.Logf(1, "#%v: triaging type=%x", proc.pid, item.flags)
	if len(item.targets) == 0 {
		return
	}
	side := "server"
	if item.triageClient {
		side = "client"
	}
	logCallName := fmt.Sprintf("%s targets=%d", side, len(item.targets))
	appendDiagLog("triage begin: %s flags=%x", logCallName, item.flags)

	totalLen := 0
	for _, p := range item.ps {
		totalLen += len(p.Calls)
	}
	if totalLen == 0 {
		appendDiagLog("triage return: empty program %s", logCallName)
		return
	}

	data, sig := serializeTriageProgram(item.ps)
	proc.fuzzer.corpusMu.RLock()
	_, alreadyInCorpus := proc.fuzzer.corpusHashes[sig]
	proc.fuzzer.corpusMu.RUnlock()

	var states []triageTargetState
	for _, target := range item.targets {
		prio := signalPrio(item.ps[target.subNum], &target.info, target.call)
		inputSignal := signal.FromRaw(target.info.Signal, prio)
		pending := inputSignal
		if !alreadyInCorpus {
			pending = proc.fuzzer.corpusSignalDiff(inputSignal)
		}
		if pending.Empty() {
			continue
		}
		states = append(states, triageTargetState{target: target, pending: pending})
	}
	if len(states) == 0 {
		appendDiagLog("triage skip: corpusSignalDiff empty %s", logCallName)
		return
	}

	infos, _, _, err := proc.executeRaw(proc.execOptsLight, item.ps, StatTriage)
	if err != nil {
		log.Logf(0, "triage: execution error, aborting this input: %v", err)
		appendDiagLog("triage return: execution error %s", logCallName)
		return
	}

	srvNum := proc.fuzzer.config.ServNum
	valid := states[:0]
	for _, st := range states {
		target := st.target
		if target.subNum < 0 || target.subNum >= len(infos) || infos[target.subNum] == nil {
			continue
		}
		if !reexecutionSuccess(infos[target.subNum], &target.info, target.call) {
			continue
		}
		res, ok := collectTriageTargetResult(item.ps, infos, target, item.triageClient, srvNum)
		if !ok {
			continue
		}
		var targetSignal signal.Signal
		if item.triageClient {
			targetSignal = res.cliSignal
		} else {
			targetSignal = res.srvSignal
		}
		pending := st.pending.Intersection(targetSignal)
		if pending.Empty() && item.flags&ProgMinimized == 0 {
			continue
		}
		st.pending = pending
		st.cliSignal = res.cliSignal
		st.srvSignal = res.srvSignal
		st.cliCover = res.cliCover
		st.srvCover = res.srvCover
		valid = append(valid, st)
	}
	if len(valid) == 0 {
		appendDiagLog("triage return: no valid target %s", logCallName)
		return
	}

	allCliSignal := make(signal.Signal)
	allSrvSignal := make(signal.Signal)
	firstSubNum := -1
	hasClientCall := false
	timings := prog.ProgTimings(item.ps)
	for _, st := range valid {
		callName := ".extra"
		if st.target.call != -1 {
			callName = item.ps[st.target.subNum].Calls[st.target.call].Meta.Name
		}
		proc.fuzzer.sendInputToManager(rpctype.RPCInput{
			Call:      callName,
			Prog:      data,
			CliSignal: st.cliSignal.Serialize(),
			SrvSignal: st.srvSignal.Serialize(),
			SrvCover:  st.srvCover.Serialize(),
			CliCover:  st.cliCover.Serialize(),
			Timings:   timings,
		})
		allCliSignal.Merge(st.cliSignal)
		allSrvSignal.Merge(st.srvSignal)
		if firstSubNum < 0 {
			firstSubNum = st.target.subNum
		}
		if item.triageClient && st.target.call != -1 {
			hasClientCall = true
		}
	}

	log.Logf(2, "added new input for %s to corpus", logCallName)
	if hasClientCall {
		proc.cltTick <- true
	}
	added := proc.fuzzer.addInputToCorpus(item.ps, allCliSignal, allSrvSignal, sig)
	proc.fuzzer.corpusMu.RLock()
	corpusLen := len(proc.fuzzer.corpus)
	proc.fuzzer.corpusMu.RUnlock()
	appendDiagLog("triage success: %s added=%v corpus=%d total=%d", logCallName, added, corpusLen, totalLen)
	if added && item.flags&ProgSmashed == 0 {
		proc.fuzzer.workQueue.enqueue(&WorkSmash{item.ps, -1, firstSubNum})
	}
}

func reexecutionSuccess(info *ipc.ProgInfo, oldInfo *ipc.CallInfo, call int) bool {
	if info == nil {
		return false
	}
	if call != -1 {
		// Don't minimize calls from successful to unsuccessful.
		// Successful calls are much more valuable.
		if oldInfo.Errno == 0 && info.Calls[call].Errno != 0 {
			return false
		}
		return len(info.Calls[call].Signal) != 0
	}
	return len(info.Extra.Signal) != 0
}

func getSignalAndCover(p *prog.Prog, info *ipc.ProgInfo, call int) (signal.Signal, []uint32) {
	inf := &info.Extra
	if call != -1 {
		inf = &info.Calls[call]
	}
	return signal.FromRaw(inf.Signal, signalPrio(p, inf, call)), inf.Cover
}

func getAllSignalAndCover(p *prog.Prog, info *ipc.ProgInfo) (signals signal.Signal, covers []uint32) {
	for call, inf := range info.Calls {
		infp := &inf
		signals.Merge(signal.FromRaw(infp.Signal, signalPrio(p, infp, call)))
		covers = append(covers, infp.Cover...)
	}
	return
}

const smashIterations = 25 // upstream-aligned (pkg/fuzzer smashJob iters=25)

func (proc *Proc) smashInput(item *WorkSmash) {
	if proc.fuzzer.comparisonTracingEnabled && item.call != -1 {
		proc.executeHintSeed(item.ps, item.call, item.subNum)
	}
	root := proc.rootProg(item.ps)
	//
	fuzzerSnapshot := proc.fuzzer.snapshot()

	{
		calls := make([]int, len(item.ps))
		total := 0
		for i, p := range item.ps {
			calls[i] = len(p.Calls)
			total += len(p.Calls)
		}
		appendDiagLog("smash begin: fsops=%v stash=%v dcache=%v calls=%v total=%d",
			root != nil && root.IsFsops, root != nil && root.IsStash, root != nil && root.IsDCache,
			calls, total)
	}

	//Failure enumeration
	if (proc.fuzzer.config.NetFailure || proc.fuzzer.config.NodeCrash) &&
		root != nil && !(root.HasNetFail || root.HasCrashFail) {
		//proc.enumFailures(item.ps)
	}

	//Normal mutation
	for i := 0; i < smashIterations; i++ {
		ps := prog.Clones(item.ps)
		/*
		   Each time only mutate one sub-testcase because: If we do multiple mutations and only one of them trigger new
		   coverage, we can't know which one contributes to the coverage and thus the testcases will have redudant
		   syscalls. However, this non-mutual mutation might not generate testcases towards more interleavings.
		*/
		//Tao TODO
		log.Logf(0, "NetFailure, Node crash: %v %v", proc.fuzzer.config.NetFailure, proc.fuzzer.config.NodeCrash)
		proc.mutateDfs(ps, fuzzerSnapshot.corpus)
		infos := proc.execute(proc.execOpts, ps, ProgNormal, StatSmash)
		if infos == nil {
			// Execution error (executor crash/hang): stop smashing this
			// seed — the executor is restarted on the next Exec and the
			// fuzzer keeps running (its state is intact).
			appendDiagLog("smash break: execution error on iteration %d", i)
			break
		}
	}
}

// mutateHmdfs mutates a hmdfs testcase: seeds go through their type-specific
// structured mutator (stash/dcache/fsops); the fsops one is DCT-based (root
// call inserted into ps[0], concurrent calls spread over the other nodes).
// Everything falls back to the standard per-client Mutate.
func (proc *Proc) clientView(ps []*prog.Prog) []*prog.Prog {
	return proc.hmcfg.ClientView(ps)
}

// rootProg returns the first client program (GlusterFS) or ps[0] (HMDFS,
// where ServNum=0 makes the client view the whole batch).
func (proc *Proc) rootProg(ps []*prog.Prog) *prog.Prog {
	if p := proc.hmcfg.RootProg(ps); p != nil {
		return p
	}
	if len(ps) > 0 {
		return ps[0]
	}
	return nil
}

func (proc *Proc) mutateDfs(ps []*prog.Prog, corpus [][]*prog.Prog) {
	if proc.fuzzer.config.DFSName == "glusterfs" {
		cps := proc.clientView(ps)
		if len(cps) == 0 {
			return
		}
		mutated := prog.MutateFsopsWithDCT(cps, proc.rnd,
			proc.fuzzer.sCalls, &proc.hmcfg, proc.lcsFsops)
		if mutated {
			return
		}
		for _, p := range cps {
			p.Mutate(proc.rnd, prog.RecommendedCalls, proc.fuzzer.choiceTable, corpus,
				proc.fuzzer.sCalls, 0, false, proc.fuzzer.config.EnableC2san, &proc.hmcfg)
		}
		return
	}
	proc.mutateHmdfs(ps, corpus)
}

func (proc *Proc) mutateHmdfs(ps []*prog.Prog, corpus [][]*prog.Prog) {
	if len(ps) == 0 {
		return
	}
	srvNum := proc.fuzzer.config.ServNum
	mutated := false
	if ps[0].IsFsops {
		mutated = prog.MutateFsopsWithDCT(ps, proc.rnd,
			proc.fuzzer.sCalls, &proc.hmcfg, proc.lcsFsops)
	} else if ps[0].IsStash {
		mutated = prog.MutateStashProg(ps, proc.rnd, proc.fuzzer.choiceTable,
			proc.fuzzer.sCalls, &proc.hmcfg)
	} else if ps[0].IsDCache {
		mutated = prog.MutateDcacheProg(ps, proc.rnd, proc.fuzzer.choiceTable,
			proc.fuzzer.sCalls, &proc.hmcfg)
	}
	// Fallback: the structured mutators all declined, so give every client
	// prog a pass of the standard mutator instead of a single random one.
	// ps[0] is skipped only when it carries failure/barrier bookkeeping:
	// GeneralFailPos/GeneralBarrierPos live on ps[0] and there is no refresh
	// path for index 0 (see mutation.go RefreshGeneralFailPos), so a generic
	// Mutate would leave those positions stale. A clean seed has no such
	// tables and ps[0] is an ordinary node, so it is included.
	if !mutated && len(ps) > srvNum {
		start := srvNum
		if proc.fuzzer.config.DFSName == "hmdfs" && srvNum == 0 &&
			(ps[0].HasCrashFail || ps[0].HasNetFail ||
				len(ps[0].GeneralFailPos) > 0 || len(ps[0].GeneralBarrierPos) > 0) {
			start = 1
		}
		if start < len(ps) {
			hasFail := ps[0].HasCrashFail || ps[0].HasNetFail
			n := 0
			for idx := start; idx < len(ps); idx++ {
				ps[idx].Mutate(proc.rnd, prog.RecommendedCalls, proc.fuzzer.choiceTable, corpus,
					proc.fuzzer.sCalls, srvNum, hasFail,
					proc.fuzzer.config.EnableC2san, &proc.hmcfg)
				prog.RefreshGeneralFailPos(ps, idx)
				n++
			}
			appendDiagLog("fallback mutate: progs=%d range=[%d,%d) hasFail=%v", n, start, len(ps), hasFail)
			log.Logf(1, "#%v: fallback mutated %d subtestcases", proc.pid, n)
		}
	}
}

// feedbackDagPairs accounts the novel DAG pairs (already filtered by novelty)
// in the HB/CC counters. The board is fed separately, from every observation.
func (proc *Proc) feedbackDagPairs(newPairs []prog.DAGPair) {
	for _, p := range newPairs {
		if p.Temporal == prog.TemporalHB {
			atomic.AddUint64(&proc.fuzzer.dagHbPairCount, 1)
		} else {
			atomic.AddUint64(&proc.fuzzer.dagCcPairCount, 1)
		}
	}
}

// resolveDirectedPending settles the Phase 2b pending batch from the last
// directed insertion using the full (not only novel) DAG pairs of this
// execution. "Executed" means the DAG pipeline attributed the execution
// (DagComputed set: at least one event matched with a resolvable path, even
// with zero pairs, which is a genuine miss); rounds without attribution (no
// trace events, pipeline all-miss, triage/minimize, executor error) clear
// the pending without counting a miss.
func (proc *Proc) resolveDirectedPending(infos []*ipc.ProgInfo) {
	lcs := proc.lcsFsops
	if lcs == nil || len(lcs.PendingDirected) == 0 {
		return
	}
	pairs, observed := dagPairsObserved(infos)
	lcs.ResolveDirectedPending(pairs, proc.hmcfg.Persistence_dir, observed)
}

// dagPairsObserved extracts the execution's DAG pair payload and reports
// whether the DAG pipeline attributed this execution. DagComputed is set on
// the designated client info whenever at least one event was matched with a
// resolvable path (even with zero pairs), so an execution that produced no
// pair at all still resolves pending cells as genuine misses instead of being
// mistaken for "no evidence".
func dagPairsObserved(infos []*ipc.ProgInfo) (pairs []prog.DAGPair, observed bool) {
	for _, info := range infos {
		if info == nil {
			continue
		}
		if info.DagComputed {
			observed = true
		}
		if pairs == nil && info.DagPairs != nil {
			pairs = info.DagPairs
		}
	}
	return pairs, observed
}

// filterNewDagPairs returns the novel DAG pair types: pairs whose novelty
// bits are in newBits (DagSignal[k] corresponds to DagPairs[k]). Multiple
// pairs sharing the same hash (identical abstract features) are collapsed to
// one entry — feedback counts pair types, not instances, consistent with the
// set semantics of newBits and the dag pair signal statistics.
func filterNewDagPairs(info *ipc.ProgInfo, newBits signal.Signal) []prog.DAGPair {
	var res []prog.DAGPair
	if len(info.DagPairs) == 0 || len(info.DagSignal) != len(info.DagPairs) {
		return res
	}
	ser := newBits.Serialize()
	newSet := make(map[uint32]bool, len(ser.Elems))
	for _, e := range ser.Elems {
		newSet[uint32(e)] = true
	}
	seen := make(map[uint32]bool)
	for i, bit := range info.DagSignal {
		if newSet[bit] && !seen[bit] {
			seen[bit] = true
			res = append(res, info.DagPairs[i])
		}
	}
	return res
}

// countDagDepth tracks the depth-bucket distribution of vertices in novel
// DAG pairs (already filtered by novelty), to validate the depth bucket
// boundaries empirically.
func (proc *Proc) countDagDepth(newPairs []prog.DAGPair) {
	for _, p := range newPairs {
		if p.A != nil {
			proc.fuzzer.countDagDepthBucket(prog.DepthBucketOf(p.A.Path))
		}
		if p.B != nil {
			proc.fuzzer.countDagDepthBucket(prog.DepthBucketOf(p.B.Path))
		}
	}
}

/*
For partiton between a (server) and b (server/client),
check whether a has already has other partitions with other nodes.
If yes, combine them into the PartNodes.
Otherwise, add a new SrvFailInfo
*/
func updateComb(comb []prog.SrvFailInfo, node int, partNode int) []prog.SrvFailInfo {
	for _, item := range comb {
		if item.Srv == node {
			item.PartNodes = append(item.PartNodes, partNode)
			log.Logf(0, "updateComb: %v", comb)
			return comb
		}
	}
	comb = append(comb, prog.SrvFailInfo{Srv: node, PartNodes: []int{partNode}})
	return comb
}

func subset1(cluster []int, cnt int) (ret []int) {
	numMap := make(map[int]bool)
	rand.Seed(time.Now().UnixNano())
	length := len(cluster)
	for i := 0; i < cnt; i++ {
		idx := 0
		for true {
			idx = rand.Intn(length)
			if _, ok := numMap[cluster[idx]]; !ok {
				break
			}
		}
		numMap[cluster[idx]] = true
		ret = append(ret, cluster[idx])
	}
	return ret
}

func subset2(cluster []prog.Conn, cnt int) (ret []prog.Conn) {
	numMap := make(map[prog.Conn]bool)
	rand.Seed(time.Now().UnixNano())
	length := len(cluster)
	for i := 0; i < cnt; i++ {
		idx := 0
		for true {
			idx = rand.Intn(length)
			if _, ok := numMap[cluster[idx]]; !ok {
				break
			}
		}
		numMap[cluster[idx]] = true
		ret = append(ret, cluster[idx])
	}
	return ret
}

/*
genNodeCombs: generate all combinations of srvNum.
*/
func genNodeCombs(srvNum int) (combs [][]prog.SrvFailInfo) {
	//for sub := 1; sub <= srvNum; sub++ {
	for sub := 1; sub <= 1; sub++ {
		//Generate combinations
		idxCombs := combin.Combinations(srvNum, sub)
		for _, c := range idxCombs {
			comb := make([]prog.SrvFailInfo, 0)
			for _, i := range c {
				comb = append(comb, prog.SrvFailInfo{Srv: i, PartNodes: nil})
			}
			combs = append(combs, comb)
		}
	}
	log.Logf(0, "genNodeCombs: %v", combs)
	return combs
}

func genEdgeCombs(srvNum int, cltNum int) (combs [][]prog.SrvFailInfo) {

	conns := make([]prog.Conn, 0)
	//Generate edges
	for i := 0; i < srvNum; i++ {
		for j := i + 1; j < srvNum+cltNum; j++ {
			conns = append(conns, prog.Conn{From: i, To: j})
		}
	}

	//Combinations
	//for sub := 1; sub <= len(conns); sub++ {
	for sub := 1; sub <= 1; sub++ {
		for _, c := range combin.Combinations(len(conns), sub) {
			comb := make([]prog.SrvFailInfo, 0)
			for _, i := range c {
				if conns[i].From <= srvNum {
					comb = updateComb(comb, conns[i].From, conns[i].To)
				} else if conns[i].To <= srvNum {
					comb = updateComb(comb, conns[i].To, conns[i].From)
				}
			}
			combs = append(combs, comb)
		}
	}
	log.Logf(0, "combs: %v", combs)
	return combs
}

func (proc *Proc) enumInner(combs [][]prog.SrvFailInfo, ps []*prog.Prog, isCrashFail bool) {

	ch := make(chan []*prog.Prog)
	go func() {
		for _, srvComb := range combs {
			//connClts := getConnClts(srvComb, conns, proc.fuzzer.config.ServNum)
			//Insert failures between the servers srvComb and clients connected to the servComb.
			log.Logf(0, "enumInner comb: %v", srvComb)
			prog.InsertFailure(proc.rnd, prog.RecommendedCalls, proc.fuzzer.choiceTable, ps, srvComb, ch,
				proc.fuzzer.sCalls, ps[0].SyncIdx, isCrashFail, proc.fuzzer.config.InitIp,
				proc.fuzzer.config.ServNum, &proc.hmcfg)
		}
		close(ch)
	}()

	for ps1 := range ch {
		log.Logf(0, "failure smash: %v %v", ps1[0].HasCrashFail, ps1[0].HasNetFail)
		proc.execute(proc.execOptsCover, ps1, ProgNormal, StatFailureEnum)
	}
	log.Logf(0, "enumInner finish %v", isCrashFail)
}

func (proc *Proc) enumFailures(ps []*prog.Prog) {

	srvNum := proc.fuzzer.config.ServNum
	cltNum := len(ps) - srvNum

	log.Logf(0, "Crash failure smash:%v %v", ps[0].HasCrashFail, ps[0].HasNetFail)
	if !ps[0].HasCrashFail && proc.fuzzer.config.NodeCrash {
		combs := genNodeCombs(srvNum)
		proc.enumInner(combs, ps, true) //isCrashFailure
	}

	log.Logf(0, "Net failure smash: %v %v", ps[0].HasCrashFail, ps[0].HasNetFail)
	if !ps[0].HasNetFail && proc.fuzzer.config.NetFailure {
		combs := genEdgeCombs(srvNum, cltNum)
		log.Logf(0, "edge combs: %v", combs)
		proc.enumInner(combs, ps, false)
	}
}

func (proc *Proc) failCall(ps []*prog.Prog, call int, subNum int) {
	for nth := 1; nth <= 100; nth++ {
		log.Logf(1, "#%v: injecting fault into call %v/%v", proc.pid, call, nth)
		newProgs := prog.Clones(ps)
		newProgs[subNum].Calls[call].Props.FailNth = nth
		infos, _, _, err := proc.executeRaw(proc.execOpts, newProgs, StatSmash)
		if err != nil {
			log.Logf(0, "failCall: execution error, stopping fault injection: %v", err)
			break
		}
		if infos != nil && len(infos[proc.fuzzer.config.ServNum+subNum].Calls) > call && infos[proc.fuzzer.config.ServNum+subNum].Calls[call].Flags&ipc.CallFaultInjected == 0 {
			break
		}
	}
}

func (proc *Proc) executeHintSeed(ps []*prog.Prog, call int, subNum int) {
	log.Logf(1, "#%v: collecting comparisons on call %d", proc.pid, call)
	// Operate on a private clone: the hint run replaces CheckInfo pointers
	// (ipc) and writes ProcId through them (conc-fs), while the live corpus
	// entry is cloned and read concurrently by other procs.
	ps = prog.Clones(ps)
	// First execute the original program to dump comparisons from KCOV.
	infos := proc.execute(proc.execOptsComps, ps, ProgNormal, StatSeed)
	if infos == nil {
		return
	}

	// Then mutate the initial program for every match between
	// a syscall argument and a comparison operand.
	// Execute each of such mutants to check if it gives new coverage.
	comps := infos[subNum].Calls[call].Comps
	for i := 0; i < proc.fuzzer.config.ServNum; i++ {
		for k, v := range infos[i].Extra.Comps {
			if _, ok := comps[k]; !ok {
				comps[k] = v
			}
		}
	}
	log.Logf(0, "------ executing comparison hint: %d", len(comps))
	prog.MutateWithHints(ps, subNum, call, comps, func(ps []*prog.Prog) {
		log.Logf(1, "#%v: executing comparison hint", proc.pid)
		proc.execute(proc.execOpts, ps, ProgNormal, StatHint)
	})
}

func (proc *Proc) triageFailure(ps []*prog.Prog, infos []*ipc.ProgInfo) {

	var SrvCover, CliCover cover.Cover
	var inputCliSignal, inputSrvSignal, newSignal signal.Signal
	for i, info := range infos {
		if i < proc.fuzzer.config.ServNum {
			proc.fuzzer.checkNewSignal(nil, info)
			//callInfo := info.Extra
			//prio := signalPrio(ps[i], &callInfo, -1)
			//inputSignal := signal.FromRaw(callInfo.Signal, prio)
			//
			thisSignal, thisCover := getSignalAndCover(ps[i], info, -1)
			inputSrvSignal.Merge(thisSignal)
			SrvCover.Merge(thisCover)
			if proc.fuzzer.config.EnableSrvFb {
				newSignal.Merge(proc.fuzzer.corpusSignalDiff(thisSignal))
			}
		} else {
			proc.fuzzer.checkNewSignal(ps[i], info)
			for j, _ := range ps[i].Calls {
				//
				thisSignal, thisCover := getSignalAndCover(ps[i], info, j)
				inputCliSignal.Merge(thisSignal)
				CliCover.Merge(thisCover)
				//
				//callInfo := info.Calls[j]
				//prio := signalPrio(ps[i], &callInfo, j)
				//inputSignal := signal.FromRaw(callInfo.Signal, prio)
				if proc.fuzzer.config.EnableClientFb {
					newSignal.Merge(proc.fuzzer.corpusSignalDiff(thisSignal))
				}
			}
		}
	}
	if newSignal.Empty() {
		return
	}

	//stable signals
	for i := 0; i < 1; i++ {
		infos, _, _, err := proc.executeRaw(proc.execOptsLight, ps, StatTriage)
		if err != nil {
			log.Logf(0, "triageFailure: execution error, aborting: %v", err)
			return
		}
		var oneRunSig signal.Signal
		for idx, info := range infos {
			if idx >= proc.fuzzer.config.ServNum {
				thisSignal, thisCover := getAllSignalAndCover(ps[idx], info)
				inputCliSignal.Merge(thisSignal)
				CliCover.Merge(thisCover)
				if proc.fuzzer.config.EnableClientFb {
					oneRunSig.Merge(thisSignal)
				}
			}

			if idx < proc.fuzzer.config.ServNum {
				thisSignal, thisCover := getSignalAndCover(ps[idx], info, -1)
				inputSrvSignal.Merge(thisSignal)
				SrvCover.Merge(thisCover)
				if proc.fuzzer.config.EnableSrvFb {
					oneRunSig.Merge(thisSignal)
				}
			}
		}
		newSignal = newSignal.Intersection(oneRunSig)
		if newSignal.Empty() {
			return
		}
	}

	//sendToManager, saveToCorpus, sendToSmash
	var data [][]byte
	var dataForHash []byte
	for _, p := range ps {
		prog := p.Serialize()
		data = append(data, prog)
		dataForHash = append(dataForHash, prog...)
	}
	sig := hash.Hash(dataForHash)

	proc.fuzzer.sendInputToManager(rpctype.RPCInput{
		Call:      "failure",
		Prog:      data,
		CliSignal: inputCliSignal.Serialize(),
		SrvSignal: inputSrvSignal.Serialize(),
		SrvCover:  SrvCover.Serialize(),
		CliCover:  CliCover.Serialize(),
		Timings:   prog.ProgTimings(ps),
	})

	_ = proc.fuzzer.addInputToCorpus(ps, inputCliSignal, inputSrvSignal, sig)
	// WorkFSmash has no consumer: the failure-enumeration pipeline
	// (enumFailures in smashInput) is disabled, and failed inputs keep
	// being explored through the regular corpus/mutation path instead.
	// proc.fuzzer.workQueue.enqueue(&WorkFSmash{ps})
}

func (proc *Proc) useSrvCovNow() bool {
	// Always use server coverage (fixed value after experiments).
	return true
}

func (proc *Proc) execute(execOpts *ipc.ExecOpts, ps []*prog.Prog, flags ProgTypes, stat Stat) (infos []*ipc.ProgInfo) {
	defer func() {
		proc.resolveDirectedPending(infos)
	}()
	if len(ps) == 0 {
		return nil
	}
	rootProg := proc.rootProg(ps)
	if rootProg != nil {
		log.Logf(0, "HasCrashFail: %v, .HasNetFail: %v", rootProg.HasCrashFail, rootProg.HasNetFail)
	}
	infos, _, _, err := proc.executeRaw(execOpts, ps, stat)
	if err != nil {
		log.Logf(0, "execute: execution error (fuzzer survives): %v", err)
		return nil
	}
	if infos == nil {
		return nil
	}

	if stat == StatFailureEnum {
		//check new signal
		proc.triageFailure(ps, infos)
		return infos
	}

	servNum := proc.fuzzer.config.ServNum
	clientHasNew := false
	covEnqueues := 0
	dagDirect := 0
	var clientTargets, serverTargets []TriageTarget
	if proc.fuzzer.config.EnableClientFb {
		for idx, info := range infos {
			//TODO: how to check the signal from servers and clients
			if idx < servNum {
				continue
			} else {
				calls, extra := proc.fuzzer.checkNewSignal(ps[idx], info)
				appendDiagLog("feedback: idx=%d EnableClientFb=%v calls=%d extra=%v", idx, proc.fuzzer.config.EnableClientFb, len(calls), extra)
				for _, callIndex := range calls {
					clientTargets = append(clientTargets, makeTriageTarget(info.Calls[callIndex], callIndex, idx)) //idx -> subNum
					covEnqueues++
					clientHasNew = true
				}
				if extra {
					clientTargets = append(clientTargets, makeTriageTarget(info.Extra, -1, idx))
					covEnqueues++
				}
			}
		}
	} else {
		appendDiagLog("feedback: EnableClientFb=false (skipped)")
	}

	//(1). With failures, exploit server feedback
	//(2). Client doesn't have feedback for a while
	if proc.fuzzer.config.EnableSrvFb && ((!clientHasNew && proc.useSrvCovNow()) ||
		(rootProg != nil && (rootProg.HasNetFail || rootProg.HasCrashFail))) {
		log.Logf(0, "----- no new client coverage: %v, %v", clientHasNew, proc.fuzzer.config.EnableEval)
		for idx, info := range infos[:servNum] {
			_, extra := proc.fuzzer.checkNewSignal(nil, info)
			if extra {
				log.Logf(0, "----- enqueue testcases with server coveraged")
				serverTargets = append(serverTargets, makeTriageTarget(info.Extra, -1, idx))
				covEnqueues++
			}
		}
	}

	//DAG feedback (hmdfs): every execution discovering a novel operation
	//pair/schedule earns a corpus entry (the "energy" model), tracked in
	//dedicated stats separate from the coverage channel. Novel pairs also
	//feed back into the DCT tables (exploration tracking + yield reset).
	if proc.fuzzer.config.EnableDagFb && proc.fuzzer.config.DFSName == "hmdfs" {
		for idx, info := range infos {
			if idx < servNum || info == nil {
				continue
			}
			// Board learning is decoupled from the globally deduped novelty
			// bits (the manager pre-seeds them on restart): offer every
			// observed pair, the board's own first-sighting ledgers count it.
			if len(info.DagPairs) > 0 {
				prog.RecordCoveragePairs(info.DagPairs)
			}
			if len(info.DagSignal) == 0 {
				continue
			}
			newBits := proc.fuzzer.checkNewDagSignal(info.DagSignal)
			if n := newBits.Len(); n > 0 {
				atomic.AddUint64(&proc.fuzzer.dagPairCount, uint64(n))
				maxDag := proc.fuzzer.config.MaxDagCorpus
				if maxDag == 0 || atomic.LoadUint64(&proc.fuzzer.dagCorpusEntries) < uint64(maxDag) {
					proc.directDagTriage(ps, infos, idx, flags)
					dagDirect++
				}
				newPairs := filterNewDagPairs(info, newBits)
				proc.feedbackDagPairs(newPairs)
				proc.countDagDepth(newPairs)
				var newRetCombos [6][6]int
				for _, p := range newPairs {
					if p.A != nil && p.B != nil {
						newRetCombos[int(p.A.RetBucket)][int(p.B.RetBucket)]++
					}
				}
				appendDiagLog("hmdfs dag novel: idx=%d bits=%d pairs=%d newRet=[%s]",
					idx, n, len(newPairs), dagRetComboString(newRetCombos))
			}
			if proc.fuzzer.config.EnableDagScheduleFb &&
				proc.fuzzer.checkNewDagSchedule(info.DagScheduleBit) > 0 {
				atomic.AddUint64(&proc.fuzzer.dagSchedCount, 1)
			}
		}
	}

	proc.enqueueTriageTargets(ps, flags, clientTargets, true)
	proc.enqueueTriageTargets(ps, flags, serverTargets, false)

	//Execute again for crash consistency bugs
	//TODO
	if proc.fuzzer.config.EnableC2san &&
		(stat == StatFuzz || stat == StatSmash || stat == StatHint || stat == StatGenerate) {
		r := prog.NewRand(ps[0].Target, proc.rnd)
		//crash all
		ps1 := prog.ProgCrashAll(ps, proc.fuzzer.config.ServNum, r, proc.fuzzer.sCalls)
		proc.executeRaw(execOpts, ps1, stat)
		if proc.fuzzer.config.ServNum > 1 {
			//random crash with proc.fuzzer.config.ServNum times
			for i := 0; i < proc.fuzzer.config.ServNum; i++ {
				ps1 = prog.ProgCrashRand(ps, proc.fuzzer.config.ServNum, r, proc.fuzzer.sCalls)
				proc.executeRaw(execOpts, ps1, stat)
			}
		}
	}

	// Outcome summary: per-node call counts (growth tracking, all nodes not
	// just ps[0]) plus which feedback channel the product earned.
	{
		calls := make([]int, len(ps))
		total := 0
		for i, p := range ps {
			calls[i] = len(p.Calls)
			total += len(p.Calls)
		}
		appendDiagLog("exec outcome: stat=%v calls=%v total=%d covEnq=%d dagDirect=%d",
			statNames[stat], calls, total, covEnqueues, dagDirect)
	}

	return infos
}

func (proc *Proc) executeRaw(opts *ipc.ExecOpts, ps []*prog.Prog, stat Stat) ([]*ipc.ProgInfo, []map[string]prog.FileMetadata, uint64, error) {

	if len(ps) == 0 {
		return nil, nil, 0, nil
	}
	rootProg := proc.rootProg(ps)

	if opts.Flags&ipc.FlagDedupCover == 0 {
		log.Fatalf("dedup cover is not enabled")
	}

	// The hmdfs execution model is one program per executor: a batch of any
	// other length means a producer regression (or a corrupted executor
	// config), which would otherwise panic inside ipc.parseOutputs. Record
	// the producing context and abort the round as a clean execution error.
	if proc.fuzzer.config.DFSName == "hmdfs" {
		if want := len(proc.fuzzer.config.Executor); len(ps) != want {
			err := fmt.Errorf("hmdfs batch has %d programs, want %d (executors)", len(ps), want)
			log.Logf(0, "MUTATION-DEBUG: %v (stat=%v lastMut=%s)", err, stat, prog.LastMutation())
			appendDiagLog("MUTATION-DEBUG: %v stat=%d lastMut=%s", err, stat, prog.LastMutation())
			return nil, nil, 0, err
		}
	}
	if proc.fuzzer.config.DFSName == "glusterfs" {
		want := len(proc.fuzzer.config.Executor)
		if len(ps) != want {
			return nil, nil, 0, fmt.Errorf("glusterfs batch has %d programs, want %d", len(ps), want)
		}
		for i := 0; i < proc.fuzzer.config.ServNum && i < len(ps); i++ {
			if len(ps[i].Calls) != 0 {
				return nil, nil, 0, fmt.Errorf("glusterfs server program %d is not empty", i)
			}
		}
		if proc.fuzzer.config.ServNum >= len(ps) {
			return nil, nil, 0, fmt.Errorf("glusterfs batch has no client programs")
		}
	}

	for _, p := range ps {
		proc.fuzzer.checkDisabledCalls(p)
	}

	// Guard against broken cross-call references produced by mutators
	// (dangling/forward fd references crash Serialize with "no result"
	// because serializer vars are registered in call order). Abort the
	// execution and record the program for mutation-path localization.
	for i, p := range ps {
		if p.HasBrokenRefs() {
			log.Logf(0, "MUTATION-DEBUG: broken refs before execute, copy %d (stat=%v) lastMut=%s: aborting execution",
				i, stat, prog.LastMutation())
			refDiag := p.DumpRefDiagnosis()
			appendDiagLog("MUTATION-DEBUG: broken refs copy %d stat=%d lastMut=%s meta: IsStash=%v IsDCache=%v IsFsops=%v\n%s",
				i, stat, prog.LastMutation(), rootProg != nil && rootProg.IsStash,
				rootProg != nil && rootProg.IsDCache, rootProg != nil && rootProg.IsFsops, refDiag)
			return nil, nil, 0, fmt.Errorf("broken refs in program copy %d", i)
		}
	}

	// Limit concurrency window and do leak checking once in a while.
	ticket := proc.fuzzer.gate.Enter()
	defer proc.fuzzer.gate.Leave(ticket)

	rootCallCount := 0
	if rootProg != nil {
		rootCallCount = len(rootProg.Calls)
	}
	if rootProg != nil && (rootProg.HasCrashFail || rootProg.HasNetFail || proc.fuzzer.config.EnableCsan) {
		local := *opts
		local.Flags &^= ipc.FlagCollide | ipc.FlagThreaded
		opts = &local
		log.Logf(0, "disable threaded and collide")
	}

	proc.logProgram(opts, ps)

	atomic.AddUint64(&proc.fuzzer.stats[stat], 1)

	var output []byte
	var infos []*ipc.ProgInfo
	var hanged bool
	var err error
	var fsMds []map[string]prog.FileMetadata
	var testdirIno uint64
	var hmdfsTraceEvents []prog.HmdfsTraceEvent
	var tscRatio float64

	output, infos, hanged, err, fsMds, testdirIno, hmdfsTraceEvents, tscRatio, _ = proc.env.Exec(opts, ps)

	// Execution errors (executor crash/hang/parse failure) bubble up instead
	// of killing the fuzzer: the executor is restarted on the next Exec
	// (env.cmd==nil → makeCommand), so the fuzzer survives and keeps its
	// workQueue/corpus/tree state. VM-level crashes and CSAN failures keep
	// their existing spawn-style handling.
	if err != nil {
		log.Logf(0, "execution error (executor will be restarted): %v\n", err)
		return nil, nil, 0, err
	}
	log.Logf(2, "result hanged=%v: %s", hanged, output)

	prog.SetMfTolTicksFromRatio(tscRatio)

	// The VM's KVM tsc_offset is fixed for one QEMU lifetime: the manager
	// injects it per fuzzer launch (config.TSCOFF), each Proc keeps a private
	// copy for matchEventToCall, and the executor converts events with the
	// same argv value. There is no in-process refresh path: a Go-side-only
	// update would desynchronize the call windows from the events the
	// executor already converted with its own copy.

	csanPassed, csanDiffs := true, []string(nil)
	// Triage/minimize rounds judge candidates purely by coverage/signal
	// equivalence (Monarch §3.4): the semantic checker is not part of the
	// reduction loop, so skip it there to keep each reduce step cheap.
	// Covers both StatTriage (input validation) and StatMinimize (the
	// prog.Minimize verification loop inside triageInput).
	if proc.fuzzer.config.EnableCsan && stat != StatTriage && stat != StatMinimize {
		csanPassed, csanDiffs = checker.ConcFSCheck(ps, infos, fsMds, proc.fuzzer.config.ServNum,
			proc.fuzzer.config.DFSName, proc.fuzzer.config.DfsSetupParams,
			proc.fuzzer.config.InitIp, testdirIno, proc.hmcfg.FileTree)
		if !csanPassed {
			log.Logf(0, "Concurrent semantic checker detects a bug")
			proc.saveCsanBug(ps, output, csanDiffs, fsMds, stat, infos)
			// The file tree across nodes is now inconsistent; continuing to
			// fuzz on it would poison later executions. Exit cleanly so that
			// the manager stops all VMs and restarts the whole group — qemu
			// -snapshot restores the tree to its initial image state.
			log.Logf(0, "==== CSAN BUG: restarting all VMs to restore the file tree ====")
			os.Exit(0)
		}
	}

	// ===== DISABLED: FileTree sync (per-exec restore keeps baseline) =====
	// clientIdx := -1
	// if csanPassed && proc.fuzzer.config.DFSName == "hmdfs" && len(ps) > 0 && len(fsMds) > proc.fuzzer.config.ServNum {
	// 	clientIdx = len(fsMds) - 1
	// 	for ; clientIdx >= proc.fuzzer.config.ServNum; clientIdx-- {
	// 		if fsMds[clientIdx] != nil && len(fsMds[clientIdx]) > 0 {
	// 			break
	// 		}
	// 	}
	// 	if clientIdx >= proc.fuzzer.config.ServNum {
	// 		ownerMap := collectCreateCallOwners(ps, infos, proc.hmcfg.Cids, &proc.hmcfg)
	// 		prog.SyncFileTreeFromFsMd(prog.CanonicalizeFsMd(fsMds[clientIdx]), ownerMap, &proc.hmcfg)
	// 	}
	// }
	// ===== END DISABLED: FileTree sync =====

	// DAG feedback: pair novelty bits + schedule bit (only for regular
	// executions; triage and minimize re-runs are excluded to avoid
	// timing-jitter noise and candidate-program feedback pollution).
	// Does not depend on csan/fsMds: call-window matching needs neither, and
	// ino-based resolution (writepage/lookup) degrades gracefully without fsMd.
	log.Logf(0, "hmdfs trace events=%d csanPassed=%v", len(hmdfsTraceEvents), csanPassed)
	failHit, failSkip := prog.FailShapeStats()
	directedPicks, directedScore := prog.DirectedPickStats()
	dcPlans, dcHits, dcMisses, dcNoPlans, dcInsertFails := prog.DirectedCellStats()
	dcBatches := prog.DirectedBatchStats()
	appendDiagLog("hmdfs trace events=%d csanPassed=%v stat=%d tscRatio=%f dataMiss=%d anchorMiss=%d alignMiss=%d failShape=%d/%d directed=%d/%d directCell=%d/%d/%d/%d/%d dcBatch=%d cfgErr=%d lastMut=%s",
		len(hmdfsTraceEvents), csanPassed, stat, tscRatio, prog.DataMutateMissCount(), prog.AnchorMissCount(), prog.TimingAlignMissCount(), failHit, failSkip,
		directedPicks, directedScore, dcPlans, dcHits, dcMisses, dcNoPlans, dcInsertFails, dcBatches, prog.DirectedCfgErrCount(), prog.LastMutation())
	if proc.fuzzer.config.DFSName == "hmdfs" && proc.lcsFsops != nil {
		if now := time.Now(); now.Sub(proc.lastDCTBoardLog) >= dctBoardLogInterval {
			proc.lastDCTBoardLog = now
			covFound, covTarget := prog.CoverageStats()
			appendDiagLog("dct pool: cover=%d/%d unreach=%d cellUnreach=%d", covFound, covTarget, prog.UnreachableComboCount(), prog.CellUnreachableCount())
		}
	}
	if stat != StatTriage && stat != StatMinimize && csanPassed && proc.fuzzer.config.DFSName == "hmdfs" &&
		len(hmdfsTraceEvents) > 0 && len(infos) > proc.fuzzer.config.ServNum {
		// DAG vertices intentionally use RAW fsMds (no CanonicalizeFsMd):
		// conflict/remote-directory suffixes are kernel-visible truth here,
		// and DAG paths never flow back into generation. The checker
		// canonicalizes fsMd, while FileTree stays at the restored baseline.
		vertices, dagDiag := prog.BuildVertices(hmdfsTraceEvents, ps, fsMds, &proc.hmcfg, proc.tscoffs)
		hbPairs, ccPairs := prog.ExtractPairs(vertices, dagDiag)
		allPairs := append(hbPairs, ccPairs...)
		pairBits, schedBit := prog.ComputeFeedback(hbPairs, ccPairs, &proc.hmcfg, dagDiag)
		log.Logf(0, "hmdfs dag: vertices=%d ps0Len=%d hbPairs=%d ccPairs=%d pairBits=%d | events=%d matchFail=%d progIdxBad=%d pathDrop=%d perNode=%v perFunc=%v | mfFunc=%v mfRetOK=%v mfNoFunc=%d mfNoCI=%d mfTime=%d mfByNode=%v mfLate=%d mfEarly=%d mfGap=%d mfShift=%d mfDistIn=%v mfDistOut=%v tol=%d | pairs=%d overlap=%d hbFwd=%d hbRev=%d filtNoMod=%d filtPathRel=%d bitsUnique=%d",
			len(vertices), rootCallCount, len(hbPairs), len(ccPairs), len(pairBits),
			dagDiag.Events, dagDiag.MatchFailed, dagDiag.ProgIdxBad, dagDiag.PathEmpty, dagDiag.PerNodeVertices, dagDiag.PerFuncVertices,
			dagDiag.MatchFailFunc, dagDiag.MatchFailRetOK, dagDiag.MatchFailNoFunc, dagDiag.MatchFailNoCI, dagDiag.MatchFailTime,
			dagDiag.MatchFailByNode, dagDiag.MFTimeLate, dagDiag.MFTimeEarly, dagDiag.MFGap, dagDiag.MFShift,
			dagDiag.MatchDistIn, dagDiag.MatchDistOut, prog.MfTolTicks(),
			dagDiag.TotalPairs, dagDiag.OverlapPairs, dagDiag.HBForwardPairs, dagDiag.HBReversePairs,
			dagDiag.FilteredNoMod, dagDiag.FilteredPathRel, dagDiag.PairBitsUnique)
		appendDiagLog("hmdfs dag: vertices=%d ps0Len=%d hbPairs=%d ccPairs=%d pairBits=%d | events=%d matchFail=%d progIdxBad=%d pathDrop=%d perNode=%v perFunc=%v | mfFunc=%v mfRetOK=%v mfNoFunc=%d mfNoCI=%d mfTime=%d mfByNode=%v mfLate=%d mfEarly=%d mfGap=%d mfShift=%d mfDistIn=%v mfDistOut=%v tol=%d | pairs=%d overlap=%d hbFwd=%d hbRev=%d filtNoMod=%d filtPathRel=%d bitsUnique=%d",
			len(vertices), rootCallCount, len(hbPairs), len(ccPairs), len(pairBits),
			dagDiag.Events, dagDiag.MatchFailed, dagDiag.ProgIdxBad, dagDiag.PathEmpty, dagDiag.PerNodeVertices, dagDiag.PerFuncVertices,
			dagDiag.MatchFailFunc, dagDiag.MatchFailRetOK, dagDiag.MatchFailNoFunc, dagDiag.MatchFailNoCI, dagDiag.MatchFailTime,
			dagDiag.MatchFailByNode, dagDiag.MFTimeLate, dagDiag.MFTimeEarly, dagDiag.MFGap, dagDiag.MFShift,
			dagDiag.MatchDistIn, dagDiag.MatchDistOut, prog.MfTolTicks(),
			dagDiag.TotalPairs, dagDiag.OverlapPairs, dagDiag.HBForwardPairs, dagDiag.HBReversePairs,
			dagDiag.FilteredNoMod, dagDiag.FilteredPathRel, dagDiag.PairBitsUnique)
		appendDiagLog("hmdfs dag marg: ret=%v err=[%s] dep=%v off=%v persist=%v type=%v retFunc=[%s] pairRet=[%s] rel=[%s] offCall=[%s] typeFunc=[%s] inoFunc=[%s]",
			dagDiag.RetHist, dagErrHistString(dagDiag.ErrHist), dagDiag.DepthHist, dagDiag.OffHist,
			dagDiag.PersistHist, dagDiag.TypeHist,
			dagRetFuncString(dagDiag.RetByFunc), dagRetComboString(dagDiag.PairRetCombos),
			dagRelHistString(dagDiag.RelHist),
			dagOffCallString(dagDiag.OffByCall), dagTypeFuncString(dagDiag.TypeByFunc),
			dagInoFuncString(dagDiag.InoByFunc))
		if len(dagDiag.MFSamples) > 0 {
			samples := ""
			for i, s := range dagDiag.MFSamples {
				if i > 0 {
					samples += " | "
				}
				samples += s
			}
			log.Logf(0, "hmdfs mf samples: %v", samples)
			appendDiagLog("hmdfs mf samples: %v", samples)
		}
		// The DAG pipeline attributed this execution (at least one event
		// matched a call with a resolvable path): mark the designated client
		// info even when no pair formed, so the 2b pending resolver can tell
		// "observed, nothing produced" (a genuine miss) from "not observed at
		// all" (pipeline failure: zero usable vertices -> clear without a
		// miss).
		if len(vertices) > 0 {
			for _, info := range infos[proc.fuzzer.config.ServNum:] {
				if info == nil {
					continue
				}
				info.DagComputed = true
				if len(pairBits) > 0 {
					info.DagSignal = pairBits
					info.DagScheduleBit = schedBit
					info.DagPairs = allPairs
				}
				break
			}
		}
	}

	return infos, fsMds, testdirIno, nil
}

// Temporary DAG/triage-pipeline diagnostic log on the host (hardcoded path;
// dctBoardLogInterval throttles the periodic coverage-board snapshot in dag.log.
const dctBoardLogInterval = 5 * time.Minute

// remove after the DAG feedback / corpus growth issues are diagnosed).
var dagDiagLog *os.File
var dagDiagMu sync.Mutex

func appendDiagLog(format string, args ...interface{}) {
	dagDiagMu.Lock()
	defer dagDiagMu.Unlock()
	if dagDiagLog == nil {
		f, err := os.OpenFile("/home/user/dfsfuzz/dag.log",
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Logf(0, "dag log open failed: %v", err)
			return
		}
		dagDiagLog = f
	}
	fmt.Fprintf(dagDiagLog, "%s %s\n",
		time.Now().Format("2006/01/02 15:04:05"), fmt.Sprintf(format, args...))
}

// dagErrHistString renders the negative-ret histogram (index = -ret-1) as
// ascending "-errno:count" entries, zero cells omitted ("" when all zero).
func dagErrHistString(h [64]int) string {
	s := ""
	for i, c := range h {
		if c == 0 {
			continue
		}
		if s != "" {
			s += ","
		}
		s += fmt.Sprintf("-%d:%d", i+1, c)
	}
	return s
}

// dagRetFuncString renders nonzero (funcID x retBucket):count entries in
// ascending func/ret order ("" when none).
func dagRetFuncString(m [prog.FuncCount][6]int) string {
	s := ""
	for f := range m {
		for r := range m[f] {
			c := m[f][r]
			if c == 0 {
				continue
			}
			if s != "" {
				s += ","
			}
			s += fmt.Sprintf("%dx%d:%d", f, r, c)
		}
	}
	return s
}

// dagRetComboString renders nonzero (retA x retB):count pair-combo entries in
// ascending order ("" when none).
// dagRelHistString renders the produced-pair PathRel histogram as
// "name:count" entries in enum order, zero cells omitted ("" when all
// zero). Used to observe geometric-relation coverage and to verify the
// entry-ino fix (sameInode should only reflect real same-inode pairs).
func dagRelHistString(h [5]int) string {
	names := [5]string{"samePath", "sameInode", "parentChild", "sameParent", "none"}
	s := ""
	for i, c := range h {
		if c == 0 {
			continue
		}
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%s:%d", names[i], c)
	}
	return s
}

// dagOffCallString renders the per-offset-call bucket histogram as
// "call:NA/zero/mid/tail/beyond" rows in the fixed OffByCall order, all-zero
// rows omitted ("" when none). Verifies the entry-position semantics:
// sequential write/read after a fresh open should land in the zero bucket,
// while pwrite64/pread64 explicit offsets reach every bucket.
func dagOffCallString(m [6][5]int) string {
	s := ""
	for r := range m {
		sum := 0
		for _, c := range m[r] {
			sum += c
		}
		if sum == 0 {
			continue
		}
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%s:%d/%d/%d/%d/%d", prog.OffCallNames[r],
			m[r][0], m[r][1], m[r][2], m[r][3], m[r][4])
	}
	return s
}

// dagTypeFuncString renders the event-time type (dir/file) of the
// type-sensitive functions, nonzero rows only ("" when none). Verifies the
// i_mode fix: namespace-op types come from the event, not from the
// post-execution tree state.
func dagTypeFuncString(m [prog.FuncCount][2]int) string {
	fids := []uint32{prog.FuncMkdir, prog.FuncCreate, prog.FuncRmdir, prog.FuncUnlink,
		prog.FuncRename, prog.FuncDirOpen, prog.FuncIterate, prog.FuncLookup,
		prog.FuncLink, prog.FuncSymlink, prog.FuncReadlink, prog.FuncGetxattr, prog.FuncSetxattr}
	s := ""
	for _, fid := range fids {
		d, f := m[fid][1], m[fid][0]
		if d == 0 && f == 0 {
			continue
		}
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%s:%d/%d", prog.HmdfsFuncName[fid], d, f)
	}
	return s
}

// dagInoFuncString renders the entry-ino capture mode of the namespace
// functions as "call:zero/zeroOK/ne/eq" (EntryIno==0, EntryIno==0 while the
// operation succeeded, EntryIno!=Ino, EntryIno==Ino), nonzero rows only ("").
// Verifies the entry_ino fix: eq should stay ~0 and ne should dominate, while
// zeroOK is the capture-failure alarm — a successful create/mkdir must have
// produced an entry inode, so zeroOK>0 for them means the capture is broken
// (LOOKUP zeroOK is legitimate: negative lookups return no dentry).
func dagInoFuncString(m [prog.FuncCount][4]int) string {
	fids := []uint32{prog.FuncMkdir, prog.FuncCreate, prog.FuncRename, prog.FuncLookup}
	s := ""
	for _, fid := range fids {
		if m[fid][0] == 0 && m[fid][1] == 0 && m[fid][2] == 0 && m[fid][3] == 0 {
			continue
		}
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%s:%d/%d/%d/%d", prog.HmdfsFuncName[fid],
			m[fid][0], m[fid][1], m[fid][2], m[fid][3])
	}
	return s
}

func dagRetComboString(m [6][6]int) string {
	s := ""
	for a := range m {
		for b := range m[a] {
			c := m[a][b]
			if c == 0 {
				continue
			}
			if s != "" {
				s += ","
			}
			s += fmt.Sprintf("%dx%d:%d", a, b, c)
		}
	}
	return s
}

// saveCsanBug dumps everything needed to locate/reproduce/analyze a
// consistency failure: the seeds of all nodes, the raw executor output, the
// differences and the involved files' full metadata.
func (proc *Proc) saveCsanBug(ps []*prog.Prog, output []byte, diffs []string,
	fsMds []map[string]prog.FileMetadata, stat Stat, infos []*ipc.ProgInfo) {
	root := proc.rootProg(ps)
	log.Logf(0, "==== CSAN BUG detected (stat=%v) ====", stat)
	log.Logf(0, "HasNetFail=%v HasCrashFail=%v", root != nil && root.HasNetFail, root != nil && root.HasCrashFail)
	for i, p := range ps {
		log.Logf(0, "node %d prog:\n%s", i, p.Serialize())
	}
	log.Logf(0, "executor output:\n%s", output)
	for _, d := range diffs {
		log.Logf(0, "diff: %s", d)
	}

	ex, err := os.Executable()
	if err != nil {
		log.Logf(0, "saveCsanBug: failed to get executable path: %v", err)
		return
	}
	// exe = <deploy-root>/bin/linux_amd64/syz-fuzzer, so 3 levels up is the
	// project root where the csan bug dumps are collected.
	dir := filepath.Join(filepath.Dir(ex), "../../../hmdfs-bugs",
		"csan-"+time.Now().Format("20060102-150405.000"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Logf(0, "saveCsanBug: failed to create dir %v: %v", dir, err)
		return
	}
	var progData, outData, diffData []byte
	for i, p := range ps {
		progData = append(progData, fmt.Sprintf("node %d:\n", i)...)
		progData = append(progData, p.Serialize()...)
		if i < len(infos) && infos[i] != nil {
			for j, ci := range infos[i].Calls {
				name := ""
				if j < len(p.Calls) {
					name = p.Calls[j].Meta.Name
				}
				progData = append(progData, fmt.Sprintf("  call[%d] %s errno=%d flags=%d\n", j, name, ci.Errno, ci.Flags)...)
			}
		}
	}
	outData = output
	for _, d := range diffs {
		diffData = append(diffData, []byte(d+"\n")...)
	}
	// Involved files: full metadata of both sides.
	diffPaths := make(map[string]bool)
	for _, d := range diffs {
		if idx := strings.IndexByte(d, ':'); idx > 0 {
			diffPaths[d[:idx]] = true
		}
	}
	for nodeIdx, fsMd := range fsMds {
		for path, md := range fsMd {
			if !diffPaths[path] {
				continue
			}
			diffData = append(diffData, fmt.Sprintf("node %d %s: %+v\n", nodeIdx, path, md)...)
		}
	}
	for name, data := range map[string][]byte{
		"prog.txt":   progData,
		"output.txt": outData,
		"diff.txt":   diffData,
	} {
		if err := osutil.WriteFile(filepath.Join(dir, name), data); err != nil {
			log.Logf(0, "saveCsanBug: write %v failed: %v", name, err)
		}
	}
	cfg := proc.fuzzer.config
	var wg sync.WaitGroup
	for i := 0; i < cfg.FuzzingVMs && i < len(cfg.Executor); i++ {
		fields := strings.Fields(cfg.Executor[i])
		hostIdx := -1
		for k, f := range fields {
			if strings.Contains(f, "@") && !strings.HasPrefix(f, "-") &&
				!strings.Contains(f, "/") {
				hostIdx = k
				break
			}
		}
		if hostIdx < 0 {
			continue
		}
		prefix := fields[:hostIdx+1]
		captures := []struct {
			name string
			cmd  string
		}{
			{fmt.Sprintf("node%d-agent.log", i), "cat /home/hmdfs_agent/hmdfs_agent.log"},
			{fmt.Sprintf("node%d-dmesg.txt", i), "dmesg | grep -i hmdfs | tail -n 300"},
		}
		for _, c := range captures {
			c, prefix := c, prefix
			wg.Add(1)
			go func() {
				defer wg.Done()
				args := append(append([]string{}, prefix[1:]...), c.cmd)
				out, err := osutil.RunCmd(15*time.Second, "", prefix[0], args...)
				if err != nil {
					log.Logf(0, "saveCsanBug: capture %s failed: %v", c.name, err)
					return
				}
				if err := osutil.WriteFile(filepath.Join(dir, c.name), out); err != nil {
					log.Logf(0, "saveCsanBug: write %s failed: %v", c.name, err)
				}
			}()
		}
	}
	wg.Wait()
	log.Logf(0, "saveCsanBug: saved to %v", dir)
}

type produceType int

const (
	produceCreate produceType = iota
	produceMkdir
	produceOpenCreate
	produceRename
)

type pathProducer struct {
	callIdx  int
	progIdx  int
	prodType produceType
	oldPath  string
}

func collectCreateCallOwners(ps []*prog.Prog, infos []*ipc.ProgInfo, cids []string, hmcfg *prog.Hmdfs_config) map[string]string {
	oldTree := hmcfg.FileTree

	deletedPaths := make(map[string]bool)
	for progIdx, p := range ps {
		if progIdx >= len(infos) || infos[progIdx] == nil {
			continue
		}
		for callIdx, call := range p.Calls {
			if callIdx >= len(infos[progIdx].Calls) {
				break
			}
			info := infos[progIdx].Calls[callIdx]
			if info.Flags&ipc.CallExecuted == 0 || info.Errno != 0 {
				continue
			}
			path := prog.GetDeletePath(call)
			if path != "" {
				deletedPaths[path] = true
			}
		}
	}

	productions := make(map[string][]pathProducer)
	for progIdx, p := range ps {
		if progIdx >= len(infos) || infos[progIdx] == nil {
			continue
		}
		for callIdx, call := range p.Calls {
			if callIdx >= len(infos[progIdx].Calls) {
				break
			}
			info := infos[progIdx].Calls[callIdx]
			if info.Flags&ipc.CallExecuted == 0 || info.Errno != 0 {
				continue
			}

			path, oldP, ctype := prog.GetCreateInfo(call)
			if path == "" {
				continue
			}

			switch ctype {
			case prog.CreateTypeOpen:
				if oldTree != nil && oldTree.FindNode(path) != nil && !deletedPaths[path] {
					continue
				}
				productions[path] = append(productions[path], pathProducer{callIdx, progIdx, produceOpenCreate, ""})
			case prog.CreateTypeFile:
				productions[path] = append(productions[path], pathProducer{callIdx, progIdx, produceCreate, ""})
			case prog.CreateTypeMkdir:
				productions[path] = append(productions[path], pathProducer{callIdx, progIdx, produceMkdir, ""})
			case prog.CreateTypeRename:
				productions[path] = append(productions[path], pathProducer{callIdx, progIdx, produceRename, oldP})
			}
		}
	}

	ownerMap := make(map[string]string)
	for path := range productions {
		cid := resolveCreateCid(path, productions, oldTree, cids, make(map[string]bool))
		if cid != "" {
			ownerMap[path] = cid
		}
	}
	return ownerMap
}

func resolveCreateCid(path string, productions map[string][]pathProducer, oldTree *prog.FileTree, cids []string, visited map[string]bool) string {
	if visited == nil {
		visited = make(map[string]bool)
	}
	if visited[path] {
		return ""
	}
	visited[path] = true

	prods, ok := productions[path]
	if !ok || len(prods) == 0 {
		if oldTree != nil {
			if node := oldTree.FindNode(path); node != nil {
				return node.OwnerCid
			}
		}
		return ""
	}

	winner := prods[0]
	for _, p := range prods[1:] {
		if p.callIdx > winner.callIdx || (p.callIdx == winner.callIdx && p.progIdx > winner.progIdx) {
			winner = p
		}
	}

	if winner.prodType == produceRename {
		return resolveCreateCid(winner.oldPath, productions, oldTree, cids, visited)
	}

	if winner.progIdx < len(cids) {
		return cids[winner.progIdx]
	}
	return ""
}

func (proc *Proc) logProgram(opts *ipc.ExecOpts, ps []*prog.Prog) {
	if proc.fuzzer.outputType == OutputNone {
		return
	}

	delimiter := []byte("---\n")
	var data []byte
	for _, p := range ps {
		if len(p.Calls) != 0 {
			log.Logf(0, "prog length: %d\n", len(p.Calls))
		}
		data = append(data, func() (out []byte) {
			// Serialize panics with "no result" when a cross-call reference
			// violates the serializer's invariants (dangling/forward/out-of-sync
			// uses). Dump the reference diagnosis first, then re-panic: the
			// program state is never mutated, so post-mortem reproduction of the
			// offending mutation stays possible.
			defer func() {
				if r := recover(); r != nil {
					log.Logf(0, "PANIC-DEBUG: serialize panic: %v\n%s", r, p.DumpRefDiagnosis())
					panic(r)
				}
			}()
			return p.Serialize()
		}()...)
		data = append(data, delimiter...)
	}

	root := proc.rootProg(ps)
	log.Logf(0, "HasCrashFail:%v HasNetFail:%v\n", root != nil && root.HasCrashFail, root != nil && root.HasNetFail)
	// The following output helps to understand what program crashed kernel.
	// It must not be intermixed.
	switch proc.fuzzer.outputType {
	case OutputStdout:
		now := time.Now()
		proc.fuzzer.logMu.Lock()
		fmt.Printf("%02v:%02v:%02v ---executing program %v:\n%s\nend of program\n",
			now.Hour(), now.Minute(), now.Second(),
			proc.pid, data)
		proc.fuzzer.logMu.Unlock()
	case OutputDmesg:
		fd, err := syscall.Open("/dev/kmsg", syscall.O_WRONLY, 0)
		if err == nil {
			buf := new(bytes.Buffer)
			fmt.Fprintf(buf, "syzkaller: executing program %v:\n%s\n",
				proc.pid, data)
			syscall.Write(fd, buf.Bytes())
			syscall.Close(fd)
		}
	case OutputFile:
		f, err := os.Create(fmt.Sprintf("%v-%v.prog", proc.fuzzer.name, proc.pid))
		if err == nil {
			f.Write(data)
			f.Close()
		}
	default:
		log.Fatalf("unknown output type: %v", proc.fuzzer.outputType)
	}
}
