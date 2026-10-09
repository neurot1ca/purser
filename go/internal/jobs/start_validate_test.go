package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/contextio"
	"purser-hook/internal/retention"
)

type startCase struct {
	Name      string           `json:"name"`
	Args      map[string]any   `json:"args"`
	Result    map[string]any   `json:"result"`
	Request   map[string]any   `json:"request"`
	Existing  bool             `json:"existing"`
	Calls     []map[string]any `json:"calls"`
	Seed      []map[string]any `json:"seed"`
	Thread    map[string]any   `json:"thread"`
	GitCase   bool             `json:"gitCase"`
	Files     map[string]any   `json:"files"`
	Launches  []map[string]any `json:"launches"`
	Decisions []map[string]any `json:"decisions"`
	Retiring  bool             `json:"retiring,omitempty"`
}

func TestStartValidationPythonParity(t *testing.T) {
	t.Setenv("PURSER_AGENT", "")
	p := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_start", "start.json")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		Cases []startCase `json:"cases"`
	}
	if e = json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Calls != nil {
				replayStart(t, c)
				return
			}
			o := StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }}
			if c.Name == "invalid_config" {
				o.FixedAgent = false
				o.ConfiguredAgent = func() (string, error) { return "", errors.New("bad configuration") }
			}
			if c.Existing {
				o.LookupExisting = func(string) map[string]any { return c.Result }
			}
			r, got := ValidateStart(c.Args, o)
			var want any = c.Result
			if c.Existing {
				got = r.ExistingResult
			} else if c.Result == nil {
				b, _ := json.Marshal(r)
				_ = json.Unmarshal(b, &got)
				want = c.Request
			}
			if actual, ok := got["structuredContent"].(map[string]any); ok {
				if expected, ok := c.Result["structuredContent"].(map[string]any); ok && expected["message"] == "Invalid Codex launch arguments" {
					if message, ok := actual["message"].(string); ok && strings.HasPrefix(message, "Invalid Codex launch arguments: ") {
						actual["message"], got["content"] = expected["message"], c.Result["content"]
					}
				}
			}
			if !sameStart(got, want) {
				t.Fatalf("got %#v want %#v", got, want)
			}
		})
	}
}

func TestValidateStartDefaultsMissingCWDToRepositoryRoot(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitStart(t, repo, "init", "-q")
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	args := map[string]any{"call": "00000000-0000-0000-0000-000000000001", "prompt": "run"}
	request, failure := ValidateStart(args, StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }})
	root, err := resolveRepositoryRoot()
	if err != nil || failure != nil || filepath.Clean(strings.ReplaceAll(root, "/", string(filepath.Separator))) != filepath.Clean(repo) {
		t.Fatalf("missing cwd did not resolve to repository root: root=%q request=%#v failure=%#v err=%v", root, request, failure, err)
	}
	if got := request.Args["cwd"]; got != root {
		t.Fatalf("missing cwd in validated launch args = %#v, want repository root %q", got, root)
	}
	argv, err := BuildCodexCommand(LaunchArgs{Name: "codex", Cwd: root, BinaryOverride: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range argv {
		if value == "-C" && (i+1 == len(argv) || argv[i+1] == "") {
			t.Fatalf("empty --cd value in argv: %#v", argv)
		}
	}
	emptyArgs := map[string]any{"call": "00000000-0000-0000-0000-000000000002", "prompt": "run", "cwd": ""}
	emptyRequest, emptyFailure := ValidateStart(emptyArgs, StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }})
	if emptyFailure != nil || emptyRequest.Args["cwd"] != root {
		t.Fatalf("empty cwd was not defaulted to repository root: request=%#v failure=%#v", emptyRequest, emptyFailure)
	}
}

func TestValidateStartRefusesMissingCWDOutsideRepository(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	args := map[string]any{"call": "00000000-0000-4000-8000-000000000001", "prompt": "run"}
	_, failure := ValidateStart(args, StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }})
	if failure == nil {
		t.Fatal("missing cwd outside a repository was accepted")
	}
	result := failure["structuredContent"].(map[string]any)
	if result["errorCode"] != "INVALID_ARGUMENT" || result["message"] != "cwd is missing and repository root could not be resolved for Codex launch" {
		t.Fatalf("unexpected missing cwd failure: %#v", result)
	}
}

func TestValidateStartAcceptsMechanicalOpenCodeArguments(t *testing.T) {
	t.Setenv("PURSER_AGENT", "")
	args := map[string]any{
		"agent": "opencode", "call": "00000000-0000-4000-8000-000000000001", "config": map[string]any{},
		"cwd": `C:\Projects\purser`, "etaMinutes": 3, "jobEffort": "high", "jobLabel": "mechanical task",
		"jobModel": "opencode/nemotron-3-ultra-free", "jobPhase": "implementation", "mode": "new",
		"model": "opencode/nemotron-3-ultra-free", "prompt": "implement the task",
	}
	request, failure := ValidateStart(args, StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(agent string) error {
		_, err := ResolveAgentPrefix(agent, nil)
		return err
	}})
	if failure != nil || request.Agent != "opencode" || request.Mode != "new" {
		t.Fatalf("OpenCode mechanical start failed validation: request=%#v failure=%#v", request, failure)
	}
}

func TestValidateStartReportsOpenCodeLaunchValidationError(t *testing.T) {
	args := map[string]any{
		"agent": "opencode", "call": "00000000-0000-4000-8000-000000000002", "config": map[string]any{},
		"cwd": t.TempDir(), "mode": "new", "model": "invalid-model", "prompt": "run",
	}
	_, failure := ValidateStart(args, StartValidationOptions{Agent: "codex", FixedAgent: true})
	if failure == nil {
		t.Fatal("invalid OpenCode model passed validation")
	}
	result := failure["structuredContent"].(map[string]any)
	if got, want := result["message"], "Invalid Codex launch arguments: OpenCode model must be provider/model"; got != want {
		t.Fatalf("message = %v, want %q", got, want)
	}
}

func TestStartBindsRegisteredPlanBeforeLaunch(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitStart(t, repo, "init", "-q")
	artifact := filepath.Join(base, "plan.md")
	if err := os.WriteFile(artifact, []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(base, "state")
	for _, id := range []string{"plan-a", "plan-b"} {
		if _, err := contextio.AcceptPlan(stateRoot, repo, id, artifact, []int{1}); err != nil {
			t.Fatal(err)
		}
	}
	for i, planID := range []string{"plan-a", "plan-b"} {
		taskID := planID + "-m1of1-p1c"
		args := map[string]any{"call": fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), "prompt": "pass", "cwd": repo, "taskId": taskID}
		launched := false
		_, _, err := Start(stateRoot, args, StartOptions{
			StateRoot:  stateRoot,
			Validation: StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }},
			Launch: func(jobID string, _ int64, _ string) error {
				launched = true
				plan, e := contextio.ReadPlanForTask(stateRoot, repo, planID)
				if e != nil || len(plan.LinkedJobs) != 1 || plan.LinkedJobs[0] != jobID || plan.PendingNextAction != "apply" {
					t.Fatalf("owning plan %s was not bound before launch: %+v err=%v", planID, plan, e)
				}
				other, e := contextio.ReadPlanForTask(stateRoot, repo, map[bool]string{true: "plan-b", false: "plan-a"}[planID == "plan-a"])
				otherHasJob := false
				if other != nil {
					for _, linkedJob := range other.LinkedJobs {
						otherHasJob = otherHasJob || linkedJob == jobID
					}
				}
				if e != nil || otherHasJob {
					t.Fatalf("job for %s was linked to sibling plan: %+v err=%v", planID, other, e)
				}
				return nil
			},
			Await: func(id string, _ *Record) map[string]any {
				return jobResult(map[string]any{"jobId": id, "state": "QUEUED"})
			},
		})
		if err != nil || !launched {
			t.Fatalf("Start for %s launched=%v err=%v", planID, launched, err)
		}
	}
}

func TestStartLaunchesWhenPlanReadFails(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitStart(t, repo, "init", "-q")
	artifact := filepath.Join(base, "plan.md")
	if err := os.WriteFile(artifact, []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(base, "state")
	if _, err := contextio.AcceptPlan(stateRoot, repo, "plan-1", artifact, []int{1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"call": "00000000-0000-4000-8000-000000000002", "prompt": "pass", "cwd": repo, "taskId": "plan-1-m1of1-p1"}
	launched := false
	_, _, err := Start(stateRoot, args, StartOptions{
		StateRoot:  stateRoot,
		Validation: StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }},
		Launch: func(string, int64, string) error {
			launched = true
			return nil
		},
		Await: func(id string, _ *Record) map[string]any {
			return jobResult(map[string]any{"jobId": id, "state": "QUEUED"})
		},
	})
	if err != nil || !launched {
		t.Fatalf("Start did not launch after plan read failure: launched=%v err=%v", launched, err)
	}
}

func replayStart(t *testing.T, c startCase) {
	state := t.TempDir()
	expand := map[string]string{"<ROOT>": state}
	normalize := map[string]string{state: "<ROOT>"}
	if c.Name == "reply_unapplied" {
		if e := os.MkdirAll(filepath.Join(state, "old-worktree"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if c.GitCase {
		repo := filepath.Join(state, "repo")
		wt := filepath.Join(state, "worktree")
		_ = os.MkdirAll(repo, 0700)
		gitStart(t, repo, "init", "-b", "main")
		gitStart(t, repo, "config", "user.name", "Oracle")
		gitStart(t, repo, "config", "user.email", "oracle@example.invalid")
		if e := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0600); e != nil {
			t.Fatal(e)
		}
		gitStart(t, repo, "add", "base.txt")
		gitStart(t, repo, "commit", "-m", "base")
		oldHead := gitStart(t, repo, "rev-parse", "HEAD")
		oldTree := gitStart(t, repo, "rev-parse", "HEAD^{tree}")
		gitStart(t, repo, "worktree", "add", "--detach", wt, "HEAD")
		if e := os.WriteFile(filepath.Join(wt, "applied.txt"), []byte("already applied\n"), 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(repo, "applied.txt"), []byte("already applied\n"), 0600); e != nil {
			t.Fatal(e)
		}
		gitStart(t, repo, "add", "applied.txt")
		gitStart(t, repo, "commit", "-m", "apply")
		head := gitStart(t, repo, "rev-parse", "HEAD")
		tree := gitStart(t, repo, "rev-parse", "HEAD^{tree}")
		expand["<ROOT>"] = state
		expand["<TREE>"] = tree
		expand["<HEAD>"] = head
		expand["<OLD_TREE>"] = oldTree
		expand["<OLD_HEAD>"] = oldHead
		normalize[tree] = "<TREE>"
		normalize[head] = "<HEAD>"
		normalize[oldTree] = "<OLD_TREE>"
		normalize[oldHead] = "<OLD_HEAD>"
	}
	dir := filepath.Join(state, "codex_exec_mcp", "jobs")
	_ = os.MkdirAll(dir, 0700)
	seed := replaceStart(c.Seed, expand).([]map[string]any)
	for _, s := range seed {
		b, _ := json.Marshal(s)
		if e := os.WriteFile(filepath.Join(dir, s["jobId"].(string)+".json"), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if c.Thread != nil {
		thread := replaceStart(c.Thread, expand).(map[string]any)
		if e := SaveRecord(filepath.Join(state, "codex_exec_mcp", "threads"), thread); e != nil {
			t.Fatal(e)
		}
		if c.Retiring {
			if _, e := retention.MarkWorktreeRetiring(state, thread["worktreePath"].(string)); e != nil {
				t.Fatal(e)
			}
		}
	}
	launched := []map[string]any{}
	decisions := []map[string]any{}
	opt := StartOptions{StateRoot: state, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }}, RecordDecision: func(j map[string]any, d, k, s string) {
		decisions = append(decisions, map[string]any{"jobId": j["jobId"], "decision": d, "kind": k, "sourceJobId": s})
	}, Launch: func(id string, g int64, _ string) error {
		launched = append(launched, map[string]any{"jobId": id, "generation": g, "token": "TOKEN"})
		return nil
	}, Await: func(id string, _ *Record) map[string]any {
		return jobResult(map[string]any{"jobId": id, "state": "QUEUED", "acceptedAt": 10, "deadlineAt": 20})
	}}
	var got map[string]any
	calls := replaceStart(c.Calls, expand).([]map[string]any)
	for _, a := range calls {
		var e error
		got, _, e = Start(state, a, opt)
		if e != nil {
			t.Fatal(e)
		}
	}
	files := map[string]any{}
	entries, _ := os.ReadDir(dir)
	for _, p := range entries {
		if p.IsDir() || filepath.Ext(p.Name()) == ".owner-lock" {
			files[p.Name()] = true
		} else {
			b, _ := os.ReadFile(filepath.Join(dir, p.Name()))
			var v any
			_ = json.Unmarshal(b, &v)
			files[p.Name()] = normalizeStart(v, normalize)
		}
	}
	if len(files) == 0 {
		files = nil
	}
	if !sameStart(normalizeStart(got, normalize), c.Result) || !sameStart(files, c.Files) || !sameStart(launched, c.Launches) || !sameStart(decisions, c.Decisions) {
		t.Fatalf("start parity mismatch: %#v files=%#v decisions=%#v", got, files, decisions)
	}
}

func gitStart(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v: %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}
func replaceStart(v any, replacements map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			x[k] = replaceStart(y, replacements)
		}
		return x
	case []any:
		for i := range x {
			x[i] = replaceStart(x[i], replacements)
		}
		return x
	case []map[string]any:
		for i := range x {
			x[i] = replaceStart(x[i], replacements).(map[string]any)
		}
		return x
	case string:
		for from, to := range replacements {
			x = strings.ReplaceAll(x, from, to)
		}
		return x
	}
	return v
}

func normalizeStart(v any, r map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			if k == "acceptedAt" {
				x[k] = "TIME"
			} else if k == "deadlineAt" {
				x[k] = "DEADLINE"
			} else if k == "signalToken" || k == "ownershipToken" {
				x[k] = "TOKEN"
			} else {
				x[k] = normalizeStart(y, r)
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalizeStart(x[i], r)
		}
		return x
	case string:
		for from, to := range r {
			x = strings.ReplaceAll(x, from, to)
		}
		return x
	}
	return v
}
func sameStart(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
