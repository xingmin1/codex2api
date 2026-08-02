package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newManualScoreBonusTestHandler(t *testing.T) (*Handler, *database.DB, *auth.Store, int64) {
	t.Helper()
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:  2,
		TestConcurrency: 1,
		TestModel:       "gpt-test",
	})
	store.AddAccount(&auth.Account{
		DBID:        accountID,
		AccessToken: "token",
		Status:      auth.StatusReady,
		PlanType:    "free",
	})
	return &Handler{db: db, store: store}, db, store, accountID
}

func invokeManualScoreBonusHandler(
	t *testing.T,
	handler func(*gin.Context),
	method string,
	accountID int64,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(
		method,
		fmt.Sprintf("/api/admin/accounts/%d/manual-score-bonus", accountID),
		bytes.NewBufferString(body),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	return recorder
}

func TestSetAccountManualScoreBonusDefaultsReplacesAndClears(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db, store, accountID := newManualScoreBonusTestHandler(t)

	first := invokeManualScoreBonusHandler(t, handler.SetAccountManualScoreBonus, http.MethodPut, accountID, `{"bonus":40}`)
	if first.Code != http.StatusOK {
		t.Fatalf("首次设置 status = %d, want 200: %s", first.Code, first.Body.String())
	}
	var firstPayload struct {
		Bonus            int64  `json:"manual_score_bonus"`
		Until            string `json:"manual_score_bonus_until"`
		RemainingSeconds int64  `json:"manual_score_bonus_remaining_seconds"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPayload); err != nil {
		t.Fatalf("解析首次设置响应返回错误: %v", err)
	}
	if firstPayload.Bonus != 40 || firstPayload.RemainingSeconds < 1790 || firstPayload.RemainingSeconds > 1800 {
		t.Fatalf("首次设置响应 = %#v, want bonus=40 remaining≈1800", firstPayload)
	}
	if until, err := time.Parse(time.RFC3339, firstPayload.Until); err != nil || time.Until(until) < 29*time.Minute {
		t.Fatalf("默认 30 分钟到期时间 = %q, err=%v", firstPayload.Until, err)
	}

	replacement := invokeManualScoreBonusHandler(t, handler.SetAccountManualScoreBonus, http.MethodPut, accountID, `{"bonus":-400,"duration_seconds":60}`)
	if replacement.Code != http.StatusOK {
		t.Fatalf("替换设置 status = %d, want 200: %s", replacement.Code, replacement.Body.String())
	}
	var replacementPayload struct {
		Bonus            int64 `json:"manual_score_bonus"`
		RemainingSeconds int64 `json:"manual_score_bonus_remaining_seconds"`
	}
	if err := json.Unmarshal(replacement.Body.Bytes(), &replacementPayload); err != nil {
		t.Fatalf("解析替换设置响应返回错误: %v", err)
	}
	if replacementPayload.Bonus != -400 || replacementPayload.RemainingSeconds < 50 || replacementPayload.RemainingSeconds > 60 {
		t.Fatalf("替换设置响应 = %#v, want bonus=-400 remaining≈60", replacementPayload)
	}
	snapshot := store.FindByID(accountID).GetSchedulerDebugSnapshot(2)
	if snapshot.ManualScoreBonus != -400 || snapshot.Breakdown.ManualScoreBonus != -400 {
		t.Fatalf("运行时临时分 = %#v, want -400", snapshot)
	}
	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(rows) != 1 || rows[0].ManualScoreBonus != -400 || !rows[0].ManualScoreBonusUntil.Valid {
		t.Fatalf("数据库临时分 = %#v, want bonus=-400 with expiry", rows)
	}

	cleared := invokeManualScoreBonusHandler(t, handler.SetAccountManualScoreBonus, http.MethodPut, accountID, `{"bonus":0}`)
	if cleared.Code != http.StatusOK {
		t.Fatalf("清除 status = %d, want 200: %s", cleared.Code, cleared.Body.String())
	}
	snapshot = store.FindByID(accountID).GetSchedulerDebugSnapshot(2)
	if snapshot.ManualScoreBonus != 0 || !snapshot.ManualScoreBonusUntil.IsZero() {
		t.Fatalf("清除后运行时加分 = %#v, want zero", snapshot)
	}
	rows, err = db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("清除后 ListActive 返回错误: %v", err)
	}
	if rows[0].ManualScoreBonus != 0 || rows[0].ManualScoreBonusUntil.Valid {
		t.Fatalf("清除后数据库加分 = %#v, want zero without expiry", rows[0])
	}
}

func TestSetAccountManualScoreBonusRejectsInvalidBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _, accountID := newManualScoreBonusTestHandler(t)
	cases := []string{
		`{"bonus":401}`,
		`{"bonus":-401}`,
		`{"bonus":1,"duration_seconds":-1}`,
		`{"bonus":1,"duration_seconds":86401}`,
	}
	for _, body := range cases {
		recorder := invokeManualScoreBonusHandler(t, handler.SetAccountManualScoreBonus, http.MethodPut, accountID, body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status = %d, want 400: %s", body, recorder.Code, recorder.Body.String())
		}
	}
}
