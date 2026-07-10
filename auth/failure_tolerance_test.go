package auth

import (
	"testing"
	"time"
)

func TestReportRequestFailureHonorsScoreThreshold(t *testing.T) {
	store := NewStore(nil, nil, nil)
	account := &Account{
		DBID:                              1,
		IgnoreUsageLimit429Cooldown:       true,
		FailureScoreThresholdEffective:    3,
		FailureCooldownThresholdEffective: 10,
		HealthTier:                        HealthTierHealthy,
	}

	for attempt := 1; attempt <= 2; attempt++ {
		store.ReportRequestFailure(account, "server", time.Second)
		if account.ConsecutiveFailureCount != attempt {
			t.Fatalf("ConsecutiveFailureCount = %d, want %d", account.ConsecutiveFailureCount, attempt)
		}
		if account.FailureStreak != 0 {
			t.Fatalf("FailureStreak = %d before threshold, want 0", account.FailureStreak)
		}
		if !account.LastFailureAt.IsZero() {
			t.Fatalf("LastFailureAt = %v before threshold, want zero", account.LastFailureAt)
		}
	}

	store.ReportRequestFailure(account, "server", time.Second)
	if account.ConsecutiveFailureCount != 3 {
		t.Fatalf("ConsecutiveFailureCount = %d, want 3", account.ConsecutiveFailureCount)
	}
	if account.FailureStreak != 1 {
		t.Fatalf("FailureStreak = %d at threshold, want 1", account.FailureStreak)
	}
	if account.LastFailureAt.IsZero() {
		t.Fatal("LastFailureAt is zero at score threshold")
	}

	store.ReportRequestSuccess(account, time.Second)
	if account.ConsecutiveFailureCount != 0 || account.FailureStreak != 0 {
		t.Fatalf("success did not reset failure counters: consecutive=%d streak=%d", account.ConsecutiveFailureCount, account.FailureStreak)
	}
}

func TestFailureToleranceCooldownThreshold(t *testing.T) {
	account := &Account{
		IgnoreUsageLimit429Cooldown:       true,
		FailureScoreThresholdEffective:    3,
		FailureCooldownThresholdEffective: 10,
		ConsecutiveFailureCount:           9,
	}
	if !account.ShouldDeferFailureCooldown() {
		t.Fatal("ShouldDeferFailureCooldown() = false before threshold")
	}

	account.ConsecutiveFailureCount = 10
	if account.ShouldDeferFailureCooldown() {
		t.Fatal("ShouldDeferFailureCooldown() = true at threshold")
	}

	account.IgnoreUsageLimit429Cooldown = false
	account.ConsecutiveFailureCount = 0
	if account.ShouldDeferFailureCooldown() {
		t.Fatal("disabled failure tolerance must not defer cooldown")
	}
}

func TestFailureToleranceCooldownThresholdIsNotBelowScoreThreshold(t *testing.T) {
	store := NewStore(nil, nil, nil)
	store.SetFailureScoreThreshold(8)
	store.SetFailureCooldownThreshold(4)
	account := &Account{IgnoreUsageLimit429Cooldown: true}

	account.mu.Lock()
	account.recomputeFailureToleranceThresholdsLocked(store)
	account.mu.Unlock()

	_, _, _, scoreEffective, cooldownEffective, _ := account.FailureToleranceSnapshot()
	if scoreEffective != 8 {
		t.Fatalf("score threshold = %d, want 8", scoreEffective)
	}
	if cooldownEffective != 8 {
		t.Fatalf("cooldown threshold = %d, want 8", cooldownEffective)
	}
}
