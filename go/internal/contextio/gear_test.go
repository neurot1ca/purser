package contextio

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestGearStateValidationFailureAndConcurrentUpdates(t *testing.T) {
	root, key := t.TempDir(), "0123456789abcdef"
	if up, err := Gear(root, key); err != nil || up {
		t.Fatalf("missing gear state = %v, %v", up, err)
	}
	if err := SetGear(root, key, "up"); err != nil {
		t.Fatal(err)
	}
	if up, err := Gear(root, key); err != nil || !up {
		t.Fatalf("gear up = %v, %v", up, err)
	}
	path := filepath.Join(root, "projects", key, "gear.json")
	if err := os.WriteFile(path, []byte(`{"version":9,"gear":"up"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Gear(root, key); err == nil {
		t.Fatal("malformed gear state was accepted")
	}
	if err := SetGear(root, key, "down"); err != nil {
		t.Fatalf("malformed gear state blocked down: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("malformed gear state was not removed: %v", err)
	}
	failureRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(failureRoot, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SetGear(failureRoot, key, "up"); err == nil {
		t.Fatal("failed state write reported success")
	}

	concurrentRoot := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			state := "up"
			if i%2 == 0 {
				state = "down"
			}
			if err := SetGear(concurrentRoot, key, state); err != nil {
				t.Errorf("concurrent %s: %v", state, err)
			}
		}(i)
	}
	wg.Wait()
	if up, err := Gear(concurrentRoot, key); err != nil {
		t.Fatalf("final concurrent state is inconsistent: %v", err)
	} else if !up {
		if _, err := os.Stat(filepath.Join(concurrentRoot, "projects", key, "gear.json")); !os.IsNotExist(err) {
			t.Fatalf("down state did not remove the record: %v", err)
		}
	}
	if err := SetGear(concurrentRoot, key, "down"); err != nil {
		t.Fatal(err)
	}
	if up, err := Gear(concurrentRoot, key); err != nil || up {
		t.Fatalf("gear down did not restore normal routing: %v, %v", up, err)
	}
}

func TestCoderEffortStateSetAndAutoClear(t *testing.T) {
	root, key := t.TempDir(), "0123456789abcdef"
	if effort, err := CoderEffort(root, key); err != nil || effort != "" {
		t.Fatalf("missing effort = %q, %v", effort, err)
	}
	if err := SetCoderEffort(root, key, "xhigh"); err != nil {
		t.Fatal(err)
	}
	if effort, err := CoderEffort(root, key); err != nil || effort != "xhigh" {
		t.Fatalf("set effort = %q, %v", effort, err)
	}
	if err := SetCoderEffort(root, key, "auto"); err != nil {
		t.Fatal(err)
	}
	if effort, err := CoderEffort(root, key); err != nil || effort != "" {
		t.Fatalf("auto effort = %q, %v", effort, err)
	}
}
