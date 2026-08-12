// Copyright 2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog_test

import (
	"testing"

	. "monarch/prog"
	_ "monarch/sys"
)

func TestChecksumCalcRandom(t *testing.T) {
	target, rs, iters := InitTest(t)
	ct := target.DefaultChoiceTable()
	for i := 0; i < iters; i++ {
		p, _ := target.Generate(rs, 10, ct, nil, false, nil, false, &Hmdfs_config{}, 0)
		for _, call := range p.Calls {
			CalcChecksumsCall(call)
		}
		p.Mutate(rs, 10, ct, nil, nil, 0, false, false, &Hmdfs_config{}, 0)
		for _, call := range p.Calls {
			CalcChecksumsCall(call)
		}
	}
}
