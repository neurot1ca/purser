package review

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBridgeOutcomeMappings(t *testing.T) {
	duration := 1.5
	tests := []struct {
		name  string
		value any
		want  map[string]any
	}{
		{"completed", CompletedReview{
			Result: NoMaterialFinding, Validation: "PASS", Findings: []Finding{{Code: "F1", Message: "finding"}},
			ConversationID: "conversation", Reviewer1ModelRequested: "review-model", DurationSeconds: &duration,
			Usage: map[string]any{"input_tokens": 3}, Provider: "codex", ServedModelAttestation: "ATTESTED", RetryCount: 2,
		}, map[string]any{
			"outcome": "completed", "review_status": "COMPLETED", "available": true, "provider": "codex",
			"result": NoMaterialFinding, "validation": "PASS", "findings": []map[string]string{{"code": "F1", "message": "finding"}},
			"conversation_id": "conversation", "reviewer1_model_requested": "review-model", "served_model_attestation": "ATTESTED",
			"usage": map[string]any{"input_tokens": 3},
		}},
		{"unavailable", UnavailableReview{ErrorCode: "NO_RESPONSE", Message: "unavailable", Reviewer1ModelRequested: "review-model", Provider: "opencode"}, map[string]any{
			"outcome": "unavailable", "review_status": "UNAVAILABLE", "available": false, "provider": "opencode",
			"result": Reviewer1Unavailable, "validation": "UNKNOWN", "findings": []map[string]string{{"code": "NO_RESPONSE", "message": "unavailable"}},
			"error_code": "NO_RESPONSE", "reviewer1_model_requested": "review-model", "served_model_attestation": "UNAVAILABLE",
		}},
		{"incomplete", IncompleteReview{Code: "PACKET_INCOMPLETE", Message: "omitted", Omitted: []string{"diff"}, Reviewer1ModelRequested: "review-model", Provider: "agy"}, map[string]any{
			"outcome": "incomplete", "review_status": "NOT_RUN_INCOMPLETE_PACKET", "available": true, "provider": "agy",
			"result": Uncertain, "validation": "UNKNOWN", "findings": []map[string]string{{"code": "PACKET_INCOMPLETE", "message": "omitted"}},
			"omitted": []string{"diff"}, "reviewer1_model_requested": "review-model", "served_model_attestation": "UNAVAILABLE",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bridgeOutcome(tc.value)
			if err != nil || got.Outcome != tc.want["outcome"] {
				t.Fatalf("bridgeOutcome = %#v, %v", got, err)
			}
			actual := map[string]any{"outcome": got.Outcome}
			for key, value := range got.Attributes {
				actual[key] = value
			}
			gotJSON, _ := json.Marshal(actual)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("bridge mapping = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestBridgeRejectsInvalidArguments(t *testing.T) {
	valid := map[string]any{
		"task_id": "task", "phase": "implementation", "route": "normal", "model": "coder", "effort": "high", "repo_root": "C:/repo",
		"delta": map[string]any{"changed_paths": []any{"file.py"}}, "classification": map[string]any{"in_scope_paths": []any{"file.py"}},
	}
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"unknown key", func(args map[string]any) { args["surprise"] = true }, "unknown key: surprise"},
		{"wrong type", func(args map[string]any) { args["delta"] = []any{} }, "delta must be an object"},
		{"missing required", func(args map[string]any) { delete(args, "task_id") }, "missing key: task_id"},
		{"non-object value", func(args map[string]any) { args["classification"] = "invalid" }, "classification must be an object or null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := make(map[string]any, len(valid)+1)
			for key, value := range valid {
				args[key] = value
			}
			tc.edit(args)
			_, err := parseBridgeArguments(args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseBridgeArguments error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestBridgePanicMapsToUnavailable(t *testing.T) {
	got, err := bridgePanic("test panic", map[string]any{"reviewer_agent": "agy", "reviewer_model": "review-model"})
	if err == nil || got.Outcome != "unavailable" {
		t.Fatalf("panic outcome = %#v, %v", got, err)
	}
	attrs := got.Attributes
	if attrs["reason"] != "REVIEWER_BRIDGE_EXCEPTION" || attrs["error_code"] != "REVIEWER_BRIDGE_EXCEPTION" || attrs["result"] != Reviewer1Unavailable || attrs["validation"] != "UNKNOWN" || attrs["reviewer1_model_requested"] != "review-model" {
		t.Fatalf("panic attributes = %#v", attrs)
	}
}
