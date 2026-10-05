package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/lease"
)

func TestPostFailureStartPythonFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/post_failure_start_python_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string         `json:"name"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 6 {
		t.Fatalf("fixture cases = %d, want 6", len(fixture.Cases))
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			root := t.TempDir()
			call := "failed start " + item.Name
			sum := sha256.Sum256([]byte(call))
			jobID := hex.EncodeToString(sum[:])
			event := map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": "mcp__codex-main__codex_start",
				"session_id": "start-failure-session", "tool_use_id": item.Name, "error": "start failed",
				"is_interrupt": false, "duration_ms": json.Number("1234")}
			key, err := callKey(event)
			if err != nil {
				t.Fatal(err)
			}
			project := "start-failure-project"
			pending := map[string]any{"tool_use_id": item.Name, "async_call": call, "job_id": jobID,
				"async_ready": true, "task_id": item.Name, "project_key": project, "route": "normal",
				"model": "gpt-fixture", "effort": "medium", "lease_owner": key,
				"repo_root": root, "kind": "call"}
			writeAsyncFixtureObject(t, filepath.Join(root, "pending"), key, pending)
			bridge := item.Name == "cancel_succeeds"
			if bridge {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				port := listener.Addr().(*net.TCPAddr).Port
				writeAsyncFixtureObject(t, filepath.Join(root, "codex_exec_mcp", "jobs"), jobID,
					map[string]any{"jobId": jobID, "call": call, "state": "RUNNING", "signalPort": port, "signalToken": "fixture-token"})
				done := make(chan bool, 1)
				go func() {
					conn, acceptErr := listener.Accept()
					if acceptErr != nil {
						done <- false
						return
					}
					defer conn.Close()
					buf := make([]byte, 256)
					_, _ = conn.Read(buf)
					jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
					if err := os.MkdirAll(jobDir, 0700); err != nil {
						done <- false
						return
					}
					jobData, _ := json.Marshal(map[string]any{"jobId": jobID, "call": call, "state": "CANCELLED", "signalPort": port, "signalToken": "fixture-token"})
					if err := os.WriteFile(filepath.Join(jobDir, jobID+".json"), jobData, 0600); err != nil {
						done <- false
						return
					}
					_, _ = conn.Write([]byte{'1'})
					done <- true
				}()
				defer func() {
					if !<-done {
						t.Error("cancel bridge did not accept")
					}
				}()
			} else if item.Name == "terminal_job" || item.Name == "live_job_rejected" {
				state := "FAILED"
				if item.Name == "live_job_rejected" {
					state = "RUNNING"
				}
				writeAsyncFixtureObject(t, filepath.Join(root, "codex_exec_mcp", "jobs"), jobID,
					map[string]any{"jobId": jobID, "call": call, "state": state})
			}
			indexPath := filepath.Join(root, "async_jobs", jobID+".json")
			writeAsyncFixtureObject(t, filepath.Join(root, "async_jobs"), jobID,
				map[string]any{"jobId": jobID, "project_key": project, "call": call})
			foreignID := hashAsyncCall(call + " foreign")
			foreignPath := filepath.Join(root, "async_jobs", foreignID+".json")
			if item.Name == "foreign_project_index" {
				writeAsyncFixtureObject(t, filepath.Join(root, "async_jobs"), foreignID,
					map[string]any{"jobId": foreignID, "project_key": "another-project"})
			}
			if item.Name == "lease_release_failure" {
				if err := os.MkdirAll(filepath.Dir(indexPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, "projects", project), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "projects", project, "scope_leases.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if acquired := lease.New(filepath.Join(root, "projects", project)).Acquire(key, item.Name,
				[]string{"src"}, lease.AcquireOptions{}, 100); acquired.Status != "OK" {
				t.Fatalf("lease acquire: %#v", acquired)
			}
			response := postFailureStart(event, root)
			pendingPath := filepath.Join(root, "pending", key+".json")
			saved, pendingExists := readOptionalObject(pendingPath)
			ctx := stringValue(mapValue(response["hookSpecificOutput"])["additionalContext"])
			ctx = strings.ReplaceAll(ctx, root, "__ROOT__")
			foreignExists := any(nil)
			if item.Name == "foreign_project_index" {
				_, err := os.Stat(foreignPath)
				foreignExists = err == nil
			}
			got := map[string]any{"response_context": ctx, "pending_exists": pendingExists,
				"pending_start_rejected": saved["start_rejected"] == true,
				"own_index_exists":       fileExists(indexPath), "foreign_index_exists": foreignExists}
			want := item.Expected
			if item.Name == "lease_release_failure" {
				if !pendingExists || !fileExists(indexPath) || !strings.Contains(ctx, "Evidence cleanup error:") {
					t.Fatalf("lease failure state = %#v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("result = %#v, want %#v", got, want)
			}
		})
	}
}

func readOptionalObject(path string) (map[string]any, bool) {
	value, err := readAsyncObject(path)
	return value, err == nil
}
