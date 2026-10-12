package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestQuotaWakeCommandOutput(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := contextio.RecordQuotaWake(repo, root, "session", "task", reset, "quota_failure", time.Now()); err != nil {
		t.Fatal(err)
	}
	capture := func(args []string) (string, int) {
		old := os.Stdout
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = writer
		code := quotaWakeMain(args)
		_ = writer.Close()
		os.Stdout = old
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(data), code
	}
	output, code := capture([]string{"--cwd", repo})
	want := `{"pending":true,"reason":"quota_failure","reset_at":"` + reset + `","task_id":"task"}` + "\n"
	if code != 0 || output != want {
		t.Fatalf("quota-wake output = %q, code %d; want %q, 0", output, code, want)
	}
	output, code = capture([]string{"--cwd", repo, "--clear"})
	if code != 0 || output != "{\"pending\":false}\n" {
		t.Fatalf("quota-wake --clear output = %q, code %d", output, code)
	}
}
