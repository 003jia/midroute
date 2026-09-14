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
	{
		Version: 3,
		Name:    "access_tokens",
		Up: `
-- 推理访问令牌（PRD M2 Token 数据模型；MR-016）
-- 只保存 SHA-256 哈希与前缀，明文仅在创建响应中出现一次。
CREATE TABLE access_tokens (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	key_hash    TEXT NOT NULL UNIQUE,
	key_prefix  TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	created_at  TEXT NOT NULL,
	last_used_at TEXT NOT NULL DEFAULT ''
);
`,
	},
	{
		Version: 4,
		Name:    "projects_token_scoping_and_response_bindings",
		Up: `
-- 项目与令牌作用域（MR-016）：项目归属、模型白名单、到期、并发上限。
-- project_id 为空表示未分组的默认作用域，兼容 v3 存量令牌。
ALTER TABLE access_tokens ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
ALTER TABLE access_tokens ADD COLUMN model_whitelist TEXT NOT NULL DEFAULT '[]';
ALTER TABLE access_tokens ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE access_tokens ADD COLUMN max_concurrency INTEGER NOT NULL DEFAULT 0;

CREATE TABLE projects (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);

-- Responses 协议会话句柄绑定（MR-013）：previous_response_id 只能回到
-- 产生它的账户，过期后拒绝而非换账户续接。
CREATE TABLE response_bindings (
	response_id TEXT PRIMARY KEY,
	account_id  TEXT NOT NULL,
	model_id    TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	expires_at  TEXT NOT NULL
);
CREATE INDEX idx_response_bindings_account ON response_bindings(account_id, created_at);
`,
	},
	{
		Version: 5,
		Name:    "account_capabilities",
		Up: `
-- 账户能力矩阵（MR-006）：每账户每能力的状态、原因、连接器版本与检查时间。
-- 平台声明支持 ≠ 当前凭据权限足够；不保存原始认证响应。
CREATE TABLE account_capabilities (
	account_id       TEXT NOT NULL REFERENCES accounts(id),
	capability       TEXT NOT NULL,
	status           TEXT NOT NULL,
	reason           TEXT NOT NULL DEFAULT '',
	connector_version TEXT NOT NULL DEFAULT '',
	checked_at       TEXT NOT NULL,
	updated_at       TEXT NOT NULL,
	PRIMARY KEY (account_id, capability)
);
`,
	},
	{
		Version: 6,
		Name:    "quota_pools_and_snapshot_semantics",
		Up: `
-- 共享额度池（MR-009）：多个 Key 共用同一官方额度时只统计一次。
CREATE TABLE quota_pools (
	id          TEXT PRIMARY KEY,
	provider_id TEXT NOT NULL REFERENCES providers(id),
	external_org TEXT NOT NULL DEFAULT '',
	scope       TEXT NOT NULL DEFAULT 'account',  -- account | org
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE TABLE quota_pool_members (
	pool_id     TEXT NOT NULL REFERENCES quota_pools(id),
	account_id  TEXT NOT NULL REFERENCES accounts(id),
	model_scope TEXT NOT NULL DEFAULT '*',
	PRIMARY KEY (pool_id, account_id)
);

-- 快照语义重建（MR-009）：未知=null，零=0；来源/可信度/新鲜度分列。
CREATE TABLE quota_snapshots_new (
	id                  TEXT PRIMARY KEY,
	account_id          TEXT NOT NULL REFERENCES accounts(id),
	pool_id             TEXT NOT NULL DEFAULT '',
	window_type         TEXT NOT NULL,          -- primary | secondary | additional-<名> | code-review
	limit_value         REAL,
	used                REAL,
	remaining           REAL,
	reset_at            TEXT,
	source              TEXT NOT NULL DEFAULT 'observed',  -- official|reported|observed|estimated|manual
	source_ref          TEXT NOT NULL DEFAULT '',
	confidence          TEXT NOT NULL DEFAULT 'reported',  -- exact|reported|estimated|unavailable
	freshness           TEXT NOT NULL DEFAULT 'unknown',   -- fresh|stale|unknown
	unit                TEXT NOT NULL DEFAULT 'percent',   -- percent|tokens|requests|currency
	connector_version   TEXT NOT NULL DEFAULT '',
	operator            TEXT NOT NULL DEFAULT '',
	manual_expires_at   TEXT NOT NULL DEFAULT '',
	taken_at            TEXT NOT NULL,
	last_success_at     TEXT NOT NULL DEFAULT '',
	CHECK (pool_id <> '' OR window_type <> '')
);
INSERT INTO quota_snapshots_new(id, account_id, window_type, limit_value, used, remaining, reset_at, source, confidence, taken_at)
	SELECT id, account_id, quota_type, limit_value, used, remaining,
		CASE WHEN reset_at = '' THEN NULL ELSE reset_at END,
		source, confidence, taken_at
	FROM quota_snapshots;
DROP TABLE quota_snapshots;
ALTER TABLE quota_snapshots_new RENAME TO quota_snapshots;
CREATE INDEX idx_quota_snapshots_account ON quota_snapshots(account_id, taken_at);
CREATE INDEX idx_quota_snapshots_pool ON quota_snapshots(pool_id, taken_at);
`,
	},
	{
		Version: 7,
		Name:    "refresh_jobs",
		Up: `
-- 后台刷新任务（MR-010）：账户+能力去重，状态机，退避，重启中断标记。
CREATE TABLE refresh_jobs (
	id               TEXT PRIMARY KEY,
	account_id       TEXT NOT NULL,
	capability       TEXT NOT NULL,           -- models | capabilities | health | quota
	state            TEXT NOT NULL DEFAULT 'pending',  -- pending|running|success|failed|interrupted
	started_at       TEXT NOT NULL DEFAULT '',
	finished_at      TEXT NOT NULL DEFAULT '',
	next_run_at      TEXT NOT NULL DEFAULT '',
	last_success_at  TEXT NOT NULL DEFAULT '',
	retry_count      INTEGER NOT NULL DEFAULT 0,
	last_error_code  TEXT NOT NULL DEFAULT '',
	created_at       TEXT NOT NULL,
	updated_at       TEXT NOT NULL,
	UNIQUE (account_id, capability)
);
`,
	},
	{
		Version: 8,
		Name:    "request_attempts",
		Up: `
-- 请求尝试（MR-015）：一个逻辑请求的每次真实上游尝试单独记录。
-- 幂等键 (request_id, attempt_id)；不保存正文；终态区分成功/失败/取消/部分/未知。
CREATE TABLE request_attempts (
	id                 TEXT PRIMARY KEY,
	request_id         TEXT NOT NULL,
	attempt_id         TEXT NOT NULL,
	account_id         TEXT NOT NULL DEFAULT '',
	provider_id        TEXT NOT NULL DEFAULT '',
	logical_model      TEXT NOT NULL DEFAULT '',
	actual_model       TEXT NOT NULL DEFAULT '',
	protocol           TEXT NOT NULL DEFAULT 'chat.completions',
	access_token_id    TEXT NOT NULL DEFAULT '',
	project_id         TEXT NOT NULL DEFAULT '',
	status             TEXT NOT NULL DEFAULT 'started',  -- started|success|failed|cancelled|partial|unknown|interrupted
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
	metering           TEXT NOT NULL DEFAULT 'unknown',  -- exact|reported|estimated|incomplete|unknown
	latency_ms         INTEGER NOT NULL DEFAULT 0,
	status_code        INTEGER NOT NULL DEFAULT 0,
	error_class        TEXT NOT NULL DEFAULT '',
	reason_code        TEXT NOT NULL DEFAULT '',
	price_version      TEXT NOT NULL DEFAULT '',
	occurred_at        TEXT NOT NULL,
	finished_at        TEXT NOT NULL DEFAULT '',
	UNIQUE (request_id, attempt_id)
);
CREATE INDEX idx_request_attempts_request ON request_attempts(request_id);
CREATE INDEX idx_request_attempts_account ON request_attempts(account_id, occurred_at);
`,
	},
	{
		Version: 9,
		Name:    "price_versions_fixed_point",
		Up: `
-- 价格定点化（MR-019）：nano-USD/令牌（int64），避免 float 累加误差。
-- 旧的 REAL 列保留兼容，不再用于新计算。
ALTER TABLE price_versions ADD COLUMN input_price_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE price_versions ADD COLUMN output_price_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE price_versions ADD COLUMN cache_read_price_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE price_versions ADD COLUMN cache_write_price_nano INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_price_versions_model_time ON price_versions(model_id, effective_at);
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
