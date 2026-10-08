package retention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func putFile(t *testing.T, path string, data []byte, stamp int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	when := unixTime(stamp)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func unixTime(seconds int64) time.Time { return time.Unix(seconds, 0) }

func TestPluginCacheKeepsRunningAndNewestOtherVersion(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "marketplace", "purser")
	for _, version := range []string{"1.0.0", "1.2.0", "1.1.9"} {
		if err := os.MkdirAll(filepath.Join(cache, version), 0700); err != nil {
			t.Fatal(err)
		}
	}
	PrunePluginCache(filepath.Join(cache, "1.0.0", "bin", "purser.exe"))
	for _, version := range []string{"1.0.0", "1.2.0"} {
		if _, err := os.Stat(filepath.Join(cache, version)); err != nil {
			t.Errorf("version %s was removed: %v", version, err)
		}
	}
}

func TestPluginCacheRemovesOlderVersions(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "market", "purser")
	for _, version := range []string{"2.0.0", "1.9.9", "1.10.0"} {
		if err := os.MkdirAll(filepath.Join(cache, version), 0700); err != nil {
			t.Fatal(err)
		}
	}
	PrunePluginCache(filepath.Join(cache, "2.0.0", "bin", "purser.exe"))
	if _, err := os.Stat(filepath.Join(cache, "1.9.9")); !os.IsNotExist(err) {
		t.Fatalf("old version remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "1.10.0")); err != nil {
		t.Fatalf("newest rollback version was removed: %v", err)
	}
}

func TestPluginCacheIgnoresNonSemverAndLinks(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "market", "purser")
	for _, name := range []string{"current", "1.0", "1.0.0-beta", "2.0.0"} {
		if err := os.MkdirAll(filepath.Join(cache, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := t.TempDir()
	marker := filepath.Join(target, "keep")
	if err := os.WriteFile(marker, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cache, "3.0.0")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	PrunePluginCache(filepath.Join(cache, "1.0.0", "bin", "purser.exe"))
	for _, name := range []string{"current", "1.0", "1.0.0-beta", "3.0.0"} {
		if _, err := os.Lstat(filepath.Join(cache, name)); err != nil {
			t.Errorf("ignored entry %s changed: %v", name, err)
		}
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "safe" {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
}

func TestPluginCacheRemovesLeftoverPruneDirectory(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "market", "purser")
	if err := os.MkdirAll(filepath.Join(cache, "2.0.0"), 0700); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(cache, ".1.0.0.prune")
	putFile(t, filepath.Join(leftover, "hooks", "hook.js"), []byte("old"), 1000)

	PrunePluginCache(filepath.Join(cache, "2.0.0", "bin", "purser.exe"))
	if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
		t.Fatalf("leftover prune directory remains: %v", err)
	}
}

func TestPluginCacheRenameFailureLeavesVersionIntact(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "market", "purser")
	for _, version := range []string{"3.0.0", "2.0.0", "1.0.0"} {
		if err := os.MkdirAll(filepath.Join(cache, version), 0700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(cache, "1.0.0", "hooks", "hook.js")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("must survive"), 0600); err != nil {
		t.Fatal(err)
	}

	originalRename := renamePluginPath
	renamePluginPath = func(oldPath, newPath string) error {
		if oldPath == filepath.Join(cache, "1.0.0") {
			return os.ErrPermission
		}
		return originalRename(oldPath, newPath)
	}
	t.Cleanup(func() { renamePluginPath = originalRename })

	PrunePluginCache(filepath.Join(cache, "3.0.0", "bin", "purser.exe"))
	if data, err := os.ReadFile(marker); err != nil || string(data) != "must survive" {
		t.Fatalf("version contents changed after rename failure: %q %v", data, err)
	}
}

func TestPluginCacheSkipsNonPluginLayout(t *testing.T) {
	root := t.TempDir()
	for _, executable := range []string{
		filepath.Join(root, "dev", "purser.exe"),
		filepath.Join(root, "market", "not-purser", "1.0.0", "bin", "purser.exe"),
	} {
		PrunePluginCache(executable)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("non-plugin layout was modified: %v", err)
	}
}

func TestAgePrunesExpiredAllowlistedFiles(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "old.cache")
	recent := filepath.Join(root, "recent.cache")
	putFile(t, old, []byte("old"), 1000)
	putFile(t, recent, []byte("recent"), 2000)
	p := policy{maxAgeDays: 1, maxFiles: -1, maxBytes: -1, deleteGlobs: []string{"*.cache"}}
	if prune(root, p, 1000+86400+1) {
		t.Fatal("remaining files unexpectedly exceed policy")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired file remains: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent file was removed: %v", err)
	}
}

func TestSizePrunesOldestEligibleFilesFirst(t *testing.T) {
	root := t.TempDir()
	oldest := filepath.Join(root, "oldest.cache")
	newest := filepath.Join(root, "newest.cache")
	putFile(t, oldest, []byte("1234"), 1000)
	putFile(t, newest, []byte("5678"), 2000)
	p := policy{maxAgeDays: -1, maxFiles: -1, maxBytes: 4, deleteGlobs: []string{"*.cache"}}
	if prune(root, p, 3000) {
		t.Fatal("remaining files unexpectedly exceed byte limit")
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("oldest file remains: %v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest file was removed: %v", err)
	}
}

func TestDirectoryPoliciesKeepOwnerManagedAndDeleteOnlyCatalogCache(t *testing.T) {
	root := t.TempDir()
	putFile(t, filepath.Join(root, "preparations", "job", "old"), []byte("owned"), 1000)
	putFile(t, filepath.Join(root, "model-catalog", "catalog.json"), []byte("cache"), 1000)
	putFile(t, filepath.Join(root, "model-catalog", "valuable.json"), []byte("durable"), 1000)
	PruneState(root, 1000+31*86400, false)
	if _, err := os.Stat(filepath.Join(root, "preparations", "job", "old")); err != nil {
		t.Fatalf("owner-managed preparation changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "model-catalog", "catalog.json")); !os.IsNotExist(err) {
		t.Fatalf("expired allowlisted cache remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "model-catalog", "valuable.json")); err != nil {
		t.Fatalf("non-allowlisted file was removed: %v", err)
	}
}

func TestCeilingWarningWritesHealthOncePerDailyStamp(t *testing.T) {
	root := t.TempDir()
	previous := stateRootMaxBytes
	stateRootMaxBytes = 0
	t.Cleanup(func() { stateRootMaxBytes = previous })
	const now = float64(2_000_000)
	checkRootCeiling(root, now)
	checkRootCeiling(root, now+60)
	stamp := filepath.Join(root, CeilingStamp)
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("daily stamp missing: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "telemetry", "telemetry_health.json"))
	if err != nil {
		t.Fatalf("health warning missing: %v", err)
	}
	var health map[string]any
	if err = json.Unmarshal(data, &health); err != nil {
		t.Fatal(err)
	}
	if health["last_error_code"] != "STATE_RETENTION_LIMIT" || health["failure_count"] != float64(1) {
		t.Fatalf("warning = %#v", health)
	}
}

func TestRetirementDoesNotFollowPreparationSymlink(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	generation := filepath.Join(root, "preparations", id, "revision-1")
	putFile(t, filepath.Join(generation, "state.json"), []byte(`{"state":"PREPARED"}`), 1000)
	external := t.TempDir()
	keep := filepath.Join(external, "keep.txt")
	putFile(t, keep, []byte("outside"), 1000)
	link := filepath.Join(generation, "external")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	putFile(t, filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"), []byte(`{"jobId":"`+id+`","state":"SUCCEEDED","applyState":"applied"}`), 1000)
	putFile(t, filepath.Join(root, "async_jobs", id+".json"), []byte(`{"stage":"CONSUMED"}`), 1000)
	RetirePreparations(root, 2000, false)
	if _, err := os.Stat(generation); !os.IsNotExist(err) {
		t.Fatalf("completed generation remains: %v", err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "outside" {
		t.Fatalf("outside target changed: data=%q err=%v", data, err)
	}
}

func TestPreparationRetiresCompletedAndOldOrphanButKeepsProtectedStates(t *testing.T) {
	root := t.TempDir()
	completed := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	preparing := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	orphanOld := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	orphanYoung := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	completedPath := filepath.Join(root, "preparations", completed, "revision-1")
	preparingPath := filepath.Join(root, "preparations", preparing, "revision-1")
	orphanOldPath := filepath.Join(root, "preparations", orphanOld, "revision-1")
	orphanYoungPath := filepath.Join(root, "preparations", orphanYoung, "revision-1")
	for _, item := range []struct{ path, state string }{{completedPath, "PREPARED"}, {preparingPath, "PREPARING"}, {orphanOldPath, "PREPARED"}, {orphanYoungPath, "PREPARED"}} {
		putFile(t, filepath.Join(item.path, "state.json"), []byte(`{"state":"`+item.state+`"}`), 1000)
		if item.path == orphanOldPath {
			if err := os.Chtimes(item.path, unixTime(1000), unixTime(1000)); err != nil {
				t.Fatal(err)
			}
		}
	}
	putFile(t, filepath.Join(root, "codex_exec_mcp", "jobs", completed+".json"), []byte(`{"jobId":"`+completed+`","state":"SUCCEEDED","applyState":"applied"}`), 1000)
	putFile(t, filepath.Join(root, "async_jobs", completed+".json"), []byte(`{"stage":"CONSUMED"}`), 1000)
	RetirePreparations(root, 1000+maxPrepAge+1, false)
	for _, path := range []string{completedPath, orphanOldPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("eligible preparation remains %s: %v", path, err)
		}
	}
	for _, path := range []string{preparingPath, orphanYoungPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("protected preparation was removed %s: %v", path, err)
		}
	}
}

func TestPreparationKeepsRecentConflictAndRetiresOldTerminalConflict(t *testing.T) {
	root := t.TempDir()
	recent := "1111111111111111111111111111111111111111111111111111111111111111"
	old := "2222222222222222222222222222222222222222222222222222222222222222"
	recentPath := filepath.Join(root, "preparations", recent, "revision-1")
	oldPath := filepath.Join(root, "preparations", old, "revision-1")
	putFile(t, filepath.Join(recentPath, "state.json"), []byte(`{"state":"CONFLICT"}`), 2000)
	putFile(t, filepath.Join(oldPath, "state.json"), []byte(`{"state":"CONFLICT"}`), 1000)
	putFile(t, filepath.Join(root, "codex_exec_mcp", "jobs", recent+".json"), []byte(`{"jobId":"`+recent+`","state":"SUCCEEDED","finishedAt":2000}`), 2000)
	putFile(t, filepath.Join(root, "codex_exec_mcp", "jobs", old+".json"), []byte(`{"jobId":"`+old+`","state":"SUCCEEDED","finishedAt":1000}`), 1000)
	RetirePreparations(root, 1000+maxPrepAge+1, false)
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("recent conflict was removed: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old terminal conflict remains: %v", err)
	}
}

func TestPreparationByteWalkOnlyRunsWhenRequested(t *testing.T) {
	root := t.TempDir()
	id := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	path := filepath.Join(root, "preparations", id, "revision-1")
	putFile(t, filepath.Join(path, "state.json"), []byte(`{"state":"PREPARING"}`), 1000)
	previous := maxPrepBytes
	maxPrepBytes = 0
	t.Cleanup(func() { maxPrepBytes = previous })
	RetirePreparations(root, 2000, false)
	if _, err := os.Stat(filepath.Join(root, "telemetry", "telemetry_health.json")); !os.IsNotExist(err) {
		t.Fatalf("byte warning emitted without byte check: %v", err)
	}
	RetirePreparations(root, 2000, true)
	data, err := os.ReadFile(filepath.Join(root, "telemetry", "telemetry_health.json"))
	if err != nil {
		t.Fatalf("byte check did not warn: %v", err)
	}
	var health map[string]any
	if err = json.Unmarshal(data, &health); err != nil {
		t.Fatal(err)
	}
	if health["last_error_code"] != "STATE_RETENTION_LIMIT" {
		t.Fatalf("unexpected byte warning: %#v", health)
	}
}

func TestPreparationRetiresTimedOutUnappliedJob(t *testing.T) {
	root := t.TempDir()
	id := "3333333333333333333333333333333333333333333333333333333333333333"
	path := filepath.Join(root, "preparations", id, "revision-1")
	putFile(t, filepath.Join(path, "state.json"), []byte(`{"state":"PREPARED"}`), 1000)
	putFile(t, filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"), []byte(`{"jobId":"`+id+`","state":"TIMED_OUT","applyState":"unapplied","finishedAt":1000}`), 1000)
	putFile(t, filepath.Join(root, "async_jobs", id+".json"), []byte(`{"stage":"CONSUMED"}`), 1000)
	RetirePreparations(root, 1000+maxPrepAge+86400, false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("TIMED_OUT unapplied preparation remains: %v", err)
	}
}
