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

func TestQuotaAndLaunchFailureP35Fixture(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "quota.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		QuotaNow   string `json:"quota_now"`
		QuotaCases []struct {
			Name  string `json:"name"`
			Files []struct {
				Day          int              `json:"day"`
				Name         string           `json:"name"`
				MTime        int64            `json:"mtime"`
				PrefixEvent  map[string]any   `json:"prefix_event"`
				PaddingBytes int              `json:"padding_bytes"`
				Events       []map[string]any `json:"events"`
			} `json:"files"`
			Expected map[string]any `json:"expected"`
		} `json:"quota_cases"`
		LaunchCases []struct {
			Name    string `json:"name"`
			Records []struct {
				ID         string         `json:"id"`
				ProjectKey string         `json:"project_key"`
				Job        map[string]any `json:"job"`
			} `json:"records"`
			Expected map[string]any `json:"expected"`
		} `json:"launch_cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339Nano, fixture.QuotaNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.QuotaCases {
		t.Run(test.Name, func(t *testing.T) {
			if !strings.Contains(test.Name, "p35") {
				t.Fatalf("case name lacks p35: %s", test.Name)
			}
			home := t.TempDir()
			for _, file := range test.Files {
				day := now.AddDate(0, 0, -file.Day)
				folder := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
				if err := os.MkdirAll(folder, 0700); err != nil {
					t.Fatal(err)
				}
				var content []byte
				if file.PrefixEvent != nil {
					line, err := json.Marshal(file.PrefixEvent)
					if err != nil {
						t.Fatal(err)
					}
					content = append(append(line, '\n'), []byte(strings.Repeat("x", file.PaddingBytes))...)
				} else {
					for _, event := range file.Events {
						line, err := json.Marshal(event)
						if err != nil {
							t.Fatal(err)
						}
						content = append(append(content, line...), '\n')
					}
				}
				path := filepath.Join(folder, file.Name)
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
				stamp := time.Unix(file.MTime, 0)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			got, _, ok := latestQuotaAt(home, now)
			if !ok {
				got = nil
			}
			if got != nil {
				if source, valid := got["source_identity"].(string); !valid || source == "" {
					t.Fatalf("quota source identity missing: %#v", got)
				}
				delete(got, "source_identity")
			}
			if !reflect.DeepEqual(got, test.Expected) {
				t.Fatalf("quota = %#v, want %#v", got, test.Expected)
			}
		})
	}
	for _, test := range fixture.LaunchCases {
		t.Run(test.Name, func(t *testing.T) {
			if !strings.Contains(test.Name, "p35") {
				t.Fatalf("case name lacks p35: %s", test.Name)
			}
			root := t.TempDir()
			indexes := filepath.Join(root, "async_jobs")
			jobs := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(indexes, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(jobs, 0700); err != nil {
				t.Fatal(err)
			}
			for _, record := range test.Records {
				index, err := json.Marshal(map[string]any{"jobId": record.ID, "project_key": record.ProjectKey})
				if err != nil {
					t.Fatal(err)
				}
				job, err := json.Marshal(record.Job)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(indexes, record.ID+".json"), index, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(jobs, record.ID+".json"), job, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := lastLaunchFailure(root, "project"); !reflect.DeepEqual(got, test.Expected) {
				t.Fatalf("launch failure = %#v, want %#v", got, test.Expected)
			}
		})
	}
}
