package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type eventFixture struct {
	Agent       string      `json:"agent"`
	Stdout      string      `json:"stdout"`
	Stderr      string      `json:"stderr"`
	ExitCode    int         `json:"exitCode"`
	Interrupted bool        `json:"interrupted"`
	Expected    EventResult `json:"expected"`
}

func TestEventFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_events")
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Python oracle fixtures found; run the oracle with PURSER_REGEN_GO_JOBS_EVENTS=1")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var fixture eventFixture
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			stream := NewEventStream(fixture.Agent)
			stream.Write([]byte(fixture.Stdout))
			actual := stream.Result(fixture.Interrupted, fixture.ExitCode, fixture.Stderr)
			if !reflect.DeepEqual(actual, fixture.Expected) {
				t.Fatalf("got %#v, want %#v", actual, fixture.Expected)
			}
		})
	}
}

func TestEventStreamRecordsThreadBeforeTerminalFailure(t *testing.T) {
	stream := NewEventStream("codex")
	threadID := stream.Write([]byte(`{"type":"thread.started","thread_id":"thread-resume"}` + "\n"))
	if threadID != "thread-resume" {
		t.Fatalf("thread ID not available before process exit: %q", threadID)
	}
	stream.Write([]byte(`{"type":"turn.failed","error":{"message":"failed"}}`))
	result := stream.Result(false, 1, "")
	if result.ThreadID != "thread-resume" || result.Status != "error" {
		t.Fatalf("unexpected result: %#v", result)
	}
}
