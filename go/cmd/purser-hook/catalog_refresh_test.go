package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/testutil"
)

func saveCatalogRefreshHooks(t *testing.T) {
	t.Helper()
	oldDiscover, oldConfig := discoverCatalogModels, loadCatalogConfig
	t.Cleanup(func() {
		discoverCatalogModels, loadCatalogConfig = oldDiscover, oldConfig
	})
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("PURSER_TEST_DISABLE_CHILD_LAUNCH", "1")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func TestCatalogRefreshReplaysSuccessfulSnapshotAfterInventoryFailure(t *testing.T) {
	saveCatalogRefreshHooks(t)
	state, _ := filepath.Abs(stateRoot())
	today := time.Now().UTC().Format("2006-01-02")
	repo := t.TempDir()
	if output, err := testutil.Hide(exec.Command("git", "-C", repo, "init", "-q")).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	resolution := map[string]any{
		"opencode_snapshot_on": today,
		"agents":               map[string][]string{"opencode": {"opencode/big-pickle", "opencode/fast-free", "opencode/paid"}},
	}
	encoded, _ := json.Marshal(resolution)
	if err := os.WriteFile(filepath.Join(state, "model-resolution.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "opencode", "candidates.json"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	listCalls := 0
	discoverCatalogModels = func(string) (string, string, bool) { listCalls++; return "", "", false }
	loadCatalogConfig = func(string) (map[string]any, error) { return map[string]any{}, nil }
	if err := refreshCatalog(state, canonical); err == nil {
		t.Fatal("expected inventory write failure")
	}
	if err := os.Remove(filepath.Join(state, "opencode", "candidates.json")); err != nil {
		t.Fatal(err)
	}
	if err := refreshCatalog(state, canonical); err != nil {
		t.Fatalf("replay successful snapshot: %v", err)
	}
	if err := refreshCatalog(state, canonical); err != nil {
		t.Fatalf("coalesced refresh: %v", err)
	}
	if listCalls != 3 {
		t.Fatalf("discover calls=%d, want every refresh to discover", listCalls)
	}
	data, err := os.ReadFile(filepath.Join(state, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Baseline string `json:"catalog_baseline"`
		Date     string `json:"catalog_date"`
		Items    []struct {
			ID string `json:"id"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, item := range inventory.Items {
		ids = append(ids, item.ID)
	}
	if inventory.Date != today || inventory.Baseline != inventory.Date || !reflect.DeepEqual(ids, []string{"opencode/big-pickle", "opencode/fast-free"}) {
		t.Fatalf("inventory date=%q baseline=%q ids=%q", inventory.Date, inventory.Baseline, ids)
	}
}

func TestCatalogRefreshFailureLeavesDiagnostic(t *testing.T) {
	saveCatalogRefreshHooks(t)
	state := stateRoot()
	if code := catalogRefreshFailure(state, errors.New("test child failure")); code != 1 {
		t.Fatalf("exit code=%d, want 1", code)
	}
	data, err := os.ReadFile(filepath.Join(state, "catalog-refresh-error.json"))
	if err != nil || !strings.Contains(string(data), "test child failure") {
		t.Fatalf("diagnostic=%q err=%v", data, err)
	}
}

func TestCatalogRefreshFreeModelRule(t *testing.T) {
	got := freeOpenCodeModels([]string{"opencode/big-pickle", "opencode/model-free", "vendor/model-free", "opencode/model", "opencode/-free"})
	want := []string{"opencode/big-pickle", "opencode/model-free", "opencode/-free"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("free models=%q, want %q", got, want)
	}
}

func TestCatalogRefreshSecondRepositoryStillDiscoversOnSameDay(t *testing.T) {
	saveCatalogRefreshHooks(t)
	today := time.Now().UTC().Format("2006-01-02")
	state := stateRoot()
	if err := os.MkdirAll(filepath.Join(state, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{
		"opencode_snapshot_on": today,
		"agents":               map[string][]string{"opencode": {"opencode/big-pickle"}},
	})
	if err := os.WriteFile(filepath.Join(state, "model-resolution.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "opencode", "candidates.json"), []byte(`{"catalog_date":"`+today+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	discoverCatalogModels = func(repository string) (string, string, bool) {
		calls = append(calls, repository)
		return "", "", false
	}
	loadCatalogConfig = func(string) (map[string]any, error) { return map[string]any{}, nil }
	first, second := filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "two")
	for _, repository := range []string{first, second} {
		if err := os.MkdirAll(repository, 0700); err != nil {
			t.Fatal(err)
		}
		if err := refreshCatalog(state, repository); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 2 || calls[0] != first || calls[1] != second {
		t.Fatalf("discover calls=%q", calls)
	}
}

func TestCatalogRefreshHeldLockExitsPromptly(t *testing.T) {
	saveCatalogRefreshHooks(t)
	state := stateRoot()
	repository := t.TempDir()
	if output, err := testutil.Hide(exec.Command("git", "-C", repository, "init", "-q")).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	lockPath := filepath.Join(state, "catalog-refresh.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := contextio.TryLock(lock); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code := catalogRefreshMain([]string{"--state-root", state, "--repo", repository})
	if code != 0 {
		t.Fatalf("exit code=%d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("held lock waited %s", elapsed)
	}
}
