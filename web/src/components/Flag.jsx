// 国旗组件：把节点地区里的 ISO 国家代码渲染成一面旗。
//
// 背景见 src/utils/flag.js —— 直接用 emoji 会依赖访客的系统字体，
// Windows 10 / 无字体容器下会变成方框，导致"地区有值、国旗不显示"。
// 本组件优先用内置字形（与宿主字体无关），拿不到时退回 emoji 兜底。
//
// 尺寸跟随字号（1.15em），因此在卡片 / 列表 / 标题里都能与周围文本对齐，
// 不需要每处单独调样式。

import { useEffect, useState } from 'react'
import { flagDataURL, flagEmoji, hasFlag, normalize } from '../utils/flag'

export default function Flag({ code, className = '', title, style }) {
  const key = normalize(code)
  const [url, setUrl] = useState(null)

  useEffect(() => {
    // 未内置字形的代码不必发起解码，直接走 emoji 兜底。
    if (!key || !hasFlag(key)) {
      setUrl(null)
      return
    }
    let alive = true
    flagDataURL(key)
      .then((u) => { if (alive) setUrl(u) })
      .catch(() => { if (alive) setUrl(null) })
    return () => { alive = false }
  }, [key])

  if (!key) return null

  const props = {
    className: className ? `${className} flag` : 'flag',
    title: title ?? key,
    style,
  }

  // 已解码：用内置位图。alt 带国家代码，便于无障碍读出与悬停确认。
  if (url) {
    return <img {...props} className={`${props.className} flag-img`} src={url} alt={key} draggable={false} />
  }

  // 解码中或未收录：先用 emoji 占位。
  // 支持彩色 emoji 的环境里这就是最终形态；不支持的环境会在解码完成后换成位图。
  return (
    <span {...props} className={`${props.className} flag-emoji`}>
      {flagEmoji(key) || key}
    </span>
  )
}
