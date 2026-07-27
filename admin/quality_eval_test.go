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

func TestQualityEvalSupportedByResponsesAPIAccountRequiresExactModel(t *testing.T) {
	account := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.example.com",
		APIKey:       "api-key",
		Models:       []string{qualityEvalModel},
	}
	if !qualityEvalSupportedByAccount(account) {
		t.Fatal("配置 gpt-5.6-sol 的 Responses API 账号应支持质量检测")
	}
	account.Models = []string{"gpt-5.5"}
	if qualityEvalSupportedByAccount(account) {
		t.Fatal("未配置 gpt-5.6-sol 的 Responses API 账号不应支持质量检测")
	}
	if !qualityEvalSupportedByAccount(&auth.Account{}) {
		t.Fatal("普通 Codex 账号应支持质量检测")
	}
}

func TestExecuteQualityEvalRequestUsesResponsesAPIAccount(t *testing.T) {
	requestPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("请求路径 = %q, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-key" {
			t.Errorf("Authorization = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
		}
		requestPayload <- body
		response := qualityEvalHTTPResponse("21")
		defer response.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{
		DBID:         88,
		Status:       auth.StatusReady,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "api-key",
		Models:       []string{qualityEvalModel},
	}
	store.AddAccount(account)
	handler := &Handler{store: store}
	result, err := handler.executeQualityEvalRequest(context.Background(), account, qualityEvalCandyPrompt)
	if err != nil {
		t.Fatalf("executeQualityEvalRequest 返回错误: %v", err)
	}
	if result.RawAnswer != "21" || result.FirstTokenMs <= 0 {
		t.Fatalf("Responses API 流结果 = %#v", result)
	}
	payload := <-requestPayload
	if gjson.GetBytes(payload, "model").String() != qualityEvalModel ||
		gjson.GetBytes(payload, "reasoning.effort").String() != qualityEvalEffort ||
		!gjson.GetBytes(payload, "stream").Bool() || gjson.GetBytes(payload, "store").Bool() ||
		gjson.GetBytes(payload, "tools").Exists() {
		t.Fatalf("Responses API 质量检测载荷不符合固定约束: %s", payload)
	}
	if account.GetTotalRequests() != 0 || account.GetActiveRequests() != 0 {
		t.Fatalf("Responses API 质量检测污染请求统计: total=%d active=%d", account.GetTotalRequests(), account.GetActiveRequests())
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

func TestQualityEvalRunConfigsUseManualDefaultsAndAutomaticOnePlusOne(t *testing.T) {
	manual, err := manualQualityEvalRunConfig(database.QualityEvalKindFull, qualityEvalRequest{})
	if err != nil {
		t.Fatalf("manualQualityEvalRunConfig 返回错误: %v", err)
	}
	batch := newQualityEvalBatch(1, database.QualityEvalTriggerManual, database.QualityEvalKindFull, nil, manual)
	if batch.Model != "gpt-5.6-sol" || batch.ReasoningEffort != "max" || batch.JuiceRequested != 2 || batch.JuiceConcurrency != 2 || batch.CandyRequested != 3 || batch.CandyConcurrency != 3 {
		t.Fatalf("完整批次 = %#v", batch)
	}
	juice := newQualityEvalBatch(1, database.QualityEvalTriggerManual, database.QualityEvalKindJuice, nil, qualityEvalRunConfigForKind(database.QualityEvalKindJuice, manual))
	if juice.JuiceRequested != 2 || juice.JuiceConcurrency != 2 || juice.CandyRequested != 0 || juice.CandyConcurrency != 0 {
		t.Fatalf("Juice 批次 = %#v", juice)
	}
	candy := newQualityEvalBatch(1, database.QualityEvalTriggerManual, database.QualityEvalKindCandy, nil, qualityEvalRunConfigForKind(database.QualityEvalKindCandy, manual))
	if candy.JuiceRequested != 0 || candy.JuiceConcurrency != 0 || candy.CandyRequested != 3 || candy.CandyConcurrency != 3 {
		t.Fatalf("糖果批次 = %#v", candy)
	}
	automatic := automaticQualityEvalRunConfig()
	if automatic.JuiceSamples != 1 || automatic.JuiceConcurrency != 1 || automatic.CandySamples != 1 || automatic.CandyConcurrency != 1 {
		t.Fatalf("自动批次配置 = %#v, want 1+1", automatic)
	}
	if qualityEvalJuiceMaxAttempts != 4 {
		t.Fatalf("Juice 总尝试数 = %d, want 4", qualityEvalJuiceMaxAttempts)
	}
}

func TestManualQualityEvalRunConfigAcceptsOneThroughTenAndRejectsOutOfRange(t *testing.T) {
	one, ten := 1, 10
	config, err := manualQualityEvalRunConfig(database.QualityEvalKindFull, qualityEvalRequest{
		JuiceSamples: &one, JuiceConcurrency: &ten, CandySamples: &ten, CandyConcurrency: &one,
	})
	if err != nil || config != (qualityEvalRunConfig{JuiceSamples: 1, JuiceConcurrency: 10, CandySamples: 10, CandyConcurrency: 1}) {
		t.Fatalf("边界配置 = %#v, err=%v", config, err)
	}
	for _, invalid := range []int{0, 11} {
		value := invalid
		if _, err := manualQualityEvalRunConfig(database.QualityEvalKindFull, qualityEvalRequest{JuiceSamples: &value}); err == nil {
			t.Fatalf("Juice 样本数 %d 应被拒绝", invalid)
		}
	}

	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"kind":"juice","juice_samples":1.5}`,
		`{"kind":"juice","juice_concurrency":0}`,
		`{"kind":"candy","candy_samples":11}`,
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Params = gin.Params{{Key: "id", Value: "1"}}
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/quality-eval", bytes.NewBufferString(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		new(Handler).RunAccountQualityEval(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("参数 %s 的状态码 = %d, want 400", body, recorder.Code)
		}
	}
}

func TestInterruptedBatchThresholdCoversMaximumManualRuntime(t *testing.T) {
	worstCase := time.Duration(qualityEvalManualMaxValue*(qualityEvalJuiceMaxAttempts+1)) * qualityEvalRequestTimeout
	if qualityEvalInterruptedBatchAfter <= worstCase {
		t.Fatalf("中断批次阈值 = %s, 必须大于手动批次最坏运行时间 %s", qualityEvalInterruptedBatchAfter, worstCase)
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
		{name: "juice partial is diagnostic only", correct: 3, graded: 3, juice: 1, want: database.QualityEvalStatusNormal},
		{name: "juice zero is diagnostic only", correct: 3, graded: 3, juice: 0, want: database.QualityEvalStatusNormal},
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
	if result.RawAnswer != "21" || result.InputTokens != 100 || result.OutputTokens != 20 || result.ReasoningTokens != 15 || result.FirstTokenMs <= 0 || result.DurationMs <= 0 || result.TerminalStatus != "completed" {
		t.Fatalf("流解析结果 = %#v", result)
	}
}

func TestReadQualityEvalStreamTreatsTransportEndAsIncomplete(t *testing.T) {
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n"
	if _, err := readQualityEvalStream(strings.NewReader(stream), time.Now()); err == nil || !strings.Contains(err.Error(), "未收到终止事件") {
		t.Fatalf("提前结束错误 = %v", err)
	}
}

func TestReadQualityEvalStreamRejectsNonCompletedTerminalStatus(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			stream := `data: {"type":"response.completed","response":{"status":"` + status + `"}}` + "\n\n"
			result, err := readQualityEvalStream(strings.NewReader(stream), time.Now())
			if err == nil || result.TerminalStatus != status {
				t.Fatalf("status=%s result=%#v err=%v", status, result, err)
			}
		})
	}
}

func TestCandyTransportFailuresRemainUngradedAndIncomplete(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{DBID: 66, AccessToken: "token", Status: auth.StatusReady, PlanType: "free"}
	store.AddAccount(account)
	for _, tc := range []struct {
		name       string
		execute    func(context.Context, *auth.Account, []byte) (*http.Response, error)
		httpStatus int
	}{
		{
			name: "429",
			execute: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("rate limited")), Header: make(http.Header)}, nil
			},
			httpStatus: http.StatusTooManyRequests,
		},
		{
			name: "transport",
			execute: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &Handler{store: store, qualityEvalExecute: tc.execute}
			sample := handler.runCandyQualityEval(context.Background(), account, 1)
			if sample.Graded || sample.Correct || sample.ErrorMessage == "" || sample.HTTPStatus != tc.httpStatus {
				t.Fatalf("失败样本 = %#v", sample)
			}
			batch := database.QualityEvalBatch{CandyRequested: 1, CandyGraded: 0, CandyCorrect: 0}
			if got := classifyQualityEvalBatch(batch, false); got != database.QualityEvalStatusIncomplete {
				t.Fatalf("失败样本批次状态 = %s, want incomplete", got)
			}
		})
	}
}

func TestScheduledQualityEvalSkipsWhenUsageLogModeIsNotFull(t *testing.T) {
	db := newTestAdminDB(t)
	db.SetUsageLogConfig(database.UsageLogModeErrors, 10, 5)
	if _, err := db.SaveQualityEvalConfig(context.Background(), database.QualityEvalConfig{
		AutoEnabled: true, IntervalMinutes: 60, LookbackHours: 5, TopAccounts: 5, MinRequests: 50, BatchConcurrency: 1,
	}); err != nil {
		t.Fatalf("保存自动检测配置失败: %v", err)
	}
	handler := &Handler{db: db, store: auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})}
	config := handler.qualityEvalConfigResponse(database.DefaultQualityEvalConfig())
	if config.AutoRunnable || !strings.Contains(config.AutoSkipReason, "usage_log_mode=full") {
		t.Fatalf("errors 模式运行态配置 = %#v", config)
	}

	now := time.Date(2026, 7, 27, 4, 0, 0, 0, time.UTC)
	handler.runScheduledQualityEvals(context.Background(), now)
	latest, err := db.GetLatestQualityEvalBatches(context.Background())
	if err != nil || len(latest) != 0 {
		t.Fatalf("errors 模式不应创建检测批次: latest=%#v err=%v", latest, err)
	}
	if claimed, err := db.TryCreateQualityEvalScheduleRun(context.Background(), now); err != nil || claimed {
		t.Fatalf("errors 模式应记录已跳过的调度桶: claimed=%v err=%v", claimed, err)
	}

	db.SetUsageLogConfig(database.UsageLogModeFull, 10, 5)
	if config := handler.qualityEvalConfigResponse(database.DefaultQualityEvalConfig()); !config.AutoRunnable || config.AutoSkipReason != "" {
		t.Fatalf("full 模式运行态配置 = %#v", config)
	}
}

func TestAutomaticQualityEvalExecutesExactlyOneJuiceAndOneCandy(t *testing.T) {
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{DBID: accountID, AccessToken: "token", Status: auth.StatusReady, PlanType: "free"}
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}
	var calls int32
	handler.qualityEvalExecute = func(_ context.Context, _ *auth.Account, payload []byte) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		prompt := gjson.GetBytes(payload, "input.0.content.0.text").String()
		if prompt == qualityEvalJuicePrompt {
			return qualityEvalHTTPResponse("1.2300"), nil
		}
		if prompt == qualityEvalCandyPrompt {
			return qualityEvalHTTPResponse("21"), nil
		}
		t.Fatalf("未知质量检测提示词: %q", prompt)
		return nil, nil
	}

	scheduledHour := time.Date(2026, 7, 27, 4, 0, 0, 0, time.UTC)
	batch := newQualityEvalBatch(accountID, database.QualityEvalTriggerAuto, database.QualityEvalKindFull, &scheduledHour, automaticQualityEvalRunConfig())
	batchID, created, err := db.CreateQualityEvalBatch(context.Background(), batch)
	if err != nil || !created {
		t.Fatalf("创建自动批次失败: id=%d created=%v err=%v", batchID, created, err)
	}
	batch.ID = batchID
	completed := handler.executeQualityEvalBatch(context.Background(), account, batch, nil, nil)
	if calls != 2 || completed.JuiceRequested != 1 || completed.JuiceGraded != 1 || completed.CandyRequested != 1 || completed.CandyGraded != 1 || completed.Status != database.QualityEvalStatusNormal {
		t.Fatalf("自动 1+1 执行结果 = %#v, calls=%d", completed, calls)
	}
	history, err := db.ListAccountQualityEvalBatches(context.Background(), accountID, 20)
	if err != nil || len(history) != 1 || len(history[0].Samples) != 2 || history[0].JuiceConcurrency != 1 || history[0].CandyConcurrency != 1 {
		t.Fatalf("自动 1+1 持久化历史 = %#v, err=%v", history, err)
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
	if strings.Count(recorder.Body.String(), `"type":"quality_eval_task_start"`) != 3 || strings.Count(recorder.Body.String(), `"type":"quality_eval_sample"`) != 3 || !strings.Contains(recorder.Body.String(), `"status":"normal"`) {
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

func TestRunAccountQualityEvalUsesCustomSampleCountAndConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{DBID: accountID, AccessToken: "token", Status: auth.StatusReady, PlanType: "free"}
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}
	var active, peak, calls int32
	firstPairStarted := make(chan struct{})
	var closeOnce sync.Once
	handler.qualityEvalExecute = func(_ context.Context, _ *auth.Account, _ []byte) (*http.Response, error) {
		current := atomic.AddInt32(&active, 1)
		atomic.AddInt32(&calls, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if current <= old || atomic.CompareAndSwapInt32(&peak, old, current) {
				break
			}
		}
		if current == 2 {
			closeOnce.Do(func() { close(firstPairStarted) })
		}
		select {
		case <-firstPairStarted:
		case <-time.After(2 * time.Second):
			return nil, context.DeadlineExceeded
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return qualityEvalHTTPResponse("21"), nil
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(accountID, 10)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/quality-eval", bytes.NewBufferString(`{"kind":"candy","juice_samples":7,"juice_concurrency":6,"candy_samples":4,"candy_concurrency":2}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.RunAccountQualityEval(ctx)

	if recorder.Code != http.StatusOK || calls != 4 || peak != 2 {
		t.Fatalf("自定义 SSE status/calls/peak = %d/%d/%d, body=%s", recorder.Code, calls, peak, recorder.Body.String())
	}
	history, err := db.ListAccountQualityEvalBatches(context.Background(), accountID, 20)
	if err != nil || len(history) != 1 || history[0].JuiceRequested != 0 || history[0].JuiceConcurrency != 0 || history[0].CandyRequested != 4 || history[0].CandyConcurrency != 2 || history[0].CandyCorrect != 4 {
		t.Fatalf("自定义批次持久化 = %#v, err=%v", history, err)
	}
}

func TestRunAccountQualityEvalKeepsAccountLockUntilWorkerStopsAfterDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{DBID: accountID, AccessToken: "token", Status: auth.StatusReady, PlanType: "free"}
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}
	requestStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	var startOnce sync.Once
	handler.qualityEvalExecute = func(ctx context.Context, _ *auth.Account, _ []byte) (*http.Response, error) {
		startOnce.Do(func() { close(requestStarted) })
		<-ctx.Done()
		<-releaseWorker
		return nil, ctx.Err()
	}

	requestContext, cancelRequest := context.WithCancel(context.Background())
	firstRecorder := httptest.NewRecorder()
	firstContext, _ := gin.CreateTestContext(firstRecorder)
	firstContext.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(accountID, 10)}}
	firstContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/quality-eval", bytes.NewBufferString(`{"kind":"candy"}`)).WithContext(requestContext)
	firstContext.Request.Header.Set("Content-Type", "application/json")
	firstHandlerDone := make(chan struct{})
	go func() {
		handler.RunAccountQualityEval(firstContext)
		close(firstHandlerDone)
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("首个质量检测请求未启动")
	}
	cancelRequest()
	select {
	case <-firstHandlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("客户端断流后 HTTP handler 未退出")
	}
	if _, locked := handler.qualityEvalAccountLocks.Load(accountID); !locked {
		t.Fatal("后台执行器尚未结束时账号锁被提前释放")
	}

	secondRecorder := httptest.NewRecorder()
	secondContext, _ := gin.CreateTestContext(secondRecorder)
	secondContext.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(accountID, 10)}}
	secondContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/quality-eval", bytes.NewBufferString(`{"kind":"candy"}`))
	secondContext.Request.Header.Set("Content-Type", "application/json")
	handler.RunAccountQualityEval(secondContext)
	if secondRecorder.Code != http.StatusConflict {
		t.Fatalf("后台执行期间第二次检测状态码 = %d, want 409", secondRecorder.Code)
	}

	close(releaseWorker)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, locked := handler.qualityEvalAccountLocks.Load(accountID); !locked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("后台执行器结束后账号锁未释放")
}

func TestQualityEvalConcurrencySharesAccountCapacityWithNormalLoad(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: qualityEvalModel})
	account := &auth.Account{DBID: 77, AccessToken: "token", Status: auth.StatusReady, PlanType: "free"}
	store.AddAccount(account)
	atomic.StoreInt64(&account.ActiveRequests, 1) // 模拟一个已在执行的正常请求。

	handler := &Handler{store: store}
	var activeProbes int32
	var peakProbes int32
	var peakAccountLoad int64
	handler.qualityEvalExecute = func(_ context.Context, currentAccount *auth.Account, _ []byte) (*http.Response, error) {
		probeCount := atomic.AddInt32(&activeProbes, 1)
		for {
			old := atomic.LoadInt32(&peakProbes)
			if probeCount <= old || atomic.CompareAndSwapInt32(&peakProbes, old, probeCount) {
				break
			}
		}
		accountLoad := currentAccount.GetActiveRequests()
		for {
			old := atomic.LoadInt64(&peakAccountLoad)
			if accountLoad <= old || atomic.CompareAndSwapInt64(&peakAccountLoad, old, accountLoad) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&activeProbes, -1)
		return qualityEvalHTTPResponse("答案是 21。"), nil
	}

	samples := handler.runQualityEvalSamples(context.Background(), account, database.QualityEvalKindCandy, 4, 4, nil)
	if len(samples) != 4 {
		t.Fatalf("样本数 = %d, want 4", len(samples))
	}
	if got := atomic.LoadInt32(&peakProbes); got != 1 {
		t.Fatalf("已有 1 个正常请求且账号上限为 2 时，探测峰值 = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&peakAccountLoad); got != 2 {
		t.Fatalf("账号总负载峰值 = %d, want 2", got)
	}
	if got := account.GetActiveRequests(); got != 1 {
		t.Fatalf("检测结束后正常请求槽应保留，active = %d, want 1", got)
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
