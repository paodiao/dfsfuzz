package main

import "syscall"

func statMd(ino uint64, isDir bool) syscall.Stat_t {
	mode := uint32(0x8000) // S_IFREG
	if isDir {
		mode = 0x4000 // S_IFDIR
	}
	return syscall.Stat_t{Ino: ino, Mode: mode, Size: 100}
}
