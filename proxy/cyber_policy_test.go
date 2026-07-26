package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// TestUpstreamCyberPolicyCodeDetectsResponseFailed 覆盖 #258：cyber_policy 封禁在
// 流式响应里以 response.failed (HTTP 200) 事件下发，必须能被
// upstreamCyberPolicyCode(responseFailedErrorBody(payload)) 识别。
func TestUpstreamCyberPolicyCodeDetectsResponseFailed(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "response.error.code",
			payload: `{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"blocked"}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "response.status_details.error.code",
			payload: `{"type":"response.failed","response":{"status_details":{"error":{"code":"cyber_policy"}}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "codex_error_info under response.error",
			payload: `{"type":"response.failed","response":{"error":{"codex_error_info":"cyber_policy"}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "substring fallback (cyber security risk)",
			payload: `{"type":"response.failed","response":{"error":{"message":"detected cyber security risk in prompt"}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "unrelated failure is not cyber_policy",
			payload: `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := upstreamCyberPolicyCode(responseFailedErrorBody([]byte(tc.payload)))
			if got != tc.want {
				t.Fatalf("upstreamCyberPolicyCode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCyberPolicyIsTerminalAndDoesNotPenalizeAccount(t *testing.T) {
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example.com",
		APIKey:       "sk-test",
	}
	payload := []byte(`{"type":"response.failed","response":{"error":{"status_code":502,"code":"cyber_policy","message":"cyber security risk detected"}}}`)
	outcome := classifyResponseFailedOutcomeForAccount(account, payload)
	if outcome.failureKind != upstreamErrorKindCyberPolicy || outcome.penalize {
		t.Fatalf("outcome = %+v, want terminal non-penalized cyber_policy", outcome)
	}
	if responseFailedStatusCode([]byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"blocked"}}}`)) != http.StatusForbidden {
		t.Fatal("cyber_policy without status_code must map to HTTP 403")
	}
	if responseFailedRetryable(payload) {
		t.Fatal("cyber_policy response.failed must not be retryable")
	}

	generalRetries, rateLimitRetries := 0, 0
	body := responseFailedErrorBody(payload)
	if shouldRetryHTTPStatusForAccount(account, http.StatusBadGateway, body, &generalRetries, &rateLimitRetries, 5, 5) {
		t.Fatal("cyber_policy HTTP failure must not consume a retry")
	}
	if generalRetries != 0 || rateLimitRetries != 0 {
		t.Fatalf("retry budgets changed: general=%d rate_limit=%d", generalRetries, rateLimitRetries)
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)
	store.SetTransportRetryPolicy("sticky")
	if retry, _, _ := newTransportRetryTracker().shouldRetrySameAccount(handler, account, true, false, upstreamErrorKindCyberPolicy); retry {
		t.Fatal("cyber_policy must bypass sticky same-account retry")
	}
	handler.reportUpstreamAttemptFailure(account, upstreamErrorKindCyberPolicy, time.Millisecond)
	_, _, _, _, _, _, _, failures := account.FailureToleranceSnapshot()
	if failures != 0 {
		t.Fatalf("cyber_policy failure count = %d, want 0", failures)
	}
}

func TestCyberPolicyDownstreamErrorsPreserveCode(t *testing.T) {
	body := []byte(`{"error":{"code":"cyber_policy","message":"blocked"}}`)
	wsErr := responsesWSUpstreamAPIError(http.StatusForbidden, body)
	if string(wsErr.Code) != upstreamErrorKindCyberPolicy || wsErr.Type != "permission_error" {
		t.Fatalf("WebSocket error = %+v", wsErr)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	sendAnthropicCyberPolicyError(ctx, http.StatusForbidden, "blocked")
	if got := gjson.Get(recorder.Body.String(), "error.code").String(); got != upstreamErrorKindCyberPolicy {
		t.Fatalf("Anthropic error code = %q, body=%s", got, recorder.Body.String())
	}

	streamRecorder := httptest.NewRecorder()
	streamCtx, _ := gin.CreateTestContext(streamRecorder)
	if err := writeAnthropicCyberPolicyStreamError(streamCtx, "blocked"); err != nil {
		t.Fatalf("writeAnthropicCyberPolicyStreamError: %v", err)
	}
	if body := streamRecorder.Body.String(); !strings.Contains(body, `event: error`) || !strings.Contains(body, `"code":"cyber_policy"`) {
		t.Fatalf("Anthropic stream error body = %s", body)
	}
}

func TestMessagesCyberPolicyStreamStopsRetryAndReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamAttempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"cyber_policy","message":"blocked by cyber policy"}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.SetMaxRetries(5)
	store.SetTransportRetryPolicy("hybrid")
	store.SetTransportSameAccountRetries(2)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4.1-direct","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.Messages(ctx)

	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want exactly 1", got)
	}
	if body := recorder.Body.String(); recorder.Code != http.StatusOK || !strings.Contains(body, `event: error`) || !strings.Contains(body, `"code":"cyber_policy"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
	account := store.Accounts()[0]
	_, _, _, _, _, _, _, failures := account.FailureToleranceSnapshot()
	if failures != 0 || account.HasActiveCooldown() {
		t.Fatalf("account was penalized: failures=%d cooldown=%v", failures, account.HasActiveCooldown())
	}
}

// TestLogUpstreamCyberPolicyRecordsStreamingFailure 端到端验证：流式 response.failed
// 里的 cyber_policy 会被写入 prompt_filter_logs，且记录完整内容（#258 + #259）。
func TestLogUpstreamCyberPolicyRecordsStreamingFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	defer db.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:               2,
		PromptFilterMode:             promptfilter.ModeBlock,
		PromptFilterThreshold:        50,
		PromptFilterMaxTextLength:    promptfilter.DefaultMaxTextLength,
		PromptFilterCustomPatterns:   "[]",
		PromptFilterDisabledPatterns: "[]",
	})
	handler := NewHandler(store, db, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"cyber security risk detected"}}}`)
	handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.4", responseFailedErrorBody(payload))

	logs, err := db.ListPromptFilterLogs(ctx.Request.Context(), 10)
	if err != nil {
		t.Fatalf("ListPromptFilterLogs error: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("prompt_filter_logs rows = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.Source != "upstream_cyber_policy" {
		t.Fatalf("source = %q, want upstream_cyber_policy", got.Source)
	}
	if got.ErrorCode != "cyber_policy" {
		t.Fatalf("error_code = %q, want cyber_policy", got.ErrorCode)
	}
	if got.Action != string(promptfilter.ActionBlock) {
		t.Fatalf("action = %q, want %q", got.Action, promptfilter.ActionBlock)
	}
	if !strings.Contains(got.FullText, "cyber_policy") {
		t.Fatalf("full_text = %q, want it to contain the upstream error body", got.FullText)
	}
}
