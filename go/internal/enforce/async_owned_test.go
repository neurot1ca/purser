package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAsyncOwnedPass71TargetFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "async_owned.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string           `json:"oracle"`
		Cases  []map[string]any `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode async-owned fixture: %v", err)
	}
	if fixture.Oracle != "codex._async_owned+_async_job+_async_pre_followup" || len(fixture.Cases) == 0 {
		t.Fatalf("unexpected async-owned fixture: oracle=%q cases=%d", fixture.Oracle, len(fixture.Cases))
	}
	t.Skip("Pass 71 implements Go Async Pre parity against this Python oracle")
}
