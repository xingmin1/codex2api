package admin

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func qualityEvalHTTPResponse(answer string) *http.Response {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":` + strconv.Quote(answer) + `}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}}}`,
		"",
	}, "\n\n")
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}
}

func TestBuildQualityEvalPayloadIsPinnedAndToolFree(t *testing.T) {
	payload, err := buildQualityEvalPayload("prompt")
	if err != nil {
		t.Fatalf("buildQualityEvalPayload 返回错误: %v", err)
	}
	if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", got)
	}
	if got := gjson.GetBytes(payload, "reasoning.effort").String(); got != "max" {
		t.Fatalf("reasoning.effort = %q, want max", got)
	}
	if !gjson.GetBytes(payload, "stream").Bool() || gjson.GetBytes(payload, "store").Bool() {
		t.Fatalf("stream/store 固定项错误: %s", payload)
	}
	if gjson.GetBytes(payload, "tools").Exists() || gjson.GetBytes(payload, "tool_choice").Exists() {
		t.Fatalf("质量检测不得提供工具: %s", payload)
	}
	if got := gjson.GetBytes(payload, "input.0.content.0.text").String(); got != "prompt" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestQualityEvalReferenceMatchers(t *testing.T) {
	juiceCases := map[string]string{
		"1.2300":        "1.2300",
		"value=-.25e+3": "-.25e+3",
		"no juice":      "",
	}
	for input, want := range juiceCases {
		if got := qualityEvalJuiceNumberPattern.FindString(input); got != want {
			t.Errorf("Juice %q = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"21", "答案是 21。", "(21)"} {
		if !qualityEvalCandyAnswerPattern.MatchString(input) {
			t.Errorf("糖果答案 %q 应判为正确", input)
		}
	}
	for _, input := range []string{"121", "210", "2.1", "二十一"} {
		if qualityEvalCandyAnswerPattern.MatchString(input) {
			t.Errorf("糖果答案 %q 不应判为正确", input)
		}
	}
}

func TestNewQualityEvalBatchUsesApprovedSampleCounts(t *testing.T) {
	batch := newQualityEvalBatch(1, database.QualityEvalTriggerManual, database.QualityEvalKindFull, nil)
	if batch.Model != "gpt-5.6-sol" || batch.ReasoningEffort != "max" || batch.JuiceRequested != 2 || batch.CandyRequested != 3 {
		t.Fatalf("完整批次 = %#v", batch)
	}
	juice := newQualityEvalBatch(1, database.QualityEvalTriggerManual, database.QualityEvalKindJuice, nil)
	if juice.JuiceRequested != 2 || juice.CandyRequested != 0 {
		t.Fatalf("Juice 批次 = %#v", juice)
	}
	candy := newQualityEvalBatch(1, database.QualityEvalTriggerManual, database.QualityEvalKindCandy, nil)
	if candy.JuiceRequested != 0 || candy.CandyRequested != 3 {
		t.Fatalf("糖果批次 = %#v", candy)
	}
	if qualityEvalJuiceConcurrency != 2 || qualityEvalCandyConcurrency != 3 || qualityEvalJuiceMaxAttempts != 4 {
		t.Fatalf("并发/重试常量 = %d/%d/%d", qualityEvalJuiceConcurrency, qualityEvalCandyConcurrency, qualityEvalJuiceMaxAttempts)
	}
}

func TestClassifyQualityEvalBatchRequiresPerfectCandyAccuracy(t *testing.T) {
	base := database.QualityEvalBatch{JuiceRequested: 2, JuiceGraded: 2, JuiceCorrect: 2, CandyRequested: 3, CandyGraded: 3}
	cases := []struct {
		name    string
		correct int
		graded  int
		juice   int
		persist bool
		want    string
	}{
		{name: "perfect", correct: 3, graded: 3, juice: 2, want: database.QualityEvalStatusNormal},
		{name: "candy partial", correct: 2, graded: 3, juice: 2, want: database.QualityEvalStatusSuspected},
		{name: "candy zero", correct: 0, graded: 3, juice: 2, want: database.QualityEvalStatusDegraded},
		{name: "juice partial", correct: 3, graded: 3, juice: 1, want: database.QualityEvalStatusSuspected},
		{name: "juice zero", correct: 3, graded: 3, juice: 0, want: database.QualityEvalStatusDegraded},
		{name: "transport incomplete", correct: 2, graded: 2, juice: 2, want: database.QualityEvalStatusIncomplete},
		{name: "persistence incomplete", correct: 3, graded: 3, juice: 2, persist: true, want: database.QualityEvalStatusIncomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batch := base
			batch.CandyCorrect = tc.correct
			batch.CandyGraded = tc.graded
			batch.JuiceCorrect = tc.juice
			if got := classifyQualityEvalBatch(batch, tc.persist); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadQualityEvalStreamExtractsAnswerUsageAndTiming(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"2"}`,
		`data: {"type":"response.output_text.delta","delta":"1"}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":100,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":15}}}}`,
		"",
	}, "\n\n")
	result, err := readQualityEvalStream(strings.NewReader(stream), time.Now().Add(-50*time.Millisecond))
	if err != nil {
		t.Fatalf("readQualityEvalStream 返回错误: %v", err)
	}
	if result.RawAnswer != "21" || result.InputTokens != 100 || result.OutputTokens != 20 || result.ReasoningTokens != 15 || result.FirstTokenMs <= 0 || result.DurationMs <= 0 {
		t.Fatalf("流解析结果 = %#v", result)
	}
}

func TestReadQualityEvalStreamTreatsTransportEndAsIncomplete(t *testing.T) {
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n"
	if _, err := readQualityEvalStream(strings.NewReader(stream), time.Now()); err == nil || !strings.Contains(err.Error(), "未收到终止事件") {
		t.Fatalf("提前结束错误 = %v", err)
	}
}

func TestRunAccountQualityEvalCandyUsesThreeWorkersAndPersistsNormalBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 3, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{DBID: accountID, AccessToken: "token", Status: auth.StatusReady, PlanType: "free"}
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}
	var active int32
	var peak int32
	var calls int32
	allStarted := make(chan struct{})
	var closeOnce sync.Once
	var payloadsMu sync.Mutex
	var payloads [][]byte
	handler.qualityEvalExecute = func(_ context.Context, _ *auth.Account, payload []byte) (*http.Response, error) {
		payloadsMu.Lock()
		payloads = append(payloads, append([]byte(nil), payload...))
		payloadsMu.Unlock()
		current := atomic.AddInt32(&active, 1)
		atomic.AddInt32(&calls, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if current <= old || atomic.CompareAndSwapInt32(&peak, old, current) {
				break
			}
		}
		if current == 3 {
			closeOnce.Do(func() { close(allStarted) })
		}
		select {
		case <-allStarted:
		case <-time.After(2 * time.Second):
			return nil, context.DeadlineExceeded
		}
		atomic.AddInt32(&active, -1)
		return qualityEvalHTTPResponse("答案是 21。"), nil
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(accountID, 10)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/quality-eval", bytes.NewBufferString(`{"kind":"candy"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.RunAccountQualityEval(ctx)

	if recorder.Code != http.StatusOK || atomic.LoadInt32(&calls) != 3 || atomic.LoadInt32(&peak) != 3 {
		t.Fatalf("SSE status/calls/peak = %d/%d/%d, body=%s", recorder.Code, calls, peak, recorder.Body.String())
	}
	if strings.Count(recorder.Body.String(), `"type":"quality_eval_sample"`) != 3 || !strings.Contains(recorder.Body.String(), `"status":"normal"`) {
		t.Fatalf("SSE 事件不完整: %s", recorder.Body.String())
	}
	for _, payload := range payloads {
		if gjson.GetBytes(payload, "model").String() != qualityEvalModel || gjson.GetBytes(payload, "reasoning.effort").String() != qualityEvalEffort || gjson.GetBytes(payload, "tools").Exists() {
			t.Fatalf("并发请求未固定模型/effort/工具: %s", payload)
		}
	}
	history, err := db.ListAccountQualityEvalBatches(context.Background(), accountID, 20)
	if err != nil || len(history) != 1 || history[0].Status != database.QualityEvalStatusNormal || history[0].CandyCorrect != 3 {
		t.Fatalf("持久化历史 = %#v, err=%v", history, err)
	}
	if account.GetTotalRequests() != 0 || account.GetActiveRequests() != 0 {
		t.Fatalf("质量检测污染账号请求统计: total=%d active=%d", account.GetTotalRequests(), account.GetActiveRequests())
	}
}

func TestRunJuiceQualityEvalRetriesOnlyMissingNumbers(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{DBID: 99, AccessToken: "token", Status: auth.StatusReady, PlanType: "free"}
	store.AddAccount(account)
	handler := &Handler{store: store}
	var calls int
	handler.qualityEvalExecute = func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		calls++
		if calls < 4 {
			return qualityEvalHTTPResponse("no juice"), nil
		}
		return qualityEvalHTTPResponse("1.2300"), nil
	}
	sample := handler.runJuiceQualityEval(context.Background(), account, 1)
	if calls != 4 || sample.AttemptCount != 4 || len(sample.AttemptAnswers) != 4 || sample.ParsedAnswer != "1.2300" || !sample.Correct || !sample.Graded {
		t.Fatalf("Juice 重试样本 = %#v, calls=%d", sample, calls)
	}
}
