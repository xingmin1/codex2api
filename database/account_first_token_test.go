package database

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func openFirstTokenTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAccountFirstTokenStatsUsesTimeWindowsAndLatestFiveSamples(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	accountID, err := db.InsertAccount(ctx, "统计账号", "rt-stats", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	otherAccountID, err := db.InsertAccount(ctx, "其他账号", "rt-other", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}

	samples := []AccountFirstTokenSample{
		{AccountID: accountID, Source: FirstTokenSourceNormal, Model: "gpt-a", FirstTokenMs: 100, CreatedAt: now.Add(-9 * time.Minute)},
		{AccountID: accountID, Source: FirstTokenSourceManualProbe, Model: "gpt-b", FirstTokenMs: 200, CreatedAt: now.Add(-8 * time.Minute)},
		{AccountID: accountID, Source: FirstTokenSourceAutoProbe, Model: "gpt-c", FirstTokenMs: 300, CreatedAt: now.Add(-7 * time.Minute)},
		{AccountID: accountID, Source: FirstTokenSourceNormal, Model: "gpt-d", FirstTokenMs: 400, CreatedAt: now.Add(-6 * time.Minute)},
		{AccountID: accountID, Source: FirstTokenSourceManualProbe, Model: "gpt-e", FirstTokenMs: 500, CreatedAt: now.Add(-5 * time.Minute)},
		{AccountID: accountID, Source: FirstTokenSourceAutoProbe, Model: "gpt-f", FirstTokenMs: 600, CreatedAt: now.Add(-4 * time.Minute)},
		{AccountID: accountID, Source: FirstTokenSourceNormal, Model: "gpt-g", FirstTokenMs: 900, CreatedAt: now.Add(-20 * time.Minute)},
		{AccountID: accountID, Source: FirstTokenSourceNormal, Model: "gpt-old", FirstTokenMs: 2000, CreatedAt: now.Add(-61 * time.Minute)},
		{AccountID: otherAccountID, Source: FirstTokenSourceNormal, Model: "gpt-other", FirstTokenMs: 777, CreatedAt: now.Add(-2 * time.Minute)},
	}
	for minute := 21; minute <= 41; minute++ {
		samples = append(samples, AccountFirstTokenSample{
			AccountID:    accountID,
			Source:       FirstTokenSourceNormal,
			Model:        "gpt-long-window",
			FirstTokenMs: 10,
			CreatedAt:    now.Add(-time.Duration(minute) * time.Minute),
		})
	}
	if err := db.insertAccountFirstTokenBatch(ctx, samples); err != nil {
		t.Fatalf("insertAccountFirstTokenBatch 返回错误: %v", err)
	}

	allStats, err := db.GetAccountsFirstTokenStats(ctx, now)
	if err != nil {
		t.Fatalf("GetAccountsFirstTokenStats 返回错误: %v", err)
	}
	stats := allStats[accountID]
	if stats.Short.SampleCount != 5 || stats.Short.MaximumMs != 600 || math.Abs(stats.Short.AverageMs-400) > 0.001 {
		t.Fatalf("短窗口统计 = %#v, want count=5 max=600 avg=400", stats.Short)
	}
	if stats.Short.SampleLimit != 5 || stats.Short.WindowSeconds != 600 {
		t.Fatalf("短窗口元数据 = %#v, want limit=5 window=600", stats.Short)
	}
	if stats.Long.SampleCount != 28 || stats.Long.MaximumMs != 900 || math.Abs(stats.Long.AverageMs-(3210.0/28.0)) > 0.001 {
		t.Fatalf("长窗口统计 = %#v, want count=28 max=900 avg=%v", stats.Long, 3210.0/28.0)
	}
	if stats.Long.SampleLimit != 0 || stats.Long.WindowSeconds != 3600 {
		t.Fatalf("长窗口元数据 = %#v, want unlimited window=3600", stats.Long)
	}
	if stats.Short.LastSampleAt == nil || !stats.Short.LastSampleAt.Equal(now.Add(-4*time.Minute)) {
		t.Fatalf("短窗口最近样本 = %v, want %v", stats.Short.LastSampleAt, now.Add(-4*time.Minute))
	}
	if got := allStats[otherAccountID].Long.SampleCount; got != 1 {
		t.Fatalf("其他账号长窗口样本数 = %d, want 1", got)
	}
}

func TestInsertAccountFirstTokenSampleValidatesAndNormalizes(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()

	if err := db.InsertAccountFirstTokenSample(ctx, &AccountFirstTokenSample{
		AccountID:    1,
		Source:       " AUTO_PROBE ",
		Model:        "gpt-test",
		FirstTokenMs: 123,
	}); err != nil {
		t.Fatalf("合法样本返回错误: %v", err)
	}
	if len(db.firstTokenBuf) != 1 || db.firstTokenBuf[0].Source != FirstTokenSourceAutoProbe {
		t.Fatalf("缓冲样本 = %#v, want normalized auto_probe", db.firstTokenBuf)
	}
	if err := db.InsertAccountFirstTokenSample(ctx, &AccountFirstTokenSample{AccountID: 1, Source: "wham", FirstTokenMs: 123}); err == nil {
		t.Fatal("非法来源应返回错误")
	}
	if err := db.InsertAccountFirstTokenSample(ctx, &AccountFirstTokenSample{AccountID: 1, Source: FirstTokenSourceNormal, FirstTokenMs: 0}); err != nil {
		t.Fatalf("无效耗时应静默忽略: %v", err)
	}
	if len(db.firstTokenBuf) != 1 {
		t.Fatalf("无效样本后缓冲长度 = %d, want 1", len(db.firstTokenBuf))
	}
}

func TestAccountManualScoreBonusPersistsReplacesAndExpires(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	activeID, err := db.InsertAccount(ctx, "活跃加分", "rt-active", "")
	if err != nil {
		t.Fatalf("InsertAccount(active) 返回错误: %v", err)
	}
	expiredID, err := db.InsertAccount(ctx, "过期加分", "rt-expired", "")
	if err != nil {
		t.Fatalf("InsertAccount(expired) 返回错误: %v", err)
	}

	firstUntil := now.Add(30 * time.Minute)
	if err := db.UpdateAccountManualScoreBonus(ctx, activeID, 80, firstUntil); err != nil {
		t.Fatalf("首次设置返回错误: %v", err)
	}
	replacementUntil := now.Add(time.Hour)
	if err := db.UpdateAccountManualScoreBonus(ctx, activeID, -400, replacementUntil); err != nil {
		t.Fatalf("替换设置返回错误: %v", err)
	}
	if err := db.UpdateAccountManualScoreBonus(ctx, expiredID, 100, now.Add(-time.Minute)); err != nil {
		t.Fatalf("设置过期加分返回错误: %v", err)
	}
	if err := db.ClearExpiredAccountManualScoreBonuses(ctx, now); err != nil {
		t.Fatalf("ClearExpiredAccountManualScoreBonuses 返回错误: %v", err)
	}

	assertBonus := func(accountID int64, wantBonus int64, wantUntil bool) {
		t.Helper()
		var bonus int64
		var untilRaw interface{}
		if err := db.conn.QueryRowContext(ctx, `SELECT manual_score_bonus, manual_score_bonus_until FROM accounts WHERE id = $1`, accountID).Scan(&bonus, &untilRaw); err != nil {
			t.Fatalf("查询账号 %d 加分返回错误: %v", accountID, err)
		}
		parsed, parseErr := parseDBNullTimeValue(untilRaw)
		if parseErr != nil {
			t.Fatalf("解析账号 %d 到期时间返回错误: %v", accountID, parseErr)
		}
		if bonus != wantBonus || parsed.Valid != wantUntil {
			t.Fatalf("账号 %d 加分=(%d,%v), want (%d,%v)", accountID, bonus, parsed.Valid, wantBonus, wantUntil)
		}
	}
	assertBonus(activeID, -400, true)
	assertBonus(expiredID, 0, false)

	if err := db.UpdateAccountManualScoreBonus(ctx, activeID, 0, time.Time{}); err != nil {
		t.Fatalf("主动清除返回错误: %v", err)
	}
	assertBonus(activeID, 0, false)
	if err := db.UpdateAccountManualScoreBonus(ctx, 999999, 1, now.Add(time.Hour)); err != sql.ErrNoRows {
		t.Fatalf("未知账号错误 = %v, want sql.ErrNoRows", err)
	}
}
