package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetryFixtures(t *testing.T) {
	p := filepath.Join("testdata", "retry.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	for _, c := range rows {
		t.Run(c["name"].(string), func(t *testing.T) {
			got, _ := json.Marshal(replayRetry(c))
			want, _ := json.Marshal(c["expected"])
			if string(got) != string(want) {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}

func replayRetry(c map[string]any) map[string]any {
	var r map[string]any
	switch c["kind"] {
	case "responsive":
		rows := c["attempts"].([]any)
		i := 0
		now := time.Duration(0)
		r = RunWithResponsivenessRetries(func() map[string]any {
			item := rows[i].(map[string]any)
			i++
			if s, ok := item["launchFailure"].(map[string]any); ok {
				return map[string]any{"typedEvidence": retryEvidenceFixture(s)}
			}
			return item
		}, func() time.Duration { return now }, func(d time.Duration) { now += d })
	case "evidence":
		events := eventFixtures(c["events"].([]any))
		r = map[string]any{"isError": true, "typedEvidence": LaunchFailureEvidence("opencode", events, "", true, false, false, nil)}
	case "fallback":
		rows := c["attempts"].([]any)
		i := 0
		run := func(_, _ string) map[string]any {
			item := rows[i].(map[string]any)
			i++
			if item["isError"] == true {
				return item
			}
			return map[string]any{"isError": false, "structuredContent": map[string]any{"threadId": "t"}}
		}
		fallback, _ := c["fallback"].(string)
		if fallback == "" {
			fallback = "codex"
		}
		var candidates []string
		if raw, ok := c["candidates"].([]any); ok {
			for _, v := range raw {
				candidates = append(candidates, v.(string))
			}
		}
		mark, ok := c["mark"].(bool)
		if !ok {
			mark = true
		}
		r = RunMechanicalFallback(RetryInput{Agent: "opencode", Model: "vendor/first", FallbackAgent: fallback, FallbackModel: "gpt-6-sol", FallbackEffort: "medium", Mechanical: true, Async: true}, run, func() []string { return candidates }, func(string) bool { return mark }, func() bool { return c["changed"] != true })
	}
	got := map[string]any{"attemptedModels": []any{}, "servedAgent": nil, "servedModel": nil, "responsivenessAttempts": r["responsivenessAttempts"], "isError": r["isError"], "errorCode": r["errorCode"], "retryAfterSeconds": nil}
	if r["isError"] == nil {
		got["isError"] = false
	}
	if r["attemptedModels"] != nil {
		got["attemptedModels"] = stringsOf(r["attemptedModels"])
	}
	if v, ok := r["structuredContent"].(map[string]any); ok {
		got["attemptedModels"] = stringsOf(v["attemptedModels"])
		got["servedAgent"] = v["servedAgent"]
		got["servedModel"] = v["servedModel"]
	}
	if e, ok := r["typedEvidence"].(map[string]any); ok {
		if n, ok := e["retryAfterSeconds"].(*float64); ok && n != nil {
			got["retryAfterSeconds"] = *n
		} else {
			got["retryAfterSeconds"] = e["retryAfterSeconds"]
		}
	}
	return got
}

func eventFixtures(raw []any) []map[string]any {
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}
func retryEvidenceFixture(s map[string]any) map[string]any {
	return LaunchFailureEvidence(s["agent"].(string), eventFixtures(s["events"].([]any)), s["stderr"].(string), s["started"].(bool), s["stdout"].(bool), s["output"].(bool), nil)
}

func stringsOf(v any) []any {
	out := []any{}
	switch values := v.(type) {
	case []string:
		for _, s := range values {
			out = append(out, s)
		}
	case []any:
		return values
	}
	return out
}
