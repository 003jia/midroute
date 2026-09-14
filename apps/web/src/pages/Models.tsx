import { useCallback, useEffect, useMemo, useState } from 'react'
import { api, type Account, type Model, type Provider } from '../lib/api'

export default function ModelsPage() {
  const [providers, setProviders] = useState<Provider[]>([])
  const [accounts, setAccounts] = useState<Account[]>([])
  const [models, setModels] = useState<Model[]>([])
  const [providerFilter, setProviderFilter] = useState('all')
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setError(null)
    try {
      const [p, a, m] = await Promise.all([api.providers.list(), api.accounts.list(), api.models.list()])
      setProviders(p.data)
      setAccounts(a.data)
      setModels(m.data)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const [p, a, m] = await Promise.all([api.providers.list(), api.accounts.list(), api.models.list()])
        if (cancelled) return
        setProviders(p.data)
        setAccounts(a.data)
        setModels(m.data)
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const filtered = useMemo(
    () => models.filter((m) => providerFilter === 'all' || m.provider_id === providerFilter),
    [models, providerFilter],
  )

  return (
    <div className="page">
      <header className="page-head">
        <h2>模型</h2>
      </header>
      {error && <p className="error">{error}</p>}
      <div className="toolbar">
        <select value={providerFilter} onChange={(e) => setProviderFilter(e.target.value)}>
          <option value="all">全部平台</option>
          {providers.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
        <button className="btn" onClick={() => void load()}>
          刷新
        </button>
      </div>
      {filtered.length === 0 && !error && <p className="empty">暂无模型。先在“账户”页对账户执行“发现模型”。</p>}
      <table className="table">
        <thead>
          <tr>
            <th>上游模型 ID</th>
            <th>平台</th>
            <th>上下文上限</th>
            <th>别名</th>
          </tr>
        </thead>
        <tbody>
          {filtered.map((m) => {
            const prov = providers.find((p) => p.id === m.provider_id)
            const holder = accounts.find((a) => a.provider_id === m.provider_id)
            return (
              <tr key={m.id}>
                <td className="mono">{m.upstream_id}</td>
                <td>{prov?.name ?? m.provider_id}</td>
                <td>{m.context_limit > 0 ? m.context_limit.toLocaleString() : '—'}</td>
                <td>{m.canonical_alias || '—'}</td>
                <td className="muted">{holder ? `由 ${holder.name} 发现` : ''}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}