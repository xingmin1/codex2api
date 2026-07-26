package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	QualityEvalTriggerManual = "manual"
	QualityEvalTriggerAuto   = "auto"
	QualityEvalKindJuice     = "juice"
	QualityEvalKindCandy     = "candy"
	QualityEvalKindFull      = "full"

	QualityEvalStatusRunning    = "running"
	QualityEvalStatusNormal     = "normal"
	QualityEvalStatusSuspected  = "suspected"
	QualityEvalStatusDegraded   = "degraded"
	QualityEvalStatusIncomplete = "incomplete"
)

// QualityEvalConfig 控制周期质量检测的候选范围与并发。
type QualityEvalConfig struct {
	AutoEnabled      bool `json:"auto_enabled"`
	IntervalMinutes  int  `json:"interval_minutes"`
	LookbackHours    int  `json:"lookback_hours"`
	TopAccounts      int  `json:"top_accounts"`
	MinRequests      int  `json:"min_requests"`
	BatchConcurrency int  `json:"batch_concurrency"`
}

// DefaultQualityEvalConfig 返回默认关闭的质量检测配置。
func DefaultQualityEvalConfig() QualityEvalConfig {
	return QualityEvalConfig{
		IntervalMinutes:  60,
		LookbackHours:    5,
		TopAccounts:      5,
		MinRequests:      50,
		BatchConcurrency: 1,
	}
}

// NormalizeQualityEvalConfig 将配置限制在安全且可运维的范围内。
func NormalizeQualityEvalConfig(config QualityEvalConfig) QualityEvalConfig {
	if config.IntervalMinutes < 60 {
		config.IntervalMinutes = 60
	}
	if config.IntervalMinutes > 24*60 {
		config.IntervalMinutes = 24 * 60
	}
	if config.LookbackHours < 1 {
		config.LookbackHours = 1
	}
	if config.LookbackHours > 24*7 {
		config.LookbackHours = 24 * 7
	}
	if config.TopAccounts < 1 {
		config.TopAccounts = 1
	}
	if config.TopAccounts > 20 {
		config.TopAccounts = 20
	}
	if config.MinRequests < 1 {
		config.MinRequests = 1
	}
	if config.MinRequests > 1000000 {
		config.MinRequests = 1000000
	}
	if config.BatchConcurrency < 1 {
		config.BatchConcurrency = 1
	}
	if config.BatchConcurrency > 5 {
		config.BatchConcurrency = 5
	}
	return config
}

// QualityEvalBatch 是一次账号质量检测批次及其聚合结果。
type QualityEvalBatch struct {
	ID              int64               `json:"id"`
	AccountID       int64               `json:"account_id"`
	TriggerSource   string              `json:"trigger_source"`
	TestKind        string              `json:"test_kind"`
	ScheduledHour   *time.Time          `json:"scheduled_hour,omitempty"`
	Model           string              `json:"model"`
	ReasoningEffort string              `json:"reasoning_effort"`
	Status          string              `json:"status"`
	JuiceRequested  int                 `json:"juice_requested"`
	JuiceGraded     int                 `json:"juice_graded"`
	JuiceCorrect    int                 `json:"juice_correct"`
	CandyRequested  int                 `json:"candy_requested"`
	CandyGraded     int                 `json:"candy_graded"`
	CandyCorrect    int                 `json:"candy_correct"`
	StartedAt       time.Time           `json:"started_at"`
	FinishedAt      *time.Time          `json:"finished_at,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	Samples         []QualityEvalSample `json:"samples,omitempty"`
}

// QualityEvalSample 保存单个 Juice 或糖果请求的原始答案、判分和性能数据。
type QualityEvalSample struct {
	ID              int64     `json:"id"`
	BatchID         int64     `json:"batch_id"`
	AccountID       int64     `json:"account_id"`
	TestKind        string    `json:"test_kind"`
	SampleIndex     int       `json:"sample_index"`
	AttemptCount    int       `json:"attempt_count"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort"`
	AttemptAnswers  []string  `json:"attempt_answers,omitempty"`
	RawAnswer       string    `json:"raw_answer"`
	ParsedAnswer    string    `json:"parsed_answer"`
	Graded          bool      `json:"graded"`
	Correct         bool      `json:"correct"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	ReasoningTokens int       `json:"reasoning_tokens"`
	FirstTokenMs    int       `json:"first_token_ms"`
	DurationMs      int       `json:"duration_ms"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// QualityEvalCandidate 是周期检测的账号候选及首轮请求数。
type QualityEvalCandidate struct {
	AccountID    int64 `json:"account_id"`
	RequestCount int64 `json:"request_count"`
}

// CreateQualityEvalBatch 创建批次。自动批次若小时桶已存在，会返回 created=false。
func (db *DB) CreateQualityEvalBatch(ctx context.Context, batch QualityEvalBatch) (id int64, created bool, err error) {
	var scheduled interface{}
	if batch.ScheduledHour != nil {
		scheduled = db.timeArg(batch.ScheduledHour.UTC())
	}
	startedAt := batch.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	row := db.conn.QueryRowContext(ctx, `
		INSERT INTO account_quality_eval_batches (
			account_id, trigger_source, test_kind, scheduled_hour, model, reasoning_effort,
			status, juice_requested, candy_requested, started_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		ON CONFLICT (account_id, trigger_source, scheduled_hour) DO NOTHING
		RETURNING id
	`, batch.AccountID, batch.TriggerSource, batch.TestKind, scheduled, batch.Model, batch.ReasoningEffort,
		QualityEvalStatusRunning, batch.JuiceRequested, batch.CandyRequested, db.timeArg(startedAt))
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return id, true, nil
}

// InsertQualityEvalSample 持久化一个已结束的质量检测样本。
func (db *DB) InsertQualityEvalSample(ctx context.Context, sample QualityEvalSample) error {
	createdAt := sample.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	attemptAnswers, err := json.Marshal(sample.AttemptAnswers)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO account_quality_eval_samples (
			batch_id, account_id, test_kind, sample_index, attempt_count, model, reasoning_effort,
			attempt_answers, raw_answer, parsed_answer, graded, correct, input_tokens, output_tokens,
			reasoning_tokens, first_token_ms, duration_ms, error_message, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
	`, sample.BatchID, sample.AccountID, sample.TestKind, sample.SampleIndex, sample.AttemptCount,
		sample.Model, sample.ReasoningEffort, string(attemptAnswers), sample.RawAnswer, sample.ParsedAnswer, sample.Graded,
		sample.Correct, sample.InputTokens, sample.OutputTokens, sample.ReasoningTokens,
		sample.FirstTokenMs, sample.DurationMs, sample.ErrorMessage, db.timeArg(createdAt))
	return err
}

// CompleteQualityEvalBatch 写入批次聚合与最终分类。
func (db *DB) CompleteQualityEvalBatch(ctx context.Context, batch QualityEvalBatch) error {
	status := strings.TrimSpace(batch.Status)
	switch status {
	case QualityEvalStatusNormal, QualityEvalStatusSuspected, QualityEvalStatusDegraded, QualityEvalStatusIncomplete:
	default:
		return fmt.Errorf("无效的质量检测状态: %q", status)
	}
	finishedAt := time.Now().UTC()
	if batch.FinishedAt != nil {
		finishedAt = batch.FinishedAt.UTC()
	}
	_, err := db.conn.ExecContext(ctx, `
		UPDATE account_quality_eval_batches
		SET status = $1, juice_graded = $2, juice_correct = $3,
			candy_graded = $4, candy_correct = $5, finished_at = $6
		WHERE id = $7
	`, status, batch.JuiceGraded, batch.JuiceCorrect, batch.CandyGraded, batch.CandyCorrect,
		db.timeArg(finishedAt), batch.ID)
	return err
}

// MarkInterruptedQualityEvalBatches 将早于截止时间的遗留运行中批次标为不完整。
// 截止时间避免多实例启动时误伤其他实例仍在执行的批次。
func (db *DB) MarkInterruptedQualityEvalBatches(ctx context.Context, before time.Time) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE account_quality_eval_batches
		SET status = $1, finished_at = CURRENT_TIMESTAMP
		WHERE status = $2 AND started_at < $3
	`, QualityEvalStatusIncomplete, QualityEvalStatusRunning, db.timeArg(before))
	return err
}

// GetQualityEvalConfig 返回持久化配置；未配置时返回默认值。
func (db *DB) GetQualityEvalConfig(ctx context.Context) (QualityEvalConfig, error) {
	config := DefaultQualityEvalConfig()
	err := db.conn.QueryRowContext(ctx, `
		SELECT auto_enabled, interval_minutes, lookback_hours, top_accounts, min_requests, batch_concurrency
		FROM quality_eval_config WHERE id = 1
	`).Scan(&config.AutoEnabled, &config.IntervalMinutes, &config.LookbackHours, &config.TopAccounts,
		&config.MinRequests, &config.BatchConcurrency)
	if errors.Is(err, sql.ErrNoRows) {
		return config, nil
	}
	if err != nil {
		return QualityEvalConfig{}, err
	}
	return NormalizeQualityEvalConfig(config), nil
}

// SaveQualityEvalConfig 持久化周期检测配置。
func (db *DB) SaveQualityEvalConfig(ctx context.Context, config QualityEvalConfig) (QualityEvalConfig, error) {
	config = NormalizeQualityEvalConfig(config)
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO quality_eval_config (
			id, auto_enabled, interval_minutes, lookback_hours, top_accounts,
			min_requests, batch_concurrency, updated_at
		) VALUES (1, $1, $2, $3, $4, $5, $6, CURRENT_TIMESTAMP)
		ON CONFLICT (id) DO UPDATE SET
			auto_enabled = EXCLUDED.auto_enabled,
			interval_minutes = EXCLUDED.interval_minutes,
			lookback_hours = EXCLUDED.lookback_hours,
			top_accounts = EXCLUDED.top_accounts,
			min_requests = EXCLUDED.min_requests,
			batch_concurrency = EXCLUDED.batch_concurrency,
			updated_at = CURRENT_TIMESTAMP
	`, config.AutoEnabled, config.IntervalMinutes, config.LookbackHours, config.TopAccounts,
		config.MinRequests, config.BatchConcurrency)
	return config, err
}

// TryCreateQualityEvalScheduleRun 原子声明一个调度桶，确保排名只在每个桶计算一次。
func (db *DB) TryCreateQualityEvalScheduleRun(ctx context.Context, scheduledHour time.Time) (bool, error) {
	result, err := db.conn.ExecContext(ctx, `
		INSERT INTO quality_eval_schedule_runs (scheduled_hour, status, started_at)
		VALUES ($1, $2, CURRENT_TIMESTAMP)
		ON CONFLICT (scheduled_hour) DO NOTHING
	`, db.timeArg(scheduledHour), QualityEvalStatusRunning)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// CompleteQualityEvalScheduleRun 标记调度桶已结束。
func (db *DB) CompleteQualityEvalScheduleRun(ctx context.Context, scheduledHour time.Time, status string) error {
	if status != QualityEvalStatusNormal && status != QualityEvalStatusIncomplete {
		return fmt.Errorf("无效的质量检测调度状态: %q", status)
	}
	_, err := db.conn.ExecContext(ctx, `
		UPDATE quality_eval_schedule_runs
		SET status = $1, finished_at = CURRENT_TIMESTAMP
		WHERE scheduled_hour = $2
	`, status, db.timeArg(scheduledHour))
	return err
}

// TryAcquireQualityEvalSchedulerLease 尝试取得跨实例自动检测租约。
func (db *DB) TryAcquireQualityEvalSchedulerLease(ctx context.Context, owner string, now, leaseUntil time.Time) (bool, error) {
	var acquiredOwner string
	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO quality_eval_scheduler_lock (id, owner, lease_until, updated_at)
		VALUES (1, $1, $2, CURRENT_TIMESTAMP)
		ON CONFLICT (id) DO UPDATE SET
			owner = EXCLUDED.owner,
			lease_until = EXCLUDED.lease_until,
			updated_at = CURRENT_TIMESTAMP
		WHERE quality_eval_scheduler_lock.owner = $1
			OR quality_eval_scheduler_lock.lease_until <= $3
		RETURNING owner
	`, owner, db.timeArg(leaseUntil), db.timeArg(now)).Scan(&acquiredOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return acquiredOwner == owner, nil
}

// RenewQualityEvalSchedulerLease 延长当前实例持有的自动检测租约。
func (db *DB) RenewQualityEvalSchedulerLease(ctx context.Context, owner string, leaseUntil time.Time) (bool, error) {
	result, err := db.conn.ExecContext(ctx, `
		UPDATE quality_eval_scheduler_lock
		SET lease_until = $1, updated_at = CURRENT_TIMESTAMP
		WHERE id = 1 AND owner = $2
	`, db.timeArg(leaseUntil), owner)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// ReleaseQualityEvalSchedulerLease 释放当前实例持有的自动检测租约。
func (db *DB) ReleaseQualityEvalSchedulerLease(ctx context.Context, owner string, now time.Time) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE quality_eval_scheduler_lock
		SET owner = '', lease_until = $1, updated_at = CURRENT_TIMESTAMP
		WHERE id = 1 AND owner = $2
	`, db.timeArg(now), owner)
	return err
}

// GetQualityEvalCandidates 返回窗口内首轮且非 499 请求最多的候选账号。
func (db *DB) GetQualityEvalCandidates(ctx context.Context, start, end time.Time, minRequests, limit int) ([]QualityEvalCandidate, error) {
	if minRequests < 1 || limit < 1 {
		return nil, nil
	}
	retryFalse := "COALESCE(usage_logs.is_retry_attempt, false) = false"
	if db.isSQLite() {
		retryFalse = "COALESCE(usage_logs.is_retry_attempt, 0) = 0"
	}
	query := fmt.Sprintf(`
		SELECT usage_logs.account_id, COUNT(*) AS request_count
		FROM usage_logs
		JOIN accounts ON accounts.id = usage_logs.account_id
		WHERE usage_logs.created_at >= $1 AND usage_logs.created_at < $2
			AND accounts.status <> 'deleted'
			AND COALESCE(accounts.error_message, '') <> 'deleted'
			AND usage_logs.status_code <> 499
			AND %s
			AND COALESCE(usage_logs.attempt_index, 0) = 0
			AND usage_logs.account_id > 0
		GROUP BY usage_logs.account_id
		HAVING COUNT(*) >= $3
		ORDER BY request_count DESC, usage_logs.account_id ASC
		LIMIT $4
	`, retryFalse)
	rows, err := db.conn.QueryContext(ctx, query, db.timeArg(start), db.timeArg(end), minRequests, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]QualityEvalCandidate, 0, limit)
	for rows.Next() {
		var candidate QualityEvalCandidate
		if err := rows.Scan(&candidate.AccountID, &candidate.RequestCount); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// GetLatestQualityEvalBatches 返回每个账号最新的批次摘要。
func (db *DB) GetLatestQualityEvalBatches(ctx context.Context) (map[int64]QualityEvalBatch, error) {
	rows, err := db.conn.QueryContext(ctx, qualityEvalBatchSelect+`
		WHERE b.id = (
			SELECT latest.id FROM account_quality_eval_batches latest
			WHERE latest.account_id = b.account_id
			ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1
		)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]QualityEvalBatch)
	for rows.Next() {
		batch, err := scanQualityEvalBatch(rows)
		if err != nil {
			return nil, err
		}
		result[batch.AccountID] = batch
	}
	return result, rows.Err()
}

// ListAccountQualityEvalBatches 返回账号最近的质量检测明细。
func (db *DB) ListAccountQualityEvalBatches(ctx context.Context, accountID int64, limit int) ([]QualityEvalBatch, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := db.conn.QueryContext(ctx, qualityEvalBatchSelect+`
		WHERE b.account_id = $1
		ORDER BY b.created_at DESC, b.id DESC
		LIMIT $2
	`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	batches := make([]QualityEvalBatch, 0, limit)
	for rows.Next() {
		batch, err := scanQualityEvalBatch(rows)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range batches {
		samples, err := db.listQualityEvalSamples(ctx, batches[index].ID)
		if err != nil {
			return nil, err
		}
		batches[index].Samples = samples
	}
	return batches, nil
}

const qualityEvalBatchSelect = `
	SELECT b.id, b.account_id, b.trigger_source, b.test_kind, b.scheduled_hour,
		b.model, b.reasoning_effort, b.status,
		b.juice_requested, b.juice_graded, b.juice_correct,
		b.candy_requested, b.candy_graded, b.candy_correct,
		b.started_at, b.finished_at, b.created_at
	FROM account_quality_eval_batches b
`

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanQualityEvalBatch(scanner rowScanner) (QualityEvalBatch, error) {
	var batch QualityEvalBatch
	var scheduledRaw, startedRaw, finishedRaw, createdRaw interface{}
	err := scanner.Scan(&batch.ID, &batch.AccountID, &batch.TriggerSource, &batch.TestKind, &scheduledRaw,
		&batch.Model, &batch.ReasoningEffort, &batch.Status,
		&batch.JuiceRequested, &batch.JuiceGraded, &batch.JuiceCorrect,
		&batch.CandyRequested, &batch.CandyGraded, &batch.CandyCorrect,
		&startedRaw, &finishedRaw, &createdRaw)
	if err != nil {
		return batch, err
	}
	if parsed, parseErr := parseDBNullTimeValue(scheduledRaw); parseErr == nil && parsed.Valid {
		value := parsed.Time
		batch.ScheduledHour = &value
	}
	if parsed, parseErr := parseDBNullTimeValue(startedRaw); parseErr == nil && parsed.Valid {
		batch.StartedAt = parsed.Time
	}
	if parsed, parseErr := parseDBNullTimeValue(finishedRaw); parseErr == nil && parsed.Valid {
		value := parsed.Time
		batch.FinishedAt = &value
	}
	if parsed, parseErr := parseDBNullTimeValue(createdRaw); parseErr == nil && parsed.Valid {
		batch.CreatedAt = parsed.Time
	}
	return batch, nil
}

func (db *DB) listQualityEvalSamples(ctx context.Context, batchID int64) ([]QualityEvalSample, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, batch_id, account_id, test_kind, sample_index, attempt_count,
			model, reasoning_effort, attempt_answers, raw_answer, parsed_answer, graded, correct,
			input_tokens, output_tokens, reasoning_tokens, first_token_ms,
			duration_ms, error_message, created_at
		FROM account_quality_eval_samples
		WHERE batch_id = $1
		ORDER BY test_kind, sample_index
	`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := make([]QualityEvalSample, 0)
	for rows.Next() {
		var sample QualityEvalSample
		var createdRaw interface{}
		var attemptAnswersJSON string
		if err := rows.Scan(&sample.ID, &sample.BatchID, &sample.AccountID, &sample.TestKind,
			&sample.SampleIndex, &sample.AttemptCount, &sample.Model, &sample.ReasoningEffort,
			&attemptAnswersJSON, &sample.RawAnswer, &sample.ParsedAnswer, &sample.Graded, &sample.Correct,
			&sample.InputTokens, &sample.OutputTokens, &sample.ReasoningTokens,
			&sample.FirstTokenMs, &sample.DurationMs, &sample.ErrorMessage, &createdRaw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(attemptAnswersJSON), &sample.AttemptAnswers)
		if parsed, parseErr := parseDBNullTimeValue(createdRaw); parseErr == nil && parsed.Valid {
			sample.CreatedAt = parsed.Time
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}
