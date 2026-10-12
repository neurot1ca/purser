package main

import "testing"

func TestCoderEffortRejectsBadValue(t *testing.T) {
	if code := coderEffortMain([]string{"ultra"}); code != 2 {
		t.Fatalf("bad effort exit code = %d, want 2", code)
	}
}
