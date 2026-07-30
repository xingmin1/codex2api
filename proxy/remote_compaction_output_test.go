package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
)

func TestRemoteCompactionV2OutputTracker(t *testing.T) {
	tests := []struct {
		name       string
		events     []string
		wantErr    bool
		wantDetail string
	}{
		{
			name:       "zero output items",
			events:     []string{`{"type":"response.completed","response":{"output":[]}}`},
			wantErr:    true,
			wantDetail: "got 0 from 0 output items",
		},
		{
			name: "one compaction output item",
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"ok"}}`,
				`{"type":"response.completed","response":{}}`,
			},
		},
		{
			name: "multiple output items before completed",
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"message"}}`,
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"ok"}}`,
				`{"type":"response.completed","response":{}}`,
			},
			wantErr:    true,
			wantDetail: "got 1 from 2 output items",
		},
		{
			name: "multiple compaction items",
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"one"}}`,
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"two"}}`,
				`{"type":"response.completed","response":{}}`,
			},
			wantErr:    true,
			wantDetail: "got 2 from 2 output items",
		},
		{
			name: "missing encrypted content",
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"compaction"}}`,
				`{"type":"response.completed","response":{}}`,
			},
			wantErr:    true,
			wantDetail: "missing non-empty encrypted_content",
		},
		{
			name: "completed status is not completed",
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"ok"}}`,
				`{"type":"response.completed","response":{"status":"incomplete"}}`,
			},
			wantErr:    true,
			wantDetail: `terminal status "incomplete"`,
		},
		{
			name: "completed output must agree with done item",
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"done"}}`,
				`{"type":"response.completed","response":{"output":[{"type":"compaction","encrypted_content":"different"}]}}`,
			},
			wantErr:    true,
			wantDetail: "output_item.done and response.completed output disagree",
		},
		{
			name: "events after completed are rejected",
			events: []string{
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"ok"}}`,
				`{"type":"response.completed","response":{}}`,
				`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"late"}}`,
			},
			wantErr:    true,
			wantDetail: `terminal status "event_after_terminal"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := &remoteCompactionV2OutputTracker{}
			var gotErr error
			for _, raw := range tt.events {
				if err := tracker.observeSSEEvent(gjson.Parse(raw)); err != nil {
					gotErr = err
					break
				}
			}
			if (gotErr != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %t", gotErr, tt.wantErr)
			}
			if tt.wantDetail != "" && !strings.Contains(gotErr.Error(), tt.wantDetail) {
				t.Fatalf("error = %q, want detail %q", gotErr, tt.wantDetail)
			}
		})
	}
}

func TestValidateRemoteCompactionV2ResponseJSON(t *testing.T) {
	if err := validateRemoteCompactionV2ResponseJSON([]byte(`{"output":[{"type":"compaction","encrypted_content":"ok"}]}`)); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if err := validateRemoteCompactionV2ResponseJSON([]byte(`{"output":[]}`)); err == nil || !strings.Contains(err.Error(), "got 0 from 0 output items") {
		t.Fatalf("empty response error = %v", err)
	}
	if err := validateRemoteCompactionV2ResponseJSON([]byte(`{"output":[{"type":"compaction","encrypted_content":" "}]}`)); err == nil || !strings.Contains(err.Error(), "missing non-empty encrypted_content") {
		t.Fatalf("blank encrypted content error = %v", err)
	}
}

func TestResponsesRelayRetriesInvalidNativeCompactionOnAnotherAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	badCalls := &atomic.Int32{}
	badUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"type":"response.created","response":{"id":"resp_bad"}}`+"\n\n"+
				`data: {"type":"response.completed","response":{"id":"resp_bad","status":"completed","output":[],"usage":{"input_tokens":30,"output_tokens":8,"total_tokens":38}}}`+"\n\n",
		)
	}))
	defer badUpstream.Close()

	goodCalls := &atomic.Int32{}
	goodUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"type":"response.created","response":{"id":"resp_good"}}`+"\n\n"+
				`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"accepted"}}`+"\n\n"+
				`data: {"type":"response.completed","response":{"id":"resp_good","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n",
		)
	}))
	defer goodUpstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	badAccount := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: badUpstream.URL, APIKey: "bad", Models: []string{"gpt-5.4"}}
	goodAccount := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: goodUpstream.URL, APIKey: "good", Models: []string{"gpt-5.4"}}
	store.AddAccount(badAccount)
	store.AddAccount(goodAccount)

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	const sessionID = "native-compact-relay"
	store.BindSessionAffinity(sessionAffinityKey(sessionID, 0), badAccount, "")

	doRequest := func() *httptest.ResponseRecorder {
		body := []byte(`{"model":"gpt-5.4","stream":true,"input":[{"role":"user","content":"compact"},{"type":"compaction_trigger"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session_id", sessionID)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = req
		handler.Responses(ctx)
		return recorder
	}

	first := doRequest()
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	if strings.Contains(first.Body.String(), "resp_bad") || !strings.Contains(first.Body.String(), "accepted") {
		t.Fatalf("invalid attempt leaked or valid compaction missing: %s", first.Body.String())
	}
	if badCalls.Load() != 1 || goodCalls.Load() != 1 {
		t.Fatalf("upstream calls bad=%d good=%d, want 1/1", badCalls.Load(), goodCalls.Load())
	}

	store.BindSessionAffinity(sessionAffinityKey(sessionID, 0), badAccount, "")
	second := doRequest()
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "accepted") {
		t.Fatalf("second response status=%d body=%s", second.Code, second.Body.String())
	}
	if badCalls.Load() != 1 {
		t.Fatalf("temporarily unsupported relay was called again, calls=%d", badCalls.Load())
	}
}

func TestResponsesCodexRetriesInvalidNativeCompactionOnAnotherAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	attempts := make(chan int64, 2)
	WebsocketExecuteFunc = func(_ context.Context, account *auth.Account, _ []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		attempts <- account.ID()
		if account.ID() == 1 {
			return sseTestResponse(
				`data: {"type":"response.created","response":{"id":"resp_bad"}}` + "\n\n" +
					`data: {"type":"response.completed","response":{"id":"resp_bad","status":"completed","output":[]}}` + "\n\n",
			), nil
		}
		return sseTestResponse(
			`data: {"type":"response.created","response":{"id":"resp_good"}}` + "\n\n" +
				`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"accepted"}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_good","status":"completed"}}` + "\n\n",
		), nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	badAccount := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	goodAccount := &auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"}
	store.AddAccount(badAccount)
	store.AddAccount(goodAccount)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	const sessionID = "native-compact-codex-http"
	store.BindSessionAffinity(sessionAffinityKey(sessionID, 0), badAccount, "")
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":[{"role":"user","content":"compact"},{"type":"compaction_trigger"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", sessionID)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "resp_bad") || !strings.Contains(recorder.Body.String(), "accepted") {
		t.Fatalf("invalid attempt leaked or valid compaction missing: %s", recorder.Body.String())
	}
	firstAttempt := <-attempts
	secondAttempt := <-attempts
	if firstAttempt != 1 || secondAttempt != 2 {
		t.Fatalf("attempt accounts = %d,%d, want 1,2", firstAttempt, secondAttempt)
	}
}

func TestResponsesNativeCompactionSemanticRetryIsBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	attempts := &atomic.Int32{}
	WebsocketExecuteFunc = func(_ context.Context, _ *auth.Account, _ []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		attempts.Add(1)
		return sseTestResponse(
			`data: {"type":"response.completed","response":{"id":"resp_bad","status":"completed","output":[]}}` + "\n\n",
		), nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 3, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	firstAccount := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	store.AddAccount(firstAccount)
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	store.AddAccount(&auth.Account{DBID: 3, AccessToken: "at-3", PlanType: "pro", AccountID: "acct-3"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	const sessionID = "native-compact-bounded"
	store.BindSessionAffinity(sessionAffinityKey(sessionID, 0), firstAccount, "")
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":[{"type":"compaction_trigger"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", sessionID)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), upstreamErrorKindInvalidCompactionOutput) {
		t.Fatalf("missing semantic error code: %s", recorder.Body.String())
	}
	wantAttempts := int32(nativeCompactionSemanticRetryLimit + 1)
	if attempts.Load() != wantAttempts {
		t.Fatalf("upstream attempts = %d, want %d", attempts.Load(), wantAttempts)
	}
}

func TestResponsesWebSocketRetriesInvalidNativeCompactionWithSilentRetryDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = false
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 0
	ApplyRuntimeSettings(nextSettings)

	attempts := make(chan int64, 2)
	WebsocketExecuteFunc = func(_ context.Context, account *auth.Account, _ []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		attempts <- account.ID()
		if account.ID() == 1 {
			return sseTestResponse(
				`data: {"type":"response.created","response":{"id":"resp_bad"}}` + "\n\n" +
					`data: {"type":"response.completed","response":{"id":"resp_bad","status":"completed","output":[]}}` + "\n\n",
			), nil
		}
		return sseTestResponse(
			`data: {"type":"response.created","response":{"id":"resp_good"}}` + "\n\n" +
				`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"accepted"}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_good","status":"completed"}}` + "\n\n",
		), nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	badAccount := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	goodAccount := &auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"}
	store.AddAccount(badAccount)
	store.AddAccount(goodAccount)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	const sessionID = "native-compact-ws"
	store.BindSessionAffinity(sessionAffinityKey(sessionID, 0), badAccount, "")
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	headers := http.Header{}
	headers.Set("Session_id", sessionID)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	request := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"compact"},{"type":"compaction_trigger"}]}`)
	if err := conn.WriteMessage(websocket.TextMessage, request); err != nil {
		t.Fatalf("write compact request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	seenBad := false
	seenCompaction := false
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read compact response: %v", err)
		}
		seenBad = seenBad || strings.Contains(string(message), "resp_bad")
		seenCompaction = seenCompaction || gjson.GetBytes(message, "item.type").String() == "compaction"
		if gjson.GetBytes(message, "type").String() == "response.completed" {
			break
		}
	}
	if seenBad || !seenCompaction {
		t.Fatalf("invalid frames leaked=%t, valid compaction seen=%t", seenBad, seenCompaction)
	}
	firstAttempt := <-attempts
	secondAttempt := <-attempts
	if firstAttempt != 1 || secondAttempt != 2 {
		t.Fatalf("attempt accounts = %d,%d, want 1,2", firstAttempt, secondAttempt)
	}
}

func sseTestResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
