#!/usr/bin/env node
// 生成国旗字形资源（web/src/utils/flagSprite.js）。
//
// ## 为什么需要它（对应 PR #11「国旗 emoji 不显示」）
//
// 直接用区域指示符拼国旗 emoji（`String.fromCodePoint(0x1F1E6 + …)`）时，
// 字形由**访客的操作系统字体**提供。三类环境拿不到彩色字形：
//
//   1. Windows 10 及更早：Segoe UI Emoji 没有区域指示符连字，
//      浏览器把两个字母各画一个方框 —— 页面看起来就是"国旗没出来"；
//   2. 精简版 Linux 容器 / 无桌面的服务器：根本没有 emoji 字体；
//   3. 企业策略禁用彩色字体的浏览器。
//
// 症状因此是"接口里 country_code 明明有值、国旗却空白/方框"，
// 且**只在部分访客的机器上复现**（运维本机是好的）—— 这正是
// 「改了几次仍没实现」的根因：问题在渲染层，不在数据链路。
//
// 本脚本把字形**固化进前端资源**：渲染不依赖宿主字体，也不请求外部 CDN。
//
// ## 为什么是位图而不是内联 SVG
//
// 259 面国旗的原始 SVG 合计约 630 KB —— 即使做路径去重 + 数值编码，
// gzip 后仍有 200 KB 级，对首屏是明显负担。
// 而国旗实际显示尺寸只有 14–18 CSS px：栅格化到 36×36（覆盖 2x DPI）
// 后视觉上与原矢量**无从分辨**，体积却只有 1/6。
// 因此这里选择：36×36 RGBA → 二值化 alpha → 全局 160 色调色板 → 8bit/px → deflate。
//
// ## 数据
//
//   - 代码/码点对照：iamcal/emoji-data（MIT）
//   - 字形：Twemoji（CC-BY 4.0）
//
// ## 栅格化依赖
//
// 需要 `cairosvg` 与 `Pillow`。缺失时脚本直接失败并给出安装提示，
// 不会产出半成品资源。解码逻辑见 src/utils/flag.js，两处需同步修改。
//
// 用法：node web/scripts/gen-flag-sprite.mjs

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import zlib from 'node:zlib'
import { fileURLToPath } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const OUT = path.resolve(HERE, '../src/utils/flagSprite.js')

// ISO 3166-1 alpha-2 全集（249 个正式代码）+ 常用非正式/特殊代码。
// 同时用于**校验**：上游数据缺任何一项都直接失败，而不是悄悄少几面旗
//（那种问题只有用户看到空白才会发现）。
const ISO_CODES = (
  'AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ ' +
  'CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR ' +
  'GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP ' +
  'KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT ' +
  'MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW ' +
  'SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG ' +
  'UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'
).split(/\s+/).filter(Boolean)
// 特殊/非正式代码：emoji-data 有字形且 IP 库可能返回（科索沃、欧盟等）。
const EXTRA_CODES = ['XK', 'EU', 'UN', 'AC', 'TA', 'CP', 'DG', 'EA', 'IC']

const SIZE = 36 // 栅格边长，覆盖 2x DPI 下 18px 的显示尺寸
const COLORS = 160 // 全局调色板容量
const ALPHA_CUTOFF = 128 // alpha 二值化阈值：由浏览器缩放时的双线性插值补回平滑

const EMOJI_DATA = 'https://raw.githubusercontent.com/iamcal/emoji-data/master/emoji.json'
const glyphURL = (u) =>
  `https://raw.githubusercontent.com/jdecked/twemoji/main/assets/svg/${u.toLowerCase()}.svg`

const die = (msg) => {
  console.error(`[gen-flag-sprite] ${msg}`)
  process.exit(1)
}

async function get(url) {
  const r = await fetch(url, { redirect: 'follow' })
  if (!r.ok) throw new Error(`GET ${url} -> HTTP ${r.status}`)
  return r
}

function havePythonDeps() {
  const r = spawnSync('python3', ['-c', 'import cairosvg, PIL'], { stdio: 'ignore' })
  return r.status === 0
}

if (!havePythonDeps()) {
  die('缺少栅格化依赖。请先安装：pip3 install cairosvg pillow')
}

// --- 1. 「国家代码 -> 字形码点」 ---
const emoji = await (await get(EMOJI_DATA)).json()
const glyphOf = new Map()
for (const e of emoji) {
  if (e.category !== 'Flags') continue
  const parts = String(e.unified || '').split('-')
  if (parts.length !== 2) continue // 只取两面区域指示符构成的国旗
  const cps = parts.map((x) => parseInt(x, 16))
  if (!cps.every((c) => c >= 0x1f1e6 && c <= 0x1f1ff)) continue
  const code = String.fromCharCode(...cps.map((c) => c - 0x1f1e6 + 65))
  if (!glyphOf.has(code)) glyphOf.set(code, e.unified)
}

const codes = [...ISO_CODES, ...EXTRA_CODES]
const missing = codes.filter((c) => !glyphOf.has(c))
if (missing.length) die(`上游数据缺少 ${missing.length} 个代码的字形: ${missing.join(' ')}`)

// --- 2. 下载 SVG 到临时目录 ---
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'mocenter-flags-'))
const files = []
for (const code of codes) {
  const svg = await (await get(glyphURL(glyphOf.get(code)))).text()
  if (!/<svg/.test(svg)) die(`${code} 的字形不是 SVG`)
  const f = path.join(tmp, `${code}.svg`)
  fs.writeFileSync(f, svg)
  files.push(f)
}

// --- 3. 栅格化 + 量化 + 压缩（交给 Python：cairosvg/Pillow 在这里最省事） ---
const py = `
import base64, json, sys, zlib
import cairosvg
from PIL import Image
from collections import Counter

codes = json.loads(sys.argv[1])
tmp, out_path = sys.argv[2], sys.argv[3]
SIZE, COLORS, CUTOFF = int(sys.argv[4]), int(sys.argv[5]), int(sys.argv[6])

pixels_per_flag = []
hist = Counter()
for c in codes:
    svg = open(f'{tmp}/{c}.svg', 'rb').read()
    png = cairosvg.svg2png(bytestring=svg, output_width=SIZE, output_height=SIZE)
    import io
    px = list(Image.open(io.BytesIO(png)).convert('RGBA').getdata())
    if len(px) != SIZE * SIZE:
        raise SystemExit(f'{c} 栅格化尺寸不符: {len(px)}')
    pixels_per_flag.append(px)
    for p in px:
        hist[p] += 1

# 全局调色板：只用不透明像素参与中位切分（透明像素不占颜色预算）。
# 每色按频次加权，上限 300 避免个别高频色支配切分。
samples = []
for color, n in hist.items():
    if color[3] < CUTOFF:
        continue
    samples.extend([color[:3]] * min(n, 300))
if not samples:
    raise SystemExit('没有不透明像素，调色板为空')
strip = Image.new('RGB', (len(samples), 1))
strip.putdata(samples)
quant = strip.quantize(colors=COLORS, method=Image.Quantize.MEDIANCUT)
raw_pal = quant.getpalette()[: COLORS * 3]
palette = [tuple(raw_pal[i * 3 : i * 3 + 3]) for i in range(COLORS)]

# 颜色 -> 调色板下标（最近邻）。同一颜色只算一次。
nearest = {}
def index_of(rgb):
    hit = nearest.get(rgb)
    if hit is not None:
        return hit
    best, best_d = 0, 1 << 30
    for i, p in enumerate(palette):
        d = (p[0] - rgb[0]) ** 2 + (p[1] - rgb[1]) ** 2 + (p[2] - rgb[2]) ** 2
        if d < best_d:
            best, best_d = i, d
    nearest[rgb] = best
    return best

data = bytearray()
for px in pixels_per_flag:
    for p in px:
        data.append(COLORS if p[3] < CUTOFF else index_of(p[:3]))

json.dump(
    {
        'size': SIZE,
        'colors': COLORS,
        'palette': ''.join('%02x%02x%02x' % p for p in palette),
        'data': base64.b64encode(zlib.compress(bytes(data), 9)).decode('ascii'),
    },
    open(out_path, 'w'),
)
print(f'栅格 {len(codes)} 面 x {SIZE}x{SIZE}，调色板 {COLORS} 色，'
      f'压缩后 {len(zlib.compress(bytes(data), 9)) / 1024:.1f} KB', file=sys.stderr)
`

const jsonPath = path.join(tmp, 'out.json')
const r = spawnSync('python3', ['-c', py, JSON.stringify(codes), tmp, jsonPath,
  String(SIZE), String(COLORS), String(ALPHA_CUTOFF)], { stdio: ['ignore', 'inherit', 'inherit'] })
if (r.status !== 0) die('栅格化失败（见上方 Python 输出）')

const out = JSON.parse(fs.readFileSync(jsonPath, 'utf8'))
const packed = Buffer.from(out.data, 'base64')
if (!packed.length) die('压缩数据为空')

// --- 4. 自检：解压后长度必须与「代码数 × 边长²」一致 ---
const expected = codes.length * SIZE * SIZE
const inflated = zlib.inflateSync(packed)
if (inflated.length !== expected) {
  die(`自检失败：解压后 ${inflated.length} 字节，期望 ${expected}`)
}
const badPixel = [...inflated].find((v) => v > COLORS)
if (badPixel !== undefined) die(`自检失败：出现越界的调色板下标 ${badPixel}`)

const header = `// 本文件由 web/scripts/gen-flag-sprite.mjs 自动生成，请勿手工编辑。
//
// 国旗字形，让国旗渲染**不依赖访客的系统 emoji 字体**：
// Windows 10 / 无字体容器里区域指示符 emoji 会渲染成方框，这就是
// 「country_code 有值但国旗不显示」的根因。
//
// 字形来源 Twemoji（CC-BY 4.0），代码对照来源 iamcal/emoji-data（MIT）。
// 解码逻辑见 src/utils/flag.js —— 两处需同步修改。
//
// 数据布局：每面国旗 ${SIZE}×${SIZE} 像素，按代码顺序紧邻排列，
// 每字节一个调色板下标；值等于调色板长度表示**全透明**。
`

const body = `${header}
/** 栅格边长（像素）。前端按 CSS 尺寸缩放显示，${SIZE}px 覆盖 2x DPI。 */
export const FLAG_SIZE = ${SIZE}

/** 调色板长度。位图里等于该值的下标表示全透明。 */
export const FLAG_COLORS = ${COLORS}

/** 调色板：每 6 个十六进制字符一个 RGB 颜色，按下标排列。 */
export const FLAG_PALETTE = '${out.palette}'

/** ISO 3166-1 alpha-2 代码，与位图数据按下标一一对应。 */
export const FLAG_CODES = ${JSON.stringify(codes)}

/** 位图数据（defLate 压缩 + base64）。前端解码见 src/utils/flag.js。 */
export const FLAG_DATA = '${out.data}'
`

fs.writeFileSync(OUT, body)
fs.rmSync(tmp, { recursive: true, force: true })

const kb = (n) => `${(n / 1024).toFixed(1)} KB`
console.log(`已生成 ${OUT}`)
console.log(`  国旗数量: ${codes.length}（${ISO_CODES.length} 个 ISO 代码 + ${EXTRA_CODES.length} 个特殊代码）`)
console.log(`  栅格/调色板: ${SIZE}×${SIZE} / ${COLORS} 色`)
console.log(`  位图压缩: ${kb(packed.length)}（未压缩 ${kb(expected)}）`)
console.log(`  文件体积: ${kb(body.length)}`)
console.log('  自检通过：解压长度与调色板下标均合法')
