package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// promptFilterFullTextMaxRunes 限制被拦截请求记录的完整文本长度（按字符截断），
// 避免单条日志过大撑爆数据库；80KB 提取上限下取 ~32K 字符足够定位问题。
const promptFilterFullTextMaxRunes = 32000

func (h *Handler) inspectPromptFilterOpenAI(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	verdict := promptfilter.Inspect(rawBody, endpoint, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		text := promptfilter.ExtractText(rawBody, endpoint, cfg.MaxTextLength)
		verdict = h.reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg)
	}
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		"Request contains content blocked by prompt filter",
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
	return true
}

func (h *Handler) inspectPromptFilterTextOpenAI(c *gin.Context, text string, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	verdict := promptfilter.InspectText(text, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = h.reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg)
	}
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		"Request contains content blocked by prompt filter",
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
	return true
}

func (h *Handler) inspectPromptFilterAnthropic(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	verdict := promptfilter.Inspect(rawBody, endpoint, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		text := promptfilter.ExtractText(rawBody, endpoint, cfg.MaxTextLength)
		verdict = h.reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg)
	}
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if verdict.Action == promptfilter.ActionBlock {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request contains content blocked by prompt filter")
		return true
	}
	return false
}

func (h *Handler) logPromptFilterVerdict(c *gin.Context, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict) {
	if h == nil || h.db == nil || !verdict.Enabled {
		return
	}
	if source == "local_filter" && len(verdict.Matched) == 0 {
		return
	}
	if h.store != nil {
		cfg := h.store.GetPromptFilterConfig()
		if source == "local_filter" && !cfg.LogMatches {
			return
		}
	}
	input := &database.PromptFilterLogInput{
		Source:          source,
		Endpoint:        endpoint,
		Model:           model,
		Action:          verdict.Action,
		Mode:            verdict.Mode,
		Score:           verdict.Score,
		Threshold:       verdict.Threshold,
		MatchedPatterns: promptfilter.MatchesJSON(verdict.Matched),
		TextPreview:     verdict.TextPreview,
		ClientIP:        c.ClientIP(),
		ErrorCode:       errorCode,
		ReviewModel:     verdict.ReviewModel,
		ReviewFlagged:   verdict.ReviewFlagged,
		ReviewError:     verdict.ReviewError,
	}
	// 被拦截（block）的请求记录完整检查文本，便于排查到底是什么触发了拦截
	// （预览只有 500 字往往看不出内容）；放行/告警仍只存预览以控制存储。
	if verdict.Action == promptfilter.ActionBlock {
		input.FullText = promptfilter.Preview(verdict.FullText, promptFilterFullTextMaxRunes)
	}
	populatePromptFilterAPIKeyMeta(c, input)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.db.InsertPromptFilterLog(ctx, input)
}

func (h *Handler) logUpstreamCyberPolicy(c *gin.Context, endpoint string, model string, body []byte) {
	if h == nil || h.store == nil {
		return
	}
	errorCode := upstreamCyberPolicyCode(body)
	if errorCode == "" {
		return
	}
	cfg := h.store.GetPromptFilterConfig()
	verdict := promptfilter.Verdict{
		Enabled:   true,
		Mode:      cfg.Mode,
		Action:    promptfilter.ActionBlock,
		Score:     0,
		Threshold: cfg.Threshold,
		Reason:    "upstream returned cyber policy",
		// 上游 cyber_policy 没有本地提取文本，把上游错误体作为「详细内容」记录，
		// 方便在日志里看清触发详情。
		FullText: string(body),
	}
	h.logPromptFilterVerdict(c, endpoint, model, "upstream_cyber_policy", errorCode, verdict)
}

func upstreamCyberPolicyCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	raw := string(body)
	for _, path := range []string{"codex_error_info", "error.codex_error_info", "error.code", "code"} {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); strings.EqualFold(value, "cyber_policy") {
			return "cyber_policy"
		}
	}
	if strings.Contains(strings.ToLower(raw), "cyber_policy") || strings.Contains(strings.ToLower(raw), "cyber security risk") {
		return "cyber_policy"
	}
	return ""
}

func populatePromptFilterAPIKeyMeta(c *gin.Context, input *database.PromptFilterLogInput) {
	if c == nil || input == nil {
		return
	}
	if v, exists := c.Get(contextAPIKeyID); exists && v != nil {
		switch typed := v.(type) {
		case int64:
			input.APIKeyID = typed
		case int:
			input.APIKeyID = int64(typed)
		}
	}
	if v, exists := c.Get(contextAPIKeyName); exists && v != nil {
		if name, ok := v.(string); ok {
			input.APIKeyName = name
		}
	}
	if v, exists := c.Get(contextAPIKeyMasked); exists && v != nil {
		if masked, ok := v.(string); ok {
			input.APIKeyMasked = masked
		}
	}
}

func shouldReviewPromptFilterVerdict(verdict promptfilter.Verdict, cfg promptfilter.Config) bool {
	if verdict.Action != promptfilter.ActionWarn && verdict.Action != promptfilter.ActionBlock {
		return false
	}
	return promptfilter.NormalizeReviewConfig(cfg.Review).Ready()
}

func (h *Handler) reviewPromptFilterVerdict(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	flagged, model, err := promptfilter.DefaultReviewClient.ReviewText(ctx, text, cfg.Review)
	return promptfilter.ApplyReviewResult(verdict, flagged, model, err, cfg.Review)
}
