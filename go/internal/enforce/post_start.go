package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/waiter"
)

// postStart mirrors codex._post_start. It is intentionally not connected to
// post.go while the Post lane is being ported in tranches.
func postStart(event map[string]any, root string) (map[string]any, error) {
	field := "updatedToolOutput"
	if event["hook_event_name"] != "PostToolUse" || !strings.HasSuffix(stringValue(event["tool_name"]), "__codex_start") {
		return map[string]any{}, nil
	}
	callKeyValue, err := callKey(event)
	if err != nil {
		return postStartFailure(event, root, nil, "tool_use_id is missing"), nil
	}
	pendingPath := filepath.Join(root, "pending", callKeyValue+".json")
	if _, err = os.Stat(filepath.Join(root, "pending", "undelivered-"+callKeyValue+".json")); err == nil {
		return postStartFailure(event, root, nil, "PreToolUse input rewrite was not delivered"), nil
	}
	pending, err := readAsyncObject(pendingPath)
	if err != nil {
		return postStartFailure(event, root, nil, "pending call record is missing"), nil
	}
	call, callOK := pending["async_call"].(string)
	if pending["async_ready"] != true || !callOK || call == "" {
		return postStartFailure(event, root, pending, "codex_start has no enforced pending call"), nil
	}
	values := decodedCodexObjects(event["tool_response"])
	if len(values) == 0 {
		return postStartFailure(event, root, pending, "codex_start result is unreadable"), nil
	}
	value := values[0]
	jobID := stringValue(value["jobId"])
	sum := sha256.Sum256([]byte(call))
	expected := hex.EncodeToString(sum[:])
	state := stringValue(value["state"])
	hostLaunchUnconfirmed := state == "FAILED" && value["errorCode"] == "HOST_LAUNCH_UNCONFIRMED"
	if configField, ok := event["post_tool_output_field"].(string); ok && (configField == "updatedToolOutput" || configField == "updatedMCPToolOutput") {
		field = configField
	}
	if jobID == "" || jobID != expected || (state != "QUEUED" && state != "RUNNING" && state != "SUCCEEDED" && !hostLaunchUnconfirmed) || codexResponseIsError(event["tool_response"]) {
		message := stringValue(value["errorCode"])
		if message == "" {
			message = "codex_start failed or returned a mismatched job"
		}
		return postStartFailure(event, root, pending, message), nil
	}
	if prior, ok := pending["job_id"]; ok && prior != nil && prior != jobID {
		return postStartFailure(event, root, pending, "pending call already belongs to another job"), nil
	}
	if pending["job_id"] == nil {
		pending["job_id"] = jobID
		if err = atomicObservationJSON(pendingPath, mustJSON(pending)); err != nil {
			return postStartFailure(event, root, pending, "pending call write failed"), nil
		}
	}
	index := map[string]any{"jobId": jobID, "call": call, "call_key": callKeyValue,
		"session_id": pending["session_id"], "project_key": pending["project_key"]}
	encoded, err := canonicalJSON(index)
	if err != nil || atomicObservationJSON(filepath.Join(root, "async_jobs", jobID+".json"), encoded) != nil {
		return postStartFailure(event, root, pending, "async job index write failed"), nil
	}
	if hostLaunchUnconfirmed {
		owner := stringValue(pending["lease_owner"])
		project := stringValue(pending["project_key"])
		if owner != "" && project != "" {
			_ = lease.New(filepath.Join(root, "projects", project)).Release(owner, float64(time.Now().UnixNano())/1e9)
		}
		textValue, _ := canonicalJSON(map[string]any{"jobId": jobID, "state": state,
			"errorCode": value["errorCode"], "message": value["message"]})
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse",
			field: []any{map[string]any{"type": "text", "text": string(textValue)}}}}, nil
	}
	mode := "background"
	if strings.HasPrefix(os.Getenv("CLAUDE_CODE_ENTRYPOINT"), "sdk-") {
		mode = "foreground"
	}
	next := fmt.Sprintf("Run in the %s (use the PowerShell tool on Windows): %s ; if it prints STILL_WAITING, run it again; otherwise call codex_result with this jobId.", mode, waiter.Command(jobID))
	textValue, _ := canonicalJSON(map[string]any{"jobId": jobID, "state": state, "next": next})
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse",
		field: []any{map[string]any{"type": "text", "text": string(textValue)}}}}, nil
}

func codexResponseIsError(response any) bool {
	if envelope, ok := response.(map[string]any); ok && envelope["isError"] == true {
		return true
	}
	for _, value := range decodedCodexObjects(response) {
		if value["isError"] == true {
			return true
		}
	}
	return false
}

func postStartFailure(event map[string]any, root string, pending map[string]any, reason string) map[string]any {
	key, keyErr := callKey(event)
	if keyErr != nil {
		key = "unknown"
	}
	call := stringValue(pending["async_call"])
	jobID := ""
	if call != "" {
		sum := sha256.Sum256([]byte(call))
		jobID = hex.EncodeToString(sum[:])
	}
	terminalOrAbsent := true
	if jobID != "" {
		job, err := readAsyncObject(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"))
		if err == nil {
			terminalOrAbsent = job["jobId"] == jobID && job["call"] == call &&
				(job["state"] == "SUCCEEDED" || job["state"] == "FAILED" || job["state"] == "CANCELLED" || job["state"] == "TIMED_OUT")
		} else if !errors.Is(err, os.ErrNotExist) {
			terminalOrAbsent = false
		}
	}
	if pending != nil && jobID != "" && !terminalOrAbsent {
		pending["start_rejected"] = true
		if data, err := canonicalJSON(pending); err == nil {
			_ = atomicObservationJSON(filepath.Join(root, "pending", key+".json"), data)
		}
	} else if pending != nil {
		if owner := stringValue(pending["lease_owner"]); owner != "" {
			project := stringValue(pending["project_key"])
			if project == "" {
				project = "unresolved"
			}
			_ = lease.New(filepath.Join(root, "projects", project)).Release(owner, float64(time.Now().UnixNano())/1e9)
		}
		_ = os.Remove(filepath.Join(root, "pending", key+".json"))
	}
	if indexedID := stringValue(pending["job_id"]); asyncHash.MatchString(indexedID) {
		_ = os.Remove(filepath.Join(root, "async_jobs", indexedID+".json"))
	}
	evidencePath := filepath.Join(root, "raw_tool_outputs", key+"-failure.json")
	_ = os.MkdirAll(filepath.Dir(evidencePath), 0700)
	evidence, _ := canonicalJSON(map[string]any{"ambiguous_outcome": true, "work_performed": "UNKNOWN", "error": reason, "pending_call": pending})
	_ = atomicObservationJSON(evidencePath, evidence)
	context := "Codex failed with an ambiguous outcome; work_performed is UNKNOWN, FULL Claude review is required, and evidence is at " + evidencePath + "."
	result := []byte(`{"state": "FAILED", "ambiguous_outcome": true, "claude_review_mode": "FULL"}`)
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse",
		"additionalContext": context, "updatedToolOutput": []any{map[string]any{"type": "text", "text": string(result)}}}}
}
