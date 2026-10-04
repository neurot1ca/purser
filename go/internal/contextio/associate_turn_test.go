package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestP62AssociateTurnFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "turn_association.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name           string `json:"name"`
			Session        string `json:"session_id"`
			Task           any    `json:"task_id"`
			Project        any    `json:"project_key"`
			Initial        any    `json:"initial_journal"`
			Contention     bool   `json:"contention"`
			LockContention bool   `json:"lock_contention"`
			Want           bool   `json:"expected_return"`
			Journal        any    `json:"expected_journal"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "context_io_telemetry.associate_turn" {
		t.Fatalf("invalid association fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			key := sessionKey(tc.Session)
			journal := ""
			contention := ""
			if key != "" {
				journal = filepath.Join(root, "context_io", "turns", key+".json")
				contention = filepath.Join(root, "context_io", "contention", key)
			}
			if tc.Initial != nil && journal != "" {
				if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
					t.Fatal(err)
				}
				switch initial := tc.Initial.(type) {
				case string:
					if err := os.WriteFile(journal, []byte(initial), 0600); err != nil {
						t.Fatal(err)
					}
				case map[string]any:
					initial["opened_at"] = float64(time.Now().UnixNano()) / 1e9
					encoded, err := json.Marshal(initial)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(journal, encoded, 0600); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unsupported initial journal %T", tc.Initial)
				}
			}
			if tc.Contention {
				if err := os.MkdirAll(filepath.Dir(contention), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(contention, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.LockContention {
				lockPath := filepath.Join(root, "context_io", "locks", key+".lock")
				if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
					t.Fatal(err)
				}
				lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if err := tryLock(lock); err != nil {
					lock.Close()
					t.Fatalf("lock: %v", err)
				}
				got := AssociateTurn(root, tc.Session, tc.Task, tc.Project)
				_ = unlock(lock)
				_ = lock.Close()
				if got != tc.Want {
					t.Fatalf("return = %v, want %v", got, tc.Want)
				}
			} else if got := AssociateTurn(root, tc.Session, tc.Task, tc.Project); got != tc.Want {
				t.Fatalf("return = %v, want %v", got, tc.Want)
			}
			var gotJournal any
			if journal != "" {
				if raw, err := os.ReadFile(journal); err == nil {
					var decoded any
					if json.Unmarshal(raw, &decoded) == nil {
						if obj, ok := decoded.(map[string]any); ok {
							obj["opened_at"] = "$NOW"
						}
						gotJournal = decoded
					} else {
						gotJournal = string(raw)
					}
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(gotJournal, tc.Journal) {
				gotJSON, _ := json.Marshal(gotJournal)
				wantJSON, _ := json.Marshal(tc.Journal)
				if strings.TrimSpace(string(gotJSON)) != strings.TrimSpace(string(wantJSON)) {
					t.Fatalf("journal = %s, want %s", gotJSON, wantJSON)
				}
			}
		})
	}
}
