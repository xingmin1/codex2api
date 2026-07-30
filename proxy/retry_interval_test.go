package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func newRetryTestHandler(t *testing.T) (*Handler, *auth.Store) {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 2})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	return handler, store
}

func TestWaitBeforeRetry(t *testing.T) {
	h, store := newRetryTestHandler(t)

	t.Run("间隔为 0 立即返回", func(t *testing.T) {
		store.SetRetryIntervalMS(0)
		start := time.Now()
		if !h.waitBeforeRetry(context.Background()) {
			t.Fatal("want true")
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Fatalf("interval 0 should not wait, took %v", elapsed)
		}
	})

	t.Run("ctx 已取消返回 false", func(t *testing.T) {
		store.SetRetryIntervalMS(0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if h.waitBeforeRetry(ctx) {
			t.Fatal("want false for canceled ctx")
		}
	})

	t.Run("按配置间隔等待", func(t *testing.T) {
		store.SetRetryIntervalMS(60)
		start := time.Now()
		if !h.waitBeforeRetry(context.Background()) {
			t.Fatal("want true")
		}
		if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
			t.Fatalf("should wait ~60ms, took %v", elapsed)
		}
	})

	t.Run("等待中客户端断开返回 false", func(t *testing.T) {
		store.SetRetryIntervalMS(5000)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		if h.waitBeforeRetry(ctx) {
			t.Fatal("want false when canceled mid-wait")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("cancel should abort the wait promptly, took %v", elapsed)
		}
	})
}

func TestRetrySettingsNormalization(t *testing.T) {
	_, store := newRetryTestHandler(t)

	store.SetRetryIntervalMS(-5)
	if got := store.GetRetryIntervalMS(); got != 0 {
		t.Errorf("negative interval → %d, want 0", got)
	}
	store.SetRetryIntervalMS(99999)
	if got := store.GetRetryIntervalMS(); got != 30000 {
		t.Errorf("oversized interval → %d, want 30000", got)
	}

	store.SetTransportRetryPolicy(" STICKY ")
	if got := store.GetTransportRetryPolicy(); got != "sticky" {
		t.Errorf("policy STICKY → %q, want sticky", got)
	}
	store.SetTransportRetryPolicy(" HYBRID ")
	if got := store.GetTransportRetryPolicy(); got != "hybrid" {
		t.Errorf("policy HYBRID → %q, want hybrid", got)
	}
	store.SetTransportRetryPolicy("whatever")
	if got := store.GetTransportRetryPolicy(); got != "rotate" {
		t.Errorf("unknown policy → %q, want rotate", got)
	}
}

func TestSameAccountRetryIncludesUnclassifiedUpstreamErrors(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetTransportRetryPolicy("hybrid")
	store.SetTransportSameAccountRetries(1)
	account := &auth.Account{DBID: 1, AccessToken: "token"}
	tracker := newTransportRetryTracker()

	retry, failures, limit := tracker.shouldRetrySameAccount(handler, account, true, false, "")
	if !retry || failures != 1 || limit != 1 {
		t.Fatalf("first retry = %v failures=%d limit=%d, want true 1 1", retry, failures, limit)
	}
	retry, failures, limit = tracker.shouldRetrySameAccount(handler, account, true, false, "")
	if retry || failures != 2 || limit != 1 {
		t.Fatalf("second retry = %v failures=%d limit=%d, want false 2 1", retry, failures, limit)
	}
}

func TestRelayFailureWindowCountsEverySameAccountAttempt(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetFailureScoreThreshold(100)
	store.SetTransportRetryPolicy("hybrid")
	store.SetTransportSameAccountRetries(2)
	account := &auth.Account{
		DBID:                              1,
		UpstreamType:                      auth.UpstreamOpenAIResponses,
		BaseURL:                           "https://relay.example.com",
		APIKey:                            "sk-test",
		IgnoreUsageLimit429Cooldown:       true,
		FailureScoreThresholdEffective:    100,
		FailureToleranceWindowEffective:   60,
		FailureCooldownThresholdEffective: 1,
		HealthTier:                        auth.HealthTierHealthy,
	}
	tracker := newTransportRetryTracker()

	for attempt := 0; attempt < 3; attempt++ {
		tracker.shouldRetrySameAccount(handler, account, true, false, "server")
		handler.reportUpstreamAttemptFailure(account, "server", time.Millisecond)
	}

	_, _, _, _, _, _, _, failures := account.FailureToleranceSnapshot()
	if failures != 3 {
		t.Fatalf("failure window count = %d, want 3", failures)
	}
}

func TestRelayHTTPFailuresUseOneGenericKind(t *testing.T) {
	account := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example.com",
		APIKey:       "sk-test",
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		if got := classifyHTTPFailureForAccount(account, status); got != "upstream" {
			t.Fatalf("status %d kind = %q, want upstream", status, got)
		}
	}
	outcome := classifyResponseFailedOutcomeForAccount(account, []byte(`{"type":"response.failed","response":{"error":{"code":"invalid_value","message":"temporary"}}}`))
	if outcome.failureKind != "upstream" || outcome.penalize || !outcome.deterministicClientError {
		t.Fatalf("relay response.failed outcome = %+v, want generic non-penalized deterministic failure", outcome)
	}
}

func TestRelay429UsesGeneralRetryBudget(t *testing.T) {
	account := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example.com",
		APIKey:       "sk-test",
	}
	generalRetries := 0
	rateLimitRetries := 0
	if !shouldRetryHTTPStatusForAccount(account, http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("relay 429 should use the ordinary retry path")
	}
	if generalRetries != 1 || rateLimitRetries != 0 {
		t.Fatalf("retry budgets = general:%d rate_limit:%d, want 1/0", generalRetries, rateLimitRetries)
	}
}

func TestSameAccountRetryExcludesFirstTokenTimeout(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetTransportRetryPolicy("sticky")
	account := &auth.Account{DBID: 1, AccessToken: "token"}

	retry, _, _ := newTransportRetryTracker().shouldRetrySameAccount(handler, account, true, true, "timeout")
	if retry {
		t.Fatal("first-token timeout must not enter same-account retry")
	}
}

func TestSameAccountStreamRetryEligibility(t *testing.T) {
	upstream400 := streamOutcome{logStatusCode: http.StatusBadRequest, failureKind: "client", penalize: false}
	if !sameAccountStreamRetryEligible(false, upstream400, false, nil, nil) {
		t.Fatal("上游 4xx response.failed 应纳入同号重试，不能复用扣分判定")
	}
	if !sameAccountStreamRetryEligible(true, upstream400, false, nil, nil) {
		t.Fatal("compact 上游流错误应满足安全同号重试条件")
	}
	if sameAccountStreamRetryEligible(false, upstream400, true, nil, nil) {
		t.Fatal("已写出下游内容后不能透明同号重试")
	}
	if sameAccountStreamRetryEligible(false, upstream400, false, context.Canceled, nil) {
		t.Fatal("客户端取消不能作为上游错误同号重试")
	}
	if sameAccountStreamRetryEligible(false, streamOutcome{logStatusCode: http.StatusOK}, false, nil, nil) {
		t.Fatal("成功流不能进入同号重试")
	}
}

func TestCompactSameAccountRetryProtectsOnlyInitialAccount(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetTransportRetryPolicy("rotate")
	store.SetCompactSameAccountRetries(2)
	first := &auth.Account{DBID: 1, AccessToken: "first"}
	second := &auth.Account{DBID: 2, AccessToken: "second"}
	tracker := newTransportRetryTracker()

	tracker.captureCompactInitialAccount(handler, first, true)
	for failure := 1; failure <= 2; failure++ {
		retry, failures, limit := tracker.shouldRetryForRequest(handler, first, true, true, failure == 1, "http")
		if !retry || failures != failure || limit != 2 {
			t.Fatalf("首账号第 %d 次失败 = retry:%v failures:%d limit:%d, want true/%d/2", failure, retry, failures, limit, failure)
		}
	}
	if retry, failures, limit := tracker.shouldRetryForRequest(handler, first, true, true, false, "http"); retry || failures != 3 || limit != 2 {
		t.Fatalf("首账号预算耗尽 = retry:%v failures:%d limit:%d, want false/3/2", retry, failures, limit)
	}
	if retry, _, _ := tracker.shouldRetryForRequest(handler, second, true, true, false, "http"); retry {
		t.Fatal("后续账号不能获得新的 compact 同号预算")
	}
}

func TestCompactSameAccountRetryBudgetDoesNotResetWithTransportRounds(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetCompactSameAccountRetries(1)
	account := &auth.Account{DBID: 1, AccessToken: "token"}
	tracker := newTransportRetryTracker()
	tracker.captureCompactInitialAccount(handler, account, true)

	if retry, _, _ := tracker.shouldRetryForRequest(handler, account, true, true, false, "http"); !retry {
		t.Fatal("首个 compact 错误应同号重试")
	}
	if got := tracker.stateMachineAttempt(1, true); got != 0 {
		t.Fatalf("compact 同号重试不应消耗原状态机 attempt: got %d, want 0", got)
	}
	tracker.reset()
	if retry, failures, limit := tracker.shouldRetryForRequest(handler, account, true, true, false, "http"); retry || failures != 2 || limit != 1 {
		t.Fatalf("状态机新轮次不能重置 compact 预算: retry=%v failures=%d limit=%d", retry, failures, limit)
	}
	if got := tracker.stateMachineAttempt(2, true); got != 1 {
		t.Fatalf("预算耗尽后应从原状态机第 1 次重试继续: got %d, want 1", got)
	}
}

func TestSameAccountRetryRequestErrorMarksUsageAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	handler, _ := newRetryTestHandler(t)
	input := &database.UsageLogInput{AccountID: 1, Endpoint: "/v1/responses"}

	handler.logSameAccountRetryRequestError(ctx, input, 2, "transport", errors.New("connection reset"))

	if !input.IsRetryAttempt || input.AttemptIndex != 3 {
		t.Fatalf("retry metadata = enabled:%v attempt:%d, want true/3", input.IsRetryAttempt, input.AttemptIndex)
	}
	if input.StatusCode != http.StatusBadGateway || input.UpstreamErrorKind != "transport" || !strings.Contains(input.ErrorMessage, "connection reset") {
		t.Fatalf("usage failure metadata = %+v", input)
	}
}

func TestApply429CooldownTreatsRelay429AsOrdinaryUpstreamFailure(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetTransportRetryPolicy("hybrid")
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example.com",
		APIKey:       "sk-test",
		PlanType:     "api",
	}
	body := []byte(`{"error":{"type":"usage_limit_reached"}}`)

	decision := handler.applyCooldownForModel(account, http.StatusTooManyRequests, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")
	if decision.Reason != "" || decision.Scope != "" {
		t.Fatalf("decision = %#v, want empty ordinary-error decision", decision)
	}
	if account.HasActiveCooldown() {
		t.Fatal("429 under same-account policy must not write account cooldown")
	}
}

func TestRelayFailuresNeverWriteCodexSemanticState(t *testing.T) {
	handler, _ := newRetryTestHandler(t)
	cases := []struct {
		status int
		body   string
	}{
		{status: http.StatusUnauthorized, body: `{"error":{"code":"token_invalidated"}}`},
		{status: http.StatusForbidden, body: `{"error":{"type":"deactivated_workspace"}}`},
		{status: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached"}}`},
	}
	for _, tc := range cases {
		account := &auth.Account{
			DBID:         int64(tc.status),
			UpstreamType: auth.UpstreamOpenAIResponses,
			BaseURL:      "https://relay.example.com",
			APIKey:       "sk-test",
			Status:       auth.StatusReady,
			HealthTier:   auth.HealthTierHealthy,
		}
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("x-codex-primary-used-percent", "100")
		resp.Header.Set("x-codex-primary-window-minutes", "300")
		handler.applyCooldownForModel(account, tc.status, []byte(tc.body), resp, "gpt-5.4")
		SyncCodexFailureUsageState(handler.store, account, resp)
		if account.HasActiveCooldown() || account.RuntimeStatus() != "active" || account.IsModelRateLimited("gpt-5.4") {
			t.Fatalf("status %d wrote relay semantic state: runtime=%s", tc.status, account.RuntimeStatus())
		}
		if _, _, ok := account.GetUsageSnapshot5h(); ok {
			t.Fatalf("status %d persisted relay usage headers", tc.status)
		}
	}
}

func TestSameAccountRetryDoesNotSuppressOfficialFailureState(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetTransportRetryPolicy("hybrid")
	account := &auth.Account{DBID: 1, AccessToken: "token", PlanType: "pro", Status: auth.StatusReady}
	body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`)
	handler.applyCooldownForModel(account, http.StatusTooManyRequests, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")
	if !account.HasActiveCooldown() {
		t.Fatal("same-account retry policy must not suppress official 429 cooldown state")
	}
}

// runWSTransportRetryScenario 驱动入站 WS 连续返回指定次数的传输错误，
// 返回每次尝试使用的账号 ID，用于验证三种传输错误重试策略。
func runWSTransportRetryScenario(t *testing.T, policy string, sameAccountRetries, failures int) []int64 {
	t.Helper()
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = true
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 2
	ApplyRuntimeSettings(nextSettings)

	var calls atomic.Int64
	attemptCh := make(chan int64, 16)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptCh <- account.ID()
		if calls.Add(1) <= int64(failures) {
			return nil, errors.New("read tcp 127.0.0.1:443: connection reset by peer")
		}
		sse := `data: {"type":"response.created"}` + "\n\n" +
			`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store := newRetryTestHandler(t)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	store.SetRetryIntervalMS(10)
	store.SetTransportRetryPolicy(policy)
	store.SetTransportSameAccountRetries(sameAccountRetries)
	// 同号重试必须由请求内目标保证，不能依赖 session affinity 恰好仍有效。
	store.SetAffinityMode(auth.AffinityModeOff)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// 读到 response.completed 为止,确认第二次尝试的流被正常转发
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, frame, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read stream frame: %v", err)
		}
		if gjson.GetBytes(frame, "type").String() == "response.completed" {
			break
		}
	}

	readAttempt := func() int64 {
		select {
		case id := <-attemptCh:
			return id
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for attempt")
			return 0
		}
	}
	attempts := make([]int64, failures+1)
	for i := range attempts {
		attempts[i] = readAttempt()
	}
	return attempts
}

// 上游错误 + sticky 策略:同号重试(不换号、保留请求内账号和代理)。issue #331
func TestResponsesWebSocketTransportRetrySticky(t *testing.T) {
	attempts := runWSTransportRetryScenario(t, "sticky", 0, 1)
	if attempts[0] != attempts[1] {
		t.Fatalf("sticky 策略应同号重试: attempts=%v", attempts)
	}
}

// 上游错误 + rotate 策略(默认):换号重试,保持旧行为。
func TestResponsesWebSocketTransportRetryRotate(t *testing.T) {
	attempts := runWSTransportRetryScenario(t, "rotate", 2, 1)
	if attempts[0] == attempts[1] {
		t.Fatalf("rotate 策略应换号重试: attempts=%v", attempts)
	}
}

func TestResponsesWebSocketTransportRetryHybridRetriesSameAccountThenRotates(t *testing.T) {
	attempts := runWSTransportRetryScenario(t, "hybrid", 2, 3)
	if attempts[0] != attempts[1] || attempts[1] != attempts[2] {
		t.Fatalf("hybrid 策略前两次额外重试应保持同号: attempts=%v", attempts)
	}
	if attempts[2] == attempts[3] {
		t.Fatalf("hybrid 策略同号预算耗尽后应换号: attempts=%v", attempts)
	}
}

func TestResponsesWebSocketTransportRetryHybridZeroRotatesImmediately(t *testing.T) {
	attempts := runWSTransportRetryScenario(t, "hybrid", 0, 1)
	if attempts[0] == attempts[1] {
		t.Fatalf("hybrid 同号次数为 0 时应立即换号: attempts=%v", attempts)
	}
}
