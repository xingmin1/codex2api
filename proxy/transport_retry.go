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
	failuresByAccount map[int64]int
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

func sameAccountStreamRetryEligible(compact bool, outcome streamOutcome, wroteAnyBody bool, requestErr, writeErr error) bool {
	return !compact &&
		outcome.logStatusCode != http.StatusOK &&
		!wroteAnyBody &&
		requestErr == nil &&
		writeErr == nil
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
