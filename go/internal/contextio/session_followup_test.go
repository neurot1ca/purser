package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/waiter"
)

func TestFollowupWaiterInstructions(t *testing.T) {
	root := t.TempDir()
	call, session, tool, project := "call", "session", "tool", "project"
	jobID := hashText(call)
	callKey := hashText(session + "\x00" + tool)
	write := func(path string, value map[string]any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "async_jobs", jobID+".json"), map[string]any{
		"jobId": jobID, "call": call, "call_key": callKey, "project_key": project, "session_id": session,
	})
	write(filepath.Join(root, "pending", callKey+".json"), map[string]any{
		"job_id": jobID, "async_call": call, "session_id": session, "project_key": project,
		"tool_use_id": tool, "task_id": "task",
	})
	write(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"), map[string]any{
		"jobId": jobID, "call": call, "state": "RUNNING",
	})

	if got := followupHints(root, project, session); !strings.Contains(got,
		"Start "+waiter.Command(jobID)+" in the background (use the PowerShell tool on Windows); call codex_result when it finishes, without adopt or asking the user.") {
		t.Fatalf("running job hint = %q", got)
	}
	if got := stallInstruction(jobID, nil); !strings.Contains(got,
		"Restart "+waiter.Command(jobID)+" in the background (use the PowerShell tool on Windows); continue waiting.") {
		t.Fatalf("stall instruction = %q", got)
	}
}
