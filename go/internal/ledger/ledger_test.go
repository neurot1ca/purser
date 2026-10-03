package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPythonLedgerFixtures(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path, err := PathForTask(root, "go-pass3-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	got := Load(path, "go-pass3-fixture")
	if got.Status != "OK" || got.Code != "LEDGER_LOADED" || got.Record["task_id"] != "go-pass3-fixture" {
		t.Fatalf("valid Python fixture: %#v", got)
	}

	if err := os.WriteFile(path, fixture[:len(fixture)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "go-pass3-fixture"); got.Status != "CORRUPT" || got.Code != "LEDGER_CORRUPT" {
		t.Fatalf("truncated fixture: %#v", got)
	}

	var envelope map[string]any
	if err := json.Unmarshal(fixture, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["checksum"] = "0000000000000000000000000000000000000000000000000000000000000000"
	changed, _ := json.Marshal(envelope)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "go-pass3-fixture"); got.Status != "CORRUPT" || got.Code != "LEDGER_CORRUPT" {
		t.Fatalf("checksum mismatch: %#v", got)
	}

	payload := envelope["payload"].(map[string]any)
	payload["unexpected"] = true
	canonical, _ := canonicalJSON(payload)
	digest := sha256.Sum256(canonical)
	envelope["checksum"] = hex.EncodeToString(digest[:])
	changed, _ = json.Marshal(envelope)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "go-pass3-fixture"); got.Status != "CORRUPT" || got.Code != "LEDGER_CORRUPT" {
		t.Fatalf("checksummed invalid record: %#v", got)
	}
}

func TestPathForTaskMatchesPython(t *testing.T) {
	path, err := PathForTask("state", "go-pass3-fixture")
	if err != nil || path != filepath.Join("state", "sessions", "f19d7c528c3d3a1bdd94a436aa90c943e99c03c384c5c4d8c05d624d42f27bcf.json") {
		t.Fatalf("PathForTask = %q, %v", path, err)
	}
	if _, err := PathForTask("state", ""); err == nil {
		t.Fatal("empty task id accepted")
	}
}

func TestMissingIsDistinctFromCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if got := Load(path, "task"); got.Status != MISSING || got.Code != "LEDGER_RECORD_MISSING" {
		t.Fatalf("missing record: %#v", got)
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "task"); got.Status != CORRUPT || got.Code != "LEDGER_CORRUPT" {
		t.Fatalf("invalid record: %#v", got)
	}
}

func TestSaveAndFailedReplacementPreservesPriorRecord(t *testing.T) {
	root := t.TempDir()
	record := testRecord("go-save-test")
	if got := Save(root, record); got.Status != OK || got.Code != "LEDGER_WRITTEN" {
		t.Fatalf("save: %#v", got)
	}
	path, _ := PathForTask(root, "go-save-test")
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oldReplace := replaceFile
	replaceFile = func(string, string) error { return errors.New("injected replacement failure") }
	t.Cleanup(func() { replaceFile = oldReplace })
	replacement := testRecord("go-save-test")
	replacement["last_outcome"] = map[string]any{"status": "changed"}
	if got := Save(root, replacement); got.Status != "UNKNOWN" || got.Code != "LEDGER_WRITE_FAILED" {
		t.Fatalf("failed replacement: %#v", got)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(prior) {
		t.Fatalf("prior record changed after failed replacement: err=%v", err)
	}
}

func TestSaveRejectsInvalidRecord(t *testing.T) {
	if got := Save(t.TempDir(), map[string]any{"task_id": "invalid"}); got.Status != "UNKNOWN" || got.Code != "LEDGER_RECORD_INVALID" {
		t.Fatalf("invalid record: %#v", got)
	}
}

func TestGoSaveLoadsInPython(t *testing.T) {
	root := t.TempDir()
	if got := Save(root, testRecord("go-to-python")); got.Status != OK {
		t.Fatalf("save: %#v", got)
	}
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Fatal("Python interpreter unavailable")
	}
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	moduleDir := filepath.Join(repo, ".claude", "hooks", "orchestrator")
	script := "import sys; sys.path.insert(0, sys.argv[1]); from ledger import SessionLedger; result=SessionLedger(sys.argv[2]).load('go-to-python'); assert result['status']=='OK' and result['record']['task_id']=='go-to-python', result"
	command := exec.Command(python, "-c", script, moduleDir, root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python could not load Go record: %v: %s", err, output)
	}
}

func testRecord(taskID string) map[string]any {
	return map[string]any{
		"task_id": taskID, "live_thread": map[string]any{
			"task_id": taskID, "thread_id": "thread-1", "route": "route", "model": "model", "effort": "high",
			"task_contract_grounded": false,
		},
		"pending_call_baseline": nil, "last_outcome": nil,
	}
}
