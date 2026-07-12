package auth

import (
	"sync"
	"testing"
	"time"
)

func newFailureToleranceTestAccount() *Account {
	return &Account{
		DBID:                              1,
		IgnoreUsageLimit429Cooldown:       true,
		FailureScoreThresholdEffective:    3,
		FailureCooldownThresholdEffective: 10,
		FailureToleranceWindowEffective:   60,
		HealthTier:                        HealthTierHealthy,
	}
}

func TestReportRequestFailureHonorsRollingWindowScoreThreshold(t *testing.T) {
	store := NewStore(nil, nil, nil)
	account := newFailureToleranceTestAccount()
	start := time.Date(2026, 7, 12, 12, 0, 59, 0, time.UTC)

	for attempt := 1; attempt <= 2; attempt++ {
		store.reportRequestFailureAt(account, "server", time.Second, start.Add(time.Duration(attempt-1)*time.Second))
		if got := len(account.failureTimestamps); got != attempt {
			t.Fatalf("failure window count = %d, want %d", got, attempt)
		}
		if account.FailureStreak != 0 {
			t.Fatalf("FailureStreak = %d before threshold, want 0", account.FailureStreak)
		}
	}

	store.reportRequestFailureAt(account, "server", time.Second, start.Add(2*time.Second))
	if account.FailureStreak != 1 || account.LastFailureAt.IsZero() {
		t.Fatalf("score threshold was not applied: streak=%d last_failure=%v", account.FailureStreak, account.LastFailureAt)
	}

	store.ReportRequestSuccess(account, time.Second)
	if got := len(account.failureTimestamps); got != 3 {
		t.Fatalf("success cleared failure window: count=%d, want 3", got)
	}
	if account.FailureStreak != 0 {
		t.Fatalf("success must still reset FailureStreak, got %d", account.FailureStreak)
	}
}

func TestFailureWindowExpiresByAge(t *testing.T) {
	store := NewStore(nil, nil, nil)
	account := newFailureToleranceTestAccount()
	start := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	store.reportRequestFailureAt(account, "server", time.Second, start)
	store.reportRequestFailureAt(account, "server", time.Second, start.Add(30*time.Second))
	store.reportRequestFailureAt(account, "server", time.Second, start.Add(61*time.Second))

	if got := len(account.failureTimestamps); got != 2 {
		t.Fatalf("failure window count after expiry = %d, want 2", got)
	}
}

func TestFailureToleranceCooldownThreshold(t *testing.T) {
	account := newFailureToleranceTestAccount()
	now := time.Now()
	for i := 0; i < 9; i++ {
		account.failureTimestamps = append(account.failureTimestamps, now)
	}
	if !account.ShouldDeferFailureCooldown() {
		t.Fatal("ShouldDeferFailureCooldown() = false before threshold")
	}

	account.mu.Lock()
	account.failureTimestamps = append(account.failureTimestamps, time.Now())
	account.mu.Unlock()
	if account.ShouldDeferFailureCooldown() {
		t.Fatal("ShouldDeferFailureCooldown() = true at threshold")
	}

	account.IgnoreUsageLimit429Cooldown = false
	if account.ShouldDeferFailureCooldown() {
		t.Fatal("disabled failure tolerance must not defer cooldown")
	}
}

func TestFailureToleranceCooldownThresholdIsNotBelowScoreThreshold(t *testing.T) {
	store := NewStore(nil, nil, nil)
	store.SetFailureScoreThreshold(8)
	store.SetFailureCooldownThreshold(4)
	store.SetFailureToleranceWindowSeconds(90)
	account := &Account{IgnoreUsageLimit429Cooldown: true}

	account.mu.Lock()
	account.recomputeFailureToleranceThresholdsLocked(store)
	account.mu.Unlock()

	_, _, _, _, scoreEffective, cooldownEffective, windowEffective, _ := account.FailureToleranceSnapshot()
	if scoreEffective != 8 || cooldownEffective != 8 || windowEffective != 90 {
		t.Fatalf("effective config = score:%d cooldown:%d window:%d", scoreEffective, cooldownEffective, windowEffective)
	}
}

func TestFailureToleranceWindowOverride(t *testing.T) {
	store := NewStore(nil, nil, nil)
	store.SetFailureToleranceWindowSeconds(60)
	account := &Account{
		IgnoreUsageLimit429Cooldown:    true,
		FailureToleranceWindowOverride: 120,
	}

	account.mu.Lock()
	account.recomputeFailureToleranceThresholdsLocked(store)
	account.mu.Unlock()

	_, _, _, windowOverride, _, _, windowEffective, _ := account.FailureToleranceSnapshot()
	if windowOverride != 120 || windowEffective != 120 {
		t.Fatalf("window override/effective = %d/%d, want 120/120", windowOverride, windowEffective)
	}
}

func TestFailureWindowCountsConcurrentFailures(t *testing.T) {
	store := NewStore(nil, nil, nil)
	account := newFailureToleranceTestAccount()
	account.FailureScoreThresholdEffective = 1000

	const failures = 100
	var wg sync.WaitGroup
	wg.Add(failures)
	for range failures {
		go func() {
			defer wg.Done()
			store.ReportRequestFailure(account, "server", time.Millisecond)
		}()
	}
	wg.Wait()

	_, _, _, _, _, _, _, count := account.FailureToleranceSnapshot()
	if count != failures {
		t.Fatalf("concurrent failure count = %d, want %d", count, failures)
	}
}
