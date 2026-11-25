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

// RankStore keeps every model call and its outcome, so a ranking outlives a
// restart and the daily call cap holds across processes.
type RankStore interface {
	// Lookup returns the newest settled outcome for a set of facts: a
	// ranking, or the failure and when it happened. Both are empty when the
	// facts were never ranked.
	Lookup(ctx context.Context, key string) (*Ranking, string, time.Time, error)
	// Reserve records a call before it is made, unless limit calls were made
	// at or after since. Check and insert are one step.
	Reserve(ctx context.Context, key string, since time.Time, limit int, at time.Time) (uint, bool, error)
	// Finish settles a reserved call with its ranking or its failure.
	Finish(ctx context.Context, id uint, r *Ranking, failure string) error
}

// pendingFailure marks a call that was reserved but has not settled.
const pendingFailure = "pending"

// rankRow is one model call.
type rankRow struct {
	ID        uint      `gorm:"primaryKey"`
	Key       string    `gorm:"size:64;index"`
	CreatedAt time.Time `gorm:"index"`
	Model     string
	TokensIn  int
	TokensOut int
	Failure   string
	Body      string `gorm:"type:text"`
}

// TableName names the table for its owner, since the database is shared.
func (rankRow) TableName() string { return "acntng_rankings" }

// PostgresStore keeps rankings in Postgres.
type PostgresStore struct {
	db *gorm.DB
}

// NewPostgresStore connects and migrates.
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.AutoMigrate(&rankRow{}); err != nil {
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	return &PostgresStore{db: db}, nil
}

// Lookup returns the newest settled outcome for key.
func (s *PostgresStore) Lookup(ctx context.Context, key string) (*Ranking, string, time.Time, error) {
	var row rankRow
	err := s.db.WithContext(ctx).
		Where("key = ? AND failure <> ?", key, pendingFailure).
		Order("created_at DESC, id DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", time.Time{}, nil
	}
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("select ranking: %w", err)
	}
	if row.Failure != "" {
		return nil, row.Failure, row.CreatedAt, nil
	}
	r := &Ranking{}
	if err := json.Unmarshal([]byte(row.Body), r); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("decode ranking %d: %w", row.ID, err)
	}
	return r, "", row.CreatedAt, nil
}

// reserveLockKey serializes Reserve across every process sharing the table.
const reserveLockKey = 0x61636e74

// Reserve inserts a pending call unless the cap is reached.
func (s *PostgresStore) Reserve(ctx context.Context, key string, since time.Time, limit int, at time.Time) (uint, bool, error) {
	row := rankRow{Key: key, CreatedAt: at.UTC(), Failure: pendingFailure}
	ok := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", reserveLockKey).Error; err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		var n int64
		if err := tx.Model(&rankRow{}).Where("created_at >= ?", since.UTC()).Count(&n).Error; err != nil {
			return fmt.Errorf("count: %w", err)
		}
		if int(n) >= limit {
			return nil
		}
		ok = true
		return tx.Create(&row).Error
	})
	if err != nil {
		return 0, false, fmt.Errorf("reserve ranking: %w", err)
	}
	return row.ID, ok, nil
}

// Finish settles a reserved call.
func (s *PostgresStore) Finish(ctx context.Context, id uint, r *Ranking, failure string) error {
	if r == nil {
		r = &Ranking{}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode ranking: %w", err)
	}
	res := s.db.WithContext(ctx).Model(&rankRow{}).Where("id = ?", id).Updates(map[string]any{
		"model":      r.Model,
		"tokens_in":  r.TokensIn,
		"tokens_out": r.TokensOut,
		"failure":    failure,
		"body":       string(body),
	})
	if res.Error != nil {
		return fmt.Errorf("update ranking %d: %w", id, res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("update ranking %d: no such call", id)
	}
	return nil
}

type memoryCall struct {
	key     string
	ranking *Ranking
	failure string
	at      time.Time
}

// MemoryStore keeps rankings for the life of the process, the fallback
// without DATABASE_URL: a restart forgets them and resets the cap.
type MemoryStore struct {
	mu    sync.Mutex
	calls []memoryCall
}

// Lookup returns the newest settled outcome for key.
func (s *MemoryStore) Lookup(_ context.Context, key string) (*Ranking, string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		c := s.calls[i]
		if c.key == key && c.failure != pendingFailure {
			return c.ranking, c.failure, c.at, nil
		}
	}
	return nil, "", time.Time{}, nil
}

// Reserve appends a pending call unless the cap is reached.
func (s *MemoryStore) Reserve(_ context.Context, key string, since time.Time, limit int, at time.Time) (uint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if !c.at.Before(since) {
			n++
		}
	}
	if n >= limit {
		return 0, false, nil
	}
	s.calls = append(s.calls, memoryCall{key: key, failure: pendingFailure, at: at})
	return uint(len(s.calls)), true, nil
}

// Finish settles a reserved call.
func (s *MemoryStore) Finish(_ context.Context, id uint, r *Ranking, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == 0 || int(id) > len(s.calls) { //nolint:gosec // bounded by the slice length
		return fmt.Errorf("no such call %d", id)
	}
	c := &s.calls[id-1]
	c.failure = failure
	if failure == "" {
		c.ranking = r
	}
	return nil
}
