package admin

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// ResetCredits 消耗账号 1 次「主动重置次数」以立即重置 Codex 额度。
// POST /api/accounts/:id/reset-credits
//
// 流程：找到账号 → 调用官方 wham/rate-limit-reset-credits/consume → 重新探测用量
// 刷新剩余次数 → 返回最新次数。次数为 0 时直接拒绝，避免无谓的上游请求。
func (h *Handler) ResetCredits(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}

	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "账号没有可用的 access token，请先刷新账号")
		return
	}

	// 本地已知次数为 0 时直接拒绝（与前端按钮门槛一致），减少无效上游调用。
	if count, ok := account.GetRateLimitResetCredits(); ok && count <= 0 {
		writeError(c, http.StatusConflict, "该账号没有可用的主动重置次数")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	proxyURL := h.store.ResolveProxyForAccount(account)
	resp, err := proxy.ConsumeResetCredit(ctx, account, proxyURL)
	if resp != nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				writeError(c, http.StatusBadGateway, "上游鉴权失败（401），请先刷新账号后重试")
			case http.StatusTooManyRequests:
				writeError(c, http.StatusBadGateway, "上游限流（429），请稍后重试")
			default:
				writeError(c, http.StatusBadGateway, "重置失败："+upstreamResetErrorMessage(resp.StatusCode, body))
			}
			return
		}
	}
	if err != nil && resp == nil {
		writeError(c, http.StatusBadGateway, "重置请求失败："+err.Error())
		return
	}

	// 重置成功后重新探测用量，刷新剩余次数（available_count 会 -1）。
	if probeErr := h.ProbeUsageSnapshot(ctx, account); probeErr != nil {
		// 探测失败不影响重置结果，仅记录；次数返回最近已知值。
		log.Printf("[账号 %d] 重置额度成功，但刷新用量失败: %v", account.DBID, probeErr)
	}

	payload := gin.H{"message": "已重置额度"}
	if count, ok := account.GetRateLimitResetCredits(); ok {
		payload["rate_limit_reset_credits"] = count
	}
	c.JSON(http.StatusOK, payload)
}

// findAccountByID 按数据库 ID 在运行时号池中查找账号；找不到返回 nil。
func (h *Handler) findAccountByID(id int64) *auth.Account {
	if h.store == nil {
		return nil
	}
	for _, acc := range h.store.Accounts() {
		if acc != nil && acc.DBID == id {
			return acc
		}
	}
	return nil
}

// upstreamResetErrorMessage 从上游响应里提取简短错误信息用于回传。
func upstreamResetErrorMessage(statusCode int, body []byte) string {
	if msg := truncate(string(body), 200); msg != "" {
		return msg
	}
	return "上游返回状态 " + strconv.Itoa(statusCode)
}
