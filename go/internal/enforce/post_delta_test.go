package enforce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"testing"

	"purser-hook/internal/ledger"
)

func TestAssemblePostDeltaPythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_delta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Case              map[string]any `json:"case"`
			Baseline          map[string]any `json:"baseline"`
			Snapshot          string         `json:"snapshot"`
			Expected          map[string]any `json:"expected"`
			PostToolUseResult map[string]any `json:"post_tool_use_result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		name, _ := test.Case["name"].(string)
		t.Run(name, func(t *testing.T) {
			root := postDeltaRepo(t)
			changes, _ := test.Case["changes"].([]any)
			for _, raw := range changes {
				change, _ := raw.(map[string]any)
				name, _ := change["path"].(string)
				content, _ := change["content"].(string)
				file := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pending := map[string]any{"phase": test.Case["phase"], "scope": test.Case["scope"], "baseline": test.Baseline, "lease_owner": test.Case["lease_owner"]}
			if test.Case["worktree"] == true {
				pending["worktree_path"], pending["worktree_snapshot"] = root, test.Snapshot
			}
			protected, _ := test.Case["protected_paths"].([]any)
			protectedPaths := make([]string, 0, len(protected))
			for _, item := range protected {
				if path, ok := item.(string); ok {
					protectedPaths = append(protectedPaths, path)
				}
			}
			owners := []map[string]any{}
			if raw, ok := test.Case["owners"].([]any); ok {
				for _, owner := range raw {
					if typed, valid := owner.(map[string]any); valid {
						owners = append(owners, typed)
					}
				}
			}
			bundle := baseBundle("")
			got, assembleErr := assemblePostDelta(root, pending, bundle, protectedPaths, owners)
			if test.Case["filter_failure"] == true {
				// The fixture records Python's internal PostToolUse fallback. Check
				// the checked Git boundary separately because the failure is injected
				// after Python has computed the worktree delta.
				if assembleErr != nil {
					t.Fatal(assembleErr)
				}
				findings, _ := test.PostToolUseResult["findings"].([]any)
				if len(findings) != 1 {
					t.Fatalf("Python PostToolUse failure result lacks findings: %#v", test.PostToolUseResult)
				}
				finding, ok := findings[0].(map[string]any)
				if !ok || finding["code"] != "INTERNAL_POST_ERROR" || finding["message"] == "" {
					t.Fatalf("Python PostToolUse failure result mismatch: %#v", findings)
				}
				bad := filepath.Join(root, ".git")
				if err := os.RemoveAll(bad); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(bad, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
				_, checkedErr := ledgerFilterFailure(root)
				if checkedErr == nil {
					t.Fatal("checked filter must return Git failure")
				}
				return
			}
			if assembleErr != nil {
				t.Fatal(assembleErr)
			}
			for field, want := range test.Expected {
				gotJSON, _ := json.Marshal(got[field])
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("%s mismatch\n got: %s\nwant: %s", field, gotJSON, wantJSON)
				}
			}
		})
	}
}

func postDeltaRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := testutil.Hide(exec.Command("git", "init", "-q", root))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for name, content := range map[string]string{"scope/file.txt": "base\n", "outside/file.txt": "base\n", "secrets/key.txt": "base\n"} {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", "--all")
	commit := testutil.Hide(exec.Command("git", "-C", root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"))
	commit.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE=2000-01-01T00:00:00+00:00")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	return root
}

func ledgerFilterFailure(root string) (map[string]any, error) {
	return ledger.FilterJobDeltaChecked(root, map[string]any{"status": "OK", "changed_paths": []string{"scope/file.txt"}})
}
