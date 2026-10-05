package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAsyncOwnedFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "async_owned.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string           `json:"oracle"`
		Cases  []map[string]any `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode async-owned fixture: %v", err)
	}
	if fixture.Oracle != "codex._async_owned+_async_job+_async_pre_followup" || len(fixture.Cases) != 9 {
		t.Fatalf("unexpected async-owned fixture: oracle=%q cases=%d", fixture.Oracle, len(fixture.Cases))
	}
	for _, item := range fixture.Cases {
		t.Run(item["name"].(string), func(t *testing.T) {
			state := t.TempDir()
			repo := filepath.Join(state, "repo")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, output)
			}
			project := projectKey(&repo)
			input := replaceAsyncFixture(item["input"], repo, project).(map[string]any)
			disk := replaceAsyncFixture(item["disk"], repo, project).(map[string]any)
			writeAsyncFixtureObject(t, filepath.Join(state, "async_jobs"), inputJobID(input), disk["index"])
			writeAsyncFixtureObject(t, filepath.Join(state, "pending"), stringValue(mapValue(disk["index"])["call_key"]), disk["pending"])
			writeAsyncFixtureObject(t, filepath.Join(state, "codex_exec_mcp", "jobs"), inputJobID(input), disk["job"])

			path, gotIndex, gotPending, original, err := asyncOwned(state, input, repo)
			want := mapValue(item["python"])["async_owned"]
			if expectedError := mapValue(want)["error"]; expectedError != nil {
				ownershipErr, ok := err.(asyncOwnershipError)
				if !ok || ownershipErr.kind != expectedError || ownershipErr.message != mapValue(want)["message"] {
					t.Fatalf("asyncOwned error = %#v, want %v: %v", err, expectedError, mapValue(want)["message"])
				}
			} else {
				if err != nil {
					t.Fatalf("asyncOwned: %v", err)
				}
				var gotPendingTask, gotSession, gotToolUse any
				if gotPending != nil {
					gotPendingTask = gotPending["task_id"]
				}
				if original != nil {
					gotSession, gotToolUse = original["session_id"], original["tool_use_id"]
				}
				got := map[string]any{"path": filepath.ToSlash(path[len(state)+1:]), "stage": gotIndex["stage"], "pending_task_id": gotPendingTask, "original_session_id": gotSession, "original_tool_use_id": gotToolUse}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("asyncOwned = %#v, want %#v", got, want)
				}
			}

			gotJob := asyncJob(state, mapValue(disk["index"]))
			wantJob := mapValue(item["python"])["async_job"]
			if wantJob == nil && gotJob != nil || wantJob != nil && !reflect.DeepEqual(gotJob, wantJob) {
				t.Fatalf("asyncJob = %#v, want %#v", gotJob, mapValue(item["python"])["async_job"])
			}
			gotPre, preErr := Pre(input, state)
			if preErr != nil {
				t.Fatalf("Pre: %v", preErr)
			}
			wantPre := mapValue(item["python"])["pre_followup"]
			if !reflect.DeepEqual(gotPre, wantPre) {
				t.Fatalf("Pre = %#v, want %#v", gotPre, wantPre)
			}
			if item["name"] == "adoptable" {
				jobPath := filepath.Join(state, "codex_exec_mcp", "jobs", inputJobID(input)+".json")
				if err := os.Remove(jobPath); err != nil {
					t.Fatal(err)
				}
				_, _, _, _, err := asyncOwned(state, input, repo)
				ownershipErr, ok := err.(asyncOwnershipError)
				if !ok || ownershipErr.kind != "WiringError" || ownershipErr.message != "job and durable result do not match" {
					t.Fatalf("adopt missing durable job error = %#v", err)
				}
				got, preErr := Pre(input, state)
				want := denyPre("UNKNOWN_ASYNC_JOB", "No pending job belongs to this session/project.")
				if preErr != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("Pre with missing durable job = %#v, %v; want %#v", got, preErr, want)
				}
			}
		})
	}
}

func inputJobID(input map[string]any) string {
	return stringValue(mapValue(input["tool_input"])["jobId"])
}

func writeAsyncFixtureObject(t *testing.T, dir, name string, value any) {
	t.Helper()
	if value == nil {
		return
	}
	path := filepath.Join(dir, name+".json")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func replaceAsyncFixture(value any, repo, project string) any {
	switch item := value.(type) {
	case string:
		if item == "__REPO__" {
			return repo
		}
		if item == "__PROJECT_KEY__" {
			return project
		}
		return item
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			out[key] = replaceAsyncFixture(child, repo, project)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			out[i] = replaceAsyncFixture(child, repo, project)
		}
		return out
	default:
		return value
	}
}
