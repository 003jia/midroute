// Package repository 提供 SQLite 数据访问（对齐 PRD §6.2 数据表与 US-011 migration 约束）。
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"midroute/internal/domain"
)

// Store 封装 SQLite 访问。同一连接由调用方持有（SQLite 单写入者）。
type Store struct {
	db *sql.DB
}

// New 创建 Store。
func New(db *sql.DB) *Store { return &Store{db: db} }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// -------- providers --------

// CreateProvider 插入 Provider。
func (s *Store) CreateProvider(ctx context.Context, p domain.Provider) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO providers(id, kind, name, base_url, capabilities, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)`,
		p.ID, p.Kind, p.Name, p.BaseURL, p.Capabilities, now(), now())
	return err
}

// ListProviders 列出所有 Provider。
func (s *Store) ListProviders(ctx context.Context) ([]domain.Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, name, base_url, capabilities FROM providers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Provider{}
	for rows.Next() {
		var p domain.Provider
		if err := rows.Scan(&p.ID, &p.Kind, &p.Name, &p.BaseURL, &p.Capabilities); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider 获取单个 Provider。
func (s *Store) GetProvider(ctx context.Context, id string) (domain.Provider, error) {
	var p domain.Provider
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, name, base_url, capabilities FROM providers WHERE id=?`, id).
		Scan(&p.ID, &p.Kind, &p.Name, &p.BaseURL, &p.Capabilities)
	return p, err
}

// -------- accounts --------

// CreateAccount 插入 Account（仅存 SecretRef 引用）。
func (s *Store) CreateAccount(ctx context.Context, a domain.Account) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts(id, provider_id, name, status, mode, auth_type, billing_mode, auth_state, vault_provider, secret_service, secret_account, secret_fingerprint, last_verified_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ProviderID, a.Name, a.Status, a.Mode, a.AuthType, a.BillingMode, a.AuthState,
		a.VaultProvider, a.SecretService, a.SecretAccount, a.SecretFingerprint, a.LastVerifiedAt, now(), now())
	return err
}

// accountColumns 是账户查询的列清单，供 List/Get 共用。
const accountColumns = `id, provider_id, name, status, mode, auth_type, billing_mode, auth_state, vault_provider, secret_service, secret_account, secret_fingerprint, last_verified_at`

// ListAccounts 列出账户。
func (s *Store) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Account{}
	for rows.Next() {
		var a domain.Account
		if err := scanAccount(rows.Scan, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 获取单个账户。
func (s *Store) GetAccount(ctx context.Context, id string) (domain.Account, error) {
	var a domain.Account
	row := s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=?`, id)
	if err := scanAccount(row.Scan, &a); err != nil {
		return domain.Account{}, err
	}
	return a, nil
}

// scanAccount 把一行账户数据按列序写入 a（Scanner 为 *sql.Row 或 *sql.Rows 的 Scan）。
func scanAccount(scan func(dest ...any) error, a *domain.Account) error {
	return scan(&a.ID, &a.ProviderID, &a.Name, &a.Status, &a.Mode, &a.AuthType, &a.BillingMode, &a.AuthState,
		&a.VaultProvider, &a.SecretService, &a.SecretAccount, &a.SecretFingerprint, &a.LastVerifiedAt)
}

// SetAccountStatus 更新账户状态。
func (s *Store) SetAccountStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET status=?, updated_at=? WHERE id=?`, status, now(), id)
	return err
}

// SetAccountAuthState 更新认证可用状态（不改动启停状态，二者语义分列）。
func (s *Store) SetAccountAuthState(ctx context.Context, id, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET auth_state=?, updated_at=? WHERE id=?`, state, now(), id)
	return err
}

// UpdateAccountSecretRef 原子切换账户的 SecretRef 引用（OAuth 刷新旋转凭据后调用）。
func (s *Store) UpdateAccountSecretRef(ctx context.Context, id string, ref domain.Account) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET vault_provider=?, secret_service=?, secret_account=?, secret_fingerprint=?, updated_at=?
		WHERE id=?`,
		ref.VaultProvider, ref.SecretService, ref.SecretAccount, ref.SecretFingerprint, now(), id)
	return err
}

// SetAccountVerifiedAt 更新最近校验时间。
func (s *Store) SetAccountVerifiedAt(ctx context.Context, id, at string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_verified_at=?, updated_at=? WHERE id=?`, at, now(), id)
	return err
}

// -------- models --------

// UpsertModels 批量写入模型（幂等，按 provider_id + upstream_id 去重）。
func (s *Store) UpsertModels(ctx context.Context, models []domain.Model) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range models {
		if m.ID == "" {
			m.ID = fmt.Sprintf("%s|%s", m.ProviderID, m.UpstreamID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO models(id, provider_id, upstream_id, canonical_alias, context_limit, capabilities, created_at)
			VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
				context_limit=excluded.context_limit,
				capabilities=excluded.capabilities`,
			m.ID, m.ProviderID, m.UpstreamID, m.CanonicalAlias, m.ContextLimit, m.Capabilities, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListModels 列出模型。
func (s *Store) ListModels(ctx context.Context) ([]domain.Model, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, provider_id, upstream_id, canonical_alias, context_limit, capabilities FROM models ORDER BY provider_id, upstream_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Model{}
	for rows.Next() {
		var m domain.Model
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.UpstreamID, &m.CanonicalAlias, &m.ContextLimit, &m.Capabilities); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// -------- account_models --------

// SaveAccountModels 记录账户可用模型。
func (s *Store) SaveAccountModels(ctx context.Context, accountID string, infos []ModelInfoRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, mi := range infos {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_models(account_id, model_id, listed, probed, usable, last_checked_at)
			VALUES(?,?,?,?,?,?)
			ON CONFLICT(account_id, model_id) DO UPDATE SET
				listed=excluded.listed, probed=excluded.probed, usable=excluded.usable, last_checked_at=excluded.last_checked_at`,
			accountID, mi.ModelID, mi.Listed, mi.Probed, mi.Usable, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ModelInfoRow 账户模型行。
type ModelInfoRow struct {
	ModelID string
	Listed  bool
	Probed  bool
	Usable  bool
}

// CountAccountModel 统计账户是否拥有某模型。
func (s *Store) CountAccountModel(ctx context.Context, accountID, modelID string, out *int) error {
	return s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_models WHERE account_id=? AND model_id=?`, accountID, modelID).Scan(out)
}

// -------- routing_policies --------

// SaveRoutingPolicy 写入路由策略（upsert by alias）。
func (s *Store) SaveRoutingPolicy(ctx context.Context, p domain.RoutingPolicy) error {
	cands, err := json.Marshal(p.Candidates)
	if err != nil {
		return err
	}
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO routing_policies(id, name, alias, candidates, priority, weight, enabled, created_at)
		VALUES(?,?,?,?,0,1,?,?)
		ON CONFLICT(id) DO UPDATE SET candidates=excluded.candidates, enabled=excluded.enabled`,
		p.ID, p.Name, p.Alias, string(cands), enabled, now())
	return err
}

// ListRoutingPolicies 列出路由策略。
func (s *Store) ListRoutingPolicies(ctx context.Context) ([]domain.RoutingPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, alias, candidates, enabled FROM routing_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RoutingPolicy{}
	for rows.Next() {
		var p domain.RoutingPolicy
		var cands string
		var enabled int
		if err := rows.Scan(&p.ID, &p.Name, &p.Alias, &cands, &enabled); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cands), &p.Candidates); err != nil {
			return nil, err
		}
		p.Enabled = enabled == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetRoutingPolicyByAlias 按逻辑模型名查路由策略。
func (s *Store) GetRoutingPolicyByAlias(ctx context.Context, alias string) (domain.RoutingPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, alias, candidates, enabled FROM routing_policies WHERE alias=?`, alias)
	if err != nil {
		return domain.RoutingPolicy{}, err
	}
	defer rows.Close()
	var p domain.RoutingPolicy
	if !rows.Next() {
		return domain.RoutingPolicy{}, sql.ErrNoRows
	}
	var cands string
	var enabled int
	if err := rows.Scan(&p.ID, &p.Name, &p.Alias, &cands, &enabled); err != nil {
		return domain.RoutingPolicy{}, err
	}
	if err := json.Unmarshal([]byte(cands), &p.Candidates); err != nil {
		return domain.RoutingPolicy{}, err
	}
	p.Enabled = enabled == 1
	return p, nil
}

// -------- usage_events --------

// RecordUsageEvent 记录请求用量元数据（不保存正文）。
func (s *Store) RecordUsageEvent(ctx context.Context, e UsageEvent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_events(id, request_id, account_id, model_id, input_tokens, output_tokens, cache_tokens, reasoning_tokens, status_code, latency_ms, error_class, occurred_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.RequestID, e.AccountID, e.ModelID, e.InputTokens, e.OutputTokens, e.CacheTokens, e.ReasoningTokens, e.StatusCode, e.LatencyMS, e.ErrorClass, now())
	return err
}

// UsageEvent 用量事件（元数据）。
type UsageEvent struct {
	ID              string
	RequestID       string
	AccountID       string
	ModelID         string
	InputTokens     int64
	OutputTokens    int64
	CacheTokens     int64
	ReasoningTokens int64
	StatusCode      int
	LatencyMS       int64
	ErrorClass      string
}
