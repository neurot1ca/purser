package jobs

import "purser-hook/internal/testutil"

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

const hostParityCall = "host-parity-call"

type hostParityFixture struct {
	Name     string         `json:"name"`
	Snapshot map[string]any `json:"snapshot"`
	Record   map[string]any `json:"record"`
}

func TestHostPythonOracleReplay(t *testing.T) {
	fixturePath := filepath.Join("testdata", "host_parity.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("missing Python host oracle fixture %s: %v", fixturePath, err)
	}
	var want []hostParityFixture
	if err = json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != 13 {
		t.Fatalf("fixture has %d cases, want 13", len(want))
	}
	for _, fixture := range want {
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOMEDRIVE", "")
			t.Setenv("HOMEPATH", "")
			t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude-config"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
			t.Setenv("APPDATA", filepath.Join(root, "appdata"))
			t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
			t.Setenv("GOCACHE", filepath.Join(root, "go-cache"))
			t.Setenv("GOTMPDIR", filepath.Join(root, "go-tmp"))
			if fixture.Name != "worktree_success" && fixture.Name != "worktree_create_failure" && fixture.Name != "worktree_setup_failure" {
				cmd := testutil.Hide(exec.Command("git", "init", "-q", root))
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v: %s", err, output)
				}
			}
			jobID := hexHash(hostParityCall)
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if fixture.Name == "queued" || fixture.Name == "cancel_queued" {
				setupHostParityAdmission(t, root)
			}
			worktreeCase := fixture.Name == "worktree_success" || fixture.Name == "worktree_create_failure" || fixture.Name == "worktree_setup_failure"
			if fixture.Name == "worktree_success" || fixture.Name == "worktree_setup_failure" {
				setupHostParityRepo(t, filepath.Join(root, "repo"), fixture.Name == "worktree_setup_failure")
			}
			if fixture.Name == "worktree_setup_failure" {
				config := filepath.Join(home, ".orchestrator", "config.toml")
				if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
					t.Fatal(err)
				}
				trust := "schema_version = 1\n[trust]\npaths = [\"" + filepath.ToSlash(filepath.Join(root, "repo")) + "\"]\n"
				if err := os.WriteFile(config, []byte(trust), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
			}
			deadline := time.Now().Add(30 * time.Second)
			if fixture.Name == "deadline" {
				deadline = time.Now().Add(2 * time.Second)
			}
			job := map[string]any{"jobId": jobID, "call": hostParityCall, "acceptedAt": float64(time.Now().UnixNano()) / 1e9, "state": "QUEUED", "generation": 1,
				"ownershipToken": "host-owner-token", "signalToken": "host-signal-token", "deadlineAt": float64(deadline.UnixNano()) / 1e9,
				"leaseQueued": fixture.Name == "queued" || fixture.Name == "cancel_queued"}
			if fixture.Name == "stale" {
				job["generation"] = 2
			}
			if fixture.Name == "stall" {
				job["liveness"] = map[string]any{"lastActivityAt": float64(time.Now().UnixNano())/1e9 - 2000,
					"stdoutBytes": 0, "longestSilentGapSeconds": 0.0}
			}
			writeHostTestJSON(t, filepath.Join(dir, jobID+".json"), job)
			agent := "codex"
			if fixture.Name == "agent_success" || fixture.Name == "agent_failure" {
				agent = "opencode"
			}
			cwd := root
			if fixture.Name == "worktree_success" || fixture.Name == "worktree_setup_failure" {
				cwd = filepath.Join(root, "repo")
			}
			if fixture.Name == "worktree_create_failure" {
				cwd = os.TempDir()
			}
			args := map[string]any{"cwd": cwd, "prompt": "hello"}
			if worktreeCase {
				args["worktree"] = true
			}
			if agent == "opencode" {
				args["model"] = "provider/model"
			}
			writeHostTestJSON(t, filepath.Join(dir, jobID+".launch"), map[string]any{
				"name": "codex", "agent": agent, "codexBin": []string{"fake-worker"}, "args": args,
			})
			_ = os.Setenv("PURSER_HOST_FAKE_WORKER", "1")
			mode := fixture.Name
			if mode == "deadline" || mode == "cancel" || mode == "cancel_queued" || mode == "stall" {
				mode = "sleep"
			}
			_ = os.Setenv("PURSER_HOST_FAKE_MODE", mode)
			_ = os.Setenv("PURSER_HOST_FAKE_AGENT", agent)
			defer os.Unsetenv("PURSER_HOST_FAKE_WORKER")
			defer os.Unsetenv("PURSER_HOST_FAKE_MODE")
			defer os.Unsetenv("PURSER_HOST_FAKE_AGENT")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			options := HostOptions{HostContained: true, WorkerCommand: func(args LaunchArgs) ([]string, error) {
				return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
			}}
			finished := make(chan error, 1)
			if fixture.Name == "queued" {
				go func() {
					_, runErr := RunHostGeneration(dir, jobID, 1, "host-owner-token", options)
					finished <- runErr
				}()
				waitForHostQueued(t, dir, jobID)
				lease.New(filepath.Join(root, "projects", "project")).Release("blocker", float64(time.Now().UnixNano())/1e9)
				if err := <-finished; err != nil {
					t.Fatal(err)
				}
			} else if fixture.Name == "cancel" || fixture.Name == "stall" || fixture.Name == "cancel_queued" {
				go func() {
					_, runErr := RunHostGeneration(dir, jobID, 1, "host-owner-token", options)
					finished <- runErr
				}()
				if fixture.Name == "cancel_queued" {
					waitForHostQueued(t, dir, jobID)
				} else {
					waitForHostStateID(t, dir, jobID, "RUNNING")
					if fixture.Name == "stall" {
						waitForHostErrorCode(t, dir, jobID, "STALLED")
						gotSnapshot := normalizeHostParity(t, readHostParityRecord(t, dir, jobID), root)
						if !reflect.DeepEqual(gotSnapshot, fixture.Snapshot) {
							t.Fatalf("stall snapshot differs\n got: %#v\nwant: %#v", gotSnapshot, fixture.Snapshot)
						}
					}
				}
				port := waitForHostSignalPort(t, dir, jobID)
				if ok, signalErr := SignalCancel("127.0.0.1:"+strconv.Itoa(port), jobID, "host-signal-token", time.Second); signalErr != nil || !ok {
					t.Fatalf("cancel failed: acknowledged=%v err=%v", ok, signalErr)
				}
				if err := <-finished; err != nil {
					t.Fatal(err)
				}
			} else if fixture.Name == "deadline" {
				go func() {
					_, runErr := RunHostGeneration(dir, jobID, 1, "host-owner-token", options)
					finished <- runErr
				}()
				waitForHostStateID(t, dir, jobID, "RUNNING")
				if err := <-finished; err != nil {
					t.Fatal(err)
				}
			} else if _, err := RunHostGeneration(dir, jobID, 1, "host-owner-token", options); err != nil {
				t.Fatal(err)
			}
			completed := readHostParityRecord(t, dir, jobID)
			if _, supervised := completed.values["treeReceipt"]; supervised {
				if stopped, err := recordedTreeStopped(dir, completed); !stopped {
					t.Fatalf("tree stop not confirmed: %v", err)
				}
			}
			got := normalizeHostParity(t, completed, root)
			if failed, ok := got["testEvidenceFailed"].(bool); ok && !failed {
				if _, oracleHasField := fixture.Record["testEvidenceFailed"]; !oracleHasField {
					delete(got, "testEvidenceFailed")
				}
			}
			if !reflect.DeepEqual(got, fixture.Record) {
				t.Fatalf("host record differs\n got: %#v\nwant: %#v", got, fixture.Record)
			}
		})
	}
}

func setupHostParityAdmission(t *testing.T, root string) {
	t.Helper()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "file"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "src/file"}, {"commit", "-qm", "baseline"}} {
		cmd := testutil.Hide(exec.Command("git", args...))
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
			"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	jobID := hexHash(hostParityCall)
	project := filepath.Join(root, "projects", "project")
	blocker, id := "blocker-job", jobID
	leases := lease.New(project)
	if leases.Acquire("blocker", "blocker", []string{"src"}, lease.AcquireOptions{JobID: &blocker}, float64(time.Now().UnixNano())/1e9).Status != "OK" {
		t.Fatal("could not create blocking lease")
	}
	if leases.Acquire("owner-key", "task", []string{"src"}, lease.AcquireOptions{JobID: &id, QueueConflict: true}, float64(time.Now().UnixNano())/1e9).Status != "OK" {
		t.Fatal("could not create queued lease")
	}
	pending := map[string]any{"job_id": jobID, "async_call": hostParityCall, "lease_queued": true, "lease_owner": "owner-key",
		"project_key": "project", "repo_root": repo, "task_id": "task", "scope": []string{"src"}}
	if err := writeFixtureJSON(filepath.Join(root, "pending", "owner-key.json"), pending); err != nil {
		t.Fatal(err)
	}
	index := map[string]any{"jobId": jobID, "call": hostParityCall, "call_key": "owner-key", "project_key": "project"}
	if err := writeFixtureJSON(filepath.Join(root, "async_jobs", jobID+".json"), index); err != nil {
		t.Fatal(err)
	}
	baseline := map[string]any{"status": "UNKNOWN", "code": "QUEUED"}
	if saved := ledger.Save(project, ledger.BuildSessionRecord("task", nil, "", "", "", map[string]any{"pending_call_baseline": baseline})); saved.Status != "OK" {
		t.Fatal("could not create queued session")
	}
}

func setupHostParityRepo(t *testing.T, repo string, setupFailure bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "file"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if setupFailure {
		config := filepath.Join(repo, ".orchestrator")
		if err := os.MkdirAll(config, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(config, "profile.toml"), []byte("schema_version = 1\n[worktree]\nsetup = \"exit 9\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "baseline"}} {
		cmd := testutil.Hide(exec.Command("git", args...))
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
			"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
}

func waitForHostQueued(t *testing.T, dir, id string) {
	t.Helper()
	until := time.Now().Add(30 * time.Second)
	lastState, lastCode, lastPort := "", "", 0
	for time.Now().Before(until) {
		record := readHostParityRecord(t, dir, id)
		var state string
		var code string
		var port int
		record.Get("state", &state)
		record.Get("errorCode", &code)
		record.Get("signalPort", &port)
		lastState, lastCode, lastPort = state, code, port
		if state == "QUEUED" && port > 0 {
			return
		}
		if state != "QUEUED" {
			t.Fatalf("host left QUEUED while awaiting lease: state=%s code=%s", state, code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("host did not remain queued with its signal listener: state=%s code=%s port=%d", lastState, lastCode, lastPort)
}

func waitForHostStateID(t *testing.T, dir, id, expected string) {
	t.Helper()
	until := time.Now().Add(30 * time.Second)
	last, code := "", ""
	for time.Now().Before(until) {
		record := readHostParityRecord(t, dir, id)
		record.Get("state", &last)
		record.Get("errorCode", &code)
		if last == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("host did not reach %s: state=%s code=%s", expected, last, code)
}

func waitForHostErrorCode(t *testing.T, dir, id, code string) {
	t.Helper()
	until := time.Now().Add(30 * time.Second)
	lastState, lastCode := "", ""
	for time.Now().Before(until) {
		record := readHostParityRecord(t, dir, id)
		record.Get("state", &lastState)
		record.Get("errorCode", &lastCode)
		if lastCode == code {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("host did not detect expected liveness state: state=%s code=%s", lastState, lastCode)
}

func waitForHostSignalPort(t *testing.T, dir, id string) int {
	t.Helper()
	until := time.Now().Add(30 * time.Second)
	for time.Now().Before(until) {
		record := readHostParityRecord(t, dir, id)
		var port int
		record.Get("signalPort", &port)
		if port > 0 {
			connection, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 100*time.Millisecond)
			if err == nil {
				_ = connection.Close()
				return port
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("host signal listener did not become ready within 30 seconds")
	return 0
}

func readHostParityRecord(t *testing.T, dir, id string) *Record {
	t.Helper()
	path := filepath.Join(dir, id+".json")
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		record, err := Read(path)
		if err == nil {
			return record
		}
		time.Sleep(time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	record, err := Read(path)
	t.Fatalf("read host parity job: %v; raw=%q", err, data)
	return record
}

func normalizeHostParity(t *testing.T, record *Record, root string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recordBytes(t, record), &value); err != nil {
		t.Fatal(err)
	}
	delete(value, "treeReceipt") // Go-only lifecycle proof is asserted separately.
	return normalizeHostParityValue(value, root).(map[string]any)
}

func normalizeHostParityValue(value any, root string) any {
	switch item := value.(type) {
	case map[string]any:
		state, _ := item["state"].(string)
		for key, child := range item {
			switch key {
			case "hostPid", "pid":
				item[key] = "__PID__"
			case "ownershipToken", "signalToken":
				item[key] = "__TOKEN__"
			case "hostCreatedAt", "signalPort":
				item[key] = "__HOST_IDENTITY__"
			case "hostContained":
				item[key] = "__HOST_CONTAINED__"
			case "longestSilentGapSeconds":
				item[key] = "__DURATION__"
			case "stdoutBytes":
				if number, ok := child.(float64); ok && number > 0 {
					item[key] = "__STDOUT_BYTES__"
				} else {
					item[key] = normalizeHostParityValue(child, root)
				}
			case "exitCode":
				if state == "TIMED_OUT" || state == "CANCELLED" {
					item[key] = "__TERMINATION_EXIT_CODE__"
				} else {
					item[key] = normalizeHostParityValue(child, root)
				}
			default:
				if strings.HasSuffix(key, "At") || strings.HasSuffix(key, "_at") {
					item[key] = "__TIME__"
				} else {
					item[key] = normalizeHostParityValue(child, root)
				}
			}
		}
	case []any:
		for index := range item {
			item[index] = normalizeHostParityValue(item[index], root)
		}
	case string:
		escapedRoot := strings.ReplaceAll(root, `\`, `\\`)
		forwardRoot := filepath.ToSlash(root)
		item = strings.ReplaceAll(item, forwardRoot, "__ROOT__")
		if strings.HasPrefix(item, "__ROOT__/") {
			item = "__ROOT__" + string(filepath.Separator) + filepath.FromSlash(strings.TrimPrefix(item, "__ROOT__/"))
		}
		item = strings.ReplaceAll(strings.ReplaceAll(item, escapedRoot, "__ROOT__"), root, "__ROOT__")
		if strings.Contains(item, "__ROOT__") {
			item = strings.ReplaceAll(item, `\\`, "/")
			item = strings.ReplaceAll(item, `\`, "/")
		}
		return item
	}
	return value
}

func recordBytes(t *testing.T, record *Record) []byte {
	t.Helper()
	value, err := record.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
