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

// UpdateProvider 更新 Provider 名称/地址（MR-004）。
func (s *Store) UpdateProvider(ctx context.Context, id string, name, baseURL *string) error {
	p, err := s.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if name != nil && *name != "" {
		p.Name = *name
	}
	if baseURL != nil && *baseURL != "" {
		p.BaseURL = *baseURL
	}
	res, err := s.db.ExecContext(ctx, `UPDATE providers SET name=?, base_url=?, updated_at=? WHERE id=?`, p.Name, p.BaseURL, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
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

// SetAccountCredentialRef 原子切换账户的 SecretRef（凭据轮换；MR-004）。
func (s *Store) SetAccountCredentialRef(ctx context.Context, id, vaultProvider, service, account, fingerprint string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET vault_provider=?, secret_service=?, secret_account=?, secret_fingerprint=?, updated_at=?
		WHERE id=?`,
		vaultProvider, service, account, fingerprint, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAccountName 更新账户名称。
func (s *Store) SetAccountName(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE accounts SET name=?, updated_at=? WHERE id=?`, name, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
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

// DeleteRoutingPolicy 删除路由策略（upsert by id）。
func (s *Store) DeleteRoutingPolicy(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM routing_policies WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountAuditEvents 统计审计事件数（测试/巡检用）。
func (s *Store) CountAuditEvents(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&n)
	return n, err
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

// ListUsageEvents 按时间倒序列出最近 n 条用量事件（测试与统计入口）。
func (s *Store) ListUsageEvents(ctx context.Context, limit int) ([]UsageEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, request_id, account_id, model_id, input_tokens, output_tokens, cache_tokens, reasoning_tokens, status_code, latency_ms, error_class
		FROM usage_events ORDER BY occurred_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageEvent{}
	for rows.Next() {
		var e UsageEvent
		if err := rows.Scan(&e.ID, &e.RequestID, &e.AccountID, &e.ModelID,
			&e.InputTokens, &e.OutputTokens, &e.CacheTokens, &e.ReasoningTokens,
			&e.StatusCode, &e.LatencyMS, &e.ErrorClass); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
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

// -------- account_capabilities --------

// SaveAccountCapabilities 批量 upsert 账户能力矩阵。
func (s *Store) SaveAccountCapabilities(ctx context.Context, accountID string, caps []domain.AccountCapability) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range caps {
		if c.AccountID == "" {
			c.AccountID = accountID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_capabilities(account_id, capability, status, reason, connector_version, checked_at, updated_at)
			VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(account_id, capability) DO UPDATE SET
				status=excluded.status,
				reason=excluded.reason,
				connector_version=excluded.connector_version,
				checked_at=excluded.checked_at,
				updated_at=excluded.updated_at`,
			c.AccountID, string(c.Capability), string(c.Status), c.Reason, c.ConnectorVersion, c.CheckedAt, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAccountCapabilities 读取账户能力矩阵。
func (s *Store) ListAccountCapabilities(ctx context.Context, accountID string) ([]domain.AccountCapability, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, capability, status, reason, connector_version, checked_at
		FROM account_capabilities WHERE account_id=? ORDER BY capability`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AccountCapability{}
	for rows.Next() {
		var c domain.AccountCapability
		if err := rows.Scan(&c.AccountID, (*string)(&c.Capability), (*string)(&c.Status), &c.Reason, &c.ConnectorVersion, &c.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// -------- quota pools & snapshots（MR-009）--------

// SaveQuotaPool 保存额度池及成员（事务）。
func (s *Store) SaveQuotaPool(ctx context.Context, p domain.QuotaPool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_pools(id, provider_id, external_org, scope, created_at, updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET external_org=excluded.external_org, scope=excluded.scope, updated_at=excluded.updated_at`,
		p.ID, p.ProviderID, p.ExternalOrg, p.Scope, now(), now()); err != nil {
		return err
	}
	for _, acc := range p.MemberIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quota_pool_members(pool_id, account_id, model_scope) VALUES(?,?,'*')
			ON CONFLICT(pool_id, account_id) DO NOTHING`, p.ID, acc); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListQuotaPools 列出池及其成员。
func (s *Store) ListQuotaPools(ctx context.Context) ([]domain.QuotaPool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, provider_id, external_org, scope FROM quota_pools ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.QuotaPool{}
	for rows.Next() {
		var p domain.QuotaPool
		if err := rows.Scan(&p.ID, &p.ProviderID, &p.ExternalOrg, &p.Scope); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	// 成员
	for i := range out {
		mrows, err := s.db.QueryContext(ctx, `SELECT account_id FROM quota_pool_members WHERE pool_id=?`, out[i].ID)
		if err != nil {
			return nil, err
		}
		defer mrows.Close()
		for mrows.Next() {
			var a string
			if err := mrows.Scan(&a); err != nil {
				return nil, err
			}
			out[i].MemberIDs = append(out[i].MemberIDs, a)
		}
	}
	return out, rows.Err()
}

// GetQuotaPool 读取单个池。
func (s *Store) GetQuotaPool(ctx context.Context, id string) (domain.QuotaPool, error) {
	pools, err := s.ListQuotaPools(ctx)
	if err != nil {
		return domain.QuotaPool{}, err
	}
	for _, p := range pools {
		if p.ID == id {
			return p, nil
		}
	}
	return domain.QuotaPool{}, sql.ErrNoRows
}

// SaveQuotaSnapshot 写入一条快照（幂等：同 account+window+source+时间去重按 ID）。
func (s *Store) SaveQuotaSnapshot(ctx context.Context, snap domain.QuotaSnapshot) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO quota_snapshots(id, account_id, pool_id, window_type, limit_value, used, remaining,
			reset_at, source, source_ref, confidence, freshness, unit, connector_version, operator, manual_expires_at, taken_at, last_success_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			limit_value=excluded.limit_value, used=excluded.used, remaining=excluded.remaining,
			reset_at=excluded.reset_at, source=excluded.source, source_ref=excluded.source_ref,
			confidence=excluded.confidence, freshness=excluded.freshness,
			connector_version=excluded.connector_version, taken_at=excluded.taken_at, last_success_at=excluded.last_success_at`,
		snap.ID, snap.AccountID, snap.PoolID, string(snap.WindowType),
		nilFloat(snap.Limit), nilFloat(snap.Used), nilFloat(snap.Remaining),
		nilStr(snap.ResetAt), string(snap.Source), snap.SourceRef, string(snap.Confidence),
		string(snap.Freshness), snap.Unit, snap.ConnectorVersion, snap.Operator,
		strOrEmpty(snap.ManualExpiresAt), snap.TakenAt, snap.LastSuccessAt)
	return err
}

// ListQuotaSnapshots 按账户读取快照（可选池过滤）。
func (s *Store) ListQuotaSnapshots(ctx context.Context, accountID string) ([]domain.QuotaSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, pool_id, window_type, limit_value, used, remaining, reset_at,
			source, source_ref, confidence, freshness, unit, connector_version, operator, manual_expires_at, taken_at, last_success_at
		FROM quota_snapshots WHERE account_id=? ORDER BY taken_at DESC, window_type`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanQuotaSnapshots(rows)
}

// ListPoolSnapshots 按池读取快照（去重后）。
func (s *Store) ListPoolSnapshots(ctx context.Context, poolID string) ([]domain.QuotaSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, pool_id, window_type, limit_value, used, remaining, reset_at,
			source, source_ref, confidence, freshness, unit, connector_version, operator, manual_expires_at, taken_at, last_success_at
		FROM quota_snapshots WHERE pool_id=? ORDER BY taken_at DESC, window_type`, poolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanQuotaSnapshots(rows)
}

// LatestQuotaSnapshot 最新一条成功观测（用于“失败保留上次成功值”）。
func (s *Store) LatestQuotaSnapshot(ctx context.Context, accountID, windowType string) (domain.QuotaSnapshot, bool, error) {
	var snap domain.QuotaSnapshot
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_id, pool_id, window_type, limit_value, used, remaining, reset_at,
			source, source_ref, confidence, freshness, unit, connector_version, operator, manual_expires_at, taken_at, last_success_at
		FROM quota_snapshots
		WHERE account_id=? AND window_type=? AND source<>'manual'
		ORDER BY taken_at DESC LIMIT 1`,
		accountID, windowType).
		Scan(&snap.ID, &snap.AccountID, &snap.PoolID, (*string)(&snap.WindowType), nilFloatP(&snap.Limit), nilFloatP(&snap.Used), nilFloatP(&snap.Remaining),
			nilStrP(&snap.ResetAt), (*string)(&snap.Source), &snap.SourceRef, (*string)(&snap.Confidence),
			(*string)(&snap.Freshness), &snap.Unit, &snap.ConnectorVersion, &snap.Operator, nilStrP(&snap.ManualExpiresAt),
			&snap.TakenAt, &snap.LastSuccessAt)
	if err == sql.ErrNoRows {
		return domain.QuotaSnapshot{}, false, nil
	}
	if err != nil {
		return domain.QuotaSnapshot{}, false, err
	}
	return snap, true, nil
}

func scanQuotaSnapshots(rows *sql.Rows) ([]domain.QuotaSnapshot, error) {
	out := []domain.QuotaSnapshot{}
	for rows.Next() {
		var snap domain.QuotaSnapshot
		if err := rows.Scan(&snap.ID, &snap.AccountID, &snap.PoolID, (*string)(&snap.WindowType),
			nilFloatP(&snap.Limit), nilFloatP(&snap.Used), nilFloatP(&snap.Remaining),
			nilStrP(&snap.ResetAt), (*string)(&snap.Source), &snap.SourceRef, (*string)(&snap.Confidence),
			(*string)(&snap.Freshness), &snap.Unit, &snap.ConnectorVersion, &snap.Operator,
			nilStrP(&snap.ManualExpiresAt), &snap.TakenAt, &snap.LastSuccessAt); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// nilFloat / nilStr 供写入用（SQLite NULL）。
func nilFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nilStr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// strOrEmpty NOT NULL 文本列的空值占位。
func strOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// nilFloatP / nilStrP 供扫描用（NULL → nil 指针）。
func nilFloatP(v **float64) any { return v }
func nilStrP(v **string) any    { return v }

// CountQuotaSnapshotsByAccount 统计账户额度快照数（删除引用检查）。
func (s *Store) CountQuotaSnapshotsByAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_snapshots WHERE account_id=?`, accountID).Scan(&n)
	return n, err
}

// -------- refresh_jobs（MR-010）--------

// RefreshJob 后台刷新任务状态。
type RefreshJob struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	Capability    string `json:"capability"`
	State         string `json:"state"` // pending|running|success|failed|interrupted
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	NextRunAt     string `json:"next_run_at"`
	LastSuccessAt string `json:"last_success_at"`
	RetryCount    int    `json:"retry_count"`
	LastErrorCode string `json:"last_error_code"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// UpsertRefreshJob 创建或重置任务（按 account+capability 去重）。
func (s *Store) UpsertRefreshJob(ctx context.Context, j RefreshJob) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO refresh_jobs(id, account_id, capability, state, started_at, finished_at, next_run_at, last_success_at, retry_count, last_error_code, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, capability) DO UPDATE SET
			id=excluded.id, state=excluded.state, started_at=excluded.started_at,
			finished_at=excluded.finished_at, next_run_at=excluded.next_run_at,
			last_success_at=excluded.last_success_at, retry_count=excluded.retry_count,
			last_error_code=excluded.last_error_code, updated_at=excluded.updated_at`,
		j.ID, j.AccountID, j.Capability, j.State, j.StartedAt, j.FinishedAt, j.NextRunAt,
		j.LastSuccessAt, j.RetryCount, j.LastErrorCode, now(), now())
	return err
}

// GetRefreshJob 读取任务。
func (s *Store) GetRefreshJob(ctx context.Context, id string) (RefreshJob, error) {
	var j RefreshJob
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_id, capability, state, started_at, finished_at, next_run_at, last_success_at, retry_count, last_error_code, created_at, updated_at
		FROM refresh_jobs WHERE id=?`, id).
		Scan(&j.ID, &j.AccountID, &j.Capability, &j.State, &j.StartedAt, &j.FinishedAt, &j.NextRunAt,
			&j.LastSuccessAt, &j.RetryCount, &j.LastErrorCode, &j.CreatedAt, &j.UpdatedAt)
	return j, err
}

// GetRefreshJobByKey 按账户+能力查任务。
func (s *Store) GetRefreshJobByKey(ctx context.Context, accountID, capability string) (RefreshJob, bool, error) {
	var j RefreshJob
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_id, capability, state, started_at, finished_at, next_run_at, last_success_at, retry_count, last_error_code, created_at, updated_at
		FROM refresh_jobs WHERE account_id=? AND capability=?`, accountID, capability).
		Scan(&j.ID, &j.AccountID, &j.Capability, &j.State, &j.StartedAt, &j.FinishedAt, &j.NextRunAt,
			&j.LastSuccessAt, &j.RetryCount, &j.LastErrorCode, &j.CreatedAt, &j.UpdatedAt)
	if err == sql.ErrNoRows {
		return RefreshJob{}, false, nil
	}
	if err != nil {
		return RefreshJob{}, false, err
	}
	return j, true, nil
}

// UpdateRefreshJobState 更新任务状态字段。
func (s *Store) UpdateRefreshJobState(ctx context.Context, id, state, finishedAt, nextRunAt, lastSuccessAt string, retryCount int, lastErrorCode string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE refresh_jobs SET state=?, finished_at=?, next_run_at=?, last_success_at=?, retry_count=?, last_error_code=?, updated_at=?
		WHERE id=?`,
		state, finishedAt, nextRunAt, lastSuccessAt, retryCount, lastErrorCode, now(), id)
	return err
}

// ListRefreshJobs 列出任务（可加账户过滤）。
func (s *Store) ListRefreshJobs(ctx context.Context, accountID string) ([]RefreshJob, error) {
	q := `SELECT id, account_id, capability, state, started_at, finished_at, next_run_at, last_success_at, retry_count, last_error_code, created_at, updated_at FROM refresh_jobs`
	args := []any{}
	if accountID != "" {
		q += ` WHERE account_id=?`
		args = append(args, accountID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RefreshJob{}
	for rows.Next() {
		var j RefreshJob
		if err := rows.Scan(&j.ID, &j.AccountID, &j.Capability, &j.State, &j.StartedAt, &j.FinishedAt, &j.NextRunAt,
			&j.LastSuccessAt, &j.RetryCount, &j.LastErrorCode, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkInterruptedRefreshJobs 重启时将遗留 running/pending 标为 interrupted。
func (s *Store) MarkInterruptedRefreshJobs(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE refresh_jobs SET state='interrupted', updated_at=? WHERE state IN ('running','pending')`, now())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// -------- request_attempts（MR-015）--------

// RequestAttempt 一次真实上游尝试。
type RequestAttempt struct {
	ID               string `json:"id"`
	RequestID        string `json:"request_id"`
	AttemptID        string `json:"attempt_id"`
	AccountID        string `json:"account_id"`
	ProviderID       string `json:"provider_id"`
	LogicalModel     string `json:"logical_model"`
	ActualModel      string `json:"actual_model"`
	Protocol         string `json:"protocol"`
	AccessTokenID    string `json:"access_token_id,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
	Status           string `json:"status"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	Metering         string `json:"metering"`
	LatencyMS        int64  `json:"latency_ms"`
	StatusCode       int    `json:"status_code"`
	ErrorClass       string `json:"error_class,omitempty"`
	ReasonCode       string `json:"reason_code,omitempty"`
	PriceVersion     string `json:"price_version,omitempty"`
	OccurredAt       string `json:"occurred_at"`
	FinishedAt       string `json:"finished_at,omitempty"`
}

// CreateAttempt 记录一次尝试的开始（幂等：同 request+attempt 冲突时忽略）。
func (s *Store) CreateAttempt(ctx context.Context, a RequestAttempt) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_attempts(id, request_id, attempt_id, account_id, provider_id, logical_model, actual_model,
			protocol, access_token_id, project_id, status, occurred_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(request_id, attempt_id) DO NOTHING`,
		a.ID, a.RequestID, a.AttemptID, a.AccountID, a.ProviderID, a.LogicalModel, a.ActualModel,
		a.Protocol, a.AccessTokenID, a.ProjectID, "started", now())
	return err
}

// FinishAttempt 幂等更新尝试终态（同一 request+attempt 重复提交不重复计费）。
func (s *Store) FinishAttempt(ctx context.Context, a RequestAttempt) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE request_attempts SET
			status=?, input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_write_tokens=?,
			reasoning_tokens=?, metering=?, latency_ms=?, status_code=?, error_class=?, reason_code=?,
			actual_model=CASE WHEN ?<>'' THEN ? ELSE actual_model END, finished_at=?
		WHERE request_id=? AND attempt_id=?`,
		a.Status, a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens,
		a.ReasoningTokens, a.Metering, a.LatencyMS, a.StatusCode, a.ErrorClass, a.ReasonCode,
		a.ActualModel, a.ActualModel, now(), a.RequestID, a.AttemptID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListAttemptsByRequest 查询一个逻辑请求的全部尝试。
func (s *Store) ListAttemptsByRequest(ctx context.Context, requestID string) ([]RequestAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, request_id, attempt_id, account_id, provider_id, logical_model, actual_model, protocol,
			access_token_id, project_id, status, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			reasoning_tokens, metering, latency_ms, status_code, error_class, reason_code, price_version, occurred_at, finished_at
		FROM request_attempts WHERE request_id=? ORDER BY occurred_at, attempt_id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestAttempt{}
	for rows.Next() {
		var a RequestAttempt
		if err := rows.Scan(&a.ID, &a.RequestID, &a.AttemptID, &a.AccountID, &a.ProviderID, &a.LogicalModel,
			&a.ActualModel, &a.Protocol, &a.AccessTokenID, &a.ProjectID, &a.Status, &a.InputTokens, &a.OutputTokens,
			&a.CacheReadTokens, &a.CacheWriteTokens, &a.ReasoningTokens, &a.Metering, &a.LatencyMS, &a.StatusCode,
			&a.ErrorClass, &a.ReasonCode, &a.PriceVersion, &a.OccurredAt, &a.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListRecentAttempts 列出近期尝试（请求列表页）。
func (s *Store) ListRecentAttempts(ctx context.Context, limit int) ([]RequestAttempt, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, request_id, attempt_id, account_id, provider_id, logical_model, actual_model, protocol,
			access_token_id, project_id, status, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			reasoning_tokens, metering, latency_ms, status_code, error_class, reason_code, price_version, occurred_at, finished_at
		FROM request_attempts ORDER BY occurred_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestAttempt{}
	for rows.Next() {
		var a RequestAttempt
		if err := rows.Scan(&a.ID, &a.RequestID, &a.AttemptID, &a.AccountID, &a.ProviderID, &a.LogicalModel,
			&a.ActualModel, &a.Protocol, &a.AccessTokenID, &a.ProjectID, &a.Status, &a.InputTokens, &a.OutputTokens,
			&a.CacheReadTokens, &a.CacheWriteTokens, &a.ReasoningTokens, &a.Metering, &a.LatencyMS, &a.StatusCode,
			&a.ErrorClass, &a.ReasonCode, &a.PriceVersion, &a.OccurredAt, &a.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkStaleAttemptsInterrupted 重启时把遗留 started 尝试标为 interrupted（结果未知）。
func (s *Store) MarkStaleAttemptsInterrupted(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE request_attempts SET status='interrupted', metering='unknown', finished_at=?
		WHERE status='started'`, now())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// -------- access_tokens --------

// tokenColumns 是令牌查询列清单（含 migration v4 作用域字段）。
const tokenColumns = `id, name, key_prefix, enabled, created_at, last_used_at, project_id, model_whitelist, expires_at, max_concurrency`

// CreateToken 写入推理令牌（只存哈希与前缀）。
func (s *Store) CreateToken(ctx context.Context, t domain.Token) error {
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	if t.ModelWhitelist == "" {
		t.ModelWhitelist = "[]"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO access_tokens(id, name, key_hash, key_prefix, enabled, created_at, last_used_at, project_id, model_whitelist, expires_at, max_concurrency)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.KeyHash, t.KeyPrefix, enabled, t.CreatedAt, t.LastUsedAt,
		t.ProjectID, t.ModelWhitelist, t.ExpiresAt, t.MaxConcurrency)
	return err
}

// ListTokens 列出令牌（不含哈希原文）。
func (s *Store) ListTokens(ctx context.Context) ([]domain.Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenColumns+` FROM access_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Token{}
	for rows.Next() {
		var t domain.Token
		if err := scanToken(rows.Scan, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanToken 按列序写入令牌（Row/Rows 共用）。
func scanToken(scan func(dest ...any) error, t *domain.Token) error {
	var enabled int
	if err := scan(&t.ID, &t.Name, &t.KeyPrefix, &enabled, &t.CreatedAt, &t.LastUsedAt,
		&t.ProjectID, &t.ModelWhitelist, &t.ExpiresAt, &t.MaxConcurrency); err != nil {
		return err
	}
	t.Enabled = enabled == 1
	return nil
}

// SetTokenEnabled 启用/禁用令牌。
func (s *Store) SetTokenEnabled(ctx context.Context, id string, enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE access_tokens SET enabled=? WHERE id=?`, e, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteToken 删除令牌。
func (s *Store) DeleteToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM access_tokens WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// FindTokenByKeyHash 按哈希查找令牌（含禁用令牌，由调用方判定可用性）。
func (s *Store) FindTokenByKeyHash(ctx context.Context, keyHash string) (domain.Token, error) {
	var t domain.Token
	row := s.db.QueryRowContext(ctx,
		`SELECT `+tokenColumns+` FROM access_tokens WHERE key_hash=?`, keyHash)
	if err := scanToken(row.Scan, &t); err != nil {
		return domain.Token{}, err
	}
	return t, nil
}

// TouchToken 更新最近使用时间。
func (s *Store) TouchToken(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE access_tokens SET last_used_at=? WHERE id=?`, now(), id)
	return err
}

// -------- projects（MR-016）--------

// CreateProject 插入项目。
func (s *Store) CreateProject(ctx context.Context, p domain.Project) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects(id, name, created_at, updated_at) VALUES(?,?,?,?)`,
		p.ID, p.Name, now(), now())
	return err
}

// ListProjects 列出项目。
func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, updated_at FROM projects ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Project{}
	for rows.Next() {
		var p domain.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject 获取单个项目。
func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	var p domain.Project
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at, updated_at FROM projects WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// DeleteProject 删除项目（引用检查由调用方完成）。
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// -------- response_bindings（MR-013）--------

// SaveResponseBinding 记录 Responses 会话句柄绑定（upsert）。
func (s *Store) SaveResponseBinding(ctx context.Context, b domain.ResponseBinding) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO response_bindings(response_id, account_id, model_id, created_at, expires_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(response_id) DO UPDATE SET expires_at=excluded.expires_at`,
		b.ResponseID, b.AccountID, b.ModelID, now(), b.ExpiresAt)
	return err
}

// GetResponseBinding 查询绑定；已过期返回 sql.ErrNoRows（过期即不可续接）。
func (s *Store) GetResponseBinding(ctx context.Context, responseID string) (domain.ResponseBinding, error) {
	var b domain.ResponseBinding
	err := s.db.QueryRowContext(ctx,
		`SELECT response_id, account_id, model_id, created_at, expires_at FROM response_bindings WHERE response_id=?`,
		responseID).Scan(&b.ResponseID, &b.AccountID, &b.ModelID, &b.CreatedAt, &b.ExpiresAt)
	if err != nil {
		return domain.ResponseBinding{}, err
	}
	if b.ExpiresAt != "" {
		if exp, perr := time.Parse(time.RFC3339, b.ExpiresAt); perr == nil && time.Now().UTC().After(exp) {
			return domain.ResponseBinding{}, sql.ErrNoRows
		}
	}
	return b, nil
}

// -------- 审计 --------

// RecordAuditEvent 记录管理操作审计（不含密钥）。
func (s *Store) RecordAuditEvent(ctx context.Context, actor, action, target, detail string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events(id, actor, action, target, detail, occurred_at)
		VALUES(?,?,?,?,?,?)`,
		"aud_"+fmt.Sprintf("%d", time.Now().UnixNano()), actor, action, target, detail, now())
	return err
}

// -------- 引用计数（删除冲突检查，MR-004）--------

// CountAccountsByProvider 统计某 Provider 下的账户数。
func (s *Store) CountAccountsByProvider(ctx context.Context, providerID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE provider_id=?`, providerID).Scan(&n)
	return n, err
}

// DeleteAccount 删除账户（无历史引用时）。
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteProvider 删除 Provider。
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountUsageEventsByAccount 统计账户历史用量事件数。
func (s *Store) CountUsageEventsByAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events WHERE account_id=?`, accountID).Scan(&n)
	return n, err
}

// CountRoutingCandidatesByAccount 统计路由策略中引用某账户的候选数。
func (s *Store) CountRoutingCandidatesByAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	rows, err := s.db.QueryContext(ctx, `SELECT candidates FROM routing_policies`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var cands string
		if err := rows.Scan(&cands); err != nil {
			return 0, err
		}
		var list []domain.Candidate
		if err := json.Unmarshal([]byte(cands), &list); err != nil {
			continue
		}
		for _, c := range list {
			if c.AccountID == accountID {
				n++
			}
		}
	}
	return n, rows.Err()
}
