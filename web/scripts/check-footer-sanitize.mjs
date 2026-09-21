// 页脚 HTML 白名单净化的回归检查（无需浏览器、无需测试框架）。
//
// 运行：cd web && node scripts/check-footer-sanitize.mjs
//
// 这里覆盖的是"曾经真的穿过过滤"的那一类，而不是通用 HTML 正确性：
//   · `<svg/onload=…>`、`<img/src=1/onerror=…>` —— 事件属性前面没有空白
//   · `&#106;avascript:`、`java<TAB>script:` —— 实体/控制符绕过字面协议匹配
//   · 属性值里塞 `</p><img src=x onerror=…>` —— 靠"引号内是字面串"侥幸的旧写法
//   · `<noscript>`/`<scr<script>ipt>` —— 同一个串在两次解析里含义不同（mXSS）
// 以及反向的一条：正常运维写的版权/备案号/链接**必须原样活下来**，
// 否则这层过滤会在下一个版本被人当 bug 删掉。

import { renderFooterHTML, sanitizeFooterHTML } from '../src/utils/siteBranding.js'

let pass = 0
const failures = []

function check(name, fn) {
  try {
    fn()
    pass++
    console.log('  ✓ ' + name)
  } catch (e) {
    failures.push(name + ' :: ' + e.message)
    console.log('  ✗ ' + name + '\n      ' + e.message)
  }
}

function ok(cond, what) {
  if (!cond) throw new Error(what)
}

// 危险面分成三类来判，因为"输出里出现 onerror 这串字符"是合法的
// ——`title="</p><img src=x onerror=…"` 里的它是**数据**（已转义，进不了属性解析）。
// 拿整串做子串匹配会把这种正常保留误判成失败，反而诱导人放宽过滤。
const badTags = ['script', 'style', 'iframe', 'svg', 'math', 'object', 'embed', 'form',
  'input', 'button', 'noscript', 'template', 'link', 'meta', 'base', 'frame', 'textarea']
const badAttrs = ['style', 'srcdoc', 'formaction', 'background', 'poster', 'dynsrc', 'lowsrc']
const badValueWords = ['javascript:', 'vbscript:', 'data:text/html', 'data:application', 'file:', 'view-source:']

// emittedTags 解析净化输出。它是我们自己拼的串，形状固定，用正则读不会误判。
function emittedTags(out) {
  const re = /<\/?([a-zA-Z][a-zA-Z0-9-]*)((?:\s+[a-zA-Z][a-zA-Z0-9-]*(?:="[^"]*")?)*)\s*\/?>/g
  const list = []
  let m
  while ((m = re.exec(out))) {
    const attrs = []
    const ar = /([a-zA-Z][a-zA-Z0-9-]*)(?:="([^"]*)")?/g
    let a
    while ((a = ar.exec(m[2] || ''))) attrs.push({ name: a[1].toLowerCase(), value: a[2] || '' })
    list.push({ raw: m[0], name: m[1].toLowerCase(), attrs })
  }
  return list
}

function assertInert(label, html) {
  const out = sanitizeFooterHTML(html)
  // 文本侧：除了我们自己的标签，不该有裸尖括号（有就说明内容被当结构吐回去了）。
  ok(!out.replace(/<\/?[a-zA-Z][^>]*>/g, '').includes('<'),
    `${label}：非标签位置出现了裸 <\n      实际 = ${out}`)
  // 每条 & 都必须是转义产物：裸 `&Tab;` 会被浏览器再解一次，
  // 于是"判过安全的值"在到达 URL 解析器时换了内容（实体二次解码）。
  ok(!/&(?!amp;|lt;|gt;|quot;|#39;)/.test(out), `${label}：输出里有未转义的 &\n      实际 = ${out}`)
  const tags = emittedTags(out)
  const bare = out.replace(/<\/?[a-zA-Z][a-zA-Z0-9-]*((?:\s+[a-zA-Z][a-zA-Z0-9-]*(?:="[^"]*")?)*)\s*\/?>/g, '')
  ok(bare === out.replace(/<\/?[a-zA-Z][^>]*>/g, ''), `${label}：有输出了形状之外的标签\n      实际 = ${out}`)
  for (const t of tags) {
    ok(!badTags.includes(t.name), `${label}：出现了危险标签 <${t.name}>`)
    for (const a of t.attrs) {
      ok(!a.name.startsWith('on'), `${label}：出现了事件属性 ${a.name}（${t.raw}）`)
      ok(!badAttrs.includes(a.name), `${label}：出现了危险属性 ${a.name}（${t.raw}）`)
      const v = a.value.toLowerCase().replace(/&amp;/g, '&')
      for (const w of badValueWords) {
        ok(!v.includes(w), `${label}：属性 ${a.name} 的值含 ${w}（${t.raw}）`)
      }
    }
  }
  return out
}

console.log('页脚 HTML 净化')

check('事件属性：需要空白的旧绕法全部失效', () => {
  assertInert('<svg/onload=alert(1)>x</svg>', '<svg/onload=alert(1)>x</svg>')
  assertInert('<img src=1/onerror=alert(1)>', '<img/src=1/onerror=alert(1)>')
  assertInert('<p onclick=x>y</p>', '<p onclick=x>y</p>')
  assertInert('<a href=/ HREF="javascript:alert(1)">z</a>', '<a href=/ HREF="javascript:alert(1)">z</a>')
  assertInert('<b onmouseover\n\t=alert(1)>t</b>', '<b onmouseover\n\t=alert(1)>t</b>')
})

check('协议判定看解码后的真值，不看字面', () => {
  assertInert('实体数字：&#106;avascript', '<a href="&#106;avascript:alert(1)">x</a>')
  assertInert('实体名字：&Tab;', '<a href="jav&Tab;ascript:alert(1)">x</a>')
  assertInert('制表符夹在协议里', '<a href="java\tscript:alert(1)">x</a>')
  assertInert('前导空白与大小写', '<a href="  JaVaScRiPt:alert(1)">x</a>')
  assertInert('NUL 与换行', '<a href="java\x00script\x0a:alert(1)">x</a>')
  assertInert('data: 文档', '<a href="data:text/html;base64,PHNjcmlwdD4=">x</a>')
  assertInert('view-source 等其它协议', '<a href="view-source:http://x">x</a>')
  assertInert('svg 位图外的 data 图', '<img src="data:image/svg+xml;base64,PHN2Zz4=">')
})

check('属性值是数据，不是重新解析的标记', () => {
  const out = assertInert('属性里闭合标签', '<p title="</p><img src=x onerror=alert(1)>">x</p>')
  ok(out.includes('&lt;/p&gt;') || !out.includes('</p><img'), '属性内容没有被当成标签吐出：' + out)
  assertInert('noscript 里的引号突围', '<noscript><p title="</noscript><img src=x onerror=alert(1)>">')
})

check('拆分与未闭合的标签不会拼出新标签', () => {
  assertInert('<scr<script>ipt> 嵌套', '<scr<script>ipt>alert(1)</scr</script>ipt>')
  assertInert('未闭合的 <script', '<script src=//evil>')
  assertInert('注释里的脚本', '<!--<a href="javascript:alert(1)">--><--!>')
  assertInert('doctype 与处理指令', '<!DOCTYPE html><?xml:namespace prefix=o><o:script>alert(1)</o:script>')
})

check('危险容器的内容一起丢掉', () => {
  const out = sanitizeFooterHTML('<style>p{}</style><script>var a=1</script>尾巴')
  ok(!out.includes('var a=1'), '脚本体不应作为文本留下：' + out)
  ok(!out.includes('p{}'), '样式体不应作为文本留下：' + out)
  ok(out.includes('尾巴'), '合法文本不能连带丢失：' + out)
})

check('正常页脚原样可用', () => {
  const src = '© {year} <b>MOONCN</b> · <a href="https://beian.miit.gov.cn/" target="_blank">京ICP备00000000号</a>' +
    ' <span class="dim">·</span> <a href="/about">关于</a><br>联系电话 <a href="tel:+8610">+86 10</a>'
  const out = renderFooterHTML(src)
  const year = String(new Date().getFullYear())
  ok(out.includes('© ' + year), '{year} 未替换：' + out)
  ok(out.includes('href="https://beian.miit.gov.cn/"'), '外部链接被吃掉：' + out)
  ok(out.includes('rel="noopener noreferrer"'), 'target=_blank 没补 rel：' + out)
  ok(out.includes('href="/about"'), '站内相对链接被吃掉：' + out)
  ok(out.includes('href="tel:+8610"'), 'tel 链接被吃掉：' + out)
  ok(out.includes('<b>MOONCN</b>'), '粗体丢失：' + out)
  ok(out.includes('<br>'), '换行丢失：' + out)
  ok(out.includes('京ICP备00000000号'), '备案号文本丢失：' + out)
  ok(!out.includes('class='), 'class 不在白名单内（样式由页面自己给）：' + out)
})

check('结构自愈：未闭合自动补、多余闭合忽略', () => {
  ok(sanitizeFooterHTML('<p>a') === '<p>a</p>', '未闭合：' + sanitizeFooterHTML('<p>a'))
  ok(sanitizeFooterHTML('a</p></div>') === 'a', '孤立闭合标签：' + sanitizeFooterHTML('a</p></div>'))
  ok(sanitizeFooterHTML('<p><b>x</p>') === '<p><b>x</b></p>', '交叉闭合：' + sanitizeFooterHTML('<p><b>x</p>'))
  ok(sanitizeFooterHTML('<br/>') === '<br>', '自闭合空元素：' + sanitizeFooterHTML('<br/>'))
  // `<em/>` 在浏览器里其实是"开启 em"（斜杠被忽略），净化后收成对更安全，
  // 也不会让后半段页脚莫名变斜体。
  ok(sanitizeFooterHTML('<em/>x') === '<em></em>x', '自闭合普通标签：' + sanitizeFooterHTML('<em/>x'))
})

check('普通文本里的尖括号不吞内容', () => {
  ok(sanitizeFooterHTML('a < b') === 'a &lt; b', '小于号：' + sanitizeFooterHTML('a < b'))
  ok(sanitizeFooterHTML('Q&A 备案') === 'Q&amp;A 备案', '裸 &：' + sanitizeFooterHTML('Q&amp;A 备案'))
  ok(sanitizeFooterHTML('') === '', '空输入')
  ok(sanitizeFooterHTML(null) === '', 'null 输入')
  ok(renderFooterHTML('   ') === '', '只有空白应返回空串（页脚整块不渲染）')
})

check('深嵌套不放大成 DoS，也不产生未闭合输出', () => {
  const deep = sanitizeFooterHTML('<p>'.repeat(5000) + 'x' + '</p>'.repeat(5000))
  ok(deep.includes('x'), '深层文本仍需保留')
  ok((deep.match(/<p>/g) || []).length <= 60, '保留深度应被限制：' + (deep.match(/<p>/g) || []).length)
  ok(deep.split('<p>').length === deep.split('</p>').length, '<p> 与 </p> 必须配平')
})

if (failures.length) {
  console.log('\n' + failures.length + ' 项失败')
  process.exit(1)
}
console.log('\n全部通过（' + pass + ' 项）')
