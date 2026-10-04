package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func TestWriteUndeliveredMarkerCleansUpRejectedAsyncStart(t *testing.T) {
	root := t.TempDir()
	event := undeliveredEvent("mcp__plugin_purser_codex-main__codex_start", "p67-start")
	callKey := undeliveredCallKey(event)
	project := "p67-project"
	pendingPath := filepath.Join(root, "pending", callKey+".json")
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
		t.Fatal(err)
	}
	pending := map[string]any{"project_key": project, "lease_owner": callKey, "async_ready": true}
	if err := atomicAdmissionJSON(pendingPath, pending); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(root, "projects", project)
	if got := lease.New(projectRoot).Acquire(callKey, "p67-task", []string{"src/a.go"}, lease.AcquireOptions{JobID: strPtr("p67-job"), TimeoutSeconds: 60}, float64(time.Now().Unix())); got.Status != "OK" {
		t.Fatalf("lease acquire = %#v", got)
	}
	session := ledger.BuildSessionRecord("p67-task", nil, "", "", "", map[string]any{"pending_call_baseline": nil, "last_outcome": nil})
	if saved := ledger.Save(projectRoot, session); saved.Status != ledger.OK {
		t.Fatalf("session save = %#v", saved)
	}
	sessionPath, _ := ledger.PathForTask(projectRoot, "p67-task")
	sessionBefore, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteUndeliveredMarker(root, event); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending record remains: %v", err)
	}
	leaseData, err := os.ReadFile(filepath.Join(projectRoot, "scope_leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var leaseState map[string]any
	if err := json.Unmarshal(leaseData, &leaseState); err != nil {
		t.Fatal(err)
	}
	owner := mapValue(mapValue(leaseState["owners"])[callKey])
	if owner["state"] == "ACTIVE" || owner["state"] == "QUEUED" {
		t.Fatalf("lease remains admitted: %#v", owner)
	}
	marker := filepath.Join(root, "pending", "undelivered-"+callKey+".json")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("undelivered marker remains: %v", err)
	}
	sessionAfter, err := os.ReadFile(sessionPath)
	if err != nil || string(sessionAfter) != string(sessionBefore) {
		t.Fatalf("session ledger changed: %v", err)
	}
}

func TestWriteUndeliveredMarkerMissingAsyncStartStateIsNoop(t *testing.T) {
	root := t.TempDir()
	event := undeliveredEvent("mcp__codex-main__codex_start", "p67-empty")
	if err := WriteUndeliveredMarker(root, event); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("files created for missing state: %v %v", entries, err)
	}
}

func TestWriteUndeliveredMarkerLeavesSyncStateAndWritesMarker(t *testing.T) {
	root := t.TempDir()
	event := undeliveredEvent("mcp__plugin_purser_codex-main__codex", "p67-sync")
	callKey := undeliveredCallKey(event)
	pendingPath := filepath.Join(root, "pending", callKey+".json")
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte(`{"kind":"pending","lease_owner":"owner"}`), 0600); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(root, "projects", "p67-sync-project")
	if got := lease.New(projectRoot).Acquire("owner", "sync-task", []string{"src/b.go"}, lease.AcquireOptions{}, float64(time.Now().Unix())); got.Status != "OK" {
		t.Fatalf("sync lease acquire = %#v", got)
	}
	leasePath := filepath.Join(projectRoot, "scope_leases.json")
	leaseBefore, err := os.ReadFile(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteUndeliveredMarker(root, event); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("sync pending was changed: %v", err)
	}
	marker := filepath.Join(root, "pending", "undelivered-"+callKey+".json")
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("sync marker missing: %v", err)
	}
	var markerValue map[string]any
	if err := json.Unmarshal(markerData, &markerValue); err != nil {
		t.Fatal(err)
	}
	if markerValue["schema"] != "purser.undelivered.v1" || markerValue["call_key"] != callKey ||
		markerValue["tool_name"] != event["tool_name"] || markerValue["reason"] != "adapter failed" {
		t.Fatalf("sync marker = %#v", markerValue)
	}
	leaseAfter, err := os.ReadFile(leasePath)
	if err != nil || string(leaseAfter) != string(leaseBefore) {
		t.Fatalf("sync lease was changed: %v", err)
	}
}

func undeliveredEvent(tool, id string) map[string]any {
	return map[string]any{"hook_event_name": "WriteUndeliveredMarker", "session_id": "p67-session",
		"tool_use_id": id, "tool_name": tool, "reason": "adapter failed"}
}

func undeliveredCallKey(event map[string]any) string {
	sum := sha256.Sum256([]byte(event["session_id"].(string) + "\x00" + event["tool_use_id"].(string)))
	return hex.EncodeToString(sum[:])
}

func strPtr(value string) *string { return &value }
