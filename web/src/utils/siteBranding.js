// 站点品牌文案：浏览器标题（<title>）与页脚内容。
//
// 背景：站点标题与页脚由后台「展示配置」维护，但**后台自己的页面**也要跟着变
// （否则访客看到自定义标题、运维登录后标签页还是旧的）。因此这里做成
// 与页面无关的工具：谁拿到配置谁调用，Admin 与公开页共用同一份逻辑。
//
// 安全说明（页脚 HTML）：
//   页脚内容由**已登录运维**在后台配置，并通过需要鉴权的 /api/site/config 保存。
//   它与"未登录访客可控的输入"不同，属于受信内容，因此允许 HTML（这是需求本身：
//   自定义版权 / 备案号 / 链接），渲染时用 dangerouslySetInnerHTML 原样输出。
//   渲染前会做一层**脚本剥离**（见 sanitizeFooterHTML）：即便运维误粘贴了
//   带 <script> 的片段，也不会在公开页上执行。

// 年份占位符：默认页脚为 `{year}©MOONCN`，渲染时按当前年份替换。
export const FOOTER_YEAR_TOKEN = '{year}'

// DEFAULT_SITE_TITLE 与后端 siteconfig.DefaultSiteTitle 保持一致。
export const DEFAULT_SITE_TITLE = 'MoCenter · 集中式主机远控管理系统'

// renderFooterHTML 把页脚配置渲染为最终 HTML 字符串。
//   - 空值 -> 空串（调用方可据此决定是否渲染页脚区块）；
//   - `{year}` -> 当前年份。
export function renderFooterHTML(raw) {
  const text = typeof raw === 'string' ? raw : ''
  if (!text.trim()) return ''
  const year = String(new Date().getFullYear())
  return sanitizeFooterHTML(text.split(FOOTER_YEAR_TOKEN).join(year))
}

// sanitizeFooterHTML 剥离可执行内容，只保留展示型 HTML。
//
// 做法刻意"保守而简单"：移除 script/style/iframe/object/embed 等标签、
// 去掉所有 on* 事件属性与 javascript: 协议。不引入 DOMPurify 这类依赖 ——
// 输入来源是受信运维，这里的目标是**防手滑**而不是对抗主动攻击者。
export function sanitizeFooterHTML(html) {
  if (!html) return ''
  let out = String(html)
  // 成对/自闭合的危险标签（含其内容）
  out = out.replace(/<\s*(script|style|iframe|object|embed|link|meta|base|form)[\s\S]*?<\s*\/\s*\1\s*>/gi, '')
  // 未闭合的危险标签（如单独一个 <script src=...>）
  out = out.replace(/<\s*\/?\s*(script|style|iframe|object|embed|link|meta|base|form)\b[^>]*>/gi, '')
  // 内联事件属性（onclick / onerror / onload …）
  out = out.replace(/\son[a-z]+\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)/gi, '')
  // javascript: / vbscript: / data:text/html 协议
  out = out.replace(/(href|src|xlink:href)\s*=\s*("|')\s*(javascript|vbscript|data)\s*:[^"']*\2/gi, '$1=$2#$2')
  return out
}

// applySiteTitle 设置浏览器标签页标题（空值回退默认标题）。
export function applySiteTitle(title) {
  const t = typeof title === 'string' && title.trim() ? title.trim() : DEFAULT_SITE_TITLE
  if (typeof document !== 'undefined' && document.title !== t) {
    document.title = t
  }
  return t
}
