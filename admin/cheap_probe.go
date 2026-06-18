package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

// ProbeCheapAccountReadiness 探测便宜账号是否已恢复可用。
//
// 这个探针只返回成功/失败，不写账号冷却、不写用量耗尽，也不做错误类型特判。
// 成功后的运行时恢复由 auth.Store.RecordCheapProbeSuccess 统一处理。
func (h *Handler) ProbeCheapAccountReadiness(ctx context.Context, account *auth.Account) error {
	if h == nil || account == nil {
		return nil
	}

	testModel, err := h.connectionTestModelForAccount(ctx, account, "")
	if err != nil {
		return err
	}
	payload := buildTestPayload(testModel)

	var resp *http.Response
	if account.IsOpenAIResponsesAPI() {
		resp, err = proxy.ExecuteOpenAIResponsesRequest(ctx, account, payload, h.store.ResolveProxyForAccount(account), nil)
	} else {
		resp, err = proxy.ExecuteRequest(ctx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	return readCheapProbeStream(resp.Body)
}

func readCheapProbeStream(body io.Reader) error {
	gotTerminal := false
	var lastEvent []byte
	readErr := proxy.ReadSSEStream(body, func(data []byte) bool {
		if len(data) == 0 {
			return true
		}
		lastEvent = append(lastEvent[:0], data...)
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "" {
			eventType = gjson.GetBytes(data, "event").String()
		}
		switch eventType {
		case "response.completed":
			gotTerminal = true
			return false
		case "response.failed", "response.incomplete", "error":
			gotTerminal = true
			return false
		default:
			status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
			if status == "completed" {
				gotTerminal = true
				return false
			}
			if status == "failed" || status == "incomplete" {
				gotTerminal = true
				return false
			}
		}
		return true
	})
	if readErr != nil {
		return fmt.Errorf("读取上游流失败: %w", readErr)
	}
	if !gotTerminal {
		return fmt.Errorf("%s", formatMissingTerminalUpstreamError(lastEvent))
	}
	if gjson.GetBytes(lastEvent, "type").String() == "response.completed" ||
		strings.EqualFold(gjson.GetBytes(lastEvent, "response.status").String(), "completed") {
		return nil
	}
	return fmt.Errorf("%s", formatUpstreamTestError(lastEvent, "便宜账号探测未完成"))
}
