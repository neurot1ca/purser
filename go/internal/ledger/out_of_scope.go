package ledger

import (
	"path/filepath"
	"strings"
	"time"
)

// DetectOutOfScopeModifications mirrors Python ledger.detect_out_of_scope_modifications.
func DetectOutOfScopeModifications(repoRoot string, baseline map[string]any, scope []string) map[string]any {
	if len(scope) == 0 {
		return map[string]any{"status": "OK", "code": "NO_SCOPE_DECLARED", "paths": []string{}}
	}
	if baseline == nil || !baselineValid(baseline) {
		return map[string]any{"status": "UNKNOWN", "code": "BASELINE_INVALID", "message": "A valid baseline was not provided for out-of-scope inspection.", "paths": []string{}}
	}
	normalized, err := normalizeScope(scope)
	if err != nil {
		if failure, ok := err.(*inspectionError); ok {
			return outOfScopeUnknown(failure.Code, failure.Message)
		}
		return outOfScopeUnknown("SCOPE_NORMALIZATION_FAILED", err.Error())
	}
	if len(normalized) == 0 {
		return map[string]any{"status": "OK", "code": "NO_SCOPE_DECLARED", "paths": []string{}}
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return outOfScopeFailure(err)
	}
	statuses, err := statusEntries(root, 10*time.Second)
	if err != nil {
		return outOfScopeFailure(err)
	}
	baselineStatuses, ok := mapsList(baseline["status_entries"])
	if !ok {
		return outOfScopeUnknown("OUT_OF_SCOPE_INSPECTION_FAILED", "baseline status entries were malformed")
	}
	preexisting := make(map[string]bool, len(baselineStatuses))
	for _, entry := range baselineStatuses {
		path, pathOK := entry["path"].(string)
		if !pathOK {
			return outOfScopeUnknown("OUT_OF_SCOPE_INSPECTION_FAILED", "baseline status entries were malformed")
		}
		state, _ := entry["state"].(string)
		preexisting[path+"\x00"+state] = true
	}
	outside := make(map[string]bool)
	for _, entry := range statuses {
		path, state := entry["path"], entry["state"]
		if preexisting[path+"\x00"+state] {
			continue
		}
		if !pathInScope(path, normalized) {
			outside[path] = true
		}
	}
	baselineHead := baseline["head"].(string)
	if baselineHead != "" {
		raw, err := runGit(root, []string{"rev-parse", "--verify", "HEAD"}, 10*time.Second, "reading current HEAD", nil, nil)
		if err != nil {
			return outOfScopeFailure(err)
		}
		currentHead := strings.TrimSpace(decodeGitPath(raw))
		if currentHead != baselineHead {
			paths, _, err := deltaDiff(root, []string{baselineHead, currentHead}, "committed out-of-scope check")
			if err != nil {
				return outOfScopeFailure(err)
			}
			for path := range paths {
				if !pathInScope(path, normalized) {
					outside[path] = true
				}
			}
		}
	}
	return map[string]any{"status": "OK", "code": "OUT_OF_SCOPE_CHECK_COMPLETED", "paths": sortedKeys(outside)}
}

func pathInScope(path string, scope []string) bool {
	for _, item := range scope {
		if item == "." || path == item || strings.HasPrefix(path, strings.TrimRight(item, "/")+"/") {
			return true
		}
	}
	return false
}

func outOfScopeUnknown(code, message string) map[string]any {
	return map[string]any{"status": "UNKNOWN", "code": code, "message": message, "paths": []string{}}
}

func outOfScopeFailure(err error) map[string]any {
	if failure, ok := err.(*inspectionError); ok {
		return outOfScopeUnknown(failure.Code, failure.Message)
	}
	return outOfScopeUnknown("OUT_OF_SCOPE_INSPECTION_FAILED", err.Error())
}
