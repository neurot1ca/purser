package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type recoverFixture struct {
	Oracle string `json:"oracle"`
	Cases  []struct {
		Name   string         `json:"name"`
		Job    map[string]any `json:"job"`
		Index  map[string]any `json:"index"`
		Launch map[string]any `json:"launch"`
		Probes struct {
			Host         string `json:"host"`
			GroupStopped bool   `json:"groupStopped"`
		} `json:"probes"`
		Result string         `json:"result"`
		Record map[string]any `json:"record"`
	} `json:"cases"`
}

func TestRecoverPythonParity(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_recover", "oracle.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture recoverFixture
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := scratch(t)
			id := c.Job["jobId"].(string)
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			jobData, _ := json.Marshal(c.Job)
			record, e := Decode(jobData)
			if e != nil {
				t.Fatal(e)
			}
			if e = Write(filepath.Join(dir, id+".json"), record); e != nil {
				t.Fatal(e)
			}
			if c.Index != nil {
				indexData, _ := json.Marshal(c.Index)
				if e = os.MkdirAll(filepath.Join(root, "async_jobs"), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(root, "async_jobs", id+".json"), indexData, 0600); e != nil {
					t.Fatal(e)
				}
			}
			launchData, _ := json.Marshal(c.Launch)
			if e = os.WriteFile(filepath.Join(dir, id+".launch"), launchData, 0600); e != nil {
				t.Fatal(e)
			}
			got := RecoverJob(root, id, 2_000_000_000, RecoverOptions{
				ProcessAlive: func(int, int64) string { return c.Probes.Host },
				GroupStopped: func(int) bool { return c.Probes.GroupStopped },
				Replace:      func(string, string, *Record, float64) string { return "RECOVERED" },
			})
			if got != c.Result {
				t.Fatalf("result = %s, want %s", got, c.Result)
			}
			switch c.Name {
			case "live_host", "dead_child_live", "missing_host", "invalid_attempts", "exhausted_attempts", "missing_reply_thread":
				if got == "RECOVERED" {
					t.Fatalf("failure direction %s returned RECOVERED", c.Name)
				}
			}
			if c.Record != nil {
				actual := recordMap(readJob(dir, id))
				if !reflect.DeepEqual(actual, c.Record) {
					t.Fatalf("record mismatch:\n got %#v\nwant %#v", actual, c.Record)
				}
			}
		})
	}
}

func TestRecoverWithoutLauncherFailsConfirmedDeadHost(t *testing.T) {
	root := scratch(t)
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	job, err := Decode([]byte(`{"jobId":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","state":"RUNNING","generation":1,"ownershipToken":"owner","hostPid":123,"hostCreatedAt":456,"hostContained":true,"pid":789}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = Write(filepath.Join(dir, id+".json"), job); err != nil {
		t.Fatal(err)
	}
	launch := []byte(`{"name":"codex","args":{"prompt":"resume"}}`)
	if err = os.WriteFile(filepath.Join(dir, id+".launch"), launch, 0600); err != nil {
		t.Fatal(err)
	}
	got := RecoverJob(root, id, 2_000_000_000, RecoverOptions{
		ProcessAlive: func(int, int64) string { return "DEAD" },
		GroupStopped: func(int) bool { return true },
	})
	if got != "FAILED" {
		t.Fatalf("default recovery result = %s, want FAILED", got)
	}
	final := recordMap(readJob(dir, id))
	if final["state"] != "FAILED" || final["errorCode"] != "HOST_DIED" {
		t.Fatalf("final record = %#v", final)
	}
}

func TestRecoverConsumedJobWithDeadHostAndExitCodeFinalizesFailure(t *testing.T) {
	root := scratch(t)
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	job, err := Decode([]byte(`{"jobId":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","state":"RUNNING","generation":1,"ownershipToken":"owner","hostPid":123,"hostCreatedAt":456,"hostContained":true,"pid":789,"exitCode":0,"leaseQueued":true,"startedAt":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = Write(filepath.Join(dir, id+".json"), job); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "async_jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "async_jobs", id+".json"), []byte(`{"stage":"CONSUMED"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := RecoverJob(root, id, 2_000_000_000, RecoverOptions{
		ProcessAlive: func(int, int64) string { return "DEAD" },
		GroupStopped: func(int) bool { return true },
	})
	if got != "FINALIZED" {
		t.Fatalf("recovery result = %s, want FINALIZED", got)
	}
	final := recordMap(readJob(dir, id))
	if final["state"] != "FAILED" || final["errorCode"] != "HOST_FINALIZATION_INTERRUPTED" {
		t.Fatalf("final record = %#v", final)
	}
}

func TestRecoverConsumedJobWithLiveHostDoesNotFinalizeExitCode(t *testing.T) {
	root := scratch(t)
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	job, err := Decode([]byte(`{"jobId":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","state":"RUNNING","generation":1,"hostPid":123,"hostCreatedAt":456,"hostContained":true,"exitCode":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = Write(filepath.Join(dir, id+".json"), job); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "async_jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "async_jobs", id+".json"), []byte(`{"stage":"CONSUMED"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := RecoverJob(root, id, 2_000_000_000, RecoverOptions{
		ProcessAlive: func(int, int64) string { return "ALIVE" },
	})
	if got != "UNCHANGED" {
		t.Fatalf("recovery result = %s, want UNCHANGED", got)
	}
	final := recordMap(readJob(dir, id))
	if final["state"] != "RUNNING" {
		t.Fatalf("live host record was changed: %#v", final)
	}
}
