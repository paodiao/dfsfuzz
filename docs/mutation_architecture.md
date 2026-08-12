# Monarch Mutation Architecture Analysis

## 1. Overall Mutation Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              proc.go: loop()                                 │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                    "Mutate an existing prog" Branch                  │   │
│  │                                                                      │   │
│  │  seedPS := fuzzerSnapshot.chooseProgram(proc.rnd)                   │   │
│  │                                                                      │   │
│  │  ┌──────────────────────────┐    ┌──────────────────────────────┐  │   │
│  │  │ HasCrashFail/HasNetFail? │───▶│ RandomInsertFailure()        │  │   │
│  │  │ AND NetFailure/NodeCrash?│    │ (故障注入突变)                │  │   │
│  │  │ AND OutOfWrap()?         │    │                              │  │   │
│  │  └──────────────────────────┘    └──────────────────────────────┘  │   │
│  │              │                                                      │   │
│  │              ▼ (else)                                               │   │
│  │  ┌──────────────────────────────────────────────────────────────┐  │   │
│  │  │                    p.Mutate()                                 │  │   │
│  │  │                    (常规突变)                                  │  │   │
│  │  └──────────────────────────────────────────────────────────────┘  │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
```

## 2. Mutation Entry Point

### Location: `src/syz-fuzzer/proc.go:264-293`

```go
// Mutate an existing prog.
seedPS := fuzzerSnapshot.chooseProgram(proc.rnd)
if !seedPS[0].HasCrashFail && !seedPS[0].HasNetFail &&
    (proc.fuzzer.config.NetFailure || proc.fuzzer.config.NodeCrash) &&
    prog.OutOfWrap(proc.rnd, seedPS[0].Target, 1, 5) {
    // Path 1: RandomInsertFailure
    ps = prog.Clones(seedPS)
    prog.RandomInsertFailure(ps, ...)
} else {
    // Path 2: Normal Mutate
    for idx, tmp_p := range seedPS {
        p := tmp_p.Clone()
        if idx >= proc.fuzzer.config.ServNum {
            p.Mutate(proc.rnd, prog.RecommendedCalls, ct, fuzzerSnapshot.corpus, ...)
        }
        ps = append(ps, p)
    }
}
```

## 3. Normal Mutation Strategies

### Location: `src/prog/mutation.go:27-85`

```go
func (p *Prog) Mutate(rs rand.Source, ncalls int, ct *ChoiceTable, corpus [][]*Prog, ...) {
    for stop, ok := false, false; !stop; stop = ok && len(p.Calls) != 0 && r.oneOf(3) {
        switch {
        case r.oneOf(5):                    // 20% probability
            ok = ctx.squashAny()
        case r.nOutOf(1, 100):              // 1% probability
            ok = ctx.splice()
        case r.nOutOf(20, 31):              // ~65% probability
            ok = ctx.insertCall()
        case r.nOutOf(10, 11):              // ~91% probability
            ok = ctx.mutateArg()
        case r.nOutOf(9, 10):               // 90% probability (only if hasFail)
            ok = ctx.mutateFailPos()
        default:                            // remaining probability
            ok = ctx.removeCall()
        }
    }
}
```

### 3.1 Strategy Probability Table

| Strategy | Probability | Condition | Description |
|----------|-------------|-----------|-------------|
| `squashAny()` | 1/5 (20%) | - | Squash complex pointer arguments into ANY |
| `splice()` | 1/100 (1%) | !hasFail | Splice with another program from corpus |
| `insertCall()` | 20/31 (~65%) | - | Insert a new syscall at random position |
| `mutateArg()` | 10/11 (~91%) | - | Mutate an argument of a random call |
| `mutateFailPos()` | 9/10 (90%) | hasFail | Move failure injection position |
| `removeCall()` | remaining | - | Remove a random call |

### 3.2 Strategy Details

#### 3.2.1 `squashAny()` - [mutation.go:132-171](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L132)

```
Purpose: Convert complex pointer arguments into ANY type, then mutate the blob data

Flow:
1. Find all complex pointers in program
2. Select a random pointer
3. Squash it into ANY type
4. Mutate the blob data using mutateData()
```

#### 3.2.2 `splice()` - [mutation.go:104-130](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L104)

```
Purpose: Combine current program with another program from corpus

Flow:
1. Select a random program p0 from corpus (without failures)
2. Choose a random index i
3. Concatenate: p.Calls[:i] + p0.Calls + p.Calls[i:]
4. Truncate if exceeds ncalls limit
```

#### 3.2.3 `insertCall()` - [mutation.go:175-195](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L175)

```
Purpose: Insert a new syscall at a random position

Flow:
1. Check if program already has maximum calls
2. Choose insertion position with bias towards end
3. Analyze state at insertion point
4. Generate new call(s) using r.generateCall()
5. Insert before the chosen position
```

#### 3.2.4 `mutateArg()` - [mutation.go:222-273](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L222)

```
Purpose: Mutate an argument of a random call

Flow:
1. Choose a call based on argument complexity (chooseCall)
2. Collect all mutable arguments
3. Choose an argument to mutate
4. Call type-specific mutate function
5. Update sizes if needed
```

#### 3.2.5 `mutateFailPos()` - [mutation.go:973-1015](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L973)

```
Purpose: Move failure injection position (for programs with failures)

Flow:
1. Find a syz_failure_sync call
2. If even ID (failure start): move it before a non-failure call
3. If odd ID (failure end): move it after a non-failure call
```

#### 3.2.6 `removeCall()` - [mutation.go:198-219](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L198)

```
Purpose: Remove a random call from program

Flow:
1. Select a random non-failure call
2. Remove it from program
```

## 4. Argument Mutation Details

### Location: `src/prog/mutation.go:295-444`

### 4.1 Type-Specific Mutation Functions

| Type | Function | Strategy |
|------|----------|----------|
| `IntType` | `mutateInt()` | Add/subtract, XOR bit flip |
| `FlagsType` | `mutate()` | Random flag value |
| `LenType` | `mutate()` | Adjust size |
| `ResourceType` | `mutate()` | Regenerate |
| `VmaType` | `mutate()` | Regenerate |
| `ProcType` | `mutate()` | Regenerate |
| `BufferType` | `mutate()` | mutateData() |
| `ArrayType` | `mutate()` | Add/remove/change elements |

### 4.2 `mutateInt()` - Integer Mutation

```go
func mutateInt(r *randGen, a *ConstArg, t *IntType) uint64 {
    switch {
    case r.nOutOf(1, 3):  // 33%: increment
        return a.Val + (uint64(r.Intn(4)) + 1)
    case r.nOutOf(1, 2):  // 50%: decrement
        return a.Val - (uint64(r.Intn(4)) + 1)
    default:              // 17%: XOR bit flip
        return a.Val ^ (1 << uint64(r.Intn(int(t.TypeBitSize()))))
    }
}
```

### 4.3 `mutateData()` - Blob Data Mutation - [mutation.go:730-870](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L730)

```go
var mutateDataFuncs = [...]func(r *randGen, data []byte, minLen, maxLen uint64) ([]byte, bool){
    // 1. Flip a random bit
    // 2. Insert random bytes
    // 3. Remove bytes
    // 4. Append bytes
    // 5. Replace int8/16/32/64 with random value
    // 6. Add/subtract from int8/16/32/64
    // 7. Set int8/16/32/64 to interesting value
    // 8. Overwrite with random bytes
}
```

## 5. Failure Injection Mutation

### Location: `src/prog/mutation.go:1402-1450`

### 5.1 `RandomInsertFailure()`

```go
func RandomInsertFailure(ps []*Prog, srvNum int, rs rand.Source, sCalls *SpecialCalls, initIp string, hmcfg *Hmdfs_config) {
    // 1. Select random servers to fail
    randomSrvs := r.RandSet(0, srvNum-1, r.Intn(srvNum)+1)
    
    // 2. For each server, decide crash or network failure
    for i, srv := range randomSrvs {
        crashFail := r.nOutOf(1, 2)  // 50% crash, 50% network
        failInfo := SrvFailInfo{srv, make([]int, 0)}
        if !crashFail {
            failInfo.PartNodes = r.RandSetExcept(...)  // Affected nodes
        }
        ctx.genSrvFailCalls(&srvStartIdx, crashFail, failInfo, ps, srv)
    }
    
    // 3. For each client, generate sync calls
    for clt := srvNum; clt < len(ps); clt++ {
        // Insert sync calls at random positions
    }
}
```

### 5.2 Failure Call Sequence Pattern

```
Server failure pattern:
  recv(idx) → syz_down(crash/node) → send(idx+1) → recv(idx+2) → syz_up() → send(idx+3)

Client sync pattern:
  syz_failure_sync(idx, nodeIdx)
```

## 6. Key Parameters

### 6.1 Global Constants

| Constant | Value | Description |
|----------|-------|-------------|
| `maxBlobLen` | 100KB | Maximum blob length |
| `maxDelta` | 35 | Maximum delta for integer mutations |
| `RecommendedCalls` | 30 | Recommended max calls in program |
| `generatePeriod` | 10 | Generate new prog every N iterations |

### 6.2 Mutation Loop Control

```go
for stop, ok := false, false; !stop; stop = ok && len(p.Calls) != 0 && r.oneOf(3) {
    // stop condition: mutation succeeded AND 1/3 probability to continue
}
```

This means:
- If mutation fails, try another strategy
- If mutation succeeds, 33% chance to continue mutating

## 7. Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              Mutation Architecture                               │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                           proc.go: loop()                                 │   │
│  │                                                                          │   │
│  │   ┌───────────────┐                                                      │   │
│  │   │ corpus select │                                                      │   │
│  │   └───────┬───────┘                                                      │   │
│  │           │                                                              │   │
│  │           ▼                                                              │   │
│  │   ┌───────────────────┐      ┌───────────────────────────────────────┐  │   │
│  │   │ HasFail && Config │─Yes─▶│         RandomInsertFailure()         │  │   │
│  │   │ && OutOfWrap?     │      │  ┌─────────────────────────────────┐  │  │   │
│  │   └───────────────────┘      │  │ genSrvFailCalls()               │  │  │   │
│  │           │No                │  │   - recv/send sync              │  │  │   │
│  │           ▼                  │  │   - syz_down/syz_up             │  │  │   │
│  │   ┌───────────────────┐      │  │   - crash/network failure      │  │  │   │
│  │   │    p.Mutate()     │      │  └─────────────────────────────────┘  │  │   │
│  │   │                   │      │  ┌─────────────────────────────────┐  │  │   │
│  │   │ ┌───────────────┐ │      │  │ Client sync insertion          │  │  │   │
│  │   │ │ squashAny()   │ │      │  │   - syz_failure_sync           │  │  │   │
│  │   │ ├───────────────┤ │      │  └─────────────────────────────────┘  │  │   │
│  │   │ │ splice()      │ │      └───────────────────────────────────────┘  │   │
│  │   │ ├───────────────┤ │                                                      │   │
│  │   │ │ insertCall()  │ │      ┌───────────────────────────────────────┐  │   │
│  │   │ ├───────────────┤ │      │           generateCall()               │  │   │
│  │   │ │ mutateArg()   │─┼─────▶│  ┌─────────────────────────────────┐  │  │   │
│  │   │ ├───────────────┤ │      │  │ Type-specific mutation:         │  │  │   │
│  │   │ │ mutateFailPos │ │      │  │  - IntType: +/-/XOR             │  │  │   │
│  │   │ ├───────────────┤ │      │  │  - BufferType: mutateData()     │  │  │   │
│  │   │ │ removeCall()  │ │      │  │  - ArrayType: add/remove/change │  │  │   │
│  │   │ └───────────────┘ │      │  │  - FlagsType: random flag       │  │  │   │
│  │   └───────────────────┘      │  └─────────────────────────────────┘  │  │   │
│  │                              └───────────────────────────────────────┘  │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                        mutateData() Functions                            │   │
│  │                                                                          │   │
│  │  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌─────────────┐       │   │
│  │  │ Flip bit    │ │ Insert bytes│ │ Remove bytes│ │ Append bytes│       │   │
│  │  └─────────────┘ └─────────────┘ └─────────────┘ └─────────────┘       │   │
│  │  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐ ┌─────────────┐       │   │
│  │  │ Replace int │ │ Add/sub int │ │ Interesting │ │ Overwrite   │       │   │
│  │  └─────────────┘ └─────────────┘ └─────────────┘ └─────────────┘       │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## 8. Key Files

| File | Purpose |
|------|---------|
| `src/syz-fuzzer/proc.go` | Main mutation entry point in loop() |
| `src/prog/mutation.go` | Core mutation strategies implementation |
| `src/prog/rand.go` | Random generation and helper functions |
| `src/prog/prog.go` | Program data structures |

## 9. smashInput Mutation Flow

### 9.1 Overview

`smashInput()` is called when a new program is added to corpus (triggered by `triageInput()`). It performs **deep testing** on new programs through two phases:

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           smashInput() Flow                                      │
│                                                                                 │
│  Trigger: New program added to corpus (ProgSmashed flag not set)               │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                        Phase 1: Failure Enumeration                       │   │
│  │                                                                           │   │
│  │   if (NetFailure || NodeCrash) && !(HasNetFail || HasCrashFail)          │   │
│  │       enumFailures(ps)                                                    │   │
│  │           │                                                               │   │
│  │           ├──▶ genNodeCombs() → enumInner(crash failure)                  │   │
│  │           │        └──▶ InsertFailure() - Enumerate all crash positions   │   │
│  │           │                                                               │   │
│  │           └──▶ genEdgeCombs() → enumInner(network failure)                │   │
│  │                    └──▶ InsertFailure() - Enumerate all network positions │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                        Phase 2: Normal Mutation (100 iterations)          │   │
│  │                                                                           │   │
│  │   for i := 0; i < 100; i++ {                                             │   │
│  │       ps := Clones(item.ps)                                              │   │
│  │       randIdx := rand.Intn(psNum-srvNum) + srvNum                        │   │
│  │       ps[randIdx].Mutate(...)                                            │   │
│  │       execute(ps)                                                         │   │
│  │   }                                                                       │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 9.2 `enumFailures()` - Failure Enumeration - [proc.go:625-642](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/proc.go#L625)

**Purpose**: Systematically enumerate ALL possible failure injection positions for a program.

```go
func (proc *Proc) enumFailures(ps []*prog.Prog) {
    srvNum := proc.fuzzer.config.ServNum
    cltNum := len(ps) - srvNum

    // Phase 1: Crash failure enumeration
    if !ps[0].HasCrashFail {
        combs := genNodeCombs(srvNum)           // Generate server combinations
        proc.enumInner(combs, ps, true)         // isCrashFailure = true
    }

    // Phase 2: Network failure enumeration
    if !ps[0].HasNetFail {
        combs := genEdgeCombs(srvNum, cltNum)   // Generate edge combinations
        proc.enumInner(combs, ps, false)        // isCrashFailure = false
    }
}
```

### 9.3 Failure Combination Generation

#### `genNodeCombs()` - Crash Failure Combinations - [proc.go:557-572](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/proc.go#L557)

```go
// Generate all combinations of servers that can crash
// Example: For 3 servers, generates: [[0], [1], [2]]
func genNodeCombs(srvNum int) (combs [][]prog.SrvFailInfo) {
    for sub := 1; sub <= 1; sub++ {  // Currently only single server failure
        idxCombs := combin.Combinations(srvNum, sub)
        for _, c := range idxCombs {
            comb := make([]prog.SrvFailInfo, 0)
            for _, i := range c {
                comb = append(comb, prog.SrvFailInfo{i, nil})
            }
            combs = append(combs, comb)
        }
    }
    return combs
}
```

#### `genEdgeCombs()` - Network Failure Combinations - [proc.go:574-601](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/proc.go#L574)

```go
// Generate all edge combinations (server-client connections)
// Example: For 1 server + 2 clients, generates edges: [(0,1), (0,2)]
func genEdgeCombs(srvNum int, cltNum int) (combs [][]prog.SrvFailInfo) {
    conns := make([]prog.Conn, 0)
    // Generate all edges between servers and clients
    for i := 0; i < srvNum; i++ {
        for j := i + 1; j < srvNum+cltNum; j++ {
            conns = append(conns, prog.Conn{i, j})
        }
    }
    // Generate combinations
    for _, c := range combin.Combinations(len(conns), 1) {
        // Build SrvFailInfo with PartNodes
        ...
    }
    return combs
}
```

### 9.4 `InsertFailure()` - Enumerative Failure Injection - [mutation.go:1343-1399](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L1343)

**Key Difference from `RandomInsertFailure()`**: This function enumerates ALL possible sync point positions.

```go
func InsertFailure(rs rand.Source, ncalls int, ct *ChoiceTable, ps []*Prog, srvComb []SrvFailInfo, ...) {
    // For each server in combination
    for _, srvItem := range srvComb {
        ctx.genSrvFailCalls(&srvSyncIdx, crashFailure, srvItem, ps, srvIdx)
    }

    queue := make([][]*Prog, 0)
    queue = append(queue, ps)

    // Enumerate all client sync positions
    for clt := srvNum; clt < len(ps); clt++ {
        for _, srvItem := range srvComb {
            tmpQueue := make([][]*Prog, 0)
            for _, ps1 := range queue {
                // Enumerate ALL possible sync positions
                ret := ctx.enumSyncPoint(ps1, clt, srvItem.Srv)
                tmpQueue = append(tmpQueue, ret...)
            }
            queue = tmpQueue
        }
    }
    // Send all variants to execution channel
    for _, ps1 := range queue {
        ch <- ps1
    }
}
```

### 9.5 `enumSyncPoint()` - Sync Position Enumeration - [mutation.go:1246-1328](file:///d:/科研/博士复现/原版备份/Monarch-master/src/prog/mutation.go#L1246)

```go
// Enumerate all possible sync point positions for a client
func (ctx *mutator) enumSyncPoint(ps1 []*Prog, clt int, srv int) []*Prog {
    startPosList, endPosList := InsertablePos(ps1, clt, srv, ctx.srvNum)

    // Enumerate all combinations of start and end positions
    for _, call1 := range startPosList {
        for _, call2 := range endPosList {
            if isAdjacent(ps1[clt], call1, call2) {
                ps2 := Clones(ps1)
                ctx.insertSync(call1, call2, &cltSyncIdx, ps2, clt, srv)
                newPs = append(newPs, ps2)
            }
        }
    }
    return newPs
}
```

## 10. Comparison: loop() vs smashInput() Mutation

### 10.1 Key Differences

| Aspect | `loop()` Mutation | `smashInput()` Mutation |
|--------|-------------------|------------------------|
| **Trigger** | Every iteration | New program added to corpus |
| **Frequency** | Continuous | Once per new program |
| **Failure Injection** | `RandomInsertFailure()` - Random | `enumFailures()` - Exhaustive enumeration |
| **Normal Mutation** | 1 iteration | 100 iterations |
| **Goal** | Ongoing exploration | Deep testing of new seeds |

### 10.2 Failure Injection Comparison

| Aspect | `RandomInsertFailure()` | `InsertFailure()` (via enumFailures) |
|--------|------------------------|-------------------------------------|
| **Server Selection** | Random subset | All combinations |
| **Failure Type** | Random (crash/network) | Both types enumerated separately |
| **Affected Nodes** | Random selection | All edge combinations |
| **Sync Position** | Random position | ALL possible positions enumerated |
| **Output Count** | 1 variant | Many variants (exponential) |

### 10.3 Flow Comparison Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    Failure Injection: Random vs Enumerative                     │
│                                                                                 │
│  ┌─────────────────────────────────┐  ┌─────────────────────────────────────┐  │
│  │     RandomInsertFailure()       │  │        enumFailures()                │  │
│  │     (in loop())                 │  │        (in smashInput())            │  │
│  │                                 │  │                                     │  │
│  │  Random server: srv=1           │  │  Enumerate all servers:             │  │
│  │  Random type: crash             │  │    srv=0, srv=1, srv=2, ...         │  │
│  │  Random nodes: [2,3]            │  │  For each server:                   │  │
│  │  Random position: call[5]       │  │    Enumerate crash failure          │  │
│  │                                 │  │    Enumerate network failure        │  │
│  │  Output: 1 variant              │  │    For each client:                 │  │
│  │                                 │  │      Enumerate ALL sync positions   │  │
│  │                                 │  │                                     │  │
│  │                                 │  │  Output: N × M × K variants         │  │
│  └─────────────────────────────────┘  └─────────────────────────────────────┘  │
│                                                                                 │
│  Example: 1 server, 2 clients, 10 calls per client                             │
│                                                                                 │
│  RandomInsertFailure(): 1 variant                                              │
│  enumFailures(): ~100 variants (all position combinations)                     │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## 11. Complete Mutation Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                        Complete Mutation Architecture                           │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                         triageInput()                                     │   │
│  │                              │                                           │   │
│  │                              ▼                                           │   │
│  │              ┌───────────────────────────────┐                           │   │
│  │              │ Add to corpus & ProgSmashed? │                           │   │
│  │              └───────────────────────────────┘                           │   │
│  │                              │ No                                        │   │
│  │                              ▼                                           │   │
│  │                   ┌─────────────────────┐                                │   │
│  │                   │  smashInput()       │                                │   │
│  │                   └─────────────────────┘                                │   │
│  │                         │           │                                    │   │
│  │            ┌────────────┘           └────────────┐                       │   │
│  │            ▼                                     ▼                       │   │
│  │  ┌─────────────────────┐              ┌─────────────────────┐           │   │
│  │  │   enumFailures()    │              │  Normal Mutation    │           │   │
│  │  │   (Exhaustive)      │              │  (100 iterations)   │           │   │
│  │  │                     │              │                     │           │   │
│  │  │ ┌─────────────────┐ │              │ for i := 0; i<100;  │           │   │
│  │  │ │ Crash Failures  │ │              │   p.Mutate()        │           │   │
│  │  │ │ genNodeCombs()  │ │              │   execute()         │           │   │
│  │  │ └─────────────────┘ │              │                     │           │   │
│  │  │ ┌─────────────────┐ │              └─────────────────────┘           │   │
│  │  │ │ Net Failures    │ │                                                │   │
│  │  │ │ genEdgeCombs()  │ │                                                │   │
│  │  │ └─────────────────┘ │                                                │   │
│  │  │         │           │                                                │   │
│  │  │         ▼           │                                                │   │
│  │  │ ┌─────────────────┐ │                                                │   │
│  │  │ │ InsertFailure() │ │                                                │   │
│  │  │ │ Enumerate ALL   │ │                                                │   │
│  │  │ │ sync positions  │ │                                                │   │
│  │  │ └─────────────────┘ │                                                │   │
│  │  └─────────────────────┘                                                │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                              loop()                                       │   │
│  │                                 │                                        │   │
│  │                 ┌───────────────┴───────────────┐                        │   │
│  │                 ▼                               ▼                        │   │
│  │        ┌─────────────────┐             ┌─────────────────┐              │   │
│  │        │ Generate New    │             │ Mutate Existing │              │   │
│  │        │ (generatePeriod)│             │                 │              │   │
│  │        └─────────────────┘             └─────────────────┘              │   │
│  │                                               │                         │   │
│  │                          ┌────────────────────┴────────────────────┐    │   │
│  │                          ▼                                         ▼    │   │
│  │                 ┌─────────────────────┐              ┌─────────────────┐ │   │
│  │                 │ RandomInsertFailure │              │   p.Mutate()    │ │   │
│  │                 │ (Random selection)  │              │   (1 iteration) │ │   │
│  │                 └─────────────────────┘              └─────────────────┘ │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## 12. Seed Lifecycle: Queue and Corpus Entry

### 12.1 Overview

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           Seed Lifecycle Flow                                    │
│                                                                                 │
│   ┌──────────────┐                                                              │
│   │ New Seed     │                                                              │
│   │ Generated    │                                                              │
│   └──────┬───────┘                                                              │
│          │                                                                      │
│          ▼                                                                      │
│   ┌──────────────┐      No new signal    ┌──────────────┐                      │
│   │  execute()   │ ─────────────────────▶│  Discarded   │                      │
│   └──────┬───────┘                        └──────────────┘                      │
│          │ Has new signal                                                      │
│          ▼                                                                      │
│   ┌──────────────────────┐                                                    │
│   │ WorkTriage Queue     │ ◀─── enqueueCallTriage()                           │
│   └──────────┬───────────┘                                                    │
│              │                                                                 │
│              ▼                                                                 │
│   ┌──────────────────────┐                                                    │
│   │   triageInput()      │                                                    │
│   │   - Verify signal    │                                                    │
│   │   - Minimize prog    │                                                    │
│   └──────────┬───────────┘                                                    │
│              │ Signal verified                                                 │
│              ▼                                                                 │
│   ┌──────────────────────┐                                                    │
│   │    Corpus            │ ◀─── addInputToCorpus()                            │
│   └──────────┬───────────┘                                                    │
│              │ ProgSmashed not set                                             │
│              ▼                                                                 │
│   ┌──────────────────────┐                                                    │
│   │   WorkSmash Queue    │ ◀─── enqueue(&WorkSmash{...})                      │
│   └──────────┬───────────┘                                                    │
│              │                                                                 │
│              ▼                                                                 │
│   ┌──────────────────────┐                                                    │
│   │   smashInput()       │                                                    │
│   │   - enumFailures()   │                                                    │
│   │   - 100 mutations    │                                                    │
│   └──────────────────────┘                                                    │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 12.2 Queue Entry Conditions

| Queue | Entry Condition | Entry Function |
|-------|-----------------|----------------|
| **WorkTriage** | `execute()` detected new signal | `enqueueCallTriage()` |
| **WorkCandidate** | Received from manager/hub | `addCandidateInput()` |
| **WorkSmash** | Added to corpus, `ProgSmashed` not set | `triageInput()` |
| **Corpus** | Signal verified in triage | `addInputToCorpus()` |

### 12.3 Detailed Entry Flow

#### 12.3.1 WorkTriage Entry - [proc.go:445-459](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/proc.go#L445)

```go
func (proc *Proc) enqueueCallTriage(ps []*Prog, info CallInfo, flags ProgTypes) {
    // Check if new signal exists
    if len(info.Signal) == 0 {
        return  // No new signal, don't enqueue
    }
    
    // Check if signal is already known
    if proc.fuzzer.corpusSignal.Diff(info.Signal).Empty() {
        return  // Signal already known, don't enqueue
    }
    
    // Enqueue for triage
    proc.fuzzer.workQueue.enqueue(&WorkTriage{
        ps:    ps,
        info:  info,
        flags: flags,
    })
}
```

#### 12.3.2 Corpus Entry - [fuzzer.go:540-560](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/fuzzer.go#L540)

```go
func (fuzzer *Fuzzer) addInputToCorpus(ps []*Prog, cliSignal, srvSignal Signal, sig hash.Sig) {
    // Check if already in corpus
    if fuzzer.corpusSignal.Dup(sig) {
        return  // Already exists
    }
    
    // Add to corpus
    fuzzer.corpus = append(fuzzer.corpus, &CorpusItem{
        ps:        ps,
        sig:       sig,
        cliSignal: cliSignal,
        srvSignal: srvSignal,
    })
    
    // Add signal to corpus signal set
    fuzzer.corpusSignal.Merge(cliSignal)
    fuzzer.corpusSignal.Merge(srvSignal)
    
    // Send to manager for distribution
    fuzzer.sendInputToManager(...)
}
```

#### 12.3.3 WorkSmash Entry - [proc.go:330-345](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/proc.go#L330)

```go
func (proc *Proc) triageInput(item *WorkTriage) {
    // ... verify signal, minimize ...
    
    // Add to corpus
    proc.fuzzer.addInputToCorpus(ps, cliSignal, srvSignal, sig)
    
    // If not smashed yet, enqueue for smashing
    if item.flags&ProgSmashed == 0 {
        proc.fuzzer.workQueue.enqueue(&WorkSmash{
            ps: ps,
        })
    }
}
```

#### 12.3.4 WorkCandidate Entry - [fuzzer.go:622-638](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/fuzzer.go#L622)

```go
// Called when receiving candidates from manager (via poll)
func (fuzzer *Fuzzer) addCandidateInput(candidate rpctype.RPCCandidate) {
    ps := fuzzer.deserializeInput(candidate.Prog)
    if ps == nil {
        return
    }
    
    flags := ProgCandidate
    if candidate.Minimized {
        flags |= ProgMinimized
    }
    if candidate.Smashed {
        flags |= ProgSmashed
    }
    
    fuzzer.workQueue.enqueue(&WorkCandidate{
        ps:    ps,
        flags: flags,
    })
}
```

### 12.4 Queue Priority Order

From [workqueue.go:103-141](file:///d:/科研/博士复现/原版备份/Monarch-master/src/syz-fuzzer/workqueue.go#L103):

```go
func (wq *WorkQueue) dequeue() (item interface{}) {
    // Priority order:
    // 1. triageCandidate (highest) - Triage from hub/manager
    // 2. candidate          - From hub/manager
    // 3. triage             - Locally generated
    // 4. smash              (lowest) - Deep testing
    
    if len(wq.triageCandidate) != 0 {
        // Return triageCandidate
    } else if len(wq.candidate) != 0 {
        // Return candidate
    } else if len(wq.triage) != 0 {
        // Return triage
    } else if len(wq.smash) != 0 {
        // Return smash
    }
    return nil
}
```

### 12.5 Complete Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    Queue and Corpus Entry Points                                 │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                         Local Fuzzer Flow                                │   │
│  │                                                                          │   │
│  │   generate() / mutate()                                                  │   │
│  │          │                                                               │   │
│  │          ▼                                                               │   │
│  │   ┌─────────────┐                                                        │   │
│  │   │  execute()  │                                                        │   │
│  │   └──────┬──────┘                                                        │   │
│  │          │                                                               │   │
│  │          ├──▶ No new signal ──▶ Discarded                                │   │
│  │          │                                                               │   │
│  │          └──▶ Has new signal                                             │   │
│  │                   │                                                      │   │
│  │                   ▼                                                      │   │
│  │          ┌─────────────────┐                                             │   │
│  │          │ enqueueCallTriage │──────────▶ WorkTriage Queue              │   │
│  │          └─────────────────┘                      │                     │   │
│  │                                                   │                     │   │
│  │                                                   ▼                     │   │
│  │                                          ┌─────────────────┐            │   │
│  │                                          │  triageInput()  │            │   │
│  │                                          └────────┬────────┘            │   │
│  │                                                   │                     │   │
│  │                                    ┌──────────────┴──────────────┐      │   │
│  │                                    │                             │      │   │
│  │                                    ▼                             ▼      │   │
│  │                           ┌───────────────┐           ┌───────────────┐ │   │
│  │                           │    Corpus     │           │ WorkSmash Queue│ │   │
│  │                           │addInputToCorpus│          │ (if !Smashed) │ │   │
│  │                           └───────┬───────┘           └───────┬───────┘ │   │
│  │                                   │                           │         │   │
│  │                                   ▼                           ▼         │   │
│  │                          sendInputToManager            smashInput()    │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                         Manager/Hub Flow                                 │   │
│  │                                                                          │   │
│  │   Manager/Hub                                                            │   │
│  │       │                                                                  │   │
│  │       │ poll() response                                                  │   │
│  │       ▼                                                                  │   │
│  │   ┌─────────────────────────────────────────────────────────────┐       │   │
│  │   │  Candidates: programs from other fuzzers                     │       │   │
│  │   │  NewInputs: programs already in corpus from other fuzzers    │       │   │
│  │   └─────────────────────────────────────────────────────────────┘       │   │
│  │       │                           │                                      │   │
│  │       ▼                           ▼                                      │   │
│  │   ┌─────────────────┐     ┌─────────────────┐                           │   │
│  │   │addCandidateInput│     │addInputFromAnother│                          │   │
│  │   └────────┬────────┘     │Fuzzer            │                          │   │
│  │            │              └────────┬─────────┘                          │   │
│  │            ▼                       │                                    │   │
│  │   WorkCandidate Queue       ┌──────┴──────┐                             │   │
│  │            │                │             │                             │   │
│  │            ▼                ▼             ▼                             │   │
│  │   execute() again     Corpus (direct)   Signal (merged)                 │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 12.6 Key Points Summary

1. **New seeds are NOT directly added to any queue or corpus**
   - They must first pass through `execute()` to check for new signal

2. **WorkTriage entry condition**: `execute()` detected new signal that is not already in `corpusSignal`

3. **Corpus entry condition**: `triageInput()` verified the signal is stable and reproducible

4. **WorkSmash entry condition**: Added to corpus but `ProgSmashed` flag not set

5. **WorkCandidate entry condition**: Received from manager/hub via `poll()` response

6. **Queue priority**: `triageCandidate > candidate > triage > smash`
   - This ensures important work (triage from hub) is processed first
   - Smash is lowest priority as it's a one-time deep testing

### 12.7 triageCandidate Queue Entry Path

The `triageCandidate` queue is used for `WorkTriage` items that have `ProgCandidate` flag set. Here's how it gets populated:

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    triageCandidate Queue Entry Path                              │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │  Step 1: WorkCandidate arrives from manager/hub                          │   │
│  │                                                                          │   │
│  │  addCandidateInput() in fuzzer.go:622-638                               │   │
│  │  ┌────────────────────────────────────────────────────────────────┐     │   │
│  │  │ flags := ProgCandidate                                          │     │   │
│  │  │ if candidate.Minimized { flags |= ProgMinimized }               │     │   │
│  │  │ if candidate.Smashed { flags |= ProgSmashed }                   │     │   │
│  │  │                                                                 │     │   │
│  │  │ workQueue.enqueue(&WorkCandidate{ps: ps, flags: flags})         │     │   │
│  │  └────────────────────────────────────────────────────────────────┘     │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                         │
│                                      ▼                                         │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │  Step 2: WorkCandidate dequeued and executed                             │   │
│  │                                                                          │   │
│  │  loop() in proc.go:202-203                                              │   │
│  │  ┌────────────────────────────────────────────────────────────────┐     │   │
│  │  │ case *WorkCandidate:                                            │     │   │
│  │  │     proc.execute(proc.execOpts, item.ps, item.flags, StatCandidate)│  │
│  │  │     // item.flags contains ProgCandidate                        │     │   │
│  │  └────────────────────────────────────────────────────────────────┘     │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                         │
│                                      ▼                                         │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │  Step 3: execute() detects new signal, calls enqueueCallTriage           │   │
│  │                                                                          │   │
│  │  execute() in proc.go:804-808                                           │   │
│  │  ┌────────────────────────────────────────────────────────────────┐     │   │
│  │  │ calls, extra := proc.fuzzer.checkNewSignal(ps[idx], info)       │     │   │
│  │  │ for _, callIndex := range calls {                               │     │   │
│  │  │     proc.enqueueCallTriage(ps, flags, callIndex, ...)           │     │   │
│  │  │     // flags is passed through from WorkCandidate.flags         │     │   │
│  │  │ }                                                               │     │   │
│  │  └────────────────────────────────────────────────────────────────┘     │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                         │
│                                      ▼                                         │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │  Step 4: enqueueCallTriage creates WorkTriage with flags                 │   │
│  │                                                                          │   │
│  │  enqueueCallTriage() in proc.go:847-861                                 │   │
│  │  ┌────────────────────────────────────────────────────────────────┐     │   │
│  │  │ proc.fuzzer.workQueue.enqueue(&WorkTriage{                      │     │   │
│  │  │     ps:    prog.Clones(ps),                                     │     │   │
│  │  │     call:  callIndex,                                           │     │   │
│  │  │     info:  info,                                                │     │   │
│  │  │     flags: flags,  // <-- Contains ProgCandidate!               │     │   │
│  │  │     ...                                                         │     │   │
│  │  │ })                                                              │     │   │
│  │  └────────────────────────────────────────────────────────────────┘     │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                         │
│                                      ▼                                         │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │  Step 5: workQueue.enqueue() routes to triageCandidate queue             │   │
│  │                                                                          │   │
│  │  enqueue() in workqueue.go:82-101                                       │   │
│  │  ┌────────────────────────────────────────────────────────────────┐     │   │
│  │  │ case *WorkTriage:                                               │     │   │
│  │  │     if item.flags&ProgCandidate != 0 {                          │     │   │
│  │  │         wq.triageCandidate = append(wq.triageCandidate, item)   │     │   │
│  │  │     } else {                                                    │     │   │
│  │  │         wq.triage = append(wq.triage, item)                     │     │   │
│  │  │     }                                                           │     │   │
│  │  └────────────────────────────────────────────────────────────────┘     │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
│  Result: WorkTriage with ProgCandidate flag → triageCandidate queue           │
└─────────────────────────────────────────────────────────────────────────────────┘
```

**Key insight**: The `flags` field is passed through the entire chain:
- `WorkCandidate.flags` → `execute(flags)` → `enqueueCallTriage(flags)` → `WorkTriage.flags`

This ensures that programs from other fuzzers (candidates) are prioritized in the `triageCandidate` queue for faster processing.

### 12.8 Complete WorkCandidate Flow Analysis

**Key Observation**: `addCandidateInput()` is the **ONLY** function that creates `WorkCandidate` items.

This means:
1. All `WorkCandidate` items have `ProgCandidate` flag set
2. All `WorkCandidate` items will enter `triageCandidate` queue (never `triage` queue)
3. After exiting `triageCandidate`, they become `WorkTriage` type
4. From this point, they follow the **same flow** as locally-generated seeds

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    WorkCandidate vs Local Seed Flow Comparison                  │
│                                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                    Remote Seeds (from hub/manager)                       │   │
│  │                                                                          │   │
│  │  addCandidateInput()                                                     │   │
│  │         │                                                                │   │
│  │         ▼                                                                │   │
│  │  WorkCandidate {flags: ProgCandidate}                                    │   │
│  │         │                                                                │   │
│  │         ▼                                                                │   │
│  │  execute() → new signal                                                  │   │
│  │         │                                                                │   │
│  │         ▼                                                                │   │
│  │  WorkTriage {flags: ProgCandidate} ──▶ triageCandidate queue            │   │
│  │         │                                            (highest priority) │   │
│  │         │                                                                │   │
│  │         └──────────────────────────────────────────────┐                │   │
│  │                                                          │                │   │
│  └──────────────────────────────────────────────────────────┼────────────────┘   │
│                                                             │                    │
│                                                             ▼                    │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │                    triageInput() - SAME PROCESSING                       │   │
│  │                                                                          │   │
│  │  - Verify signal stability                                               │   │
│  │  - Minimize (if ProgMinimized not set)                                   │   │
│  │  - Add to corpus                                                         │   │
│  │  - Enqueue WorkSmash (if ProgSmashed not set)                            │   │
│  │                                                                          │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                             ▲                    │
│                                                             │                    │
│  ┌──────────────────────────────────────────────────────────┼────────────────┐   │
│  │                    Local Seeds                            │                │   │
│  │                                                          │                │   │
│  │  generate() / mutate()                                   │                │   │
│  │         │                                                │                │   │
│  │         ▼                                                │                │   │
│  │  execute() → new signal                                  │                │   │
│  │         │                                                │                │   │
│  │         ▼                                                │                │   │
│  │  WorkTriage {flags: ProgNormal} ──▶ triage queue        │                │   │
│  │         │                           (lower priority)    │                │   │
│  │         │                                                │                │   │
│  │         └────────────────────────────────────────────────┘                │   │
│  │                                                                          │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
│  Conclusion: After triageCandidate/triage queue, ALL WorkTriage items         │
│              follow the SAME processing path in triageInput()                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 12.9 flags Field Usage in triageInput()

The `flags` field in `WorkTriage` only affects:

| Check | Effect |
|-------|--------|
| `flags & ProgMinimized == 0` | Skip minimization if already minimized |
| `flags & ProgSmashed == 0` | Enqueue for smashing if not yet smashed |

**Important**: The `ProgCandidate` flag itself does NOT affect processing logic in `triageInput()`. It only determines which queue the item enters (triageCandidate vs triage).
