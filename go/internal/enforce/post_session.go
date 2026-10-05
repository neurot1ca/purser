package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

const unappliedWorktreeRetentionSeconds = 2 * 60 * 60

// finishPostSession mirrors the durable tail of Python post_tool_use. It is
// intentionally not wired into post.go until the surrounding post lane moves.
func finishPostSession(root string, event, pending, bundle map[string]any, threadID string,
	repository string, unappliedWorktree bool, now float64) error {
	project := pendingProjectKey(pending)
	if project == "" {
		project = "unresolved"
	}
	taskID := stringValue(pending["task_id"])
	if pending["kind"] == "pretool_bypass" {
		if taskID != "" && stringValue(pending["route"]) != "" && stringValue(pending["model"]) != "" && stringValue(pending["effort"]) != "" {
			record := ledger.BuildSessionRecord(taskID, optionalThread(threadID), stringValue(pending["route"]),
				stringValue(pending["model"]), stringValue(pending["effort"]), map[string]any{
					"agent": pendingAgent(pending), "phase": pending["phase"], "scope": pendingScope(pending),
					"pending_call_baseline": nil, "last_outcome": bundle, "label": pending["label"],
				})
			if saved := ledger.Save(filepath.Join(root, "projects", project), record); saved.Status != ledger.OK {
				message := "Bypass outcome ledger update failed."
				if saved.Code == "LEDGER_RECORD_INVALID" {
					message = "The session record was rejected: INVALID_RECORD_STRUCTURE."
				} else if saved.Code == "LEDGER_WRITE_FAILED" {
					message = "The session record could not be written atomically."
				}
				appendPostLedgerFindingOnly(bundle, saved.Code, message)
			}
		}
		return removePostPending(root, event)
	}

	if taskID == "" {
		return errors.New("normal pending call identity is incomplete")
	}
	options := map[string]any{
		"agent": pendingAgent(pending), "phase": pending["phase"], "scope": pendingScope(pending),
		"pending_call_baseline": nil, "last_outcome": bundle,
		"task_objective": pending["task_objective"], "acceptance_criteria": pending["acceptance_criteria"],
		"task_contract_grounded": pending["task_contract_grounded"], "reason": pending["reason"],
		"profile_checks": pending["profile_checks"], "label": pending["label"],
	}
	if options["task_contract_grounded"] == nil {
		options["task_contract_grounded"] = false
	}
	if options["phase"] == "implementation" {
		if scope, ok := options["scope"].([]any); !ok || len(scope) == 0 {
			delete(options, "phase")
			delete(options, "scope")
		}
	}
	record := ledger.BuildSessionRecord(taskID, optionalThread(threadID), stringValue(pending["route"]),
		stringValue(pending["model"]), stringValue(pending["effort"]), options)
	saved := ledger.Save(filepath.Join(root, "projects", project), record)
	if saved.Status != ledger.OK {
		message := "Ledger update failed."
		if saved.Code == "LEDGER_WRITE_FAILED" {
			message = "The session record could not be written atomically."
		} else if saved.Code == "LEDGER_RECORD_INVALID" {
			message = "The session record was rejected: INVALID_RECORD_STRUCTURE."
		}
		appendPostLedgerFinding(bundle, saved.Code, message)
	} else if path, ok := threadIndexPath(root, threadID); ok && repository != "" {
		data, err := json.Marshal(map[string]any{"thread_id": threadID, "project_root": repository})
		if err != nil {
			return err
		}
		if err := atomicObservationJSON(path, data); err != nil {
			return err
		}
	}

	if owner := stringValue(pending["lease_owner"]); owner != "" {
		store := lease.New(filepath.Join(root, "projects", project))
		if unappliedWorktree {
			result := store.ExtendRetention(owner, now+unappliedWorktreeRetentionSeconds, now)
			if result.Status != "OK" && result.Code != "LEASE_OWNER_MISSING" {
				return errors.New(result.Code)
			}
		} else {
			result := store.Release(owner, now)
			if result.Status != "OK" {
				return errors.New(result.Code)
			}
		}
	}
	if unappliedWorktree && pending["async_call"] != nil && pending["async_call"] != false {
		return nil
	}
	return removePostPending(root, event)
}

func optionalThread(thread string) *string {
	if thread == "" {
		return nil
	}
	return &thread
}

func pendingAgent(pending map[string]any) any {
	if agent := pending["agent"]; agent != nil {
		return agent
	}
	return "codex"
}

func pendingScope(pending map[string]any) any {
	if scope := pending["scope"]; scope != nil {
		return scope
	}
	return []any{}
}

func appendPostLedgerFinding(bundle map[string]any, code, message string) {
	bundle["claude_review_mode"] = "FULL"
	appendPostLedgerFindingOnly(bundle, code, message)
}

func appendPostLedgerFindingOnly(bundle map[string]any, code, message string) {
	findings, _ := bundle["findings"].([]any)
	if code == "" {
		code = "LEDGER_WRITE_FAILED"
	}
	bundle["findings"] = append(findings, map[string]any{"code": code, "message": message})
}

func removePostPending(root string, event map[string]any) error {
	for _, path := range pendingCandidates(root, event) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
