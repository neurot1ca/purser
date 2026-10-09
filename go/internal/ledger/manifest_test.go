package ledger

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestScopedManifestPythonFixture(t *testing.T) {
	root := initGitRepository(t)
	manifestFixtureRepository(t, root)
	statuses, err := statusEntries(root, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := trackedSnapshots(root, statuses, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := scopedManifest(root, []string{"scope"}, time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]json.RawMessage
	if err = json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	assertManifestJSON(t, expected["status"], statuses)
	assertManifestJSON(t, expected["tracked_snapshots"], snapshots)
	assertManifestJSON(t, expected["manifest"], manifest)
	changed := filepath.Join(root, "scope", "untracked.txt")
	before := snapshots["scope/untracked.txt"]["worktree"].(map[string]any)["mode"]
	changedMode := os.FileMode(0644)
	if runtime.GOOS == "windows" {
		changedMode = 0444
	}
	if err := os.Chmod(changed, changedMode); err != nil {
		t.Fatal(err)
	}
	changedStatuses, err := statusEntries(root, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	changedSnapshots, err := trackedSnapshots(root, changedStatuses, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := changedSnapshots["scope/untracked.txt"]["worktree"].(map[string]any)["mode"]
	if reflect.DeepEqual(after, before) {
		t.Fatal("permission change disappeared from the baseline snapshot")
	}
}

func TestScopedManifestBudgetFailures(t *testing.T) {
	root := initGitRepository(t)
	manifestFixtureRepository(t, root)
	for _, budget := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		_, err := scopedManifest(root, []string{"scope"}, time.Second, budget)
		failure, ok := err.(*inspectionError)
		if !ok || failure.Code != "INVALID_MANIFEST_BUDGET" {
			t.Fatalf("budget %v: %v", budget, err)
		}
	}
	oldClock := manifestClock
	base := time.Now()
	calls := 0
	manifestClock = func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * 150 * time.Millisecond)
	}
	t.Cleanup(func() { manifestClock = oldClock })
	manifest, err := scopedManifest(root, []string{"scope"}, time.Second, 1)
	failure, ok := err.(*inspectionError)
	if !ok || failure.Code != "MANIFEST_BUDGET_EXCEEDED" || manifest != nil {
		t.Fatalf("expired manifest = %#v, %v", manifest, err)
	}
}

func TestScopedManifestFilesystemFailureIsNotPartialEvidence(t *testing.T) {
	root := initGitRepository(t)
	manifestFixtureRepository(t, root)
	oldClock := manifestClock
	base := time.Now()
	calls := 0
	manifestClock = func() time.Time {
		calls++
		if calls == 3 {
			if err := os.Remove(filepath.Join(root, "scope", "untracked.txt")); err != nil {
				t.Fatalf("remove listed file: %v", err)
			}
		}
		return base.Add(time.Duration(calls) * 10 * time.Millisecond)
	}
	t.Cleanup(func() { manifestClock = oldClock })
	manifest, err := scopedManifest(root, []string{"scope"}, time.Second, 3)
	failure, ok := err.(*inspectionError)
	if !ok || failure.Code != "MANIFEST_IO_FAILED" || manifest != nil {
		t.Fatalf("filesystem failure = %#v, %v", manifest, err)
	}
}

func TestFilesystemFingerprintKinds(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("abc\n"), 0640); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := filesystemFingerprint(file)
	wantMode := 0640
	if runtime.GOOS == "windows" {
		wantMode = 0666
	}
	if err != nil || fingerprint["kind"] != "file" || fingerprint["mode"] != wantMode {
		t.Fatalf("file fingerprint = %#v, %v", fingerprint, err)
	}
	missing, err := filesystemFingerprint(filepath.Join(root, "missing"))
	if err != nil || !reflect.DeepEqual(missing, map[string]any{"kind": "missing"}) {
		t.Fatalf("missing fingerprint = %#v, %v", missing, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fingerprint, err = filesystemFingerprint(link)
	if err != nil || fingerprint["kind"] != "symlink" {
		t.Fatalf("symlink fingerprint = %#v, %v", fingerprint, err)
	}
}

func manifestFixtureRepository(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"scope", filepath.Join("scope", "__pycache__")} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		".gitignore":       "scope/ignored.txt\n",
		"scope/before.txt": "rename me\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(content), 0666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0666); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", "--all")
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	if err := os.Rename(filepath.Join(root, "scope", "before.txt"), filepath.Join(root, "scope", "after.txt")); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"scope/untracked.txt": "one\ntwo",
		"scope/ignored.txt":   "ignored\n",
		"scope/cache.pyc":     "bytecode",
		filepath.Join("scope", "__pycache__", "module.pyc"): "bytecode",
	} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), 0666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0666); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", "-A", "--", "scope/before.txt", "scope/after.txt")
}

func assertManifestJSON(t *testing.T, expected json.RawMessage, actual any) {
	t.Helper()
	encoded, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	decoder := json.NewDecoder(bytes.NewReader(expected))
	decoder.UseNumber()
	if err = decoder.Decode(&want); err != nil {
		t.Fatal(err)
	}
	decoder = json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err = decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		var formatted bytes.Buffer
		_ = json.Indent(&formatted, encoded, "", "  ")
		t.Fatalf("Go result differs from Python fixture: got %s; want %s", formatted.String(), expected)
	}
}
