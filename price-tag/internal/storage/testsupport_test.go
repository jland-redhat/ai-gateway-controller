package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// openTestStore hands each test a throwaway database that has run the full
// migration chain. It skips when DATABASE_URL is unset:
//
//	DATABASE_URL=postgres://... go test ./internal/storage/
func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — integration tests need a Postgres")
	}
	dbName := fmt.Sprintf("storetest_%d", time.Now().UnixNano())
	// Create a throwaway database so tests never see (or pollute) real data.
	{
		s, err := New(dsn, 0, PoolConfig{})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if _, err := s.db.Exec("CREATE DATABASE " + dbName); err != nil {
			s.Close()
			t.Fatalf("create test db: %v", err)
		}
		s.Close()
	}
	testDSN := replaceDBName(dsn, dbName)
	store, err := New(testDSN, 0, PoolConfig{})
	if err != nil {
		t.Fatalf("connect fresh db: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		s, err := New(dsn, 0, PoolConfig{})
		if err == nil {
			s.db.Exec("DROP DATABASE " + dbName) //nolint:errcheck
			s.Close()
		}
	})
	return store, context.Background()
}

func replaceDBName(dsn, name string) string {
	// postgres://user:pass@host:port/OLDNAME?params — swap the path segment.
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '/' {
			end := len(dsn)
			if q := indexOf(dsn[i:], '?'); q >= 0 {
				end = i + q
				return dsn[:i+1] + name + dsn[end:]
			}
			return dsn[:i+1] + name
		}
	}
	return dsn
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func sp(s string) *string { return &s }
