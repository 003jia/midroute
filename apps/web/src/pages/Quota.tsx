import { useEffect, useState } from 'react'
import { api, type Account, type QuotaPool, type QuotaSnapshot, type RefreshJob } from '../lib/api'

const windowLabels: Record<string, string> = {
  primary: '主窗口（5 小时）',
  secondary: '次窗口（每周）',
  additional: '附加窗口',
  'code-review': 'Code Review',
  day: '自然日',
  month: '自然月',
  billing: '账单周期',
  unknown: '未知窗口',
}

const sourceLabels: Record<string, string> = {
  official: '官方',
  reported: '上游报告',
  observed: '本地观测',
  estimated: '估算',
  manual: '手工',
}

const freshnessLabels: Record<string, string> = {
  fresh: '新鲜',
  stale: '过期',
  unknown: '未知',
}

export default function QuotaPage() {
  
  const [accounts, setAccounts] = useState<Account[]>([])
  const [pools, setPools] = useState<QuotaPool[]>([])
  const [snapshots, setSnapshots] = useState<QuotaSnapshot[]>([])
  const [jobs, setJobs] = useState<RefreshJob[]>([])
  const [selected, setSelected] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [manual, setManual] = useState<{ accountId: string; window: string; used: string } | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const [a, poolsData] = await Promise.all([api.accounts.list(), api.quotaPools.list()])
        if (cancelled) return
        
        setAccounts(a.data)
        setPools(poolsData.data)
        if (a.data.length > 0) setSelected(a.data[0].id)
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    if (!selected) return
    let cancelled = false
    ;(async () => {
      try {
        const [s, j] = await Promise.all([api.accounts.quota(selected), api.jobs.list(selected)])
        if (cancelled) return
        setSnapshots(s.data)
        setJobs(j.data)
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [selected])

  const triggerRefresh = async (cap: string) => {
    if (!selected) return
    setError(null)
    try {
      await api.accounts.refresh(selected, cap)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const submitManual = async () => {
    if (!manual) return
    setError(null)
    try {
      const used = manual.used === '' ? null : Number(manual.used)
      await api.accounts.manualQuota(manual.accountId, {
        window_type: manual.window,
        used,
        unit: 'percent',
      })
      setManual(null)
      setSelected(manual.accountId)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const pool = pools.find((p) => p.member_ids.includes(selected))

  return (
    <div className="page">
      <header className="page-head">
        <h2>套餐与额度</h2>
        <div className="toolbar">
          <select value={selected} onChange={(e) => setSelected(e.target.value)}>
            {accounts.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
              </option>
            ))}
          </select>
          <button className="btn" onClick={() => void triggerRefresh('capabilities')}>
            刷新能力
          </button>
          <button className="btn" onClick={() => void triggerRefresh('models')}>
            刷新模型
          </button>
        </div>
      </header>
      {error && <p className="error">{error}</p>}
      {selected && accounts.find((a) => a.id === selected) && (
        <p className="muted">
          账户：{accounts.find((a) => a.id === selected)?.name}
          {pool && ` · 共享池 ${pool.external_org || pool.id}（成员 ${pool.member_ids.length}）`}
          {' · '}
          默认每项额度单独展示，官方/本地观测/手工分列，未知不写 0。
        </p>
      )}

      {snapshots.length === 0 ? (
        <p className="empty">暂无额度快照。额度来自响应头被动观测或手工录入；无自动读取能力时不展示伪造数字。</p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>窗口</th>
              <th>已用</th>
              <th>剩余</th>
              <th>重置时间</th>
              <th>来源</th>
              <th>时效</th>
              <th>采集时间</th>
            </tr>
          </thead>
          <tbody>
            {snapshots.map((s) => (
              <tr key={s.id}>
                <td>{windowLabels[s.window_type] ?? s.window_type}</td>
                <td>{s.used === null || s.used === undefined ? '未知' : `${s.used}${s.unit === 'percent' ? '%' : ''}`}</td>
                <td>{s.remaining === null || s.remaining === undefined ? '未知' : s.remaining}</td>
                <td>{s.reset_at ? new Date(s.reset_at).toLocaleString() : '—'}</td>
                <td>
                  {sourceLabels[s.source] ?? s.source}
                  {s.operator && `（${s.operator}）`}
                </td>
                <td>{freshnessLabels[s.freshness] ?? s.freshness}</td>
                <td>{new Date(s.taken_at).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div className="toolbar">
        {selected && (
          <button
            className="btn"
            onClick={() => setManual({ accountId: selected, window: 'primary', used: '' })}
          >
            手工补充额度
          </button>
        )}
      </div>

      {manual && (
        <div className="modal">
          <div className="modal-card">
            <header>
              <h3>手工补充额度</h3>
              <button className="btn" onClick={() => setManual(null)}>
                关闭
              </button>
            </header>
            <label>
              窗口
              <select value={manual.window} onChange={(e) => setManual({ ...manual, window: e.target.value })}>
                <option value="primary">主窗口（5 小时）</option>
                <option value="secondary">次窗口（每周）</option>
                <option value="code-review">Code Review</option>
                <option value="day">自然日</option>
                <option value="month">自然月</option>
              </select>
            </label>
            <label>
              已用百分比（留空 = 未知）
              <input
                type="number"
                min={0}
                max={100}
                value={manual.used}
                onChange={(e) => setManual({ ...manual, used: e.target.value })}
              />
            </label>
            <p className="hint">手工数据以 source=manual 保存，不会覆盖官方/观测快照。</p>
            <footer>
              <button className="btn" onClick={() => setManual(null)}>
                取消
              </button>
              <button className="btn primary" onClick={() => void submitManual()}>
                保存
              </button>
            </footer>
          </div>
        </div>
      )}

      <h3 style={{ marginTop: '1.5rem' }}>刷新任务</h3>
      <table className="table">
        <thead>
          <tr>
            <th>能力</th>
            <th>状态</th>
            <th>重试</th>
            <th>下次执行</th>
            <th>最后成功</th>
          </tr>
        </thead>
        <tbody>
          {jobs.map((j) => (
            <tr key={j.id}>
              <td>{j.capability}</td>
              <td>
                <span className={`badge badge-${j.state}`}>{j.state}</span>
              </td>
              <td>{j.retry_count}</td>
              <td>{j.next_run_at ? new Date(j.next_run_at).toLocaleString() : '—'}</td>
              <td>{j.last_success_at ? new Date(j.last_success_at).toLocaleString() : '—'}</td>
            </tr>
          ))}
          {jobs.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                暂无刷新任务
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}