package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
)

// WhamUsageURL 是 ChatGPT 后端用量查询端点。
// 该端点返回结构化 JSON（不消耗任何额度），可用于零成本获取账号 5h/7d 用量。
const WhamUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// WhamResetCreditsConsumeURL 是「消耗 1 次主动重置次数、立即重置额度」的端点。
const WhamResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

// whamURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var whamURLForTest = ""

// whamConsumeURLForTest 允许测试替换重置端点 URL。生产代码不要赋值。
var whamConsumeURLForTest = ""

// WhamUsage 是 /backend-api/wham/usage 的响应结构。
type WhamUsage struct {
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	PlanType  string `json:"plan_type"`

	RateLimit struct {
		Allowed         bool             `json:"allowed"`
		LimitReached    bool             `json:"limit_reached"`
		PrimaryWindow   *WhamUsageWindow `json:"primary_window"`
		SecondaryWindow *WhamUsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`

	Credits *struct {
		HasCredits          bool   `json:"has_credits"`
		Unlimited           bool   `json:"unlimited"`
		OverageLimitReached bool   `json:"overage_limit_reached"`
		Balance             string `json:"balance"`
		ApproxLocalMessages []int  `json:"approx_local_messages"`
		ApproxCloudMessages []int  `json:"approx_cloud_messages"`
	} `json:"credits,omitempty"`

	SpendControl *struct {
		Reached         bool        `json:"reached"`
		IndividualLimit interface{} `json:"individual_limit"`
	} `json:"spend_control,omitempty"`

	// RateLimitResetCredits 是账号在 OpenAI 官方那边剩余的「主动重置次数」。
	// available_count > 0 时可调用 wham/rate-limit-reset-credits/consume 立即重置额度。
	RateLimitResetCredits *struct {
		AvailableCount int `json:"available_count"`
	} `json:"rate_limit_reset_credits,omitempty"`
}

// WhamUsageWindow 是单个限流窗口（primary=5h，secondary=7d）。
type WhamUsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// QueryWhamUsage 调用 /backend-api/wham/usage 获取账号当前用量。
// 该调用不消耗任何 token 额度——比发送最小 /responses 请求更便宜。
func QueryWhamUsage(ctx context.Context, account *auth.Account, proxyURL string) (*WhamUsage, *http.Response, error) {
	url := WhamUsageURL
	if whamURLForTest != "" {
		url = whamURLForTest
	}
	return queryWhamUsageWithURL(ctx, account, proxyURL, url)
}

func queryWhamUsageWithURL(ctx context.Context, account *auth.Account, proxyURL, url string) (*WhamUsage, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build wham request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", latestCodexCLIUserAgentPrefix)
	req.Header.Set("Originator", Originator)
	if accountID := strings.TrimSpace(account.AccountID); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	client := &http.Client{Transport: newCodexStandardTransport(proxyURL)}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("wham request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 调用方需要根据状态码触发刷新 / 冷却；返回 resp 让上层处理 body。
		return nil, resp, fmt.Errorf("wham returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read wham response: %w", err)
	}

	var usage WhamUsage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, resp, fmt.Errorf("parse wham response: %w", err)
	}
	return &usage, resp, nil
}

// ConsumeResetCredit 消耗账号 1 次「主动重置次数」以立即重置额度。
// 向 /backend-api/wham/rate-limit-reset-credits/consume 发送 POST，body 携带一个
// 随机幂等键 redeem_request_id。成功（2xx）返回 nil；非 2xx 返回带状态码的 resp，
// 由调用方据此触发刷新 / 冷却 / 错误提示。
func ConsumeResetCredit(ctx context.Context, account *auth.Account, proxyURL string) (*http.Response, error) {
	url := WhamResetCreditsConsumeURL
	if whamConsumeURLForTest != "" {
		url = whamConsumeURLForTest
	}
	return consumeResetCreditWithURL(ctx, account, proxyURL, url)
}

func consumeResetCreditWithURL(ctx context.Context, account *auth.Account, proxyURL, url string) (*http.Response, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}

	payload, _ := json.Marshal(map[string]string{"redeem_request_id": uuid.New().String()})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build reset-credit request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", latestCodexCLIUserAgentPrefix)
	req.Header.Set("Originator", Originator)
	if accountID := strings.TrimSpace(account.AccountID); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	client := &http.Client{Transport: newCodexStandardTransport(proxyURL)}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reset-credit request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 保留 resp 让调用方读取 body 并据状态码处理（401 刷新、429 等）。
		return resp, fmt.Errorf("reset-credit returned status %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	return resp, nil
}

// ApplyWhamUsage 将 /wham/usage 返回的数据写入账号 state + 持久化。
// 行为与 SyncCodexUsageState（处理 /responses 响应头时）保持一致：
//   - plan_type 同步到内存 + DB
//   - 窗口按 limit_window_seconds 精确分类（18000s→5h，604800s→7d）；
//     未精确匹配时优先按 free plan / 长 reset 识别 7d，再按字段位置兜底。
//   - 5h 窗口写入 SetUsageSnapshot5h
//   - 7d 窗口走 PersistUsageSnapshot
//   - premium 5h 用尽时走 MarkPremium5hRateLimited
func ApplyWhamUsage(store *auth.Store, account *auth.Account, usage *WhamUsage) CodexUsageSyncResult {
	result := CodexUsageSyncResult{}
	if account == nil || usage == nil {
		return result
	}

	if store != nil && usage.PlanType != "" {
		store.UpdateAccountPlanType(account, usage.PlanType)
	}

	// 记录「主动重置次数」（OpenAI 官方剩余的手动重置额度次数）。
	if usage.RateLimitResetCredits != nil {
		account.SetRateLimitResetCredits(usage.RateLimitResetCredits.AvailableCount)
	}

	now := time.Now()

	// 记录本次成功的 wham 探针时间。「主动重置次数」只能由 wham 刷新，
	// 用它独立判断重置次数是否过期（见 auth.Account.NeedsUsageProbe）。
	account.MarkResetCreditsProbed(now)

	w5h, w7d := pickClassifiedWhamWindows(usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow, usage.PlanType, now)

	if w5h != nil {
		resetAt := whamWindowResetAt(w5h, now)
		account.SetUsageSnapshot5hAt(w5h.UsedPercent, resetAt, now)
		result.UsagePct5h = w5h.UsedPercent
		result.Reset5hAt = resetAt
		result.HasUsage5h = true
		result.Used5hHeaders = true
	}

	if w7d != nil {
		resetAt := whamWindowResetAt(w7d, now)
		account.SetReset7dAt(resetAt)
		result.UsagePct7d = w7d.UsedPercent
		result.HasUsage7d = true
		if store != nil {
			store.PersistUsageSnapshot(account, w7d.UsedPercent)
			if result.UsagePct7d >= 100 {
				result.Usage7dRateLimited = store.MarkUsage7dRateLimited(account)
			}
		}
	} else if result.Used5hHeaders && store != nil {
		// 只有 5h 数据时，单独持久化 5h 快照
		store.PersistUsageSnapshot5hOnly(account)
		result.Persisted5hOnly = true
	}

	// premium 5h 限流标记
	if result.Used5hHeaders && account.IsPremium5hPlan() && result.HasUsage5h && result.UsagePct5h >= 100 {
		if store != nil {
			store.MarkPremium5hRateLimited(account, result.Reset5hAt)
		}
		result.Premium5hRateLimited = true
	}

	return result
}

// 已知窗口长度（秒）。和 CPA-Manager src/utils/quota/codexQuota.ts 保持一致。
const (
	whamWindow5hSeconds int64 = 18_000
	whamWindow7dSeconds int64 = 604_800
)

// pickClassifiedWhamWindows 把 primary/secondary 两个窗口归类到 5h/7d 槽位。
//
// 策略对齐 CPA-Manager 的 pickClassifiedWindows：
//  1. 第一遍：按 limit_window_seconds 精确匹配（18000→5h，604800→7d）
//  2. 第二遍：把 free plan 或 reset 明显超过 5h 的未知窗口归到 7d
//  3. 最后按字段位置兜底（primary→5h、secondary→7d），只填补空槽位
//
// 这样可同时正确处理：
//   - plus：primary=18000(5h) + secondary=604800(7d)
//   - free：primary=604800(7d) + secondary=null（issue #168 报告的场景）
//   - 字段位置颠倒
//   - 未来出现的未知窗口长度（先防止 free/长周期误进 5h，再避免数据完全丢失）
func pickClassifiedWhamWindows(primary, secondary *WhamUsageWindow, planType string, now time.Time) (w5h, w7d *WhamUsageWindow) {
	for _, w := range []*WhamUsageWindow{primary, secondary} {
		if w == nil {
			continue
		}
		switch w.LimitWindowSeconds {
		case whamWindow5hSeconds:
			if w5h == nil {
				w5h = w
			}
		case whamWindow7dSeconds:
			if w7d == nil {
				w7d = w
			}
		}
	}

	if w5h == nil && primary != nil && primary != w7d {
		if shouldTreatUnknownWhamWindowAs7d(primary, planType, now) && w7d == nil {
			w7d = primary
		} else {
			w5h = primary
		}
	}
	if w7d == nil && secondary != nil && secondary != w5h {
		if shouldTreatUnknownWhamWindowAs7d(secondary, planType, now) {
			w7d = secondary
		} else if w5h == nil {
			w5h = secondary
		} else {
			w7d = secondary
		}
	}
	return w5h, w7d
}

func shouldTreatUnknownWhamWindowAs7d(w *WhamUsageWindow, planType string, now time.Time) bool {
	if w == nil {
		return false
	}
	if auth.NormalizePlanType(planType) == "free" {
		return true
	}
	if w.ResetAfterSeconds > whamWindow5hSeconds {
		return true
	}
	if w.ResetAt > 0 && !now.IsZero() && time.Unix(w.ResetAt, 0).After(now.Add(time.Duration(whamWindow5hSeconds)*time.Second)) {
		return true
	}
	return false
}

func whamWindowResetAt(w *WhamUsageWindow, now time.Time) time.Time {
	if w == nil {
		return time.Time{}
	}
	// reset_at 是 unix 时间戳（秒），优先使用；缺失时 fallback 到 reset_after_seconds
	if w.ResetAt > 0 {
		return time.Unix(w.ResetAt, 0)
	}
	if w.ResetAfterSeconds > 0 {
		return now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
	}
	return time.Time{}
}
