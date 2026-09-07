// Package testutil 提供测试用共享设施（内存 SQLite + Store）。
package testutil

import (
	"context"
	"database/sql"
	"testing"

	"midroute/internal/db"
	"midroute/internal/repository"
)

// NewStore 创建已迁移的内存 SQLite 与 repository.Store。
func NewStore(t *testing.T) (*sql.DB, *repository.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	return conn, repository.New(conn)
}
