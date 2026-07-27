package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func newClientReplayTestHandler(t *testing.T, maxRetries, keepaliveSeconds int) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                  2,
		TestConcurrency:                 1,
		TestModel:                       "gpt-5.4",
		ClientRequestReplayEnabled:      true,
		ClientRequestReplayMaxRetries:   maxRetries,
		ClientRequestReplayKeepaliveSec: keepaliveSeconds,
	})
	t.Cleanup(store.Stop)
	return NewHandler(store, nil, nil, nil)
}

func newClientReplayTestContext(t *testing.T, stream bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body := fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"input":"hi"}`, stream)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Originator", "codex-tui")
	return c, recorder
}

func writeClientReplaySuccess(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	_, _ = c.Writer.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
	_, _ = c.Writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	c.Writer.Flush()
}

func TestClientRequestReplayRetriesEveryHTTPFailureWithoutClassification(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			handler := newClientReplayTestHandler(t, 1, 0)
			c, recorder := newClientReplayTestContext(t, true)
			attempts := 0

			handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
				attempts++
				if attempts == 1 {
					c.JSON(status, gin.H{"error": gin.H{"message": "first attempt must stay hidden"}})
					return
				}
				writeClientReplaySuccess(c)
			})

			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2", attempts)
			}
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"delta":"ok"`) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "first attempt") {
				t.Fatalf("失败轮响应泄漏给客户端: %s", recorder.Body.String())
			}
		})
	}
}

func TestClientRequestReplayRetriesNonStreamResponseFailed(t *testing.T) {
	handler := newClientReplayTestHandler(t, 1, 0)
	c, recorder := newClientReplayTestContext(t, false)
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
		attempts++
		c.Header("Content-Type", "application/json")
		if attempts == 1 {
			c.Data(http.StatusOK, "application/json", []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_request_error"}}}`))
			return
		}
		c.Data(http.StatusOK, "application/json", []byte(`{"id":"resp_ok","status":"completed"}`))
	})

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"id":"resp_ok","status":"completed"}` {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesClientReplayReentersExistingSelectionAfterResponseFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"status":"failed","error":{"status_code":400,"code":"invalid_request_error","message":"temporary relay failure"}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_replayed"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"recovered"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_replayed","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(0)
	store.SetTransportRetryPolicy("rotate")
	store.SetClientRequestReplayEnabled(true)
	store.SetClientRequestReplayMaxRetries(1)
	store.SetClientRequestReplayKeepaliveSeconds(0)
	accounts := store.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	accounts[0].IgnoreUsageLimit429Cooldown = true
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)
	c, recorder := newClientReplayTestContext(t, true)
	body := []byte(`{"model":"gpt-4.1-direct","stream":true,"input":"hi"}`)
	setRawRequestBody(c, body)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))

	handler.Responses(c)

	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
	responseBody := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(responseBody, "recovered") {
		t.Fatalf("status=%d body=%s", recorder.Code, responseBody)
	}
	if strings.Contains(responseBody, "temporary relay failure") {
		t.Fatalf("首轮 response.failed 泄漏给 Codex: %s", responseBody)
	}
	_, _, _, _, _, _, _, failures := accounts[0].FailureToleranceSnapshot()
	if failures == 0 {
		t.Fatal("被隐藏的失败轮仍应进入账号失败时间窗")
	}
}

func TestChatCompletionsClientReplayReentersAfterHTTPFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamAttempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := upstreamAttempts.Add(1)
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary failure"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_chat_replayed"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"recovered"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(0)
	store.SetTransportRetryPolicy("rotate")
	store.SetClientRequestReplayEnabled(true)
	store.SetClientRequestReplayMaxRetries(1)
	store.SetClientRequestReplayBaseIntervalMS(0)
	store.SetClientRequestReplayKeepaliveSeconds(0)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-4.1-direct","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	setRawRequestBody(c, body)

	handler.ChatCompletions(c)

	if got := upstreamAttempts.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2", got)
	}
	responseBody := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(responseBody, "recovered") || !strings.HasSuffix(responseBody, "data: [DONE]\n\n") {
		t.Fatalf("status=%d body=%s", recorder.Code, responseBody)
	}
	if strings.Contains(responseBody, "temporary failure") {
		t.Fatalf("首轮 Chat 失败泄漏给客户端: %s", responseBody)
	}
}

func TestResponsesCyberPolicyStopsInnerAndWholeRequestReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamAttempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamAttempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"status_code":502,"code":"cyber_policy","message":"cyber security risk detected"}}`)
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                  4,
		MaxRetries:                      5,
		MaxRateLimitRetries:             5,
		ClientRequestReplayEnabled:      true,
		ClientRequestReplayMaxRetries:   5,
		ClientRequestReplayKeepaliveSec: 0,
	})
	store.SetTransportRetryPolicy("hybrid")
	store.SetTransportSameAccountRetries(2)
	accounts := []*auth.Account{
		{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "sk-first", Models: []string{"gpt-5.6-sol"}, PlanType: "api"},
		{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "sk-second", Models: []string{"gpt-5.6-sol"}, PlanType: "api"},
	}
	for _, account := range accounts {
		store.AddAccount(account)
	}
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)
	c, recorder := newClientReplayTestContext(t, true)
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`)
	setRawRequestBody(c, body)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))

	handler.Responses(c)

	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want exactly 1", got)
	}
	if recorder.Code != http.StatusBadGateway || gjson.Get(recorder.Body.String(), "error.code").String() != upstreamErrorKindCyberPolicy {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, account := range accounts {
		_, _, _, _, _, _, _, failures := account.FailureToleranceSnapshot()
		if failures != 0 || account.HasActiveCooldown() {
			t.Fatalf("account %d was penalized: failures=%d cooldown=%v", account.ID(), failures, account.HasActiveCooldown())
		}
	}
}

func TestResponsesCyberPolicyResponseFailedStopsReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamAttempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"status":"failed","error":{"status_code":400,"code":"cyber_policy","message":"blocked by cyber policy"}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.SetMaxRetries(5)
	store.SetMaxRateLimitRetries(5)
	store.SetTransportRetryPolicy("hybrid")
	store.SetTransportSameAccountRetries(2)
	store.SetClientRequestReplayEnabled(true)
	store.SetClientRequestReplayMaxRetries(5)
	store.SetClientRequestReplayKeepaliveSeconds(0)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)
	c, recorder := newClientReplayTestContext(t, true)
	body := []byte(`{"model":"gpt-4.1-direct","stream":true,"input":"hi"}`)
	setRawRequestBody(c, body)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))

	handler.Responses(c)

	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want exactly 1", got)
	}
	if recorder.Code != http.StatusBadRequest || gjson.Get(recorder.Body.String(), "error.code").String() != upstreamErrorKindCyberPolicy {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	account := store.Accounts()[0]
	_, _, _, _, _, _, _, failures := account.FailureToleranceSnapshot()
	if failures != 0 || account.HasActiveCooldown() {
		t.Fatalf("account was penalized: failures=%d cooldown=%v", failures, account.HasActiveCooldown())
	}
}

func TestClientRequestReplayStopsAfterBusinessStreamStarts(t *testing.T) {
	handler := newClientReplayTestHandler(t, 5, 0)
	c, recorder := newClientReplayTestContext(t, true)
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
		attempts++
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
		_, _ = c.Writer.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"))
		c.Writer.Flush()
	})

	if attempts != 1 {
		t.Fatalf("已输出业务流后 attempts = %d, want 1", attempts)
	}
	if !strings.Contains(recorder.Body.String(), "partial") || !strings.Contains(recorder.Body.String(), "response.failed") {
		t.Fatalf("实时流未原样交付: %s", recorder.Body.String())
	}
}

func TestClientRequestReplayRetriesAfterReasoningOnlyFailure(t *testing.T) {
	handler := newClientReplayTestHandler(t, 1, 0)
	c, recorder := newClientReplayTestContext(t, true)
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
		attempts++
		c.Header("Content-Type", "text/event-stream")
		if attempts == 1 {
			for _, event := range []string{
				`{"type":"response.created"}`,
				`{"type":"response.reasoning_summary_text.delta","delta":"private partial reasoning"}`,
				`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error"}}}`,
			} {
				_, _ = c.Writer.Write([]byte("data: "))
				_, _ = c.Writer.Write([]byte(event))
				_, _ = c.Writer.Write([]byte("\n\n"))
			}
			c.Writer.Flush()
			return
		}
		writeClientReplaySuccess(c)
	})

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"delta":"ok"`) || strings.Contains(body, "private partial reasoning") || strings.Contains(body, "server_error") {
		t.Fatalf("仅推理失败轮必须完整隐藏并重试: %s", body)
	}
}

func TestClientRequestReplayHonorsFiniteRetryLimit(t *testing.T) {
	handler := newClientReplayTestHandler(t, 2, 0)
	c, recorder := newClientReplayTestContext(t, true)
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
		attempts++
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"message": fmt.Sprintf("failure-%d", attempts)}})
	})

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "failure-3") {
		t.Fatalf("最终错误不是最后一轮原始响应: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestClientRequestReplayLegacyZeroUsesSafeDefault(t *testing.T) {
	handler := newClientReplayTestHandler(t, 0, 0)
	c, recorder := newClientReplayTestContext(t, true)
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
		attempts++
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "always fails"}})
	})

	wantAttempts := 1 + database.DefaultClientRequestReplayMaxRetries
	if attempts != wantAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, wantAttempts)
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestClientRequestReplayDelayExponentialAndCapped(t *testing.T) {
	base := time.Second
	maxDelay := 30 * time.Second
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for replayIndex, wantDelay := range want {
		if got := clientRequestReplayDelay(base, maxDelay, replayIndex); got != wantDelay {
			t.Fatalf("delay(%d) = %s, want %s", replayIndex, got, wantDelay)
		}
	}
	if got := clientRequestReplayDelay(0, maxDelay, 10); got != 0 {
		t.Fatalf("zero base delay = %s, want 0", got)
	}
}

func TestClientRequestReplayControllerStopsAtDurationBudget(t *testing.T) {
	controller := newClientRequestReplayController(context.Background(), 20*time.Millisecond)
	defer controller.close()

	select {
	case <-controller.context().Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not stop at duration budget")
	}
	if got := controller.reason(); got != clientRequestReplayStopMaxDuration {
		t.Fatalf("stop reason = %q, want %q", got, clientRequestReplayStopMaxDuration)
	}
}

func TestClientRequestReplayControllerDurationStopsAfterBusinessStarts(t *testing.T) {
	controller := newClientRequestReplayController(context.Background(), 20*time.Millisecond)
	defer controller.close()
	controller.markBusinessStarted()
	time.Sleep(40 * time.Millisecond)
	if err := controller.context().Err(); err != nil {
		t.Fatalf("business stream was canceled by replay duration budget: %v", err)
	}
}

func TestClientRequestReplayRealClientDisconnectStopsCurrentAttempt(t *testing.T) {
	handler := newClientReplayTestHandler(t, 5, 0)
	var attempts atomic.Int32
	attemptStarted := make(chan struct{})
	attemptCanceled := make(chan struct{})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
			if attempts.Add(1) == 1 {
				close(attemptStarted)
			}
			<-c.Request.Context().Done()
			close(attemptCanceled)
		})
	})
	server := httptest.NewServer(router)
	defer server.Close()

	clientCtx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(clientCtx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- requestErr
	}()

	select {
	case <-attemptStarted:
	case <-time.After(time.Second):
		t.Fatal("attempt did not start")
	}
	cancel()
	select {
	case <-attemptCanceled:
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel current attempt")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client request did not finish after cancellation")
	}
	time.Sleep(50 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts after client disconnect = %d, want 1", got)
	}
}

func TestClientRequestReplayRealClientDisconnectStopsBackoff(t *testing.T) {
	handler := newClientReplayTestHandler(t, 5, 0)
	handler.store.SetClientRequestReplayBaseIntervalMS(500)
	handler.store.SetClientRequestReplayMaxIntervalSeconds(1)
	var attempts atomic.Int32
	firstAttemptDone := make(chan struct{})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
			if attempts.Add(1) == 1 {
				close(firstAttemptDone)
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "retry later"}})
		})
	})
	server := httptest.NewServer(router)
	defer server.Close()

	clientCtx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(clientCtx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- requestErr
	}()

	select {
	case <-firstAttemptDone:
	case <-time.After(time.Second):
		t.Fatal("first attempt did not finish")
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client request did not finish after cancellation during backoff")
	}
	time.Sleep(550 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts after cancellation during backoff = %d, want 1", got)
	}
}

func TestResponsesClientDisconnectStopsAccountWaitBeforeUpstreamRequest(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		upstreamAttempts.Add(1)
		response.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.SetClientRequestReplayEnabled(true)
	store.SetClientRequestReplayMaxRetries(5)
	store.SetClientRequestReplayKeepaliveSeconds(0)
	first, _ := store.NextForSession("occupied-1", 0, nil)
	second, _ := store.NextForSession("occupied-2", 0, nil)
	if first == nil || second == nil {
		t.Fatal("failed to occupy all account concurrency slots")
	}
	defer store.Release(first)
	defer store.Release(second)
	t.Cleanup(store.Stop)

	handler := NewHandler(store, nil, nil, nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(router)
	defer server.Close()

	clientCtx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(clientCtx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- requestErr
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client request did not leave account wait after cancellation")
	}
	time.Sleep(100 * time.Millisecond)
	if got := upstreamAttempts.Load(); got != 0 {
		t.Fatalf("upstream attempts after account-wait cancellation = %d, want 0", got)
	}
}

func TestResponsesClientDisconnectCancelsRelayAndPreventsNewUpstreamRequest(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		first := upstreamAttempts.Add(1) == 1
		if first {
			close(upstreamStarted)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		response.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
			if first {
				close(upstreamCanceled)
			}
		case <-releaseUpstream:
		}
	}))
	defer close(releaseUpstream)
	defer upstream.Close()

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(0)
	store.SetClientRequestReplayEnabled(true)
	store.SetClientRequestReplayMaxRetries(5)
	store.SetClientRequestReplayBaseIntervalMS(0)
	store.SetClientRequestReplayKeepaliveSeconds(0)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(router)
	defer server.Close()

	clientCtx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(clientCtx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- requestErr
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("relay request did not start")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel relay request before business output")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client request did not finish")
	}
	time.Sleep(100 * time.Millisecond)
	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts after client disconnect = %d, want 1", got)
	}
}

func TestResponsesClientDisconnectDrainsExistingUpstreamAfterBusinessOutput(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstreamCanceled := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		first := upstreamAttempts.Add(1) == 1
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(response, `data: {"type":"response.output_text.delta","delta":"started"}`+"\n\n")
		response.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
			if first {
				close(upstreamCanceled)
			}
		case <-releaseUpstream:
		}
	}))
	defer close(releaseUpstream)
	defer upstream.Close()

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(0)
	store.SetClientRequestReplayEnabled(true)
	store.SetClientRequestReplayMaxRetries(5)
	store.SetClientRequestReplayBaseIntervalMS(0)
	store.SetClientRequestReplayKeepaliveSeconds(0)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	server := httptest.NewServer(router)
	defer server.Close()

	clientCtx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(clientCtx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatalf("read first business event: %v", err)
	}
	cancel()
	_ = response.Body.Close()

	select {
	case <-upstreamCanceled:
		t.Fatal("client disconnect canceled upstream before usage drain window")
	case <-time.After(100 * time.Millisecond):
	}
	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts during drain = %d, want 1", got)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(upstreamDrainTimeout + time.Second):
		t.Fatal("upstream exceeded usage drain timeout after client disconnect")
	}
	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts after drain timeout = %d, want 1", got)
	}
}

func TestClientRequestReplayEmptyInternalResponseBecomesBadGateway(t *testing.T) {
	handler := newClientReplayTestHandler(t, 1, 0)
	c, recorder := newClientReplayTestContext(t, false)
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(*gin.Context) {
		attempts++
	})

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "empty_internal_response") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestClientRequestReplayStopsWhenClientCancels(t *testing.T) {
	handler := newClientReplayTestHandler(t, 0, 0)
	c, recorder := newClientReplayTestContext(t, true)
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
		attempts++
		if attempts == 4 {
			cancel()
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "temporary"}})
	})

	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("客户端取消后不应再提交错误: %s", recorder.Body.String())
	}
}

func TestClientRequestReplayRestoresOriginalBodyAndContext(t *testing.T) {
	handler := newClientReplayTestHandler(t, 1, 0)
	c, recorder := newClientReplayTestContext(t, false)
	original, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(original))
	attempts := 0

	handler.handleWithClientRequestReplay(c, "/v1/responses", func(c *gin.Context) {
		attempts++
		body, ok := rawRequestBodyFromContext(c)
		if !ok || !bytes.Equal(body, original) {
			t.Fatalf("第 %d 轮请求体未恢复: %s", attempts, body)
		}
		if attempts == 1 {
			setRawRequestBody(c, []byte(`{"mutated":true}`))
			c.Set("attempt_only", true)
			c.JSON(http.StatusBadRequest, gin.H{"error": "retry"})
			return
		}
		if _, exists := c.Get("attempt_only"); exists {
			t.Fatal("上一轮 Gin 上下文泄漏到新一轮")
		}
		c.Data(http.StatusOK, "application/json", []byte(`{"status":"completed"}`))
	})

	if recorder.Code != http.StatusOK || attempts != 2 {
		t.Fatalf("status=%d attempts=%d body=%s", recorder.Code, attempts, recorder.Body.String())
	}
}

func TestClientRequestReplayKeepaliveAndFinalSSEFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	delivery := newClientRequestReplayDelivery(c.Writer, nil, clientRequestReplayProtocolResponses, true)

	if err := delivery.writeKeepalive(); err != nil {
		t.Fatalf("writeKeepalive: %v", err)
	}
	delivery.commitSSEFailure(http.StatusForbidden, []byte(`{"error":{"message":"quota temporarily unavailable"}}`), clientRequestReplayStopMaxRetries)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "codex2api.keepalive") {
		t.Fatalf("Codex 保活未写出: status=%d body=%s", recorder.Code, body)
	}
	if !strings.Contains(body, "response.failed") || !strings.Contains(body, "quota temporarily unavailable") {
		t.Fatalf("保活后最终错误未转换为 SSE: %s", body)
	}
}

func TestClientRequestReplayKeepalivePreservesCyberPolicyFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	delivery := newClientRequestReplayDelivery(c.Writer, nil, clientRequestReplayProtocolResponses, true)

	if err := delivery.writeKeepalive(); err != nil {
		t.Fatalf("writeKeepalive: %v", err)
	}
	delivery.commitSSEFailure(http.StatusForbidden, []byte(`{"error":{"code":"cyber_policy","message":"blocked"}}`), clientRequestReplayStopCyberPolicy)

	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"cyber_policy"`) || strings.Contains(body, "client_request_replay_exhausted") {
		t.Fatalf("cyber_policy SSE failure was not preserved: %s", body)
	}
}

func TestGenericClientRequestReplayKeepaliveUsesSSEComment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	delivery := newClientRequestReplayDelivery(c.Writer, nil, clientRequestReplayProtocolResponses, false)

	if err := delivery.writeKeepalive(); err != nil {
		t.Fatalf("writeKeepalive: %v", err)
	}
	if got := recorder.Body.String(); got != string(genericClientReplayKeepalive) {
		t.Fatalf("generic keepalive = %q, want %q", got, genericClientReplayKeepalive)
	}
}

func TestChatClientRequestReplayKeepaliveEndsWithDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	delivery := newClientRequestReplayDelivery(c.Writer, nil, clientRequestReplayProtocolChat, true)

	if err := delivery.writeKeepalive(); err != nil {
		t.Fatalf("writeKeepalive: %v", err)
	}
	delivery.commitSSEFailure(http.StatusServiceUnavailable, []byte(`{"error":{"message":"upstream unavailable"}}`), clientRequestReplayStopMaxRetries)

	body := recorder.Body.String()
	if !strings.HasPrefix(body, string(genericClientReplayKeepalive)) {
		t.Fatalf("Chat 保活必须使用 SSE 注释，不能注入 Responses 事件: %q", body)
	}
	if !strings.Contains(body, `"code":"client_request_replay_exhausted"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("Chat 重发耗尽必须返回结构化错误并以 [DONE] 收尾: %q", body)
	}
}

func TestMessagesClientRequestReplayKeepaliveEndsWithErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	delivery := newClientRequestReplayDelivery(c.Writer, nil, clientRequestReplayProtocolMessages, false)

	if err := delivery.writeKeepalive(); err != nil {
		t.Fatalf("writeKeepalive: %v", err)
	}
	delivery.commitSSEFailure(http.StatusServiceUnavailable, []byte(`{"error":{"message":"upstream unavailable"}}`), clientRequestReplayStopMaxRetries)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error\n") || !strings.Contains(body, `"type":"overloaded_error"`) {
		t.Fatalf("Messages 重发耗尽必须返回 Anthropic error 事件: %q", body)
	}
}

func TestClientRequestReplayStreamDecision(t *testing.T) {
	tests := []struct {
		name     string
		protocol clientRequestReplayProtocol
		payload  string
		commit   bool
		failed   bool
	}{
		{"Responses 推理增量继续缓冲", clientRequestReplayProtocolResponses, `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, false, false},
		{"Responses 推理 item 完成继续缓冲", clientRequestReplayProtocolResponses, `{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"opaque"}}`, false, false},
		{"Responses 答案增量提交", clientRequestReplayProtocolResponses, `{"type":"response.output_text.delta","delta":"answer"}`, true, false},
		{"Responses 失败触发重试", clientRequestReplayProtocolResponses, `{"type":"response.failed","response":{"status":"failed"}}`, false, true},
		{"Chat 推理增量继续缓冲", clientRequestReplayProtocolChat, `{"choices":[{"delta":{"reasoning":"thinking"}}]}`, false, false},
		{"Chat 工具声明继续缓冲", clientRequestReplayProtocolChat, `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"lookup","arguments":""}}]}}]}`, false, false},
		{"Chat 工具参数提交", clientRequestReplayProtocolChat, `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`, true, false},
		{"Chat 答案增量提交", clientRequestReplayProtocolChat, `{"choices":[{"delta":{"content":"answer"}}]}`, true, false},
		{"Chat 错误触发重试", clientRequestReplayProtocolChat, `{"error":{"type":"upstream_error"}}`, false, true},
		{"Messages thinking_delta 继续缓冲", clientRequestReplayProtocolMessages, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`, false, false},
		{"Messages 工具声明继续缓冲", clientRequestReplayProtocolMessages, `{"type":"content_block_start","content_block":{"type":"tool_use","name":"lookup"}}`, false, false},
		{"Messages 工具参数提交", clientRequestReplayProtocolMessages, `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`, true, false},
		{"Messages text_delta 提交", clientRequestReplayProtocolMessages, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}`, true, false},
		{"Messages error 触发重试", clientRequestReplayProtocolMessages, `{"type":"error","error":{"type":"api_error"}}`, false, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commit, failed := clientRequestReplayStreamDecision(test.protocol, []byte(test.payload))
			if commit != test.commit || failed != test.failed {
				t.Fatalf("commit=%v failed=%v, want commit=%v failed=%v", commit, failed, test.commit, test.failed)
			}
		})
	}
}
