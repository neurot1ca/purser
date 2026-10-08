package enforce

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"strconv"
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

func TestMechanicalShadowCompletionOracle(t *testing.T) {
	fake := buildFakeOpenCode(t)
	cases := []struct {
		name, stdout, exit, status string
		timeout                    time.Duration
		ok                         bool
		in, out, total             int
	}{
		{"success", "{\"type\":\"text\",\"part\":{\"text\":\"Changed parser to retain valid free candidates.\"}}\n{\"type\":\"step_finish\",\"part\":{\"reason\":\"stop\",\"tokens\":{\"input\":10,\"output\":4,\"total\":14}}}\n", "", "completed", time.Second, true, 10, 4, 14},
		{"worker failure", "{\"type\":\"step_finish\",\"part\":{\"reason\":\"error\",\"tokens\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n", "7", "failed", time.Second, false, 3, 2, 5},
		{"timeout", "", "", "failed", 100 * time.Millisecond, false, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_STDOUT", tc.stdout)
			t.Setenv("FAKE_EXIT", tc.exit)
			t.Setenv("FAKE_SLEEP", "")
			if tc.name == "timeout" {
				t.Setenv("FAKE_SLEEP", "5s")
			}
			repo, scratch := shadowFixture(t)
			root := t.TempDir()
			project := "0123456789abcdef"
			_ = os.MkdirAll(filepath.Join(root, "opencode"), 0700)
			inventory := `{"candidates":[{"id":"served","status":"measured","mechanical_samples":[true,true,true,false,false]},{"id":"shadow","status":"available","mechanical_samples":[true,true,true,true]}]}`
			_ = os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), []byte(inventory), 0600)
			shadows := filepath.Join(root, "projects", project, "opencode_shadows")
			_ = os.MkdirAll(shadows, 0700)
			job := mechanicalShadowRecord{Model: "shadow", Prompt: "fixture prompt", Repository: repo, Scratch: scratch, AcceptedReport: "Changed parser to retain valid free candidates.", ProjectKey: project, StateRoot: root}
			raw, _ := json.Marshal(job)
			jobPath := filepath.Join(shadows, "fixture.json")
			_ = os.WriteFile(jobPath, raw, 0600)
			_ = os.WriteFile(filepath.Join(shadows, "running.lock"), []byte("1"), 0600)
			if got := runMechanicalShadowJob(context.Background(), jobPath, fake, tc.timeout); got != tc.ok {
				t.Fatalf("result=%v", got)
			}
			for _, p := range []string{jobPath, scratch, filepath.Join(shadows, "running.lock")} {
				if _, e := os.Stat(p); !os.IsNotExist(e) {
					t.Fatalf("artifact remains %s", p)
				}
			}
			data, _ := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
			sample := "[true,true,true,true,false]"
			if tc.ok {
				sample = "[true,true,true,true,true]"
			}
			if !strings.Contains(string(data), sample) {
				t.Fatalf("samples: %s", data)
			}
			data, e := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "shadow-job.jsonl"))
			if e != nil {
				t.Fatal(e)
			}
			events := string(data)
			if !strings.Contains(events, "MECHANICAL_MODEL_RANK_CHANGED") || !strings.Contains(events, "SHADOW_FINISHED") || !strings.Contains(events, `"status":"`+tc.status+`"`) {
				t.Fatalf("telemetry: %s", events)
			}
			for key, want := range map[string]int{"input_tokens": tc.in, "output_tokens": tc.out, "total_tokens": tc.total} {
				if want > 0 && !strings.Contains(events, `"`+key+`":`+strconv.Itoa(want)) {
					t.Fatalf("missing %s: %s", key, events)
				}
			}
			if tc.ok && !strings.Contains(events, `"agreement":true`) {
				t.Fatalf("missing agreement: %s", events)
			}
		})
	}
}
func TestRecoverStaleMechanicalShadow(t *testing.T) {
	s := t.TempDir()
	lock := filepath.Join(s, "running.lock")
	stale := []string{lock, filepath.Join(s, "stale.json"), filepath.Join(s, "run-old")}
	_ = os.WriteFile(lock, []byte("2147483647"), 0600)
	old := time.Now().Add(-mechanicalShadowLockMaxAge - time.Second)
	_ = os.Chtimes(lock, old, old)
	_ = os.WriteFile(stale[1], []byte("{}"), 0600)
	_ = os.Mkdir(stale[2], 0700)
	if ok, e := recoverStaleMechanicalShadow(s, time.Now()); e != nil || !ok {
		t.Fatalf("stale recovery=%v,%v", ok, e)
	}
	for _, p := range stale {
		if _, e := os.Stat(p); !os.IsNotExist(e) {
			t.Fatalf("stale artifact remains: %s", p)
		}
	}
	_ = os.WriteFile(lock, []byte("1"), 0600)
	if ok, e := recoverStaleMechanicalShadow(s, time.Now()); e != nil || ok {
		t.Fatalf("live recovery=%v,%v", ok, e)
	}
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
	if output, err := testutil.Hide(exec.Command("go", "build", "-o", path, src)).CombinedOutput(); err != nil {
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
