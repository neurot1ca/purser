package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestSessionLookupParityFixture(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	fixturePath := filepath.Join(repo, ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "session_lookup.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle       string `json:"oracle"`
		Construction []struct {
			Input  map[string]any `json:"input"`
			Output map[string]any `json:"output"`
		} `json:"construction"`
		Lookup []struct {
			Name     string                     `json:"name"`
			ThreadID string                     `json:"thread_id"`
			Files    map[string]json.RawMessage `json:"files"`
			Output   map[string]any             `json:"output"`
		} `json:"lookup"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "Python ledger.build_session_record and SessionLedger.find_by_thread_id" {
		t.Fatalf("unexpected oracle: %q", fixture.Oracle)
	}
	var roundTrip map[string]any
	for _, test := range fixture.Construction {
		input := test.Input
		var threadID *string
		if value, ok := input["thread_id"].(string); ok {
			threadID = &value
		}
		options, _ := input["options"].(map[string]any)
		got := BuildSessionRecord(input["task_id"].(string), threadID, input["route"].(string), input["model"].(string), input["effort"].(string), options)
		if !reflect.DeepEqual(got, test.Output) {
			t.Fatalf("BuildSessionRecord(%v) = %#v, want %#v", input, got, test.Output)
		}
		if roundTrip == nil && got["live_thread"] != nil {
			roundTrip = got
		}
	}
	if roundTrip == nil {
		t.Fatal("fixture has no constructed live-thread record")
	}
	root := t.TempDir()
	if saved := Save(root, roundTrip); saved.Status != OK || saved.Code != "LEDGER_WRITTEN" {
		t.Fatalf("save constructed record: %#v", saved)
	}
	path, _ := PathForTask(root, roundTrip["task_id"].(string))
	loaded := Load(path, roundTrip["task_id"].(string))
	if loaded.Status != OK || !reflect.DeepEqual(loaded.Record, roundTrip) {
		t.Fatalf("Go round trip: %#v", loaded)
	}
	moduleDir := filepath.Join(repo, ".claude", "hooks", "orchestrator")
	expected, _ := json.Marshal(roundTrip)
	script := "import json,sys; sys.path.insert(0,sys.argv[1]); from ledger import SessionLedger; result=SessionLedger(sys.argv[2]).load(sys.argv[3]); assert result['status']=='OK' and result['record']==json.loads(sys.argv[4]), result"
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Fatal("Python interpreter unavailable")
	}
	command := exec.Command(python, "-B", "-c", script, moduleDir, root, roundTrip["task_id"].(string), string(expected))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python could not load constructed Go record: %v: %s", err, output)
	}
	for _, test := range fixture.Lookup {
		sessions := filepath.Join(root, "sessions")
		if err := os.RemoveAll(sessions); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(sessions, 0700); err != nil {
			t.Fatal(err)
		}
		for name, envelope := range test.Files {
			if err := os.WriteFile(filepath.Join(sessions, name), envelope, 0600); err != nil {
				t.Fatal(err)
			}
			if test.Name == "invalid-record-ignored" || test.Name == "checksum-failure-ignored" {
				var decoded struct {
					Checksum string         `json:"checksum"`
					Payload  map[string]any `json:"payload"`
				}
				if err := json.Unmarshal(envelope, &decoded); err != nil {
					t.Fatal(err)
				}
				thread := decoded.Payload["live_thread"].(map[string]any)
				canonical, _ := canonicalJSON(decoded.Payload)
				digest := sha256.Sum256(canonical)
				checksumValid := decoded.Checksum == hex.EncodeToString(digest[:])
				recordValid := validRecord(decoded.Payload, "")
				if thread["thread_id"] != test.ThreadID || (test.Name == "invalid-record-ignored" && (!checksumValid || recordValid)) || (test.Name == "checksum-failure-ignored" && (checksumValid || !recordValid)) {
					t.Fatalf("%s does not isolate its rejection branch", test.Name)
				}
			}
		}
		if got := FindByThreadID(root, test.ThreadID); !reflect.DeepEqual(got, test.Output) {
			t.Fatalf("FindByThreadID(%q), case %s = %#v, want %#v", test.ThreadID, test.Name, got, test.Output)
		}
	}
}

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
