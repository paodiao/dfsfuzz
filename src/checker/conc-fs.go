package checker

import (
	"fmt"
	"path/filepath"
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
	fsType string, cfg_mode string, initIP string, testdirIno uint64) (bool, []string) {

	log.Logf(0, "ConcFSCheck fsMds:%v", fsMds)

	log.Logf(0, "testdirIno: %x", testdirIno)

	// Cross-check states from multiple client nodes
	allIcs := make([]string, 0)
	for i := srvNum; i < len(fsMds)-1; i++ {
		ics := MdCmp(fsMds[i], fsMds[i+1])
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

	cmd := exec.Command("python2.7",
		filepath.Join(exePath, "../../checker/symsc/monarch_emul.py"),
		"-v", "-t", fsType, "-p", symscProgStr,
		"-i", string(checkInfos_json), "-c", symsc_stat,
		"-g", string(seq_programs_json), "-s", fmt.Sprintf("%v", srvNum),
		"-f", cfg_mode, "-a", initIP, "-n", fmt.Sprintf("%v", testdirIno))

	log.Logf(0, "python2.7 %v -v -t %v -p \"%v\" -i \"%v\" -c \"%v\" -g \"%v\" -s %v -f \"%v\" -a \"%v\" -n %v",
		filepath.Join(exePath, "../../checker/symsc/monarch_emul.py"),
		fsType, strings.Replace(symscProgStr, "\"", "\\\"", -1), strings.Replace(string(checkInfos_json), "\"", "\\\"", -1),
		symsc_stat, string(seq_programs_json),
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
				strings.Contains(call.Meta.Name, "$")  {
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
	fsMd2 map[string]prog.FileMetadata) []string {
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
		ics = append(ics, compareFileMeta(filepath, md1, md2)...)
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

func compareFileMeta(path string, m1 prog.FileMetadata, m2 prog.FileMetadata) []string {
	var ics []string

	if m1.Checksum != m2.Checksum {
		ics = append(ics, fmt.Sprintf("%s: checksum %d vs %d", path, m1.Checksum, m2.Checksum))
	}
	if m1.StatMd.Mode != m2.StatMd.Mode {
		ics = append(ics, fmt.Sprintf("%s: mode %o vs %o", path, m1.StatMd.Mode, m2.StatMd.Mode))
	}
	if m1.StatMd.Uid != m2.StatMd.Uid {
		ics = append(ics, fmt.Sprintf("%s: uid %d vs %d", path, m1.StatMd.Uid, m2.StatMd.Uid))
	}
	if m1.StatMd.Gid != m2.StatMd.Gid {
		ics = append(ics, fmt.Sprintf("%s: gid %d vs %d", path, m1.StatMd.Gid, m2.StatMd.Gid))
	}
	if m1.StatMd.Mtim.Sec != m2.StatMd.Mtim.Sec {
		ics = append(ics, fmt.Sprintf("%s: mtime %d vs %d", path, m1.StatMd.Mtim.Sec, m2.StatMd.Mtim.Sec))
	}
	isDir := (m1.StatMd.Mode & syscall.S_IFDIR) != 0
	if !isDir && m1.StatMd.Size != m2.StatMd.Size {
		ics = append(ics, fmt.Sprintf("%s: size %d vs %d", path, m1.StatMd.Size, m2.StatMd.Size))
	}
	if !xattrCmp(m1.Xattr, m2.Xattr) {
		ics = append(ics, fmt.Sprintf("%s: xattr differ", path))
	}

	return ics
}
