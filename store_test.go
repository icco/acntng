package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// testDSN returns TEST_DATABASE_URL, or starts a throwaway Postgres.
func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}

	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}

	dir := t.TempDir()
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)). //nolint:gosec // a TCP port fits in uint32
		Encoding("UTF8").
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		CachePath(filepath.Join(os.TempDir(), "acntng-embedded-postgres")).
		Logger(nil))
	if err := pg.Start(); err != nil {
		t.Fatalf("start embedded postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := pg.Stop(); err != nil {
			t.Errorf("stop embedded postgres: %v", err)
		}
	})
	return fmt.Sprintf("host=localhost port=%d user=postgres password=postgres dbname=postgres sslmode=disable", port)
}

// storeContract is what the rank cache relies on from any store.
func storeContract(t *testing.T, s RankStore) {
	t.Helper()
	ctx := context.Background()
	day := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	at := day.Add(10 * time.Hour)

	if r, f, _, err := s.Lookup(ctx, "facts-a"); err != nil || r != nil || f != "" {
		t.Fatalf("empty lookup = %v, %q, %v", r, f, err)
	}

	id, ok, err := s.Reserve(ctx, "facts-a", day, 2, at)
	if err != nil || !ok {
		t.Fatalf("first reserve = %v, %v", ok, err)
	}
	if r, f, _, _ := s.Lookup(ctx, "facts-a"); r != nil || f != "" {
		t.Errorf("a pending call is not an outcome: %v, %q", r, f)
	}
	if err := s.Finish(ctx, id, nil, "proxy down"); err != nil {
		t.Fatal(err)
	}
	if _, f, when, _ := s.Lookup(ctx, "facts-a"); f != "proxy down" || !when.Equal(at) {
		t.Errorf("failure lookup = %q at %v", f, when)
	}

	id, ok, _ = s.Reserve(ctx, "facts-a", day, 2, at.Add(time.Hour))
	if !ok {
		t.Fatal("second reserve refused")
	}
	want := &Ranking{Summary: "second", Model: "fake-model", Picks: []Pick{{ID: 3, Why: "because"}}}
	if err := s.Finish(ctx, id, want, ""); err != nil {
		t.Fatal(err)
	}
	if r, f, _, _ := s.Lookup(ctx, "facts-a"); r == nil || f != "" || r.Summary != "second" || len(r.Picks) != 1 || r.Picks[0].ID != 3 {
		t.Errorf("ranking lookup = %+v, %q", r, f)
	}

	if _, ok, _ := s.Reserve(ctx, "facts-b", day, 2, at.Add(2*time.Hour)); ok {
		t.Error("a third call on the same day passed a cap of two")
	}
	if _, ok, _ := s.Reserve(ctx, "facts-b", day.Add(24*time.Hour), 2, at.Add(24*time.Hour)); !ok {
		t.Error("the cap should reset the next day")
	}
}

func TestMemoryStore(t *testing.T) {
	storeContract(t, &MemoryStore{})
}

func TestPostgresStore(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a database")
	}
	dsn := testDSN(t)
	s, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	if err := s.db.Exec("TRUNCATE acntng_rankings").Error; err != nil {
		t.Fatalf("truncate: %v", err)
	}
	storeContract(t, s)

	// A restart opens the table again and must still see the ranking.
	again, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if r, _, _, err := again.Lookup(context.Background(), "facts-a"); err != nil || r == nil || r.Summary != "second" {
		t.Errorf("after reopen: %+v, %v", r, err)
	}
}

func TestRankingSurvivesARestart(t *testing.T) {
	store := &MemoryStore{}
	first := &fakeRanker{subjects: []string{"Old Town Water"}}
	s, _ := rankedServer(t, first, 4)
	s.rank.store = store
	if _, err := s.plan(context.Background(), planNow); err != nil {
		t.Fatal(err)
	}

	second := &fakeRanker{err: errors.New("should not be asked")}
	s2, _ := rankedServer(t, second, 4)
	s2.rank.store = store
	p, err := s2.plan(context.Background(), planNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.calls != 0 || p.ChosenBy != "fake-model" {
		t.Errorf("a new process asked again: %d calls, chosen by %s", second.calls, p.ChosenBy)
	}
}
