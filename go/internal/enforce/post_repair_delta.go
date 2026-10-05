package enforce

import (
	"fmt"

	"purser-hook/internal/ledger"
)

// recomputePostRepairDelta mirrors codex.post_tool_use's post-repair block.
// It uses the shared post delta assembler, while applying only fields that
// Python replaces at each success/failure boundary.
func recomputePostRepairDelta(repoRoot string, pending, bundle map[string]any, protectedPaths []string,
	foreignOwners []map[string]any, repaired bool) map[string]any {
	scratch := baseBundle("")
	assembled, err := assemblePostDelta(repoRoot, pending, scratch, protectedPaths, foreignOwners)
	if err != nil {
		delta := map[string]any{"status": "UNKNOWN", "code": "POST_REPAIR_DELTA_FAILED",
			"message": fmt.Sprintf("Post-repair delta could not be computed: %v", err), "changed_paths": nil}
		bundle["force_full_review"] = true
		bundle["findings"] = appendPostFinding(bundle["findings"], "POST_REPAIR_DELTA_FAILED",
			"Post-repair delta could not be computed; forcing FULL Claude review.")
		if repaired {
			applyPostRepairFallbackScope(repoRoot, pending, bundle, protectedPaths)
			setPostRepairStatus(bundle, "FAILED")
		}
		return delta
	}
	delta, _ := assembled["delta"].(map[string]any)
	if delta["status"] != ledger.OK {
		bundle["force_full_review"] = true
		code, _ := delta["code"].(string)
		if code == "" {
			code = "POST_REPAIR_DELTA_FAILED"
		}
		bundle["findings"] = appendPostFinding(bundle["findings"], code,
			"Post-repair delta could not be computed; forcing FULL Claude review.")
		if repaired {
			applyPostRepairFallbackScope(repoRoot, pending, bundle, protectedPaths)
			setPostRepairStatus(bundle, "FAILED")
		}
		return delta
	}

	// Python refreshes these post-repair fields, but deliberately leaves the
	// original changed_path_count and baseline_head untouched.
	bundle["whole_tree_delta"] = map[string]any{"changed_paths": delta["changed_paths"],
		"changed_file_count": delta["changed_file_count"], "changed_line_count": delta["changed_line_count"]}
	for _, key := range []string{"whole_tree_anomaly", "changed_paths", "delta_lines", "delta_files",
		"current_head", "protected_paths_touched", "concurrent_writer"} {
		if value, exists := assembled[key]; exists {
			bundle[key] = value
		}
	}
	if scope := postScope(pending); len(scope) > 0 {
		if owner, ok := pending["lease_owner"].(string); ok && owner != "" {
			bundle["known_concurrent_scopes"] = assembled["known_concurrent_scopes"]
		}
	}
	// assemblePostDelta calculates findings and force from the initial scope
	// check. Python defers applying that check until repair succeeded.
	if repaired {
		anomaly, _ := assembled["whole_tree_anomaly"].(map[string]any)
		if anomaly != nil && anomaly["status"] == ledger.OK {
			known := postKnownPaths(bundle["known_concurrent_scopes"])
			paths, _ := postStringList(anomaly["paths"])
			filtered := make([]string, 0, len(paths))
			for _, path := range paths {
				if !known[path] {
					filtered = append(filtered, path)
				}
			}
			anomaly["paths"] = filtered
		}
		force, _ := bundle["force_full_review"].(bool)
		bundle["force_full_review"] = applyOutOfScopeResult(bundle, anomaly, protectedPaths) || force
	}
	return delta
}

func applyPostRepairFallbackScope(repoRoot string, pending, bundle map[string]any, protectedPaths []string) {
	baseline, _ := pending["baseline"].(map[string]any)
	anomaly := ledger.DetectOutOfScopeModifications(repoRoot, baseline, postScope(pending))
	if anomaly["status"] == ledger.OK {
		known := postKnownPaths(bundle["known_concurrent_scopes"])
		paths, _ := postStringList(anomaly["paths"])
		filtered := make([]string, 0, len(paths))
		for _, path := range paths {
			if !known[path] {
				filtered = append(filtered, path)
			}
		}
		anomaly["paths"] = filtered
	}
	if applyOutOfScopeResult(bundle, anomaly, protectedPaths) {
		bundle["force_full_review"] = true
	}
}

func setPostRepairStatus(bundle map[string]any, status string) {
	entry, ok := bundle["micro_repair"].(map[string]any)
	if ok {
		entry["status"] = status
	}
}

func postScope(pending map[string]any) []string {
	scope, _ := postStringList(pending["scope"])
	return scope
}
