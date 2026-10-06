package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestPostGearAdviceUsesCompletionLaneDurationAndClaimPreventsReplay(t *testing.T) {
	for _, lane := range []string{"sync", "async"} {
		t.Run(lane, func(t *testing.T) {
			root, quotaHome, key := gearAdviceFixture(t)
			t.Setenv("CODEX_HOME", quotaHome)
			if lane == "sync" {
				event := map[string]any{"duration_ms": float64(1500)}
				response := map[string]any{}
				postGearAdvice(root, event, gearPending(key), gearConfig(), response, numberOr(event["duration_ms"], 0), "")
				if !strings.Contains(stringValue(mapValue(response["hookSpecificOutput"])["additionalContext"]), "/purser:gear-up") {
					t.Fatalf("sync worker duration did not produce advice: %#v", response)
				}
				replay := map[string]any{}
				postGearAdvice(root, event, gearPending(key), gearConfig(), replay, numberOr(event["duration_ms"], 0), "")
				if replay["hookSpecificOutput"] != nil {
					t.Fatalf("sync replay duplicated advice: %#v", replay)
				}
				return
			}
			// Collection duration is short; only the job record's worker interval
			// crosses the route p90 threshold.
			job := map[string]any{"startedAt": float64(100), "finishedAt": float64(101.5)}
			event := map[string]any{"duration_ms": float64(5)}
			response := map[string]any{}
			postGearAdvice(root, event, gearPending(key), gearConfig(), response,
				workerDuration(job["startedAt"], job["finishedAt"]), "completed-job")
			if !strings.Contains(stringValue(mapValue(response["hookSpecificOutput"])["additionalContext"]), "/purser:gear-up") {
				t.Fatalf("async worker duration did not produce advice: %#v", response)
			}
			replay := map[string]any{}
			postGearAdvice(root, event, gearPending(key), gearConfig(), replay,
				workerDuration(job["startedAt"], job["finishedAt"]), "completed-job")
			if replay["hookSpecificOutput"] != nil {
				t.Fatalf("async replay duplicated advice: %#v", replay)
			}
		})
	}
}

func gearAdviceFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root, quotaHome, key := t.TempDir(), t.TempDir(), "0123456789abcdef"
	now := time.Now().UTC()
	day := filepath.Join(quotaHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	quota := map[string]any{"timestamp": now.Format(time.RFC3339Nano), "payload": map[string]any{
		"plan_type": "plus", "rate_limits": map[string]any{
			"primary":   map[string]any{"window_minutes": 300, "used_percent": 10.0, "resets_at": float64(now.Add(2 * time.Hour).Unix())},
			"secondary": map[string]any{"window_minutes": 10080, "used_percent": 20.0, "resets_at": float64(now.Add(6 * 24 * time.Hour).Unix())},
		},
	}}
	data, _ := json.Marshal(quota)
	if err := os.WriteFile(filepath.Join(day, "rollout-fixture.jsonl"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "telemetry", "events", now.Format("2006-01"))
	if err := os.MkdirAll(events, 0700); err != nil {
		t.Fatal(err)
	}
	lines := make([]byte, 0)
	for i := 0; i < 10; i++ {
		line, _ := json.Marshal(map[string]any{"event_type": "IMPLEMENTER_FINISHED", "project_key": key,
			"ts_utc": now.Format(time.RFC3339Nano), "payload": map[string]any{"route": "mechanical", "duration_ms": 1000.0, "implementer": "codex-main"}})
		lines = append(lines, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(events, "session_fixture.jsonl"), lines, 0600); err != nil {
		t.Fatal(err)
	}
	return root, quotaHome, key
}

func gearPending(key string) map[string]any {
	return map[string]any{"agent": "codex", "route": "mechanical", "project_key": key, "model": "gpt-6-luna", "effort": "high"}
}

func gearConfig() map[string]any {
	return map[string]any{"coder_agent": "codex", "routes": map[string]any{
		"mechanical": map[string]any{"model": "gpt-6-luna", "effort": "high"},
	}}
}

func TestWorkerDurationRejectsMissingAndInvalidJobTimes(t *testing.T) {
	if workerDuration(float64(2), float64(1)) != 0 || workerDuration("2", float64(3)) != 0 || workerDuration(float64(1), float64(3)) != 2000 {
		t.Fatal("worker duration must use valid startedAt and finishedAt timestamps")
	}
}

func TestPostGearAdviceSuppressesStateChangeAfterClaim(t *testing.T) {
	root, quotaHome, key := gearAdviceFixture(t)
	t.Setenv("CODEX_HOME", quotaHome)
	originalClaim := claimPostGearAdvice
	defer func() { claimPostGearAdvice = originalClaim }()
	claimPostGearAdvice = func(root, source, window string, advice contextio.GearAdvice) (bool, error) {
		claimed, err := originalClaim(root, source, window, advice)
		if claimed && err == nil {
			err = contextio.SetGear(root, key, "up")
		}
		return claimed, err
	}
	response := map[string]any{}
	postGearAdvice(root, map[string]any{}, gearPending(key), gearConfig(), response, 1500, "")
	if response["hookSpecificOutput"] != nil {
		t.Fatalf("stale advice delivered after gear state changed: %#v", response)
	}
}
