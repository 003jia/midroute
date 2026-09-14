// 管理 API 类型化客户端。密钥仅存在于创建/轮换请求体内，不进入任何持久存储。

export type ApiError = { code: string; message: string; request_id?: string; retryable?: boolean }

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    headers: { 'content-type': 'application/json' },
    ...init,
  })
  if (!resp.ok) {
    let err: ApiError = { code: 'unknown', message: `HTTP ${resp.status}` }
    try {
      const body = (await resp.json()) as { error?: ApiError }
      if (body?.error) err = body.error
    } catch {
      /* ignore parse failure */
    }
    throw new Error(err.message || err.code)
  }
  if (resp.status === 204) return undefined as T
  return (await resp.json()) as T
}

export type Provider = { id: string; kind: string; name: string; base_url: string }

export type Account = {
  id: string
  provider_id: string
  name: string
  status: string
  mode: string
  auth_type: string
  billing_mode: string
  auth_state: string
  secret_fingerprint: string
  last_verified_at: string
}

export type AccountCapability = {
  account_id: string
  capability: string
  status: string
  reason?: string
  connector_version: string
  checked_at: string
}

export type Model = {
  id: string
  provider_id: string
  upstream_id: string
  canonical_alias?: string
  context_limit: number
}

export type QuotaSnapshot = {
  id: string
  account_id: string
  pool_id?: string
  window_type: string
  limit?: number | null
  used?: number | null
  remaining?: number | null
  reset_at?: string | null
  source: string
  source_ref?: string
  confidence: string
  freshness: string
  unit: string
  connector_version: string
  operator?: string
  taken_at: string
}

export type QuotaPool = {
  id: string
  provider_id: string
  external_org?: string
  scope: string
  member_ids: string[]
}

export type RefreshJob = {
  id: string
  account_id: string
  capability: string
  state: string
  started_at: string
  finished_at?: string
  next_run_at?: string
  last_success_at?: string
  retry_count: number
  last_error_code?: string
}

export const api = {
  providers: {
    list: () => request<{ data: Provider[] }>('/api/v1/providers'),
    create: (body: { kind: string; name: string; base_url?: string }) =>
      request<{ id: string }>('/api/v1/providers', { method: 'POST', body: JSON.stringify(body) }),
  },
  accounts: {
    list: () => request<{ data: Account[] }>('/api/v1/accounts'),
    create: (body: { provider_id: string; name?: string; mode?: string; api_key: string }) =>
      request<{ id: string; secret_fingerprint: string }>('/api/v1/accounts', {
        method: 'POST',
        body: JSON.stringify(body),
      }),
    get: (id: string) => request<Account>(`/api/v1/accounts/${id}`),
    patch: (id: string, body: { name?: string; status?: string }) =>
      request<Account>(`/api/v1/accounts/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
    del: (id: string) => request<{ ok: boolean }>(`/api/v1/accounts/${id}`, { method: 'DELETE' }),
    verify: (id: string) =>
      request<{ ok: boolean; verified_at: string }>(`/api/v1/accounts/${id}/verify`, { method: 'POST' }),
    discover: (id: string) =>
      request<{ ok: boolean; count: number }>(`/api/v1/accounts/${id}/discover`, { method: 'POST' }),
    capabilities: (id: string) =>
      request<{ data: AccountCapability[] }>(`/api/v1/accounts/${id}/capabilities`, { method: 'POST' }),
    rotateCredential: (id: string, apiKey: string) =>
      request<{ ok: boolean; secret_fingerprint: string }>(`/api/v1/accounts/${id}/credential`, {
        method: 'POST',
        body: JSON.stringify({ api_key: apiKey }),
      }),
    quota: (id: string) => request<{ data: QuotaSnapshot[] }>(`/api/v1/accounts/${id}/quota`),
    manualQuota: (id: string, body: { window_type: string; used?: number | null; limit?: number | null; remaining?: number | null; unit?: string }) =>
      request<QuotaSnapshot>(`/api/v1/accounts/${id}/quota-manual`, { method: 'PUT', body: JSON.stringify(body) }),
    refresh: (id: string, capability: string) =>
      request<{ job_id: string; state: string }>(`/api/v1/accounts/${id}/refresh`, {
        method: 'POST',
        body: JSON.stringify({ capability }),
      }),
  },
  models: {
    list: () => request<{ data: Model[] }>('/api/v1/models'),
  },
  quotaPools: {
    list: () => request<{ data: QuotaPool[] }>('/api/v1/quota-pools'),
  },
  jobs: {
    list: (accountId?: string) =>
      request<{ data: RefreshJob[] }>(`/api/v1/jobs${accountId ? `?account_id=${accountId}` : ''}`),
  },
}