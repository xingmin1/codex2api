package proxy

import (
	"log"

	"github.com/codex2api/auth"
)

const (
	transportRetryPolicySticky = "sticky"
	transportRetryPolicyHybrid = "hybrid"
)

type transportRetryTracker struct {
	failuresByAccount map[int64]int
}

func newTransportRetryTracker() *transportRetryTracker {
	return &transportRetryTracker{failuresByAccount: make(map[int64]int)}
}

func (t *transportRetryTracker) reset() {
	if t == nil {
		return
	}
	t.failuresByAccount = make(map[int64]int)
}

func (t *transportRetryTracker) shouldRetrySameAccount(handler *Handler, account *auth.Account, retryable, timedOut bool, failureKind string) (bool, int, int) {
	if handler == nil || handler.store == nil || account == nil || !retryable || timedOut || failureKind == "" {
		return false, 0, 0
	}

	switch handler.store.GetTransportRetryPolicy() {
	case transportRetryPolicySticky:
		return true, 0, -1
	case transportRetryPolicyHybrid:
		limit := handler.store.TransportSameAccountRetriesForAccount(account)
		failures := t.failuresByAccount[account.ID()] + 1
		t.failuresByAccount[account.ID()] = failures
		return failures <= limit, failures, limit
	default:
		return false, 0, 0
	}
}

func logTransportSameAccountRetry(accountID int64, attempt, failures, limit int, endpoint string) {
	if limit < 0 {
		log.Printf("传输错误粘滞同号重试：保留账号 %d 与会话亲和 (attempt %d, endpoint %s)", accountID, attempt, endpoint)
		return
	}
	log.Printf("传输错误混合同号重试：保留账号 %d 与会话亲和 (attempt %d, same_account_failure %d/%d, endpoint %s)", accountID, attempt, failures, limit, endpoint)
}
