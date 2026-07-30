package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexTruncation(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantExists bool
		wantValue  string
	}{
		{name: "disabled is omitted", input: `{"truncation":"disabled"}`, wantExists: false},
		{name: "case and whitespace disabled are omitted", input: `{"truncation":" DISABLED "}`, wantExists: false},
		{name: "auto remains explicit", input: `{"truncation":"auto"}`, wantExists: true, wantValue: "auto"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{}
			if err := json.Unmarshal([]byte(tc.input), &body); err != nil {
				t.Fatal(err)
			}
			normalizeCodexTruncation(body)
			value, exists := body["truncation"]
			if exists != tc.wantExists {
				t.Fatalf("truncation exists = %v, want %v; body=%v", exists, tc.wantExists, body)
			}
			if tc.wantExists && value != tc.wantValue {
				t.Fatalf("truncation = %#v, want %q", value, tc.wantValue)
			}
		})
	}
}

func TestPrepareResponsesBodiesKeepCodexAutoTruncationMarker(t *testing.T) {
	for _, truncation := range []string{"disabled", "auto"} {
		raw := []byte(`{"model":"gpt-4.1-direct","input":"hello","truncation":"` + truncation + `"}`)
		codexBody, _ := PrepareResponsesBody(raw)
		codexTruncation := gjson.GetBytes(codexBody, "truncation")
		if truncation == "disabled" {
			if codexTruncation.Exists() {
				t.Fatalf("Codex body should omit truncation=disabled: %s", codexBody)
			}
		} else if codexTruncation.String() != truncation {
			t.Fatalf("Codex body must keep truncation=%q as a routing guard: %s", truncation, codexBody)
		}
		relayBody := PrepareOpenAIResponsesBody(raw)
		if got := gjson.GetBytes(relayBody, "truncation").String(); got != truncation {
			t.Fatalf("relay truncation = %q, want %q; body=%s", got, truncation, relayBody)
		}
	}
}

func TestUnsupportedTruncationErrorShapes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "top-level unsupported parameter",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"unsupported_parameter","param":"truncation","message":"Unsupported parameter: truncation"}}`,
			want:   true,
		},
		{
			name:   "nested unknown parameter",
			status: http.StatusBadRequest,
			body:   `{"response":{"error":{"type":"unknown_parameter","param":"truncation","message":"Unknown parameter truncation"}}}`,
			want:   true,
		},
		{
			name:   "ordinary invalid request",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"invalid_request_error","message":"bad input"}}`,
			want:   false,
		},
		{
			name:   "same body with server status",
			status: http.StatusBadGateway,
			body:   `{"error":{"code":"unsupported_parameter","param":"truncation"}}`,
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnsupportedTruncationError(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("isUnsupportedTruncationError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnsupportedTruncationResponseFailedIsDeterministic(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"unsupported_parameter","param":"truncation","message":"Unsupported parameter: truncation"}}}`)
	if !isUnsupportedTruncationPayload(payload) {
		t.Fatal("response.failed truncation rejection was not recognized")
	}
	if responseFailedRetryable(payload) {
		t.Fatal("unsupported truncation response.failed must not be retryable")
	}
	outcome := markUnsupportedTruncationOutcome(classifyResponseFailedOutcome(payload))
	if outcome.logStatusCode != http.StatusBadRequest || outcome.failureKind != upstreamErrorKindUnsupportedTruncation || outcome.penalize {
		t.Fatalf("outcome = %+v, want deterministic non-penalized 400", outcome)
	}
	tracker := newTransportRetryTracker()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	handler := NewHandler(store, nil, nil, nil)
	account := &auth.Account{DBID: 1, AccessToken: "token", PlanType: "pro"}
	if retry, _, _ := tracker.shouldRetryForRequest(handler, account, false, true, false, upstreamErrorKindUnsupportedTruncation); retry {
		t.Fatal("unsupported truncation must not receive same-account retry")
	}
}

type truncationTestJSON = map[string]any

func TestRouteResponsesByTruncationCapabilityRequiresRelay(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	codexAccount := &auth.Account{DBID: 1, AccessToken: "token", Models: []string{"gpt-5.4"}}
	store.AddAccount(codexAccount)
	if _, err := routeResponsesByTruncationCapability([]byte(`{"truncation":"auto"}`), store, nil); err == nil {
		t.Fatal("auto should fail when only Codex accounts are available")
	}

	relayAccount := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk-test", Models: []string{"gpt-5.4"}}
	store.AddAccount(relayAccount)
	filter, err := routeResponsesByTruncationCapability([]byte(`{"truncation":"auto"}`), store, nil)
	if err != nil || filter == nil {
		t.Fatalf("auto should route to relay: filter=%v err=%v", filter, err)
	}
	if !filter(relayAccount) || filter(codexAccount) {
		t.Fatal("truncation auto filter must accept relay and reject Codex")
	}
}

func TestSendUnsupportedTruncationRequestErrorBlocksReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	sendUnsupportedTruncationRequestError(ctx, nil)
	if recorder.Code != http.StatusBadRequest || gjson.GetBytes(recorder.Body.Bytes(), "error.code").String() != upstreamErrorKindUnsupportedTruncation {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := clientRequestReplayBlockReason(ctx); got != clientRequestReplayStopUnsupportedTruncation {
		t.Fatalf("replay block reason = %q, want %q", got, clientRequestReplayStopUnsupportedTruncation)
	}
}
