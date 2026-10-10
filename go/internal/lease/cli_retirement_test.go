package lease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestActiveListsTaskScopeAgeAndJobState(t *testing.T) {
	state := t.TempDir()
	project := filepath.Join(state, "projects", "project-one")
	jobID := sha256Hex("cli-job")
	record := readLeaseTestdata(t, "cli_active.json")
	record = strings.ReplaceAll(record, "__JOB_ID__", jobID)
	writeLeaseRecord(t, project, record)
	jobs := filepath.Join(state, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobs, 0700); err != nil {
		t.Fatal(err)
	}
	job, err := json.Marshal(map[string]any{"jobId": jobID, "call": "cli-job", "state": "RUNNING"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, jobID+".json"), job, 0600); err != nil {
		t.Fatal(err)
	}

	rows, err := New(project).Active(2000000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("active rows = %#v, want two", rows)
	}
	byTask := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		byTask[row["task_id"].(string)] = row
	}
	if !reflect.DeepEqual(byTask["task-sync"]["scope"], []any{"src/a.py"}) ||
		!reflect.DeepEqual(byTask["task-async"]["scope"], []any{"src/b.py"}) {
		t.Fatalf("active task scopes = %#v", byTask)
	}
	if byTask["task-sync"]["age_seconds"] != float64(10) || byTask["task-sync"]["job_state"] != nil {
		t.Fatalf("sync active row = %#v", byTask["task-sync"])
	}
	if byTask["task-async"]["age_seconds"] != float64(20) || byTask["task-async"]["job_state"] != "RUNNING" {
		t.Fatalf("async active row = %#v", byTask["task-async"])
	}
}

func TestReleaseTaskOnlyReleasesRequestedActiveLeasesAcrossProjects(t *testing.T) {
	state := t.TempDir()
	projectOne := filepath.Join(state, "projects", "project-one")
	projectTwo := filepath.Join(state, "projects", "project-two")
	writeLeaseRecord(t, projectOne, readLeaseTestdata(t, "cli_release.json"))
	writeLeaseRecord(t, projectTwo, `{"version":1,"owners":{"target-owner":{"task_id":"target-task","scope":["docs"],"state":"ACTIVE","acquired_at":1999999990}},"conflicts":[]}`)

	var released []map[string]any
	for projectKey, project := range map[string]string{"project-one": projectOne, "project-two": projectTwo} {
		rows, err := New(project).ReleaseTask("target-task", 2000000000)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			row["project_key"] = projectKey
			released = append(released, row)
		}
	}
	got := make(map[string]string, len(released))
	for _, row := range released {
		got[row["project_key"].(string)] = strings.Join(toStringSlice(row["scope"]), ",")
	}
	if !reflect.DeepEqual(got, map[string]string{"project-one": "src/a.py", "project-two": "docs"}) {
		t.Fatalf("released leases = %#v", released)
	}

	active, err := New(projectOne).Active(2000000001)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0]["task_id"] != "other-task" {
		t.Fatalf("remaining project-one active leases = %#v", active)
	}
	again, err := New(projectOne).ReleaseTask("target-task", 2000000002)
	if err != nil || len(again) != 0 {
		t.Fatalf("repeated release = %#v, %v; want no rows", again, err)
	}
}

func readLeaseTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeLeaseRecord(t *testing.T, project, record string) {
	t.Helper()
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "scope_leases.json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func toStringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		result = append(result, item.(string))
	}
	return result
}
