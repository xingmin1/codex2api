package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func TestNextForSessionPrefersBoundAccountAndProxy(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", 0, nil)
	if acc == nil {
		t.Fatal("expected account")
	}
	if acc.DBID != 2 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 2)
	}
	if proxyURL != "http://proxy-2" {
		t.Fatalf("proxyURL = %q, want %q", proxyURL, "http://proxy-2")
	}
}

func TestNextForSessionUsesCachedAffinityWhenLocalBindingMissing(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	if err := tokenCache.SetSessionAffinity(context.Background(), "session-redis", cache.SessionAffinityBinding{
		AccountID: 2,
		ProxyURL:  "http://proxy-2",
	}, time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 2,
		tokenCache:     tokenCache,
	}

	acc, proxyURL := store.NextForSession("session-redis", 0, nil)
	if acc == nil {
		t.Fatal("expected account")
	}
	if acc.DBID != 2 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 2)
	}
	if proxyURL != "http://proxy-2" {
		t.Fatalf("proxyURL = %q, want %q", proxyURL, "http://proxy-2")
	}
}

func TestBindSessionAffinityUsesConfigurableTTL(t *testing.T) {
	t.Setenv("CODEX_SESSION_AFFINITY_TTL", "2h")
	store := &Store{}
	account := &Account{DBID: 1, AccessToken: "tok-1"}

	before := time.Now()
	store.bindSessionAffinity("session-ttl", account, "http://proxy-1")

	store.sessionMu.RLock()
	binding, ok := store.sessionBindings["session-ttl"]
	store.sessionMu.RUnlock()
	if !ok {
		t.Fatal("expected session binding")
	}
	if binding.expiresAt.Before(before.Add(2*time.Hour - time.Second)) {
		t.Fatalf("expiresAt too early: got %s want about 2h from now", binding.expiresAt)
	}
}

func TestBindSessionAffinityDoesNotExtendExistingDeadline(t *testing.T) {
	t.Setenv("CODEX_SESSION_AFFINITY_TTL", "2h")
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{}
	store.bindSessionAffinity("session-fixed-deadline", account, "http://proxy-1")

	store.sessionMu.RLock()
	first := store.sessionBindings["session-fixed-deadline"]
	store.sessionMu.RUnlock()
	store.bindSessionAffinity("session-fixed-deadline", account, "http://proxy-1")
	store.sessionMu.RLock()
	second := store.sessionBindings["session-fixed-deadline"]
	store.sessionMu.RUnlock()

	if !second.boundAt.Equal(first.boundAt) {
		t.Fatalf("boundAt changed: got %s want %s", second.boundAt, first.boundAt)
	}
	if !second.expiresAt.Equal(first.expiresAt) {
		t.Fatalf("expiresAt extended: got %s want %s", second.expiresAt, first.expiresAt)
	}
}

func TestBindSessionAffinityDoesNotExtendCachedDeadline(t *testing.T) {
	t.Setenv("CODEX_SESSION_AFFINITY_TTL", "2h")
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{tokenCache: tokenCache}

	store.bindSessionAffinity("session-cache-deadline", account, "http://proxy-1")
	store.sessionMu.Lock()
	binding := store.sessionBindings["session-cache-deadline"]
	binding.expiresAt = time.Now().Add(500 * time.Millisecond)
	store.sessionBindings["session-cache-deadline"] = binding
	store.sessionMu.Unlock()
	store.bindSessionAffinity("session-cache-deadline", account, "http://proxy-1")
	time.Sleep(600 * time.Millisecond)

	if _, ok, err := tokenCache.GetSessionAffinity(context.Background(), "session-cache-deadline"); err != nil || ok {
		t.Fatalf("cached deadline was extended: ok=%v err=%v", ok, err)
	}
}

func TestBoundedAffinityEscapesAfterLocalRequestLimit(t *testing.T) {
	now := time.Now()
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", SchedulerPriority: 10},
			{DBID: 2, AccessToken: "tok-2", SchedulerPriority: 0},
		},
		maxConcurrency: 2,
		sessionBindings: map[string]sessionAffinity{
			"session-request-limit": {
				accountID:    2,
				boundAt:      now.Add(-time.Minute),
				requestCount: defaultMaxAffinityRequests,
				expiresAt:    now.Add(time.Hour),
			},
		},
	}

	account, _ := store.NextForSession("session-request-limit", 0, nil)
	if account == nil || account.DBID != 1 {
		t.Fatalf("account = %+v, want DBID=1", account)
	}
}

func TestBoundedAffinityEscapesAfterLocalDurationLimit(t *testing.T) {
	now := time.Now()
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", SchedulerPriority: 10},
			{DBID: 2, AccessToken: "tok-2", SchedulerPriority: 0},
		},
		maxConcurrency: 2,
		sessionBindings: map[string]sessionAffinity{
			"session-duration-limit": {
				accountID: 2,
				boundAt:   now.Add(-defaultMaxAffinityDuration),
				expiresAt: now.Add(time.Hour),
			},
		},
	}

	account, _ := store.NextForSession("session-duration-limit", 0, nil)
	if account == nil || account.DBID != 1 {
		t.Fatalf("account = %+v, want DBID=1", account)
	}
}

func TestLegacyCachedAffinityIsUpgradedWithoutSlidingDeadline(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	legacy := cache.SessionAffinityBinding{AccountID: 2, ProxyURL: "http://proxy-2"}
	if err := tokenCache.SetSessionAffinity(context.Background(), "session-legacy", legacy, time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 2,
		tokenCache:     tokenCache,
	}

	account, proxyURL := store.NextForSession("session-legacy", 0, nil)
	if account == nil || account.DBID != 2 || proxyURL != "http://proxy-2" {
		t.Fatalf("selection = account %+v proxy %q, want DBID=2 with cached proxy", account, proxyURL)
	}
	store.sessionMu.RLock()
	restored := store.sessionBindings["session-legacy"]
	store.sessionMu.RUnlock()
	store.bindSessionAffinity("session-legacy", account, proxyURL)
	store.sessionMu.RLock()
	upgraded := store.sessionBindings["session-legacy"]
	store.sessionMu.RUnlock()

	if restored.boundAt.IsZero() || restored.expiresAt.IsZero() {
		t.Fatalf("legacy cache metadata was not initialized: %+v", restored)
	}
	if !upgraded.boundAt.Equal(restored.boundAt) || !upgraded.expiresAt.Equal(restored.expiresAt) {
		t.Fatalf("legacy deadline slid after writeback: restored=%+v upgraded=%+v", restored, upgraded)
	}
}

func TestBoundedAffinityEscapeDoesNotRestoreCachedBinding(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	now := time.Now()
	stale := sessionAffinity{
		accountID:    2,
		proxyURL:     "http://proxy-2",
		boundAt:      now.Add(-time.Minute),
		requestCount: defaultMaxAffinityRequests,
		expiresAt:    now.Add(time.Hour),
	}
	if err := tokenCache.SetSessionAffinity(context.Background(), "session-escape", sessionAffinityCacheBinding(stale), time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", SchedulerPriority: 10},
			{DBID: 2, AccessToken: "tok-2", SchedulerPriority: 0},
		},
		maxConcurrency:  2,
		tokenCache:      tokenCache,
		sessionBindings: map[string]sessionAffinity{"session-escape": stale},
	}

	account, proxyURL := store.NextForSession("session-escape", 0, nil)
	if account == nil || account.DBID != 1 {
		t.Fatalf("account = %+v, want DBID=1", account)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty", proxyURL)
	}
	if _, ok, err := tokenCache.GetSessionAffinity(context.Background(), "session-escape"); err != nil || ok {
		t.Fatalf("escaped cached binding still exists: ok=%v err=%v", ok, err)
	}
}

func TestBoundedAffinityHonorsCachedRequestCount(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	now := time.Now()
	cached := sessionAffinity{
		accountID:    2,
		proxyURL:     "http://proxy-2",
		boundAt:      now.Add(-time.Minute),
		requestCount: defaultMaxAffinityRequests,
		expiresAt:    now.Add(time.Hour),
	}
	if err := tokenCache.SetSessionAffinity(context.Background(), "session-cached-limit", sessionAffinityCacheBinding(cached), time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", SchedulerPriority: 10},
			{DBID: 2, AccessToken: "tok-2", SchedulerPriority: 0},
		},
		maxConcurrency: 2,
		tokenCache:     tokenCache,
	}

	account, _ := store.NextForSession("session-cached-limit", 0, nil)
	if account == nil || account.DBID != 1 {
		t.Fatalf("account = %+v, want DBID=1", account)
	}
}

func TestConditionalAffinityDeleteDoesNotRemoveNewBinding(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	oldBinding := cache.SessionAffinityBinding{
		AccountID:         1,
		BoundAtUnixNano:   "1700000000000000001",
		ExpiresAtUnixNano: "1700003600000000001",
		RequestCount:      3,
	}
	newBinding := oldBinding
	newBinding.BoundAtUnixNano = "1700000000000000002"
	newBinding.ExpiresAtUnixNano = "1700003600000000002"
	if err := tokenCache.SetSessionAffinity(context.Background(), "session-race", newBinding, time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	if err := tokenCache.DeleteSessionAffinityIfMatches(context.Background(), "session-race", oldBinding); err != nil {
		t.Fatalf("DeleteSessionAffinityIfMatches: %v", err)
	}
	got, ok, err := tokenCache.GetSessionAffinity(context.Background(), "session-race")
	if err != nil || !ok {
		t.Fatalf("new binding missing: ok=%v err=%v", ok, err)
	}
	if got != newBinding {
		t.Fatalf("binding = %+v, want %+v", got, newBinding)
	}
}

func TestConditionalAffinityDeleteIgnoresStaleRequestCount(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	cached := cache.SessionAffinityBinding{
		AccountID:         1,
		BoundAtUnixNano:   "1",
		ExpiresAtUnixNano: "2",
		RequestCount:      3,
	}
	if err := tokenCache.SetSessionAffinity(context.Background(), "session-stale-count", cached, time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	local := cached
	local.RequestCount = 4
	if err := tokenCache.DeleteSessionAffinityIfMatches(context.Background(), "session-stale-count", local); err != nil {
		t.Fatalf("DeleteSessionAffinityIfMatches: %v", err)
	}
	if _, ok, err := tokenCache.GetSessionAffinity(context.Background(), "session-stale-count"); err != nil || ok {
		t.Fatalf("stale logical binding still exists: ok=%v err=%v", ok, err)
	}
}

func TestStrictAffinityIgnoresBoundedEscapeLimits(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", SchedulerPriority: 10},
			{DBID: 2, AccessToken: "tok-2", SchedulerPriority: 0},
		},
		maxConcurrency: 2,
		sessionBindings: map[string]sessionAffinity{
			"session-strict": {
				accountID:    2,
				requestCount: defaultMaxAffinityRequests,
				boundAt:      time.Now().Add(-defaultMaxAffinityDuration),
				expiresAt:    time.Now().Add(time.Hour),
			},
		},
	}
	store.SetAffinityMode(AffinityModeStrict)

	account, _ := store.NextForSession("session-strict", 0, nil)
	if account == nil || account.DBID != 2 {
		t.Fatalf("account = %+v, want DBID=2", account)
	}
}

func TestOffAffinityAlwaysUsesFullScheduling(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", SchedulerPriority: 10},
			{DBID: 2, AccessToken: "tok-2", SchedulerPriority: 0},
		},
		maxConcurrency: 2,
		sessionBindings: map[string]sessionAffinity{
			"session-off": {
				accountID: 2,
				expiresAt: time.Now().Add(time.Hour),
			},
		},
	}
	store.SetAffinityMode(AffinityModeOff)

	account, _ := store.NextForSession("session-off", 0, nil)
	if account == nil || account.DBID != 1 {
		t.Fatalf("account = %+v, want DBID=1", account)
	}
}

func TestNextForSessionFallsBackWhenBoundAccountExcluded(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", 0, map[int64]bool{2: true})
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNextForSessionWithFilterFallsBackWhenBoundAccountRejected(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", PlanType: "pro"},
			{DBID: 2, AccessToken: "tok-2", PlanType: "plus"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSessionWithFilter("session-1", 0, nil, func(acc *Account) bool {
		return acc.GetPlanType() == "pro"
	})
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNextForSessionFallsBackWhenBoundAccountIsError(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", Status: StatusError, ErrorMsg: "deactivated_workspace"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
	if store.accounts[1].IsAvailable() {
		t.Fatal("error account should not be available for scheduling")
	}
}

func TestWaitForSessionAvailableReturnsBoundAccount(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 1,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, 0, nil)
	if acc == nil {
		t.Fatal("expected bound account")
	}
	if acc.DBID != 2 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 2)
	}
	if proxyURL != "http://proxy-2" {
		t.Fatalf("proxyURL = %q, want %q", proxyURL, "http://proxy-2")
	}
}

func TestWaitForSessionAvailableFallsBackWhenBindingExpired(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency:  1,
		sessionBindings: map[string]sessionAffinity{"session-1": {accountID: 99, proxyURL: "http://stale", expiresAt: time.Now().Add(-time.Minute)}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestWaitForSessionAvailableRespectsExcludeSet(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 1,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, 0, map[int64]bool{2: true})
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestWaitForSessionAvailableReturnsImmediatelyWhenNoDispatchCandidate(t *testing.T) {
	store := &Store{
		accounts:       []*Account{},
		maxConcurrency: 1,
	}

	start := time.Now()
	acc, proxyURL := store.WaitForSessionAvailable(context.Background(), "", 2*time.Second, 0, nil)
	elapsed := time.Since(start)

	if acc != nil {
		t.Fatalf("account = %+v, want nil", acc)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty", proxyURL)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("WaitForSessionAvailable took %s with no dispatch candidates; want fast failure", elapsed)
	}
}

func TestWaitForSessionAvailableKeepsWaitingWhenCandidateIsBusy(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{
		accounts:       []*Account{account},
		maxConcurrency: 1,
	}
	atomic.StoreInt64(&account.ActiveRequests, 1)

	go func() {
		time.Sleep(75 * time.Millisecond)
		store.Release(account)
	}()

	acc, proxyURL := store.WaitForSessionAvailable(context.Background(), "", 500*time.Millisecond, 0, nil)
	if acc == nil {
		t.Fatal("expected busy candidate to become available")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty", proxyURL)
	}
}

func TestUnbindSessionAffinityRemovesMatchingBinding(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency: 1,
	}
	// 绑定一个不在 accounts 列表中的账号，unbind 后只能回退到 DBID=1
	store.bindSessionAffinity("session-1", &Account{DBID: 2, AccessToken: "tok-2"}, "http://proxy-2")

	store.UnbindSessionAffinity("session-1", 2)

	acc, proxyURL := store.NextForSession("session-1", 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNextForSessionFallsBackWhenAPIKeyNotAllowed(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", AllowedAPIKeyIDs: []int64{2}, allowedAPIKeySet: map[int64]struct{}{2: {}}},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", 1, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNextForSessionFallsBackWhenAPIKeyGroupNotAllowed(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", GroupIDs: []int64{20}},
			{DBID: 2, AccessToken: "tok-2", GroupIDs: []int64{10}},
		},
		maxConcurrency: 2,
	}
	store.SetAPIKeyAllowedGroups(1, []int64{20})
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", 1, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}
