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

// 传输错误 + sticky 策略:同号重试(不换号、保留会话亲和)。issue #331
func TestResponsesWebSocketTransportRetrySticky(t *testing.T) {
	attempts := runWSTransportRetryScenario(t, "sticky", 0, 1)
	if attempts[0] != attempts[1] {
		t.Fatalf("sticky 策略应同号重试: attempts=%v", attempts)
	}
}

// 传输错误 + rotate 策略(默认):换号重试,保持旧行为。
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
