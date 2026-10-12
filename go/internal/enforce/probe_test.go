package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestProbeP38PythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/probe.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Commands []struct {
			Prefix   []string `json:"prefix"`
			Cwd      string   `json:"cwd"`
			Model    string   `json:"model"`
			Output   string   `json:"output"`
			Expected []string `json:"expected"`
		} `json:"commands"`
		Cases []struct {
			Name       string         `json:"name"`
			Stdout     string         `json:"stdout"`
			Stderr     string         `json:"stderr"`
			OutputData bool           `json:"output_data"`
			Started    bool           `json:"started"`
			Returncode int            `json:"returncode"`
			TimedOut   bool           `json:"timed_out"`
			Expected   map[string]any `json:"expected"`
		} `json:"cases"`
		RuntimeCases []struct {
			Name           string         `json:"name"`
			LaunchFailure  bool           `json:"launch_failure"`
			TimeoutSeconds float64        `json:"timeout_seconds"`
			Expected       map[string]any `json:"expected"`
		} `json:"runtime_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Commands {
		tmp := t.TempDir()
		output := materializeProbePath(item.Output, tmp)
		want := make([]string, len(item.Expected))
		for i, arg := range item.Expected {
			want[i] = materializeProbePath(arg, tmp)
		}
		if got := ProbeCommand(item.Prefix, item.Cwd, item.Model, output); !reflect.DeepEqual(got, want) {
			t.Errorf("command p38 = %#v, want %#v", got, want)
		}
	}
	for _, item := range fixture.Cases {
		got := ProbeResult([]byte(item.Stdout), item.Stderr, item.OutputData, item.Started, item.Returncode, item.TimedOut)
		if !reflect.DeepEqual(got, item.Expected) {
			t.Errorf("%s: got %#v, want %#v", item.Name, got, item.Expected)
		}
		if got["ok"] == true && item.Name != "p38_completed" && item.Name != "p39_success" {
			t.Errorf("%s reported success", item.Name)
		}
	}
	for _, item := range fixture.RuntimeCases {
		t.Run(item.Name, func(t *testing.T) {
			missing, _ := filepath.Abs(filepath.Join("..", "..", "..", ".tmp", "missing-p39-cli"))
			prefix := []string{missing}
			if !item.LaunchFailure {
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join("..", "..", "..", ".tmp")
				if err = os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				fake := filepath.Join(dir, "p39-fake-cli")
				if runtime.GOOS == "windows" {
					fake += ".exe"
				}
				if err = copyProbeFake(binary, fake); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(fake) })
				fake, err = filepath.Abs(fake)
				if err != nil {
					t.Fatal(err)
				}
				prefix = []string{fake, "-test.run=TestP39FakeCLI", "--"}
				if err = os.Setenv("PURSER_P39_FAKE_CASE", item.Name); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Unsetenv("PURSER_P39_FAKE_CASE") })
			}
			got := RunProbe(prefix, "gpt-probe", time.Duration(item.TimeoutSeconds*float64(time.Second)))
			if !reflect.DeepEqual(got, item.Expected) {
				t.Fatalf("runtime result = %#v, want %#v", got, item.Expected)
			}
		})
	}
}

func materializeProbePath(value, root string) string {
	if !strings.HasPrefix(value, "__TMP__/") {
		return value
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(value, "__TMP__/")))
}

func copyProbeFake(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := os.FileMode(0700)
	if runtime.GOOS == "windows" {
		mode = 0600
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func TestP39FakeCLI(t *testing.T) {
	name := os.Getenv("PURSER_P39_FAKE_CASE")
	if name == "" {
		return
	}
	switch name {
	case "p39_success":
		if err := validateProbeInvocation(); err != nil {
			t.Fatal(err)
		}
		if record := os.Getenv("PURSER_P39_CWD_RECORD"); record != "" {
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(record, []byte(cwd), 0600); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println(`{"type":"turn.completed"}`)
	case "p39_stale":
		marker, release := os.Getenv("PURSER_P39_MARKER"), os.Getenv("PURSER_P39_RELEASE")
		if err := os.WriteFile(marker, []byte("started"), 0600); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(5 * time.Second)
		for time.Now().Before(until) {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Println(`{"type":"turn.completed"}`)
	case "p39_failure_exit":
		fmt.Fprint(os.Stderr, "insufficient account balance")
		os.Exit(1)
	case "p39_timeout":
		time.Sleep(30 * time.Second)
	}
}

func validateProbeInvocation() error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if string(input) != "Reply with OK." {
		return fmt.Errorf("probe stdin = %q", input)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("probe cwd was not empty: %v", entries)
	}
	args := strings.Join(os.Args, " ")
	if !strings.Contains(args, "gpt-probe") || !strings.Contains(args, "--json") {
		return fmt.Errorf("probe args missing model or JSON mode: %s", args)
	}
	return nil
}

func TestP39SuccessfulProbeReturnsAndQueuesCreditNotice(t *testing.T) {
	project, root, state := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	active := map[string]any{"active_coder": "fallback", "triggering_evidence": map[string]any{"type": "CREDIT_EXHAUSTED", "trigger_id": "credit-p39"}, "reset_cycle_marker": nil, "last_probe_time": nil}
	writeFallbackFixtureState(t, project, root, active)
	fake := p39FakeBinary(t)
	t.Setenv("PURSER_P39_FAKE_CASE", "p39_success")
	cwdRecord := filepath.Join("..", "..", "..", ".tmp", "p39-cwd-record")
	cwdRecord, _ = filepath.Abs(cwdRecord)
	_ = os.Remove(cwdRecord)
	t.Setenv("PURSER_P39_CWD_RECORD", cwdRecord)
	returned, err := ProbeFallback(project, root, "CREDIT_EXHAUSTED", "credit-p39", map[string]any{"session_id": "session-p39"}, []string{fake, "-test.run=TestP39FakeCLI", "--"}, "gpt-probe")
	if err != nil || !returned {
		t.Fatalf("probe return = %v, %v", returned, err)
	}
	current := readFallbackFixtureState(t, project, root)
	if current["active_coder"] != "primary" || current["triggering_evidence"] != nil {
		t.Fatalf("returned state = %#v", current)
	}
	probeCWD, err := os.ReadFile(cwdRecord)
	if err != nil {
		t.Fatal(err)
	}
	wantParent := filepath.Join(state, "cache", "tmp")
	if filepath.Dir(string(probeCWD)) != wantParent {
		t.Fatalf("probe temp cwd = %s, want parent %s", probeCWD, wantParent)
	}
	if _, err = os.Stat(string(probeCWD)); !os.IsNotExist(err) {
		t.Fatalf("probe temp cwd remains after completion: %s (%v)", probeCWD, err)
	}
	_ = os.Remove(cwdRecord)
	hash := sha256.Sum256([]byte(project))
	projectKey := hex.EncodeToString(hash[:])[:16]
	if runtime.GOOS == "windows" {
		lower := strings.ToLower(project)
		hash = sha256.Sum256([]byte(lower))
		projectKey = hex.EncodeToString(hash[:])[:16]
	}
	path := filepath.Join(state, "projects", projectKey, "telemetry", "compaction_advisor.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var advisor map[string]any
	if err = json.Unmarshal(data, &advisor); err != nil {
		t.Fatal(err)
	}
	pending := advisor["fallback_pending"].(map[string]any)
	want := "> ⚓ **gpt-probe's credits have been replenished; returned to the primary coder.**"
	if pending["line"] != want || pending["notice"] != true {
		t.Fatalf("credit notice = %#v", pending)
	}
}

func TestP39StaleProbeCannotReplaceNewTrigger(t *testing.T) {
	project, root := t.TempDir(), t.TempDir()
	active := map[string]any{"active_coder": "fallback", "triggering_evidence": map[string]any{"type": "CREDIT_EXHAUSTED", "trigger_id": "old-p39"}, "reset_cycle_marker": nil, "last_probe_time": nil}
	writeFallbackFixtureState(t, project, root, active)
	fake := p39FakeBinary(t)
	dir := filepath.Join("..", "..", "..", ".tmp")
	marker, release := filepath.Join(dir, "p39-started"), filepath.Join(dir, "p39-release")
	marker, _ = filepath.Abs(marker)
	release, _ = filepath.Abs(release)
	_ = os.Remove(marker)
	_ = os.Remove(release)
	t.Setenv("PURSER_P39_FAKE_CASE", "p39_stale")
	t.Setenv("PURSER_P39_MARKER", marker)
	t.Setenv("PURSER_P39_RELEASE", release)
	result := make(chan bool, 1)
	go func() {
		returned, _ := ProbeFallback(project, root, "CREDIT_EXHAUSTED", "old-p39", nil, []string{fake, "-test.run=TestP39FakeCLI", "--"}, "gpt-probe")
		result <- returned
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("fake probe did not start")
	}
	changed, err := contextio.UpdateFallbackTrigger(project, root, "CREDIT_EXHAUSTED", "old-p39", func(state map[string]any) {
		state["triggering_evidence"].(map[string]any)["trigger_id"] = "new-p39"
	})
	if err != nil || !changed {
		t.Fatalf("replace trigger = %v, %v", changed, err)
	}
	if err = os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	if <-result {
		t.Fatal("stale probe returned to primary")
	}
	current := readFallbackFixtureState(t, project, root)
	if current["active_coder"] != "fallback" || current["triggering_evidence"].(map[string]any)["trigger_id"] != "new-p39" {
		t.Fatalf("new trigger was changed: %#v", current)
	}
	_ = os.Remove(marker)
	_ = os.Remove(release)
}

func p39FakeBinary(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("..", "..", "..", ".tmp")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "p39-fake-cli")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	if err = copyProbeFake(binary, fake); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fake) })
	fake, err = filepath.Abs(fake)
	if err != nil {
		t.Fatal(err)
	}
	return fake
}
