package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupMaintenanceCompletionBoundaries(t *testing.T) {
	old := requestCleanup
	t.Cleanup(func() { requestCleanup = old })
	scratch := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(scratch, "cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	called := false
	requestCleanup = func(got string) error {
		called = true
		if got != root {
			t.Fatalf("root=%q", got)
		}
		return errors.New("launch failed")
	}
	result, err := dispatchOperation(context.Background(), "codex_cancel", map[string]any{"jobId": "missing"}, root)
	if !called || result == nil || err != nil {
		t.Fatalf("completion/failure changed result: called=%v result=%v err=%v", called, result, err)
	}
	called = false
	if _, err := dispatchOperation(context.Background(), "codex_start", map[string]any{"prompt": "x"}, root); err == nil {
		// codex_start may fail because the fixture has no configured agent; cleanup
		// scheduling is still the assertion below.
	}
	if called {
		t.Fatal("non-final codex_start launched cleanup")
	}
}
