package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// adviceRow is one model call. Failed calls are kept so they count against
// the monthly budget.
type adviceRow struct {
	ID        uint      `gorm:"primaryKey"`
	Month     string    `gorm:"size:7;index"`
	CreatedAt time.Time `gorm:"index"`
	Model     string
	TokensIn  int
	TokensOut int
	Failure   string
	Body      string `gorm:"type:text"`
}

// TableName names the table for its owner, since the database is shared.
func (adviceRow) TableName() string { return "acntng_advice" }

// PostgresStore keeps advice in Postgres, so the call budget survives a
// restart.
type PostgresStore struct {
	db *gorm.DB
}

// NewPostgresStore connects and migrates.
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.AutoMigrate(&adviceRow{}); err != nil {
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	return &PostgresStore{db: db}, nil
}

// Latest returns the newest successful advice for a review month, or nil.
func (s *PostgresStore) Latest(ctx context.Context, month string) (*Advice, error) {
	var row adviceRow
	err := s.db.WithContext(ctx).
		Where("month = ? AND failure = ''", month).
		Order("created_at DESC, id DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select advice: %w", err)
	}

	advice := &Advice{}
	if err := json.Unmarshal([]byte(row.Body), advice); err != nil {
		return nil, fmt.Errorf("decode stored advice %d: %w", row.ID, err)
	}
	return advice, nil
}

// pendingFailure marks an attempt that has been reserved but not settled.
const pendingFailure = "pending"

// reserveLockKey serializes Reserve across every process sharing the table.
const reserveLockKey = 0x61636e74

// Reserve inserts a pending attempt unless the budget is spent. An advisory
// lock makes the count and the insert one step across processes.
func (s *PostgresStore) Reserve(ctx context.Context, month string, since time.Time, limit int, at time.Time) (uint, bool, error) {
	row := adviceRow{Month: month, CreatedAt: at.UTC(), Failure: pendingFailure}
	ok := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", reserveLockKey).Error; err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		var n int64
		if err := tx.Model(&adviceRow{}).Where("created_at >= ?", since.UTC()).Count(&n).Error; err != nil {
			return fmt.Errorf("count: %w", err)
		}
		if int(n) >= limit {
			return nil
		}
		ok = true
		return tx.Create(&row).Error
	})
	if err != nil {
		return 0, false, fmt.Errorf("reserve advice: %w", err)
	}
	return row.ID, ok, nil
}

// Finish settles a reserved attempt.
func (s *PostgresStore) Finish(ctx context.Context, id uint, a *Advice, failure string) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode advice: %w", err)
	}
	res := s.db.WithContext(ctx).Model(&adviceRow{}).Where("id = ?", id).Updates(map[string]any{
		"model":      a.Model,
		"tokens_in":  a.TokensIn,
		"tokens_out": a.TokensOut,
		"failure":    failure,
		"body":       string(body),
	})
	if res.Error != nil {
		return fmt.Errorf("update advice %d: %w", id, res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("update advice %d: no such attempt", id)
	}
	return nil
}

type memoryAttempt struct {
	month   string
	advice  *Advice
	settled bool
	at      time.Time
}

// MemoryStore keeps advice for the life of the process. It is the fallback
// without DATABASE_URL: a restart forgets the advice and resets the budget.
type MemoryStore struct {
	mu       sync.Mutex
	attempts []memoryAttempt
}

// Latest returns the newest successful advice for a review month, or nil.
func (s *MemoryStore) Latest(_ context.Context, month string) (*Advice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var best *memoryAttempt
	for i := range s.attempts {
		a := &s.attempts[i]
		if a.month == month && a.settled && a.advice != nil && (best == nil || !a.at.Before(best.at)) {
			best = a
		}
	}
	if best == nil {
		return nil, nil
	}
	return best.advice, nil
}

// Reserve appends a pending attempt unless the budget is spent.
func (s *MemoryStore) Reserve(_ context.Context, month string, since time.Time, limit int, at time.Time) (uint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, a := range s.attempts {
		if !a.at.Before(since) {
			n++
		}
	}
	if n >= limit {
		return 0, false, nil
	}
	s.attempts = append(s.attempts, memoryAttempt{month: month, at: at})
	return uint(len(s.attempts)), true, nil
}

// Finish settles a reserved attempt.
func (s *MemoryStore) Finish(_ context.Context, id uint, a *Advice, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == 0 || int(id) > len(s.attempts) { //nolint:gosec // bounded by the slice length
		return fmt.Errorf("no such attempt %d", id)
	}
	attempt := &s.attempts[id-1]
	attempt.settled = true
	if failure == "" {
		attempt.advice = a
	}
	return nil
}
