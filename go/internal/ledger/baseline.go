package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func diffFingerprint(root string, cached bool, timeout time.Duration) (string, error) {
	args := []string{"diff"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, "--binary", "--no-ext-diff", "--no-textconv", "--")
	raw, err := runGit(root, args, timeout, map[bool]string{true: "fingerprinting the staged diff", false: "fingerprinting the unstaged diff"}[cached], nil, nil)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func unknownBaseline(code, message string) map[string]any {
	return map[string]any{"status": "UNKNOWN", "code": code, "message": message, "changed_paths": nil, "baseline_head": nil, "current_head": nil}
}

func captureBaseline(repoRoot string, scope []string, timeout time.Duration, manifestBudgetSeconds float64) map[string]any {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return unknownBaseline("REPOSITORY_IO_FAILED", "The repository could not be inspected while capturing the baseline.")
	}
	normalized := []string{}
	if len(scope) > 0 {
		normalized, err = normalizeScope(scope)
		if err != nil {
			failure := err.(*inspectionError)
			return unknownBaseline(failure.Code, failure.Message)
		}
	}
	headRaw, err := runGit(root, []string{"rev-parse", "--verify", "HEAD"}, timeout, "reading HEAD", nil, nil)
	if err != nil {
		return baselineError(err)
	}
	head := strings.TrimSpace(decodeGitPath(headRaw))
	statuses, err := statusEntries(root, timeout)
	if err != nil {
		return baselineError(err)
	}
	staged, err := diffFingerprint(root, true, timeout)
	if err != nil {
		return baselineError(err)
	}
	unstaged, err := diffFingerprint(root, false, timeout)
	if err != nil {
		return baselineError(err)
	}
	snapshots, err := trackedSnapshots(root, statuses, timeout, nil)
	if err != nil {
		return baselineError(err)
	}
	manifest := []map[string]any{}
	if len(normalized) > 0 {
		manifest, err = scopedManifest(root, normalized, timeout, manifestBudgetSeconds)
		if err != nil {
			return baselineError(err)
		}
	}
	return map[string]any{"status": "OK", "code": "BASELINE_CAPTURED", "head": head, "scope": normalized, "manifest_enabled": len(normalized) > 0, "status_entries": statuses, "staged_diff_fingerprint": staged, "unstaged_diff_fingerprint": unstaged, "tracked_snapshots": snapshots, "scoped_manifest": manifest}
}

func baselineError(err error) map[string]any {
	if failure, ok := err.(*inspectionError); ok {
		return unknownBaseline(failure.Code, failure.Message)
	}
	if _, ok := err.(*os.PathError); ok {
		return unknownBaseline("REPOSITORY_IO_FAILED", "The repository could not be inspected while capturing the baseline.")
	}
	return unknownBaseline("REPOSITORY_IO_FAILED", "The repository could not be inspected while capturing the baseline.")
}
