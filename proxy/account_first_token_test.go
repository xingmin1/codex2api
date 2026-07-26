package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestLogUsageForRequestRecordsFirstTokenWhenUsageLogsAreOff(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	ctx := context.Background()
	accountID, err := db.InsertAccount(ctx, "正常请求账号", "rt-normal", "")
	if err != nil {
		_ = db.Close()
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}

	db.SetUsageLogConfig(database.UsageLogModeOff, 200, 5)
	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	handler.logUsageForRequest(requestContext, &database.UsageLogInput{
		AccountID:      accountID,
		Model:          "requested-model",
		EffectiveModel: "effective-model",
		FirstTokenMs:   321,
	})
	handler.logUsageForRequest(requestContext, &database.UsageLogInput{
		AccountID:    accountID,
		Model:        "ignored-no-first-token",
		FirstTokenMs: 0,
	})

	if err := db.Close(); err != nil {
		t.Fatalf("关闭并刷新数据库返回错误: %v", err)
	}
	db, err = database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("重新打开数据库返回错误: %v", err)
	}
	defer db.Close()
	stats, err := db.GetAccountsFirstTokenStats(ctx, time.Now())
	if err != nil {
		t.Fatalf("GetAccountsFirstTokenStats 返回错误: %v", err)
	}
	accountStats := stats[accountID]
	if accountStats.Short.SampleCount != 1 || accountStats.Short.AverageMs != 321 {
		t.Fatalf("正常请求首字统计 = %#v, want count=1 avg=321", accountStats.Short)
	}
}
