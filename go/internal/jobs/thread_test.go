package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type threadFixture struct {
	Cases []struct {
		Name        string         `json:"name"`
		ThreadID    string         `json:"thread_id"`
		Initial     map[string]any `json:"initial,omitempty"`
		Record      map[string]any `json:"record"`
		RecordJSON  string         `json:"record_json"`
		Loaded      map[string]any `json:"loaded"`
		ReplyConfig map[string]any `json:"reply_config"`
		Metadata    []struct {
			Args     map[string]any `json:"args"`
			Recorded map[string]any `json:"recorded"`
		} `json:"metadata"`
		Corrupt string `json:"corrupt"`
	} `json:"cases"`
}

func TestThreadParity(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_thread", "records.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture threadFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.Name == "missing" {
				if got := LoadRecord(dir, tc.ThreadID); got != nil {
					t.Fatalf("missing record = %#v, want nil", got)
				}
				return
			}
			if tc.Name == "corrupt" {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(RecordPath(dir, tc.ThreadID), []byte(tc.Corrupt), 0o600); err != nil {
					t.Fatal(err)
				}
				if got := LoadRecord(dir, tc.ThreadID); got != nil {
					t.Fatalf("corrupt record = %#v, want nil", got)
				}
				return
			}
			if tc.Initial != nil {
				if err := SaveRecord(dir, tc.Initial); err != nil {
					t.Fatal(err)
				}
			}
			if err := SaveRecord(dir, tc.Record); err != nil {
				t.Fatal(err)
			}
			gotBytes, err := os.ReadFile(RecordPath(dir, tc.ThreadID))
			if err != nil {
				t.Fatal(err)
			}
			if string(gotBytes) != tc.RecordJSON {
				t.Fatalf("saved bytes = %s, want %s", gotBytes, tc.RecordJSON)
			}
			var want, got any
			if err := json.Unmarshal([]byte(tc.RecordJSON), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(gotBytes, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("saved JSON = %s, want semantically %s", gotBytes, tc.RecordJSON)
			}
			loaded := LoadRecord(dir, tc.ThreadID)
			if !reflect.DeepEqual(loaded, tc.Loaded) {
				t.Fatalf("loaded = %#v, want %#v", loaded, tc.Loaded)
			}
			if tc.ReplyConfig != nil {
				reply := ReplyConfig(loaded)
				for key, want := range tc.ReplyConfig {
					if !reflect.DeepEqual(reply[key], want) {
						t.Fatalf("reply config %q = %#v, want captured launch value %#v", key, reply[key], want)
					}
				}
			}
			for _, metadata := range tc.Metadata {
				if got := ReplyMetadata(metadata.Args); !reflect.DeepEqual(got, metadata.Recorded) {
					t.Fatalf("reply metadata = %#v, want captured job fields %#v", got, metadata.Recorded)
				}
			}
		})
	}
}

func TestThreadStateDirOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "state")
	t.Setenv("PURSER_CODEX_SHIM_STATE", override)
	t.Setenv("PURSER_STATE_DIR", filepath.Join(t.TempDir(), "ignored"))
	want, err := filepath.Abs(override)
	if err != nil {
		t.Fatal(err)
	}
	if got := StateDir(); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}
