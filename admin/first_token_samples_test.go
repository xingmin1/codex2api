package admin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestAccountFirstTokenObserverRecordsOnlyFirstContentPayload(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	ctx := context.Background()
	accountID, err := db.InsertAccount(ctx, "探测账号", "rt-probe", "")
	if err != nil {
		_ = db.Close()
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}

	handler := &Handler{db: db}
	account := &auth.Account{DBID: accountID}
	observe := handler.newAccountFirstTokenObserver(
		account,
		database.FirstTokenSourceManualProbe,
		"gpt-probe",
		time.Now().Add(-25*time.Millisecond),
	)
	observe([]byte(`{"type":"response.created"}`))
	observe([]byte(`{"type":"response.in_progress"}`))
	observe([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	observe([]byte(`{"type":"response.output_text.delta","delta":"again"}`))

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
	accountStats, ok := stats[accountID]
	if !ok {
		t.Fatal("未找到探测账号的首字统计")
	}
	if accountStats.Short.SampleCount != 1 || accountStats.Long.SampleCount != 1 {
		t.Fatalf("首字样本数 = short:%d long:%d, want 1/1", accountStats.Short.SampleCount, accountStats.Long.SampleCount)
	}
	if accountStats.Short.AverageMs < 1 {
		t.Fatalf("首字耗时 = %v, want >= 1", accountStats.Short.AverageMs)
	}
}

func TestAccountFirstTokenObserverIgnoresMissingDependencies(t *testing.T) {
	var nilHandler *Handler
	nilHandler.newAccountFirstTokenObserver(nil, database.FirstTokenSourceAutoProbe, "", time.Now())([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))

	handler := &Handler{}
	handler.newAccountFirstTokenObserver(&auth.Account{DBID: 1}, database.FirstTokenSourceAutoProbe, "", time.Now())([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
}
