package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func newQuotaAutoPauseTestAccount() *Account {
	return &Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
}

func TestQuotaAutoPause5hThresholdFencesAccount(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.95
	acc.UsagePercent5h = 95
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = time.Now().Add(time.Hour)

	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false after 5h auto-pause threshold is reached")
	}
	if got := acc.RuntimeStatus(); got != "quota_paused" {
		t.Fatalf("RuntimeStatus() = %q, want quota_paused after auto-pause threshold is reached", got)
	}
	_, _, _, _, available := acc.fastSchedulerSnapshot(4, time.Now())
	if available {
		t.Fatal("fastSchedulerSnapshot available = true, want false")
	}
}

func TestQuotaAutoPauseIgnoresBelowThresholdAndDisabledWindow(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.95
	acc.UsagePercent5h = 94.9
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = time.Now().Add(time.Hour)

	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true below threshold")
	}

	acc.UsagePercent5h = 99
	acc.AutoPause5hDisabled = true
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true when 5h auto-pause is disabled")
	}
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active when 5h auto-pause is disabled", got)
	}
}

func TestQuotaAutoPauseStopsAfterResetTime(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.95
	acc.UsagePercent5h = 99
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = time.Now().Add(-time.Minute)

	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true after reset time has passed")
	}
}

func TestQuotaAutoPause7dThresholdFencesAccount(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause7dThreshold = 0.9
	acc.UsagePercent7d = 91
	acc.UsagePercent7dValid = true

	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false after 7d auto-pause threshold is reached")
	}
}

func TestAccountCooldownIgnoreFlagsRestoreFromCredentials(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "ignore-cooldown-flags", map[string]interface{}{
		"access_token":                    "at",
		"ignore_usage_limit_429_cooldown": true,
		"ignore_unauthorized_cooldown":    true,
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials 返回错误: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init 返回错误: %v", err)
	}

	account := store.FindByID(id)
	if account == nil {
		t.Fatal("FindByID 返回 nil")
	}
	if !account.ShouldIgnoreUsageLimit429Cooldown() {
		t.Fatal("ShouldIgnoreUsageLimit429Cooldown() = false, want true")
	}
	if !account.ShouldIgnoreUnauthorizedCooldown() {
		t.Fatal("ShouldIgnoreUnauthorizedCooldown() = false, want true")
	}
}
