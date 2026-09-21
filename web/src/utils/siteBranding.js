// 站点品牌文案：浏览器标题（<title>）与页脚内容。
//
// 背景：站点标题与页脚由后台「展示配置」维护，但**后台自己的页面**也要跟着变
// （否则访客看到自定义标题、运维登录后标签页还是旧的）。因此这里做成
// 与页面无关的工具：谁拿到配置谁调用，Admin 与公开页共用同一份逻辑。
//
// 安全说明（页脚 HTML）：
//   页脚是唯一一处把服务端下发的字符串交给 dangerouslySetInnerHTML 的地方，
//   而它的读者包括**未登录的匿名访客**（公开状态页）与**其他管理员的会话**（后台里
//   配置对所有人可见）。所以"能写配置的人"可信，"写进来的那段内容"不可信：
//   一次误粘贴、一个被拿下的账号，载荷就正好投递到权限更高的一批读者身上。
//   因此这里做**白名单重建**，不是"剥掉危险标签"（理由见 sanitizeFooterHTML）。

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

// keepTags 是唯一能出现在输出里的标签：只有展示型文本与结构，没有可交互元素。
// 不放 form/input/button/iframe/svg/math —— 前两者能提交与导航，
// 后两者自带一套与 HTML 不同的解析规则（正是 mXSS 的温床）。
const keepTags = new Set([
  'a', 'br', 'hr', 'p', 'div', 'span', 'b', 'strong', 'i', 'em', 'u', 's',
  'small', 'sub', 'sup', 'mark', 'code', 'pre', 'abbr', 'blockquote',
  'ul', 'ol', 'li', 'img',
])

// voidTags 没有闭合标签，输出时按空元素处理。
const voidTags = new Set(['br', 'hr', 'img'])

// dropContentTags 连**内容**一起丢：这些标签里的文本在原文里不是给人读的
// （样式表、脚本体、SVG 节点）。只拆标签留文本会留下一堆半截代码样的字符串，
// 而"留文本"对 noscript/template 这类还会在特定解析态下重新变成标记。
const dropContentTags = new Set([
  'script', 'style', 'iframe', 'frame', 'frameset', 'object', 'embed', 'noscript',
  'noembed', 'noframes', 'template', 'svg', 'math', 'xmp', 'plaintext', 'title',
  'textarea', 'select', 'option', 'head', 'html', 'body',
])

const globalAttrs = new Set(['title', 'alt'])
const tagAttrs = {
  a: new Set(['href', 'target']),
  img: new Set(['src']),
}

// 允许显式出现在链接里的协议。img 另有一条只认位图 data: 的分支。
const navSchemes = /^(?:https?:|mailto:|tel:|ftp:)/i
const dataImageUrl = /^data:image\/(?:png|jpe?g|gif|webp);base64,[a-z0-9+/=]*$/i
const anyScheme = /^[a-z][a-z0-9+.-]*:/i
// maxKeepDepth 挡的是"三万个嵌套 <p>"这类输入：白名单已经保证了不可执行，
// 这里只是不让一次渲染把浏览器卡住。超深处的标签按拆壳处理（文本照常保留）。
const maxKeepDepth = 48

const namedEntities = {
  amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ',
}

// decodeEntities 解出属性值里的 `&#106;` / `&#x6a;` / `&amp;`。
// 不解这一层，`href="&#106;avascript:alert(1)"` 就能穿着"看起来不像 javascript:"
// 的外衣过协议判定，而浏览器递给它自己的时候是会解的。
function decodeEntities(s) {
  if (!s.includes('&')) return s
  return s.replace(/&(#[xX]?[0-9a-fA-F]+|[a-zA-Z]+);?/g, (m, body) => {
    if (!body) return m
    if (body[0] === '#') {
      const hex = body[1] === 'x' || body[1] === 'X'
      const cp = Number.parseInt(hex ? body.slice(2) : body.slice(1), hex ? 16 : 10)
      if (!Number.isFinite(cp) || cp < 0 || cp > 0x10ffff) return ''
      return String.fromCodePoint(cp)
    }
    const key = body.toLowerCase()
    return Object.prototype.hasOwnProperty.call(namedEntities, key) ? namedEntities[key] : m
  })
}

// escText / escAttr 是输出的最后一道：文本里的尖括号一律成实体，
// 于是"任何看起来像标签的东西"都不可能是标签了。
// 先解一次实体再转义：否则运维写的 `&lt;` 会变成 `&amp;lt;`（显示成字面 &lt;），
// 而"解一层 → 转义"的往返是幂等的，浏览器只解一层，安全性质不变。
function escText(s) {
  return escapeOnly(decodeEntities(s))
}

function escapeOnly(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

// escAttr 收到的是 readTag 已经解过一次实体的值，这里只转义不再解码 ——
// 解两次会让 `&amp;lt;` 少一层，显示成 `<` 而不是运维写的 `&lt;`。
function escAttr(s) {
  return escapeOnly(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;')
}

// cleanURL 返回可用的 URL 值，或空串表示"这个属性不该出现"。
function cleanURL(raw, { image = false } = {}) {
  // 控制符与空白先去掉再判协议：`java&#9;script:`、` javascript:alert(1)` 这类
  // 写法靠"字符串开头不像协议"穿过字面匹配，浏览器却照样认。
  const v = raw.replace(/[\u0000-\u0020\u007f]/g, '')
  if (!v) return ''
  if (v[0] === '#') return v
  if (dataImageUrl.test(v)) return image ? v : ''
  if (image && !/^(?:https?:|\/\/)/i.test(v)) return ''
  if (v.startsWith('//')) return v // 协议相对：仍是 http 族，交给页面自己的协议
  if (navSchemes.test(v)) return v
  // 其余带显式协议的（javascript:/vbscript:/file:/blob:/view-source:…）不给；
  // 不带协议的一律当相对路径。
  return anyScheme.test(v) ? '' : v
}

// readTag 从 src[i]（必为 '<'）读出一个标签。返回 null 表示这不是标签起点
// （`<` 后面不是名字），调用方按文本处理 —— 否则 "ICP<b 备案号" 会被啃掉一段。
function readTag(src, i) {
  let j = i + 1
  let closing = false
  if (src[j] === '/') { closing = true; j += 1 }
  const nameStart = j
  while (j < src.length && /[a-zA-Z0-9]/.test(src[j])) j += 1
  const name = src.slice(nameStart, j).toLowerCase()
  if (!name) return null
  const attrs = []
  let selfClosing = false
  while (j < src.length) {
    while (j < src.length && /[\s/]/.test(src[j])) {
      // 名字与属性之间的 '/' 只有在紧跟（可含空白的）'>' 时才算自闭合，
      // 否则 `<a/x href=…>` 里的斜杠会被误读成自闭合并提前收尾。
      if (src[j] === '/' && /^\s*>/.test(src.slice(j + 1))) selfClosing = true
      j += 1
    }
    if (j >= src.length) return null // 未闭合：后面全是文本
    if (src[j] === '>') { j += 1; break }
    let k = j
    while (k < src.length && !/[\s=/>]/.test(src[k])) k += 1
    const attrName = src.slice(j, k).toLowerCase()
    if (!attrName) { j = k + 1; continue }
    let p = k
    while (p < src.length && /[\s]/.test(src[p])) p += 1
    let value = ''
    if (src[p] === '=') {
      p += 1
      while (p < src.length && /[\s]/.test(src[p])) p += 1
      const q = src[p]
      if (q === '"' || q === "'") {
        const end = src.indexOf(q, p + 1)
        value = src.slice(p + 1, end < 0 ? src.length : end)
        p = end < 0 ? src.length : end + 1
      } else {
        let e = p
        while (e < src.length && !/[\s>]/.test(src[e])) e += 1
        // 裸值里吃到结尾的 '/' 属于自闭合标记而不是值的一部分（`href=/x/>`）
        value = src.slice(p, e).replace(/\/+$/, '')
        p = e
      }
    } else {
      p = k
    }
    attrs.push({ name: attrName, value: decodeEntities(value) })
    j = p
  }
  return { name, closing, selfClosing, attrs, end: j }
}

// openTagHTML 按白名单重建起始标签；属性名不在表里就直接不存在，
// 值一律重新转义输出（原样回吐等于把解码后的内容再交给解析器）。
function openTagHTML(name, attrs) {
  const allowed = tagAttrs[name]
  const out = ['<', name]
  let targetBlank = false
  for (const { name: an, value } of attrs) {
    const isNav = an === 'href'
    const isSrc = an === 'src'
    if (!(globalAttrs.has(an) || (allowed && allowed.has(an)))) continue
    if (isNav || isSrc) {
      const v = cleanURL(value, { image: name === 'img' })
      if (!v) continue
      out.push(' ', an, '="', escAttr(v), '"')
      continue
    }
    if (an === 'target') {
      if (value === '_blank') { targetBlank = true; out.push(' target="_blank"') }
      continue
    }
    out.push(' ', an, '="', escAttr(value), '"')
  }
  // 新窗口打开必须由服务端一侧给不出来的方式约束：没有 rel 的 target=_blank
  // 让打开页拿到 window.opener。
  if (targetBlank) out.push(' rel="noopener noreferrer"')
  out.push('>')
  return out.join('')
}

// sanitizeFooterHTML 把任意输入重建为"只含展示型标签与属性"的 HTML 串。
//
// 为什么不是正则剥危险构造（旧实现）：黑名单永远列不全。`<svg/onload=…>` 里
// `on*` 前面没有空白、`<img/src=1/onerror=…>` 用斜杠当分隔符、`&#106;avascript:`
// 用实体绕过字面匹配，还有 mXSS（`<noscript><p title="</noscript><img src=x onerror=…>">`）
// 这种"同一个串在两次解析里含义不同"的类别。黑名单要对抗的是"HTML 语法有多少写法"，
// 白名单重建要对抗的只是"我列出的这几个标签浏览器会怎么解释" —— 后者能穷举。
//
// 未识别的标签**拆壳留文本**（`<marquee>x</marquee>` 输出 `x`）：整段丢弃会让
// 运维辛苦配的备案号凭空少半句，比留文本更容易被当成 bug 回退掉这层过滤。
export function sanitizeFooterHTML(html) {
  if (!html) return ''
  const src = String(html)
  let out = ''
  const stack = []
  const drop = [] // 连内容一起丢的标签名栈
  let i = 0
  while (i < src.length) {
    const lt = src.indexOf('<', i)
    if (lt < 0) {
      if (!drop.length) out += escText(src.slice(i))
      break
    }
    if (!drop.length) out += escText(src.slice(i, lt))
    i = lt
    if (src.startsWith('<!--', i)) {
      const end = src.indexOf('-->', i + 4)
      i = end < 0 ? src.length : end + 3
      continue
    }
    if (src.startsWith('<!', i) || src.startsWith('<?', i)) {
      const end = src.indexOf('>', i)
      i = end < 0 ? src.length : end + 1
      continue
    }
    const tag = readTag(src, i)
    if (!tag) { // 不是标签起点：'<' 当普通文本
      if (!drop.length) out += '&lt;'
      i += 1
      continue
    }
    i = tag.end
    if (drop.length) {
      // 丢弃区内只找它自己的闭合标签，其余一概不看。
      if (tag.closing && tag.name === drop[drop.length - 1]) drop.pop()
      continue
    }
    if (dropContentTags.has(tag.name)) {
      if (!tag.closing) drop.push(tag.name)
      continue
    }
    if (!keepTags.has(tag.name)) continue // 拆壳：标签标记消失，文本继续走循环
    if (tag.closing) {
      const at = stack.lastIndexOf(tag.name)
      if (at >= 0) {
        while (stack.length > at) out += '</' + stack.pop() + '>'
      }
      continue
    }
    if (voidTags.has(tag.name)) {
      out += openTagHTML(tag.name, tag.attrs)
      continue
    }
    if (stack.length >= maxKeepDepth) continue // 超深：同样拆壳
    out += openTagHTML(tag.name, tag.attrs)
    if (!tag.selfClosing) stack.push(tag.name)
    else out += '</' + tag.name + '>'
  }
  while (stack.length) out += '</' + stack.pop() + '>'
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
