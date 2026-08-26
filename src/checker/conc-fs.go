package checker

import (
	"fmt"
	"path/filepath"
	"sort"
	"syscall"

	"monarch/pkg/ipc"
	"monarch/pkg/log"
	"monarch/prog"

	"encoding/json"
	"os"
	"os/exec"
	"strings"
)

type Calls []*prog.Call

func ConcFSCheck(progs []*prog.Prog, infos []*ipc.ProgInfo,
	fsMds []map[string]prog.FileMetadata, srvNum int,
	fsType string, cfg_mode string, initIP string, testdirIno uint64,
	ft *prog.FileTree) (bool, []string) {

	log.Logf(0, "ConcFSCheck fsMds:%v", fsMds)

	log.Logf(0, "testdirIno: %x", testdirIno)

	// Cross-check states from multiple client nodes
	allIcs := make([]string, 0)
	for i := srvNum; i < len(fsMds)-1; i++ {
		ics := MdCmp(fsMds[i], fsMds[i+1], fsType)
		allIcs = append(allIcs, ics...)
	}

	if len(allIcs) > 0 {
		log.Logf(0, "Consistency FAILED: %d differences:", len(allIcs))
		for _, ic := range allIcs {
			log.Logf(0, "  %s", ic)
		}
		return false, allIcs
	}

	log.Logf(0, "Cross-client metadata check PASSED")

	// Final state checking
	symsc_stat := " "
	for filepath, fileMd := range fsMds[len(fsMds)-1] {
		statMd := fileMd.StatMd
		checksum := fileMd.Checksum
		symlinkPath := fileMd.SymlinkPath
		xattrs := ""
		j := 0
		for k, v := range fileMd.Xattr {
			if j != 0 {
				xattrs = xattrs + ";"
			}
			xattrs = xattrs + k + ":" + v
			j += 1
		}

		type_converted := 0
		switch statMd.Mode & syscall.S_IFMT {
		case 0x8000:
			type_converted = 1 // Regular file
		case 0x4000:
			type_converted = 2 // Directory
		case 0xA000:
			type_converted = 3 // Symbolic link
		case 0x1000:
			type_converted = 4 // Fifo file
		}

		//pass metadata to symsc
		symsc_stat = symsc_stat +
			fmt.Sprintf("%s\t%d\t%v\t%v\t%v\t%v\t%v\t%o\t%v\t%s\t%s\t\n",
				filepath, type_converted, statMd.Ino, statMd.Nlink,
				statMd.Size, statMd.Blksize, statMd.Blocks,
				statMd.Mode & ^uint32(syscall.S_IFMT), checksum,
				symlinkPath, xattrs)
	}

	prog1 := prog.Prog{
		Target: progs[0].Target,
		Calls:  make([]*prog.Call, 0),
	}
	seq_programs := make([][]int, 0)
	checkInfos := make([]prog.FileMetadata, 0)

	// Filter the sync pseudo syscall, e.g., syz_failure_recv/send/sync
	filterErr, newProgs := filter_failure_sync_calls(progs)
	if filterErr != nil {
		return false, nil
	}

	i := 0
	for procId, prog := range newProgs {
		prog1.Calls = append(prog1.Calls, prog.Calls...)
		prog_ops := make([]int, 0)
		for j := 0; j < len(prog.Calls); i, j = i+1, j+1 {
			prog.Calls[j].CheckInfo.ProcId = procId
			checkInfos = append(checkInfos, *prog.Calls[j].CheckInfo)
			prog_ops = append(prog_ops, i)
		}
		seq_programs = append(seq_programs, prog_ops)
	}

	// Serialize as symsc program string
	symscProgStr := prog1.SerializeForSymc3()
	if symscProgStr == "" {
		return false, nil
	}

	seq_programs_json, err := json.Marshal(seq_programs)
	if err != nil {
		log.Logf(0, "json marshal seq_programs error: %v\n", err)
		return false, nil
	}

	checkInfos_json, err := json.Marshal(checkInfos)
	if err != nil {
		log.Logf(0, "json marshal checkInfos error: %v\n", err)
		return false, nil
	}

	ex, err := os.Executable()
	if err != nil {
		panic(err)
	}
	exePath := filepath.Dir(ex)

	// Large payloads (prog text, checkInfos, seq programs, stat snapshot)
	// exceed the command-line limits (ARG_MAX / MAX_ARG_STRLEN with big
	// directories), so write them to temp files and pass the paths instead.
	writeTemp := func(name string, data []byte) (string, error) {
		fp, err := os.CreateTemp("", "symsc-"+name+"-*")
		if err != nil {
			return "", err
		}
		defer fp.Close()
		if _, err := fp.Write(data); err != nil {
			os.Remove(fp.Name())
			return "", err
		}
		return fp.Name(), nil
	}

	progFile, err := writeTemp("prog", []byte(symscProgStr))
	if err != nil {
		log.Logf(0, "write symsc prog temp file error: %v\n", err)
		return false, nil
	}
	defer os.Remove(progFile)

	infosFile, err := writeTemp("infos", checkInfos_json)
	if err != nil {
		log.Logf(0, "write symsc infos temp file error: %v\n", err)
		return false, nil
	}
	defer os.Remove(infosFile)

	seqsFile, err := writeTemp("seqs", seq_programs_json)
	if err != nil {
		log.Logf(0, "write symsc seqs temp file error: %v\n", err)
		return false, nil
	}
	defer os.Remove(seqsFile)

	statFile, err := writeTemp("stat", []byte(symsc_stat))
	if err != nil {
		log.Logf(0, "write symsc stat temp file error: %v\n", err)
		return false, nil
	}
	defer os.Remove(statFile)

	initTree := initTreeSubset(progs, ft)
	initFile, err := writeTemp("init", []byte(initTree))
	if err != nil {
		log.Logf(0, "write symsc init tree temp file error: %v\n", err)
		return false, nil
	}
	defer os.Remove(initFile)

	cmd := exec.Command("python3",
		filepath.Join(exePath, "../../checker/symsc/monarch_emul.py"),
		"-v", "-t", fsType, "-p", progFile,
		"-i", infosFile, "-c", statFile,
		"-g", seqsFile, "-l", initFile, "-s", fmt.Sprintf("%v", srvNum),
		"-f", cfg_mode, "-a", initIP, "-n", fmt.Sprintf("%v", testdirIno))

	log.Logf(0, "python3 %v -v -t %v -p %v -i %v -c %v -g %v -l %v -s %v -f \"%v\" -a \"%v\" -n %v",
		filepath.Join(exePath, "../../checker/symsc/monarch_emul.py"),
		fsType, progFile, infosFile,
		statFile, seqsFile, initFile,
		srvNum, cfg_mode, initIP, testdirIno)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	if err != nil {
		log.Logf(0, "consistency Python script error: %v\n", err)
		return false, nil
	}

	return true, nil
}

func filter_failure_sync_calls(progs []*prog.Prog) (error, []prog.Prog) {

	newProgs := make([]prog.Prog, len(progs))
	for idx, prog1 := range progs {
		filtered_calls := make([]*prog.Call, 0)
		for _, call := range prog1.Calls {
			log.Logf(0, "call name: %v\n", call.Meta.Name)
			if call.Meta.Name == "syz_failure_recv" ||
				call.Meta.Name == "syz_failure_send" ||
				call.Meta.Name == "syz_failure_sync" {
				continue
			}
			if call.Meta.Name == "ioctl" ||
				call.Meta.Name == "fcntl" ||
				call.Meta.Name == "sendfile" ||
				call.Meta.Name == "faccessat" ||
				call.Meta.Name == "preadv" ||
				call.Meta.Name == "pwritev" ||
				call.Meta.Name == "flock" ||
				strings.Contains(call.Meta.Name, "$") {
				return fmt.Errorf("not supported syscalls"), nil
			}
			filtered_calls = append(filtered_calls, call)
		}
		newProgs[idx].Calls = filtered_calls
	}
	return nil, newProgs
}

func xattrCmp(xattr1 map[string]string, xattr2 map[string]string) bool {
	for name, value1 := range xattr1 {
		if value2, ok := xattr2[name]; !ok || value1 != value2 {
			return false
		}
	}
	return true
}

func MdCmp(fsMd1 map[string]prog.FileMetadata,
	fsMd2 map[string]prog.FileMetadata, fsType string) []string {
	var ics []string

	log.Logf(0, "----- comparison: clientMdCmp: %v\n%v\n", fsMd1, fsMd2)

	if len(fsMd1) != len(fsMd2) {
		ics = append(ics, fmt.Sprintf("file count: %d vs %d", len(fsMd1), len(fsMd2)))
	}

	if len(fsMd1) == 0 && len(fsMd2) == 0 {
		return ics
	}

	for filepath, md1 := range fsMd1 {
		log.Logf(0, "globalFsMd: %s", filepath)
		md2, ok := fsMd2[filepath]
		if !ok {
			ics = append(ics, fmt.Sprintf("%s: missing from client2", filepath))
			continue
		}
		outputBuf := fmt.Sprintf("ConsistencySan stat:\n%+v\n%+v\n", md1, md2)
		log.Logf(0, outputBuf)
		ics = append(ics, compareFileMeta(filepath, md1, md2, fsType)...)
	}

	for filepath := range fsMd2 {
		if _, ok := fsMd1[filepath]; !ok {
			ics = append(ics, fmt.Sprintf("%s: missing from client1", filepath))
		}
	}

	if len(ics) == 0 {
		log.Logf(0, "----- consistency sanitizer: all equal")
	} else {
		for _, ic := range ics {
			log.Logf(0, "WARNING: consistencySanitizer: %v", ic)
		}
	}

	return ics
}

func compareFileMeta(path string, m1 prog.FileMetadata, m2 prog.FileMetadata, fsType string) []string {
	// Type mismatch is the strongest cross-node inconsistency signal: the
	// same path being a directory on one node and a non-directory on another
	// makes every other field (checksum/size/mtime/...) incomparable.
	// Report it directly and short-circuit instead of comparing the derived
	// fields (e.g. checksum 0 for a directory vs a real CRC for a file).
	//
	// Only the directory bit (S_IFDIR) is compared:
	// 1. Under CONFIG_HMDFS_FS_PERMISSION, uid/gid/mode permission bits
	//    on the remote view are simplified values (uid/gid inherited from
	//    the parent dir, mode hardcoded 0660), so they are intentionally
	//    inconsistent across nodes.
	// 2. A full S_IFMT type compare is avoided because hmdfs hardcodes
	//    the symlink inode type as S_IFREG (fill_inode_remote LNK branch),
	//    which would falsely report S_IFLNK (owning node) vs S_IFREG
	//    (remote view) on symlinks.
	//
	// Directory vs non-directory must match across nodes - that is the
	// meaningful cross-node type consistency signal (path-set equality is
	// already checked by MdCmp; a same-path file/dir type conflict would
	// otherwise go undetected).
	if (m1.StatMd.Mode & syscall.S_IFDIR) != (m2.StatMd.Mode & syscall.S_IFDIR) {
		return []string{fmt.Sprintf("%s: dirtype %o vs %o", path,
			m1.StatMd.Mode&syscall.S_IFDIR, m2.StatMd.Mode&syscall.S_IFDIR)}
	}

	var ics []string

	if m1.Checksum != m2.Checksum {
		ics = append(ics, fmt.Sprintf("%s: checksum %d vs %d", path, m1.Checksum, m2.Checksum))
	}
	/*
	 * Nlink and full-mode comparisons are skipped for hmdfs: the
	 * remote-view cached attr leaves nlink unset (0) and hardcodes mode
	 * permission bits (0660), so they are intentionally inconsistent across
	 * nodes (same rationale as mtime/size below). The directory bit is
	 * still compared for all filesystems.
	 */
	if fsType != "hmdfs" && m1.StatMd.Nlink != m2.StatMd.Nlink {
		ics = append(ics, fmt.Sprintf("%s: nlink %d vs %d", path,
			m1.StatMd.Nlink, m2.StatMd.Nlink))
	}
	if fsType != "hmdfs" && m1.StatMd.Mode != m2.StatMd.Mode {
		ics = append(ics, fmt.Sprintf("%s: mode %o vs %o", path,
			m1.StatMd.Mode, m2.StatMd.Mode))
	}
	/*
	 * Uid/Gid comparison disabled: with CONFIG_HMDFS_FS_PERMISSION the
	 * remote view returns uid/gid inherited from the parent dir (e.g.
	 * 1008) while the owning node returns the real ext4 values (e.g.
	 * 1000) - intentionally inconsistent by design. Kept commented out
	 * for reuse if another filesystem needs strict uid/gid comparison.
	 */
	// if m1.StatMd.Uid != m2.StatMd.Uid {
	// 	ics = append(ics, fmt.Sprintf("%s: uid %d vs %d", path, m1.StatMd.Uid, m2.StatMd.Uid))
	// }
	// if m1.StatMd.Gid != m2.StatMd.Gid {
	// 	ics = append(ics, fmt.Sprintf("%s: gid %d vs %d", path, m1.StatMd.Gid, m2.StatMd.Gid))
	// }
	/*
	 * mtime/size comparison is skipped for hmdfs: the remote-view stat
	 * (get_cached_attr_remote) returns the device_view inode's cached
	 * getattr_isize/i_mtime, which only refresh on open/own-write/inode
	 * rebuild - not on stat or cross-node write. The values are therefore
	 * intentionally inconsistent across nodes (weak consistency), same
	 * rationale as nlink/mode/uid-gid above. Content integrity is still
	 * covered by the Checksum comparison.
	 */
	if fsType != "hmdfs" && m1.StatMd.Mtim.Sec != m2.StatMd.Mtim.Sec {
		ics = append(ics, fmt.Sprintf("%s: mtime %d vs %d", path, m1.StatMd.Mtim.Sec, m2.StatMd.Mtim.Sec))
	}
	isDir := (m1.StatMd.Mode & syscall.S_IFDIR) != 0
	if fsType != "hmdfs" && !isDir && m1.StatMd.Size != m2.StatMd.Size {
		ics = append(ics, fmt.Sprintf("%s: size %d vs %d", path, m1.StatMd.Size, m2.StatMd.Size))
	}
	if !xattrCmp(m1.Xattr, m2.Xattr) {
		ics = append(ics, fmt.Sprintf("%s: xattr differ", path))
	}

	return ics
}

// initTreeSubset serializes the subset of the initial file tree touched by
// the test programs (plus all ancestor directories, up to and including the
// merge_view root) for symsc. The initial tree can contain thousands of
// files while a program only touches a few dozen, so passing the whole tree
// would blow up the emulation's per-call state copies.
func initTreeSubset(ps []*prog.Prog, ft *prog.FileTree) string {
	if ft == nil {
		return ""
	}
	seen := make(map[string]bool)
	var lines []string
	addPath := func(path string) {
		path = strings.TrimPrefix(path, "./")
		if node := ft.FindNode(path); node != nil {
			for n := node; n != nil; n = n.Parent {
				fp := n.FullPath
				if seen[fp] {
					continue
				}
				seen[fp] = true
				t := "dir"
				if n.Type == prog.NodeTypeFile {
					t = "file"
				}
				lines = append(lines, fmt.Sprintf("%s\t%s\t%d\t%d", fp, t, n.Size, len(n.Children)))
			}
		}
	}
	for _, p := range ps {
		for _, call := range p.Calls {
			for _, arg := range call.Args {
				switch a := arg.(type) {
				case *prog.PointerArg:
					if d, ok := a.Res.(*prog.DataArg); ok && d.Dir() != prog.DirOut &&
						strings.Contains(string(d.Data()), "merge_view") {
						addPath(string(d.Data()))
					}
				case *prog.DataArg:
					if a.Dir() != prog.DirOut && strings.Contains(string(a.Data()), "merge_view") {
						addPath(string(a.Data()))
					}
				}
			}
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
