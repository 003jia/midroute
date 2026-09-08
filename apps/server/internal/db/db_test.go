package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	v, err := Migrate(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if v != KnownVersion() {
		t.Fatalf("version=%d, want %d", v, KnownVersion())
	}
	// 幂等：再次迁移不报错
	v2, err := Migrate(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v {
		t.Fatalf("re-migrate version=%d", v2)
	}
	// 表存在
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no tables created")
	}
}

func TestMigrateFailureRollsBack(t *testing.T) {
	// 注入一条必然失败的迁移，验证：报错、版本不推进、部分写入回滚（MR-003）。
	orig := Migrations
	defer func() { Migrations = orig }()
	Migrations = append([]Migration{}, orig...)
	Migrations = append(Migrations, Migration{
		Version: 99,
		Name:    "boom",
		Up: `
CREATE TABLE rollback_probe(id TEXT PRIMARY KEY);
INSERT INTO rollback_probe VALUES('a');  -- ok
INSERT INTO missing_table VALUES('x');   -- 失败
`,
	})

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fail.db")
	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	v, err := Migrate(ctx, conn)
	if err == nil {
		t.Fatal("expected migration failure")
	}
	if v != 3 { // 前三个版本成功
		t.Fatalf("version=%d want 3", v)
	}
	// 失败版本未记录
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=99`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed migration must not be recorded")
	}
	// 事务回滚：boom 内先创建的表不存在
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='rollback_probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("rollback_probe must be rolled back")
	}
}

func TestOpenCreatesWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.db")
	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var mode string
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode=%q", mode)
	}
}

func TestAccountTableStoresNoPlainSecret(t *testing.T) {
	// 迁移后 accounts 表只含 SecretRef 字段，验证列结构
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schema.db")
	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(accounts)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	if !cols["vault_provider"] || !cols["secret_service"] || !cols["secret_account"] || !cols["secret_fingerprint"] {
		t.Fatalf("accounts missing SecretRef columns: %v", cols)
	}
	for _, bad := range []string{"api_key", "secret", "token"} {
		if cols[bad] {
			t.Fatalf("accounts must not have raw secret column %q", bad)
		}
	}
}

// TestMigrateV1ToV2PreservesData 验证 v1 旧库升级到 v2：新增列落默认值，存量行保留。
func TestMigrateV1ToV2PreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 手工搭一个 v1 库（schema_migrations 由 Migrate 负责，这里手工建）并写入一行旧账户
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version     INTEGER PRIMARY KEY,
			name        TEXT NOT NULL,
			applied_at  TEXT NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, initialSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(1,'initial_schema','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO providers(id, kind, name, created_at, updated_at) VALUES('prov_x','openai','旧平台','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO accounts(id, provider_id, name, status, created_at, updated_at)
		 VALUES('acc_old','prov_x','旧账户','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	v, err := Migrate(ctx, conn)
	if err != nil {
		t.Fatalf("upgrade v1->v2: %v", err)
	}
	if v < 2 {
		t.Fatalf("version=%d, want >= 2", v)
	}
	var name, mode, authType, billingMode, authState string
	if err := conn.QueryRowContext(ctx,
		`SELECT name, mode, auth_type, billing_mode, auth_state FROM accounts WHERE id='acc_old'`).
		Scan(&name, &mode, &authType, &billingMode, &authState); err != nil {
		t.Fatal(err)
	}
	if name != "旧账户" || mode != "relay_and_monitor" || authType != "api_key" || billingMode != "unknown" || authState != "unknown" {
		t.Fatalf("upgraded row unexpected: %q %q %q %q %q", name, mode, authType, billingMode, authState)
	}
}

// TestMigrateRejectsFutureSchema 验证数据库版本高于程序已知版本时拒绝写入。
func TestMigrateRejectsFutureSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "future.db")
	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	// 模拟"未来程序"把库升到了 v99
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schema_migrations(version,name,applied_at) VALUES(99,'from_the_future','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, conn); err == nil {
		t.Fatal("expected future schema to be rejected, got nil error")
	}
}

// TestModelsUniqueProviderUpstream 验证同名模型属于不同 Provider 不冲突、同 Provider 重复被拒。
func TestModelsUniqueProviderUpstream(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "uniq.db")
	conn, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	for _, p := range []string{"prov_a", "prov_b"} {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO providers(id, kind, name, created_at, updated_at) VALUES(?,?,?,?,?)`,
			p, "openai", p, now, now); err != nil {
			t.Fatal(err)
		}
	}
	ins := func(prov, upstream string) error {
		_, err := conn.ExecContext(ctx,
			`INSERT INTO models(id, provider_id, upstream_id, created_at) VALUES(?,?,?,?)`,
			prov+"|"+upstream, prov, upstream, now)
		return err
	}
	if err := ins("prov_a", "gpt-x"); err != nil {
		t.Fatal(err)
	}
	if err := ins("prov_b", "gpt-x"); err != nil {
		t.Fatalf("same upstream id under another provider must be allowed: %v", err)
	}
	if err := ins("prov_a", "gpt-x"); err == nil {
		t.Fatal("duplicate (provider_id, upstream_id) must violate unique index")
	}
}
