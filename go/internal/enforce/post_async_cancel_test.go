package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/lease"
)

func TestPostAsyncCancelPythonFixture(t *testing.T) {
	path := filepath.Join("testdata", "post_async_cancel_python_fixture.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string `json:"name"`
			Expected any    `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "codex._cancel_start_job+_start_job_terminal_or_absent+_async_owned" || len(fixture.Cases) != 9 {
		t.Fatalf("unexpected Python fixture: oracle=%q cases=%d", fixture.Oracle, len(fixture.Cases))
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			root := t.TempDir()
			call := "async-call:" + item.Name
			jobID := hashAsyncCall(call)
			var got any
			switch item.Name {
			case "authenticated_cancel", "wrong_auth_cancel", "absent_auth_cancel", "terminal_cancel":
				got = fixtureCancel(t, root, call, jobID, item.Name)
			case "terminal_job", "running_job", "start_job_absent", "malformed_job":
				pending := map[string]any{"async_call": call}
				jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
				if item.Name == "terminal_job" || item.Name == "running_job" {
					owner, project := "owner", "project"
					pending["lease_owner"], pending["project_key"] = owner, project
					if got := lease.New(filepath.Join(root, "projects", project)).Acquire(owner, "task", []string{"src"}, lease.AcquireOptions{}, 100); got.Status != "OK" {
						t.Fatalf("lease acquire: %#v", got)
					}
					state := "CANCELLED"
					if item.Name == "running_job" {
						state = "RUNNING"
					}
					writeAsyncFixtureObject(t, filepath.Dir(jobPath), jobID, map[string]any{
						"jobId": jobID, "call": call, "state": state})
					var cleanupErr error
					got, cleanupErr = cleanupTerminalAsyncStart(root, pending, nil, 200)
					if cleanupErr != nil {
						t.Fatal(cleanupErr)
					}
				} else if item.Name == "malformed_job" {
					if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(jobPath, []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				got = asyncStartTerminalOrAbsent(root, pending)
			case "cancel_response_other_project":
				got = fixtureForeignCancelOwnership(t, root, call, jobID)
			default:
				t.Fatalf("unknown case %q", item.Name)
			}
			if !reflect.DeepEqual(got, item.Expected) {
				t.Fatalf("result = %#v, want %#v", got, item.Expected)
			}
		})
	}
}

func fixtureCancel(t *testing.T, root, call, jobID, name string) any {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	response := byte('1')
	if name == "wrong_auth_cancel" {
		response = '0'
	}
	request := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			request <- ""
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		request <- string(buf[:n])
		_, _ = conn.Write([]byte{response})
	}()
	state := "RUNNING"
	if name == "terminal_cancel" {
		state = "CANCELLED"
	}
	job := map[string]any{"jobId": jobID, "call": call, "state": state, "signalPort": port}
	if name != "absent_auth_cancel" {
		job["signalToken"] = "bridge-secret"
	}
	writeAsyncFixtureObject(t, filepath.Join(root, "codex_exec_mcp", "jobs"), jobID, job)
	result := cancelAsyncStart(root, call, jobID)
	var observed any
	if name != "absent_auth_cancel" {
		observed = <-request
		if observed != jobID+":bridge-secret:CANCEL" {
			t.Fatalf("bridge request = %q", observed)
		}
	}
	return map[string]any{"cancelled": result, "request": observed}
}

func fixtureForeignCancelOwnership(t *testing.T, root, call, jobID string) any {
	t.Helper()
	state := filepath.Join(root, "state")
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	original := map[string]any{"tool_name": "mcp__codex-main__codex", "session_id": "owner-session", "tool_use_id": "origin"}
	key, err := callKey(original)
	if err != nil {
		t.Fatal(err)
	}
	project := "owned-project"
	index := map[string]any{"jobId": jobID, "project_key": project, "call_key": key,
		"session_id": "owner-session", "call": call, "stage": "ACTIVE"}
	pending := map[string]any{"tool_use_id": "origin", "job_id": jobID, "async_call": call,
		"session_id": "owner-session", "project_key": project, "task_id": "task", "baseline": map[string]any{}}
	writeAsyncFixtureObject(t, filepath.Join(state, "async_jobs"), jobID, index)
	writeAsyncFixtureObject(t, filepath.Join(state, "pending"), key, pending)
	writeAsyncFixtureObject(t, filepath.Join(state, "codex_exec_mcp", "jobs"), jobID, map[string]any{"jobId": jobID, "call": call})
	event := map[string]any{"tool_name": "mcp__codex-main__codex_cancel", "session_id": "other-session",
		"cwd": repo, "tool_input": map[string]any{"jobId": jobID}}
	_, _, _, _, err = asyncOwned(state, event, repo)
	ownershipErr, ok := err.(asyncOwnershipError)
	if !ok {
		return map[string]any{"error": "unexpected"}
	}
	return map[string]any{"error": ownershipErr.kind, "message": ownershipErr.message}
}

func hashAsyncCall(call string) string {
	sum := sha256.Sum256([]byte(call))
	return hex.EncodeToString(sum[:])
}
