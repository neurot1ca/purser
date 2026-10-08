package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMechanicalShadowCommandRunsJob(t *testing.T) {
	base := t.TempDir()
	binDir := filepath.Join(base, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	fakeSource := filepath.Join(base, "fake.go")
	fakeCode := "package main\nimport \"fmt\"\nfunc main() {\n" +
		"fmt.Println(`{\"type\":\"text\",\"part\":{\"text\":\"accepted report\"}}`)\n" +
		"fmt.Println(`{\"type\":\"step_finish\",\"part\":{\"reason\":\"stop\"}}`)\n}\n"
	if err := os.WriteFile(fakeSource, []byte(fakeCode), 0600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(binDir, "opencode")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	if output, err := exec.Command("go", "build", "-o", fake, fakeSource).CombinedOutput(); err != nil {
		t.Fatalf("build fake OpenCode: %v: %s", err, output)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root, repo := filepath.Join(base, "state"), filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "input.txt"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	inventory := `{"candidates":[{"id":"served","status":"measured","mechanical_samples":[true,true,true,false,false]},{"id":"shadow","status":"available","mechanical_samples":[true,true,true,true]}]}`
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), []byte(inventory), 0600); err != nil {
		t.Fatal(err)
	}
	project := "fixture-project"
	shadows := filepath.Join(root, "projects", project, "opencode_shadows")
	if err := os.MkdirAll(shadows, 0700); err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(shadows, "job.json")
	job := map[string]string{
		"model": "shadow", "prompt": "fixture prompt", "repository": repo,
		"scratch": filepath.Join(shadows, "run-fixture"), "accepted_report": "accepted report",
		"project_key": project, "state_root": root,
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(jobPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(shadows, "running.lock"), []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := mechanicalShadowMain([]string{jobPath}); code != 0 {
		t.Fatalf("mechanicalShadowMain() = %d, want 0", code)
	}
	for _, path := range []string{jobPath, filepath.Join(shadows, "running.lock"), job["scratch"]} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("artifact remains at %q: %v", path, err)
		}
	}
}
