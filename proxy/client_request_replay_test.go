package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
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

func TestClientRequestReplayUnlimitedStopsWhenClientCancels(t *testing.T) {
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
	delivery := newClientRequestReplayDelivery(c.Writer, true)

	if err := delivery.writeKeepalive(); err != nil {
		t.Fatalf("writeKeepalive: %v", err)
	}
	delivery.commitSSEFailure(http.StatusForbidden, []byte(`{"error":{"message":"quota temporarily unavailable"}}`))

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "codex2api.keepalive") {
		t.Fatalf("Codex 保活未写出: status=%d body=%s", recorder.Code, body)
	}
	if !strings.Contains(body, "response.failed") || !strings.Contains(body, "quota temporarily unavailable") {
		t.Fatalf("保活后最终错误未转换为 SSE: %s", body)
	}
}

func TestGenericClientRequestReplayKeepaliveUsesSSEComment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	delivery := newClientRequestReplayDelivery(c.Writer, false)

	if err := delivery.writeKeepalive(); err != nil {
		t.Fatalf("writeKeepalive: %v", err)
	}
	if got := recorder.Body.String(); got != string(genericClientReplayKeepalive) {
		t.Fatalf("generic keepalive = %q, want %q", got, genericClientReplayKeepalive)
	}
}
