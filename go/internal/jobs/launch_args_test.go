package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestFlattenConfigPythonValues(t *testing.T) {
	got, err := FlattenConfig(OrderedObject{
		{Key: "nested", Value: OrderedObject{
			{Key: "text", Value: "quote \" slash \\ line\n雪"},
			{Key: "enabled", Value: true},
			{Key: "count", Value: json.Number("7")},
			{Key: "ratio", Value: json.Number("1.25")},
			{Key: "large_ratio", Value: json.Number("1000000.0")},
			{Key: "small_ratio", Value: json.Number("1e-5")},
			{Key: "items", Value: []any{"x", false, json.Number("2")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`nested.text="quote \" slash \\ line\n雪"`, "nested.enabled=true", "nested.count=7", "nested.ratio=1.25", "nested.large_ratio=1000000.0", "nested.small_ratio=1e-05", `nested.items=["x", false, 2]`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildCodexCommandNewAndResume(t *testing.T) {
	dev, base, approval := "dev", "base", "never"
	baseArgs := LaunchArgs{
		BinaryOverride: `"codex"`, Name: "codex", Cwd: `C:\work`, Model: "gpt-test",
		Config: OrderedObject{{Key: "model_reasoning_effort", Value: "high"}}, Sandbox: "workspace-write",
		OutputPath: `C:\temp\last.txt`, DeveloperInstructions: &dev, BaseInstructions: &base,
		ApprovalPolicy: &approval, Images: []string{`C:\temp\image.png`},
	}
	got, err := BuildCodexCommand(baseArgs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "exec", "--json", "--skip-git-repo-check", "-m", "gpt-test",
		"-c", `model_reasoning_effort="high"`, "-c", `developer_instructions="dev"`,
		"-c", `base_instructions="base"`, "-c", `approval_policy="never"`,
		"-s", "workspace-write", "-C", `C:\work`, "--image", `C:\temp\image.png`,
		"-o", `C:\temp\last.txt`, "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}

	baseArgs.Name, baseArgs.ThreadID, baseArgs.Sandbox = "codex-reply", "thread-1", ""
	got, err = BuildCodexCommand(baseArgs)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"codex", "exec", "resume", "--json", "--skip-git-repo-check", "-m", "gpt-test",
		"-c", `model_reasoning_effort="high"`, "-c", `developer_instructions="dev"`,
		"-c", `base_instructions="base"`, "-c", `approval_policy="never"`,
		"--image", `C:\temp\image.png`, "-o", `C:\temp\last.txt`, "thread-1", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildCodexCommandOmitsAbsentOptionsAndKeepsPromptOffArgv(t *testing.T) {
	got, err := BuildCodexCommand(LaunchArgs{BinaryOverride: "codex", Name: "codex-reply", ThreadID: "thread"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "exec", "resume", "--json", "--skip-git-repo-check", "-o", ".", "thread", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestResolveCodexPrefixWindowsLaunchers(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows launcher resolution")
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	launcher := filepath.Join(binDir, "codex.cmd")
	entry := filepath.Join(binDir, "node_modules", "@openai", "codex", "bin", "codex.js")
	node := filepath.Join(root, "node.exe")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{launcher, entry, node, filepath.Join(binDir, "codex.exe")} {
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+binDir)
	got, err := ResolveCodexPrefix(filepath.Join(binDir, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{node, entry}) {
		t.Fatalf(".cmd: got %#v", got)
	}
	got, err = ResolveCodexPrefix(filepath.Join(binDir, "codex.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{filepath.Join(binDir, "codex.exe")}) {
		t.Fatalf(".exe: got %#v", got)
	}
}

func TestLaunchArgsMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PURSER_CODEX_BIN", "")
	if _, err := ResolveCodexPrefix(""); err == nil || err.Error() != "Codex executable was not found" {
		t.Fatalf("error = %v, want missing executable error", err)
	}
}

type launchFixture struct {
	Name     string     `json:"name"`
	Input    LaunchArgs `json:"input"`
	Expected []string   `json:"expected"`
	Error    string     `json:"error"`
}

type launchFixtureFile struct {
	Cases []launchFixture `json:"cases"`
}

func TestLaunchArgsPythonFixtures(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_launch_args", "launch_args.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Python launch argument fixtures: %v", err)
	}
	var fixture launchFixtureFile
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("Python launch argument fixture has no cases")
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			tmp := t.TempDir()
			test.Input.OutputPath = materializeLaunchPath(test.Input.OutputPath, tmp)
			got, err := BuildCodexCommand(test.Input)
			if test.Error != "" {
				if err == nil || !strings.Contains(err.Error(), test.Error) {
					t.Fatalf("error = %v, want substring %q", err, test.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := make([]string, len(test.Expected))
			for i, arg := range test.Expected {
				want[i] = materializeLaunchPath(arg, tmp)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got, want)
			}
		})
	}
}

func materializeLaunchPath(value, root string) string {
	if !strings.HasPrefix(value, "__TMP__/") {
		return value
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(value, "__TMP__/")))
}
