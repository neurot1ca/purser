package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"purser-hook/internal/review"
)

func nonReportBytes(bundle map[string]any) (int, error) {
	value := make(map[string]any, len(bundle))
	for key, item := range bundle {
		if key != "implementer_report" {
			value[key] = item
		}
	}
	encoded, err := canonicalJSON(value)
	return len(encoded), err
}

func copyFindings(value any, limit *int) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{}
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		finding, ok := item.(map[string]any)
		if !ok {
			continue
		}
		message, ok := finding["message"].(string)
		if !ok {
			message = ""
		}
		copy := map[string]any{"code": finding["code"], "message": message}
		if reference, ok := finding["reference"].(string); ok && reference != "" {
			copy["reference"] = reference
		}
		if limit != nil {
			runes := []rune(message)
			if len(runes) > *limit {
				copy["message"] = string(runes[:*limit])
				sum := sha256.Sum256([]byte(message))
				copy["message_char_count"] = len(runes)
				copy["message_sha256"] = hex.EncodeToString(sum[:])
			}
		}
		result = append(result, copy)
	}
	return result
}

// compactBundle follows Python's compact response shaping and mandatory-field floor.
func compactBundle(bundle map[string]any, cap int) (map[string]any, bool, error) {
	compact := make(map[string]any, len(bundle))
	for key, value := range bundle {
		compact[key] = value
	}
	for _, key := range []string{"hook_checks_full", "codex_quota", "whole_tree_delta", "whole_tree_anomaly", "known_concurrent_scopes", "_review_diff", "_review_job_id", "review_navigation"} {
		delete(compact, key)
	}
	if scopes, ok := bundle["known_concurrent_scopes"].([]any); ok && len(scopes) > 0 {
		ids := map[string]bool{}
		for _, value := range scopes {
			if item, ok := value.(map[string]any); ok {
				if id, ok := item["task_id"].(string); ok {
					ids[id] = true
				}
			}
		}
		ordered := make([]string, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		if len(ordered) > 5 {
			ordered = ordered[:5]
		}
		compact["known_concurrent_scopes"] = map[string]any{"count": len(scopes), "task_ids": ordered}
	}
	paths, pathsOK := bundle["changed_paths"].([]any)
	if strings, ok := bundle["changed_paths"].([]string); ok {
		paths, pathsOK = make([]any, len(strings)), true
		for i, path := range strings {
			paths[i] = path
		}
	}
	if pathsOK {
		encoded, err := canonicalJSON(paths)
		if err != nil {
			return nil, false, err
		}
		sum := sha256.Sum256(encoded)
		preview := paths
		if len(preview) > 10 {
			preview = preview[:10]
		}
		compact["changed_paths"] = preview
		compact["changed_path_count"] = len(paths)
		if digest, ok := bundle["changed_paths_sha256"].(string); ok && digest != "" {
			compact["changed_paths_sha256"] = digest
		} else {
			compact["changed_paths_sha256"] = hex.EncodeToString(sum[:])
		}
	}
	compact["findings"] = copyFindings(bundle["findings"], nil)
	if size, err := nonReportBytes(compact); err != nil {
		return nil, false, err
	} else if size <= cap {
		return compact, false, nil
	}

	findings, _ := compact["findings"].([]any)
	high := 0
	for _, value := range findings {
		if item, ok := value.(map[string]any); ok {
			if message, ok := item["message"].(string); ok && utf8.RuneCountInString(message) > high {
				high = utf8.RuneCountInString(message)
			}
		}
	}
	low, best := 0, map[string]any(nil)
	for low <= high {
		limit := (low + high) / 2
		candidate := make(map[string]any, len(compact))
		for key, value := range compact {
			candidate[key] = value
		}
		candidate["findings"] = copyFindings(bundle["findings"], &limit)
		size, err := nonReportBytes(candidate)
		if err != nil {
			return nil, false, err
		}
		if size <= cap {
			best, low = candidate, limit+1
		} else {
			high = limit - 1
		}
	}
	if best != nil {
		return best, false, nil
	}
	compact["findings"] = copyFindings(bundle["findings"], intPointer(0))
	if paths, ok := compact["changed_paths"].([]any); ok {
		for len(paths) > 0 {
			paths = paths[:len(paths)-1]
			compact["changed_paths"] = paths
			if size, err := nonReportBytes(compact); err != nil {
				return nil, false, err
			} else if size <= cap {
				return compact, false, nil
			}
		}
	}
	planSummaryNotice := ""
	if summary, ok := bundle["plan_summary"].(string); ok && summary != "" {
		if planPath, ok := bundle["plan_path"].(string); ok && planPath != "" {
			planSummaryNotice = "Inline plan summary omitted; complete plan is available at " + planPath + "."
			candidate := make(map[string]any, len(compact))
			for key, value := range compact {
				candidate[key] = value
			}
			candidate["plan_summary"] = planSummaryNotice
			if size, err := nonReportBytes(candidate); err != nil {
				return nil, false, err
			} else if size <= cap {
				return candidate, false, nil
			}
		}
	}

	mandatory := map[string]any{}
	for _, key := range []string{"task_id", "implementer", "model", "effort", "thread_id", "routing_enforced", "claude_review_mode", "reviewer1_result", "reviewer1_skipped_reason", "project_root_reason", "protected_paths_touched", "concurrent_writer", "micro_repair_cleared_review", "validation", "hook_checks", "full_bundle_path", "plan_path", "plan_summary"} {
		mandatory[key] = bundle[key]
	}
	if planSummaryNotice != "" {
		mandatory["plan_summary"] = planSummaryNotice
	}
	mandatory["micro_repair"] = nil
	if repair, ok := bundle["micro_repair"].(map[string]any); ok {
		mandatory["micro_repair"] = map[string]any{"status": repair["status"]}
	}
	mandatory["audit_receipt"] = nil
	if audit, ok := bundle["audit_receipt"].(map[string]any); ok && audit["verdict"] != nil {
		mandatory["audit_receipt"] = map[string]any{"verdict": audit["verdict"]}
	}
	mandatory["changed_paths"] = []any{}
	mandatory["changed_path_count"] = compact["changed_path_count"]
	mandatory["changed_paths_sha256"] = compact["changed_paths_sha256"]
	mandatory["findings"] = copyFindings(bundle["findings"], intPointer(0))
	mandatory["implementer_report"] = bundle["implementer_report"]
	for _, key := range []string{"known_concurrent_scopes", "reviewer1_scope", "code_map"} {
		if value, ok := compact[key]; ok {
			mandatory[key] = value
		}
	}
	size, err := nonReportBytes(mandatory)
	return mandatory, size > cap, err
}

func stripWorkerValidation(bundle map[string]any) {
	report, ok := bundle["implementer_report"].(string)
	worker, workerOK := bundle["worker_validation"].(map[string]any)
	if !ok || !workerOK {
		return
	}
	lines := splitPythonLines(report)
	if len(lines) == 0 {
		return
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) >= 2 && strings.HasPrefix(last, "`") && strings.HasSuffix(last, "`") {
		last = strings.TrimSpace(last[1 : len(last)-1])
	}
	if !strings.HasPrefix(last, "VALIDATION:") || strings.Contains(stringValue(worker["summary"]), last) {
		return
	}
	reported := parseWorkerValidation(last)
	if reported["source"] == worker["source"] && reported["status"] == worker["status"] &&
		reported["summary"] == worker["summary"] &&
		postSameStrings(reported["commands"], worker["commands"]) &&
		postSameStrings(reported["artifacts"], worker["artifacts"]) &&
		postSameInts(reported["exit_codes"], worker["exit_codes"]) {
		bundle["implementer_report"] = strings.TrimSpace(strings.Join(lines[:len(lines)-1], "\n"))
	}
}

func postSameStrings(left, right any) bool {
	a, aOK := postStringList(left)
	b, bOK := postStringList(right)
	if !aOK || !bOK || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func postSameInts(left, right any) bool {
	a, aOK := left.([]any)
	b, bOK := right.([]any)
	if !aOK || !bOK || len(a) != len(b) {
		return false
	}
	for i := range a {
		if fmt.Sprint(a[i]) != fmt.Sprint(b[i]) {
			return false
		}
	}
	return true
}

func intPointer(value int) *int { return &value }

func finalizeBundle(root string, event map[string]any, bundle map[string]any, cap, reportLimit int) (map[string]any, string, error) {
	key, err := callKey(event)
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(root, "bundle_artifacts", key+".json")
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	bundle["full_bundle_path"] = absolute
	compact, overflow, err := compactBundle(bundle, cap)
	if err != nil {
		return nil, "", err
	}
	if overflow && bundle["claude_review_mode"] != "FULL" {
		bundle["claude_review_mode"], bundle["audit_receipt"] = "FULL", nil
		compact, _, err = compactBundle(bundle, cap)
		if err != nil {
			return nil, "", err
		}
	}
	if report, ok := compact["implementer_report"].(string); ok {
		runes := []rune(report)
		if len(runes) > reportLimit {
			compact["implementer_report"] = string(runes[:reportLimit])
		}
	}
	if bundle["claude_review_mode"] == review.Full && numberOr(bundle["delta_lines"], 0) > 500 {
		diff, available := bundle["_review_diff"].(string)
		if !available {
			prior := mapValue(bundle["review_navigation"])
			if raw, readErr := os.ReadFile(stringValue(prior["raw_diff_path"])); readErr == nil {
				index, indexed := review.NavigationIndex(string(raw))
				if indexed && index == prior["index"] {
					diff, available = string(raw), true
				}
			}
		}
		metadata := map[string]any{"presentation": "fallback", "status": "UNAVAILABLE"}
		if available {
			index, indexed := review.NavigationIndex(diff)
			sum := sha256.Sum256([]byte(diff))
			name := fmt.Sprintf("%x.diff", sum)
			if indexed {
				rawPath, pathErr := filepath.Abs(filepath.Join(root, "review_diffs", name))
				if pathErr == nil && atomicWrite(rawPath, []byte(diff)) == nil {
					metadata = map[string]any{"presentation": "navigator", "status": "READY", "raw_diff_path": rawPath, "index": index}
				}
			}
		}
		// The pilot uses stable call-key cohorts. Control retains its presentation.
		if metadata["status"] == "READY" && key[len(key)-1]%2 == 0 {
			metadata["presentation"] = "existing"
		}
		bundle["review_navigation"] = metadata

	}
	applyReviewNavigationPreview(compact, bundle)
	delete(bundle, "_review_diff")
	encoded, err := canonicalJSON(bundle)
	if err != nil {
		return nil, "", err
	}
	if err = atomicWrite(absolute, encoded); err != nil {
		return nil, "", err
	}
	return compact, absolute, nil
}

// Apply only at the final response boundary, after validation stripping and
// quota compaction. The entire index remains in the full artifact.
func applyReviewNavigationPreview(compact, bundle map[string]any) {
	metadata := mapValue(bundle["review_navigation"])
	if metadata == nil || metadata["presentation"] == "existing" {
		return
	}
	preview := "REVIEW NAVIGATION UNAVAILABLE; inspect the full change.\n"
	if metadata["status"] == "READY" {
		preview = "Review navigation; raw diff: " + stringValue(metadata["raw_diff_path"]) + "\n" + stringValue(metadata["index"])
	}
	report := stringValue(compact["implementer_report"])
	budget, _ := canonicalJSON(report)
	candidate := []rune(preview + report)
	if len(candidate) > len(budget) {
		candidate = candidate[:len(budget)]
	}
	for len(candidate) > 0 {
		encoded, _ := canonicalJSON(string(candidate))
		if len(encoded) <= len(budget) {
			break
		}
		candidate = candidate[:len(candidate)-1]
	}
	compact["implementer_report"] = string(candidate)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
