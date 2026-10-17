package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCursorReviewerIsUnavailableUntilInternallyEnabled(t *testing.T) {
	adapter, err := ReviewerAdapter("cursor")
	if err != nil {
		t.Fatal(err)
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor-agent")
	if runtime.GOOS == "windows" {
		cursorPath += ".cmd"
	}
	config := map[string]any{"reviewer1_model_requested": "claude-sonnet-4-5", "reviewer1_executable": cursorPath}
	if _, err := adapter.BuildCommand("review", reviewSchema, config, CommandOptions{WorkingDirectory: t.TempDir()}); err == nil {
		t.Fatal("Cursor reviewer command built without the internal enable flag")
	}
	config["reviewer1_internal_cursor_enabled"] = true
	command, err := adapter.BuildCommand("review", reviewSchema, config, CommandOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command, "\x00")
	if !strings.Contains(joined, "--mode\x00ask\x00--trust") || strings.Contains(joined, "--force") || strings.Contains(joined, "--yolo") {
		t.Fatalf("unsafe Cursor reviewer command: %#v", command)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(command[0]), ".cmd") {
		t.Fatalf("Windows Cursor executable = %q, want cursor-agent.cmd", command[0])
	}
}

func TestCursorAutoReviewerModel(t *testing.T) {
	adapter, err := ReviewerAdapter("cursor")
	if err != nil {
		t.Fatal(err)
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor-agent")
	config := map[string]any{"reviewer1_model_requested": "auto", "reviewer1_internal_cursor_enabled": true, "reviewer1_executable": cursorPath}
	command, err := adapter.BuildCommand("review", reviewSchema, config, CommandOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("Cursor auto reviewer command rejected: %v", err)
	}
	if !hasFlag(command, "--model", "auto") || !hasFlag(command, "--mode", "ask") || !hasFlag(command, "--trust", "") {
		t.Fatalf("Cursor auto reviewer argv = %#v", command)
	}
}

func TestCursorUnknownVendorReviewerIsRejected(t *testing.T) {
	adapter, err := ReviewerAdapter("cursor")
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ValidateModel("muse-spark-1.3-low"); err == nil {
		t.Fatal("unknown-vendor Cursor reviewer was accepted")
	}
}

func TestCursorReviewerConfinementAndEnvelope(t *testing.T) {
	adapter, err := ReviewerAdapter("cursor")
	if err != nil {
		t.Fatal(err)
	}
	command := []string{"cursor-agent", "--print"}
	name, data := adapter.ConfinementConfig(command)
	if name == "" || len(data) == 0 {
		t.Fatal("Cursor reviewer has no restrictive project permission configuration")
	}
	var confinement struct {
		Version     int `json:"version"`
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &confinement); err != nil {
		t.Fatal(err)
	}
	if confinement.Version != 1 || len(confinement.Permissions.Allow) != 1 || confinement.Permissions.Allow[0] != "Read(**)" || len(confinement.Permissions.Deny) < 4 {
		t.Fatalf("Cursor permissions are not deny-only: %s", data)
	}
	if _, err := adapter.ParseEnvelope(`{"session_id":"cursor-session","result":"{\"result\":\"NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE\",\"validation\":\"PASS\",\"findings\":[]}"}`); err != nil {
		t.Fatalf("valid Cursor response rejected: %v", err)
	}
	if _, err := adapter.ParseEnvelope(`{"session_id":"cursor-session","result":"not json"}`); err == nil {
		t.Fatal("invalid Cursor result was accepted")
	}
}

func TestCursorReviewerWritesSchemaAndPermissionSidecars(t *testing.T) {
	adapter, err := ReviewerAdapter("cursor")
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	executable := "cursor-agent"
	if runtime.GOOS == "windows" {
		executable += ".cmd"
	}
	config := map[string]any{"reviewer1_model_requested": "claude-sonnet-4-5", "reviewer1_internal_cursor_enabled": true, "reviewer1_executable": filepath.Join(cwd, executable)}
	command, err := adapter.BuildCommand("review", reviewSchema, config, CommandOptions{WorkingDirectory: cwd})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command, "\x00")
	schemaPath := filepath.Join(cwd, "reviewer1-output-schema.json")
	if _, err := os.Stat(schemaPath); err != nil {
		t.Fatalf("Cursor schema sidecar missing: %v", err)
	}
	if strings.Contains(joined, "--force") || strings.Contains(joined, "--yolo") {
		t.Fatalf("unsafe Cursor reviewer command: %#v", command)
	}
}

func TestNativeCursorReviewerGateAndEnabledResult(t *testing.T) {
	root, mock := nativeFixture(t)
	args := nativeArgs(root, mock, NoMaterialFinding)
	args["reviewer_agent"] = "cursor"
	args["reviewer_model"] = "claude-sonnet-4-5"
	isolateReviewerPath(t)
	got, err := RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Outcome != "unavailable" || got.Attributes["error_code"] != "REVIEWER_CURSOR_CONFINEMENT_UNVERIFIED" || got.Attributes["validation"] != "UNKNOWN" {
		t.Fatalf("default Cursor reviewer outcome = %#v, %v", got, err)
	}

	cursorMock := filepath.Join(root, "cursor-agent")
	if runtime.GOOS == "windows" {
		cursorMock += ".exe"
	}
	mockBytes, err := os.ReadFile(mock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorMock, mockBytes, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, ".claude", "hooks", "orchestrator", "config.json")
	config := map[string]any{
		"reviewer1_model_requested": "claude-sonnet-4-5", "reviewer1_timeout_seconds": 60,
		"reviewer1_packet_max_bytes": 200000, "reviewer1_internal_cursor_enabled": true,
	}
	encoded, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	args["reviewer_executable"] = cursorMock
	response, _ := json.Marshal(map[string]any{"result": NoMaterialFinding, "validation": "PASS", "findings": []any{}})
	envelope, _ := json.Marshal(map[string]string{"session_id": "cursor-session", "result": string(response)})
	t.Setenv("MOCK_OUTPUT", string(envelope))
	got, err = RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Outcome != "completed" || got.Attributes["conversation_id"] != "cursor-session" {
		t.Fatalf("enabled Cursor reviewer outcome = %#v, %v", got, err)
	}
}
