package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func int64Ptr(v int64) *int64 {
	return &v
}

func recomputeTestAccount(acc *Account, baseLimit int64) {
	acc.mu.Lock()
	acc.recomputeSchedulerLocked(baseLimit)
	acc.mu.Unlock()
}

func TestApplyAccountManualScoreBonusOnlyChangesEligibleDispatchScore(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:  4,
		TestConcurrency: 1,
		TestModel:       "gpt-test",
	})
	account := &Account{
		DBID:        9101,
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "free",
	}
	store.AddAccount(account)
	baseline := account.GetSchedulerDebugSnapshot(4)

	if !store.ApplyAccountManualScoreBonus(account.DBID, 40, time.Now().Add(time.Hour)) {
		t.Fatal("ApplyAccountManualScoreBonus() = false, want true")
	}
	boosted := account.GetSchedulerDebugSnapshot(4)
	if boosted.SchedulerScore != baseline.SchedulerScore {
		t.Fatalf("SchedulerScore = %v, want unchanged %v", boosted.SchedulerScore, baseline.SchedulerScore)
	}
	if boosted.DispatchScore != baseline.DispatchScore+40 {
		t.Fatalf("DispatchScore = %v, want %v", boosted.DispatchScore, baseline.DispatchScore+40)
	}
	if boosted.ManualScoreBonus != 40 || boosted.Breakdown.ManualScoreBonus != 40 {
		t.Fatalf("临时加分快照 = %#v, want 40", boosted)
	}
	if !store.ApplyAccountManualScoreBonus(account.DBID, -400, time.Now().Add(time.Hour)) {
		t.Fatal("ApplyAccountManualScoreBonus(-400) = false, want true")
	}
	penalized := account.GetSchedulerDebugSnapshot(4)
	if penalized.SchedulerScore != baseline.SchedulerScore || penalized.HealthTier != baseline.HealthTier {
		t.Fatalf("负向临时分改变了健康调度状态: baseline=%#v penalized=%#v", baseline, penalized)
	}
	if penalized.DispatchScore != baseline.DispatchScore-400 || penalized.Breakdown.ManualScoreBonus != -400 {
		t.Fatalf("负向 DispatchScore = %#v, want baseline-400", penalized)
	}

	if !store.ApplyAccountManualScoreBonus(account.DBID, 60, time.Now().Add(-time.Second)) {
		t.Fatal("设置已过期加分应视为成功清除")
	}
	cleared := account.GetSchedulerDebugSnapshot(4)
	if cleared.ManualScoreBonus != 0 || !cleared.ManualScoreBonusUntil.IsZero() {
		t.Fatalf("过期加分未清除: %#v", cleared)
	}
	if cleared.DispatchScore != baseline.DispatchScore {
		t.Fatalf("清除后 DispatchScore = %v, want %v", cleared.DispatchScore, baseline.DispatchScore)
	}
	if store.ApplyAccountManualScoreBonus(999999, 1, time.Now().Add(time.Hour)) {
		t.Fatal("未知账号不应应用临时加分")
	}
}

func TestMaintenanceSlotsShareAccountLoadWithoutDispatchSideEffects(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-test"})
	account := &Account{DBID: 9102, AccessToken: "token", Status: StatusReady, PlanType: "free"}
	store.AddAccount(account)
	baseline := account.GetSchedulerDebugSnapshot(2)
	if !store.TryAcquireMaintenanceSlot(account) || !store.TryAcquireMaintenanceSlot(account) {
		t.Fatal("前两个维护槽应成功")
	}
	if store.TryAcquireMaintenanceSlot(account) {
		t.Fatal("维护槽不应超过账号并发上限")
	}
	if account.GetActiveRequests() != 2 || account.GetTotalRequests() != 0 {
		t.Fatalf("负载/请求统计 = %d/%d, want 2/0", account.GetActiveRequests(), account.GetTotalRequests())
	}
	store.ReleaseMaintenanceSlot(account)
	if !store.TryAcquireMaintenanceSlot(account) {
		t.Fatal("释放后应能重新占用维护槽")
	}
	store.ReleaseMaintenanceSlot(account)
	store.ReleaseMaintenanceSlot(account)
	after := account.GetSchedulerDebugSnapshot(2)
	if account.GetActiveRequests() != 0 || after.SchedulerScore != baseline.SchedulerScore || after.HealthTier != baseline.HealthTier {
		t.Fatalf("维护请求改变了账号状态: active=%d baseline=%#v after=%#v", account.GetActiveRequests(), baseline, after)
	}
}

func TestManualScoreBonusDoesNotMakeErrorAccountDispatchEligible(t *testing.T) {
	account := &Account{
		AccessToken:           "token",
		Status:                StatusError,
		PlanType:              "free",
		ManualScoreBonus:      200,
		ManualScoreBonusUntil: time.Now().Add(time.Hour),
	}
	recomputeTestAccount(account, 4)
	snapshot := account.GetSchedulerDebugSnapshot(4)
	if snapshot.Breakdown.ManualScoreBonus != 0 {
		t.Fatalf("错误账号 Breakdown.ManualScoreBonus = %v, want 0", snapshot.Breakdown.ManualScoreBonus)
	}
	if snapshot.DispatchScore != snapshot.SchedulerScore {
		t.Fatalf("错误账号 DispatchScore = %v, want SchedulerScore %v", snapshot.DispatchScore, snapshot.SchedulerScore)
	}
}

func TestAccountPremiumPlanGetsDefaultScoreBias(t *testing.T) {
	acc := &Account{
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "plus",
	}

	recomputeTestAccount(acc, 6)

	if acc.SchedulerScore != 100 {
		t.Fatalf("SchedulerScore = %v, want 100", acc.SchedulerScore)
	}
	if acc.DispatchScore != 150 {
		t.Fatalf("DispatchScore = %v, want 150", acc.DispatchScore)
	}
	if acc.ScoreBiasEffective != 50 {
		t.Fatalf("ScoreBiasEffective = %d, want 50", acc.ScoreBiasEffective)
	}
	if acc.BaseConcurrencyEffective != 6 {
		t.Fatalf("BaseConcurrencyEffective = %d, want 6", acc.BaseConcurrencyEffective)
	}
}

func TestAccountScoreBiasOverrideReplacesPlanDefault(t *testing.T) {
	acc := &Account{
		AccessToken:       "token",
		Status:            StatusReady,
		PlanType:          "team",
		ScoreBiasOverride: int64Ptr(12),
	}

	recomputeTestAccount(acc, 6)

	if acc.DispatchScore != 112 {
		t.Fatalf("DispatchScore = %v, want 112", acc.DispatchScore)
	}
	if acc.ScoreBiasEffective != 12 {
		t.Fatalf("ScoreBiasEffective = %d, want 12", acc.ScoreBiasEffective)
	}
}

func TestAccountRiskyTierDoesNotApplyScoreBias(t *testing.T) {
	acc := &Account{
		AccessToken:        "token",
		Status:             StatusReady,
		PlanType:           "pro",
		LastUnauthorizedAt: time.Now(),
	}

	recomputeTestAccount(acc, 6)

	if acc.HealthTier != HealthTierRisky {
		t.Fatalf("HealthTier = %s, want %s", acc.HealthTier, HealthTierRisky)
	}
	if acc.SchedulerScore >= 60 {
		t.Fatalf("SchedulerScore = %v, want < 60", acc.SchedulerScore)
	}
	if acc.DispatchScore != acc.SchedulerScore {
		t.Fatalf("DispatchScore = %v, want raw score %v when risky", acc.DispatchScore, acc.SchedulerScore)
	}
	if acc.ScoreBiasEffective != 0 {
		t.Fatalf("ScoreBiasEffective = %d, want 0", acc.ScoreBiasEffective)
	}
}

func TestAccountBaseConcurrencyOverrideControlsDynamicLimit(t *testing.T) {
	acc := &Account{
		AccessToken:             "token",
		Status:                  StatusReady,
		PlanType:                "plus",
		BaseConcurrencyOverride: int64Ptr(4),
	}

	recomputeTestAccount(acc, 10)
	if acc.DynamicConcurrencyLimit != 4 {
		t.Fatalf("healthy DynamicConcurrencyLimit = %d, want 4", acc.DynamicConcurrencyLimit)
	}

	acc.mu.Lock()
	acc.LastFailureAt = time.Now()
	acc.mu.Unlock()
	recomputeTestAccount(acc, 10)
	if acc.HealthTier != HealthTierWarm {
		t.Fatalf("warm HealthTier = %s, want %s", acc.HealthTier, HealthTierWarm)
	}
	if acc.DynamicConcurrencyLimit != 2 {
		t.Fatalf("warm DynamicConcurrencyLimit = %d, want 2", acc.DynamicConcurrencyLimit)
	}

	acc.mu.Lock()
	acc.LastUnauthorizedAt = time.Now()
	acc.mu.Unlock()
	recomputeTestAccount(acc, 10)
	if acc.HealthTier != HealthTierRisky {
		t.Fatalf("risky HealthTier = %s, want %s", acc.HealthTier, HealthTierRisky)
	}
	if acc.DynamicConcurrencyLimit != 1 {
		t.Fatalf("risky DynamicConcurrencyLimit = %d, want 1", acc.DynamicConcurrencyLimit)
	}
}

func TestAccountSkipWarmTierPromotesWarmScoreToHealthy(t *testing.T) {
	acc := &Account{
		AccessToken:   "token",
		Status:        StatusReady,
		PlanType:      "pro",
		SkipWarmTier:  true,
		LastTimeoutAt: time.Now(),
	}

	recomputeTestAccount(acc, 6)

	if acc.SchedulerScore >= 85 || acc.SchedulerScore < 60 {
		t.Fatalf("SchedulerScore = %v, want warm score range", acc.SchedulerScore)
	}
	if acc.HealthTier != HealthTierHealthy {
		t.Fatalf("HealthTier = %s, want %s", acc.HealthTier, HealthTierHealthy)
	}
	if acc.DynamicConcurrencyLimit != 6 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want full healthy limit 6", acc.DynamicConcurrencyLimit)
	}
}

func TestAccountSkipWarmTierPromotesRecentFailureWarmToHealthy(t *testing.T) {
	acc := &Account{
		AccessToken:   "token",
		Status:        StatusReady,
		PlanType:      "pro",
		SkipWarmTier:  true,
		LastFailureAt: time.Now(),
	}

	recomputeTestAccount(acc, 4)

	if acc.HealthTier != HealthTierHealthy {
		t.Fatalf("HealthTier = %s, want %s", acc.HealthTier, HealthTierHealthy)
	}
	if acc.DynamicConcurrencyLimit != 4 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want full healthy limit 4", acc.DynamicConcurrencyLimit)
	}
}

func TestAccountSkipWarmTierDoesNotPromoteRiskyOrBanned(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		acc  *Account
		want AccountHealthTier
	}{
		{
			name: "low score remains risky",
			acc: &Account{
				AccessToken:        "token",
				Status:             StatusReady,
				PlanType:           "pro",
				SkipWarmTier:       true,
				LastUnauthorizedAt: now,
			},
			want: HealthTierRisky,
		},
		{
			name: "banned remains banned",
			acc: &Account{
				AccessToken:  "token",
				Status:       StatusReady,
				PlanType:     "pro",
				HealthTier:   HealthTierBanned,
				SkipWarmTier: true,
			},
			want: HealthTierBanned,
		},
		{
			name: "premium 5h limit remains risky",
			acc: &Account{
				AccessToken:         "token",
				Status:              StatusReady,
				PlanType:            "plus",
				SkipWarmTier:        true,
				UsagePercent5h:      100,
				UsagePercent5hValid: true,
				Reset5hAt:           now.Add(time.Hour),
			},
			want: HealthTierRisky,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recomputeTestAccount(tc.acc, 6)
			if tc.acc.HealthTier != tc.want {
				t.Fatalf("HealthTier = %s, want %s", tc.acc.HealthTier, tc.want)
			}
		})
	}
}

func TestIgnoreUsageLimitStatusOverridePrecedence(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	forceOn := true
	forceOff := false
	inherit := &Account{DBID: 101, AccessToken: "inherit", Status: StatusReady}
	off := &Account{DBID: 102, AccessToken: "off", Status: StatusReady, IgnoreUsageLimitStatusOverride: &forceOff}
	on := &Account{DBID: 103, AccessToken: "on", Status: StatusReady, IgnoreUsageLimitStatusOverride: &forceOn}
	store.AddAccount(inherit)
	store.AddAccount(off)
	store.AddAccount(on)

	if !inherit.IgnoresUsageLimitStatus() || off.IgnoresUsageLimitStatus() || !on.IgnoresUsageLimitStatus() {
		t.Fatalf("global=true effective values = inherit:%v off:%v on:%v", inherit.IgnoresUsageLimitStatus(), off.IgnoresUsageLimitStatus(), on.IgnoresUsageLimitStatus())
	}

	store.SetIgnoreUsageLimitStatus(false)
	if inherit.IgnoresUsageLimitStatus() || off.IgnoresUsageLimitStatus() || !on.IgnoresUsageLimitStatus() {
		t.Fatalf("global=false effective values = inherit:%v off:%v on:%v", inherit.IgnoresUsageLimitStatus(), off.IgnoresUsageLimitStatus(), on.IgnoresUsageLimitStatus())
	}
}

func TestIgnoreUsageLimitStatusKeepsExhaustedAccountSchedulable(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	account := &Account{
		DBID:                104,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
		UsagePercent7d:      100,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(24 * time.Hour),
	}
	store.AddAccount(account)

	if !account.IsAvailable() {
		t.Fatal("account should remain available when usage windows are informational")
	}
	if got := store.Next(); got != account {
		t.Fatalf("Next() = %p, want exhausted-but-usable account %p", got, account)
	} else {
		store.Release(got)
	}
	store.BindSessionAffinity("continued-session", account, "")
	if got, _ := store.NextForSession("continued-session", 0, nil); got != account {
		t.Fatalf("NextForSession() = %p, want bound exhausted-but-usable account %p", got, account)
	} else {
		store.Release(got)
	}
}

func TestCleanFullUsageSkipsAccountsIgnoringUsageLimitStatus(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	account := &Account{
		DBID:                106,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      100,
		UsagePercent7dValid: true,
	}
	store.AddAccount(account)

	if cleaned := store.CleanFullUsageAccounts(context.Background()); cleaned != 0 {
		t.Fatalf("CleanFullUsageAccounts() = %d, want 0: informational snapshots must not delete accounts", cleaned)
	}
	if store.FindByID(account.DBID) == nil {
		t.Fatal("account ignoring usage-limit status should survive full-usage cleanup")
	}

	store.SetIgnoreUsageLimitStatus(false)
	if cleaned := store.CleanFullUsageAccounts(context.Background()); cleaned != 1 {
		t.Fatalf("CleanFullUsageAccounts() = %d, want 1 once snapshots are authoritative again", cleaned)
	}
	if store.FindByID(account.DBID) != nil {
		t.Fatal("account should be cleaned when usage windows are authoritative")
	}
}

func TestResponsesSuccessClearsOnlyUsageCooldownWhenIgnored(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	account := &Account{DBID: 105, AccessToken: "token", Status: StatusReady, PlanType: "plus"}
	store.AddAccount(account)
	store.MarkPremium5hRateLimited(account, time.Now().Add(time.Hour))

	if account.IsAvailable() {
		t.Fatal("a real usage cooldown must remain unavailable before Responses succeeds")
	}
	if !store.ConfirmResponsesAvailable(account) {
		t.Fatal("ConfirmResponsesAvailable() = false, want usage cooldown cleared")
	}
	if !account.IsAvailable() {
		t.Fatal("successful Responses evidence should restore scheduling despite the 100% snapshot")
	}

	account.mu.Lock()
	account.Status = StatusCooldown
	account.CooldownReason = "unauthorized"
	account.CooldownUtil = time.Now().Add(time.Hour)
	account.mu.Unlock()
	if store.ConfirmResponsesAvailable(account) {
		t.Fatal("Responses success must not clear an unauthorized cooldown")
	}
}

func TestConfirmResponsesAvailableSinceRespectsLatestRateLimit(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	account := &Account{DBID: 107, AccessToken: "token", Status: StatusReady, PlanType: "plus"}
	store.AddAccount(account)

	staleRequestStartedAt := time.Now()
	store.MarkCooldown(account, time.Hour, "rate_limited")
	if store.ConfirmResponsesAvailableSince(account, staleRequestStartedAt) {
		t.Fatal("a request started before the latest rate limit must not clear its cooldown")
	}
	if !account.HasActiveCooldown() || account.IsAvailable() {
		t.Fatal("newer rate-limit evidence should keep the account unavailable")
	}

	account.mu.RLock()
	freshRequestStartedAt := account.LastRateLimitedAt.Add(time.Nanosecond)
	account.mu.RUnlock()
	if !store.ConfirmResponsesAvailableSince(account, freshRequestStartedAt) {
		t.Fatal("a request started after the latest rate limit should clear its cooldown")
	}
	if account.HasActiveCooldown() || !account.IsAvailable() {
		t.Fatal("fresh successful Responses evidence should restore account availability")
	}
}

func TestNeedsUsageProbeRateLimitedAllowsResetCreditsRefresh(t *testing.T) {
	// 429 冷却 + 重置次数从未探测过（stale）：应允许探针（wham-only）刷新「主动重置次数」。
	acc := &Account{
		AccessToken:    "token",
		Status:         StatusCooldown,
		CooldownReason: "rate_limited",
	}
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return true when reset credits are stale, even during rate_limited cooldown")
	}

	// 该状态应被标记为 limited，以保证探针只走 wham、不回退 /responses。
	if !acc.InLimitedState() {
		t.Fatal("InLimitedState should be true for rate_limited cooldown")
	}

	// 重置次数刚探测过（fresh）：429 冷却期间不应再发探针（避免 /responses 加重限流）。
	acc.MarkResetCreditsProbed(time.Now())
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return false during rate_limited cooldown once reset credits are fresh")
	}
}

func TestNeedsUsageProbeSkipsUnauthorized(t *testing.T) {
	acc := &Account{
		AccessToken:    "token",
		Status:         StatusCooldown,
		CooldownReason: "unauthorized",
	}
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return false for unauthorized cooldown")
	}
}

func TestNeedsUsageProbeAllowsReadyAccount(t *testing.T) {
	acc := &Account{
		AccessToken: "token",
		Status:      StatusReady,
	}
	// UsagePercent7dValid = false，应该返回 true
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return true for ready account without valid usage data")
	}
}

func TestNeedsUsageProbeRefreshesStaleResetCreditsDespiteFreshUsage(t *testing.T) {
	now := time.Now()
	// 核心修复：账号用量快照很新鲜（活跃账号被业务流量持续刷新），
	// 但「主动重置次数」从未/很久没探测过，仍应触发 wham 探针刷新它。
	acc := &Account{
		AccessToken:         "token",
		Status:              StatusReady,
		UsagePercent7d:      30,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      now, // 用量刚刷新
	}
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return true when reset credits are stale even if usage snapshot is fresh")
	}

	// 重置次数也刚探测过：用量与重置次数都新鲜，则无需探针。
	acc.MarkResetCreditsProbed(now)
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return false when both usage and reset credits are fresh")
	}
}

func TestNeedsUsageProbeDoesNotRequireMissing5hWhenAutoPauseEnabled(t *testing.T) {
	// issue #382：上游可永久不返回 5h。auto-pause 5h 配置在无快照时不应强制探测。
	acc := &Account{
		AccessToken:          "token",
		Status:               StatusReady,
		UsagePercent7d:       12,
		UsagePercent7dValid:  true,
		UsageUpdatedAt:       time.Now(),
		AutoPause5hThreshold: 0.95,
	}
	acc.recomputeEffectiveAutoPause(nil)
	acc.MarkResetCreditsProbed(time.Now()) // 隔离 reset-credits 过期影响

	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = true, want false when 5h auto-pause is enabled but 5h window is absent")
	}
}

func TestNeedsUsageProbeRefreshesStale5hWhenAutoPauseEnabled(t *testing.T) {
	now := time.Now()
	acc := &Account{
		AccessToken:          "token",
		Status:               StatusReady,
		UsagePercent7d:       12,
		UsagePercent7dValid:  true,
		UsageUpdatedAt:       now,
		AutoPause5hThreshold: 0.95,
		UsagePercent5h:       40,
		UsagePercent5hValid:  true,
		Reset5hAt:            now.Add(2 * time.Hour),
		UsageUpdatedAt5h:     now.Add(-20 * time.Minute),
	}
	acc.recomputeEffectiveAutoPause(nil)
	acc.MarkResetCreditsProbed(now)

	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true when valid 5h snapshot is stale under auto-pause")
	}
}

// TestNeedsUsageProbeRefreshesStale5hAfterWindowReset 验证 Bug B 修复：
// 5h 窗口的重置时间已过、但快照仍是重置前的高用量时，应触发一次 wham 刷新，
// 让用量进度条跟随官方窗口重置而恢复，而不是一直停在旧值（如 100%）。
func TestNeedsUsageProbeRefreshesStale5hAfterWindowReset(t *testing.T) {
	now := time.Now()
	acc := &Account{
		AccessToken:         "token",
		Status:              StatusReady,
		UsagePercent7d:      12,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      now, // 7d 快照新鲜，隔离 7d 路径
		// 5h：窗口重置时间已过，但快照仍是重置前采集的 100%
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           now.Add(-1 * time.Minute),
		UsageUpdatedAt5h:    now.Add(-3 * time.Hour),
	}
	acc.MarkResetCreditsProbed(now) // 隔离 reset-credits 过期影响，专测窗口重置路径

	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true: 5h window reset passed but snapshot is stale")
	}

	// 刷新后（快照刚更新、重置时间在未来）：不应再反复触发探针。
	acc.SetUsageSnapshot5hAt(8, now.Add(5*time.Hour), now)
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = true, want false after 5h snapshot refreshed")
	}
}

func TestPersistUsageSnapshotDoesNotRequireMissing5hAfter7dOnly(t *testing.T) {
	// issue #382：7d-only 持久化后，缺失 5h 不再因 auto-pause 配置强制探测。
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	acc := &Account{
		DBID:                 1,
		AccessToken:          "token",
		Status:               StatusReady,
		AutoPause5hThreshold: 0.95,
		AutoPause7dThreshold: 0.95,
		UsagePercent5hValid:  false,
		UsagePercent7dValid:  false,
	}
	acc.recomputeEffectiveAutoPause(store)

	store.PersistUsageSnapshot(acc, 20)
	acc.MarkResetCreditsProbed(time.Now())

	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = true, want false after 7d-only persistence when 5h window is absent")
	}
}

func TestPersistUsageSnapshot5hOnlyDoesNotRefreshStale7dSnapshot(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		UsagePercent7d:      40,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      time.Now().Add(-20 * time.Minute),
		UsagePercent5h:      25,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	}

	store.PersistUsageSnapshot5hOnly(acc)
	acc.MarkResetCreditsProbed(time.Now()) // 隔离 reset-credits 过期影响，专测 7d 新鲜度不被 5h-only 持久化刷新

	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true because 5h-only persistence must not refresh stale 7d freshness")
	}
}

func TestTriggerUsageProbeAsyncRunsInLazyMode(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.SetLazyMode(true)
	store.AddAccount(&Account{DBID: 1, AccessToken: "token", Status: StatusReady})

	called := make(chan struct{}, 1)
	store.SetUsageProbeFunc(func(ctx context.Context, acc *Account) error {
		called <- struct{}{}
		return nil
	})

	store.TriggerUsageProbeAsync()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("usage probe was not triggered in lazy mode")
	}
}

func TestTriggerUsageProbeForceAsyncRunsInLazyMode(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.SetLazyMode(true)
	store.AddAccount(&Account{DBID: 1, AccessToken: "token", Status: StatusReady})

	called := make(chan struct{}, 1)
	store.SetUsageProbeFunc(func(ctx context.Context, acc *Account) error {
		called <- struct{}{}
		return nil
	})

	store.TriggerUsageProbeForceAsync()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("forced usage probe was not triggered in lazy mode")
	}
}

func TestRefreshSingleBypassesCachedAccessToken(t *testing.T) {
	ctx := context.Background()
	tokenCache := cache.NewMemory(1)
	if err := tokenCache.SetAccessToken(ctx, 7, "cached-token", time.Hour); err != nil {
		t.Fatalf("SetAccessToken 返回错误: %v", err)
	}

	store := NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&Account{
		DBID:        7,
		AccessToken: "old-token",
		ExpiresAt:   time.Now().Add(time.Hour),
		Status:      StatusReady,
	})

	err := store.RefreshSingle(ctx, 7)
	if err == nil {
		t.Fatal("RefreshSingle should force upstream refresh instead of using cached token")
	}
	if !strings.Contains(err.Error(), "refresh_token 为空") {
		t.Fatalf("RefreshSingle error = %v, want missing refresh_token", err)
	}
}

func TestApplyRefreshedPlanTypeKeepsFreeUsageLimitAuthoritative(t *testing.T) {
	now := time.Now()
	acc := &Account{
		PlanType:            "free",
		UsagePercent7d:      100,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(time.Hour),
	}

	acc.mu.Lock()
	plan, applied := acc.applyRefreshedPlanTypeLocked("pro", now)
	acc.mu.Unlock()

	if plan != "pro" {
		t.Fatalf("plan = %q, want pro", plan)
	}
	if applied {
		t.Fatal("refreshed pro plan should not override active free usage-limit metadata")
	}
	if got := acc.GetPlanType(); got != "free" {
		t.Fatalf("PlanType = %q, want free", got)
	}
}

func TestApplyRefreshedPlanTypeKeepsActiveFreeUsageWindowAuthoritative(t *testing.T) {
	now := time.Now()
	acc := &Account{
		PlanType:            "free",
		UsagePercent7d:      3,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(24 * time.Hour),
	}

	acc.mu.Lock()
	plan, applied := acc.applyRefreshedPlanTypeLocked("pro", now)
	acc.mu.Unlock()

	if plan != "pro" {
		t.Fatalf("plan = %q, want pro", plan)
	}
	if applied {
		t.Fatal("refreshed pro plan should not override an active free 7d usage window")
	}
	if got := acc.GetPlanType(); got != "free" {
		t.Fatalf("PlanType = %q, want free", got)
	}
}

func TestApplyRefreshedPlanTypeAllowsPlanUpgradeAfterUsageReset(t *testing.T) {
	now := time.Now()
	acc := &Account{
		PlanType:            "free",
		UsagePercent7d:      100,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(-time.Minute),
	}

	acc.mu.Lock()
	plan, applied := acc.applyRefreshedPlanTypeLocked("pro", now)
	acc.mu.Unlock()

	if plan != "pro" || !applied {
		t.Fatalf("plan=%q applied=%v, want pro true", plan, applied)
	}
	if got := acc.GetPlanType(); got != "pro" {
		t.Fatalf("PlanType = %q, want pro", got)
	}
}

func TestStoreNextPrefersHigherDispatchScoreWithinTier(t *testing.T) {
	premium := &Account{
		DBID:        1,
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "pro",
	}
	regular := &Account{
		DBID:        2,
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "free",
	}
	recomputeTestAccount(premium, 2)
	recomputeTestAccount(regular, 2)

	store := &Store{
		accounts: []*Account{regular, premium},
	}
	store.SetMaxConcurrency(2)

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != premium.DBID {
		t.Fatalf("Next() picked dbID=%d, want premium account %d", got.DBID, premium.DBID)
	}
}

func TestStoreNextConcurrentAcquireDoesNotExceedDynamicLimit(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "pro",
	}
	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 1,
	}

	const workers = 32
	var entered int64
	start := make(chan struct{})
	filterGate := make(chan struct{})
	results := make(chan *Account, workers)

	filter := func(candidate *Account) bool {
		if candidate != nil && candidate.DBID == acc.DBID {
			atomic.AddInt64(&entered, 1)
		}
		<-filterGate
		return true
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			results <- store.NextExcludingWithFilter(0, nil, filter)
		}()
	}
	close(start)

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt64(&entered) < workers {
		select {
		case <-deadline:
			close(filterGate)
			t.Fatalf("only %d/%d workers reached the scheduler filter", atomic.LoadInt64(&entered), workers)
		default:
			time.Sleep(time.Millisecond)
		}
	}

	acc.mu.Lock()
	close(filterGate)
	time.Sleep(20 * time.Millisecond)
	acc.mu.Unlock()

	wg.Wait()
	close(results)

	acquired := 0
	for got := range results {
		if got != nil {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("acquired accounts = %d, want 1", acquired)
	}
	if got := atomic.LoadInt64(&acc.ActiveRequests); got != 1 {
		t.Fatalf("ActiveRequests = %d, want 1", got)
	}
	store.Release(acc)
}

func TestAccountPremium5hUrgencyBonusOnlyAffectsDispatchScore(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      20,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(30 * time.Minute),
		UsagePercent7d:      45,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(4 * 24 * time.Hour),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.SchedulerScore != 100 {
		t.Fatalf("SchedulerScore = %v, want 100", snapshot.SchedulerScore)
	}
	if snapshot.Breakdown.UsageUrgencyBonus5h <= 20 {
		t.Fatalf("UsageUrgencyBonus5h = %v, want > 20", snapshot.Breakdown.UsageUrgencyBonus5h)
	}
	if snapshot.DispatchScore <= 170 {
		t.Fatalf("DispatchScore = %v, want plan bias plus urgency bonus", snapshot.DispatchScore)
	}
	if snapshot.HealthTier != string(HealthTierHealthy) {
		t.Fatalf("HealthTier = %q, want %q", snapshot.HealthTier, HealthTierHealthy)
	}
}

func TestAccountPremium5hUrgencyBonusSkipsNearlyExhaustedWindow(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      96,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(30 * time.Minute),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.Breakdown.UsageUrgencyBonus5h != 0 {
		t.Fatalf("UsageUrgencyBonus5h = %v, want 0", snapshot.Breakdown.UsageUrgencyBonus5h)
	}
	if snapshot.DispatchScore != 150 {
		t.Fatalf("DispatchScore = %v, want only plan bias", snapshot.DispatchScore)
	}
}

func TestAccountPremium7dUrgencyBonusOnlyAffectsDispatchScore(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      63,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(36 * time.Hour),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.SchedulerScore != 100 {
		t.Fatalf("SchedulerScore = %v, want 100", snapshot.SchedulerScore)
	}
	if snapshot.Breakdown.UsageUrgencyBonus7d <= 20 {
		t.Fatalf("UsageUrgencyBonus7d = %v, want > 20", snapshot.Breakdown.UsageUrgencyBonus7d)
	}
	if snapshot.DispatchScore <= 170 {
		t.Fatalf("DispatchScore = %v, want plan bias plus 7d urgency bonus", snapshot.DispatchScore)
	}
	if snapshot.HealthTier != string(HealthTierHealthy) {
		t.Fatalf("HealthTier = %q, want %q", snapshot.HealthTier, HealthTierHealthy)
	}
}

func TestAccountPremium7dUrgencyBonusSkipsDistantReset(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      63,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(5 * 24 * time.Hour),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.Breakdown.UsageUrgencyBonus7d != 0 {
		t.Fatalf("UsageUrgencyBonus7d = %v, want 0", snapshot.Breakdown.UsageUrgencyBonus7d)
	}
	if snapshot.DispatchScore != 150 {
		t.Fatalf("DispatchScore = %v, want only plan bias", snapshot.DispatchScore)
	}
}

func TestStoreNextPrefersPremium7dResetSoonOverProvenAccount(t *testing.T) {
	now := time.Now()
	soon := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      63,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(36 * time.Hour),
	}
	later := &Account{
		DBID:                2,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      68,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(5 * 24 * time.Hour),
	}
	atomic.StoreInt64(&later.TotalRequests, 450)
	recomputeTestAccount(soon, 2)
	recomputeTestAccount(later, 2)

	store := &Store{
		accounts: []*Account{later, soon},
	}
	store.SetMaxConcurrency(2)

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != soon.DBID {
		t.Fatalf("Next() picked dbID=%d, want 7d reset-soon account %d", got.DBID, soon.DBID)
	}
}

func TestStoreNextPrefersPremium5hResetSoonWithinTier(t *testing.T) {
	now := time.Now()
	soon := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      25,
		UsagePercent5hValid: true,
		Reset5hAt:           now.Add(30 * time.Minute),
	}
	later := &Account{
		DBID:                2,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      25,
		UsagePercent5hValid: true,
		Reset5hAt:           now.Add(5 * time.Hour),
	}
	recomputeTestAccount(soon, 2)
	recomputeTestAccount(later, 2)

	store := &Store{
		accounts: []*Account{later, soon},
	}
	store.SetMaxConcurrency(2)

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != soon.DBID {
		t.Fatalf("Next() picked dbID=%d, want reset-soon account %d", got.DBID, soon.DBID)
	}
}

func TestParsePriceMultiplierFromName(t *testing.T) {
	tests := []struct {
		name string
		want float64
		ok   bool
	}{
		{name: "cheap-0.5", want: 0.5, ok: true},
		{name: "proxy 0.25", want: 0.25, ok: true},
		{name: "pool_a_0.8", want: 0.8, ok: true},
		{name: "xmin.2", want: 0.2, ok: true},
		{name: "cheap-.05", want: 0.05, ok: true},
		{name: "plain", ok: false},
		{name: "integer-2", ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParsePriceMultiplierFromName(tc.name)
			if ok != tc.ok {
				t.Fatalf("ok = %t, want %t", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("multiplier = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveAccountRowPriceMultiplierRequiresExplicitCredential(t *testing.T) {
	row := &database.AccountRow{Name: "cheap-0.5", Credentials: map[string]interface{}{}}
	if got := resolveAccountRowPriceMultiplier(row); got != 0.5 {
		t.Fatalf("multiplier inferred from name = %v, want 0.5", got)
	}

	row.Credentials["price_multiplier"] = 0.25
	if got := resolveAccountRowPriceMultiplier(row); got != 0.25 {
		t.Fatalf("explicit multiplier = %v, want 0.25", got)
	}
}

func TestDispatchMaxMultiplierFiltersDispatchCandidates(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:        2,
		TestConcurrency:       1,
		TestModel:             "gpt-5.4",
		DispatchMaxMultiplier: 0.5,
	})
	cheap := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.5}
	expensive := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.8}
	unset := &Account{DBID: 3, AccessToken: "token", Status: StatusReady, PlanType: "plus"}
	expensive.ScoreBiasOverride = int64Ptr(200)
	unset.ScoreBiasOverride = int64Ptr(300)
	for _, acc := range []*Account{cheap, expensive, unset} {
		recomputeTestAccount(acc, 2)
		store.AddAccount(acc)
	}

	got := store.NextExcludingWithFilter(0, nil, nil)
	if got == nil {
		t.Fatal("NextExcludingWithFilter returned nil")
	}
	if got.DBID != cheap.DBID {
		t.Fatalf("picked account %d, want cheap account %d", got.DBID, cheap.DBID)
	}
}

func TestDispatchMaxMultiplierTreatsUnsetAsOne(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:        2,
		TestConcurrency:       1,
		TestModel:             "gpt-5.4",
		DispatchMaxMultiplier: 1,
	})
	unset := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus"}
	recomputeTestAccount(unset, 2)
	store.AddAccount(unset)

	got := store.NextExcludingWithFilter(0, nil, nil)
	if got == nil || got.DBID != unset.DBID {
		t.Fatalf("picked account = %+v, want unset account treated as multiplier 1", got)
	}
}

func TestPriceMultiplierDoesNotDirectlyChangeDispatchScore(t *testing.T) {
	base := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus"}
	cheap := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.25}

	recomputeTestAccount(base, 4)
	recomputeTestAccount(cheap, 4)

	if base.DispatchScore != cheap.DispatchScore {
		t.Fatalf("cheap DispatchScore = %v, want same as base %v", cheap.DispatchScore, base.DispatchScore)
	}
}

func TestCheapProbeCandidatesUseMultiplierRanking(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	top := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 1}
	mid := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.8}
	low := &Account{DBID: 3, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.2}
	normal := &Account{DBID: 4, AccessToken: "token", Status: StatusReady, PlanType: "plus"}
	top.ScoreBiasOverride = int64Ptr(60)
	store.AddAccount(top)
	store.AddAccount(mid)
	store.AddAccount(low)
	store.AddAccount(normal)

	targets, probeTop := store.cheapProbeCandidates(store.Accounts(), time.Now())

	if !probeTop.found || probeTop.dbID != top.DBID {
		t.Fatalf("top = %+v, want dbID %d", probeTop, top.DBID)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	if targets[0].account.DBID != mid.DBID || targets[0].interval != store.cheapProbeIntervalForRank(0) {
		t.Fatalf("first target = dbID %d interval %s, want mid interval rank0", targets[0].account.DBID, targets[0].interval)
	}
	if targets[1].account.DBID != low.DBID || targets[1].interval != store.cheapProbeIntervalForRank(1) {
		t.Fatalf("second target = dbID %d interval %s, want low interval rank1", targets[1].account.DBID, targets[1].interval)
	}
}

func TestCheapProbeCandidatesTreatUnsetMultiplierAsOne(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	top := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.8}
	unset := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus"}
	cheap := &Account{DBID: 3, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.5}
	top.ScoreBiasOverride = int64Ptr(60)
	store.AddAccount(top)
	store.AddAccount(unset)
	store.AddAccount(cheap)

	targets, probeTop := store.cheapProbeCandidates(store.Accounts(), time.Now())

	if !probeTop.found || probeTop.dbID != top.DBID {
		t.Fatalf("top = %+v, want dbID %d", probeTop, top.DBID)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want only explicit cheap account", len(targets))
	}
	if targets[0].account.DBID != cheap.DBID {
		t.Fatalf("target dbID = %d, want cheap account %d", targets[0].account.DBID, cheap.DBID)
	}
}

func TestCheapProbeCandidatesRespectMaxMultiplier(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:          2,
		TestConcurrency:         1,
		TestModel:               "gpt-5.4",
		CheapProbeMaxMultiplier: 0.3,
	})
	top := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 1}
	tooExpensive := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.4}
	cheap := &Account{DBID: 3, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.2}
	top.ScoreBiasOverride = int64Ptr(60)
	store.AddAccount(top)
	store.AddAccount(tooExpensive)
	store.AddAccount(cheap)

	targets, probeTop := store.cheapProbeCandidates(store.Accounts(), time.Now())

	if !probeTop.found || probeTop.dbID != top.DBID {
		t.Fatalf("top = %+v, want dbID %d", probeTop, top.DBID)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want only account below max multiplier", len(targets))
	}
	if targets[0].account.DBID != cheap.DBID {
		t.Fatalf("target dbID = %d, want cheap account %d", targets[0].account.DBID, cheap.DBID)
	}
}

func TestCheapProbeCandidatesRespectDispatchSelectionState(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	pausedHighScore := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.2}
	lockedHighScore := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.04}
	top := &Account{DBID: 3, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.05}
	expensive := &Account{DBID: 4, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.1}
	cheap := &Account{DBID: 5, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.03}
	disabledCheap := &Account{DBID: 6, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.01}
	pausedHighScore.ScoreBiasOverride = int64Ptr(300)
	lockedHighScore.ScoreBiasOverride = int64Ptr(80)
	top.ScoreBiasOverride = int64Ptr(100)
	atomic.StoreInt32(&pausedHighScore.DispatchPaused, 1)
	atomic.StoreInt32(&disabledCheap.Disabled, 1)
	atomic.StoreInt32(&lockedHighScore.Locked, 1)
	store.AddAccount(pausedHighScore)
	store.AddAccount(lockedHighScore)
	store.AddAccount(top)
	store.AddAccount(expensive)
	store.AddAccount(cheap)
	store.AddAccount(disabledCheap)

	targets, probeTop := store.cheapProbeCandidates(store.Accounts(), time.Now())

	if !probeTop.found || probeTop.dbID != top.DBID || probeTop.priceMultiplier != top.PriceMultiplier {
		t.Fatalf("top = %+v, want dbID %d multiplier %v", probeTop, top.DBID, top.PriceMultiplier)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want locked and lower-multiplier selectable accounts", len(targets))
	}
	if targets[0].account.DBID != lockedHighScore.DBID {
		t.Fatalf("first target dbID = %d, want locked account %d", targets[0].account.DBID, lockedHighScore.DBID)
	}
	if targets[1].account.DBID != cheap.DBID {
		t.Fatalf("second target dbID = %d, want cheap account %d", targets[1].account.DBID, cheap.DBID)
	}
}

func TestCheapProbeRankPolicyIsConfigurable(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                    2,
		TestConcurrency:                   1,
		TestModel:                         "gpt-5.4",
		CheapProbeEnabled:                 true,
		CheapProbeRankBaseIntervalSeconds: 200,
		CheapProbeRankStepSeconds:         40,
		CheapProbeRankMinIntervalSeconds:  80,
	})

	if got := store.cheapProbeIntervalForRank(0); got != 200*time.Second {
		t.Fatalf("rank0 interval = %s, want 200s", got)
	}
	if got := store.cheapProbeIntervalForRank(1); got != 160*time.Second {
		t.Fatalf("rank1 interval = %s, want 160s", got)
	}
	if got := store.cheapProbeIntervalForRank(4); got != 80*time.Second {
		t.Fatalf("rank4 interval = %s, want min 80s", got)
	}
}

func TestRecordCheapProbeSuccessClearsCheapProbeState(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	cheap := &Account{
		DBID:                2,
		AccessToken:         "token",
		Status:              StatusCooldown,
		CooldownUtil:        time.Now().Add(time.Hour),
		CooldownReason:      "rate_limited",
		ErrorMsg:            "old cooldown",
		PlanType:            "plus",
		PriceMultiplier:     0.2,
		HealthTier:          HealthTierRisky,
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
		UsageUpdatedAt5h:    time.Now().Add(-time.Minute),
	}
	store.AddAccount(cheap)

	if !store.RecordCheapProbeSuccess(cheap) {
		t.Fatal("RecordCheapProbeSuccess returned false, want state cleared")
	}

	cheap.mu.RLock()
	defer cheap.mu.RUnlock()
	if cheap.Status != StatusReady || !cheap.CooldownUtil.IsZero() || cheap.CooldownReason != "" || cheap.ErrorMsg != "" {
		t.Fatalf("cheap status not cleared: status=%v cooldown=%v reason=%q error=%q", cheap.Status, cheap.CooldownUtil, cheap.CooldownReason, cheap.ErrorMsg)
	}
	if cheap.UsagePercent5hValid || !cheap.Reset5hAt.IsZero() || !cheap.UsageUpdatedAt5h.IsZero() {
		t.Fatalf("5h usage marker not cleared: valid=%t reset=%v updated=%v", cheap.UsagePercent5hValid, cheap.Reset5hAt, cheap.UsageUpdatedAt5h)
	}
	if cheap.CheapProbeRecoveryBonus != 0 || !cheap.CheapProbeBonusUntil.IsZero() {
		t.Fatalf("cheap probe bonus state not cleared: bonus=%v until=%v", cheap.CheapProbeRecoveryBonus, cheap.CheapProbeBonusUntil)
	}
}

func TestApplyCheapProbeRecoveryBonusUsesAccountOverride(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                 2,
		TestConcurrency:                1,
		TestModel:                      "gpt-5.4",
		CheapProbeEnabled:              true,
		CheapProbeRecoveryMargin:       10,
		CheapProbeBonusDurationMinutes: 10,
	})
	top := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 1}
	top.ScoreBiasOverride = int64Ptr(60)
	cheap := &Account{
		DBID:                     2,
		AccessToken:              "token",
		Status:                   StatusReady,
		PlanType:                 "plus",
		PriceMultiplier:          0.2,
		CheapProbeRecoveryMargin: 25,
		CheapProbeBonusDuration:  time.Minute,
	}
	store.AddAccount(top)
	store.AddAccount(cheap)
	topScore := top.GetDispatchScore()
	topSnapshot := cheapProbeTopAccount{
		dbID:            top.DBID,
		dispatchScore:   topScore,
		priceMultiplier: 1,
		found:           true,
	}
	batchVersion := store.cheapProbeTopologyVersion.Load()

	if !store.RecordCheapProbeSuccess(cheap) {
		t.Fatal("RecordCheapProbeSuccess returned false, want state cleared")
	}

	start := time.Now()
	if !store.applyCheapProbeRecoveryBonus(cheap, topSnapshot, start, batchVersion) {
		t.Fatal("applyCheapProbeRecoveryBonus returned false, want account override bonus applied")
	}

	cheap.mu.RLock()
	defer cheap.mu.RUnlock()
	if cheap.DispatchScore < topScore+25 {
		t.Fatalf("DispatchScore = %v, want >= %v", cheap.DispatchScore, topScore+25)
	}
	if cheap.CheapProbeBonusUntil.Before(start.Add(time.Minute)) || cheap.CheapProbeBonusUntil.After(start.Add(2*time.Minute)) {
		t.Fatalf("CheapProbeBonusUntil = %v, want about 1 minute after start", cheap.CheapProbeBonusUntil)
	}
}

func TestApplyCheapProbeRecoveryBonusSkipsBonusWhenCandidateNoLongerCheaperThanTop(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                 2,
		TestConcurrency:                1,
		TestModel:                      "gpt-5.4",
		CheapProbeEnabled:              true,
		CheapProbeRecoveryMargin:       10,
		CheapProbeBonusDurationMinutes: 10,
	})
	top := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.049}
	top.ScoreBiasOverride = int64Ptr(60)
	alsoCheap := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.0503}
	store.AddAccount(top)
	store.AddAccount(alsoCheap)

	topSnapshot := cheapProbeTopAccount{
		dbID:            top.DBID,
		dispatchScore:   top.GetDispatchScore(),
		priceMultiplier: 0.049,
		found:           true,
	}
	batchVersion := store.cheapProbeTopologyVersion.Load()
	if !store.RecordCheapProbeSuccess(alsoCheap) {
		t.Fatal("alsoCheap state not cleared")
	}
	if store.applyCheapProbeRecoveryBonus(alsoCheap, topSnapshot, time.Now(), batchVersion) {
		t.Fatal("alsoCheap bonus applied even though current top has a lower price multiplier")
	}

	alsoCheap.mu.RLock()
	alsoCheapBonus := alsoCheap.CheapProbeRecoveryBonus
	alsoCheapBonusUntil := alsoCheap.CheapProbeBonusUntil
	alsoCheap.mu.RUnlock()

	if alsoCheapBonus != 0 {
		t.Fatalf("alsoCheap bonus = %v, want 0", alsoCheapBonus)
	}
	if !alsoCheapBonusUntil.IsZero() {
		t.Fatalf("alsoCheap bonus until = %v, want zero", alsoCheapBonusUntil)
	}
}

func TestApplyCheapProbeRecoveryBonusSkipsAboveMaxMultiplier(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                 2,
		TestConcurrency:                1,
		TestModel:                      "gpt-5.4",
		CheapProbeEnabled:              true,
		CheapProbeRecoveryMargin:       10,
		CheapProbeBonusDurationMinutes: 10,
		CheapProbeMaxMultiplier:        0.3,
	})
	top := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 1}
	top.ScoreBiasOverride = int64Ptr(60)
	aboveMax := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.4}
	store.AddAccount(top)
	store.AddAccount(aboveMax)

	topSnapshot := cheapProbeTopAccount{
		dbID:            top.DBID,
		dispatchScore:   top.GetDispatchScore(),
		priceMultiplier: 1,
		found:           true,
	}
	batchVersion := store.cheapProbeTopologyVersion.Load()
	if !store.RecordCheapProbeSuccess(aboveMax) {
		t.Fatal("aboveMax state not cleared")
	}
	if store.applyCheapProbeRecoveryBonus(aboveMax, topSnapshot, time.Now(), batchVersion) {
		t.Fatal("aboveMax bonus applied even though multiplier is above configured max")
	}

	aboveMax.mu.RLock()
	bonus := aboveMax.CheapProbeRecoveryBonus
	bonusUntil := aboveMax.CheapProbeBonusUntil
	aboveMax.mu.RUnlock()
	if bonus != 0 {
		t.Fatalf("aboveMax bonus = %v, want 0", bonus)
	}
	if !bonusUntil.IsZero() {
		t.Fatalf("aboveMax bonus until = %v, want zero", bonusUntil)
	}
}

func TestSetCheapProbeMaxMultiplierClearsOutOfRangeBonus(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                 2,
		TestConcurrency:                1,
		TestModel:                      "gpt-5.4",
		CheapProbeEnabled:              true,
		CheapProbeRecoveryMargin:       10,
		CheapProbeBonusDurationMinutes: 10,
	})
	inRange := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.03}
	outOfRange := &Account{DBID: 2, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.08}
	for _, acc := range []*Account{inRange, outOfRange} {
		acc.CheapProbeRecoveryBonus = 50
		acc.CheapProbeBonusUntil = time.Now().Add(time.Hour)
		store.AddAccount(acc)
	}

	store.SetCheapProbeMaxMultiplier(0.05)

	inRange.mu.RLock()
	inRangeBonus := inRange.CheapProbeRecoveryBonus
	inRangeUntil := inRange.CheapProbeBonusUntil
	inRange.mu.RUnlock()
	outOfRange.mu.RLock()
	outOfRangeBonus := outOfRange.CheapProbeRecoveryBonus
	outOfRangeUntil := outOfRange.CheapProbeBonusUntil
	outOfRange.mu.RUnlock()

	if inRangeBonus == 0 || inRangeUntil.IsZero() {
		t.Fatal("in-range cheap probe bonus was unexpectedly cleared")
	}
	if outOfRangeBonus != 0 || !outOfRangeUntil.IsZero() {
		t.Fatalf("out-of-range bonus = %v until=%v, want cleared", outOfRangeBonus, outOfRangeUntil)
	}
	expectCheapProbeWake(t, store)
}

func TestCheapProbeSuccessSkipsDispatchPausedBeforeStateClear(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                 2,
		TestConcurrency:                1,
		TestModel:                      "gpt-5.4",
		CheapProbeEnabled:              true,
		CheapProbeRecoveryMargin:       10,
		CheapProbeBonusDurationMinutes: 10,
	})
	top := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 1}
	top.ScoreBiasOverride = int64Ptr(60)
	paused := &Account{
		DBID:            2,
		AccessToken:     "token",
		Status:          StatusCooldown,
		CooldownUtil:    time.Now().Add(time.Hour),
		CooldownReason:  "manual-paused-before-success",
		ErrorMsg:        "old cooldown",
		PlanType:        "plus",
		PriceMultiplier: 0.2,
	}
	store.AddAccount(top)
	store.AddAccount(paused)

	topSnapshot := cheapProbeTopAccount{
		dbID:            top.DBID,
		dispatchScore:   top.GetDispatchScore(),
		priceMultiplier: 1,
		found:           true,
	}
	batchVersion := store.cheapProbeTopologyVersion.Load()
	atomic.StoreInt32(&paused.DispatchPaused, 1)

	if _, ok := cheapProbeEligibleForSuccess(paused, store.GetCheapProbeMaxMultiplier()); ok {
		t.Fatal("dispatch-paused account is eligible for cheap-probe success handling")
	}
	if store.applyCheapProbeRecoveryBonus(paused, topSnapshot, time.Now(), batchVersion) {
		t.Fatal("dispatch-paused account received cheap-probe recovery bonus")
	}

	paused.mu.RLock()
	defer paused.mu.RUnlock()
	if paused.Status != StatusCooldown || paused.CooldownReason != "manual-paused-before-success" || paused.ErrorMsg != "old cooldown" {
		t.Fatalf("paused account state was cleared: status=%v reason=%q error=%q", paused.Status, paused.CooldownReason, paused.ErrorMsg)
	}
	if atomic.LoadInt32(&paused.DispatchPaused) != 1 {
		t.Fatal("dispatch-paused flag was cleared")
	}
}

func expectCheapProbeWake(t *testing.T, store *Store) {
	t.Helper()
	if store.cheapProbeWakeCh == nil {
		t.Fatal("cheapProbeWakeCh is nil")
	}
	select {
	case <-store.cheapProbeWakeCh:
	case <-time.After(time.Second):
		t.Fatal("cheapProbeWakeCh did not receive wake signal")
	}
}

func expectNoCheapProbeWake(t *testing.T, store *Store) {
	t.Helper()
	if store.cheapProbeWakeCh == nil {
		t.Fatal("cheapProbeWakeCh is nil")
	}
	select {
	case <-store.cheapProbeWakeCh:
		t.Fatal("cheapProbeWakeCh received unexpected wake signal")
	default:
	}
}

func TestCheapProbeConfigChangesWakeProbeLoop(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                2,
		TestConcurrency:               1,
		TestModel:                     "gpt-5.4",
		CheapProbeEnabled:             false,
		CheapProbeScanIntervalSeconds: 10,
	})
	expectNoCheapProbeWake(t, store)

	store.SetCheapProbeEnabled(true)
	expectCheapProbeWake(t, store)

	store.SetCheapProbeScanInterval(15 * time.Second)
	expectCheapProbeWake(t, store)

	store.SetCheapProbeMaxMultiplier(0.2)
	expectCheapProbeWake(t, store)
}

func TestCheapProbeTopologyChangesMarkRescanRequested(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	acc := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.5}

	if version := store.cheapProbeTopologyVersion.Load(); version != 0 {
		t.Fatalf("initial cheapProbeTopologyVersion = %d, want 0", version)
	}

	store.AddAccount(acc)
	if version := store.cheapProbeTopologyVersion.Load(); version != 1 {
		t.Fatalf("version after AddAccount = %d, want 1", version)
	}
	if !store.cheapProbeRescanRequested.Load() {
		t.Fatal("cheapProbeRescanRequested is false after AddAccount, want true")
	}
	expectCheapProbeWake(t, store)

	if !store.ApplyAccountPriceMultiplier(acc.DBID, 0.25) {
		t.Fatal("ApplyAccountPriceMultiplier returned false")
	}
	if version := store.cheapProbeTopologyVersion.Load(); version != 2 {
		t.Fatalf("version after ApplyAccountPriceMultiplier = %d, want 2", version)
	}
	if !store.cheapProbeRescanRequested.Load() {
		t.Fatal("cheapProbeRescanRequested is false after ApplyAccountPriceMultiplier, want true")
	}
	expectCheapProbeWake(t, store)
}

func TestRecordCheapProbeFailureDoesNotWriteCooldownOrError(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	acc := &Account{DBID: 1, AccessToken: "token", Status: StatusReady, PlanType: "plus", PriceMultiplier: 0.5}

	store.RecordCheapProbeFailure(acc, fmt.Errorf("temporary upstream failure"))

	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if acc.Status != StatusReady {
		t.Fatalf("Status = %v, want ready", acc.Status)
	}
	if !acc.CooldownUtil.IsZero() || acc.CooldownReason != "" || acc.ErrorMsg != "" {
		t.Fatalf("failure wrote cooldown/error: cooldown=%v reason=%q error=%q", acc.CooldownUtil, acc.CooldownReason, acc.ErrorMsg)
	}
	if acc.LastCheapProbeError == "" {
		t.Fatal("LastCheapProbeError is empty, want recorded probe error")
	}
}

func TestApplyAccountPriceMultiplierClearsCheapProbeState(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	lastProbeAt := time.Now().Add(-time.Hour)
	lastSuccessAt := time.Now().Add(-30 * time.Minute)
	acc := &Account{
		DBID:                    1,
		AccessToken:             "token",
		Status:                  StatusReady,
		PlanType:                "plus",
		PriceMultiplier:         0.5,
		LastCheapProbeAt:        lastProbeAt,
		LastCheapProbeSuccessAt: lastSuccessAt,
		LastCheapProbeError:     "old probe error",
		CheapProbeRecoveryBonus: 20,
		CheapProbeBonusUntil:    time.Now().Add(time.Minute),
	}
	store.AddAccount(acc)
	beforeVersion := store.cheapProbeTopologyVersion.Load()

	if !store.ApplyAccountPriceMultiplier(acc.DBID, 0.25) {
		t.Fatal("ApplyAccountPriceMultiplier returned false")
	}

	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if acc.PriceMultiplier != 0.25 {
		t.Fatalf("PriceMultiplier = %v, want 0.25", acc.PriceMultiplier)
	}
	if !acc.LastCheapProbeAt.IsZero() || !acc.LastCheapProbeSuccessAt.IsZero() ||
		acc.LastCheapProbeError != "" || acc.CheapProbeRecoveryBonus != 0 || !acc.CheapProbeBonusUntil.IsZero() {
		t.Fatalf("cheap probe state not cleared: last=%v success=%v err=%q bonus=%v until=%v",
			acc.LastCheapProbeAt, acc.LastCheapProbeSuccessAt, acc.LastCheapProbeError, acc.CheapProbeRecoveryBonus, acc.CheapProbeBonusUntil)
	}
	if got := store.cheapProbeTopologyVersion.Load(); got != beforeVersion+1 {
		t.Fatalf("cheapProbeTopologyVersion = %d, want %d", got, beforeVersion+1)
	}
	if !store.cheapProbeRescanRequested.Load() {
		t.Fatal("cheapProbeRescanRequested is false after ApplyAccountPriceMultiplier, want true")
	}
}
