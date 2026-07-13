package proxy

import (
	"log"
	"net/http"

	"github.com/codex2api/auth"
)

const (
	transportRetryPolicySticky = "sticky"
	transportRetryPolicyHybrid = "hybrid"
)

type transportRetryTracker struct {
	failuresByAccount       map[int64]int
	compactInitialized      bool
	compactInitialAccountID int64
	compactFailures         int
	compactLimit            int
	compactExhausted        bool
}

type sameAccountRetryTarget struct {
	accountID int64
	proxyURL  string
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

func (t *transportRetryTracker) captureCompactInitialAccount(handler *Handler, account *auth.Account, compact bool) {
	if t == nil || t.compactInitialized || !compact || handler == nil || handler.store == nil || account == nil {
		return
	}
	t.compactInitialized = true
	t.compactInitialAccountID = account.ID()
	t.compactLimit = handler.store.CompactSameAccountRetriesForAccount(account)
}

func (t *sameAccountRetryTarget) remember(account *auth.Account, proxyURL string) {
	if t == nil || account == nil {
		return
	}
	t.accountID = account.ID()
	t.proxyURL = proxyURL
}

func (t *sameAccountRetryTarget) take(store *auth.Store, apiKeyID int64, filter auth.AccountFilter) (*auth.Account, string) {
	if t == nil || store == nil || t.accountID == 0 {
		return nil, ""
	}
	accountID := t.accountID
	proxyURL := t.proxyURL
	t.accountID = 0
	t.proxyURL = ""
	return store.TakeAccountForRetryWithFilter(accountID, apiKeyID, filter), proxyURL
}

func sameAccountStreamRetryEligible(_ bool, outcome streamOutcome, wroteAnyBody bool, requestErr, writeErr error) bool {
	return outcome.logStatusCode != http.StatusOK &&
		!wroteAnyBody &&
		requestErr == nil &&
		writeErr == nil
}

func (t *transportRetryTracker) shouldRetryForRequest(handler *Handler, account *auth.Account, compact, eligible, timedOut bool, failureKind string) (bool, int, int) {
	if !compact {
		return t.shouldRetrySameAccount(handler, account, eligible, timedOut, failureKind)
	}
	if t == nil || handler == nil || handler.store == nil || account == nil || !eligible {
		return false, 0, 0
	}
	t.captureCompactInitialAccount(handler, account, true)
	if t.compactExhausted || account.ID() != t.compactInitialAccountID {
		return false, 0, t.compactLimit
	}

	t.compactFailures++
	if t.compactFailures <= t.compactLimit {
		return true, t.compactFailures, t.compactLimit
	}
	t.compactExhausted = true
	return false, t.compactFailures, t.compactLimit
}

func (t *transportRetryTracker) stateMachineAttempt(attempt int, compact bool) int {
	if t == nil || !compact || attempt <= 0 {
		return attempt
	}
	used := t.compactFailures
	if used > t.compactLimit {
		used = t.compactLimit
	}
	if used >= attempt {
		return 0
	}
	return attempt - used
}

func (t *transportRetryTracker) shouldRetrySameAccount(handler *Handler, account *auth.Account, eligible, timedOut bool, _ string) (bool, int, int) {
	if handler == nil || handler.store == nil || account == nil || !eligible || timedOut {
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
		log.Printf("上游错误粘滞同号重试：保留账号 %d 与会话亲和 (attempt %d, endpoint %s)", accountID, attempt, endpoint)
		return
	}
	log.Printf("上游错误混合同号重试：保留账号 %d 与会话亲和 (attempt %d, same_account_failure %d/%d, endpoint %s)", accountID, attempt, failures, limit, endpoint)
}

func logCompactSameAccountRetry(accountID int64, attempt, failures, limit int, endpoint string) {
	log.Printf("compact 首账号同号重试：保留账号 %d 与会话亲和 (attempt %d, compact_same_account_failure %d/%d, endpoint %s)", accountID, attempt, failures, limit, endpoint)
}
