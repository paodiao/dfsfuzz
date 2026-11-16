package checker

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultGlusterfsCSANLog = "/home/user/dfsfuzz/glusterfs-csan.log"
	glusterfsCSANMaxSamples = 32
)

var (
	glusterfsCSANMu        sync.Mutex
	glusterfsCSANFile      *os.File
	glusterfsCSANOpenError bool
	glusterfsCSANCheckSeq  uint64
)

func glusterfsCSANLogPath() string {
	if path := os.Getenv("GLUSTERFS_CSAN_LOG"); path != "" {
		return path
	}
	return defaultGlusterfsCSANLog
}

func glusterfsCSANLogf(format string, args ...interface{}) {
	line := fmt.Sprintf("%s pid=%d %s\n",
		time.Now().Format("2006/01/02 15:04:05"), os.Getpid(),
		fmt.Sprintf(format, args...))

	glusterfsCSANMu.Lock()
	defer glusterfsCSANMu.Unlock()
	if glusterfsCSANFile == nil {
		if glusterfsCSANOpenError {
			return
		}
		f, err := os.OpenFile(glusterfsCSANLogPath(),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			glusterfsCSANOpenError = true
			return
		}
		glusterfsCSANFile = f
	}
	_, _ = glusterfsCSANFile.WriteString(line)
}

// glusterfsCSANDiag accumulates B-class differences for one ConcFSCheck.
// Samples are capped; the counters are always complete.
type glusterfsCSANDiag struct {
	check        uint64
	nlink        uint64
	mtime        uint64
	nlinkSamples []string
	mtimeSamples []string
}

func newGlusterfsCSANDiag() *glusterfsCSANDiag {
	return &glusterfsCSANDiag{
		check: atomic.AddUint64(&glusterfsCSANCheckSeq, 1),
	}
}

func (d *glusterfsCSANDiag) recordNlink(path string, a, b uint64) {
	d.nlink++
	if len(d.nlinkSamples) < glusterfsCSANMaxSamples {
		d.nlinkSamples = append(d.nlinkSamples,
			fmt.Sprintf("path=%s a=%d b=%d", path, a, b))
	}
}

func (d *glusterfsCSANDiag) recordMtime(path string, a, b int64) {
	d.mtime++
	if len(d.mtimeSamples) < glusterfsCSANMaxSamples {
		d.mtimeSamples = append(d.mtimeSamples,
			fmt.Sprintf("path=%s a=%d b=%d", path, a, b))
	}
}

func (d *glusterfsCSANDiag) flush() {
	if d == nil || (d.nlink == 0 && d.mtime == 0) {
		return
	}
	glusterfsCSANLogf("check=%d summary nlink=%d mtime=%d",
		d.check, d.nlink, d.mtime)
	for _, sample := range d.nlinkSamples {
		glusterfsCSANLogf("check=%d kind=nlink %s", d.check, sample)
	}
	for _, sample := range d.mtimeSamples {
		glusterfsCSANLogf("check=%d kind=mtime %s", d.check, sample)
	}
}
