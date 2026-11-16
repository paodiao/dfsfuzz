package ipc

import (
	"testing"

	"monarch/prog"
)

const glusterfsTestEventSize = 1228

func glusterfsU32(v uint32) []byte {
	b := make([]byte, 4)
	prog.HostEndian.PutUint32(b, v)
	return b
}

func glusterfsU64(v uint64) []byte {
	b := make([]byte, 8)
	prog.HostEndian.PutUint64(b, v)
	return b
}

func glusterfsTraceHeader(count uint32) []byte {
	var out []byte
	out = append(out, glusterfsU64(1_000_000_000)...) // ratio 1.0
	out = append(out, glusterfsU64(0x1234)...)        // tsc offset
	out = append(out, glusterfsU64(7)...)             // gap
	out = append(out, glusterfsU32(count)...)
	return out
}

type glusterfsEventSpec struct {
	timestamp uint64
	entryTS   uint64
	funcID    uint32
	ret       int32
	nodeID    uint64
	off       uint64
	size      uint64
	mode      uint32
	path      string
	path2     string
	xattr     string
	truncated uint32
}

func glusterfsEvent(t *testing.T, s glusterfsEventSpec) []byte {
	t.Helper()
	if len(s.path) > 512 || len(s.path2) > 512 || len(s.xattr) > 128 {
		t.Fatalf("test event string too long")
	}
	ev := make([]byte, glusterfsTestEventSize)
	copy(ev[0:8], glusterfsU64(s.timestamp))
	copy(ev[8:16], glusterfsU64(s.entryTS))
	copy(ev[16:20], glusterfsU32(s.funcID))
	copy(ev[20:24], glusterfsU32(uint32(s.ret)))
	copy(ev[24:32], glusterfsU64(s.nodeID))
	copy(ev[32:40], glusterfsU64(0)) // unique
	copy(ev[40:48], glusterfsU64(s.off))
	copy(ev[48:56], glusterfsU64(s.size))
	copy(ev[56:60], glusterfsU32(s.mode))
	copy(ev[60:64], glusterfsU32(uint32(len(s.path))))
	copy(ev[64:68], glusterfsU32(uint32(len(s.path2))))
	copy(ev[68:72], glusterfsU32(uint32(len(s.xattr))))
	copy(ev[72:76], glusterfsU32(s.truncated))
	copy(ev[76:76+len(s.path)], s.path)
	copy(ev[588:588+len(s.path2)], s.path2)
	copy(ev[1100:1100+len(s.xattr)], s.xattr)
	return ev
}

func TestParseGlusterfsTraceEventsFixedStride(t *testing.T) {
	e1 := glusterfsEvent(t, glusterfsEventSpec{
		timestamp: 100,
		entryTS:   90,
		funcID:    9,
		ret:       0,
		nodeID:    0x1111,
		off:       0,
		size:      0,
		mode:      2,
		path:      "q0",
	})
	e2 := glusterfsEvent(t, glusterfsEventSpec{
		timestamp: 200,
		entryTS:   180,
		funcID:    12,
		ret:       -2,
		nodeID:    0x2222,
		off:       12,
		size:      0,
		mode:      1,
		path:      "dir/a",
		path2:     "dir/b",
		xattr:     "user.foo",
		truncated: 1,
	})
	data := append(glusterfsTraceHeader(2), e1...)
	data = append(data, e2...)

	out := data
	events, ratio, tscOffset := parseGlusterfsTraceEvents(&out)
	if len(events) != 2 {
		t.Fatalf("parsed %d events, want 2", len(events))
	}
	if ratio != 1.0 {
		t.Fatalf("ratio = %v, want 1.0", ratio)
	}
	if tscOffset != 0x1234 {
		t.Fatalf("tscOffset = %#x, want 0x1234", tscOffset)
	}
	if len(out) != 0 {
		t.Fatalf("parser left %d bytes", len(out))
	}

	got := events[0]
	if got.Timestamp != 100 || got.EntryTs != 90 || got.FuncID != 9 ||
		got.Ret != 0 || got.NodeID != 0x1111 || got.Path != "q0" ||
		got.Mode != 2 || got.Ino != 0 || got.EntryIno != 0 {
		t.Fatalf("event0 = %+v", got)
	}
	got = events[1]
	if got.Timestamp != 200 || got.EntryTs != 180 || got.FuncID != 12 ||
		got.Ret != -2 || got.NodeID != 0x2222 || got.Off != 12 ||
		got.Size != 0 || got.Mode != 1 || got.Path != "dir/a" ||
		got.Path2 != "dir/b" || got.XattrName != "user.foo" ||
		got.Truncated != 1 || got.Ino != 0 || got.EntryIno != 0 {
		t.Fatalf("event1 = %+v", got)
	}
}

func TestParseGlusterfsTraceEventsShortRecord(t *testing.T) {
	e := glusterfsEvent(t, glusterfsEventSpec{funcID: 9, path: "q0"})
	data := append(glusterfsTraceHeader(1), e[:100]...)
	out := data
	events, _, _ := parseGlusterfsTraceEvents(&out)
	if len(events) != 0 {
		t.Fatalf("short record parsed %d events", len(events))
	}
}

func TestParseGlusterfsTraceEventsCountCap(t *testing.T) {
	e := glusterfsEvent(t, glusterfsEventSpec{funcID: 9, path: "q0"})
	data := append(glusterfsTraceHeader(10), e...)
	out := data
	events, _, _ := parseGlusterfsTraceEvents(&out)
	if len(events) != 1 {
		t.Fatalf("count cap parsed %d events, want 1", len(events))
	}
}

func TestParseGlusterfsTraceEventsBadLengthKeepsStreamAligned(t *testing.T) {
	e := glusterfsEvent(t, glusterfsEventSpec{funcID: 9, path: "q0"})
	copy(e[60:64], glusterfsU32(513)) // path_len > 512
	data := append(glusterfsTraceHeader(1), e...)
	data = append(data, []byte("fsmd")...)
	out := data
	events, _, _ := parseGlusterfsTraceEvents(&out)
	if len(events) != 0 {
		t.Fatalf("bad-length event parsed %d events", len(events))
	}
	if string(out) != "fsmd" {
		t.Fatalf("bad-length event left %q, want fsmd", out)
	}
}

func TestParseHmdfsTraceEventsUnchanged(t *testing.T) {
	var ev []byte
	ev = append(ev, glusterfsU64(123)...)
	ev = append(ev, glusterfsU32(9)...)
	ev = append(ev, glusterfsU32(0)...)
	ev = append(ev, glusterfsU64(0x42)...)
	for i := 0; i < 6; i++ {
		ev = append(ev, glusterfsU64(0)...)
	}
	ev = append(ev, glusterfsU64(7)...)  // off
	ev = append(ev, glusterfsU64(8)...)  // size
	ev = append(ev, glusterfsU64(0)...)  // entry ino
	ev = append(ev, glusterfsU32(1)...)  // mode
	ev = append(ev, glusterfsU64(99)...) // entry ts
	if len(ev) != 112 {
		t.Fatalf("HMDFS test event = %d bytes, want 112", len(ev))
	}
	data := append(glusterfsTraceHeader(1), ev...)
	out := data
	events, ratio, tscOffset := parseHmdfsTraceEvents(&out)
	if len(events) != 1 || ratio != 1.0 || tscOffset != 0x1234 {
		t.Fatalf("HMDFS parser: events=%d ratio=%v off=%#x", len(events), ratio, tscOffset)
	}
	got := events[0]
	if got.Timestamp != 123 || got.FuncID != 9 || got.Ino != 0x42 ||
		got.Off != 7 || got.Size != 8 || got.Mode != 1 || got.EntryTs != 99 {
		t.Fatalf("HMDFS event = %+v", got)
	}
	if got.Path != "" || got.NodeID != 0 || got.Truncated != 0 {
		t.Fatalf("HMDFS parser filled GlusterFS fields: %+v", got)
	}
}
