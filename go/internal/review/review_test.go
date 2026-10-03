package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func fakeReviewer(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "go-review-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	source := `package main
import ("os"; "time")
func main() { switch os.Getenv("PURSER_FAKE_MODE") {
case "success": os.Stdout.Write([]byte(os.Getenv("PURSER_FAKE_OUTPUT")))
case "failure": os.Exit(7)
case "timeout": time.Sleep(2*time.Second)
} }`
	sourcePath := filepath.Join(dir, "fake.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "fake-reviewer.exe")
	if runtime.GOOS != "windows" {
		binary = filepath.Join(dir, "fake-reviewer")
	}
	build := exec.Command("go", "build", "-o", binary, sourcePath)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake reviewer: %v: %s", err, output)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

func TestReviewerOutcomes(t *testing.T) {
	fake := fakeReviewer(t)
	cwd, repository := t.TempDir(), t.TempDir()
	response := `{"conversation_id":"fake-1","status":"SUCCESS","response":"","duration_seconds":0.01,"num_turns":1,"usage":{},"structured_output":{"result":"CHANGES_REQUIRED","validation":"FAIL","findings":[{"code":"BUG","message":"bad"}]}}`
	tests := []struct {
		name, mode, source, executable   string
		args                             []string
		timeout                          time.Duration
		status, result, validation, code string
	}{
		{"success parsed verdict", "success", "reviewer.py:1123-1150, 1361-1390", fake, nil, time.Second, "COMPLETED", "CHANGES_REQUIRED", "FAIL", ""},
		{"refused command", "success", "reviewer.py:914-941, 1400-1407", fake, []string{"--mode", "plan", "--disable-slash-commands"}, time.Second, "UNAVAILABLE", "REVIEWER1_UNAVAILABLE", "UNKNOWN", "REVIEWER_RESPONSE_INVALID"},
		{"failed command", "failure", "reviewer.py:1302-1329", fake, nil, time.Second, "UNAVAILABLE", "REVIEWER1_UNAVAILABLE", "UNKNOWN", "REVIEWER_PROCESS_FAILED"},
		{"timeout", "timeout", "reviewer.py:1280-1299", fake, nil, 50 * time.Millisecond, "UNAVAILABLE", "REVIEWER1_UNAVAILABLE", "UNKNOWN", "REVIEWER_TIMEOUT"},
		{"missing executable", "success", "reviewer.py:1391-1399", filepath.Join(t.TempDir(), "missing"), nil, time.Second, "UNAVAILABLE", "REVIEWER1_UNAVAILABLE", "UNKNOWN", "REVIEWER_EXECUTABLE_MISSING"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Log("mirrors " + test.source)
			t.Setenv("PURSER_FAKE_MODE", test.mode)
			t.Setenv("PURSER_FAKE_OUTPUT", response)
			got := Review(Request{Command: append([]string{test.executable}, test.args...), CWD: cwd, Repository: repository, Timeout: test.timeout})
			if got.ReviewStatus != test.status || got.Result != test.result || got.Validation != test.validation || got.ErrorCode != test.code {
				t.Fatalf("outcome = %#v", got)
			}
			if test.status == "UNAVAILABLE" && (got.Result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" || got.Validation == "PASS") {
				t.Fatalf("unavailable reviewer appeared passing: %#v", got)
			}
		})
	}
}
