package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

var asyncHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

type asyncOwnershipError struct {
	kind, message string
}

func (e asyncOwnershipError) Error() string { return e.message }

func asyncOwned(root string, event map[string]any, repoRoot string) (string, map[string]any, map[string]any, map[string]any, error) {
	input, _ := event["tool_input"].(map[string]any)
	jobID, _ := input["jobId"].(string)
	unknown := func() error { return asyncOwnershipError{"WiringError", "unknown async job"} }
	notOwned := func() error { return asyncOwnershipError{"WiringError", "job is not owned by this session/project"} }
	if !asyncHash.MatchString(jobID) {
		return "", nil, nil, nil, unknown()
	}
	path := filepath.Join(root, "async_jobs", jobID+".json")
	index, err := readAsyncObject(path)
	if err != nil {
		return "", nil, nil, nil, unknown()
	}
	call, callOK := index["call"].(string)
	project, projectOK := index["project_key"].(string)
	callKeyValue, callKeyOK := index["call_key"].(string)
	session, sessionOK := index["session_id"].(string)
	if index["jobId"] != jobID || !projectOK || project == "" || !callKeyOK || !asyncHash.MatchString(callKeyValue) || !sessionOK || session == "" || !callOK {
		return "", nil, nil, nil, notOwned()
	}
	sum := sha256.Sum256([]byte(call))
	if hex.EncodeToString(sum[:]) != jobID {
		return "", nil, nil, nil, notOwned()
	}
	var pending, original map[string]any
	if index["stage"] != "CONSUMED" {
		pending, err = readAsyncObject(filepath.Join(root, "pending", callKeyValue+".json"))
		if err != nil {
			return "", nil, nil, nil, unknown()
		}
		originID, ok := pending["tool_use_id"].(string)
		if !ok || originID == "" {
			return "", nil, nil, nil, unknown()
		}
		original = make(map[string]any, len(event)+2)
		for key, value := range event {
			original[key] = value
		}
		original["tool_name"], original["session_id"], original["tool_use_id"] = "mcp__codex-main__codex", session, originID
		computed, keyErr := callKey(original)
		if keyErr != nil {
			return "", nil, nil, nil, unknown()
		}
		taskID, taskOK := pending["task_id"].(string)
		_, baselineOK := pending["baseline"].(map[string]any)
		if computed != callKeyValue || pending["job_id"] != jobID || pending["async_call"] != call || pending["session_id"] != session || pending["project_key"] != project || !taskOK || taskID == "" || !baselineOK {
			return "", nil, nil, nil, asyncOwnershipError{"WiringError", "job and pending call do not match"}
		}
	}
	eventSession, _ := event["session_id"].(string)
	if session != eventSession {
		var explicitRoot *string
		if repoRoot != "" {
			explicitRoot = &repoRoot
		}
		resolvedRoot, _ := resolveProjectRoot(event, explicitRoot)
		if resolvedRoot == nil || project != projectKey(resolvedRoot) || event["tool_name"] != "mcp__codex-main__codex_result" && event["tool_name"] != "mcp__plugin_purser_codex-main__codex_result" {
			return "", nil, nil, nil, notOwned()
		}
		if index["stage"] != "CONSUMED" && asyncJob(root, index) == nil {
			return "", nil, nil, nil, asyncOwnershipError{"WiringError", "job and durable result do not match"}
		}
		if input["adopt"] != true {
			return "", nil, nil, nil, asyncOwnershipError{"AsyncAdoptionRequired", "user consent is required for adoption"}
		}
	}
	return path, index, pending, original, nil
}

func asyncJob(root string, index map[string]any) map[string]any {
	jobID, idOK := index["jobId"].(string)
	call, callOK := index["call"].(string)
	if !idOK || !callOK {
		return nil
	}
	job, err := readAsyncObject(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"))
	if err != nil || job["jobId"] != jobID || job["call"] != call {
		return nil
	}
	return job
}

func readAsyncObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil || value == nil {
		return nil, errors.New("invalid async object")
	}
	return value, nil
}
