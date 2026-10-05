package jobs

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type finishCase struct {
	Name      string           `json:"name"`
	Finish    []any            `json:"finish"`
	Now       float64          `json:"now"`
	Result    map[string]any   `json:"result"`
	Files     map[string]any   `json:"files"`
	Decisions []map[string]any `json:"decisions"`
	Releases  []map[string]any `json:"releases"`
}

func TestFinishResultPythonParity(t *testing.T) {
	p := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_finish", "finish.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []finishCase `json:"cases"`
	}
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			decisions, releases := []map[string]any{}, []map[string]any{}
			stateRoot, id := t.TempDir(), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
			initial := map[string]any{"jobId": id, "state": "RUNNING", "acceptedAt": c.Now - 20, "deadlineAt": c.Now + 30}
			switch c.Name {
			case "expired_record":
				initial["state"] = "FAILED"
				initial["finishedAt"] = c.Now - float64(jobRetentionSeconds) - 1
				initial["errorCode"] = "CODEX_FAILED"
				initial["message"] = "Codex failed"
			case "terminal_missing_output":
				initial["state"] = "SUCCEEDED"
				initial["finishedAt"] = c.Now
				initial["threadId"] = "thread-1"
				initial["outputFile"] = id + ".output"
			case "worktree_setup_failed":
				initial["setupError"] = map[string]any{"timedOut": true, "output": "setup log"}
			case "queued_lease_release":
				initial["leaseQueued"] = true
			case "accepted_decision":
				initial["state"] = "SUCCEEDED"
				initial["finishedAt"] = c.Now
				initial["threadId"] = "thread-1"
				initial["outputFile"] = id + ".output"
				initial["taskId"] = "task-accepted"
			case "unknown_job":
				initial = nil
			}
			if initial != nil {
				raw, _ := json.Marshal(initial)
				r, e := Decode(raw)
				if e != nil {
					t.Fatal(e)
				}
				if e = Write(filepath.Join(dir, id+".json"), r); e != nil {
					t.Fatal(e)
				}
			}
			if c.Name == "accepted_decision" {
				if e := os.WriteFile(filepath.Join(dir, id+".output"), []byte("accepted"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if c.Name == "queued_lease_release" {
				root := stateRoot
				indexDir := filepath.Join(root, "async_jobs")
				if e := os.MkdirAll(indexDir, 0700); e != nil {
					t.Fatal(e)
				}
				b, _ := json.Marshal(map[string]any{"project_key": "project-1", "call_key": "call-1"})
				if e := os.WriteFile(filepath.Join(indexDir, id+".json"), b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if len(c.Finish) > 0 {
				s, _ := c.Finish[0].(string)
				code, _ := c.Finish[1].(string)
				result, _ := c.Finish[2].(map[string]any)
				msg, _ := c.Finish[3].(string)
				if summary, ok := result["content"].(map[string]any); ok {
					length := int(summary["length"].(float64))
					content := strings.Repeat("x", length)
					if !sameFinish(textSummary(content), summary) {
						t.Fatal("generated oversized content summary mismatch")
					}
					result["content"] = content
				}
				if err = finishJob(dir, id, s, code, result, msg, c.Now, FinishOptions{ReleaseLease: func(project, call string) {
					releases = append(releases, map[string]any{"project": project, "callKey": call})
				}}); err != nil {
					t.Fatal(err)
				}
			}
			got := resultJob(dir, id, c.Now, ResultOptions{RecoverJob: func(string) string { return "UNCONFIRMED" }, RecordDecision: func(job map[string]any, decision, kind, source string) {
				decisions = append(decisions, map[string]any{"jobId": job["jobId"], "decision": decision, "kind": kind, "sourceJobId": source})
			}})
			if !sameFinish(normalizeFinish(got), normalizeFinish(c.Result)) {
				t.Fatalf("result mismatch: got %#v want %#v", got, c.Result)
			}
			files := map[string]any{}
			for _, name := range []string{id + ".json", id + ".output"} {
				raw, e := os.ReadFile(filepath.Join(dir, name))
				if e == nil {
					if name == id+".output" {
						files[name] = string(raw)
					} else {
						var v any
						if e = json.Unmarshal(raw, &v); e != nil {
							t.Fatal(e)
						}
						files[name] = normalizeFinish(v)
					}
				}
			}
			if len(files) == 0 {
				files = nil
			}
			if !sameFinish(files, c.Files) {
				t.Fatalf("records mismatch: got %#v want %#v", files, c.Files)
			}
			if !sameFinish(decisions, c.Decisions) || !sameFinish(releases, c.Releases) {
				t.Fatalf("side effects differ: decisions=%#v releases=%#v", decisions, releases)
			}
		})
	}
}

func normalizeFinish(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			if k == "acceptedAt" || k == "finishedAt" {
				x[k] = "TIME"
			} else if k == "deadlineAt" {
				x[k] = "DEADLINE"
			} else {
				x[k] = normalizeFinish(y)
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalizeFinish(x[i])
		}
		return x
	}
	return v
}

func textSummary(s string) map[string]any {
	h := sha256.Sum256([]byte(s))
	return map[string]any{"length": len(s), "sha256": fmt.Sprintf("%x", h), "head": s[:32], "tail": s[len(s)-32:]}
}

func sameFinish(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
