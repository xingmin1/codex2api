package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const (
	FirstTokenSourceNormal      = "normal"
	FirstTokenSourceManualProbe = "manual_probe"
	FirstTokenSourceAutoProbe   = "auto_probe"

	accountFirstTokenShortWindow = 10 * time.Minute
	accountFirstTokenLongWindow  = time.Hour
	accountFirstTokenShortLimit  = 5
	accountFirstTokenRetention   = 2 * time.Hour
	firstTokenCleanupInterval    = 10 * time.Minute
)

// AccountFirstTokenSample 是一条账号首字耗时观测。
type AccountFirstTokenSample struct {
	AccountID    int64
	Source       string
	Model        string
	FirstTokenMs int
	CreatedAt    time.Time
}

// AccountFirstTokenWindowStats 描述一个时间窗口内的账号首字表现。
type AccountFirstTokenWindowStats struct {
	WindowSeconds int64      `json:"window_seconds"`
	SampleLimit   int        `json:"sample_limit,omitempty"`
	AverageMs     float64    `json:"average_ms"`
	MaximumMs     int        `json:"maximum_ms"`
	SampleCount   int        `json:"sample_count"`
	LastSampleAt  *time.Time `json:"last_sample_at,omitempty"`
}

// AccountFirstTokenStats 同时包含短期和长期首字表现。
type AccountFirstTokenStats struct {
	Short AccountFirstTokenWindowStats `json:"short"`
	Long  AccountFirstTokenWindowStats `json:"long"`
}

// InsertAccountFirstTokenSample 将首字样本加入异步批写缓冲。
func (db *DB) InsertAccountFirstTokenSample(_ context.Context, sample *AccountFirstTokenSample) error {
	if db == nil || sample == nil || sample.AccountID <= 0 || sample.FirstTokenMs <= 0 {
		return nil
	}
	source := normalizeFirstTokenSource(sample.Source)
	if source == "" {
		return fmt.Errorf("无效的首字样本来源: %q", sample.Source)
	}
	createdAt := sample.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	model := strings.TrimSpace(sample.Model)
	if len(model) > 100 {
		model = model[:100]
	}

	db.logMu.Lock()
	db.firstTokenBuf = append(db.firstTokenBuf, AccountFirstTokenSample{
		AccountID:    sample.AccountID,
		Source:       source,
		Model:        model,
		FirstTokenMs: sample.FirstTokenMs,
		CreatedAt:    createdAt.UTC(),
	})
	bufLen := len(db.firstTokenBuf)
	db.logMu.Unlock()

	if bufLen >= db.GetUsageLogBatchSize() {
		db.notifyLogFlush()
	}
	return nil
}

func normalizeFirstTokenSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case FirstTokenSourceNormal:
		return FirstTokenSourceNormal
	case FirstTokenSourceManualProbe:
		return FirstTokenSourceManualProbe
	case FirstTokenSourceAutoProbe:
		return FirstTokenSourceAutoProbe
	default:
		return ""
	}
}

func (db *DB) insertAccountFirstTokenBatch(ctx context.Context, batch []AccountFirstTokenSample) error {
	if len(batch) == 0 {
		return nil
	}
	if db.isSQLite() {
		return db.insertSQLiteAccountFirstTokenBatch(ctx, batch)
	}

	const maxRowsPerBatch = 10000
	for start := 0; start < len(batch); start += maxRowsPerBatch {
		end := start + maxRowsPerBatch
		if end > len(batch) {
			end = len(batch)
		}
		valueStrings := make([]string, 0, end-start)
		args := make([]interface{}, 0, (end-start)*5)
		argIndex := 1
		for _, sample := range batch[start:end] {
			valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d)", argIndex, argIndex+1, argIndex+2, argIndex+3, argIndex+4))
			args = append(args, sample.AccountID, sample.Source, sample.Model, sample.FirstTokenMs, db.timeArg(sample.CreatedAt))
			argIndex += 5
		}
		query := `INSERT INTO account_first_token_samples (account_id, source, model, first_token_ms, created_at) VALUES ` + strings.Join(valueStrings, ",")
		if _, err := db.conn.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) insertSQLiteAccountFirstTokenBatch(ctx context.Context, batch []AccountFirstTokenSample) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO account_first_token_samples (account_id, source, model, first_token_ms, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, sample := range batch {
		if _, err := stmt.ExecContext(ctx, sample.AccountID, sample.Source, sample.Model, sample.FirstTokenMs, db.timeArg(sample.CreatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) requeueAccountFirstTokenBatch(batch []AccountFirstTokenSample) {
	if len(batch) == 0 {
		return
	}
	db.logMu.Lock()
	defer db.logMu.Unlock()

	requeued := make([]AccountFirstTokenSample, 0, len(batch)+len(db.firstTokenBuf))
	requeued = append(requeued, batch...)
	requeued = append(requeued, db.firstTokenBuf...)
	db.firstTokenBuf = requeued
}

func (db *DB) maybeCleanupAccountFirstTokenSamples(ctx context.Context, now time.Time) error {
	nowNanos := now.UnixNano()
	last := atomic.LoadInt64(&db.firstTokenCleanupAt)
	if last != 0 && time.Duration(nowNanos-last) < firstTokenCleanupInterval {
		return nil
	}
	if !atomic.CompareAndSwapInt64(&db.firstTokenCleanupAt, last, nowNanos) {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM account_first_token_samples WHERE created_at < $1`, db.timeArg(now.Add(-accountFirstTokenRetention))); err != nil {
		atomic.CompareAndSwapInt64(&db.firstTokenCleanupAt, nowNanos, last)
		return err
	}
	return nil
}

// GetAccountsFirstTokenStats 批量返回各账号的双窗口首字统计。
func (db *DB) GetAccountsFirstTokenStats(ctx context.Context, now time.Time) (map[int64]AccountFirstTokenStats, error) {
	if now.IsZero() {
		now = time.Now()
	}
	longStart := now.Add(-accountFirstTokenLongWindow)
	shortStart := now.Add(-accountFirstTokenShortWindow)
	rows, err := db.conn.QueryContext(ctx, `
		WITH recent AS (
			SELECT account_id, first_token_ms, created_at,
				ROW_NUMBER() OVER (PARTITION BY account_id ORDER BY created_at DESC, id DESC) AS recent_rank
			FROM account_first_token_samples
			WHERE created_at >= $1 AND created_at <= $2 AND first_token_ms > 0
		)
		SELECT account_id,
			COALESCE(AVG(CASE WHEN created_at >= $3 AND recent_rank <= 5 THEN first_token_ms END), 0),
			COALESCE(MAX(CASE WHEN created_at >= $3 AND recent_rank <= 5 THEN first_token_ms END), 0),
			COUNT(CASE WHEN created_at >= $3 AND recent_rank <= 5 THEN 1 END),
			MAX(CASE WHEN created_at >= $3 AND recent_rank <= 5 THEN created_at END),
			COALESCE(AVG(first_token_ms), 0),
			COALESCE(MAX(first_token_ms), 0),
			COUNT(*),
			MAX(created_at)
		FROM recent
		GROUP BY account_id
	`, db.timeArg(longStart), db.timeArg(now), db.timeArg(shortStart))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]AccountFirstTokenStats)
	for rows.Next() {
		var accountID int64
		var shortAverage float64
		var shortMaximum int
		var shortCount int
		var shortLastRaw interface{}
		var longAverage float64
		var longMaximum int
		var longCount int
		var longLastRaw interface{}
		if err := rows.Scan(&accountID, &shortAverage, &shortMaximum, &shortCount, &shortLastRaw, &longAverage, &longMaximum, &longCount, &longLastRaw); err != nil {
			return nil, err
		}

		stats := AccountFirstTokenStats{
			Short: AccountFirstTokenWindowStats{
				WindowSeconds: int64(accountFirstTokenShortWindow / time.Second),
				SampleLimit:   accountFirstTokenShortLimit,
				AverageMs:     shortAverage,
				MaximumMs:     shortMaximum,
				SampleCount:   shortCount,
			},
			Long: AccountFirstTokenWindowStats{
				WindowSeconds: int64(accountFirstTokenLongWindow / time.Second),
				AverageMs:     longAverage,
				MaximumMs:     longMaximum,
				SampleCount:   longCount,
			},
		}
		if parsed, parseErr := parseDBNullTimeValue(shortLastRaw); parseErr == nil && parsed.Valid {
			lastSampleAt := parsed.Time
			stats.Short.LastSampleAt = &lastSampleAt
		}
		if parsed, parseErr := parseDBNullTimeValue(longLastRaw); parseErr == nil && parsed.Valid {
			lastSampleAt := parsed.Time
			stats.Long.LastSampleAt = &lastSampleAt
		}
		result[accountID] = stats
	}
	return result, rows.Err()
}

// UpdateAccountManualScoreBonus 持久化账号临时调度分调整，bonus 为零时清除。
func (db *DB) UpdateAccountManualScoreBonus(ctx context.Context, accountID int64, bonus int64, until time.Time) error {
	var untilValue interface{}
	if bonus != 0 && !until.IsZero() {
		untilValue = db.timeArg(until)
	} else {
		bonus = 0
	}
	result, err := db.conn.ExecContext(ctx, `
		UPDATE accounts
		SET manual_score_bonus = $1, manual_score_bonus_until = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $3 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'
	`, bonus, untilValue, accountID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ClearExpiredAccountManualScoreBonuses 清理数据库中已经到期的临时调度分。
func (db *DB) ClearExpiredAccountManualScoreBonuses(ctx context.Context, now time.Time) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE accounts
		SET manual_score_bonus = 0, manual_score_bonus_until = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE manual_score_bonus_until IS NOT NULL AND manual_score_bonus_until <= $1
	`, db.timeArg(now))
	return err
}
