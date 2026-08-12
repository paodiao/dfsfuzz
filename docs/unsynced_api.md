# API 变更与未同步接口清单

> 范围：`Generate`/`Deserialize`/`Minimize`/`Mutate`/`MutateWithHints`/`BuildChoiceTable`/`LogEntry`/`ipc.Exec` 等接口签名变更后，
> 未随之一并适配的调用方清单。**默认构建链（`make all`）不涉及这些包**，故暂不统一；如需启用对应工具，按本清单适配即可。

## 构建范围（src/Makefile:111-113）

```makefile
all: host target custom_compilers
host: manager prog2c #mutate runtest repro prog2c upgrade db
target: fuzzer executor checker execprog corpusprog #stress
```

- **默认构建**：syz-manager、syz-prog2c、syz-fuzzer、syz-executor、checker（features-check-ssh）、syz-execprog、corpusprog
- **已注释（永不构建）**：syz-mutate、syz-stress、syz-runtest、syz-repro、syz-db、syz-hub
- **有 target 但非默认**：syz-verifier、syz-runner、syz-trace2syz、syz-expand、syz-imagegen、syz-hubtool、syz-bisect、syz-crush、syz-reporter、syz-ci 等（`make <target>` 显式调用才构建）
- **vm 后端**：仅 qemu/odroid 适配了 vmimpl 新接口；bhyve/adb/kvm/gce/gvisor/vmware/isolated/vmm 未适配（Monarch 实际只使用 qemu）

## 新接口签名（适配模板）

```go
// 1. Generate：9 参，返回 (*Prog, map[string]bool)
p, _ := target.Generate(rs, ncalls, ct, files, isForSrv, sCalls, enableC2san, &Hmdfs_config{}, idx)
//    hmcfg 必须非 nil（generation.go:33 访问 hmcfg.DfsName）

// 2. Deserialize：入参 [][]byte（多程序），返回 ([]*Prog, error)
ps, err := target.Deserialize([][]byte{data}, mode)
p := ps[0] // 单程序场景取首元素

// 3. Minimize：6 参，pred 收 []*Prog，返回 ([]*Prog, int)
minPs, ci := Minimize([]*Prog{p}, callIndex, subNum, crash, srvNum,
    func(ps []*Prog, callIndex int) bool { return pred(ps[0], callIndex) })
p1 := minPs[0]

// 4. Mutate：10 参，corpus 类型 []*Prog → [][]*Prog
p.Mutate(rs, ncalls, ct, [][]*Prog{corpus}, sCalls, srvNum, hasFail, enableC2san, &Hmdfs_config{}, idx)

// 5. MutateWithHints：包级函数（原为 Prog 方法）
MutateWithHints([]*Prog{p}, subNum, callIndex, comps, func(ps []*Prog) { ... })

// 6. BuildChoiceTable：corpus 类型 []*Prog → [][]*Prog
target.BuildChoiceTable([][]*Prog{corpus}, enabled)

// 7. LogEntry：字段 P *Prog → Ps []*Prog（multi-prog 日志）
ent.Ps[0]

// 8. ipc.Env.Exec：7 返回值
//    (output, infos, hanged, err0, fsMds, testdirIno, hmdfsTraceEvents)
output, info, hanged, err, _, _, _ := env.Exec(opts, ps)

// 9. ipc.MakeEnv：3 参 (config, pid, shmId)；host.Check 参数已扩展
```

## 已适配（本次）

| 位置 | 内容 |
|---|---|
| src/prog 全部测试（any/encoding/encodingexec/hints/prio/rand/checksum/minimization/size/prog/mutation/parse/dynamic） | Generate/Deserialize/Minimize/Mutate/MutateWithHints/BuildChoiceTable/LogEntry.Ps 全部新签名 |
| src/prog/test/fuzz.go | FuzzDeserialize 适配 [][]byte + Ps[0] + Mutate 10 参 |
| src/tools/syz-execprog/execprog.go:172 | env.Exec 7 返回值 |

## 未适配清单（`GOOS=linux go build ./...` 实测 98 处错误的非构建链部分）

### 已注释工具（如需启用按模板修）

| 文件 | 位置 | 差异 |
|---|---|---|
| tools/syz-mutate/mutate.go | 69/75/82/89 | corpus [][]*Prog 误用；Generate 3 参；Deserialize []byte；Mutate 4 参 |
| tools/syz-stress/stress.go | 73/96/105/107/110/111/113/136 | host.Check 参数；ipc.MakeEnv 2 参；Generate 3 参；Mutate/Clone；env.Exec 参数 |
| tools/syz-upgrade/upgrade.go | 43/47 | Deserialize []byte + p 单 prog 用法 |
| tools/syz-db/syz-db.go | 97/101/108/128/146 | Deserialize []byte；rec.Val [][]byte 类型链（WriteFile 需 rec.Val[0] 或拼接） |
| tools/syz-repro/repro.go | 79/81 | res.Prog 字段不存在（repro.Result 结构变化） |

### 非默认 target 工具

| 文件 | 位置 | 差异 |
|---|---|---|
| syz-verifier/verifier.go | 232 | Generate 3 参（作为返回值） |
| tools/syz-expand/expand.go | 46/51 | Deserialize []byte + p.SerializeVerbose |
| tools/syz-imagegen/imagegen.go | 672/677 | Deserialize []byte + p.SerializeForExec |
| tools/syz-trace2syz/trace2syz.go | 113 | prog.Serialize() []byte 放入需要 [][]byte 的结构 |
| tools/syz-hubtool/hubtool.go | 126/147 | Deserialize 入参 + p.Serialize（[]*Prog） |
| tools/corpusprog.go | 35 附近 | 若启用需确认 rec.Val（[][]byte）与 p0 用法 |

### 测试文件（仅在 `go test ./...` 时编译）

| 文件 | 位置 | 差异 |
|---|---|---|
| syz-fuzzer/fuzzer_test.go | 83 | Generate 3 参 |
| pkg/csource/csource_test.go | 53/67/127/191 | Generate 3 参 ×2、Minimize 4 参、Deserialize []byte |

### vm 后端（vmimpl 接口扩展后未适配；仅 qemu/odroid 可用）

| 文件 | 差异 |
|---|---|
| vm/bhyve、vm/adb、vm/kvm、vm/gce、vm/gvisor、vm/vmware、vm/isolated、vm/vmm | Pool.Create 需返回 3 值（+io.ReadCloser）；Instance 需实现 GetSSHArgs(bin)；merger.Add 需 3 参（+chan bool）；merger.AddDecoder 需 4 参 |

### 其他（与 prog API 无关的预先遗留）

| 文件 | 位置 | 差异 |
|---|---|---|
| tools/syz-execprog/execprog.go 其余 | — | 已修 Exec 7 值；若仍报错按 ipc.go 核对 MakeEnv/ExecOpts |
| dashboard / pkg/analytics 等 | — | 未检查（非构建链） |
