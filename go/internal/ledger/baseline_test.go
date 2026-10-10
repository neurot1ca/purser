package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var baselineGoldenPath = filepath.Join("testdata", "capture.json")

func TestBaselineCapturePythonGolden(t *testing.T) {
	for name, expected := range baselineExpectations(t) {
		if name != "scoped" && name != "unscoped" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			root := initGitRepository(t)
			baselineFixtureRepository(t, root)
			var scope []string
			if name == "scoped" {
				scope = []string{"scope"}
			}
			assertBaselineExpected(t, captureBaseline(root, scope, time.Second, 3), expected)
		})
	}
}

func TestBaselineCaptureRejectsAtOwnBranch(t *testing.T) {
	expected := baselineExpectations(t)
	root := initGitRepository(t)
	assertBaselineExpected(t, captureBaseline(root, []string{"../escape"}, time.Second, 3), expected["invalid_scope"])
	assertBaselineExpected(t, captureBaseline(root, nil, time.Second, 3), expected["git_failure"])
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	assertBaselineExpected(t, captureBaseline(root, []string{"scope"}, time.Second, 1e-12), expected["manifest_deadline"])
	root = initGitRepository(t)
	baselineFixtureRepository(t, root)
	oldClock, base, calls := manifestClock, time.Now(), 0
	manifestClock = func() time.Time {
		calls++
		if calls == 6 {
			_ = os.Remove(filepath.Join(root, "scope", "new.txt"))
		}
		return base.Add(time.Duration(calls) * time.Millisecond)
	}
	t.Cleanup(func() { manifestClock = oldClock })
	assertBaselineExpected(t, captureBaseline(root, []string{"scope"}, time.Second, 3), expected["filesystem_failure"])
}

func baselineExpectations(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(baselineGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string          `json:"name"`
			Expected json.RawMessage `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	expected := make(map[string]json.RawMessage, len(fixture.Cases))
	for _, test := range fixture.Cases {
		expected[test.Name] = test.Expected
	}
	return expected
}

func assertBaselineExpected(t *testing.T, got map[string]any, expected json.RawMessage) {
	t.Helper()
	actual, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var actualValue, expectedValue any
	if err := json.Unmarshal(actual, &actualValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &expectedValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualValue, expectedValue) {
		t.Fatalf("baseline mismatch\n got: %s\nwant: %s", actual, expected)
	}
}

func baselineFixtureRepository(t *testing.T, root string) {
	t.Helper()
	tracked := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tracked, 0666); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "tracked.txt")
	t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00+00:00")
	t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00+00:00")
	command := []string{"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture"}
	git(t, root, command...)
	if err := os.WriteFile(tracked, []byte("work\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tracked, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scope"), 0700); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(root, "scope", "new.txt")
	if err := os.WriteFile(newFile, []byte("new\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(newFile, 0666); err != nil {
		t.Fatal(err)
	}
}
