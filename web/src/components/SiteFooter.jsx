import React, { useEffect } from 'react'
import { applySiteTitle, renderFooterHTML } from '../utils/siteBranding.js'

// 站点页脚：内容来自后台「展示配置 → 站点标题与页脚」，支持 HTML。
//
// 这是唯一一处渲染页脚自定义 HTML 的地方：把 dangerouslySetInnerHTML 收在
// 一个小组件里，便于审阅（输入来源见 utils/siteBranding.js 的说明）。
//
// html 为空时不渲染整块页脚 —— 与其显示一行空白，不如干脆不要。
export default function SiteFooter({ html }) {
  const rendered = renderFooterHTML(html)
  if (!rendered) return null
  return (
    <footer
      className="public-foot site-foot"
      dangerouslySetInnerHTML={{ __html: rendered }}
    />
  )
}

// useSiteTitle 把浏览器标签页标题同步为站点配置值。
// 放在 hook 里是因为"标题"不是 DOM 结构：页面切换/配置刷新都应重新应用，
// 且组件卸载后无需回滚（下一个页面会再设一次）。
export function useSiteTitle(title) {
  useEffect(() => {
    if (title === undefined || title === null || title === '') return
    applySiteTitle(title)
  }, [title])
}
