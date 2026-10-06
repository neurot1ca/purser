package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHostMainRecordsReturnedError(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	jobDir := filepath.Join(t.TempDir(), "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}

	const jobID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if code := hostMain([]string{jobDir, jobID, "7", "token"}); code == 0 {
		t.Fatal("hostMain succeeded with a missing job record")
	}

	f, err := os.Open(filepath.Join(stateRoot, "diagnostics.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var diagnostic map[string]any
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic["phase"] != "host" || diagnostic["job_id"] != jobID || diagnostic["generation"] != float64(7) {
		t.Fatalf("host diagnostic identity = %#v", diagnostic)
	}
	if diagnostic["error"] == "" || diagnostic["at"] == "" {
		t.Fatalf("host diagnostic is missing error or timestamp: %#v", diagnostic)
	}
}
