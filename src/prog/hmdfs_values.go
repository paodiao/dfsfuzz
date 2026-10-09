package prog

import "strings"

// Centralized hmdfs value model shared by seed generation and mutation: length,
// offset (bucket-first, aligned with offsetBucketOf), mode/flags, and the fd
// capability model used by fd reuse. Probabilities are out of 100 per layer and
// mirrored in the plan doc (seed-optimization-plan.md, section 2).

const (
	lenBoundaryCap  = uint64(65536)
	truncateHardCap = uint64(1 << 20)
)

var (
	// randHmdfsLen layers: size-relative / page-boundary / small / uniform / big.
	hmdfsLenProbs = [5]int{35, 25, 15, 23, 2}
	// truncate layers: size-relative / page-boundary / uniform-2size / small.
	hmdfsTruncProbs = [4]int{30, 20, 40, 10}
	// offset layers: zero / mid / tail / beyond / free.
	hmdfsOffBucketProbs = [5]int{15, 35, 25, 20, 5}
	// pickMutationLength layers: size-relative / page-boundary / ranges / free.
	hmdfsMutLenProbs = [4]int{25, 40, 25, 10}

	// OffsetDifferent three-way mix (percent) and the bucket re-roll cap.
	diffOffsetOverlapProb = 40
	diffOffsetBucketBias  = 40
	diffOffsetFreeProb    = 20
	diffOffsetRetryCap    = 1

	hmdfsPageLens = []uint64{4095, 4096, 4097, 8191, 8192, 512, 1024}

	chmodModesCommon   = []uint64{0o644, 0o755, 0o777, 0o600, 0o640}
	chmodModesBoundary = []uint64{0, 0o001, 0o4755, 0o1777, 0o7777}
	chmodModeProbs     = [3]int{60, 25, 15}
	fileModeProbs      = [3]int{85, 10, 5}
)

// rollByProbs maps a uniform roll over [0,100) onto probs (whose sum is 100).
func (r *randGen) rollByProbs(probs []int) int {
	roll := r.Intn(100)
	acc := 0
	for i, p := range probs {
		acc += p
		if roll < acc {
			return i
		}
	}
	return len(probs) - 1
}

func clampU64(v, lo, hi uint64) uint64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// tailBeginOf returns the first offset of the file's last partial page.
func tailBeginOf(size uint64) uint64 {
	if size == 0 {
		return 0
	}
	return size - size%writebackBlocksize
}

// bucketOfOffset mirrors offsetBucketOf for a raw (size, offset) pair.
func bucketOfOffset(size, offset uint64) uint64 {
	if offset == 0 {
		return offsetBucketZero
	}
	if offset >= size {
		return offsetBucketBeyond
	}
	if tail := tailBeginOf(size); tail < size && offset >= tail {
		return offsetBucketTail
	}
	return offsetBucketMid
}

// randHmdfsLen picks an operation length in [minLen, maxLen] from the layered
// size-relative / page-boundary / small / uniform / big distribution.
func (r *randGen) randHmdfsLen(size, minLen, maxLen uint64) uint64 {
	if maxLen == 0 {
		maxLen = 8192
	}
	if maxLen < minLen {
		maxLen = minLen
	}
	var v uint64
	switch r.rollByProbs(hmdfsLenProbs[:]) {
	case 0: // size-relative boundaries
		if size == 0 {
			v = r.randRange(minLen, maxLen)
		} else {
			cands := [...]uint64{size - 1, size, size + 1, size / 2, 2 * size, size + writebackBlocksize}
			v = cands[r.Intn(len(cands))]
			if v > lenBoundaryCap {
				v = lenBoundaryCap
			}
		}
	case 1: // page boundaries
		v = hmdfsPageLens[r.Intn(len(hmdfsPageLens))]
	case 2: // small values
		hi := maxLen
		if hi > 1024 {
			hi = 1024
		}
		v = r.randRange(1, hi)
	case 3: // uniform within the file-size window
		hi := maxLen
		if size > 0 && size+writebackBlocksize < hi {
			hi = size + writebackBlocksize
		}
		v = r.randRange(1, hi)
	default: // big random
		v = r.randRange(1, maxLen)
	}
	return clampU64(v, minLen, maxLen)
}

// randHmdfsTruncLen picks a truncate()/ftruncate() length.
func (r *randGen) randHmdfsTruncLen(size uint64) uint64 {
	var v uint64
	switch r.rollByProbs(hmdfsTruncProbs[:]) {
	case 0: // size-relative
		if size == 0 {
			v = 0
		} else {
			cands := [...]uint64{size - 1, size, size + 1, 2 * size}
			v = cands[r.Intn(len(cands))]
		}
	case 1: // page boundaries
		v = []uint64{0, 4096, 8192}[r.Intn(3)]
	case 2: // uniform over the expansion window
		if size == 0 {
			v = r.randRange(0, 8191)
		} else {
			v = r.randRange(0, 2*size)
		}
	default: // small uniform
		v = r.randRange(0, 8191)
	}
	if v > truncateHardCap {
		v = truncateHardCap
	}
	return v
}

// randOffsetBucket picks one of the zero/mid/tail/beyond buckets (free is
// represented by offsetBucketNA so randOffsetInBucket's default arm handles
// it). Buckets that cannot exist for the file size have their weight
// redistributed: an empty mid (tailBegin<2) folds into tail (or zero), a
// missing tail (page-multiple or size<2) folds into mid (or zero).
func (r *randGen) randOffsetBucket(size uint64) uint64 {
	if size == 0 {
		if r.Intn(100) < 40 {
			return offsetBucketZero
		}
		return offsetBucketBeyond
	}
	tailBegin := tailBeginOf(size)
	midOK := tailBegin >= 2                             // mid = [1, tailBegin-1]
	tailOK := size >= 2 && size%writebackBlocksize != 0 // tail = [max(tailBegin,1), size-1]
	wZero, wMid, wTail, wBeyond, wFree := hmdfsOffBucketProbs[0], hmdfsOffBucketProbs[1],
		hmdfsOffBucketProbs[2], hmdfsOffBucketProbs[3], hmdfsOffBucketProbs[4]
	if !midOK {
		if tailOK {
			wTail += wMid
		} else {
			wZero += wMid
		}
		wMid = 0
	}
	if !tailOK {
		if midOK {
			wMid += wTail
		} else {
			wZero += wTail
		}
		wTail = 0
	}
	roll := r.Intn(wZero + wMid + wTail + wBeyond + wFree)
	acc := 0
	for _, wb := range [][2]int{
		{offsetBucketZero, wZero},
		{offsetBucketMid, wMid},
		{offsetBucketTail, wTail},
		{offsetBucketBeyond, wBeyond},
		{offsetBucketNA, wFree},
	} {
		acc += wb[1]
		if roll < acc {
			return uint64(wb[0])
		}
	}
	return offsetBucketMid // defensive
}

// randOffsetInBucket samples an offset inside the given bucket, embedding the
// domain boundary values (last byte, partial-page start, EOF, page multiples).
// Degenerate sizes may return a value outside the requested bucket (e.g. no
// mid region, a full last page, or size 0); callers must verify the result
// with bucketOfOffset and treat a mismatch as unrealizable.
func (r *randGen) randOffsetInBucket(size, bucket uint64) uint64 {
	switch bucket {
	case offsetBucketZero:
		return 0
	case offsetBucketMid:
		hi := uint64(0)
		if size > 0 {
			if tail := tailBeginOf(size); tail > 0 {
				hi = tail - 1
			}
		}
		if hi == 0 {
			// Empty mid (size too small); randOffsetBucket folds this bucket's
			// weight elsewhere, so this is only reachable via direct calls.
			return 0
		}
		if r.bin() {
			if pages := hi / writebackBlocksize; pages > 0 {
				k := uint64(r.Intn(int(pages))) + 1
				off := k * writebackBlocksize
				if off > 1 && off-1 <= hi && r.bin() {
					return off - 1
				}
				return off
			}
		}
		return r.randRange(1, hi)
	case offsetBucketTail:
		lo := tailBeginOf(size)
		if lo < 1 {
			lo = 1
		}
		if size < 2 {
			// No tail page: size 0 has no offsets at all and size 1 has only
			// offset 0, which belongs to the Zero bucket. Callers verify with
			// bucketOfOffset, so this is a degenerate sentinel, not a tail.
			return 0
		}
		if lo > size-1 {
			// The last page is full: only the final offset exists, which
			// classifies as Mid.
			return size - 1
		}
		hi := size - 1
		switch {
		case r.oneOf(2):
			return hi // last byte
		case r.bin():
			return lo // last partial page start
		}
		return r.randRange(lo, hi)
	case offsetBucketBeyond:
		switch {
		case r.bin():
			// Exactly EOF. Direct calls with size==0 return 0 (which is the
			// zero bucket); randHmdfsOffset special-cases size==0 before any
			// bucket sampling, so this is only a defensive edge.
			return size // exactly EOF
		case r.oneOf(2):
			return size + writebackBlocksize
		}
		return r.randRange(size, size+lenBoundaryCap)
	default: // free
		return r.randRange(0, size+lenBoundaryCap)
	}
}

// randHmdfsOffset picks an offset by bucket-first sampling.
func (r *randGen) randHmdfsOffset(size uint64) uint64 {
	if size == 0 {
		if r.Intn(100) < 40 {
			return 0
		}
		return r.randRange(1, 4096)
	}
	return r.randOffsetInBucket(size, r.randOffsetBucket(size))
}

// randHmdfsOffsetExcluding avoids excludeBucket for up to diffOffsetRetryCap
// re-rolls (used by the OffsetDifferent bucket-dedup arm).
func (r *randGen) randHmdfsOffsetExcluding(size, exclude uint64) uint64 {
	if size == 0 {
		return r.randHmdfsOffset(size)
	}
	for attempt := 0; ; attempt++ {
		off := r.randHmdfsOffset(size)
		if bucketOfOffset(size, off) != exclude || attempt >= diffOffsetRetryCap {
			return off
		}
	}
}

// pickHmdfsLenAt picks a length for a call at the given offset, keeping the
// offset bucket semantics (tail crosses EOF, mid stays inside the file).
func (r *randGen) pickHmdfsLenAt(size, offset, maxLen uint64) uint64 {
	length := r.randHmdfsLen(size, 1, maxLen)
	return clampLenToBucket(size, offset, length, bucketOfOffset(size, offset))
}

// randHmdfsOffsetDifferent implements the OffsetDifferent three-way mix around
// the group anchor: partial overlap / different bucket / free.
func (r *randGen) randHmdfsOffsetDifferent(size, anchor, refLen uint64) uint64 {
	biasEnd := diffOffsetOverlapProb + diffOffsetBucketBias
	freeEnd := biasEnd + diffOffsetFreeProb // == 100 by design
	roll := r.Intn(100)
	switch {
	case roll < diffOffsetOverlapProb:
		maxDelta := refLen
		if maxDelta == 0 {
			maxDelta = 1
		}
		delta := uint64(r.Intn(int(maxDelta))) + 1
		if r.bin() && anchor >= delta {
			return anchor - delta
		}
		return anchor + delta
	case roll < biasEnd:
		if size > 0 {
			return r.randHmdfsOffsetExcluding(size, bucketOfOffset(size, anchor))
		}
		return r.randHmdfsOffset(size)
	case roll < freeEnd:
		return r.randHmdfsOffset(size)
	default:
		// Defensive: the configured weights should sum to 100.
		return r.randHmdfsOffset(size)
	}
}

// clampLenToBucket keeps (offset, length) consistent with the bucket semantics:
// tail writes cross EOF, mid writes stay inside the file.
func clampLenToBucket(size, offset, length, bucket uint64) uint64 {
	if length == 0 {
		length = 1
	}
	if size == 0 || offset >= size {
		return length
	}
	switch bucket {
	case offsetBucketTail:
		if need := size - offset + 1; length < need {
			length = need
		}
	case offsetBucketMid:
		if maxLen := size - offset; length > maxLen {
			length = maxLen
		}
	}
	if length == 0 {
		length = 1
	}
	return length
}

// randChmodMode picks a chmod mode: common / boundary / uniform 1..0777.
func (r *randGen) randChmodMode() uint64 {
	switch r.rollByProbs(chmodModeProbs[:]) {
	case 0:
		return chmodModesCommon[r.Intn(len(chmodModesCommon))]
	case 1:
		return chmodModesBoundary[r.Intn(len(chmodModesBoundary))]
	default:
		return uint64(r.Intn(0o777) + 1)
	}
}

// randFileMode picks a mkdir/creat mode: common / boundary / uniform.
func (r *randGen) randFileMode() uint64 {
	switch r.rollByProbs(fileModeProbs[:]) {
	case 0:
		if r.bin() {
			return 0o755
		}
		return 0o666
	case 1:
		return []uint64{0, 0o777, 0o7777}[r.Intn(3)]
	default:
		return uint64(r.Intn(0o777) + 1)
	}
}

// randGetdentsBufSize picks a getdents64 buffer size: page boundaries 60%,
// uniform 1..8192 otherwise.
func (r *randGen) randGetdentsBufSize() uint64 {
	if r.Intn(100) < 60 {
		page := []uint64{4095, 4096, 4097, 8192}
		return page[r.Intn(len(page))]
	}
	return r.randRange(1, 8192)
}

// randOpenFlags picks open flags matching the capability the fd must have.
// VFS short-circuiting combinations (bad flags, O_EXCL, ...) are deliberately
// not produced: they never reach the traced hmdfs functions.
func (r *randGen) randOpenFlags(target *Target, want uint8, isDir bool) uint64 {
	if isDir && target != nil {
		return target.GetConst("O_DIRECTORY")
	}
	// O_RDONLY is 0 everywhere; O_RDWR/O_CREAT come from the target when
	// available (linux/amd64 fallback 2/64).
	rdwr, creat := uint64(2), uint64(64)
	if target != nil {
		if v := target.GetConst("O_RDWR"); v != 0 {
			rdwr = v
		}
		if v := target.GetConst("O_CREAT"); v != 0 {
			creat = v
		}
	}
	switch {
	case want&fdCapWrite != 0 && want&fdCapRead == 0:
		if r.Intn(100) < 85 {
			return rdwr
		}
		return rdwr | creat
	case want&fdCapRead != 0 && want&fdCapWrite == 0:
		if r.Intn(100) < 60 {
			return 0 // O_RDONLY
		}
		return rdwr
	default:
		if r.Intn(100) < 40 {
			return 0
		}
		return rdwr
	}
}

// fd capability bits.
const (
	fdCapRead uint8 = 1 << iota
	fdCapWrite
	fdCapDir
)

// fdCapOf decodes the capability of the fd produced by an open/creat call.
func fdCapOf(target *Target, call *Call) uint8 {
	name := call.Meta.Name
	if strings.Contains(name, "creat") {
		return fdCapWrite
	}
	if !strings.Contains(name, "open") {
		return 0
	}
	var flags uint64
	if len(call.Args) >= 2 {
		if a, ok := call.Args[1].(*ConstArg); ok {
			flags = a.Val
		}
	}
	if target != nil {
		if od := target.GetConst("O_DIRECTORY"); od != 0 && flags&od != 0 {
			return fdCapDir
		}
	}
	mask := uint64(3) // O_ACCMODE
	if target != nil {
		if v := target.GetConst("O_ACCMODE"); v != 0 {
			mask = v
		}
	}
	switch flags & mask {
	case 1:
		return fdCapWrite // O_WRONLY
	case 2:
		return fdCapRead | fdCapWrite // O_RDWR
	default:
		return fdCapRead // O_RDONLY == 0
	}
}

// capNeeded maps an fd-requiring call name to the capability its fd must have.
// fsync/fdatasync accept any regular-file fd, so they need no capability bit
// of their own (a read-only fd reaches hmdfs_fsync_local, see A.4); the
// file/dir class restriction comes from the caller's useFd/fdSet.
func capNeeded(callName string) uint8 {
	switch {
	case strings.Contains(callName, "getdents"):
		return fdCapDir
	case strings.Contains(callName, "fsync") || strings.Contains(callName, "fdatasync"):
		return 0
	case strings.Contains(callName, "read"):
		return fdCapRead
	case strings.Contains(callName, "write"):
		return fdCapWrite
	}
	return 0
}
