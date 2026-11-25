package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

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

func TestPostgresStore(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a database")
	}

	dsn := testDSN(t)
	s, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	if err := s.db.Exec("TRUNCATE acntng_advice").Error; err != nil {
		t.Fatalf("truncate: %v", err)
	}
	storeContract(t, s)

	// Opening again must migrate cleanly and still see the rows: this is what
	// lets the call budget outlive a restart.
	again, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if a, err := again.Latest(context.Background(), "2026-10"); err != nil || a == nil || a.Summary != "second" {
		t.Errorf("after reopen: %+v, %v", a, err)
	}
}
