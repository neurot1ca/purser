package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"runtime"
	"testing"
)

func TestPostAsyncClaimPythonFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_async_claim.json"))
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
	if fixture.Oracle != "codex._async_claim+_async_owned+_async_post_followup" || len(fixture.Cases) != 6 {
		t.Fatalf("unexpected async-claim fixture: oracle=%q cases=%d", fixture.Oracle, len(fixture.Cases))
	}
	for _, test := range fixture.Cases {
		t.Run(test["name"].(string), func(t *testing.T) {
			state := t.TempDir()
			repo := filepath.Join(state, "repo")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if output, err := execGitInit(repo); err != nil {
				t.Fatalf("git init: %v: %s", err, output)
			}
			project := projectKey(&repo)
			input := replaceAsyncFixture(test["event"], repo, project).(map[string]any)
			index := replaceAsyncFixture(test["index"], repo, project).(map[string]any)
			writeAsyncFixtureObject(t, filepath.Join(state, "async_jobs"), inputJobID(input), index)
			if test["pending"] != nil {
				writeAsyncFixtureObject(t, filepath.Join(state, "pending"), stringValue(index["call_key"]), replaceAsyncFixture(test["pending"], repo, project))
			}
			if test["job"] != nil {
				writeAsyncFixtureObject(t, filepath.Join(state, "codex_exec_mcp", "jobs"), inputJobID(input), replaceAsyncFixture(test["job"], repo, project))
			}
			want := mapValue(test["expected"])
			switch test["name"] {
			case "claim_lock":
				path := filepath.Join(state, "async_jobs", inputJobID(input)+".claim.lock")
				lock, gotIndex, _, _, cached, err := claimAsyncResult(state, input, repo)
				if err != nil {
					t.Fatal(err)
				}
				wantSize := int64(0)
				if runtime.GOOS == "windows" {
					wantSize = 1
				}
				lockInfo, err := os.Stat(path)
				if err != nil || lockInfo.Size() != wantSize {
					t.Fatalf("fresh claim lock size = %v, err=%v; want %d", lockInfo, err, wantSize)
				}
				if runtime.GOOS == "windows" {
					marker := []byte{0}
					if n, readErr := lock.ReadAt(marker, 0); readErr != nil || n != 1 || marker[0] != '0' {
						t.Fatalf("fresh Windows claim lock marker = %q, n=%d, err=%v; want %q", marker, n, readErr, "0")
					}
				}
				_, _, _, _, _, blockedErr := claimAsyncResult(state, input, repo)
				if !errors.Is(blockedErr, errAsyncClaimed) || want["concurrent_blocked"] != true {
					t.Fatalf("concurrent claim error = %v, blocked=%v", blockedErr, want["concurrent_blocked"])
				}
				if cached != nil || gotIndex["stage"] != want["stage"] {
					t.Fatalf("claim result stage=%v cached=%#v, want %v", gotIndex["stage"], cached, want["stage"])
				}
				response := map[string]any{"review": "cached"}
				indexPath := filepath.Join(state, "async_jobs", inputJobID(input)+".json")
				if err := consumeAsyncResult(indexPath, gotIndex, "bundle.json", response); err != nil {
					t.Fatal(err)
				}
				releaseTaskUsageLock(lock)
				lock, _, _, _, cached, err = claimAsyncResult(state, input, repo)
				if err != nil {
					t.Fatal(err)
				}
				defer releaseTaskUsageLock(lock)
				if !reflect.DeepEqual(cached, response) {
					t.Fatalf("second claim cached response = %#v, want %#v", cached, response)
				}
			case "consumed_cached_response":
				index["response"] = replaceAsyncFixture(mapValue(want)["cached"], repo, project)
				writeAsyncFixtureObject(t, filepath.Join(state, "async_jobs"), inputJobID(input), index)
				lock, _, _, _, cached, err := claimAsyncResult(state, input, repo)
				if err != nil {
					t.Fatal(err)
				}
				defer releaseTaskUsageLock(lock)
				if !reflect.DeepEqual(cached, replaceAsyncFixture(want["response"], repo, project)) {
					t.Fatalf("cached response = %#v, want %#v", cached, want["response"])
				}
			case "adoption_required", "malformed_index", "revalidation_changed_job":
				_, _, _, _, _, err := claimAsyncResult(state, input, repo)
				wantKind, _ := want["error"].(string)
				ownership, ok := err.(asyncOwnershipError)
				if !ok || ownership.kind != wantKind {
					t.Fatalf("claim error = %#v, want %s", err, wantKind)
				}
			case "adoption_allowed":
				lock, gotIndex, _, _, cached, err := claimAsyncResult(state, input, repo)
				if err != nil {
					t.Fatal(err)
				}
				defer releaseTaskUsageLock(lock)
				if cached != nil || gotIndex["stage"] != "FINALIZING" {
					t.Fatalf("adopted claim stage=%v cached=%#v", gotIndex["stage"], cached)
				}
			}
		})
	}
}

func execGitInit(repo string) ([]byte, error) {
	return testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput()
}
