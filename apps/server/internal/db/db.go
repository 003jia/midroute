// Package db 提供 SQLite 打开与版本化 migration。
// 对齐 PRD US-011/FR-25：单向版本化 migration、事务、失败拒绝启动。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Migration 一个版本化迁移。
type Migration struct {
	Version int
	Name    string
	Up      string // SQL（在事务内执行）
}

// Migrations 按版本升序排列。禁止修改已发布的迁移；新增变更只能追加版本。
var Migrations = []Migration{
	{
		Version: 1,
		Name:    "initial_schema",
		Up:      initialSchema,
	},
	{
		Version: 2,
		Name:    "account_mode_and_model_identity",
		Up: `
-- 账户模式/认证类型/计费方式/认证状态（合约 v1 §accounts；MR-003）
ALTER TABLE accounts ADD COLUMN mode TEXT NOT NULL DEFAULT 'relay_and_monitor';
ALTER TABLE accounts ADD COLUMN auth_type TEXT NOT NULL DEFAULT 'api_key';
ALTER TABLE accounts ADD COLUMN billing_mode TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE accounts ADD COLUMN auth_state TEXT NOT NULL DEFAULT 'unknown';
-- 同名模型属于不同 Provider 时互不覆盖（合约 v1 §models；MR-003）
CREATE UNIQUE INDEX idx_models_provider_upstream ON models(provider_id, upstream_id);
`,
	},
}

// KnownVersion 当前程序支持的最高 schema 版本。
func KnownVersion() int {
	return Migrations[len(Migrations)-1].Version
}

// Open 打开（或创建）SQLite 数据库并设置 WAL。
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1) // SQLite 单写入者
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Migrate 执行所有未应用的迁移。每个迁移在独立事务中执行并记录版本。
// 失败时该迁移回滚并返回错误，由调用方决定拒绝启动。
func Migrate(ctx context.Context, conn *sql.DB) (int, error) {
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			name        TEXT NOT NULL,
			applied_at  TEXT NOT NULL
		)`); err != nil {
		return 0, fmt.Errorf("db: create schema_migrations: %w", err)
	}
	applied, err := currentVersion(ctx, conn)
	if err != nil {
		return 0, err
	}
	// 未来 schema 拒绝写入：数据库版本高于程序已知版本时拒绝启动，
	// 防止旧程序向新结构写坏数据（对齐 MR-003 验收）。
	if known := KnownVersion(); applied > known {
		return applied, fmt.Errorf("db: schema version %d is newer than supported %d; upgrade the application before using this database", applied, known)
	}
	for _, m := range Migrations {
		if m.Version <= applied {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return applied, err
		}
		if _, err := tx.ExecContext(ctx, m.Up); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("db: migrate v%d %s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`,
			m.Version, m.Name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("db: record v%d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return applied, err
		}
		applied = m.Version
	}
	return applied, nil
}

func currentVersion(ctx context.Context, conn *sql.DB) (int, error) {
	var v int
	err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// CurrentVersion 只读查询当前 schema 版本。
func CurrentVersion(ctx context.Context, conn *sql.DB) (int, error) {
	return currentVersion(ctx, conn)
}

const initialSchema = `
CREATE TABLE providers (
	id          TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,            -- openai | anthropic | gemini | openai-compatible | codex | ...
	name        TEXT NOT NULL,
	base_url    TEXT NOT NULL DEFAULT '',
	capabilities TEXT NOT NULL DEFAULT '[]',
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);

CREATE TABLE accounts (
	id              TEXT PRIMARY KEY,
	provider_id     TEXT NOT NULL REFERENCES providers(id),
	name            TEXT NOT NULL,
	status          TEXT NOT NULL DEFAULT 'active',   -- active | disabled | error
	vault_provider  TEXT NOT NULL DEFAULT '',         -- SecretRef 引用，绝不存明文密钥
	secret_service  TEXT NOT NULL DEFAULT '',
	secret_account  TEXT NOT NULL DEFAULT '',
	secret_fingerprint TEXT NOT NULL DEFAULT '',
	last_verified_at TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX idx_accounts_provider ON accounts(provider_id);

CREATE TABLE models (
	id              TEXT PRIMARY KEY,
	provider_id     TEXT NOT NULL REFERENCES providers(id),
	upstream_id     TEXT NOT NULL,        -- 厂商原始模型 ID
	canonical_alias TEXT NOT NULL DEFAULT '',
	context_limit   INTEGER NOT NULL DEFAULT 0,
	capabilities    TEXT NOT NULL DEFAULT '[]',
	created_at      TEXT NOT NULL
);
CREATE INDEX idx_models_provider ON models(provider_id);

CREATE TABLE account_models (
	account_id      TEXT NOT NULL REFERENCES accounts(id),
	model_id        TEXT NOT NULL REFERENCES models(id),
	listed          INTEGER NOT NULL DEFAULT 0,
	probed          INTEGER NOT NULL DEFAULT 0,
	usable          INTEGER NOT NULL DEFAULT 0,
	last_checked_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (account_id, model_id)
);

CREATE TABLE quota_snapshots (
	id          TEXT PRIMARY KEY,
	account_id  TEXT NOT NULL REFERENCES accounts(id),
	quota_type  TEXT NOT NULL,             -- plan_5h | plan_week | day | month | rpm | tpm
	limit_value REAL NOT NULL,
	used        REAL NOT NULL,
	remaining   REAL NOT NULL,
	reset_at    TEXT NOT NULL DEFAULT '',
	source      TEXT NOT NULL,             -- official | observed | estimated | manual
	confidence  TEXT NOT NULL,             -- exact | reported | estimated | unavailable
	taken_at    TEXT NOT NULL
);
CREATE INDEX idx_quota_snapshots_account ON quota_snapshots(account_id, taken_at);

CREATE TABLE usage_events (
	id          TEXT PRIMARY KEY,
	request_id  TEXT NOT NULL,
	account_id  TEXT NOT NULL,
	model_id    TEXT NOT NULL,
	input_tokens   INTEGER NOT NULL DEFAULT 0,
	output_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_tokens   INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	status_code INTEGER NOT NULL DEFAULT 0,
	latency_ms  INTEGER NOT NULL DEFAULT 0,
	error_class TEXT NOT NULL DEFAULT '',
	occurred_at TEXT NOT NULL
);
CREATE INDEX idx_usage_events_account ON usage_events(account_id, occurred_at);
CREATE INDEX idx_usage_events_model ON usage_events(model_id, occurred_at);

CREATE TABLE health_samples (
	id          TEXT PRIMARY KEY,
	account_id  TEXT NOT NULL,
	model_id    TEXT NOT NULL,
	ttft_ms     INTEGER NOT NULL DEFAULT 0,
	latency_ms  INTEGER NOT NULL DEFAULT 0,
	status_code INTEGER NOT NULL DEFAULT 0,
	error_class TEXT NOT NULL DEFAULT '',
	sampled_at  TEXT NOT NULL
);
CREATE INDEX idx_health_samples_model ON health_samples(model_id, sampled_at);

CREATE TABLE routing_policies (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	alias       TEXT NOT NULL DEFAULT '',
	candidates  TEXT NOT NULL DEFAULT '[]',  -- 逻辑模型 → 候选
	priority    INTEGER NOT NULL DEFAULT 0,
	weight      INTEGER NOT NULL DEFAULT 1,
	enabled     INTEGER NOT NULL DEFAULT 1,
	created_at  TEXT NOT NULL
);

CREATE TABLE price_versions (
	id            TEXT PRIMARY KEY,
	provider_id   TEXT NOT NULL,
	model_id      TEXT NOT NULL,
	input_price   REAL NOT NULL DEFAULT 0,
	output_price  REAL NOT NULL DEFAULT 0,
	cache_price   REAL NOT NULL DEFAULT 0,
	currency      TEXT NOT NULL DEFAULT 'usd',
	effective_at  TEXT NOT NULL,
	source        TEXT NOT NULL DEFAULT 'manual'
);
CREATE INDEX idx_price_versions_model ON price_versions(model_id, effective_at);

CREATE TABLE audit_events (
	id          TEXT PRIMARY KEY,
	actor       TEXT NOT NULL DEFAULT '',
	action      TEXT NOT NULL,
	target      TEXT NOT NULL DEFAULT '',
	detail      TEXT NOT NULL DEFAULT '',
	occurred_at TEXT NOT NULL
);
CREATE INDEX idx_audit_events_at ON audit_events(occurred_at);
`
