package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func TestPostAsyncFailurePythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_async_failure.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string           `json:"oracle"`
		Cases  []map[string]any `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "codex._async_post_followup terminal failure" || len(fixture.Cases) != 7 {
		t.Fatalf("unexpected async failure fixture: oracle=%q cases=%d", fixture.Oracle, len(fixture.Cases))
	}
	for _, item := range fixture.Cases {
		t.Run(stringValue(item["name"]), func(t *testing.T) {
			root := t.TempDir()
			event, index := mapValue(item["event"]), mapValue(item["index"])
			pending, job := mapValue(item["pending"]), mapValue(item["job"])
			index["project_key"], pending["project_key"] = "0123456789abcdef", "0123456789abcdef"
			key, id := stringValue(index["call_key"]), stringValue(index["jobId"])
			index["stage"] = "FINALIZING"
			writeAsyncFixtureObject(t, filepath.Join(root, "async_jobs"), id, index)
			writeAsyncFixtureObject(t, filepath.Join(root, "pending"), key, pending)
			writeAsyncFixtureObject(t, filepath.Join(root, "codex_exec_mcp", "jobs"), id, job)
			want := mapValue(item["expected"])
			if item["name"] != "lease_release_failure" {
				result := lease.New(filepath.Join(root, "projects", stringValue(pending["project_key"]))).Acquire(
					stringValue(pending["lease_owner"]), stringValue(pending["task_id"]), []string{"src"}, lease.AcquireOptions{}, 100)
				if result.Status != "OK" {
					t.Fatalf("seed lease: %#v", result)
				}
			}
			if item["name"] == "crash_recovery" {
				if _, err := persistFailureEvidence(root, key, mapValue(item["evidence"])); err != nil {
					t.Fatal(err)
				}
			}
			if item["name"] == "replay_preparation_failure" {
				response, retryErr := retryAsyncFinalization(root, index, errors.New(stringValue(want["diagnostic"])), "updatedToolOutput")
				if retryErr != nil || !reflect.DeepEqual(replaceFailureRoot(response, root), want["response"]) {
					t.Fatalf("retryable replay response = %#v, %v; want %#v", response, retryErr, want["response"])
				}
				stored, readErr := readAsyncObject(filepath.Join(root, "async_jobs", id+".json"))
				if readErr != nil || stored["stage"] != "RETRYABLE" || stored["diagnostic"] != want["diagnostic"] {
					t.Fatalf("retryable replay index = %#v, %v", stored, readErr)
				}
				if _, statErr := os.Stat(filepath.Join(root, "pending", key+".json")); statErr != nil {
					t.Fatalf("retryable replay removed pending data: %v", statErr)
				}
				return
			}
			response, handled, err := finalizeAsyncFailure(root, event, index, pending)
			assertAsyncFailureSession(t, root, pending, key, false)
			if item["name"] == "lease_release_failure" {
				if err == nil || !handled {
					t.Fatalf("lease failure = %v, handled=%v", err, handled)
				}
				stored, readErr := readAsyncObject(filepath.Join(root, "async_jobs", id+".json"))
				if readErr != nil || stored["stage"] != "CONSUMED" {
					t.Fatalf("lease failure lost consumed response: %#v, %v", stored, readErr)
				}
				lock, _, _, _, cached, claimErr := claimAsyncResult(root, event, "")
				if claimErr != nil {
					t.Fatal(claimErr)
				}
				releaseTaskUsageLock(lock)
				recovered, recoveredOK, recoverErr := finalizeAsyncFailure(root, event, stored, nil)
				if recoverErr != nil || !recoveredOK || !reflect.DeepEqual(recovered, cached) {
					t.Fatalf("cached cleanup recovery = %#v, %v, handled=%v", recovered, recoverErr, recoveredOK)
				}
				if _, statErr := os.Stat(filepath.Join(root, "pending", key+".json")); !os.IsNotExist(statErr) {
					t.Fatalf("pending survives cached cleanup: %v", statErr)
				}
				return
			}
			if err != nil || !handled {
				t.Fatalf("finalize = %#v, handled=%v, err=%v", response, handled, err)
			}
			if got, expected := replaceFailureRoot(response, root), want["response"]; !reflect.DeepEqual(got, expected) {
				t.Fatalf("response = %#v, want %#v", got, expected)
			}
			stored, readErr := readAsyncObject(filepath.Join(root, "async_jobs", id+".json"))
			if readErr != nil || stored["stage"] != "CONSUMED" || !reflect.DeepEqual(stored["response"], response) {
				t.Fatalf("consumed index = %#v, %v", stored, readErr)
			}
			if _, statErr := os.Stat(filepath.Join(root, "pending", key+".json")); !os.IsNotExist(statErr) {
				t.Fatalf("pending remains: %v", statErr)
			}
			leases, leaseErr := readAsyncObject(filepath.Join(root, "projects", stringValue(pending["project_key"]), "scope_leases.json"))
			owner := mapValue(mapValue(leases["owners"])[stringValue(pending["lease_owner"])])
			if leaseErr != nil || owner["state"] != "RELEASED" {
				t.Fatalf("terminal failure lease state=%v err=%v", owner["state"], leaseErr)
			}
			evidence, readErr := readAsyncObject(filepath.Join(root, "raw_tool_outputs", key+"-failure.json"))
			if readErr != nil || !reflect.DeepEqual(replaceFailureRoot(evidence, root), want["evidence"]) {
				t.Fatalf("evidence = %#v, %v", evidence, readErr)
			}
			lock, _, _, _, cached, claimErr := claimAsyncResult(root, event, "")
			if claimErr != nil || !reflect.DeepEqual(cached, response) {
				t.Fatalf("repeat result = %#v, %v", cached, claimErr)
			}
			releaseTaskUsageLock(lock)
			observation, obsErr := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", stringValue(pending["task_id"])+".jsonl"))
			if obsErr != nil || !strings.Contains(string(observation), `"event_type":"IMPLEMENTER_FAILED"`) {
				t.Fatalf("failure observation missing: %v", obsErr)
			}
		})
	}
}

func TestAsyncFailureFinalizeErrorReleasesScopeLease(t *testing.T) {
	root := t.TempDir()
	project, owner, call := "project-fixture", "lease-owner", "failed async call"
	id := jobHash(call)
	writeAsyncFixtureObject(t, filepath.Join(root, "codex_exec_mcp", "jobs"), id,
		map[string]any{"jobId": id, "call": call, "state": "FAILED"})
	index := map[string]any{"jobId": id, "call": call, "call_key": "invalid-call-key", "project_key": project, "stage": "FINALIZING"}
	pending := map[string]any{"lease_owner": owner, "project_key": project}
	if got := lease.New(filepath.Join(root, "projects", project)).Acquire(owner, "failed-task", []string{"src"}, lease.AcquireOptions{}, 100); got.Status != "OK" {
		t.Fatalf("seed lease: %#v", got)
	}
	if _, handled, err := finalizeAsyncFailure(root, map[string]any{}, index, pending); err == nil || !handled {
		t.Fatalf("finalize error handled=%v err=%v", handled, err)
	}
	leases, err := readAsyncObject(filepath.Join(root, "projects", project, "scope_leases.json"))
	leaseOwner := mapValue(mapValue(leases["owners"])[owner])
	if err != nil || leaseOwner["state"] != "RELEASED" {
		t.Fatalf("finalize error lease state=%v err=%v", leaseOwner["state"], err)
	}
}

func assertAsyncFailureSession(t *testing.T, root string, pending map[string]any, key string, replay bool) {
	t.Helper()
	sessionPath, err := ledger.PathForTask(filepath.Join(root, "projects", stringValue(pending["project_key"])), stringValue(pending["task_id"]))
	if err != nil {
		t.Fatal(err)
	}
	saved := ledger.Load(sessionPath, stringValue(pending["task_id"]))
	if saved.Status != ledger.OK || mapValue(saved.Record["last_outcome"])["evidence_path"] == nil {
		t.Fatalf("failure session record missing: %#v", saved)
	}
	if replay && mapValue(saved.Record["live_thread"])["thread_id"] != "thread-fixture" {
		t.Fatalf("replay thread attribution missing: %#v", saved)
	}
}

func replaceFailureRoot(value any, root string) any {
	switch item := value.(type) {
	case string:
		escapedRoot := strings.ReplaceAll(root, `\`, `\\`)
		item = strings.ReplaceAll(item, escapedRoot, "__ROOT__")
		item = strings.ReplaceAll(item, root, "__ROOT__")
		if strings.Contains(item, "__ROOT__") {
			item = strings.ReplaceAll(item, `\`, "/")
		}
		return item
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			out[i] = replaceFailureRoot(child, root)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			if key == "project_key" {
				out[key] = "__PROJECT_KEY__"
			} else {
				out[key] = replaceFailureRoot(child, root)
			}
		}
		return out
	default:
		return value
	}
}
