package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/lease"
)

type postIntakeResult struct {
	Pending                         map[string]any
	Profile                         postProfile
	ThreadID, Report, RawOutputPath string
	Bundle                          map[string]any
}

type postWiringError string

func (err postWiringError) Error() string         { return string(err) }
func (err postWiringError) PostErrorKind() string { return "WiringError" }

func postIntake(event map[string]any, root string) (intake postIntakeResult, early map[string]any, err error) {
	tool := stringValue(event["tool_name"])
	if event["hook_event_name"] != "PostToolUse" || !codexTools[tool] ||
		strings.HasSuffix(tool, "__codex_start") || strings.HasSuffix(tool, "__codex_result") || strings.HasSuffix(tool, "__codex_cancel") {
		return intake, map[string]any{}, nil
	}
	root = defaultPreStateRoot(root)
	paths := pendingCandidates(root, event)
	pending, err := readPostPending(paths)
	repository := stringValue(pending["repo_root"])
	if repository == "" {
		resolved, _ := resolveProjectRoot(event, nil)
		repository = pointerValue(resolved)
	}
	loadedConfig, configErr := loadPreConfig(repository)
	if configErr != nil {
		return intake, nil, configErr
	}
	if err != nil {
		return intake, nil, err
	}
	markerPath, markerData, markerFound := readPostUndelivered(root, event)
	if markerFound {
		reason := stringValue(markerData["reason"])
		if reason == "" {
			reason = "adapter failed before rewritten input was delivered"
		}
		pending["kind"], pending["reason"] = "pretool_bypass", reason
	}
	if served := decodedCodexObjects(event["tool_response"]); len(served) > 0 {
		first := served[0]
		agent, model := stringValue(first["servedAgent"]), stringValue(first["servedModel"])
		if (agent == "codex" || agent == "opencode" || agent == "agy" || agent == "cursor") && model != "" {
			pending["agent"], pending["model"], pending["attempted_models"] = agent, model, first["attemptedModels"]
			if path, pathErr := pendingPath(root, event); pathErr == nil {
				if data, marshalErr := canonicalJSON(pending); marshalErr == nil {
					_ = atomicObservationJSON(path, data)
				}
			}
		}
	}
	rawPath, err := persistRawOutput(root, event, event["tool_response"])
	if err != nil {
		return intake, nil, err
	}
	threadID, report, decodeErr := decodeCodexResult(event["tool_response"])
	bundle := baseBundle("")
	bundle["implementer"] = stringValue(pending["agent"]) + "-main"
	if bundle["implementer"] == "-main" {
		bundle["implementer"] = "codex-main"
	}
	for target, source := range map[string]string{"task_id": "task_id", "phase": "phase", "route": "route", "model": "model", "effort": "effort"} {
		bundle[target] = pending[source]
	}
	bundle["reviewer1_model_requested"] = loadedConfig["reviewer1_model_requested"]
	bundle["raw_output_path"] = rawPath
	bundle["routing_enforced"] = pending["kind"] != "pretool_bypass"
	bundle["task_contract_grounded"] = pending["task_contract_grounded"]
	if bundle["task_contract_grounded"] == nil {
		bundle["task_contract_grounded"] = false
	}
	bundle["project_root_reason"] = pending["project_root_reason"]
	if decodeErr == nil {
		bundle["thread_id"], bundle["implementer_report"] = threadID, report
	} else {
		bundle["findings"] = append(bundle["findings"].([]any), map[string]any{"code": "THREAD_ID_UNRECOVERABLE", "message": decodeErr.Error()})
		bundle["claude_review_mode"] = "FULL"
	}
	profile := postProfileFromPending(pending, loadedConfig)
	intake = postIntakeResult{Pending: pending, Profile: profile, ThreadID: threadID, Report: report,
		RawOutputPath: rawPath, Bundle: bundle}
	if pending["kind"] != "pretool_bypass" {
		return intake, nil, nil
	}
	reason := stringValue(pending["reason"])
	if reason == "" {
		reason = "the PreToolUse bypass reason was unavailable"
	}
	bundle["findings"] = append(bundle["findings"].([]any), map[string]any{
		"code":    "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED",
		"message": "Routing was NOT enforced for this call and no usable baseline was captured. Internal PreToolUse reason: " + reason,
	})
	bundle["claude_review_mode"] = "FULL"
	for _, path := range paths {
		_ = os.Remove(path)
	}
	if markerFound {
		if owner := stringValue(pending["lease_owner"]); owner != "" {
			_ = lease.New(filepath.Join(root, "projects", pendingProjectKey(pending))).Release(owner, float64(time.Now().UnixNano())/1e9)
		}
		preEmit(root, event, nil, stringValue(pending["task_id"]), pendingProjectKey(pending),
			"ROUTING_PROTOCOL_VIOLATION", map[string]any{"violation_code": "CODEX_INPUT_REWRITE_UNDELIVERED"})
		_ = os.Remove(markerPath)
	}
	field := stringValue(loadedConfig["post_tool_output_field"])
	early, err = replacement(bundle, field)
	return intake, early, err
}

func pendingCandidates(root string, event map[string]any) []string {
	paths := []string{}
	if path, err := pendingPath(root, event); err == nil {
		paths = append(paths, path)
	}
	bypass := bypassPath(root, event)
	if len(paths) == 0 || paths[0] != bypass {
		paths = append(paths, bypass)
	}
	return paths
}

func readPostPending(paths []string) (map[string]any, error) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, errors.New("pending call record is unreadable")
		}
		var pending map[string]any
		if json.Unmarshal(data, &pending) != nil || pending == nil {
			return nil, errors.New("pending call record is invalid")
		}
		return pending, nil
	}
	return nil, postWiringError("pending call record is missing")
}

func readPostUndelivered(root string, event map[string]any) (string, map[string]any, bool) {
	key, err := callKey(event)
	if err != nil {
		return "", nil, false
	}
	path := filepath.Join(root, "pending", "undelivered-"+key+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, false
	}
	if err != nil {
		return path, map[string]any{}, true
	}
	var marker map[string]any
	if json.Unmarshal(data, &marker) != nil || marker == nil {
		marker = map[string]any{}
	}
	return path, marker, true
}

func pendingProjectKey(pending map[string]any) string {
	if project := stringValue(pending["project_key"]); project != "" {
		return project
	}
	if repository := stringValue(pending["repo_root"]); repository != "" {
		return projectKey(&repository)
	}
	return "unresolved"
}
