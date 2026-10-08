package enforce

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMechanicalShadowWorker(t *testing.T) {
	fake := buildFakeOpenCode(t)
	t.Run("success uses a scratch copy and matches raw outcome", func(t *testing.T) {
		repo, scratch := shadowFixture(t)
		observed := filepath.Join(t.TempDir(), "observed.txt")
		t.Setenv("FAKE_OBSERVED", observed)
		wantStdout := "{\"type\":\"text\",\"part\":{\"text\":\"Changed parser to retain valid free candidates.\"}}\n" +
			"{\"type\":\"step_finish\",\"part\":{\"reason\":\"stop\",\"tokens\":{\"input\":10,\"output\":4,\"total\":14}}}\n"
		t.Setenv("FAKE_STDOUT", wantStdout)
		outcome, err := runMechanicalShadow(context.Background(), mechanicalShadowJob{
			Model: "fixture/model", Prompt: "fixture prompt", Repository: repo, Scratch: scratch,
		}, fake, 45*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Stdout != wantStdout || outcome.ExitCode != 0 || outcome.TimedOut {
			t.Fatalf("outcome = %#v", outcome)
		}
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			t.Fatalf("scratch remains: %v", err)
		}
		observation, err := os.ReadFile(observed)
		if err != nil || !strings.HasPrefix(string(observation), scratch+"|repository-input|") || !strings.Contains(string(observation), `"edit":"ask"`) {
			t.Fatalf("worker observation = %q, %v; want scratch copy", observation, err)
		}
		if data, err := os.ReadFile(filepath.Join(repo, "input.txt")); err != nil || string(data) != "repository-input" {
			t.Fatalf("repository changed: %q, %v", data, err)
		}
	})
	t.Run("worker failure is reported and scratch is removed", func(t *testing.T) {
		repo, scratch := shadowFixture(t)
		wantStdout := "{\"type\":\"step_finish\",\"part\":{\"reason\":\"error\",\"tokens\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n"
		t.Setenv("FAKE_EXIT", "7")
		t.Setenv("FAKE_STDOUT", wantStdout)
		outcome, err := runMechanicalShadow(context.Background(), mechanicalShadowJob{
			Model: "fixture/model", Prompt: "fixture prompt", Repository: repo, Scratch: scratch,
		}, fake, 45*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Stdout != wantStdout || outcome.ExitCode != 7 || outcome.TimedOut {
			t.Fatalf("outcome = %#v", outcome)
		}
		assertShadowScratchRemoved(t, scratch)
	})
	t.Run("timeout kills worker and removes scratch", func(t *testing.T) {
		repo, scratch := shadowFixture(t)
		t.Setenv("FAKE_SLEEP", "5s")
		t.Setenv("FAKE_STDOUT", "")
		outcome, err := runMechanicalShadow(context.Background(), mechanicalShadowJob{
			Model: "fixture/model", Prompt: "fixture prompt", Repository: repo, Scratch: scratch,
		}, fake, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.TimedOut || outcome.Stdout != "" {
			t.Fatalf("outcome = %#v", outcome)
		}
		assertShadowScratchRemoved(t, scratch)
	})
	t.Run("scratch inside repository is rejected without writing there", func(t *testing.T) {
		repo, _ := shadowFixture(t)
		scratch := filepath.Join(repo, "scratch")
		if _, err := runMechanicalShadow(context.Background(), mechanicalShadowJob{Repository: repo, Scratch: scratch}, fake, time.Second); err == nil {
			t.Fatal("runMechanicalShadow() error = nil, want repository overlap rejection")
		}
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			t.Fatalf("overlap created scratch in repository: %v", err)
		}
	})
}

func shadowFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "repository")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("repository-input"), 0600); err != nil {
		t.Fatal(err)
	}
	return repo, filepath.Join(base, "scratch")
}

func buildFakeOpenCode(t *testing.T) string {
	t.Helper()
	source := `package main
import ("fmt"; "os"; "path/filepath"; "time")
func main() {
 cwd, _ := os.Getwd(); data, _ := os.ReadFile(filepath.Join(cwd, "input.txt"))
 config := os.Getenv("OPENCODE_CONFIG_CONTENT")
 if path := os.Getenv("FAKE_OBSERVED"); path != "" { _ = os.WriteFile(path, []byte(cwd+"|"+string(data)+"|"+config), 0600) }
 fmt.Print(os.Getenv("FAKE_STDOUT"))
 if value := os.Getenv("FAKE_SLEEP"); value != "" { d, _ := time.ParseDuration(value); time.Sleep(d) }
 if value := os.Getenv("FAKE_EXIT"); value != "" { var code int; fmt.Sscan(value, &code); os.Exit(code) }
}`
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	if err := os.WriteFile(src, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "opencode")
	if os.PathSeparator == '\\' {
		path += ".exe"
	}
	if output, err := exec.Command("go", "build", "-o", path, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake OpenCode: %v: %s", err, output)
	}
	return path
}

func assertShadowScratchRemoved(t *testing.T, scratch string) {
	t.Helper()
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch remains: %v", err)
	}
}
