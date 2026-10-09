package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
)

const jobOutputLimit = 1_000_000
const jobRetentionSeconds = 7 * 24 * 60 * 60

var terminalJobs = map[string]bool{"SUCCEEDED": true, "FAILED": true, "CANCELLED": true, "TIMED_OUT": true, "UNKNOWN_OR_IN_PROGRESS": true}

type FinishOptions struct {
	ReleaseLease func(projectKey, callKey string)
	Generation   *int64
	Token        string
}
type ResultOptions struct {
	StateRoot      string
	RecoverJob     func(jobID string) string
	RecordDecision func(job map[string]any, decision, kind, sourceJobID string)
}

// Result returns the durable bridge result for a job.
func Result(stateRoot, jobID string, now float64, opt ResultOptions) map[string]any {
	return resultJob(filepath.Join(stateRoot, "codex_exec_mcp", "jobs"), jobID, now, opt)
}

// finishJob durably stores output before publishing a terminal record.
func finishJob(dir, id, state, code string, result map[string]any, message string, now float64, opt FinishOptions) error {
	lock, err := AcquireOwnership(dir, id)
	if err != nil {
		return err
	}
	defer lock.Release()
	path := filepath.Join(dir, id+".json")
	r, err := Read(path)
	if err != nil {
		return nil
	}
	if opt.Generation != nil {
		var generation int64
		var token string
		r.Get("generation", &generation)
		r.Get("ownershipToken", &token)
		if generation != *opt.Generation || token != opt.Token {
			return nil
		}
	}
	var got, current string
	if !r.Get("jobId", &got) || got != id || !r.Get("state", &current) || terminalJobs[current] {
		return nil
	}
	_ = r.Set("state", state)
	_ = r.Set("finishedAt", now)
	if terminalJobs[state] {
		r.Delete("stallDetectedAt")
		var old string
		if r.Get("errorCode", &old) && old == "STALLED" {
			r.Delete("errorCode")
		}
	}
	if code != "" {
		if message == "" {
			message = map[string]string{"CODEX_FAILED": "Codex failed", "DEADLINE_EXCEEDED": "Codex deadline exceeded", "CANCELLED": "Codex cancelled", "PROCESS_UNCONFIRMED": "Process termination unconfirmed", "RECOVERY_RESUME_FAILED": "Interrupted Codex thread could not be resumed", "ADMISSION_BASELINE_UNAVAILABLE": "Fresh admission baseline could not be bound; FULL review required", "WORKTREE_SNAPSHOT_FAILED": "Worktree snapshot could not be created", "WORKTREE_MISSING": "Job worktree is unavailable", "WORKTREE_SETUP_FAILED": "Worktree setup failed"}[code]
		}
		if code == "WORKTREE_SETUP_FAILED" {
			setup, _ := recordMap(r)["setupError"].(map[string]any)
			reason := "exited with code " + pythonString(setup["exitCode"])
			if truthy(setup["timedOut"]) {
				reason = "timed out"
			}
			message = "Worktree setup " + reason
			if output, ok := setup["output"]; ok && truthy(output) {
				message += ": " + pythonString(output)
			}
		}
		_ = r.Set("errorCode", code)
		_ = r.Set("message", message)
	}
	if result != nil {
		content, _ := result["content"].(string)
		data := []byte(content)
		if len(data) > jobOutputLimit {
			_ = r.Set("state", "FAILED")
			_ = r.Set("errorCode", "CODEX_FAILED")
			_ = r.Set("message", "Codex output exceeded limit")
		} else {
			output := id + ".output"
			if err := atomicWrite(filepath.Join(dir, output), data); err != nil {
				return err
			}
			_ = r.Set("threadId", result["threadId"])
			_ = r.Set("outputFile", output)
			if usage, ok := result["usage"].(map[string]any); ok {
				if normalized := codexUsage(usage); normalized != nil {
					_ = r.Set("usage", normalized)
				}
			}
			if notes, ok := result["notes"]; ok {
				_ = r.Set("notes", notes)
			}
			if models, ok := result["attemptedModels"]; ok {
				_ = r.Set("attemptedModels", models)
				_ = r.Set("servedAgent", result["servedAgent"])
				_ = r.Set("servedModel", result["servedModel"])
			}
		}
	}
	if terminalJobs[state] {
		var count any
		if !r.Get("boardDisplayCount", &count) {
			_ = r.Set("boardDisplayCount", 0)
		}
	}
	if err := Write(path, r); err != nil {
		return err
	}
	if leaseQueued, _ := recordMap(r)["leaseQueued"].(bool); leaseQueued {
		job := recordMap(r)
		if _, started := job["startedAt"]; !started && (state == "CANCELLED" || state == "TIMED_OUT" || code == "HOST_LAUNCH_UNCONFIRMED") {
			root := filepath.Dir(filepath.Dir(dir))
			if b, e := os.ReadFile(filepath.Join(root, "async_jobs", id+".json")); e == nil {
				var index map[string]any
				if json.Unmarshal(b, &index) == nil {
					project, _ := index["project_key"].(string)
					call, _ := index["call_key"].(string)
					if project != "" && call != "" {
						if opt.ReleaseLease != nil {
							opt.ReleaseLease(project, call)
						} else {
							lease.New(filepath.Join(root, "projects", project)).Release(call, now)
						}
					}
				}
			}
		}
	}
	if state != "UNKNOWN_OR_IN_PROGRESS" {
		_ = os.Remove(filepath.Join(dir, id+".launch"))
	}
	return nil
}

// resultJob reads a durable job record and returns the Python bridge result shape.
func resultJob(dir, id string, now float64, opt ResultOptions) map[string]any {
	r := readJob(dir, id)
	if r == nil {
		return finishResult(map[string]any{"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"}, "state", "errorCode", "message")
	}
	m := recordMap(r)
	state, _ := m["state"].(string)
	if state == "EXPIRED" {
		return expiredJobResult(m)
	}
	finished, ok := m["finishedAt"].(float64)
	if !ok {
		finished, _ = m["acceptedAt"].(float64)
	}
	if terminalJobs[state] && now-finished > jobRetentionSeconds {
		name, _ := m["outputFile"].(string)
		if name == "" {
			name = "unused"
		}
		_ = os.Remove(filepath.Join(dir, name))
		_ = r.Set("state", "EXPIRED")
		_ = r.Set("errorCode", "JOB_EXPIRED")
		_ = r.Set("message", "Job record expired")
		_ = Write(filepath.Join(dir, id+".json"), r)
		return expiredJobResult(recordMap(r))
	}
	if !terminalJobs[state] {
		root := opt.StateRoot
		if root == "" {
			root = filepath.Dir(filepath.Dir(dir))
		}
		hostState := "UNCONFIRMED"
		if opt.RecoverJob != nil {
			hostState = opt.RecoverJob(id)
		} else {
			hostState = RecoverJob(root, id, now, RecoverOptions{})
		}
		if current := readJob(dir, id); current != nil {
			r = current
			m = recordMap(r)
			state, _ = m["state"].(string)
		}
		if hostState == "UNCONFIRMED" {
			m["message"] = "Host identity unconfirmed"
			return finishResult(map[string]any{"jobId": m["jobId"], "state": m["state"], "message": m["message"]}, "jobId", "state", "message")
		}
	}
	if state == "SUCCEEDED" && !truthy(m["worktreePath"]) && !truthy(m["decisionRecorded"]) {
		lock, err := AcquireOwnership(dir, id)
		if err == nil {
			current := readJob(dir, id)
			if current != nil {
				job := recordMap(current)
				if job["state"] == "SUCCEEDED" && !truthy(job["worktreePath"]) && !truthy(job["decisionRecorded"]) && nonemptyString(job["taskId"]) != "" {
					if opt.RecordDecision != nil {
						opt.RecordDecision(job, "accepted", "result", id)
					} else {
						root := opt.StateRoot
						if root == "" {
							root = filepath.Dir(filepath.Dir(dir))
						}
						enforce.RecordClaudeDecision(root, job, "accepted", "result", id)
					}
					_ = current.Set("decisionRecorded", true)
					_ = Write(filepath.Join(dir, id+".json"), current)
					r, m = current, job
					m["decisionRecorded"] = true
				}
			}
			_ = lock.Release()
		}
	}
	value := map[string]any{"jobId": m["jobId"], "state": m["state"]}
	keys := []string{"jobId", "state"}
	for _, key := range []string{"attemptedModels", "servedAgent", "servedModel"} {
		if v, ok := m[key]; ok {
			value[key] = v
			keys = append(keys, key)
		}
	}
	if state == "SUCCEEDED" {
		name, nOK := m["outputFile"].(string)
		thread, tOK := m["threadId"].(string)
		b, err := os.ReadFile(filepath.Join(dir, name))
		if !nOK || !tOK || err != nil {
			value["state"] = "FAILED"
			value["errorCode"] = "OUTPUT_UNAVAILABLE"
			value["message"] = "Job output is unavailable"
			keys = append(keys, "errorCode", "message")
		} else {
			value["threadId"] = thread
			value["content"] = string(b)
			keys = append(keys, "threadId", "content")
			if usage, ok := m["usage"].(map[string]any); ok && len(usage) > 0 {
				value["usage"] = usage
				keys = append(keys, "usage")
			}
			if notes, ok := m["notes"]; ok {
				value["notes"] = notes
				keys = append(keys, "notes")
			}
		}
	} else if terminalJobs[state] {
		value["errorCode"] = m["errorCode"]
		if value["errorCode"] == nil {
			value["errorCode"] = "PROCESS_UNCONFIRMED"
		}
		value["message"] = m["message"]
		if value["message"] == nil {
			value["message"] = "Process state cannot be confirmed"
		}
		keys = append(keys, "errorCode", "message")
	}
	if v, ok := m["worktreePath"]; ok {
		value["worktreePath"] = v
		keys = append(keys, "worktreePath")
	}
	return finishResult(value, keys...)
}

func expiredJobResult(m map[string]any) map[string]any {
	return finishResult(map[string]any{"jobId": m["jobId"], "state": "FAILED", "errorCode": "JOB_EXPIRED", "message": "Job record expired"}, "jobId", "state", "errorCode", "message")
}

func finishResult(value map[string]any, keys ...string) map[string]any {
	result := jobResult(value)
	var text bytes.Buffer
	text.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			text.WriteString(", ")
		}
		k, _ := pythonJSON(key)
		v, _ := pythonJSON(value[key])
		text.Write(k)
		text.WriteString(": ")
		text.Write(v)
	}
	text.WriteByte('}')
	content := result["content"].([]any)
	content[0].(map[string]any)["text"] = text.String()
	return result
}

func pythonString(v any) string {
	if v == nil {
		return "None"
	}
	if b, ok := v.(bool); ok {
		if b {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}
