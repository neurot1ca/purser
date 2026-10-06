package contextio

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gearQuota(now time.Time, used5 float64, reset5 time.Duration, used7 float64, reset7 time.Duration) map[string]any {
	return map[string]any{
		"windows_seen": true, "snapshot_at": now.Format(time.RFC3339Nano), "source_identity": "account-a",
		"5h_used_pct": used5, "5h_resets_at": now.Add(reset5).Format(time.RFC3339Nano),
		"7d_used_pct": used7, "7d_resets_at": now.Add(reset7).Format(time.RFC3339Nano),
	}
}

func gearSamples() []DurationSample {
	samples := make([]DurationSample, 10)
	for i := range samples {
		samples[i] = DurationSample{DurationMS: float64(i+1) * 1000, Implementer: "codex-main"}
	}
	return samples
}

func gearInput(now time.Time, quota map[string]any) GearAdviceInput {
	return GearAdviceInput{Quota: quota, Now: now, Route: "mechanical", Eligible: true,
		DurationMS: 10001, Samples: gearSamples(), Implementer: "codex-main"}
}

func TestGearAdviceUpBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		used5  float64
		reset5 time.Duration
		used7  float64
		reset7 time.Duration
		plan   string
		want   GearAdvice
	}{
		{"5h inclusive 50 and strict under 3h", 50, 3*time.Hour - time.Nanosecond, 35, 7 * 24 * time.Hour, "", GearAdviceUp},
		{"5h above 50", 50.0001, 2 * time.Hour, 35, 7 * 24 * time.Hour, "", GearAdviceNone},
		{"5h exactly 3h", 50, 3 * time.Hour, 35, 7 * 24 * time.Hour, "", GearAdviceNone},
		{"weekly inclusive 35 and strict under 3.5d", -1, 0, 35, 84*time.Hour - time.Nanosecond, "weekly-only", GearAdviceUp},
		{"weekly exactly 3.5d", -1, 0, 35, 84 * time.Hour, "weekly-only", GearAdviceNone},
		{"weekly above 35", -1, 0, 35.0001, 24 * time.Hour, "weekly-only", GearAdviceNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quota := gearQuota(now, tt.used5, tt.reset5, tt.used7, tt.reset7)
			if tt.used5 < 0 {
				delete(quota, "5h_used_pct")
				delete(quota, "5h_resets_at")
				quota["plan_type"] = tt.plan
			}
			got := EvaluateGearAdvice(gearInput(now, quota))
			if got.Advice != tt.want {
				t.Fatalf("advice=%s, want %s (%#v)", got.Advice, tt.want, got)
			}
			if tt.want != GearAdviceNone && got.WindowID == "" {
				t.Fatal("suggestion missing governing window")
			}
		})
	}
}

func TestGearAdviceWeeklySuppressionAndDownBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		gearUp bool
		used5  float64
		reset5 time.Duration
		used7  float64
		reset7 time.Duration
		want   GearAdvice
	}{
		{"weekly 80 suppressed with over one day", false, 50, 2 * time.Hour, 80, 24*time.Hour + time.Nanosecond, GearAdviceNone},
		{"weekly 80 allowed at one day", false, 50, 2 * time.Hour, 80, 24 * time.Hour, GearAdviceUp},
		{"5h down inclusive 85 and strict over one hour", true, 85, time.Hour + time.Nanosecond, 20, 2 * 24 * time.Hour, GearAdviceDown},
		{"5h down exactly one hour", true, 85, time.Hour, 20, 2 * 24 * time.Hour, GearAdviceNone},
		{"weekly down inclusive 90 and strict over one day", true, 20, time.Hour, 90, 24*time.Hour + time.Nanosecond, GearAdviceDown},
		{"weekly down exactly one day", true, 20, time.Hour, 90, 24 * time.Hour, GearAdviceNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := gearInput(now, gearQuota(now, tt.used5, tt.reset5, tt.used7, tt.reset7))
			in.GearUp = tt.gearUp
			got := EvaluateGearAdvice(in)
			if got.Advice != tt.want {
				t.Fatalf("advice=%s, want %s", got.Advice, tt.want)
			}
		})
	}
}

func TestGearAdviceRejectsStaleMissingAndUncertainPlan(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"stale", func(q map[string]any) { q["snapshot_at"] = now.Add(-31 * time.Minute).Format(time.RFC3339Nano) }},
		{"missing snapshot time", func(q map[string]any) { delete(q, "snapshot_at") }},
		{"missing source identity", func(q map[string]any) { delete(q, "source_identity") }},
		{"missing 7d", func(q map[string]any) { delete(q, "7d_used_pct"); delete(q, "7d_resets_at") }},
		{"invalid percentage", func(q map[string]any) { q["5h_used_pct"] = 101.0 }},
		{"expired reset", func(q map[string]any) { q["5h_resets_at"] = now.Format(time.RFC3339Nano) }},
		{"missing 5h is not weekly-only", func(q map[string]any) { delete(q, "5h_used_pct"); delete(q, "5h_resets_at") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := gearQuota(now, 40, 2*time.Hour, 20, 4*24*time.Hour)
			tt.mutate(q)
			if got := EvaluateGearAdvice(gearInput(now, q)); got.Advice != GearAdviceNone {
				t.Fatalf("invalid evidence advice=%s", got.Advice)
			}
		})
	}
}

func TestGearAdviceRequiresPriorP90EvidenceAndExcludesCompletedJob(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	in := gearInput(now, gearQuota(now, 40, 2*time.Hour, 20, 4*24*time.Hour))
	in.DurationMS = 9000
	in.Samples = gearSamples()
	in.Samples = append(in.Samples, DurationSample{DurationMS: 100000, Implementer: "codex-main", JobID: "completed"})
	in.CompletedJob = "completed"
	if got := EvaluateGearAdvice(in); got.Advice != GearAdviceNone {
		t.Fatalf("equal-to-prior-p90 duration triggered advice: %s", got.Advice)
	}
	in.DurationMS = 9001
	if got := EvaluateGearAdvice(in); got.Advice != GearAdviceUp {
		t.Fatalf("excluded current job did not leave p90 trigger: %s", got.Advice)
	}
	in.Samples = in.Samples[:4]
	if got := EvaluateGearAdvice(in); got.Advice != GearAdviceNone {
		t.Fatalf("insufficient samples triggered advice: %s", got.Advice)
	}
}

func TestClaimGearAdviceConcurrentAndWindowRollover(t *testing.T) {
	root := t.TempDir()
	const workers = 24
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := ClaimGearAdvice(root, "account-a", "reset-1", GearAdviceUp)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := claimed.Load(); got != 1 {
		t.Fatalf("concurrent claims=%d, want 1", got)
	}
	if ok, err := ClaimGearAdvice(root, "account-a", "reset-1", GearAdviceDown); err != nil || ok {
		t.Fatalf("opposite advice reused same window claim=(%v,%v), want false,nil", ok, err)
	}
	if ok, err := ClaimGearAdvice(root, "account-a", "reset-2", GearAdviceDown); err != nil || !ok {
		t.Fatalf("rollover claim=(%v,%v), want true,nil", ok, err)
	}
	if ok, err := ClaimGearAdvice(root, "account-b", "reset-1", GearAdviceDown); err != nil || !ok {
		t.Fatalf("different source claim=(%v,%v), want true,nil", ok, err)
	}
}

func TestRouteP90UsesExistingFallbackSampleRule(t *testing.T) {
	samples := []DurationSample{}
	for i := 0; i < 9; i++ {
		samples = append(samples, DurationSample{DurationMS: float64(i + 1)})
	}
	if _, _, ok := RouteP90(samples, "codex-main", ""); ok {
		t.Fatal("nine route-wide samples unexpectedly sufficient")
	}
	samples = append(samples, DurationSample{DurationMS: 100})
	p90, count, ok := RouteP90(samples, "codex-main", "")
	if !ok || p90 != 9 || count != 10 {
		t.Fatalf("route-wide p90=(%v,%d,%v)", p90, count, ok)
	}
}
