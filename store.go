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

// Record saves one attempt.
func (s *PostgresStore) Record(ctx context.Context, month string, a *Advice, failure string, at time.Time) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode advice: %w", err)
	}
	row := adviceRow{
		Month:     month,
		CreatedAt: at.UTC(),
		Model:     a.Model,
		TokensIn:  a.TokensIn,
		TokensOut: a.TokensOut,
		Failure:   failure,
		Body:      string(body),
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("insert advice: %w", err)
	}
	return nil
}

// CountSince counts attempts at or after since.
func (s *PostgresStore) CountSince(ctx context.Context, since time.Time) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&adviceRow{}).
		Where("created_at >= ?", since.UTC()).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count advice: %w", err)
	}
	return int(n), nil
}

type memoryAttempt struct {
	month  string
	advice *Advice
	failed bool
	at     time.Time
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

	for i := len(s.attempts) - 1; i >= 0; i-- {
		if a := s.attempts[i]; a.month == month && !a.failed {
			return a.advice, nil
		}
	}
	return nil, nil
}

// Record saves one attempt.
func (s *MemoryStore) Record(_ context.Context, month string, a *Advice, failure string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.attempts = append(s.attempts, memoryAttempt{month: month, advice: a, failed: failure != "", at: at})
	return nil
}

// CountSince counts attempts at or after since.
func (s *MemoryStore) CountSince(_ context.Context, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, a := range s.attempts {
		if !a.at.Before(since) {
			n++
		}
	}
	return n, nil
}
