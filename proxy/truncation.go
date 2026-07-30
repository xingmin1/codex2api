package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const upstreamErrorKindUnsupportedTruncation = "unsupported_truncation"

// normalizeCodexTruncation 删除 Codex 上游不接受、且省略后语义不变的 disabled。
// auto 必须保留：它代表客户端明确要求自动截断，不能被静默改写成 disabled。
func normalizeCodexTruncation(body map[string]any) {
	if body == nil {
		return
	}
	truncation, _ := body["truncation"].(string)
	if strings.EqualFold(strings.TrimSpace(truncation), "disabled") {
		delete(body, "truncation")
	}
}

func codexAutoTruncationRequested(body []byte) bool {
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "truncation").String()), "auto")
}

func unsupportedCodexTruncationAPIError() *api.APIError {
	return api.NewAPIError(
		api.ErrorCode(upstreamErrorKindUnsupportedTruncation),
		"Codex upstream does not support truncation=auto; omit truncation or use truncation=disabled",
		api.ErrorTypeInvalidRequest,
	)
}

// sendUnsupportedTruncationRequestError 返回确定性的客户端错误，并让外层整请求重放器停止。
func sendUnsupportedTruncationRequestError(c *gin.Context, err *api.APIError) {
	if err == nil {
		err = unsupportedCodexTruncationAPIError()
	}
	blockClientRequestReplay(c, clientRequestReplayStopUnsupportedTruncation)
	api.SendErrorWithStatus(c, err, http.StatusBadRequest)
}

// routeResponsesByTruncationCapability 将 auto 请求限制到支持公开 Responses
// 截断语义的 relay 账号；没有可用 relay 时返回确定性的客户端错误。
func routeResponsesByTruncationCapability(rawBody []byte, store *auth.Store, base auth.AccountFilter) (auth.AccountFilter, *api.APIError) {
	if !codexAutoTruncationRequested(rawBody) {
		return base, nil
	}
	relayFilter := func(account *auth.Account) bool {
		if base != nil && !base(account) {
			return false
		}
		return account != nil && account.IsOpenAIResponsesAPI()
	}
	if store == nil {
		return nil, unsupportedCodexTruncationAPIError()
	}
	for _, account := range store.Accounts() {
		if relayFilter(account) {
			return relayFilter, nil
		}
	}
	return nil, unsupportedCodexTruncationAPIError()
}

// isUnsupportedTruncationError 识别上游明确拒绝 truncation 参数的 400。
// 这类错误是确定性的请求兼容性错误，不能换号或整请求重放。
func isUnsupportedTruncationError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest || len(body) == 0 {
		return false
	}
	for _, prefix := range []string{"error", "response.error", "response.status_details.error", "status_details.error"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, prefix+".code").String()))
		typ := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, prefix+".type").String()))
		param := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, prefix+".param").String()))
		message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, prefix+".message").String()))
		if code == "" && typ == "" && param == "" && message == "" {
			continue
		}
		if param != "truncation" && !strings.Contains(code+" "+typ+" "+message, "truncation") {
			continue
		}
		if code == "unsupported_parameter" ||
			code == "unknown_parameter" ||
			typ == "unsupported_parameter" ||
			typ == "unknown_parameter" ||
			strings.Contains(message, "unsupported parameter") ||
			strings.Contains(message, "unknown parameter") {
			return true
		}
	}
	return false
}

func isUnsupportedTruncationFailure(payload []byte) bool {
	return isUnsupportedTruncationError(responseFailedStatusCode(payload), responseFailedErrorBody(payload))
}

func isUnsupportedTruncationPayload(payload []byte) bool {
	if isUnsupportedTruncationFailure(payload) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "type").String()), "response.failed") || responsesPayloadIsFailed(payload) {
		return isUnsupportedTruncationError(http.StatusBadRequest, payload)
	}
	return false
}

// responseFailedHTTPError 将 HTTP 200 中携带的 response.failed 还原为统一的上游错误。
// compact 端点不能把失败对象当作成功结果透传，否则客户端会误以为压缩完成。
func responseFailedHTTPError(payload []byte) (int, []byte) {
	statusCode := responseFailedStatusCode(payload)
	if statusCode < http.StatusBadRequest || statusCode > 599 {
		statusCode = http.StatusBadGateway
	}
	return statusCode, responseFailedErrorBody(payload)
}

// compactResponseFailureDetails 将 compact 的 HTTP 200 失败体统一成可重试的状态机输入。
// 失败终态本身不参与 native compaction exact-one 校验，但瞬时失败仍可按账号重试策略处理。
func compactResponseFailureDetails(account *auth.Account, payload []byte) (int, []byte, streamOutcome, bool, bool) {
	statusCode, body := responseFailedHTTPError(payload)
	outcome := classifyResponseFailedOutcomeForAccount(account, payload)
	unsupported := isUnsupportedTruncationPayload(payload)
	if unsupported {
		outcome = markUnsupportedTruncationOutcome(outcome)
	}
	return statusCode, body, outcome, outcome.deterministicClientError, unsupported
}

// 普通失败沿用上游错误响应；truncation 不支持则走确定性的 400 且禁止所有重试。
func (h *Handler) finishCompactResponseFailure(c *gin.Context, account *auth.Account, affinityKey string, resp *http.Response, payload []byte) {
	statusCode, body := responseFailedHTTPError(payload)
	if isUnsupportedTruncationPayload(payload) {
		h.finishUnsupportedTruncation(c, account, affinityKey, resp, statusCode, body, false)
		return
	}
	blockDeterministicResponseFailureReplay(c, payload)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if h != nil && h.store != nil && account != nil {
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
		h.store.Release(account)
	}
	if c.Request.Context().Err() == nil {
		h.sendFinalUpstreamError(c, statusCode, body)
	}
}

func markUnsupportedTruncationOutcome(outcome streamOutcome) streamOutcome {
	outcome.logStatusCode = http.StatusBadRequest
	outcome.failureKind = upstreamErrorKindUnsupportedTruncation
	outcome.penalize = false
	return outcome
}

func unsupportedTruncationMessage(statusCode int, body []byte) string {
	message := strings.TrimSpace(usageLogErrorMessage(statusCode, body))
	if message == "" || strings.EqualFold(message, http.StatusText(statusCode)) {
		return "The upstream does not support the truncation parameter"
	}
	return message
}

func unsupportedTruncationAPIError(statusCode int, body []byte) *api.APIError {
	return api.NewAPIError(
		api.ErrorCode(upstreamErrorKindUnsupportedTruncation),
		unsupportedTruncationMessage(statusCode, body),
		api.ErrorTypeInvalidRequest,
	)
}

// finishUnsupportedTruncation 释放当前账号并返回确定性的 400；已有业务流时只能停止流，不能伪造新的 HTTP 状态。
func (h *Handler) finishUnsupportedTruncation(c *gin.Context, account *auth.Account, affinityKey string, resp *http.Response, statusCode int, body []byte, wroteAnyBody bool) {
	blockClientRequestReplay(c, clientRequestReplayStopUnsupportedTruncation)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if h != nil && h.store != nil && account != nil {
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
		h.store.Release(account)
	}
	if !wroteAnyBody && c.Request.Context().Err() == nil {
		api.SendErrorWithStatus(c, unsupportedTruncationAPIError(statusCode, body), http.StatusBadRequest)
	}
}
