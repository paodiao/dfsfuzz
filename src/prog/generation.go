// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"math/rand"
)

// Generate generates a random program with ncalls calls.
// ct contains a set of allowed syscalls, if nil all syscalls are used.
func (target *Target) Generate(rs rand.Source, ncalls int, ct *ChoiceTable, files map[string]bool, isForSrv bool,
	sCalls *SpecialCalls, enableC2san bool, hmcfg *Hmdfs_config, idx int) (*Prog, map[string]bool) {
	p := &Prog{
		Target: target,
		//IsForSrv: isForSrv,
	}

	if isForSrv {
		return p, nil
	}

	r := newRand(target, rs)
	r.hmcfg = hmcfg
	r.curIdx = idx
	s := newState(target, ct, nil)
	//Add file set from previously generated syscalls to new state
	for file, _ := range files {
		if !s.files[file] {
			s.files[file] = true
		}
	}

	for len(p.Calls) < ncalls {
		calls := r.generateCall(s, p, len(p.Calls), sCalls, enableC2san)
		for _, c := range calls {
			s.analyze(c)
			p.Calls = append(p.Calls, c)
		}
	}
	// For the last generated call we could get additional calls that create
	// resources and overflow ncalls. Remove some of these calls.
	// The resources in the last call will be replaced with the default values,
	// which is exactly what we want.
	for len(p.Calls) > ncalls {
		p.RemoveCall(ncalls - 1)
	}
	p.sanitizeFix()
	p.debugValidate()
	return p, s.files
}

func (target *Target) GenerateProgsForHmdfsStash(rs rand.Source, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	r := newRand(target, rs)
	r.hmcfg = hmcfg
	s := newState(target, nil, nil)

	mode := r.Intn(3)

	if mode == 2 && hmcfg.Node_num >= 2 {
		return target.generateProgsForMultiFileStash(r, s, sCalls, hmcfg)
	}

	filePath := r.selectRemoteFile(hmcfg, hmcfg.Cids[0])

	failStartIdx := -1
	failEndIdx := -1
	syncstartidx := -1
	syncendidx := -1

	p0 := &Prog{Target: target, SyncIdx: 0}
	idxSlice := make([]int, 0, hmcfg.Node_num-1)
	for i := 0; i < hmcfg.Node_num; i++ {
		if i != 0 { // 排除执行节点（net_down 恒插在 p0）——故障目标 = 其他节点（L2）
			idxSlice = append(idxSlice, i)
		}
	}
	calls0, writeInfos0, fs, fe, netInsertPos := r.generateWriteCallsWithNetCallForHmdfsStash(s, p0, sCalls, filePath, &p0.SyncIdx, 0, idxSlice)
	failStartIdx = fs
	failEndIdx = fe
	for _, c := range calls0 {
		p0.Calls = append(p0.Calls, c)
	}
	p0.IsStash = true
	p0.HasNetFail = true
	ps = append(ps, p0)

	allWriteInfos := writeInfos0

	if mode == 1 && hmcfg.Node_num >= 2 {
		p1 := &Prog{Target: target, SyncIdx: 0}
		calls1, writeInfos1, syncstart, syncend := r.generateWriteCallsWithoutNetCallForHmdfsStash(s, p1, sCalls, filePath, &p1.SyncIdx, 1, true)
		syncstartidx = syncstart
		syncendidx = syncend
		for _, c := range calls1 {
			p1.Calls = append(p1.Calls, c)
		}
		p1.IsStash = true
		ps = append(ps, p1)
		allWriteInfos = append(allWriteInfos, writeInfos1...)
	}

	startIdx := 1
	if mode == 1 {
		startIdx = 2
	}

	var syncPositions [][]int
	failPos := make([]int, 0)
	failPos = append(failPos, 0*100+1, failStartIdx, 0*100+2, failEndIdx)
	if syncstartidx >= 0 {
		failPos = append(failPos, 1*100+1, syncstartidx)
	}
	if syncendidx >= 0 {
		failPos = append(failPos, 1*100+2, syncendidx)
	}

	for i := startIdx; i < hmcfg.Node_num; i++ {
		p := &Prog{Target: target, SyncIdx: 0}
		calls, syncStart, syncEnd := r.generateReadCallsForHmdfsStash(s, p, sCalls, filePath, allWriteInfos, &p.SyncIdx, i, true, netInsertPos)
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}
		p.IsStash = true
		ps = append(ps, p)

		if syncStart >= 0 {
			failPos = append(failPos, i*100+1, syncStart)
		}
		if syncEnd >= 0 {
			failPos = append(failPos, i*100+2, syncEnd)
		}
	}

	syncPositions = append(syncPositions, failPos)
	if len(ps) > 0 {
		ps[0].GeneralFailPos = syncPositions
	}

	return ps
}

func (target *Target) generateProgsForMultiFileStash(r *randGen, s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	numFiles := 3
	if hmcfg.Node_num < 4 {
		numFiles = hmcfg.Node_num - 1
	}

	filePaths := make([]string, numFiles)
	for i := 0; i < numFiles; i++ {
		filePaths[i] = r.selectFileInOneNode(hmcfg, hmcfg.Cids[i+1])
	}

	idxSlice := make([]int, 0, hmcfg.Node_num-1)
	for i := 0; i < hmcfg.Node_num; i++ {
		if i != 0 { // 排除执行节点（net_down 恒插在 p0）——故障目标 = 其他节点（L2）
			idxSlice = append(idxSlice, i)
		}
	}

	p0 := &Prog{Target: target, SyncIdx: 0}
	calls0, writeInfosMap, fs, fe, netInsertPos := r.generateMultiFileWriteCallsForStash(s, p0, sCalls, filePaths, &p0.SyncIdx, 0, idxSlice)
	for _, c := range calls0 {
		p0.Calls = append(p0.Calls, c)
	}
	p0.IsStash = true
	p0.HasNetFail = true
	ps = append(ps, p0)

	failPos := make([]int, 0)
	failPos = append(failPos, 0*100+1, fs, 0*100+2, fe)

	if hmcfg.Node_num > 1 {
		p1 := &Prog{Target: target, SyncIdx: 0}
		calls1, normalWriteInfos, ss, se := r.generateNormalWriteCallsForStash(s, sCalls, filePaths, &p1.SyncIdx, 1, true, netInsertPos)
		for _, c := range calls1 {
			p1.Calls = append(p1.Calls, c)
		}
		p1.IsStash = true
		ps = append(ps, p1)

		if ss >= 0 {
			failPos = append(failPos, 1*100+1, ss)
		}
		if se >= 0 {
			failPos = append(failPos, 1*100+2, se)
		}
		for key, value := range normalWriteInfos {
			if len(writeInfosMap[key]) == 0 {
				writeInfosMap[key] = []WriteInfo{}
			}
			writeInfosMap[key] = append(writeInfosMap[key], value...)
		}
	}

	var syncPositions [][]int

	startIdx := 2

	for i := startIdx; i < hmcfg.Node_num; i++ {
		p := &Prog{Target: target, SyncIdx: 0}
		calls, syncStart, syncEnd := r.generateMultiFileReadCallsForStash(s, p, sCalls, filePaths, writeInfosMap, &p.SyncIdx, i, true, netInsertPos)
		for _, c := range calls {
			p.Calls = append(p.Calls, c)
		}
		p.IsStash = true
		ps = append(ps, p)

		if syncStart >= 0 {
			failPos = append(failPos, i*100+1, syncStart)
		}
		if syncEnd >= 0 {
			failPos = append(failPos, i*100+2, syncEnd)
		}
	}

	syncPositions = append(syncPositions, failPos)
	if len(ps) > 0 {
		ps[0].GeneralFailPos = syncPositions
	}

	return ps
}

func (target *Target) GenerateProgsForHmdfsDcache(rs rand.Source, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	r := newRand(target, rs)
	r.hmcfg = hmcfg
	s := newState(target, nil, nil)

	testType := r.Intn(3)

	switch testType {
	case 0:
		ps = r.generateProgsForDcacheTimeout(s, sCalls, hmcfg)
		//修改：timeout相关的生成和突变可能要改掉
	case 1:
		ps = r.generateProgsForDcachePersistence(s, sCalls, hmcfg)
		if len(ps) == 0 {
			ps = r.generateProgsForDropPush(s, sCalls, hmcfg)
		}
	case 2:
		ps = r.generateProgsForDropPush(s, sCalls, hmcfg)
	}
	for _, p := range ps {
		p.IsDCache = true
	}
	return ps
}

func (target *Target) GenerateProgsForHmdfsInodeops(rs rand.Source, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	r := newRand(target, rs)
	r.hmcfg = hmcfg
	s := newState(target, nil, nil)

	lcs := NewLayeredChoiceStrategy("inodeops", hmcfg, target)

	if lcs.ShouldUsePattern(r.Rand) {
		ps = r.generateFromPredefinedPattern(s, sCalls, hmcfg, lcs, "inodeops")
		r.expandWithDCT(ps, sCalls, hmcfg, lcs, "inodeops")
	} else {
		ps = r.generateDCTMultiRound(s, sCalls, hmcfg, lcs, "inodeops")
	}

	if len(ps) == 0 {
		testDir := r.selectRemoteDir(hmcfg, hmcfg.Cids[0])
		if testDir == "" {
			testDir = "merge_view"
		}

		testType := r.Intn(6)

		switch testType {
		case 0:
			ps = r.generateFileopsSetattrConcurrent(s, sCalls, hmcfg, testDir)
		case 1:
			ps = r.generateConcurrentDirCreate(s, sCalls, hmcfg, testDir)
		case 2:
			ps = r.generateConcurrentDirDelete(s, sCalls, hmcfg, testDir)
		case 3:
			ps = r.generateConcurrentDirMixed(s, sCalls, hmcfg, testDir)
		case 4:
			ps = r.generateConcurrentInodeOps(s, sCalls, hmcfg, testDir)
		case 5:
			ps = r.generateConcurrentRenameOps(s, sCalls, hmcfg, testDir)
		}
	}

	for _, p := range ps {
		p.IsInodeOps = true
	}
	return ps
}

func (target *Target) GenerateProgsForHmdfsFileops(rs rand.Source, sCalls *SpecialCalls, hmcfg *Hmdfs_config) []*Prog {
	var ps []*Prog

	r := newRand(target, rs)
	r.hmcfg = hmcfg
	s := newState(target, nil, nil)

	lcs := NewLayeredChoiceStrategy("fileops", hmcfg, target)

	if lcs.ShouldUsePattern(r.Rand) {
		ps = r.generateFromPredefinedPattern(s, sCalls, hmcfg, lcs, "fileops")
		r.expandWithDCT(ps, sCalls, hmcfg, lcs, "fileops")
	} else {
		ps = r.generateDCTMultiRound(s, sCalls, hmcfg, lcs, "fileops")
	}

	if len(ps) == 0 {
		testType := r.Intn(3)

		switch testType {
		case 0:
			ps = r.generateProgsForConcurrentRW(s, sCalls, hmcfg)
		case 1:
			ps = r.generateProgsForFsyncTest(s, sCalls, hmcfg)
		case 2:
			ps = r.generateProgsForAppendTest(s, sCalls, hmcfg)
		}
	}

	for _, p := range ps {
		p.IsFileOps = true
	}
	return ps
}

// expandWithDCT inserts further root+variant groups (via insertCallFromDCT)
// until the average number of calls across all progs in ps reaches a
// per-seed target (4-8, randomized for corpus diversity) or the round cap.
// Shared by both the DCT and the pattern generation entry points: fresh-path
// mkdir/creat preprocessing, fd lifecycle handling and time-aligned variants
// are inherited from insertCallFromDCT.
func (r *randGen) expandWithDCT(ps []*Prog, sCalls *SpecialCalls, hmcfg *Hmdfs_config,
	lcs *LayeredChoiceStrategy, seedType string) {
	if len(ps) == 0 {
		return
	}
	target := 6 + r.Intn(4) // 6-9
	failed := 0
	for round := 0; round < 6 && failed < 2; round++ {
		if avgCalls(ps) >= target {
			break
		}
		if !insertCallFromDCT(ps, r, nil, sCalls, hmcfg, lcs, seedType) {
			failed++ // ct is a dead parameter (unused inside); stop after 2 consecutive failures
			continue
		}
		failed = 0
	}
}

// generateDCTMultiRound builds a minimal DCT seed, then grows it with
// expandWithDCT (multi-group programs with continuous fd lifecycles).
func (r *randGen) generateDCTMultiRound(s *state, sCalls *SpecialCalls, hmcfg *Hmdfs_config,
	lcs *LayeredChoiceStrategy, seedType string) []*Prog {
	ps := r.generateFromDistributedChoiceTable(s, sCalls, hmcfg, lcs, seedType)
	if len(ps) == 0 {
		return ps
	}
	r.expandWithDCT(ps, sCalls, hmcfg, lcs, seedType)
	return ps
}

func avgCalls(ps []*Prog) int {
	total := 0
	for _, p := range ps {
		total += len(p.Calls)
	}
	return total / len(ps)
}
