#!/usr/bin/env node
// 校验国旗字形数据与解码逻辑是否自洽（CI / 本地均可跑，无需网络）。
//
// 为什么需要它：字形数据是**生成**的，解码是**手写**的。
// 两边一旦脱节（改了生成脚本忘了改解码，反之亦然），
// 症状是"国旗又不见了"—— 而这恰恰是本 PR 反复出问题的形态。
// 这里用 node 侧的独立实现解一遍，至少保证：
//
//   1. base64 可解、deflate 可解；
//   2. 解压后长度 = 代码数 × 边长²；
//   3. 每个像素值都在 [0, 调色板长度] 区间内（含透明标记）；
//   4. 常用国家的字形**不是全透明**（否则就是"文件在、画不出来"）；
//   5. 汉字/emoji 字体缺失的环境下也能工作 —— 这里通过"数据自带颜色"间接保证。
//
// 用法：node web/scripts/check-flag-sprite.mjs

import zlib from 'node:zlib'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const mod = await import(path.resolve(HERE, '../src/utils/flagSprite.js'))

const { FLAG_SIZE, FLAG_COLORS, FLAG_PALETTE, FLAG_CODES, FLAG_DATA } = mod

const problems = []
const check = (cond, msg) => { if (!cond) problems.push(msg) }

// --- 结构 ---
check(Number.isInteger(FLAG_SIZE) && FLAG_SIZE > 0, `FLAG_SIZE 非法: ${FLAG_SIZE}`)
check(Number.isInteger(FLAG_COLORS) && FLAG_COLORS > 0, `FLAG_COLORS 非法: ${FLAG_COLORS}`)
check(
  FLAG_PALETTE.length === FLAG_COLORS * 6,
  `调色板长度应为 ${FLAG_COLORS * 6}，实际 ${FLAG_PALETTE.length}`
)
check(/^[0-9a-f]+$/.test(FLAG_PALETTE), '调色板含非十六进制字符')
check(Array.isArray(FLAG_CODES) && FLAG_CODES.length > 200, '代码表过短，疑似生成失败')
check(new Set(FLAG_CODES).size === FLAG_CODES.length, '代码表存在重复项')

// --- 解压 ---
let pixels
try {
  pixels = zlib.inflateSync(Buffer.from(FLAG_DATA, 'base64'))
} catch (e) {
  problems.push(`位图数据无法解压: ${e.message}`)
}

if (pixels) {
  const expected = FLAG_CODES.length * FLAG_SIZE * FLAG_SIZE
  check(pixels.length === expected, `解压长度 ${pixels.length} != 期望 ${expected}`)

  // --- 像素值域 ---
  let outOfRange = 0
  // --- 每面国旗的非透明像素数（全透明的旗 = 画不出东西）---
  let blank = []
  for (let i = 0; i < FLAG_CODES.length; i++) {
    const base = i * FLAG_SIZE * FLAG_SIZE
    let opaque = 0
    for (let j = 0; j < FLAG_SIZE * FLAG_SIZE; j++) {
      const v = pixels[base + j]
      if (v > FLAG_COLORS) outOfRange++
      else if (v !== FLAG_COLORS) opaque++
    }
    // 至少 20% 的像素不透明才算"有图形"（国旗基本都是满幅色块）
    if (opaque < FLAG_SIZE * FLAG_SIZE * 0.2) {
      blank.push(`${FLAG_CODES[i]}(${opaque})`)
    }
  }
  check(outOfRange === 0, `有 ${outOfRange} 个像素值超出调色板范围`)
  check(blank.length === 0, `以下代码的字形几乎全透明: ${blank.join(' ')}`)

  // --- 抽查：常用国家必须存在且颜色合理 ---
  const mustHave = ['CN', 'US', 'JP', 'GB', 'DE', 'FR', 'RU', 'IN', 'BR', 'AU', 'CA', 'KR', 'SG', 'HK', 'TW']
  const idx = new Map(FLAG_CODES.map((c, i) => [c, i]))
  const missing = mustHave.filter((c) => !idx.has(c))
  check(missing.length === 0, `缺少常用代码: ${missing.join(' ')}`)

  // 抽查 CN：应以红色像素为主（避免"字形错位"这类生成事故）
  if (idx.has('CN')) {
    const base = idx.get('CN') * FLAG_SIZE * FLAG_SIZE
    let red = 0
    let opaque = 0
    for (let j = 0; j < FLAG_SIZE * FLAG_SIZE; j++) {
      const v = pixels[base + j]
      if (v === FLAG_COLORS) continue
      opaque++
      const o = v * 6
      const [r, g, b] = [
        parseInt(FLAG_PALETTE.slice(o, o + 2), 16),
        parseInt(FLAG_PALETTE.slice(o + 2, o + 4), 16),
        parseInt(FLAG_PALETTE.slice(o + 4, o + 6), 16),
      ]
      if (r > 140 && r > g * 2 && r > b * 2) red++
    }
    check(opaque > 0 && red / opaque > 0.4, `CN 国旗红色像素占比异常（${red}/${opaque}）`)
  }
}

// --- 覆盖 ISO 3166-1 的完整性（对照生成脚本里的清单）---
const ISO = (
  'AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ ' +
  'CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR ' +
  'GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP ' +
  'KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT ' +
  'MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW ' +
  'SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG ' +
  'UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'
).split(/\s+/).filter(Boolean)
const have = new Set(FLAG_CODES)
const missingISO = ISO.filter((c) => !have.has(c))
check(missingISO.length === 0, `缺少 ${missingISO.length} 个 ISO 代码: ${missingISO.join(' ')}`)

// --- 结果 ---
if (problems.length) {
  console.error('国旗字形校验失败：')
  for (const p of problems) console.error(`  - ${p}`)
  console.error('\n若字形数据本身有问题，请重新运行：node web/scripts/gen-flag-sprite.mjs')
  process.exit(1)
}

const kb = (n) => `${(n / 1024).toFixed(1)} KB`
console.log('国旗字形校验通过')
console.log(`  数量: ${FLAG_CODES.length}（含 ${ISO.length} 个 ISO 代码）`)
console.log(`  栅格: ${FLAG_SIZE}×${FLAG_SIZE}，调色板 ${FLAG_COLORS} 色`)
console.log(`  位图: ${kb(zlib.inflateSync(Buffer.from(FLAG_DATA, 'base64')).length)} 解压后`)
console.log(`  文件: ${kb(fs.statSync(path.resolve(HERE, '../src/utils/flagSprite.js')).size)}`)
