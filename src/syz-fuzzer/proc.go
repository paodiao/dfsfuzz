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
	freqCov           int32
	cltTick           chan bool
	hmcfg             prog.Hmdfs_config
	lcsInodeops       *prog.LayeredChoiceStrategy
	lcsFileops        *prog.LayeredChoiceStrategy
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
				}
			}
		} else {
			log.Logf(0, "InitDir is not configured, skipping initial files loading")
		}
	}

	hc.FileTree = prog.NewFileTree()
	hc.FileTree.InitFromHmdfsConfig(&hc)

	var lcsInodeops, lcsFileops *prog.LayeredChoiceStrategy
	if hc.DfsName == "hmdfs" {
		lcsInodeops = prog.NewLayeredChoiceStrategy("inodeops", &hc, fuzzer.target)
		lcsFileops = prog.NewLayeredChoiceStrategy("fileops", &hc, fuzzer.target)
		lcsInodeops.SetTscOffsets(fuzzer.tscoffs)
		lcsFileops.SetTscOffsets(fuzzer.tscoffs)
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
		freqCov:           freqCov,
		cltTick:           cltTick,
		hmcfg:             hc,
		lcsInodeops:       lcsInodeops,
		lcsFileops:        lcsFileops,
	}
	return proc, nil
}

func (proc *Proc) loop() {
	generatePeriod := 100
	if proc.fuzzer.config.Flags&ipc.FlagSignal == 0 {
		// If we don't have real coverage signal, generate programs more frequently
		// because fallback signal is weak.
		generatePeriod = 2
	}
	for i := 0; ; i++ {
		item := proc.fuzzer.workQueue.dequeue()
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
			rand.Seed(time.Now().UnixNano())
			if proc.fuzzer.config.DFSName == "hmdfs" {
				// 按概率选择种子类型
				seedType := rand.Intn(4)
				switch seedType {
				case 0:
					ps = proc.fuzzer.target.GenerateProgsForHmdfsStash(proc.rnd, proc.fuzzer.sCalls, &proc.hmcfg)
				case 1:
					ps = proc.fuzzer.target.GenerateProgsForHmdfsDcache(proc.rnd, proc.fuzzer.sCalls, &proc.hmcfg)
				case 2:
					ps = proc.fuzzer.target.GenerateProgsForHmdfsFileops(proc.rnd, proc.fuzzer.sCalls, &proc.hmcfg)
				case 3:
					ps = proc.fuzzer.target.GenerateProgsForHmdfsInodeops(proc.rnd, proc.fuzzer.sCalls, &proc.hmcfg)
				default:
					//ps = r.Generate(...)  // 原有的随机生成
				}
				proc.execute(proc.execOpts, ps, ProgNormal, StatGenerate)
			} else {
				//Generate empty subtestcase for servers
				for idx := 0; idx < proc.fuzzer.config.ServNum; idx++ {
					p, _ := proc.fuzzer.target.Generate(proc.rnd, 0, nil, nil, true, proc.fuzzer.sCalls,
						proc.fuzzer.config.EnableC2san, &proc.hmcfg, idx)
					ps = append(ps, p)
				}
				//Generate subtestcases for clients
				subTsNum := proc.fuzzer.config.FuzzingVMs - proc.fuzzer.config.ServNum
				var files map[string]bool
				for idx := 0; idx < subTsNum; {
					repeatNum := rand.Intn(subTsNum-idx) + 1
					curIdx := proc.fuzzer.config.ServNum + idx
					p, newFiles := proc.fuzzer.target.Generate(proc.rnd, prog.RecommendedCalls, ct, files, false,
						proc.fuzzer.sCalls, proc.fuzzer.config.EnableC2san, &proc.hmcfg, curIdx)
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
								seedPS[0].HasCrashFail || seedPS[0].HasNetFail, proc.fuzzer.config.EnableC2san, &proc.hmcfg, idx)
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

func (proc *Proc) triageInput(item *WorkTriage) {
	log.Logf(1, "#%v: triaging type=%x", proc.pid, item.flags)

	callName := ".extra"
	logCallName := "extra"
	if item.triageDag {
		logCallName = "dag"
	} else if item.call != -1 {
		callName = item.ps[item.subNum].Calls[item.call].Meta.Name
		logCallName = fmt.Sprintf("call #%v %v", item.call, callName)
	}

	var newSignal signal.Signal
	if !item.triageDag {
		prio := signalPrio(item.ps[item.subNum], &item.info, item.call)
		inputSignal := signal.FromRaw(item.info.Signal, prio)
		newSignal = proc.fuzzer.corpusSignalDiff(inputSignal)
		if newSignal.Empty() {
			return
		}
		log.Logf(3, "1 triaging input for %v (new signal=%v)", logCallName, newSignal.Len())
	}
	var SrvCover, CliCover cover.Cover
	const (
		signalRuns       = 3
		minimizeAttempts = 3
	)
	// Compute input coverage and non-flaky signal for minimization.
	notexecuted := 0
	if !item.triageDag {
		for i := 0; i < signalRuns; i++ {
			infos, _, _ := proc.executeRaw(proc.execOptsCover, item.ps, StatTriage)
			if !reexecutionSuccess(infos[item.subNum], &item.info, item.call) {
				// The call was not executed or failed.
				notexecuted++
				if notexecuted > signalRuns/2+1 {
					log.Logf(0, "----- triage return due to unsuccessful execution %s", logCallName)
					return // if happens too often, give up
				}
				continue
			}
			thisSignal, _ := getSignalAndCover(item.ps[item.subNum], infos[item.subNum], item.call)
			newSignal = newSignal.Intersection(thisSignal)
			// Without !minimized check manager starts losing some considerable amount
			// of coverage after each restart. Mechanics of this are not completely clear.
			if newSignal.Empty() && item.flags&ProgMinimized == 0 {
				log.Logf(0, "----- triage return due to empty signal %s", logCallName)
				return
			}
		}

		if item.flags&ProgMinimized == 0 && item.subNum >= proc.fuzzer.config.ServNum {
			//修改：minimize是不是也要定制一下？
			item.ps, item.call = prog.Minimize(item.ps, item.call, item.subNum, false, proc.fuzzer.config.ServNum,
				func(ps1 []*prog.Prog, call1 int) bool {
					for i := 0; i < minimizeAttempts; i++ {
						infos := proc.execute(proc.execOptsNoCollide, ps1, ProgNormal, StatMinimize)
						if !reexecutionSuccess(infos[item.subNum], &item.info, call1) {
							// The call was not executed or failed.
							continue
						}
						thisSignal, _ := getSignalAndCover(ps1[item.subNum], infos[item.subNum], call1)
						if newSignal.Intersection(thisSignal).Len() == newSignal.Len() {
							return true
						}
					}
					return false
				})
		}
	}

	//#issue: why the testcase after minimized is empty.
	totalLen := 0
	for _, p := range item.ps {
		totalLen += len(p.Calls)
	}
	if totalLen == 0 {
		return
	}

	//Merge server coverage
	var inputCliSignal, inputSrvSignal signal.Signal
	srvNum := proc.fuzzer.config.ServNum
	for i := 0; i < signalRuns; i++ {
		infos, _, _ := proc.executeRaw(proc.execOptsCover, item.ps, StatTriage) //TODO
		thisSignal, thisCover := getSignalAndCover(item.ps[item.subNum], infos[item.subNum], item.call)
		if item.triageClient {
			CliCover.Merge(thisCover)
			inputCliSignal.Merge(thisSignal)
			for idx, info := range infos[:srvNum] {
				//proc.fuzzer.checkNewSignal(item.ps[idx], info)
				thisSignal, thisCover := getSignalAndCover(item.ps[idx], info, -1)
				inputSrvSignal.Merge(thisSignal)
				SrvCover.Merge(thisCover)
			}
		} else {
			SrvCover.Merge(thisCover)
			inputSrvSignal.Merge(thisSignal)
			for idx, info := range infos {
				if idx < srvNum {
					continue
				}
				//proc.fuzzer.checkNewSignal(item.ps[idx], info)
				thisSignal, thisCover := getAllSignalAndCover(item.ps[idx], info)
				inputCliSignal.Merge(thisSignal)
				CliCover.Merge(thisCover)
			}
		}
	}

	var data [][]byte
	var dataForHash []byte
	for _, p := range item.ps {
		prog := p.Serialize()
		data = append(data, prog)
		dataForHash = append(dataForHash, prog...)
	}
	sig := hash.Hash(dataForHash)

	log.Logf(2, "added new input for %v to corpus:\n%s", logCallName, dataForHash)
	proc.fuzzer.sendInputToManager(rpctype.RPCInput{
		Call:      callName,
		Prog:      data,
		CliSignal: inputCliSignal.Serialize(),
		SrvSignal: inputSrvSignal.Serialize(),
		SrvCover:  SrvCover.Serialize(),
		CliCover:  CliCover.Serialize(),
	})

	if item.call != -1 {
		proc.cltTick <- true
	}

	log.Logf(0, "triageInput addInputToCorpus: HasCrashFail: %v, HasNetFail: %v", item.ps[0].HasCrashFail, item.ps[0].HasNetFail)
	if item.triageDag {
		atomic.AddUint64(&proc.fuzzer.dagCorpusCount, 1)
	}
	added := proc.fuzzer.addInputToCorpus(item.ps, inputCliSignal, inputSrvSignal, sig)
	if item.triageDag && added {
		atomic.AddUint64(&proc.fuzzer.dagCorpusEntries, 1)
	}

	if item.flags&ProgSmashed == 0 {
		proc.fuzzer.workQueue.enqueue(&WorkSmash{item.ps, item.call, item.subNum})
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

func (proc *Proc) smashInput(item *WorkSmash) {
	if proc.fuzzer.comparisonTracingEnabled && item.call != -1 {
		proc.executeHintSeed(item.ps, item.call, item.subNum)
	}
	//
	rand.Seed(time.Now().UnixNano())
	fuzzerSnapshot := proc.fuzzer.snapshot()

	//Failure enumeration
	if (proc.fuzzer.config.NetFailure || proc.fuzzer.config.NodeCrash) && !(item.ps[0].HasNetFail || item.ps[0].HasCrashFail) {
		//proc.enumFailures(item.ps)
	}

	//Normal mutation
	for i := 0; i < 100; i++ {
		ps := prog.Clones(item.ps)
		/*
		   Each time only mutate one sub-testcase because: If we do multiple mutations and only one of them trigger new
		   coverage, we can't know which one contributes to the coverage and thus the testcases will have redudant
		   syscalls. However, this non-mutual mutation might not generate testcases towards more interleavings.
		*/
		//Tao TODO
		log.Logf(0, "NetFailure, Node crash: %v %v", proc.fuzzer.config.NetFailure, proc.fuzzer.config.NodeCrash)
		proc.mutateHmdfs(ps, fuzzerSnapshot.corpus)
		proc.execute(proc.execOpts, ps, ProgNormal, StatSmash)
	}
}

// mutateHmdfs mutates a hmdfs testcase: seeds go through their type-specific
// structured mutator (stash/dcache/inodeops/fileops); the inode/file-op ones
// are DCT-based (root call inserted into ps[0], concurrent calls spread over
// the other nodes). Everything falls back to the standard per-client Mutate.
func (proc *Proc) mutateHmdfs(ps []*prog.Prog, corpus [][]*prog.Prog) {
	if len(ps) == 0 {
		return
	}
	srvNum := proc.fuzzer.config.ServNum
	mutated := false
	if ps[0].IsInodeOps {
		mutated = prog.MutateInodeOpsWithDCT(ps, proc.rnd, proc.fuzzer.choiceTable,
			proc.fuzzer.sCalls, &proc.hmcfg, proc.lcsInodeops)
	} else if ps[0].IsFileOps {
		mutated = prog.MutateFileopsWithDCT(ps, proc.rnd, proc.fuzzer.choiceTable,
			proc.fuzzer.sCalls, &proc.hmcfg, proc.lcsFileops)
	} else if ps[0].IsStash {
		mutated = prog.MutateStashProg(ps, proc.rnd, proc.fuzzer.choiceTable,
			proc.fuzzer.sCalls, &proc.hmcfg)
	} else if ps[0].IsDCache {
		mutated = prog.MutateDcacheProg(ps, proc.rnd, proc.fuzzer.choiceTable,
			proc.fuzzer.sCalls, &proc.hmcfg)
	}
	//修改：这里要保留原始突变策略吗？
	if !mutated && len(ps) > srvNum {
		start := srvNum
		if proc.fuzzer.config.DFSName == "hmdfs" && srvNum == 0 {
			start = 1 // hmdfs 的 ps[0] 是主 prog（故障窗口/位置表/同步结构）——普通 Mutate 不动它
		}
		if start >= len(ps) {
			return
		}
		randIdx := rand.Intn(len(ps)-start) + start
		ps[randIdx].Mutate(proc.rnd, prog.RecommendedCalls, proc.fuzzer.choiceTable, corpus,
			proc.fuzzer.sCalls, srvNum, ps[0].HasCrashFail || ps[0].HasNetFail,
			proc.fuzzer.config.EnableC2san, &proc.hmcfg, randIdx)
		log.Logf(1, "#%v: smash mutated %d-th subtestcase", proc.pid, randIdx)
	}
}

// feedbackDagPairs feeds novel DAG pairs (already filtered by novelty) back
// into the DCT tables. Two layers:
//   - both concurrent and HB pairs update the temporal-form weights of the
//     combo (the second layer: which insertion form actually produced the
//     corresponding pair);
//   - every novel pair — concurrent or HB — marks the (root, variant) combo
//     as explored and resets its no-yield counter (direction 1/2): the combo
//     is rewarded for its combined output, whatever form it took.
func (proc *Proc) feedbackDagPairs(newPairs []prog.DAGPair) {
	for _, p := range newPairs {
		if p.Temporal == prog.TemporalHB {
			atomic.AddUint64(&proc.fuzzer.dagHbPairCount, 1)
		} else {
			atomic.AddUint64(&proc.fuzzer.dagCcPairCount, 1)
		}
		root, variant, temporal, ok := prog.DagPairToVariant(&p)
		if !ok {
			continue
		}
		if lcs := proc.lcsForRootCall(root); lcs != nil {
			lcs.UpdateTemporalWeight(root, variant, temporal)
			lcs.MarkYield(root, variant)
		}
	}
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

// lcsForRootCall picks the LayeredChoiceStrategy whose DCT table has the
// given root call.
func (proc *Proc) lcsForRootCall(rootCallName string) *prog.LayeredChoiceStrategy {
	if proc.lcsInodeops != nil && proc.lcsInodeops.GetDCT().HasRoot(rootCallName) {
		return proc.lcsInodeops
	}
	if proc.lcsFileops != nil && proc.lcsFileops.GetDCT().HasRoot(rootCallName) {
		return proc.lcsFileops
	}
	return nil
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
		infos, _, _ := proc.executeRaw(proc.execOpts, newProgs, StatSmash)
		if infos != nil && len(infos[proc.fuzzer.config.ServNum+subNum].Calls) > call && infos[proc.fuzzer.config.ServNum+subNum].Calls[call].Flags&ipc.CallFaultInjected == 0 {
			break
		}
	}
}

func (proc *Proc) executeHintSeed(ps []*prog.Prog, call int, subNum int) {
	log.Logf(1, "#%v: collecting comparisons on call %d", proc.pid, call)
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
		infos, _, _ := proc.executeRaw(proc.execOptsCover, ps, StatTriage)
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

func (proc *Proc) execute(execOpts *ipc.ExecOpts, ps []*prog.Prog, flags ProgTypes, stat Stat) []*ipc.ProgInfo {
	if len(ps) == 0 {
		return nil
	}
	log.Logf(0, "HasCrashFail: %v, .HasNetFail: %v", ps[0].HasCrashFail, ps[0].HasNetFail)
	infos, _, _ := proc.executeRaw(execOpts, ps, stat)
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
	if proc.fuzzer.config.EnableClientFb {
		for idx, info := range infos {
			//TODO: how to check the signal from servers and clients
			if idx < servNum {
				continue
			} else {
				calls, extra := proc.fuzzer.checkNewSignal(ps[idx], info)
				for _, callIndex := range calls {
					proc.enqueueCallTriage(ps, flags, callIndex, info.Calls[callIndex], idx, true) //idx -> subNum
					clientHasNew = true
				}
				if extra {
					proc.enqueueCallTriage(ps, flags, -1, info.Extra, idx, true)
				}
			}
		}
	}

	//(1). With failures, exploit server feedback
	//(2). Client doesn't have feedback for a while
	if proc.fuzzer.config.EnableSrvFb && ((!clientHasNew && proc.useSrvCovNow()) || ps[0].HasNetFail || ps[0].HasCrashFail) {
		log.Logf(0, "----- no new client coverage: %v, %v", clientHasNew, proc.fuzzer.config.EnableEval)
		for idx, info := range infos[:servNum] {
			_, extra := proc.fuzzer.checkNewSignal(nil, info)
			if extra {
				log.Logf(0, "----- enqueue testcases with server coveraged")
				proc.enqueueCallTriage(ps, flags, -1, info.Extra, idx, false)
			}
		}
	}

	//DAG feedback (hmdfs): every execution discovering a novel operation
	//pair/schedule earns a corpus entry (the "energy" model), tracked in
	//dedicated stats separate from the coverage channel. Novel pairs also
	//feed back into the DCT tables (exploration tracking + yield reset).
	if proc.fuzzer.config.EnableDagFb && proc.fuzzer.config.DFSName == "hmdfs" {
		for idx, info := range infos {
			if idx < servNum || info == nil || len(info.DagSignal) == 0 {
				continue
			}
			newBits := proc.fuzzer.checkNewDagSignal(info.DagSignal)
			if n := newBits.Len(); n > 0 {
				atomic.AddUint64(&proc.fuzzer.dagPairCount, uint64(n))
				maxDag := proc.fuzzer.config.MaxDagCorpus
				if maxDag == 0 || atomic.LoadUint64(&proc.fuzzer.dagCorpusEntries) < uint64(maxDag) {
					proc.enqueueDagTriage(ps, flags, idx)
				}
				newPairs := filterNewDagPairs(info, newBits)
				proc.feedbackDagPairs(newPairs)
				proc.countDagDepth(newPairs)
			}
			if proc.fuzzer.config.EnableDagScheduleFb &&
				proc.fuzzer.checkNewDagSchedule(info.DagScheduleBit) > 0 {
				atomic.AddUint64(&proc.fuzzer.dagSchedCount, 1)
			}
		}
	}

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

	return infos
}

func (proc *Proc) enqueueCallTriage(ps []*prog.Prog, flags ProgTypes, callIndex int, info ipc.CallInfo, subNum int,
	triageClient bool) {
	// info.Signal points to the output shmem region, detach it before queueing.
	info.Signal = append([]uint32{}, info.Signal...)
	// None of the caller use Cover, so just nil it instead of detaching.
	// Note: triage input uses executeRaw to get coverage.
	info.Cover = nil
	proc.fuzzer.workQueue.enqueue(&WorkTriage{
		ps:           prog.Clones(ps),
		call:         callIndex,
		subNum:       subNum,
		info:         info,
		flags:        flags,
		triageClient: triageClient,
	})
}

// enqueueDagTriage queues a corpus entry whose interestingness comes from the
// DAG feedback (new operation pair), not from coverage.
func (proc *Proc) enqueueDagTriage(ps []*prog.Prog, flags ProgTypes, subNum int) {
	proc.fuzzer.workQueue.enqueue(&WorkTriage{
		ps:           prog.Clones(ps),
		call:         -1,
		subNum:       subNum,
		flags:        flags,
		triageClient: true,
		triageDag:    true,
	})
}

func (proc *Proc) executeRaw(opts *ipc.ExecOpts, ps []*prog.Prog, stat Stat) ([]*ipc.ProgInfo, []map[string]prog.FileMetadata, uint64) {

	if len(ps) == 0 {
		return nil, nil, 0
	}

	if opts.Flags&ipc.FlagDedupCover == 0 {
		log.Fatalf("dedup cover is not enabled")
	}

	for _, p := range ps {
		proc.fuzzer.checkDisabledCalls(p)
	}

	// Limit concurrency window and do leak checking once in a while.
	ticket := proc.fuzzer.gate.Enter()
	defer proc.fuzzer.gate.Leave(ticket)

	if ps[0].HasCrashFail || ps[0].HasNetFail || proc.fuzzer.config.EnableCsan {
		opts.Flags &= ^ipc.FlagCollide
		opts.Flags &= ^ipc.FlagThreaded
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

	output, infos, hanged, err, fsMds, testdirIno, hmdfsTraceEvents = proc.env.Exec(opts, ps)

	if err != nil {
		log.Fatalf("execution errors or hangs: %v\n", err)
	}
	log.Logf(2, "result hanged=%v: %s", hanged, output)

	csanPassed, csanDiffs := true, []string(nil)
	if proc.fuzzer.config.EnableCsan {
		csanPassed, csanDiffs = checker.ConcFSCheck(ps, infos, fsMds, proc.fuzzer.config.ServNum,
			proc.fuzzer.config.DFSName, proc.fuzzer.config.DfsSetupParams,
			proc.fuzzer.config.InitIp, testdirIno)
		if !csanPassed {
			log.Logf(0, "Concurrent semantic checker detects a bug")
			proc.saveCsanBug(ps, output, csanDiffs, fsMds, stat)
			// The file tree across nodes is now inconsistent; continuing to
			// fuzz on it would poison later executions. Exit cleanly so that
			// the manager stops all VMs and restarts the whole group — qemu
			// -snapshot restores the tree to its initial image state.
			log.Logf(0, "==== CSAN BUG: restarting all VMs to restore the file tree ====")
			os.Exit(0)
		}
	}

	clientIdx := -1
	if csanPassed && proc.fuzzer.config.DFSName == "hmdfs" && len(ps) > 0 && len(fsMds) > proc.fuzzer.config.ServNum {
		clientIdx = len(fsMds) - 1
		for ; clientIdx >= proc.fuzzer.config.ServNum; clientIdx-- {
			if fsMds[clientIdx] != nil && len(fsMds[clientIdx]) > 0 {
				break
			}
		}
		if clientIdx >= proc.fuzzer.config.ServNum {
			ownerMap := collectCreateCallOwners(ps, infos, proc.hmcfg.Cids, &proc.hmcfg)
			prog.SyncFileTreeFromFsMd(fsMds[clientIdx], ownerMap, &proc.hmcfg)
		}
	}

	// DAG feedback: pair novelty bits + schedule bit (only for regular
	// executions; triage re-runs are excluded to avoid timing-jitter noise).
	// Does not depend on csan/fsMds: call-window matching needs neither, and
	// ino-based resolution (writepage/lookup) degrades gracefully without fsMd.
	if stat != StatTriage && csanPassed && proc.fuzzer.config.DFSName == "hmdfs" &&
		len(hmdfsTraceEvents) > 0 && len(infos) > proc.fuzzer.config.ServNum {
		vertices := prog.BuildVertices(hmdfsTraceEvents, ps, fsMds, &proc.hmcfg, proc.fuzzer.tscoffs)
		hbPairs, ccPairs := prog.ExtractPairs(vertices)
		allPairs := append(hbPairs, ccPairs...)
		pairBits, schedBit := prog.ComputeFeedback(hbPairs, ccPairs, &proc.hmcfg)
		if len(pairBits) > 0 {
			for _, info := range infos[proc.fuzzer.config.ServNum:] {
				if info != nil {
					info.DagSignal = pairBits
					info.DagScheduleBit = schedBit
					info.DagPairs = allPairs
					break
				}
			}
		}
	}

	return infos, fsMds, testdirIno
}

// saveCsanBug dumps everything needed to locate/reproduce/analyze a
// consistency failure: the seeds of all nodes, the raw executor output, the
// differences and the involved files' full metadata.
func (proc *Proc) saveCsanBug(ps []*prog.Prog, output []byte, diffs []string,
	fsMds []map[string]prog.FileMetadata, stat Stat) {
	log.Logf(0, "==== CSAN BUG detected (stat=%v) ====", stat)
	log.Logf(0, "HasNetFail=%v HasCrashFail=%v", ps[0].HasNetFail, ps[0].HasCrashFail)
	for i, p := range ps {
		log.Logf(0, "node %d prog:\n%s", i, p.Serialize())
	}
	log.Logf(0, "executor output:\n%s", output)
	for _, d := range diffs {
		log.Logf(0, "diff: %s", d)
	}

	dir := filepath.Join(proc.fuzzer.bugDir, "csan-"+time.Now().Format("20060102-150405.000"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Logf(0, "saveCsanBug: failed to create dir %v: %v", dir, err)
		return
	}
	var progData, outData, diffData []byte
	for i, p := range ps {
		progData = append(progData, fmt.Sprintf("node %d:\n", i)...)
		progData = append(progData, p.Serialize()...)
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
		data = append(data, p.Serialize()...)
		data = append(data, delimiter...)
	}

	log.Logf(0, "HasCrashFail:%v HasNetFail:%v\n", ps[0].HasCrashFail, ps[0].HasNetFail)
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
