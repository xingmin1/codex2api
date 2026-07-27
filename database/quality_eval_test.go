package database

import (
	"context"
	"testing"
	"time"
)

func TestQualityEvalBatchPersistsSamplesAndDeduplicatesScheduledHour(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 14, 0, 0, 0, time.UTC)
	batch := QualityEvalBatch{
		AccountID: 7, TriggerSource: QualityEvalTriggerAuto, TestKind: QualityEvalKindFull,
		ScheduledHour: &now, Model: "gpt-5.6-sol", ReasoningEffort: "max",
		JuiceRequested: 2, CandyRequested: 3, StartedAt: now,
	}
	batchID, created, err := db.CreateQualityEvalBatch(ctx, batch)
	if err != nil || !created || batchID <= 0 {
		t.Fatalf("CreateQualityEvalBatch = (%d,%v,%v), want created", batchID, created, err)
	}
	if duplicateID, duplicateCreated, err := db.CreateQualityEvalBatch(ctx, batch); err != nil || duplicateCreated || duplicateID != 0 {
		t.Fatalf("重复小时桶 = (%d,%v,%v), want deduplicated", duplicateID, duplicateCreated, err)
	}

	sample := QualityEvalSample{
		BatchID: batchID, AccountID: 7, TestKind: QualityEvalKindJuice, SampleIndex: 1,
		AttemptCount: 2, Model: "gpt-5.6-sol", ReasoningEffort: "max",
		AttemptAnswers: []string{"none", "1.2500"}, RawAnswer: "1.2500", ParsedAnswer: "1.2500",
		Graded: true, Correct: true, InputTokens: 10, OutputTokens: 2, ReasoningTokens: 8,
		FirstTokenMs: 123, DurationMs: 456, CreatedAt: now.Add(time.Minute),
	}
	if err := db.InsertQualityEvalSample(ctx, sample); err != nil {
		t.Fatalf("InsertQualityEvalSample 返回错误: %v", err)
	}
	batch.ID = batchID
	batch.Status = QualityEvalStatusIncomplete
	batch.JuiceGraded = 1
	batch.JuiceCorrect = 1
	finishedAt := now.Add(2 * time.Minute)
	batch.FinishedAt = &finishedAt
	if err := db.CompleteQualityEvalBatch(ctx, batch); err != nil {
		t.Fatalf("CompleteQualityEvalBatch 返回错误: %v", err)
	}

	history, err := db.ListAccountQualityEvalBatches(ctx, 7, 20)
	if err != nil || len(history) != 1 || len(history[0].Samples) != 1 {
		t.Fatalf("历史 = %#v, err=%v", history, err)
	}
	got := history[0].Samples[0]
	if got.ParsedAnswer != "1.2500" || len(got.AttemptAnswers) != 2 || got.AttemptAnswers[0] != "none" {
		t.Fatalf("样本原始字符串未完整保留: %#v", got)
	}
	latest, err := db.GetLatestQualityEvalBatches(ctx)
	if err != nil || latest[7].Status != QualityEvalStatusIncomplete {
		t.Fatalf("最新摘要 = %#v, err=%v", latest, err)
	}
}

func TestQualityEvalManualBatchesAllowRepeatedRuns(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	batch := QualityEvalBatch{
		AccountID: 8, TriggerSource: QualityEvalTriggerManual, TestKind: QualityEvalKindCandy,
		Model: "gpt-5.6-sol", ReasoningEffort: "max", CandyRequested: 3,
	}
	for run := 0; run < 2; run++ {
		if id, created, err := db.CreateQualityEvalBatch(ctx, batch); err != nil || !created || id <= 0 {
			t.Fatalf("手动批次 %d = (%d,%v,%v), want created", run, id, created, err)
		}
	}
}

func TestQualityEvalSchedulerLeaseAndBucketClaimAreCrossInstanceExclusive(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 14, 0, 0, 0, time.UTC)
	acquired, err := db.TryAcquireQualityEvalSchedulerLease(ctx, "instance-a", now, now.Add(5*time.Minute))
	if err != nil || !acquired {
		t.Fatalf("instance-a 取得租约 = %v, err=%v", acquired, err)
	}
	if acquired, err := db.TryAcquireQualityEvalSchedulerLease(ctx, "instance-b", now.Add(time.Minute), now.Add(6*time.Minute)); err != nil || acquired {
		t.Fatalf("未过期租约被 instance-b 取得 = %v, err=%v", acquired, err)
	}
	if renewed, err := db.RenewQualityEvalSchedulerLease(ctx, "instance-a", now.Add(10*time.Minute)); err != nil || !renewed {
		t.Fatalf("instance-a 续租 = %v, err=%v", renewed, err)
	}
	if renewed, err := db.RenewQualityEvalSchedulerLease(ctx, "instance-b", now.Add(10*time.Minute)); err != nil || renewed {
		t.Fatalf("非持有者续租 = %v, err=%v", renewed, err)
	}
	if err := db.ReleaseQualityEvalSchedulerLease(ctx, "instance-a", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("释放租约返回错误: %v", err)
	}
	if acquired, err := db.TryAcquireQualityEvalSchedulerLease(ctx, "instance-b", now.Add(2*time.Minute), now.Add(7*time.Minute)); err != nil || !acquired {
		t.Fatalf("释放后 instance-b 取得租约 = %v, err=%v", acquired, err)
	}

	if claimed, err := db.TryCreateQualityEvalScheduleRun(ctx, now); err != nil || !claimed {
		t.Fatalf("首次声明调度桶 = %v, err=%v", claimed, err)
	}
	if claimed, err := db.TryCreateQualityEvalScheduleRun(ctx, now); err != nil || claimed {
		t.Fatalf("重复声明调度桶 = %v, err=%v", claimed, err)
	}
	if err := db.CompleteQualityEvalScheduleRun(ctx, now, QualityEvalStatusNormal); err != nil {
		t.Fatalf("完成调度桶返回错误: %v", err)
	}
}

func TestMarkInterruptedQualityEvalBatchesLeavesRecentOtherInstanceRunAlone(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 14, 0, 0, 0, time.UTC)
	oldBatch := QualityEvalBatch{AccountID: 31, TriggerSource: QualityEvalTriggerManual, TestKind: QualityEvalKindCandy, Model: "gpt-5.6-sol", ReasoningEffort: "max", StartedAt: now.Add(-time.Hour)}
	recentBatch := QualityEvalBatch{AccountID: 32, TriggerSource: QualityEvalTriggerManual, TestKind: QualityEvalKindCandy, Model: "gpt-5.6-sol", ReasoningEffort: "max", StartedAt: now.Add(-time.Minute)}
	if _, created, err := db.CreateQualityEvalBatch(ctx, oldBatch); err != nil || !created {
		t.Fatalf("创建旧批次失败: created=%v err=%v", created, err)
	}
	if _, created, err := db.CreateQualityEvalBatch(ctx, recentBatch); err != nil || !created {
		t.Fatalf("创建新批次失败: created=%v err=%v", created, err)
	}
	if err := db.MarkInterruptedQualityEvalBatches(ctx, now.Add(-45*time.Minute)); err != nil {
		t.Fatalf("MarkInterruptedQualityEvalBatches 返回错误: %v", err)
	}
	latest, err := db.GetLatestQualityEvalBatches(ctx)
	if err != nil {
		t.Fatalf("GetLatestQualityEvalBatches 返回错误: %v", err)
	}
	if latest[31].Status != QualityEvalStatusIncomplete || latest[32].Status != QualityEvalStatusRunning {
		t.Fatalf("清理后状态 = old:%s recent:%s", latest[31].Status, latest[32].Status)
	}
}

func TestQualityEvalConfigDefaultsAndNormalizes(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	defaults, err := db.GetQualityEvalConfig(ctx)
	if err != nil || defaults.AutoEnabled || defaults.IntervalMinutes != 60 || defaults.LookbackHours != 5 || defaults.TopAccounts != 5 || defaults.MinRequests != 50 || defaults.BatchConcurrency != 1 {
		t.Fatalf("默认配置 = %#v, err=%v", defaults, err)
	}
	saved, err := db.SaveQualityEvalConfig(ctx, QualityEvalConfig{
		AutoEnabled: true, IntervalMinutes: 1, LookbackHours: 999, TopAccounts: 99,
		MinRequests: 0, BatchConcurrency: 99,
	})
	if err != nil {
		t.Fatalf("SaveQualityEvalConfig 返回错误: %v", err)
	}
	if !saved.AutoEnabled || saved.IntervalMinutes != 60 || saved.LookbackHours != 168 || saved.TopAccounts != 20 || saved.MinRequests != 1 || saved.BatchConcurrency != 5 {
		t.Fatalf("归一化配置 = %#v", saved)
	}
	loaded, err := db.GetQualityEvalConfig(ctx)
	if err != nil || loaded != saved {
		t.Fatalf("重载配置 = %#v, want %#v, err=%v", loaded, saved, err)
	}
}

func TestQualityEvalCandidatesCountOnlyFirstAttemptsAndExclude499(t *testing.T) {
	db := openFirstTokenTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 14, 0, 0, 0, time.UTC)
	for _, accountID := range []int64{10, 20} {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO accounts (id, name, credentials, status) VALUES ($1, $2, '{}', 'active')`, accountID, "候选账号"); err != nil {
			t.Fatalf("插入候选账号返回错误: %v", err)
		}
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO accounts (id, name, credentials, status) VALUES (30, '已删除账号', '{}', 'deleted')`); err != nil {
		t.Fatalf("插入已删除账号返回错误: %v", err)
	}
	insert := func(accountID int64, status, retry, attempt int, createdAt time.Time) {
		t.Helper()
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO usage_logs (account_id, status_code, is_retry_attempt, attempt_index, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, accountID, status, retry, attempt, db.timeArg(createdAt))
		if err != nil {
			t.Fatalf("插入用量日志返回错误: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		insert(10, 200, 0, 1, now.Add(-time.Duration(i+1)*time.Minute))
	}
	insert(10, 200, 0, 0, now.Add(-4*time.Minute)) // 兼容字段引入前的首轮日志。
	for i := 0; i < 3; i++ {
		insert(20, 500, 0, 1, now.Add(-time.Duration(i+1)*time.Minute))
	}
	for i := 0; i < 5; i++ {
		insert(30, 200, 0, 1, now.Add(-time.Duration(i+1)*time.Minute))
	}
	insert(20, 200, 1, 1, now.Add(-time.Minute))
	insert(20, 200, 0, 2, now.Add(-time.Minute))
	insert(20, 499, 0, 1, now.Add(-time.Minute))
	insert(20, 200, 0, 1, now.Add(-6*time.Hour))

	candidates, err := db.GetQualityEvalCandidates(ctx, now.Add(-5*time.Hour), now, 3, 5)
	if err != nil {
		t.Fatalf("GetQualityEvalCandidates 返回错误: %v", err)
	}
	if len(candidates) != 2 || candidates[0].AccountID != 10 || candidates[0].RequestCount != 4 || candidates[1].AccountID != 20 || candidates[1].RequestCount != 3 {
		t.Fatalf("候选 = %#v, want 10:4 then 20:3", candidates)
	}
}
