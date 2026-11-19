package main

import (
	"reflect"
	"testing"
)

func TestPlannerTscOffsetsClientView(t *testing.T) {
	full := []int64{100, 200, 300, 400, 500, 600}
	if got, want := plannerTscOffsets(full, 3), []int64{400, 500, 600}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planner offsets with 3 servers = %v, want %v", got, want)
	}
	if got := plannerTscOffsets(full, 0); !reflect.DeepEqual(got, full) {
		t.Fatalf("planner offsets without servers = %v, want full slice", got)
	}
	if got := plannerTscOffsets(full, len(full)); !reflect.DeepEqual(got, full) {
		t.Fatalf("planner offsets with an out-of-range server count = %v, want full slice", got)
	}
}
