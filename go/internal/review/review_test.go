package review

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func fakeReviewer(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "go-review-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	source := `package main
import ("encoding/json"; "io"; "os"; "time")
func main() { switch os.Getenv("PURSER_FAKE_MODE") {
case "success": input, _ := io.ReadAll(os.Stdin); cwd, _ := os.Getwd(); call, _ := json.Marshal(map[string]any{"args":os.Args, "cwd":cwd, "env":os.Getenv("PURSER_REVIEW_TEST_ENV"), "stdin":string(input)}); os.WriteFile(os.Getenv("PURSER_FAKE_CAPTURE"), call, 0600); os.Stdout.Write([]byte(os.Getenv("PURSER_FAKE_OUTPUT")))
case "failure": os.Exit(7)
case "timeout": time.Sleep(2*time.Second)
} }`
	sourcePath, binary := filepath.Join(dir, "fake.go"), filepath.Join(dir, "fake-reviewer.exe")
	if runtime.GOOS != "windows" {
		binary = filepath.Join(dir, "fake-reviewer")
	}
	if err := os.WriteFile(sourcePath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("go", "build", "-o", binary, sourcePath).CombinedOutput(); err != nil {
		t.Fatalf("build fake reviewer: %v: %s", err, output)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	return binary, dir
}

func TestReviewerAGYCommandAndOutcomes(t *testing.T) {
	fake, scratch := fakeReviewer(t)
	repository, cwd := filepath.Join(scratch, "repo"), filepath.Join(scratch, "worker")
	for _, dir := range []string{repository, cwd} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	packet := filepath.Join(cwd, "review-packet.json")
	if err := os.WriteFile(packet, []byte(`{"packet":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	response := `{"conversation_id":"fake-1","status":"SUCCESS","response":"","duration_seconds":0.01,"num_turns":1,"usage":{},"structured_output":{"result":"CHANGES_REQUIRED","validation":"FAIL","findings":[{"code":"BUG","message":"bad"}]}}`
	binDir := filepath.Join(scratch, "path")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	agyName := "agy.exe"
	if runtime.GOOS != "windows" {
		agyName = "agy"
	}
	agy := filepath.Join(binDir, agyName)
	binary, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agy, binary, 0700); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	missing := filepath.Join(scratch, "missing-agy.exe")
	tests := []struct {
		name, source, executable, wantExecutable, model, mode, errorCode string
		extra                                                            []string
		timeout                                                          time.Duration
		pathLookup                                                       bool
	}{
		{"default model", "reviewer.py:176-182, 184-215, 667-679", "", agy, pinnedModel, "success", "", nil, time.Second, true},
		{"configured model", "reviewer.py:178-182, 184-215, 1501-1508, 667-670", fake, fake, "gemini-3.8-flash-high", "success", "", nil, time.Second, false},
		{"missing executable", "reviewer.py:667-679, 1391-1399", missing, missing, pinnedModel, "success", "REVIEWER_EXECUTABLE_MISSING", nil, time.Second, false},
		{"refused flags", "reviewer.py:914-941, 1400-1407", fake, fake, pinnedModel, "success", "REVIEWER_RESPONSE_INVALID", []string{"--disable-slash-commands"}, time.Second, false},
		{"failed command", "reviewer.py:1302-1329", fake, fake, pinnedModel, "failure", "REVIEWER_PROCESS_FAILED", nil, time.Second, false},
		{"timeout", "reviewer.py:1280-1299", fake, fake, pinnedModel, "timeout", "REVIEWER_TIMEOUT", nil, 50 * time.Millisecond, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Log("mirrors " + test.source)
			if test.pathLookup {
				t.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath)
			}
			t.Setenv("PURSER_FAKE_MODE", test.mode)
			t.Setenv("PURSER_FAKE_OUTPUT", response)
			t.Setenv("PURSER_REVIEW_TEST_ENV", "inherited-value")
			capture := filepath.Join(scratch, "capture.json")
			t.Setenv("PURSER_FAKE_CAPTURE", capture)
			req := Request{Executable: test.executable, Model: test.model, PacketPath: packet, Schema: map[string]any{"type": "object"}, ExtraFlags: test.extra, CWD: cwd, Repository: repository, Timeout: test.timeout}
			got := Review(req)
			if test.errorCode == "" {
				if got.ReviewStatus != "COMPLETED" || got.Result != "CHANGES_REQUIRED" || got.Validation != "FAIL" {
					t.Fatalf("outcome = %#v", got)
				}
			} else if got.ReviewStatus != "UNAVAILABLE" || got.Result != "REVIEWER1_UNAVAILABLE" || got.Validation != "UNKNOWN" || got.ErrorCode != test.errorCode {
				t.Fatalf("outcome = %#v", got)
			}
			if got.ReviewStatus == "UNAVAILABLE" && (got.Result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" || got.Validation == "PASS") {
				t.Fatalf("unavailable reviewer appeared passing: %#v", got)
			}

			if test.name == "refused flags" {
				return
			}
			want := []string{test.wantExecutable, "--print", reviewPrompt(packet), "--output-format", "json", "--json-schema", `{"type":"object"}`, "--model", "gemini-3.8-flash-high", "--mode", "plan", "--sandbox"}
			command, err := buildCommand(req)
			if err != nil {
				t.Fatal(err)
			}
			if !equal(command, want) {
				t.Fatalf("argv = %q, want %q", command, want)
			}
			if test.errorCode != "" {
				return
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var call struct {
				Args  []string `json:"args"`
				CWD   string   `json:"cwd"`
				Env   string   `json:"env"`
				Stdin string   `json:"stdin"`
			}
			if err := json.Unmarshal(data, &call); err != nil {
				t.Fatal(err)
			}
			if !equal(call.Args, want) || call.CWD != cwd || call.Env != "inherited-value" || call.Stdin != "" {
				t.Fatalf("launch = %#v, want argv=%q cwd=%q inherited env and empty stdin", call, want, cwd)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
