package ledger

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeJobDeltaPythonFixture(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "job_delta.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string `json:"name"`
			Snapshot string `json:"snapshot"`
			Changes  []struct {
				Op         string `json:"op"`
				Path       string `json:"path"`
				Content    string `json:"content"`
				ContentHex string `json:"content_hex"`
			} `json:"changes"`
			Result map[string]any `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root := initGitRepository(t)
			if test.Snapshot == "missing" {
				got := ComputeJobDelta(filepath.Join(root, "absent"), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				assertJobDeltaExpected(t, got, test.Result)
				return
			}
			for name, content := range map[string][]byte{"tracked.txt": []byte("base\n"), "delete.txt": []byte("delete\n"), "binary.dat": {0, 1, 2, 255}} {
				if err := os.WriteFile(filepath.Join(root, name), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			git(t, root, "add", "--all")
			git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
			snapshot := stringsOutput(t, root, "rev-parse", "HEAD^{tree}")
			if test.Snapshot == "commit" {
				snapshot = stringsOutput(t, root, "rev-parse", "HEAD")
			}
			if test.Snapshot == "invalid" {
				snapshot = "invalid"
			}
			for _, change := range test.Changes {
				path := filepath.Join(root, filepath.FromSlash(change.Path))
				if change.Op == "delete" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					continue
				}
				content := []byte(change.Content)
				if change.ContentHex != "" {
					var decodeErr error
					content, decodeErr = hex.DecodeString(change.ContentHex)
					if decodeErr != nil {
						t.Fatal(decodeErr)
					}
				}
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := ComputeJobDelta(root, snapshot)
			assertJobDeltaExpected(t, got, test.Result)
		})
	}
}

func TestJobChangedPathsSHA256BindsDiffContent(t *testing.T) {
	root := initGitRepository(t)
	path := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "--all")
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	snapshot := stringsOutput(t, root, "rev-parse", "HEAD^{tree}")
	if err := os.WriteFile(path, []byte("first content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := JobChangedPathsSHA256(root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("different content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := JobChangedPathsSHA256(root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("revision did not change with diff content: %s", first)
	}
}

func stringsOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
func assertJobDeltaExpected(t *testing.T, got, want map[string]any) {
	t.Helper()
	got = cloneMap(got)
	if got["baseline_head"] != nil {
		got["baseline_head"] = "<SNAPSHOT>"
	}
	if got["current_head"] != nil {
		got["current_head"] = "<HEAD>"
	}
	left, _ := json.Marshal(got)
	right, _ := json.Marshal(want)
	if string(left) != string(right) {
		t.Fatalf("job delta mismatch\n got: %s\nwant: %s", left, right)
	}
}
