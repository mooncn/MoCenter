import React from 'react'
import { useSearchParams } from 'react-router-dom'
import Deploy from './Deploy.jsx'
import AgentEnroll from './AgentEnroll.jsx'

// Agent 页面：把「部署 Agent」与「Agent 接入」收进同一入口的两个页签。
// 用 URL 参数记录页签，旧地址 /deploy、/enroll 会重定向到对应页签。
const TABS = [
  { key: 'ssh', label: 'SSH 部署（Linux）' },
  { key: 'universal', label: '通用部署（Agent 接入）' },
]

export default function Agent() {
  const [params, setParams] = useSearchParams()
  const tab = params.get('tab') === 'universal' ? 'universal' : 'ssh'

  return (
    <div>
      <div className="page-head">
        <div>
          <h2>Agent</h2>
          <div className="sub">Linux 主机走 SSH 一键部署；其它系统或手工安装按通用接入流程操作。</div>
        </div>
      </div>

      <div className="tabs" role="tablist" aria-label="Agent 部署方式">
        {TABS.map((t) => (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={tab === t.key}
            className={`tab-btn ${tab === t.key ? 'active' : ''}`}
            onClick={() => setParams({ tab: t.key })}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'ssh' ? <Deploy /> : <AgentEnroll />}
    </div>
  )
}
