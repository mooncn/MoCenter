// 国旗渲染：把 ISO 3166-1 alpha-2 代码变成可显示的旗帜。
//
// ## 为什么不直接用国旗 emoji（PR #11 的核心问题）
//
// 最省事的做法是用区域指示符拼出 emoji：
//
//     String.fromCodePoint(0x1F1E6 + code[0] - 65, 0x1F1E6 + code[1] - 65)
//
// 但它能不能显示**取决于访客的机器**，而不是取决于我们的代码：
//
//   1. Windows 10 及更早：Segoe UI Emoji 没有区域指示符连字，
//      浏览器会把两个字母各画成一个方框（"🇨🇳" 变成两个豆腐块）；
//   2. 精简版 Linux 容器 / 无桌面的服务器：完全没有 emoji 字体；
//   3. 企业策略禁用彩色字体回退的浏览器。
//
// 于是症状是"接口里 country_code 明明有值、页面上国旗却是空白/方框"，
// 并且**只在部分访客的机器上复现** —— 运维在自己电脑上测是正常的。
// 这正是"改了几次还是没实现"的根因：问题在渲染层，不在数据链路。
//
// ## 方案
//
// 把字形固化进前端资源（见 flagSprite.js，由 scripts/gen-flag-sprite.mjs 生成）：
// 渲染与宿主字体无关、不请求外部 CDN、也不受离线/内网环境影响。
//
// ## 兜底顺序
//
//   1. 内置位图字形（覆盖 249 个 ISO 代码 + 常用特殊代码）；
//   2. 未收录的代码退回区域指示符 emoji —— 在不支持的环境里至少
//      还能看到国家代码文本，而不是一片空白。

import {
  FLAG_SIZE,
  FLAG_COLORS,
  FLAG_PALETTE,
  FLAG_CODES,
  FLAG_DATA,
} from './flagSprite.js'

// 透明标记：位图里等于调色板长度的下标。
const TRANSPARENT = FLAG_COLORS

// 调色板解码成 [r,g,b] 数组（只在首次用到时做一次）。
let paletteRGB = null
function palette() {
  if (paletteRGB) return paletteRGB
  const out = new Array(FLAG_COLORS)
  for (let i = 0; i < FLAG_COLORS; i++) {
    const o = i * 6
    out[i] = [
      parseInt(FLAG_PALETTE.slice(o, o + 2), 16),
      parseInt(FLAG_PALETTE.slice(o + 2, o + 4), 16),
      parseInt(FLAG_PALETTE.slice(o + 4, o + 6), 16),
    ]
  }
  paletteRGB = out
  return out
}

// 位图解码：base64 -> 字节流。压缩用的是 zlib deflate，
// 现代浏览器可用 DecompressionStream('deflate') 原生解压（异步）。
// 为保持调用方同步，这里改为在**模块加载时不需要解压** ——
// 每个国旗只取自己那段像素，而 deflate 无法随机访问，所以只能整体解压一次。
//
// 折中：解压推迟到第一次真正渲染国旗时，且只做一次；结果常驻内存。
let bitmap = null

async function ensureBitmap() {
  if (bitmap) return bitmap
  const binary = atob(FLAG_DATA)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)

  const ds = new DecompressionStream('deflate')
  const stream = new Blob([bytes]).stream().pipeThrough(ds)
  const buf = new Uint8Array(await new Response(stream).arrayBuffer())
  if (buf.length !== FLAG_CODES.length * FLAG_SIZE * FLAG_SIZE) {
    throw new Error(`国旗字形数据长度不符（${buf.length}）`)
  }
  bitmap = buf
  return bitmap
}

// 代码 -> 起始下标
const OFFSET = new Map(FLAG_CODES.map((c, i) => [c, i * FLAG_SIZE * FLAG_SIZE]))

// 解码后的 data URL 缓存：同一面国旗会同时出现在卡片、列表、详情多处，
// 每处重新栅格化 + 编码一遍 base64 是纯浪费。
const urlCache = new Map()

/**
 * 异步取得国旗的 data URL；代码未收录或解压失败时返回 null。
 *
 * 为什么是异步：位图要整体解压（deflate 不支持随机访问），
 * 这一步放在首次调用时做一次，之后走缓存。
 */
export async function flagDataURL(code) {
  const key = normalize(code)
  if (!key) return null
  const hit = urlCache.get(key)
  if (hit !== undefined) return hit

  const offset = OFFSET.get(key)
  if (offset === undefined) {
    urlCache.set(key, null)
    return null
  }

  let buf
  try {
    buf = await ensureBitmap()
  } catch {
    // 数据异常不能让整个页面挂掉：退回 emoji 兜底（调用方 decision 用它）
    urlCache.set(key, null)
    return null
  }

  const pal = palette()
  const canvas = document.createElement('canvas')
  canvas.width = FLAG_SIZE
  canvas.height = FLAG_SIZE
  const ctx = canvas.getContext('2d')
  const img = ctx.createImageData(FLAG_SIZE, FLAG_SIZE)
  const px = img.data

  for (let i = 0; i < FLAG_SIZE * FLAG_SIZE; i++) {
    const v = buf[offset + i]
    if (v === TRANSPARENT) continue // 保持全透明
    const [r, g, b] = pal[v]
    const o = i * 4
    px[o] = r
    px[o + 1] = g
    px[o + 2] = b
    px[o + 3] = 255
  }
  ctx.putImageData(img, 0, 0)

  const url = canvas.toDataURL('image/png')
  urlCache.set(key, url)
  return url
}

/** 归一化国家代码：只接受两位字母。 */
export function normalize(code) {
  if (typeof code !== 'string') return ''
  const s = code.trim().toUpperCase()
  return /^[A-Z]{2}$/.test(s) ? s : ''
}

/**
 * 国旗 emoji（区域指示符）。**仅作最后兜底** —— 在 Windows 10 等
 * 缺少区域指示符字形的环境里会显示成方框，正常路径见 flagDataURL。
 */
export function flagEmoji(code) {
  const s = normalize(code)
  if (!s) return ''
  const base = 0x1f1e6
  const a = 'A'.charCodeAt(0)
  return String.fromCodePoint(base + (s.charCodeAt(0) - a), base + (s.charCodeAt(1) - a))
}

/** 是否内置了该代码的字形（同步，用于决定是否值得发起异步渲染）。 */
export function hasFlag(code) {
  return OFFSET.has(normalize(code))
}
