package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexHookPortMechanicalOpenCodeCandidateSelection(t *testing.T) {
	config := map[string]any{"coder_agent": "codex", "mechanical_opencode_enabled": true}
	candidate := func(id string, passed int) any {
		return map[string]any{"id": id, "status": "unmeasured", "benchmarks": map[string]any{
			"1": map[string]any{"passed_count": passed, "total": 6},
		}}
	}
	for _, tc := range []struct {
		name       string
		candidates []any
		wantAgent  string
		wantModel  string
	}{
		{name: "qualified score binds candidate", candidates: []any{candidate("opencode/nemotron-3-ultra-free", 5)}, wantAgent: "opencode", wantModel: "opencode/nemotron-3-ultra-free"},
		{name: "below threshold keeps configured coder", candidates: []any{candidate("vendor/weak", 4)}, wantAgent: "codex", wantModel: "gpt-6-luna"},
		{name: "empty inventory keeps configured coder", wantAgent: "codex", wantModel: "gpt-6-luna"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectCoder("mechanical", "gpt-6-luna", "high", "new_task", true, false,
				config, nil, nil, nil, tc.candidates, false)
			if got.Agent != tc.wantAgent || got.Model != tc.wantModel {
				t.Fatalf("selection = %#v, want agent=%q model=%q", got, tc.wantAgent, tc.wantModel)
			}
		})
	}

	optOut := map[string]any{"coder_agent": "codex", "mechanical_opencode_enabled": false}
	if got := SelectCoder("mechanical", "gpt-6-luna", "high", "new_task", true, false,
		optOut, nil, nil, nil, []any{candidate("vendor/seeded", 6)}, false); got.Agent != "codex" || got.Model != "gpt-6-luna" {
		t.Fatalf("opt-out selection = %#v", got)
	}
	if got := SelectCoder("normal", "gpt-6-sol", "medium", "new_task", true, false,
		config, nil, nil, nil, []any{candidate("vendor/seeded", 6)}, false); got.Agent != "codex" || got.Model != "gpt-6-sol" {
		t.Fatalf("nonmechanical selection = %#v", got)
	}
	configuredOpenCode := map[string]any{"coder_agent": "opencode"}
	if got := SelectCoder("mechanical", "provider/global", "none", "new_task", true, false,
		configuredOpenCode, nil, nil, nil, nil, false); got.Agent != "opencode" || got.Model != "provider/global" {
		t.Fatalf("empty candidates changed configured OpenCode binding: %#v", got)
	}
	if got := SelectCoder("mechanical", "gpt-6-luna", "high", "", false, true,
		config, nil, map[string]any{"agent": "codex", "model": "thread-model", "effort": "high"}, nil,
		[]any{candidate("vendor/seeded", 6)}, false); got.Model != "thread-model" {
		t.Fatalf("reply changed recorded model: %#v", got)
	}
	if got := SelectCoder("normal", "provider/global", "none", "continue", false, true,
		configuredOpenCode, map[string]any{}, map[string]any{"agent": "codex", "model": "foreign-model"}, nil, nil, false); got.Code != "THREAD_AGENT_MISMATCH" {
		t.Fatalf("cross-agent reply was admitted: %#v", got)
	}
}

func TestCodexHookPortBoardStatesAndRetention(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1800000000, 0)
	writeJob := func(id, label, state string, accepted float64, values map[string]any, bundle map[string]any) {
		t.Helper()
		call, callKey := "call-"+id, "key-"+id
		indexDir := filepath.Join(root, "async_jobs")
		jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
		if err := os.MkdirAll(indexDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(jobDir, 0700); err != nil {
			t.Fatal(err)
		}
		write := func(path string, value any) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(indexDir, id+".json"), map[string]any{"project_key": "project", "jobId": id, "call": call, "call_key": callKey})
		job := map[string]any{"jobId": id, "call": call, "state": state, "acceptedAt": accepted, "label": label,
			"model": "gpt-6-sol", "effort": "medium", "route": "normal", "boardDisplayCount": json.Number("0")}
		for key, value := range values {
			job[key] = value
		}
		write(filepath.Join(jobDir, id+".json"), job)
		if bundle != nil {
			bundleDir := filepath.Join(root, "bundle_artifacts")
			if err := os.MkdirAll(bundleDir, 0700); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(bundleDir, callKey+".json"), bundle)
		}
	}
	writeJob(strings.Repeat("a", 64), "stalled", "RUNNING", float64(now.Unix()-120), map[string]any{
		"errorCode": "STALLED", "stallDetectedAt": float64(now.Unix() - 30), "startedAt": float64(now.Unix() - 30), "etaMinutes": json.Number("5"),
	}, nil)
	writeJob(strings.Repeat("b", 64), "unlabelled", "RUNNING", float64(now.Unix()), map[string]any{"label": ""}, nil)
	writeJob(strings.Repeat("c", 64), "no-change", "SUCCEEDED", float64(now.Unix()-30), nil, map[string]any{
		"phase": "implementation", "apply": "codex_apply", "changed_paths": []any{},
	})
	writeJob(strings.Repeat("d", 64), "pending", "SUCCEEDED", float64(now.Unix()-20), nil, map[string]any{
		"phase": "implementation", "apply": "codex_apply", "changed_paths": []any{"file.go"},
	})
	writeJob(strings.Repeat("e", 64), "expired", "FAILED", float64(now.Unix()-4000), map[string]any{
		"finishedAt": float64(now.Unix() - 4000),
	}, nil)
	writeJob(strings.Repeat("f", 64), "already-shown", "SUCCEEDED", float64(now.Unix()-10), map[string]any{
		"finishedAt": float64(now.Unix() - 10), "boardDisplayCount": json.Number("2"),
	}, nil)
	writeJob(strings.Repeat("1", 64), "default-eta", "RUNNING", float64(now.Unix()-10), map[string]any{
		"startedAt": float64(now.Unix() - 10),
	}, nil)
	writeJob(strings.Repeat("2", 64), "mechanical-eta", "RUNNING", float64(now.Unix()), map[string]any{
		"route": "mechanical", "startedAt": float64(now.Unix()),
	}, nil)
	writeJob(strings.Repeat("3", 64), "queued-old", "QUEUED", float64(now.Unix()-3600), map[string]any{
		"etaMinutes": json.Number("5"),
	}, nil)

	board, ids := PostProjectJobBoard(root, "project", now)
	for _, want := range []string{
		"| stalled | gpt-6-sol (medium) | stalled | ~5 min |",
		"| no-change | gpt-6-sol (medium) | done | — | — | no change |",
		"| pending | gpt-6-sol (medium) | in review | — | — | — |",
		"| default-eta | gpt-6-sol (medium) | coding | ~5 min |",
		"| mechanical-eta | gpt-6-sol (medium) | coding | ~3 min |",
		"| queued-old | gpt-6-sol (medium) | queued | — |",
	} {
		if !strings.Contains(board, want) {
			t.Errorf("board lacks %q:\n%s", want, board)
		}
	}
	for _, absent := range []string{"unlabelled", "expired", "already-shown"} {
		if strings.Contains(board, absent) {
			t.Errorf("board unexpectedly includes %q:\n%s", absent, board)
		}
	}
	if len(ids) != 6 {
		t.Fatalf("visible job ids = %v, want 6", ids)
	}
}
