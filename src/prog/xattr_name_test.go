// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestXattrNameArgIsUnion pins the shape of the getxattr/setxattr name
// argument: xattr_name is a varlen union, so the argument must be a
// PointerArg to a UnionArg, not a DataArg carrying the union type.
func TestXattrNameArgIsUnion(t *testing.T) {
	target := InitTargetTest(t, "linux", "amd64")
	if target.SyscallMap["setxattr"] == nil {
		t.Skip("setxattr is not available")
	}
	r := newRand(target, rand.NewSource(1))
	s := newState(target, nil, nil)
	c := r.generateSetxattrCallFull(s, nil, "/tmp/eifuzz", "user.eifuzz", "bar", 0)
	if c == nil {
		t.Fatal("generateSetxattrCallFull returned nil")
	}
	ptr, ok := c.Args[1].(*PointerArg)
	if !ok {
		t.Fatalf("name arg is %T, want *PointerArg", c.Args[1])
	}
	union, ok := ptr.Res.(*UnionArg)
	if !ok {
		t.Fatalf("name pointee is %T, want *UnionArg", ptr.Res)
	}
	if union.Index != 0 {
		t.Fatalf("xattr_name index = %d, want 0 (known)", union.Index)
	}
	data, ok := union.Option.(*DataArg)
	if !ok {
		t.Fatalf("xattr_name option is %T, want *DataArg", union.Option)
	}
	if got := string(bytes.TrimSuffix(data.Data(), []byte{0})); got != "user.eifuzz" {
		t.Fatalf("xattr name = %q, want %q", got, "user.eifuzz")
	}
	p := &Prog{Target: target, Calls: []*Call{c}}
	if err := p.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if out := p.Serialize(); len(out) == 0 {
		t.Fatal("serialize returned no data")
	}
}
