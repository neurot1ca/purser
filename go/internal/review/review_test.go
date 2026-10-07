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
	t.Log("findings retention mirrors reviewer.py:1362-1382")
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
	missing := filepath.Join(scratch, "missing-agy.exe")
	tests := []struct {
		name, source, executable, wantExecutable, model, mode, errorCode string
		extra                                                            []string
		timeout                                                          time.Duration
	}{
		{"default model", "reviewer.py:176-182, 184-215, 667-679", fake, fake, pinnedModel, "success", "", nil, time.Second},
		{"configured model", "reviewer.py:178-182, 184-215, 1501-1508, 667-670", fake, fake, "gemini-3.8-flash-high", "success", "", nil, time.Second},
		{"missing executable", "reviewer.py:667-679, 1391-1399", missing, missing, pinnedModel, "success", "REVIEWER_EXECUTABLE_MISSING", nil, time.Second},
		{"refused flags", "reviewer.py:914-941, 1400-1407", fake, fake, pinnedModel, "success", "REVIEWER_RESPONSE_INVALID", []string{"--disable-slash-commands"}, time.Second},
		{"failed command", "reviewer.py:1302-1329", fake, fake, pinnedModel, "failure", "REVIEWER_PROCESS_FAILED", nil, time.Second},
		{"timeout", "reviewer.py:1280-1299", fake, fake, pinnedModel, "timeout", "REVIEWER_TIMEOUT", nil, 50 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Log("mirrors " + test.source)
			t.Setenv("PATH", t.TempDir())
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
				if len(got.Findings) != 1 || got.Findings[0] != (Finding{Code: "BUG", Message: "bad"}) {
					t.Fatalf("findings were not retained: %#v", got.Findings)
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

func TestChangesRequiredNeedsFinding(t *testing.T) {
	_, err := ValidateResponse(map[string]any{"result": "CHANGES_REQUIRED", "validation": "FAIL", "findings": []any{}})
	if err == nil {
		t.Fatal("CHANGES_REQUIRED without findings was accepted")
	}
	envelope := []byte(`{"conversation_id":"fixture","status":"SUCCESS","response":"","duration_seconds":0.1,"num_turns":1,"usage":{},"structured_output":{"result":"CHANGES_REQUIRED","validation":"FAIL","findings":[]}}`)
	if got := parse(envelope); got.ReviewStatus != "UNAVAILABLE" || got.ErrorCode != "REVIEWER_RESPONSE_INVALID" {
		t.Fatalf("native response outcome = %#v", got)
	}
}

func TestReviewDecisionPrimitives(t *testing.T) {
	tests := []struct {
		name, source string
		check        func() bool
	}{
		{"skip versus executed pass", "policy.py:621-688; codex.py:3948-3956", func() bool {
			skip := Gate("implementation", "normal", true, 0, 0, 2, 20, true)
			run := Gate("implementation", "normal", true, 4, 40, 2, 20, true)
			passed := Outcome{ReviewStatus: "COMPLETED", Result: "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", Validation: "PASS"}
			return skip.Reason == NoDelta && !skip.Run && run.Run && !Passed(Outcome{}, NoDelta) && Passed(passed, "")
		}},
		{"same vendor excluded", "policy.py:580-618; codex.py:4582-4609", func() bool {
			return SameFamily("gpt", "openai") && SameFamily("anthropic", "claude") && !SameFamily("google", "openai")
		}},
		{"full mode selection", "policy.py:715-770", func() bool {
			cases := []struct{ route, skip, result, want string }{
				{"normal", "REVIEWER_UNAVAILABLE", "", Full},
				{"normal", "", "CHANGES_REQUIRED", Full},
				{"normal", "", "UNCERTAIN", Full},
				{"normal", "", "REVIEWER1_UNAVAILABLE", Full},
				{"normal", "", "unrecognized", Full},
			}
			for _, tc := range cases {
				if got := ReviewMode(true, false, tc.skip, tc.result).Mode; got != tc.want {
					return false
				}
			}
			return ReviewMode(true, true, "NO_DELTA", "").Mode == Full && ReviewMode(false, false, "", "").Mode == Full
		}},
		{"only trivial skips select audit", "policy.py:740-758", func() bool {
			return ReviewMode(true, false, NoDelta, "").Mode == Audit &&
				ReviewMode(true, false, BelowThreshold, "").Mode == Audit &&
				ReviewMode(true, false, "", "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE").Mode == Audit
		}},
		{"trivial skips are not reviewer passes", "policy.py:740-751; codex.py:3948-3956", func() bool {
			for _, reason := range []string{NoDelta, BelowThreshold} {
				if Passed(Outcome{ReviewStatus: "COMPLETED", Result: "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", Validation: "PASS"}, reason) {
					return false
				}
			}
			return true
		}},
		{"below threshold skips", "policy.py:672-688", func() bool {
			got := Gate("implementation", "normal", true, 1, 10, 2, 20, true)
			return !got.Run && got.Reason == BelowThreshold
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Log("mirrors " + test.source)
			if !test.check() {
				t.Fatal("decision did not match Python contract")
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
