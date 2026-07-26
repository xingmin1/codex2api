package admin

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func (h *Handler) newAccountFirstTokenObserver(account *auth.Account, source, model string, startedAt time.Time) func([]byte) {
	recorded := false
	return func(payload []byte) {
		if recorded || h == nil || h.db == nil || account == nil || !proxy.IsFirstTokenPayload(payload) {
			return
		}
		recorded = true
		firstTokenMs := max(int(time.Since(startedAt).Milliseconds()), 1)
		if err := h.db.InsertAccountFirstTokenSample(context.Background(), &database.AccountFirstTokenSample{
			AccountID:    account.ID(),
			Source:       source,
			Model:        model,
			FirstTokenMs: firstTokenMs,
		}); err != nil {
			log.Printf("记录账号首字样本失败 (account %d, source %s): %v", account.ID(), source, err)
		}
	}
}
