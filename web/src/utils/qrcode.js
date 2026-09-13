// 轻量 QR 码生成器（纯 JS，零依赖），用于渲染 otpauth:// 绑定二维码。
// 支持 Byte 模式（UTF-8）、纠错等级 L/M/Q/H，自动选择最小可用版本（1-40）。
// 仅实现二维码生成所需的必要路径：数据编码 -> 纠错码(RS) -> 矩阵构建 -> 掩码选择。

// ---------------------------------------------------------------- GF(256) 运算
const EXP = new Uint8Array(512)
const LOG = new Uint8Array(256)
;(function initGF() {
  let x = 1
  for (let i = 0; i < 255; i++) {
    EXP[i] = x
    LOG[x] = i
    x <<= 1
    if (x & 0x100) x ^= 0x11d
  }
  for (let i = 255; i < 512; i++) EXP[i] = EXP[i - 255]
})()

const gfMul = (a, b) => (a === 0 || b === 0 ? 0 : EXP[LOG[a] + LOG[b]])

// 生成 RSA 纠错码字
function rsEncode(data, ecCount) {
  const gen = generatorPoly(ecCount)
  const res = new Uint8Array(data.length + ecCount)
  res.set(data)
  for (let i = 0; i < data.length; i++) {
    const coef = res[i]
    if (coef === 0) continue
    for (let j = 0; j < gen.length; j++) {
      res[i + j] ^= gfMul(gen[j], coef)
    }
  }
  return res.slice(data.length)
}

function generatorPoly(degree) {
  let poly = new Uint8Array([1])
  for (let i = 0; i < degree; i++) {
    const next = new Uint8Array(poly.length + 1)
    for (let j = 0; j < poly.length; j++) {
      next[j] ^= poly[j]
      next[j + 1] ^= gfMul(poly[j], EXP[i])
    }
    poly = next
  }
  return poly
}

// ---------------------------------------------------------------- 版本容量表
// 各纠错等级在版本 1-40 下的：每块纠错码字数、块数
// 为保证体积可控，仅收录常用版本 1-20（足够容纳 otpauth URI）。
const EC_TABLE = {
  L: [
    [7, 1], [10, 1], [15, 1], [20, 1], [26, 1], [18, 2], [20, 2], [24, 2], [30, 2], [18, 2],
    [20, 4], [24, 4], [26, 4], [30, 4], [22, 4], [24, 4], [28, 4], [30, 4], [28, 5], [28, 5],
  ],
  M: [
    [10, 1], [16, 1], [26, 1], [18, 2], [24, 2], [16, 4], [18, 4], [22, 4], [22, 5], [26, 5],
    [30, 5], [22, 8], [22, 8], [24, 8], [24, 8], [28, 8], [28, 8], [26, 10], [26, 10], [26, 10],
  ],
  Q: [
    [13, 1], [22, 1], [18, 2], [26, 2], [18, 4], [24, 4], [18, 6], [22, 6], [20, 8], [24, 8],
    [28, 8], [26, 10], [24, 12], [20, 16], [30, 12], [24, 17], [28, 16], [28, 18], [26, 21], [26, 20],
  ],
  H: [
    [17, 1], [28, 1], [22, 2], [16, 4], [22, 4], [28, 4], [26, 5], [26, 6], [24, 8], [28, 8],
    [24, 11], [28, 11], [22, 16], [24, 16], [24, 18], [30, 16], [28, 19], [28, 21], [26, 25], [28, 25],
  ],
}

// 各版本总码字数（数据 + 纠错）
const TOTAL_CODEWORDS = [
  26, 44, 70, 100, 134, 172, 196, 242, 292, 346,
  404, 466, 532, 581, 655, 733, 815, 901, 991, 1085,
]

// 对齐图案中心坐标
const ALIGN_POS = [
  [], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34],
  [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50], [6, 30, 54],
  [6, 32, 58], [6, 34, 62], [6, 26, 46, 66], [6, 26, 48, 70], [6, 26, 50, 74],
  [6, 30, 54, 78], [6, 30, 56, 82], [6, 30, 58, 86], [6, 34, 62, 90], [6, 28, 50, 72, 94],
]

// 版本信息（版本 >= 7）
const VERSION_INFO = [
  0x07c94, 0x085bc, 0x09a99, 0x0a4d3, 0x0bbf6, 0x0c762, 0x0d847, 0x0e60d,
  0x0f928, 0x10b78, 0x1145d, 0x12a17, 0x13532, 0x149a6, 0x15683, 0x168c9,
  0x177ec, 0x18ec4, 0x191e1, 0x1afab, 0x1b08e,
]

// ---------------------------------------------------------------- UTF-8 编码
function toUtf8(str) {
  const out = []
  for (let i = 0; i < str.length; i++) {
    let c = str.charCodeAt(i)
    if (c < 0x80) out.push(c)
    else if (c < 0x800) {
      out.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f))
    } else if (c >= 0xd800 && c <= 0xdbff && i + 1 < str.length) {
      const c2 = str.charCodeAt(++i)
      const cp = 0x10000 + ((c - 0xd800) << 10) + (c2 - 0xdc00)
      out.push(0xf0 | (cp >> 18), 0x80 | ((cp >> 12) & 0x3f), 0x80 | ((cp >> 6) & 0x3f), 0x80 | (cp & 0x3f))
    } else {
      out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f))
    }
  }
  return out
}

// ---------------------------------------------------------------- 位缓冲
class BitBuffer {
  constructor() {
    this.bits = []
  }
  put(value, length) {
    for (let i = length - 1; i >= 0; i--) this.bits.push((value >>> i) & 1)
  }
  get length() {
    return this.bits.length
  }
}

// ---------------------------------------------------------------- 数据编码
function encodeData(text, ecLevel) {
  const bytes = toUtf8(text)
  const [ecPerBlock, blockCount] = findVersion(bytes.length, ecLevel)
  const version = findVersion.version
  const totalCw = TOTAL_CODEWORDS[version - 1]
  const dataCw = totalCw - ecPerBlock * blockCount

  const buf = new BitBuffer()
  buf.put(0b0100, 4) // Byte 模式
  const ccBits = version <= 9 ? 8 : 16
  buf.put(bytes.length, ccBits)
  for (const b of bytes) buf.put(b, 8)

  // 终止符
  const cap = dataCw * 8
  if (buf.length < cap) buf.put(0, Math.min(4, cap - buf.length))
  // 补齐到字节边界
  while (buf.length % 8 !== 0) buf.bits.push(0)
  // 填充字节 0xEC / 0x11
  const padBytes = [0xec, 0x11]
  let pi = 0
  while (buf.length < cap) buf.put(padBytes[pi++ % 2], 8)

  // 转码字
  const data = new Uint8Array(dataCw)
  for (let i = 0; i < buf.length; i += 8) {
    let v = 0
    for (let j = 0; j < 8; j++) v = (v << 1) | buf.bits[i + j]
    data[i / 8] = v
  }

  // 分块 + RS 纠错 + 交织
  const blocks = splitBlocks(data, dataCw, blockCount, ecPerBlock)
  for (const blk of blocks) blk.ec = rsEncode(blk.data, ecPerBlock)
  return { version, codewords: interleave(blocks, ecPerBlock), ecPerBlock, blockCount, dataCw }
}

function splitBlocks(data, dataCw, blockCount, ecPerBlock) {
  const blocks = []
  const shortLen = Math.floor(dataCw / blockCount)
  const numLong = dataCw % blockCount
  let offset = 0
  for (let i = 0; i < blockCount; i++) {
    const len = shortLen + (i >= blockCount - numLong ? 1 : 0)
    blocks.push({ data: data.slice(offset, offset + len), ec: null })
    offset += len
  }
  return blocks
}

function interleave(blocks, ecPerBlock) {
  const out = []
  const maxData = Math.max(...blocks.map((b) => b.data.length))
  for (let i = 0; i < maxData; i++) {
    for (const b of blocks) if (i < b.data.length) out.push(b.data[i])
  }
  for (let i = 0; i < ecPerBlock; i++) {
    for (const b of blocks) out.push(b.ec[i])
  }
  return out
}

function findVersion(byteLen, ecLevel) {
  const table = EC_TABLE[ecLevel]
  for (let v = 1; v <= 20; v++) {
    const [ecPerBlock, blockCount] = table[v - 1]
    const totalCw = TOTAL_CODEWORDS[v - 1]
    const dataCw = totalCw - ecPerBlock * blockCount
    const ccBits = v <= 9 ? 8 : 16
    const need = 4 + ccBits + byteLen * 8
    if (need <= dataCw * 8) {
      findVersion.version = v
      return [ecPerBlock, blockCount]
    }
  }
  throw new Error('内容过长，超出 QR 版本 20 容量')
}

// ---------------------------------------------------------------- 矩阵构建
function buildMatrix(version, codewords, ecLevel) {
  const size = version * 4 + 17
  const m = Array.from({ length: size }, () => new Uint8Array(size).fill(0))
  const reserved = Array.from({ length: size }, () => new Uint8Array(size).fill(0))

  placeFinder(m, reserved, 0, 0, size)
  placeFinder(m, reserved, size - 7, 0, size)
  placeFinder(m, reserved, 0, size - 7, size)
  placeTiming(m, reserved, size)
  placeAlignment(m, reserved, version, size)
  placeFormatInfo(m, reserved, ecLevel, size)
  if (version >= 7) placeVersionInfo(m, reserved, version, size)
  placeData(m, reserved, codewords, size)

  // 选择最优掩码
  let best = null
  let bestScore = Infinity
  for (let mask = 0; mask < 8; mask++) {
    const test = applyMask(m, reserved, mask, size)
    const score = penalty(test, size)
    if (score < bestScore) {
      bestScore = score
      best = { mask, matrix: test }
    }
  }
  // 写入最终格式信息（含掩码编号）
  placeFormatInfo(best.matrix, reserved, ecLevel, size, best.mask)
  return best.matrix
}

function placeFinder(m, r, col, row, size) {
  for (let dy = -1; dy <= 7; dy++) {
    for (let dx = -1; dx <= 7; dx++) {
      const y = row + dy
      const x = col + dx
      if (y < 0 || y >= size || x < 0 || x >= size) continue
      const inRing = (dy >= 0 && dy <= 6 && (dx === 0 || dx === 6)) || (dx >= 0 && dx <= 6 && (dy === 0 || dy === 6))
      const inCore = dy >= 2 && dy <= 4 && dx >= 2 && dx <= 4
      m[y][x] = inRing || inCore ? 1 : 0
      r[y][x] = 1
    }
  }
}

function placeTiming(m, r, size) {
  for (let i = 8; i < size - 8; i++) {
    if (!r[6][i]) { m[6][i] = i % 2 === 0 ? 1 : 0; r[6][i] = 1 }
    if (!r[i][6]) { m[i][6] = i % 2 === 0 ? 1 : 0; r[i][6] = 1 }
  }
}

function placeAlignment(m, r, version, size) {
  const pos = ALIGN_POS[version - 1] || []
  for (const cy of pos) {
    for (const cx of pos) {
      if (r[cy][cx]) continue
      for (let dy = -2; dy <= 2; dy++) {
        for (let dx = -2; dx <= 2; dx++) {
          const on = Math.max(Math.abs(dy), Math.abs(dx)) !== 1
          m[cy + dy][cx + dx] = on ? 1 : 0
          r[cy + dy][cx + dx] = 1
        }
      }
    }
  }
}

function placeFormatInfo(m, r, ecLevel, size, mask = 0) {
  const ecBits = { L: 0b01, M: 0b00, Q: 0b11, H: 0b10 }[ecLevel]
  const data = (ecBits << 3) | mask
  let rem = data
  for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >> 9) * 0x537)
  const fmt = ((data << 10) | rem) ^ 0x5412

  for (let i = 0; i <= 5; i++) setFmt(m, r, 8, i, (fmt >> i) & 1, size)
  setFmt(m, r, 8, 7, (fmt >> 6) & 1, size)
  setFmt(m, r, 8, 8, (fmt >> 7) & 1, size)
  setFmt(m, r, 7, 8, (fmt >> 8) & 1, size)
  for (let i = 9; i < 15; i++) setFmt(m, r, 14 - i, 8, (fmt >> i) & 1, size)

  for (let i = 0; i < 8; i++) setFmt(m, r, size - 1 - i, 8, (fmt >> i) & 1, size)
  for (let i = 8; i < 15; i++) setFmt(m, r, 8, size - 15 + i, (fmt >> i) & 1, size)
  m[size - 8][8] = 1
}

function setFmt(m, r, x, y, v, size) {
  if (x < 0 || y < 0 || x >= size || y >= size) return
  m[y][x] = v
  r[y][x] = 1
}

function placeVersionInfo(m, r, version, size) {
  const info = VERSION_INFO[version - 7]
  for (let i = 0; i < 18; i++) {
    const bit = (info >> i) & 1
    const a = Math.floor(i / 3)
    const b = i % 3
    m[size - 11 + b][a] = bit
    r[size - 11 + b][a] = 1
    m[a][size - 11 + b] = bit
    r[a][size - 11 + b] = 1
  }
}

function placeData(m, r, codewords, size) {
  let bitIndex = 0
  let upward = true
  for (let col = size - 1; col > 0; col -= 2) {
    if (col === 6) col--
    for (let i = 0; i < size; i++) {
      const row = upward ? size - 1 - i : i
      for (let c = 0; c < 2; c++) {
        const x = col - c
        if (r[row][x]) continue
        let bit = 0
        if (bitIndex < codewords.length * 8) {
          bit = (codewords[bitIndex >> 3] >> (7 - (bitIndex & 7))) & 1
        }
        m[row][x] = bit
        bitIndex++
      }
    }
    upward = !upward
  }
}

function applyMask(m, reserved, mask, size) {
  const out = m.map((row) => Uint8Array.from(row))
  const fn = [
    (i, j) => (i + j) % 2 === 0,
    (i, j) => i % 2 === 0,
    (i, j) => j % 3 === 0,
    (i, j) => (i + j) % 3 === 0,
    (i, j) => (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0,
    (i, j) => ((i * j) % 2) + ((i * j) % 3) === 0,
    (i, j) => (((i * j) % 2) + ((i * j) % 3)) % 2 === 0,
    (i, j) => (((i + j) % 2) + ((i * j) % 3)) % 2 === 0,
  ][mask]
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      if (!reserved[y][x] && fn(y, x)) out[y][x] ^= 1
    }
  }
  return out
}

function penalty(m, size) {
  let score = 0
  // 规则 1：行/列连续同色
  for (let i = 0; i < size; i++) {
    let runRow = 1
    let runCol = 1
    for (let j = 1; j < size; j++) {
      runRow = m[i][j] === m[i][j - 1] ? runRow + 1 : 1
      if (runRow === 5) score += 3
      else if (runRow > 5) score += 1
      runCol = m[j][i] === m[j - 1][i] ? runCol + 1 : 1
      if (runCol === 5) score += 3
      else if (runCol > 5) score += 1
    }
  }
  // 规则 2：2x2 同色块
  for (let y = 0; y < size - 1; y++) {
    for (let x = 0; x < size - 1; x++) {
      const v = m[y][x]
      if (v === m[y][x + 1] && v === m[y + 1][x] && v === m[y + 1][x + 1]) score += 3
    }
  }
  // 规则 3：1:1:3:1:1 比率
  const pat1 = [1, 0, 1, 1, 1, 0, 1, 0, 0, 0, 0]
  const pat2 = [0, 0, 0, 0, 1, 0, 1, 1, 1, 0, 1]
  for (let i = 0; i < size; i++) {
    for (let j = 0; j <= size - 11; j++) {
      let m1 = true
      let m2 = true
      let m3 = true
      let m4 = true
      for (let k = 0; k < 11; k++) {
        if (m[i][j + k] !== pat1[k]) m1 = false
        if (m[i][j + k] !== pat2[k]) m2 = false
        if (m[j + k][i] !== pat1[k]) m3 = false
        if (m[j + k][i] !== pat2[k]) m4 = false
      }
      if (m1) score += 40
      if (m2) score += 40
      if (m3) score += 40
      if (m4) score += 40
    }
  }
  // 规则 4：深色比例
  let dark = 0
  for (let y = 0; y < size; y++) for (let x = 0; x < size; x++) dark += m[y][x]
  const ratio = (dark * 100) / (size * size)
  score += Math.floor(Math.abs(ratio - 50) / 5) * 10
  return score
}

// ---------------------------------------------------------------- 对外接口
// generateMatrix 生成 QR 码矩阵（二维 0/1 数组）。ecLevel 取 L/M/Q/H。
export function generateMatrix(text, ecLevel = 'M') {
  const { version, codewords } = encodeData(text, ecLevel)
  return buildMatrix(version, codewords, ecLevel)
}

// toSvgPath 将矩阵转为 SVG path 的 d 属性（每个模块一个方块）。
export function toSvgPath(matrix) {
  const parts = []
  for (let y = 0; y < matrix.length; y++) {
    for (let x = 0; x < matrix[y].length; x++) {
      if (matrix[y][x]) parts.push(`M${x} ${y}h1v1h-1z`)
    }
  }
  return parts.join('')
}

// createQrSvg 生成可直接插入的 SVG 字符串（含白色静区）。
export function createQrSvg(text, size = 200, ecLevel = 'M') {
  const matrix = generateMatrix(text, ecLevel)
  const n = matrix.length
  const quiet = 4
  const dim = n + quiet * 2
  const path = toSvgPath(matrix)
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${size}" height="${size}" viewBox="0 0 ${dim} ${dim}" shape-rendering="crispEdges"><rect width="${dim}" height="${dim}" fill="#ffffff"/><g transform="translate(${quiet},${quiet})"><path d="${path}" fill="#000000"/></g></svg>`
}
