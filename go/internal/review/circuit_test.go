package review

import (
	"testing"
	"time"
)

func TestReviewerCircuitOpensAfterThreeTimeouts(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		RecordReviewerCircuitOutcome(dir, "agy", "gemini-3.8-flash-high", "REVIEWER_TIMEOUT", now)
		if ReviewerCircuitOpen(dir, "agy", "gemini-3.8-flash-high", now) {
			t.Fatalf("circuit opened after %d timeouts", i+1)
		}
	}
	RecordReviewerCircuitOutcome(dir, "agy", "gemini-3.8-flash-high", "REVIEWER_TIMEOUT", now)
	if !ReviewerCircuitOpen(dir, "agy", "gemini-3.8-flash-high", now) {
		t.Fatal("circuit did not open after three timeouts")
	}
}

func TestReviewerCircuitProbesAfterTwoHours(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		RecordReviewerCircuitOutcome(dir, "agy", "gemini-3.8-flash-high", "REVIEWER_TIMEOUT", now)
	}
	if !ReviewerCircuitOpen(dir, "agy", "gemini-3.8-flash-high", now.Add(2*time.Hour-time.Nanosecond)) {
		t.Fatal("circuit closed before two hours")
	}
	if ReviewerCircuitOpen(dir, "agy", "gemini-3.8-flash-high", now.Add(2*time.Hour)) {
		t.Fatal("circuit remained open at the two hour probe time")
	}
}

func TestReviewerCircuitSuccessResetsTimeoutStreak(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		RecordReviewerCircuitOutcome(dir, "agy", "gemini-3.8-flash-high", "REVIEWER_TIMEOUT", now)
	}
	RecordReviewerCircuitOutcome(dir, "agy", "gemini-3.8-flash-high", "COMPLETED", now)
	for i := 0; i < 2; i++ {
		RecordReviewerCircuitOutcome(dir, "agy", "gemini-3.8-flash-high", "REVIEWER_TIMEOUT", now)
	}
	if ReviewerCircuitOpen(dir, "agy", "gemini-3.8-flash-high", now) {
		t.Fatal("circuit opened from timeouts split by success")
	}
}

func TestReviewerCircuitSeparatesProjectReviewerAndModel(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 3; i++ {
		RecordReviewerCircuitOutcome(dir, "agy", "gemini-3.8-flash-high", "REVIEWER_TIMEOUT", now)
	}
	if ReviewerCircuitOpen(dir, "agy", "gemini-3.8-flash-high", now) == ReviewerCircuitOpen(t.TempDir(), "agy", "gemini-3.8-flash-high", now) {
		t.Fatal("circuit state was not project-scoped")
	}
	if ReviewerCircuitOpen(dir, "agy", "gemini-2.5-pro", now) {
		t.Fatal("circuit state was not model-scoped")
	}
}
