import { useCallback, useEffect, useState } from 'react'
import { api, type Account, type AccountCapability, type Provider } from '../lib/api'

type StatusFilter = 'all' | 'active' | 'disabled' | 'error'

const capabilityLabels: Record<string, string> = {
  verifyCredential: '凭据验证',
  discoverModels: '模型发现',
  forward: '请求转发',
  oauth: 'OAuth 登录',
  subscription: '套餐读取',
  quota: '额度读取',
  refresh: '凭据刷新',
  probeHealth: '健康探测',
}

const capabilityStatusLabels: Record<string, string> = {
  supported: '支持',
  unsupported: '不支持',
  permission_required: '需更高权限',
  reauth_required: '需重新授权',
  error: '出错',
  unknown: '未知',
}

export default function AccountsPage() {
  const [providers, setProviders] = useState<Provider[]>([])
  const [accounts, setAccounts] = useState<Account[]>([])
  const [filter, setFilter] = useState<StatusFilter>('all')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<{ account: Account; caps: AccountCapability[] } | null>(null)
  const [editing, setEditing] = useState<Account | null>(null)
  const [showWizard, setShowWizard] = useState(false)

  const load = useCallback(async () => {
    setError(null)
    try {
      const [p, a] = await Promise.all([api.providers.list(), api.accounts.list()])
      setProviders(p.data)
      setAccounts(a.data)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const [p, a] = await Promise.all([api.providers.list(), api.accounts.list()])
        if (cancelled) return
        setProviders(p.data)
        setAccounts(a.data)
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const run = useCallback(async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }, [load])

  const visible = accounts.filter((a) => filter === 'all' || a.status === filter)

  return (
    <div className="page">
      <header className="page-head">
        <h2>账户</h2>
        <button className="btn primary" onClick={() => setShowWizard(true)} disabled={busy}>
          添加账户
        </button>
      </header>
      {error && <p className="error">{error}</p>}

      <div className="toolbar">
        {(['all', 'active', 'disabled', 'error'] as StatusFilter[]).map((s) => (
          <button
            key={s}
            className={`btn chip ${filter === s ? 'active' : ''}`}
            onClick={() => setFilter(s)}
          >
            {s === 'all' ? '全部' : s === 'active' ? '启用' : s === 'disabled' ? '禁用' : '异常'}
          </button>
        ))}
      </div>

      {visible.length === 0 && !busy && <p className="empty">暂无账户，点击“添加账户”开始。</p>}

      <table className="table">
        <thead>
          <tr>
            <th>名称</th>
            <th>平台</th>
            <th>模式</th>
            <th>状态</th>
            <th>认证</th>
            <th>指纹</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          {visible.map((a) => (
            <tr key={a.id}>
              <td>{a.name}</td>
              <td>{providers.find((p) => p.id === a.provider_id)?.kind ?? a.provider_id}</td>
              <td>{a.mode === 'monitor_only' ? '仅监测' : '转发+监测'}</td>
              <td>
                <span className={`badge badge-${a.status}`}>
                  {a.status === 'active' ? '启用' : a.status === 'disabled' ? '禁用' : '异常'}
                </span>
                {a.auth_state === 'reauth_required' && <span className="badge badge-reauth">需重新授权</span>}
              </td>
              <td>{a.auth_type}</td>
              <td className="mono">{a.secret_fingerprint}</td>
              <td className="row-actions">
                <button className="btn" onClick={() => void run(() => api.accounts.verify(a.id))}>
                  验证
                </button>
                <button className="btn" onClick={() => void run(() => api.accounts.discover(a.id))}>
                  发现模型
                </button>
                <button
                  className="btn"
                  onClick={() => void run(() => api.accounts.capabilities(a.id).then((r) => setSelected({ account: a, caps: r.data })))}
                >
                  能力
                </button>
                <button className="btn" onClick={() => setEditing(a)}>
                  编辑
                </button>
                {a.status === 'disabled' ? (
                  <button className="btn" onClick={() => void run(() => api.accounts.patch(a.id, { status: 'active' }))}>
                    重新启用
                  </button>
                ) : (
                  <button className="btn" onClick={() => void run(() => api.accounts.patch(a.id, { status: 'disabled' }))}>
                    禁用
                  </button>
                )}
                <button className="btn danger" onClick={() => void run(() => api.accounts.del(a.id))}>
                  删除
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {showWizard && <AccountWizard providers={providers} onClose={() => setShowWizard(false)} onDone={() => void load()} />}
      {editing && <AccountEditor account={editing} onClose={() => setEditing(null)} onDone={() => void load()} />}
      {selected && (
        <div className="modal">
          <div className="modal-card">
            <header>
              <h3>能力矩阵 · {selected.account.name}</h3>
              <button className="btn" onClick={() => setSelected(null)}>
                关闭
              </button>
            </header>
            <table className="table">
              <thead>
                <tr>
                  <th>能力</th>
                  <th>状态</th>
                  <th>原因</th>
                  <th>连接器</th>
                  <th>检查时间</th>
                </tr>
              </thead>
              <tbody>
                {selected.caps.map((c) => (
                  <tr key={c.capability}>
                    <td>{capabilityLabels[c.capability] ?? c.capability}</td>
                    <td>
                      <span className={`badge badge-cap-${c.status}`}>
                        {capabilityStatusLabels[c.status] ?? c.status}
                      </span>
                    </td>
                    <td>{c.reason ?? ''}</td>
                    <td className="mono">{c.connector_version}</td>
                    <td>{new Date(c.checked_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  )
}

function AccountWizard({
  providers,
  onClose,
  onDone,
}: {
  providers: Provider[]
  onClose: () => void
  onDone: () => void
}) {
  const [providerId, setProviderId] = useState(providers[0]?.id ?? '')
  const [name, setName] = useState('')
  const [mode, setMode] = useState<'relay_and_monitor' | 'monitor_only'>('relay_and_monitor')
  const [apiKey, setApiKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.accounts.create({ provider_id: providerId, name: name || undefined, mode, api_key: apiKey })
      setApiKey('')
      onDone()
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="modal">
      <form className="modal-card" onSubmit={submit}>
        <header>
          <h3>添加账户</h3>
          <button type="button" className="btn" onClick={onClose}>
            关闭
          </button>
        </header>
        {error && <p className="error">{error}</p>}
        <label>
          平台
          <select value={providerId} onChange={(e) => setProviderId(e.target.value)} required>
            {providers.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}（{p.kind}）
              </option>
            ))}
          </select>
        </label>
        <label>
          名称
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="可选" />
        </label>
        <label>
          账户模式
          <select value={mode} onChange={(e) => setMode(e.target.value as typeof mode)}>
            <option value="relay_and_monitor">转发并监测（默认）</option>
            <option value="monitor_only">仅监测（不参与转发）</option>
          </select>
        </label>
        <label>
          API Key
          <input
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder="仅本次请求传入，不落库"
            required
            autoComplete="off"
          />
        </label>
        <p className="hint">密钥经系统密钥库保存，仅存指纹与引用；创建后页面不再显示完整密钥。</p>
        <footer>
          <button type="button" className="btn" onClick={onClose}>
            取消
          </button>
          <button type="submit" className="btn primary" disabled={busy || !providerId || !apiKey}>
            {busy ? '提交中…' : '添加'}
          </button>
        </footer>
      </form>
    </div>
  )
}

function AccountEditor({
  account,
  onClose,
  onDone,
}: {
  account: Account
  onClose: () => void
  onDone: () => void
}) {
  const [name, setName] = useState(account.name)
  const [newKey, setNewKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      if (name !== account.name) await api.accounts.patch(account.id, { name })
      if (newKey) await api.accounts.rotateCredential(account.id, newKey)
      onDone()
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="modal">
      <form className="modal-card" onSubmit={submit}>
        <header>
          <h3>编辑账户 · {account.name}</h3>
          <button type="button" className="btn" onClick={onClose}>
            关闭
          </button>
        </header>
        {error && <p className="error">{error}</p>}
        <label>
          名称
          <input value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label>
          更换 API Key（可选）
          <input
            type="password"
            value={newKey}
            onChange={(e) => setNewKey(e.target.value)}
            placeholder="新密钥验证成功后原子切换；失败保留原凭据"
            autoComplete="off"
          />
        </label>
        <footer>
          <button type="button" className="btn" onClick={onClose}>
            取消
          </button>
          <button type="submit" className="btn primary" disabled={busy}>
            {busy ? '保存中…' : '保存'}
          </button>
        </footer>
      </form>
    </div>
  )
}