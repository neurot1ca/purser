package enforce

import (
	"errors"
	"os"
	"path/filepath"
)

var errAsyncClaimed = errors.New("async result is already being finalized")

// claimAsyncResult mirrors the lock, post-lock ownership revalidation, and
// cached CONSUMED response branch of codex._async_post_followup.
func claimAsyncResult(root string, event map[string]any, repoRoot string) (*os.File, map[string]any, map[string]any, map[string]any, any, error) {
	path, _, _, _, err := asyncOwned(root, event, repoRoot)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	claimPath := path[:len(path)-len(filepath.Ext(path))] + ".claim.lock"
	if err = os.MkdirAll(filepath.Dir(claimPath), 0700); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	lock, err := os.OpenFile(claimPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if info, statErr := lock.Stat(); statErr != nil {
		_ = lock.Close()
		return nil, nil, nil, nil, nil, statErr
	} else if info.Size() == 0 {
		_, _ = lock.WriteAt([]byte{'0'}, 0)
	}
	if err = lockTaskUsageFile(lock); err != nil {
		_ = lock.Close()
		return nil, nil, nil, nil, nil, errAsyncClaimed
	}
	release := func(cause error) (*os.File, map[string]any, map[string]any, map[string]any, any, error) {
		releaseTaskUsageLock(lock)
		return nil, nil, nil, nil, nil, cause
	}
	path, index, pending, original, err := asyncOwned(root, event, repoRoot)
	if err != nil {
		return release(err)
	}
	if index["stage"] == "CONSUMED" || index["stage"] == "FAILED" {
		return lock, index, pending, original, index["response"], nil
	}
	index["stage"] = "FINALIZING"
	if err = atomicAdmissionJSON(path, index); err != nil {
		return release(err)
	}
	return lock, index, pending, original, nil, nil
}

func consumeAsyncResult(path string, index map[string]any, bundlePath string, response any) error {
	index["stage"], index["bundle_path"], index["response"] = "CONSUMED", bundlePath, response
	return atomicAdmissionJSON(path, index)
}
