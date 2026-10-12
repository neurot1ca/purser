package review

import (
	"os"
	"path/filepath"
)

func runtimeTempDir(prefix string) (string, error) {
	dir := filepath.Join(reviewerDiagnosticStateRoot(), "cache", "tmp")
	if err := os.MkdirAll(dir, 0700); err == nil {
		if path, err := os.MkdirTemp(dir, prefix); err == nil {
			return path, nil
		}
	}
	return os.MkdirTemp("", prefix)
}
