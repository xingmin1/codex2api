package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	qualityEvalModel                  = "gpt-5.6-sol"
	qualityEvalEffort                 = "max"
	qualityEvalManualJuiceSamples     = 2
	qualityEvalManualCandySamples     = 3
	qualityEvalManualJuiceConcurrency = 2
	qualityEvalManualCandyConcurrency = 3
	qualityEvalAutoSamples            = 1
	qualityEvalAutoConcurrency        = 1
	qualityEvalManualMaxValue         = 10
	qualityEvalJuiceMaxAttempts       = 4
	qualityEvalHistoryLimit           = 20
	qualityEvalHistoryRetention       = 30 * 24 * time.Hour
	qualityEvalRequestTimeout         = 5 * time.Minute
	qualityEvalSSEKeepalive           = 15 * time.Second
	qualityEvalSchedulerLease         = 5 * time.Minute
	qualityEvalLeaseRenewal           = time.Minute
	qualityEvalDBFinalizeTimeout      = 5 * time.Second
	qualityEvalInterruptedBatchAfter  = time.Duration(qualityEvalManualMaxValue*(qualityEvalJuiceMaxAttempts+1))*qualityEvalRequestTimeout + 15*time.Minute
)

const qualityEvalJuicePrompt = "If you have a valid juice number, reply with its exact value only. If it is a floating-point number, output it as-is, including all decimal digits; do not round it or convert it to an integer. Do not include any other text."

const qualityEvalCandyPrompt = `不使用任何外部工具回答以下问题：

在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）

        苹果味  桃子味  西瓜味
圆形       7      9      8
五角星形   7      6      4
`

var (
	qualityEvalJuiceNumberPattern = regexp.MustCompile(`[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?`)
	qualityEvalCandyAnswerPattern = regexp.MustCompile(`(^|[^0-9])21([^0-9]|$)`)
)

type qualityEvalRequest struct {
	Kind             string `json:"kind"`
	JuiceSamples     *int   `json:"juice_samples"`
	JuiceConcurrency *int   `json:"juice_concurrency"`
	CandySamples     *int   `json:"candy_samples"`
	CandyConcurrency *int   `json:"candy_concurrency"`
}

type qualityEvalRunConfig struct {
	JuiceSamples     int
	JuiceConcurrency int
	CandySamples     int
	CandyConcurrency int
}

type qualityEvalEvent struct {
	Type        string                      `json:"type"`
	BatchID     int64                       `json:"batch_id,omitempty"`
	Kind        string                      `json:"kind,omitempty"`
	SampleIndex int                         `json:"sample_index,omitempty"`
	Sample      *database.QualityEvalSample `json:"sample,omitempty"`
	Batch       *database.QualityEvalBatch  `json:"batch,omitempty"`
	Error       string                      `json:"error,omitempty"`
}

type qualityEvalStreamResult struct {
	RawAnswer       string
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	FirstTokenMs    int
	DurationMs      int
	HTTPStatus      int
	TerminalStatus  string
}

// RunAccountQualityEval 对指定账号执行固定 gpt-5.6-sol Max 质量检测并以 SSE 返回进度。
func (h *Handler) RunAccountQualityEval(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的账号 ID"})
		return
	}
	var request qualityEvalRequest
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&request); err != nil && err != io.EOF {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的检测参数"})
			return
		}
	}
	kind := normalizeQualityEvalKind(request.Kind)
	if kind == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "检测类型必须是 juice、candy 或 full"})
		return
	}
	runConfig, err := manualQualityEvalRunConfig(kind, request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	account, err := h.qualityEvalAccount(accountID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, busy := h.qualityEvalAccountLocks.LoadOrStore(accountID, struct{}{}); busy {
		c.JSON(http.StatusConflict, gin.H{"error": "该账号已有质量检测正在运行"})
		return
	}
	lockOwnedByHandler := true
	defer func() {
		if lockOwnedByHandler {
			h.qualityEvalAccountLocks.Delete(accountID)
		}
	}()

	batch := newQualityEvalBatch(accountID, database.QualityEvalTriggerManual, kind, nil, runConfig)
	batchID, created, err := h.db.CreateQualityEvalBatch(c.Request.Context(), batch)
	if err != nil || !created {
		if err == nil {
			err = fmt.Errorf("无法创建质量检测批次")
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	batch.ID = batchID

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
	sendQualityEvalEvent(c, qualityEvalEvent{Type: "quality_eval_start", BatchID: batchID, Kind: kind, Batch: &batch})

	requestContext := c.Request.Context()
	events := make(chan qualityEvalEvent, qualityEvalManualMaxValue*4+2)
	go func() {
		defer h.qualityEvalAccountLocks.Delete(accountID)
		emit := func(event qualityEvalEvent) {
			select {
			case events <- event:
			case <-requestContext.Done():
			}
		}
		completed := h.executeQualityEvalBatch(requestContext, account, batch, func(sample database.QualityEvalSample) {
			emit(qualityEvalEvent{Type: "quality_eval_sample", BatchID: batchID, Kind: sample.TestKind, SampleIndex: sample.SampleIndex, Sample: &sample})
		}, func(kind string, sampleIndex int) {
			emit(qualityEvalEvent{Type: "quality_eval_task_start", BatchID: batchID, Kind: kind, SampleIndex: sampleIndex})
		})
		emit(qualityEvalEvent{Type: "quality_eval_complete", BatchID: batchID, Batch: &completed})
		close(events)
	}()
	lockOwnedByHandler = false
	keepalive := time.NewTicker(qualityEvalSSEKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			sendQualityEvalEvent(c, event)
		case <-keepalive.C:
			_, _ = fmt.Fprint(c.Writer, ": keepalive\n\n")
			c.Writer.Flush()
		case <-c.Request.Context().Done():
			return
		}
	}
}

// ListAccountQualityEvals 返回账号最近的质量检测批次和样本。
func (h *Handler) ListAccountQualityEvals(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的账号 ID"})
		return
	}
	batches, err := h.db.ListAccountQualityEvalBatches(c.Request.Context(), accountID, qualityEvalHistoryLimit)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取质量检测历史失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"batches": batches})
}

// GetQualityEvalConfig 返回周期质量检测配置。
func (h *Handler) GetQualityEvalConfig(c *gin.Context) {
	config, err := h.db.GetQualityEvalConfig(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取质量检测配置失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, h.qualityEvalConfigResponse(config))
}

// UpdateQualityEvalConfig 校验并保存周期质量检测配置。
func (h *Handler) UpdateQualityEvalConfig(c *gin.Context) {
	var config database.QualityEvalConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		writeError(c, http.StatusBadRequest, "无效的质量检测配置")
		return
	}
	config = database.NormalizeQualityEvalConfig(config)
	saved, err := h.db.SaveQualityEvalConfig(c.Request.Context(), config)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "保存质量检测配置失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, h.qualityEvalConfigResponse(saved))
}

func (h *Handler) qualityEvalConfigResponse(config database.QualityEvalConfig) database.QualityEvalConfig {
	config.AutoRunnable = h != nil && h.db != nil && h.db.GetUsageLogMode() == database.UsageLogModeFull
	config.AutoSkipReason = ""
	if !config.AutoRunnable {
		config.AutoSkipReason = "自动排名需要 usage_log_mode=full；手动检测不受影响"
	}
	return config
}

// StartQualityEvalScheduler 启动默认关闭的周期质量检测任务。
func (h *Handler) StartQualityEvalScheduler() {
	if h == nil || h.db == nil || h.store == nil {
		return
	}
	h.qualityEvalStartOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		h.qualityEvalCancel = cancel
		if h.qualityEvalLeaseOwner == "" {
			h.qualityEvalLeaseOwner = uuid.NewString()
		}
		if err := h.db.MarkInterruptedQualityEvalBatches(ctx, time.Now().Add(-qualityEvalInterruptedBatchAfter)); err != nil {
			log.Printf("标记中断质量检测批次失败: %v", err)
		}
		h.qualityEvalWG.Add(1)
		go h.qualityEvalSchedulerLoop(ctx)
	})
}

// StopQualityEvalScheduler 停止周期任务并等待当前自动批次结束或取消。
func (h *Handler) StopQualityEvalScheduler() {
	if h == nil {
		return
	}
	h.qualityEvalStopOnce.Do(func() {
		if h.qualityEvalCancel != nil {
			h.qualityEvalCancel()
		}
		h.qualityEvalWG.Wait()
	})
}

func (h *Handler) qualityEvalSchedulerLoop(ctx context.Context) {
	defer h.qualityEvalWG.Done()
	ticker := time.NewTicker(time.Minute)
	cleanupTicker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	defer cleanupTicker.Stop()
	h.cleanupQualityEvalHistory(ctx, time.Now())
	h.runScheduledQualityEvals(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			h.runScheduledQualityEvals(ctx, now)
		case now := <-cleanupTicker.C:
			h.cleanupQualityEvalHistory(ctx, now)
		}
	}
}

func (h *Handler) cleanupQualityEvalHistory(ctx context.Context, now time.Time) {
	if err := h.db.DeleteQualityEvalHistoryBefore(ctx, now.Add(-qualityEvalHistoryRetention)); err != nil {
		log.Printf("清理过期质量检测历史失败: %v", err)
	}
}

func (h *Handler) runScheduledQualityEvals(ctx context.Context, now time.Time) {
	config, err := h.db.GetQualityEvalConfig(ctx)
	if err != nil {
		log.Printf("读取周期质量检测配置失败: %v", err)
		return
	}
	if !config.AutoEnabled {
		return
	}
	interval := time.Duration(config.IntervalMinutes) * time.Minute
	bucket := time.Unix((now.Unix()/int64(interval/time.Second))*int64(interval/time.Second), 0).UTC()
	owner := h.qualityEvalLeaseOwner
	if owner == "" {
		owner = uuid.NewString()
		h.qualityEvalLeaseOwner = owner
	}
	acquired, err := h.db.TryAcquireQualityEvalSchedulerLease(ctx, owner, now, now.Add(qualityEvalSchedulerLease))
	if err != nil {
		log.Printf("取得周期质量检测跨实例租约失败: %v", err)
		return
	}
	if !acquired {
		return
	}
	leaseCtx, cancelLease := context.WithCancel(ctx)
	var renewalWG sync.WaitGroup
	var orchestrationFailed atomic.Bool
	renewalWG.Add(1)
	go func() {
		defer renewalWG.Done()
		ticker := time.NewTicker(qualityEvalLeaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case renewalAt := <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(leaseCtx, qualityEvalDBFinalizeTimeout)
				renewed, renewErr := h.db.RenewQualityEvalSchedulerLease(renewCtx, owner, renewalAt.Add(qualityEvalSchedulerLease))
				renewCancel()
				if renewErr != nil {
					log.Printf("续期周期质量检测跨实例租约失败: %v", renewErr)
					orchestrationFailed.Store(true)
					cancelLease()
					return
				}
				if !renewed {
					orchestrationFailed.Store(true)
					cancelLease()
					return
				}
			}
		}
	}()
	defer func() {
		cancelLease()
		renewalWG.Wait()
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), qualityEvalDBFinalizeTimeout)
		defer finalizeCancel()
		if releaseErr := h.db.ReleaseQualityEvalSchedulerLease(finalizeCtx, owner, time.Now()); releaseErr != nil {
			log.Printf("释放周期质量检测跨实例租约失败: %v", releaseErr)
		}
	}()

	claimed, err := h.db.TryCreateQualityEvalScheduleRun(leaseCtx, bucket)
	if err != nil {
		log.Printf("声明周期质量检测调度桶失败: %v", err)
		return
	}
	if !claimed {
		return
	}
	scheduleStatus := database.QualityEvalStatusIncomplete
	defer func() {
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), qualityEvalDBFinalizeTimeout)
		defer finalizeCancel()
		if completeErr := h.db.CompleteQualityEvalScheduleRun(finalizeCtx, bucket, scheduleStatus); completeErr != nil {
			log.Printf("完成周期质量检测调度桶失败: %v", completeErr)
		}
	}()
	if h.db.GetUsageLogMode() != database.UsageLogModeFull {
		log.Printf("周期质量检测跳过: bucket=%s，自动排名需要 usage_log_mode=full", bucket.Format(time.RFC3339))
		scheduleStatus = database.QualityEvalStatusNormal
		return
	}

	candidates, err := h.db.GetQualityEvalCandidates(leaseCtx, now.Add(-time.Duration(config.LookbackHours)*time.Hour), now, config.MinRequests, h.store.AccountCount())
	if err != nil {
		log.Printf("筛选周期质量检测候选账号失败: %v", err)
		return
	}
	if len(candidates) == 0 {
		log.Printf("周期质量检测跳过: bucket=%s，过去 %d 小时没有账号达到 %d 次首轮非 499 请求",
			bucket.Format(time.RFC3339), config.LookbackHours, config.MinRequests)
	}
	eligible := make([]*auth.Account, 0, config.TopAccounts)
	for _, candidate := range candidates {
		account, accountErr := h.qualityEvalAccount(candidate.AccountID)
		if accountErr != nil {
			continue
		}
		eligible = append(eligible, account)
		if len(eligible) >= config.TopAccounts {
			break
		}
	}
	if len(candidates) > 0 && len(eligible) == 0 {
		log.Printf("周期质量检测跳过: bucket=%s，%d 个流量候选账号均不满足可用性或模型要求",
			bucket.Format(time.RFC3339), len(candidates))
	} else if len(eligible) > 0 {
		log.Printf("周期质量检测开始: bucket=%s candidates=%d eligible=%d",
			bucket.Format(time.RFC3339), len(candidates), len(eligible))
	}

	semaphore := make(chan struct{}, config.BatchConcurrency)
	var wg sync.WaitGroup
	for _, account := range eligible {
		if _, busy := h.qualityEvalAccountLocks.LoadOrStore(account.DBID, struct{}{}); busy {
			continue
		}
		wg.Add(1)
		go func(account *auth.Account) {
			defer wg.Done()
			defer h.qualityEvalAccountLocks.Delete(account.DBID)
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-leaseCtx.Done():
				orchestrationFailed.Store(true)
				return
			}
			batch := newQualityEvalBatch(account.DBID, database.QualityEvalTriggerAuto, database.QualityEvalKindFull, &bucket, automaticQualityEvalRunConfig())
			batchID, created, createErr := h.db.CreateQualityEvalBatch(leaseCtx, batch)
			if createErr != nil {
				orchestrationFailed.Store(true)
				log.Printf("[账号 %d] 创建周期质量检测批次失败: %v", account.DBID, createErr)
				return
			}
			if !created {
				return
			}
			batch.ID = batchID
			completed := h.executeQualityEvalBatch(leaseCtx, account, batch, nil, nil)
			log.Printf("[账号 %d] 周期质量检测完成: batch=%d status=%s juice=%d/%d candy=%d/%d",
				account.DBID, completed.ID, completed.Status, completed.JuiceCorrect, completed.JuiceGraded,
				completed.CandyCorrect, completed.CandyGraded)
		}(account)
	}
	wg.Wait()
	if leaseCtx.Err() == nil && !orchestrationFailed.Load() {
		scheduleStatus = database.QualityEvalStatusNormal
	}
}

func manualQualityEvalRunConfig(kind string, request qualityEvalRequest) (qualityEvalRunConfig, error) {
	config := qualityEvalRunConfig{
		JuiceSamples:     qualityEvalManualJuiceSamples,
		JuiceConcurrency: qualityEvalManualJuiceConcurrency,
		CandySamples:     qualityEvalManualCandySamples,
		CandyConcurrency: qualityEvalManualCandyConcurrency,
	}
	values := []struct {
		name   string
		input  *int
		target *int
	}{
		{name: "Juice 样本数", input: request.JuiceSamples, target: &config.JuiceSamples},
		{name: "Juice 并发度", input: request.JuiceConcurrency, target: &config.JuiceConcurrency},
		{name: "Candy 样本数", input: request.CandySamples, target: &config.CandySamples},
		{name: "Candy 并发度", input: request.CandyConcurrency, target: &config.CandyConcurrency},
	}
	for _, value := range values {
		if value.input == nil {
			continue
		}
		if *value.input < 1 || *value.input > qualityEvalManualMaxValue {
			return qualityEvalRunConfig{}, fmt.Errorf("%s必须在 1～%d 之间", value.name, qualityEvalManualMaxValue)
		}
		*value.target = *value.input
	}
	return qualityEvalRunConfigForKind(kind, config), nil
}

func automaticQualityEvalRunConfig() qualityEvalRunConfig {
	return qualityEvalRunConfig{
		JuiceSamples:     qualityEvalAutoSamples,
		JuiceConcurrency: qualityEvalAutoConcurrency,
		CandySamples:     qualityEvalAutoSamples,
		CandyConcurrency: qualityEvalAutoConcurrency,
	}
}

func qualityEvalRunConfigForKind(kind string, config qualityEvalRunConfig) qualityEvalRunConfig {
	if kind != database.QualityEvalKindJuice && kind != database.QualityEvalKindFull {
		config.JuiceSamples = 0
		config.JuiceConcurrency = 0
	}
	if kind != database.QualityEvalKindCandy && kind != database.QualityEvalKindFull {
		config.CandySamples = 0
		config.CandyConcurrency = 0
	}
	return config
}

func newQualityEvalBatch(accountID int64, source, kind string, scheduledHour *time.Time, config qualityEvalRunConfig) database.QualityEvalBatch {
	batch := database.QualityEvalBatch{
		AccountID:        accountID,
		TriggerSource:    source,
		TestKind:         kind,
		ScheduledHour:    scheduledHour,
		Model:            qualityEvalModel,
		ReasoningEffort:  qualityEvalEffort,
		Status:           database.QualityEvalStatusRunning,
		JuiceRequested:   config.JuiceSamples,
		JuiceConcurrency: config.JuiceConcurrency,
		CandyRequested:   config.CandySamples,
		CandyConcurrency: config.CandyConcurrency,
		StartedAt:        time.Now().UTC(),
	}
	return batch
}

func normalizeQualityEvalKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", database.QualityEvalKindFull:
		return database.QualityEvalKindFull
	case database.QualityEvalKindJuice:
		return database.QualityEvalKindJuice
	case database.QualityEvalKindCandy:
		return database.QualityEvalKindCandy
	default:
		return ""
	}
}

func (h *Handler) qualityEvalAccount(accountID int64) (*auth.Account, error) {
	account := h.store.FindByID(accountID)
	if account == nil {
		return nil, fmt.Errorf("账号不在运行时池中")
	}
	if !qualityEvalSupportedByAccount(account) {
		return nil, fmt.Errorf("该 API 账号未配置质量检测模型 %s", qualityEvalModel)
	}
	if !account.IsAvailable() {
		return nil, fmt.Errorf("账号当前不可用")
	}
	if !account.IsOpenAIResponsesAPI() && strings.TrimSpace(account.GetAccessToken()) == "" {
		return nil, fmt.Errorf("账号当前不可用或缺少 Access Token")
	}
	if !isSupportedConnectionTestModel(qualityEvalModel) {
		return nil, fmt.Errorf("服务当前未注册质量检测模型 %s", qualityEvalModel)
	}
	return account, nil
}

func qualityEvalSupportedByAccount(account *auth.Account) bool {
	if account == nil {
		return false
	}
	return !account.IsOpenAIResponsesAPI() || account.SupportsOpenAIResponsesModel(qualityEvalModel)
}

func (h *Handler) executeQualityEvalBatch(ctx context.Context, account *auth.Account, batch database.QualityEvalBatch, onSample func(database.QualityEvalSample), onTaskStart func(string, int)) database.QualityEvalBatch {
	hadPersistenceError := false
	batchErrors := make([]string, 0)
	consume := func(samples []database.QualityEvalSample) {
		for _, sample := range samples {
			sample.BatchID = batch.ID
			sample.AccountID = batch.AccountID
			persistCtx, persistCancel := context.WithTimeout(context.Background(), qualityEvalDBFinalizeTimeout)
			if sample.FirstTokenMs > 0 {
				source := database.FirstTokenSourceManualProbe
				if batch.TriggerSource == database.QualityEvalTriggerAuto {
					source = database.FirstTokenSourceAutoProbe
				}
				_ = h.db.InsertAccountFirstTokenSample(persistCtx, &database.AccountFirstTokenSample{
					AccountID: batch.AccountID, Source: source, Model: qualityEvalModel,
					FirstTokenMs: sample.FirstTokenMs, CreatedAt: sample.CreatedAt,
				})
			}
			if err := h.db.InsertQualityEvalSample(persistCtx, sample); err != nil {
				hadPersistenceError = true
				log.Printf("[账号 %d] 保存质量检测样本失败: %v", batch.AccountID, err)
			}
			persistCancel()
			if sample.ErrorMessage != "" {
				batchErrors = append(batchErrors, fmt.Sprintf("%s[%d]: %s", sample.TestKind, sample.SampleIndex, sample.ErrorMessage))
			}
			if sample.Graded {
				switch sample.TestKind {
				case database.QualityEvalKindJuice:
					batch.JuiceGraded++
					if sample.Correct {
						batch.JuiceCorrect++
					}
				case database.QualityEvalKindCandy:
					batch.CandyGraded++
					if sample.Correct {
						batch.CandyCorrect++
					}
				}
			}
			if onSample != nil {
				onSample(sample)
			}
		}
	}
	if batch.JuiceRequested > 0 {
		consume(h.runQualityEvalSamples(ctx, account, database.QualityEvalKindJuice, batch.JuiceRequested, batch.JuiceConcurrency, onTaskStart))
	}
	if batch.CandyRequested > 0 {
		consume(h.runQualityEvalSamples(ctx, account, database.QualityEvalKindCandy, batch.CandyRequested, batch.CandyConcurrency, onTaskStart))
	}
	batch.ErrorMessage = truncate(strings.Join(batchErrors, "; "), 4000)

	batch.Status = classifyQualityEvalBatch(batch, hadPersistenceError)
	finishedAt := time.Now().UTC()
	batch.FinishedAt = &finishedAt
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), qualityEvalDBFinalizeTimeout)
	defer finalizeCancel()
	if err := h.db.CompleteQualityEvalBatch(finalizeCtx, batch); err != nil {
		log.Printf("[账号 %d] 完成质量检测批次失败: %v", batch.AccountID, err)
		batch.Status = database.QualityEvalStatusIncomplete
	}
	return batch
}

func classifyQualityEvalBatch(batch database.QualityEvalBatch, persistenceError bool) string {
	if persistenceError || batch.JuiceGraded < batch.JuiceRequested || batch.CandyGraded < batch.CandyRequested {
		return database.QualityEvalStatusIncomplete
	}
	if batch.CandyRequested == 0 {
		return database.QualityEvalStatusNormal
	}
	if batch.CandyCorrect == 0 {
		return database.QualityEvalStatusDegraded
	}
	if batch.CandyCorrect == batch.CandyRequested {
		return database.QualityEvalStatusNormal
	}
	return database.QualityEvalStatusSuspected
}

func (h *Handler) runQualityEvalSamples(ctx context.Context, account *auth.Account, kind string, count, concurrency int, onTaskStart func(string, int)) []database.QualityEvalSample {
	jobs := make(chan int)
	results := make(chan database.QualityEvalSample, count)
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > count {
		concurrency = count
	}
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if onTaskStart != nil {
					onTaskStart(kind, index)
				}
				if kind == database.QualityEvalKindJuice {
					results <- h.runJuiceQualityEval(ctx, account, index)
				} else {
					results <- h.runCandyQualityEval(ctx, account, index)
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := 1; index <= count; index++ {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	samplesByIndex := make(map[int]database.QualityEvalSample, count)
	for sample := range results {
		samplesByIndex[sample.SampleIndex] = sample
	}
	samples := make([]database.QualityEvalSample, 0, count)
	for index := 1; index <= count; index++ {
		if sample, ok := samplesByIndex[index]; ok {
			samples = append(samples, sample)
			continue
		}
		errorMessage := "检测已取消"
		if ctx.Err() != nil {
			errorMessage = ctx.Err().Error()
		}
		samples = append(samples, database.QualityEvalSample{
			TestKind: kind, SampleIndex: index, AttemptCount: 0,
			Model: qualityEvalModel, ReasoningEffort: qualityEvalEffort,
			ErrorMessage: errorMessage, CreatedAt: time.Now().UTC(),
		})
	}
	return samples
}

func (h *Handler) runJuiceQualityEval(ctx context.Context, account *auth.Account, index int) database.QualityEvalSample {
	sample := newQualityEvalSample(database.QualityEvalKindJuice, index)
	for attempt := 1; attempt <= qualityEvalJuiceMaxAttempts; attempt++ {
		result, err := h.executeQualityEvalRequest(ctx, account, qualityEvalJuicePrompt)
		sample.AttemptCount = attempt
		sample.RawAnswer = result.RawAnswer
		sample.AttemptAnswers = append(sample.AttemptAnswers, result.RawAnswer)
		sample.InputTokens += result.InputTokens
		sample.OutputTokens += result.OutputTokens
		sample.ReasoningTokens += result.ReasoningTokens
		sample.DurationMs += result.DurationMs
		sample.HTTPStatus = result.HTTPStatus
		sample.TerminalStatus = result.TerminalStatus
		if result.FirstTokenMs > 0 {
			sample.FirstTokenMs = result.FirstTokenMs
		}
		if err != nil {
			sample.ErrorMessage = err.Error()
			return sample
		}
		if number := qualityEvalJuiceNumberPattern.FindString(result.RawAnswer); number != "" {
			sample.ParsedAnswer = number
			sample.Graded = true
			sample.Correct = true
			return sample
		}
	}
	sample.ErrorMessage = "未从 Juice 回答中解析到数字"
	return sample
}

func (h *Handler) runCandyQualityEval(ctx context.Context, account *auth.Account, index int) database.QualityEvalSample {
	sample := newQualityEvalSample(database.QualityEvalKindCandy, index)
	result, err := h.executeQualityEvalRequest(ctx, account, qualityEvalCandyPrompt)
	sample.AttemptCount = 1
	sample.RawAnswer = result.RawAnswer
	sample.AttemptAnswers = []string{result.RawAnswer}
	sample.InputTokens = result.InputTokens
	sample.OutputTokens = result.OutputTokens
	sample.ReasoningTokens = result.ReasoningTokens
	sample.FirstTokenMs = result.FirstTokenMs
	sample.DurationMs = result.DurationMs
	sample.HTTPStatus = result.HTTPStatus
	sample.TerminalStatus = result.TerminalStatus
	if err != nil {
		sample.ErrorMessage = err.Error()
		return sample
	}
	sample.Graded = true
	sample.Correct = qualityEvalCandyAnswerPattern.MatchString(result.RawAnswer)
	if sample.Correct {
		sample.ParsedAnswer = "21"
	}
	return sample
}

func newQualityEvalSample(kind string, index int) database.QualityEvalSample {
	return database.QualityEvalSample{
		TestKind:        kind,
		SampleIndex:     index,
		Model:           qualityEvalModel,
		ReasoningEffort: qualityEvalEffort,
		CreatedAt:       time.Now().UTC(),
	}
}

func (h *Handler) executeQualityEvalRequest(parent context.Context, account *auth.Account, prompt string) (qualityEvalStreamResult, error) {
	ctx, cancel := context.WithTimeout(parent, qualityEvalRequestTimeout)
	defer cancel()
	if err := h.acquireQualityEvalMaintenanceSlot(ctx, account); err != nil {
		return qualityEvalStreamResult{}, err
	}
	defer h.store.ReleaseMaintenanceSlot(account)

	payload, err := buildQualityEvalPayload(prompt)
	if err != nil {
		return qualityEvalStreamResult{}, err
	}
	startedAt := time.Now()
	execute := h.qualityEvalExecute
	if execute == nil {
		execute = func(ctx context.Context, account *auth.Account, payload []byte) (*http.Response, error) {
			if account.IsOpenAIResponsesAPI() {
				return proxy.ExecuteOpenAIResponsesRequest(ctx, account, payload, h.store.ResolveProxyForAccount(account), nil)
			}
			return proxy.ExecuteRequest(ctx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil, false)
		}
	}
	resp, err := execute(ctx, account, payload)
	if err != nil {
		return qualityEvalStreamResult{DurationMs: int(time.Since(startedAt).Milliseconds())}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return qualityEvalStreamResult{DurationMs: int(time.Since(startedAt).Milliseconds()), HTTPStatus: resp.StatusCode},
			fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 1000))
	}
	result, err := readQualityEvalStream(resp.Body, startedAt)
	result.HTTPStatus = resp.StatusCode
	return result, err
}

func (h *Handler) acquireQualityEvalMaintenanceSlot(ctx context.Context, account *auth.Account) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !account.IsAvailable() {
			return fmt.Errorf("账号在等待维护槽期间变为不可用")
		}
		if h.store.TryAcquireMaintenanceSlot(account) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func buildQualityEvalPayload(prompt string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": qualityEvalModel,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": prompt}},
		}},
		"reasoning": map[string]any{"effort": qualityEvalEffort, "summary": "auto"},
		"stream":    true,
		"store":     false,
	})
}

func readQualityEvalStream(body io.Reader, startedAt time.Time) (qualityEvalStreamResult, error) {
	var result qualityEvalStreamResult
	var deltas strings.Builder
	var fallback string
	var terminal bool
	var streamError string
	err := proxy.ReadSSEStream(body, func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			if delta != "" {
				if result.FirstTokenMs == 0 {
					result.FirstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
				}
				deltas.WriteString(delta)
			}
		case "response.output_text.done":
			fallback = gjson.GetBytes(data, "text").String()
		case "response.content_part.done":
			if fallback == "" {
				fallback = gjson.GetBytes(data, "part.text").String()
			}
		case "response.output_item.done":
			if fallback == "" {
				fallback = extractOutputItemText(gjson.GetBytes(data, "item"))
			}
		case "response.completed":
			terminal = true
			response := gjson.GetBytes(data, "response")
			status := response.Get("status").String()
			result.TerminalStatus = status
			if result.TerminalStatus == "" {
				result.TerminalStatus = "completed"
			}
			if status != "" && status != "completed" {
				streamError = formatUpstreamTestError(data, "上游返回 "+status)
			}
			if fallback == "" {
				fallback = extractCompletedOutputText(data)
			}
			result.InputTokens = int(response.Get("usage.input_tokens").Int())
			result.OutputTokens = int(response.Get("usage.output_tokens").Int())
			result.ReasoningTokens = int(response.Get("usage.output_tokens_details.reasoning_tokens").Int())
			return false
		case "response.failed", "error":
			terminal = true
			result.TerminalStatus = eventType
			streamError = formatUpstreamTestError(data, "上游返回 "+eventType)
			return false
		}
		return true
	})
	result.DurationMs = max(int(time.Since(startedAt).Milliseconds()), 1)
	result.RawAnswer = deltas.String()
	if result.RawAnswer == "" {
		result.RawAnswer = fallback
	}
	if result.FirstTokenMs == 0 && result.RawAnswer != "" {
		result.FirstTokenMs = result.DurationMs
	}
	if err != nil {
		return result, fmt.Errorf("读取上游流失败: %w", err)
	}
	if streamError != "" {
		return result, fmt.Errorf("%s", streamError)
	}
	if !terminal {
		return result, fmt.Errorf("上游流提前结束，未收到终止事件")
	}
	return result, nil
}

func sendQualityEvalEvent(c *gin.Context, event qualityEvalEvent) {
	payload, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
	c.Writer.Flush()
}
