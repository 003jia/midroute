import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'

type Health = { ok: boolean; service: string; schema_version: number } | null

export default function App() {
  const [health, setHealth] = useState<Health>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/healthz')
      .then((r) => r.json())
      .then(setHealth)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
  }, [])

  return (
    <div className="app">
      <header>
        <h1>Midroute · 中间路由平台</h1>
        <p className="subtitle">本地优先的 AI 凭据、套餐、用量与路由管理</p>
      </header>
      <main>
        <nav>
          <Link to="/">总览</Link>
          <Link to="/accounts">账户</Link>
          <Link to="/models">模型</Link>
          <Link to="/usage">用量</Link>
        </nav>
        <section className="status-card">
          {error && <p className="error">无法连接后端：{error}</p>}
          {health ? (
            <ul>
              <li>服务：{health.service}</li>
              <li>状态：{health.ok ? '正常' : '异常'}</li>
              <li>数据库 schema 版本：{health.schema_version}</li>
            </ul>
          ) : (
            !error && <p>正在连接后端…</p>
          )}
        </section>
      </main>
    </div>
  )
}