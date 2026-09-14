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
// 标准 RS 分块表（ISO/IEC 18004）。每项为三元组展开：
//   [块数, 每块总码字, 每块数据码字, (可重复) ...]
// 同一版本+纠错级别下可能存在「长短块混合」（如 5-Q 为 2 块 15 数据字 + 2 块 16 数据字），
// 因此不能用单个 (ecPerBlock, blockCount) 表示——必须按组存储。
// 数据顺序与纠错级别索引一致：L / M / Q / H，覆盖版本 1-40。
const RS_BLOCKS = {
  L: [
    [1, 26, 19], [1, 44, 34],
    [1, 70, 55], [1, 100, 80],
    [1, 134, 108], [2, 86, 68],
    [2, 98, 78], [2, 121, 97],
    [2, 146, 116], [2, 86, 68, 2, 87, 69],
    [4, 101, 81], [2, 116, 92, 2, 117, 93],
    [4, 133, 107], [3, 145, 115, 1, 146, 116],
    [5, 109, 87, 1, 110, 88], [5, 122, 98, 1, 123, 99],
    [1, 135, 107, 5, 136, 108], [5, 150, 120, 1, 151, 121],
    [3, 141, 113, 4, 142, 114], [3, 135, 107, 5, 136, 108],
    [4, 144, 116, 4, 145, 117], [2, 139, 111, 7, 140, 112],
    [4, 151, 121, 5, 152, 122], [6, 147, 117, 4, 148, 118],
    [8, 132, 106, 4, 133, 107], [10, 142, 114, 2, 143, 115],
    [8, 152, 122, 4, 153, 123], [3, 147, 117, 10, 148, 118],
    [7, 146, 116, 7, 147, 117], [5, 145, 115, 10, 146, 116],
    [13, 145, 115, 3, 146, 116], [17, 145, 115],
    [17, 145, 115, 1, 146, 116], [13, 145, 115, 6, 146, 116],
    [12, 151, 121, 7, 152, 122], [6, 151, 121, 14, 152, 122],
    [17, 152, 122, 4, 153, 123], [4, 152, 122, 18, 153, 123],
    [20, 147, 117, 4, 148, 118], [19, 148, 118, 6, 149, 119],
  ],
  M: [
    [1, 26, 16], [1, 44, 28],
    [1, 70, 44], [2, 50, 32],
    [2, 67, 43], [4, 43, 27],
    [4, 49, 31], [2, 60, 38, 2, 61, 39],
    [3, 58, 36, 2, 59, 37], [4, 69, 43, 1, 70, 44],
    [1, 80, 50, 4, 81, 51], [6, 58, 36, 2, 59, 37],
    [8, 59, 37, 1, 60, 38], [4, 64, 40, 5, 65, 41],
    [5, 65, 41, 5, 66, 42], [7, 73, 45, 3, 74, 46],
    [10, 74, 46, 1, 75, 47], [9, 69, 43, 4, 70, 44],
    [3, 70, 44, 11, 71, 45], [3, 67, 41, 13, 68, 42],
    [17, 68, 42], [17, 74, 46],
    [4, 75, 47, 14, 76, 48], [6, 73, 45, 14, 74, 46],
    [8, 75, 47, 13, 76, 48], [19, 74, 46, 4, 75, 47],
    [22, 73, 45, 3, 74, 46], [3, 73, 45, 23, 74, 46],
    [21, 73, 45, 7, 74, 46], [19, 75, 47, 10, 76, 48],
    [2, 74, 46, 29, 75, 47], [10, 74, 46, 23, 75, 47],
    [14, 74, 46, 21, 75, 47], [14, 74, 46, 23, 75, 47],
    [12, 75, 47, 26, 76, 48], [6, 75, 47, 34, 76, 48],
    [29, 74, 46, 14, 75, 47], [13, 74, 46, 32, 75, 47],
    [40, 75, 47, 7, 76, 48], [18, 75, 47, 31, 76, 48],
  ],
  Q: [
    [1, 26, 13], [1, 44, 22],
    [2, 35, 17], [2, 50, 24],
    [2, 33, 15, 2, 34, 16], [4, 43, 19],
    [2, 32, 14, 4, 33, 15], [4, 40, 18, 2, 41, 19],
    [4, 36, 16, 4, 37, 17], [6, 43, 19, 2, 44, 20],
    [4, 50, 22, 4, 51, 23], [4, 46, 20, 6, 47, 21],
    [8, 44, 20, 4, 45, 21], [11, 36, 16, 5, 37, 17],
    [5, 54, 24, 7, 55, 25], [15, 43, 19, 2, 44, 20],
    [1, 50, 22, 15, 51, 23], [17, 50, 22, 1, 51, 23],
    [17, 47, 21, 4, 48, 22], [15, 54, 24, 5, 55, 25],
    [17, 50, 22, 6, 51, 23], [7, 54, 24, 16, 55, 25],
    [11, 54, 24, 14, 55, 25], [11, 54, 24, 16, 55, 25],
    [7, 54, 24, 22, 55, 25], [28, 50, 22, 6, 51, 23],
    [8, 53, 23, 26, 54, 24], [4, 54, 24, 31, 55, 25],
    [1, 53, 23, 37, 54, 24], [15, 54, 24, 25, 55, 25],
    [42, 54, 24, 1, 55, 25], [10, 54, 24, 35, 55, 25],
    [29, 54, 24, 19, 55, 25], [44, 54, 24, 7, 55, 25],
    [39, 54, 24, 14, 55, 25], [46, 54, 24, 10, 55, 25],
    [49, 54, 24, 10, 55, 25], [48, 54, 24, 14, 55, 25],
    [43, 54, 24, 22, 55, 25], [34, 54, 24, 34, 55, 25],
  ],
  H: [
    [1, 26, 9], [1, 44, 16],
    [2, 35, 13], [4, 25, 9],
    [2, 33, 11, 2, 34, 12], [4, 43, 15],
    [4, 39, 13, 1, 40, 14], [4, 40, 14, 2, 41, 15],
    [4, 36, 12, 4, 37, 13], [6, 43, 15, 2, 44, 16],
    [3, 36, 12, 8, 37, 13], [7, 42, 14, 4, 43, 15],
    [12, 33, 11, 4, 34, 12], [11, 36, 12, 5, 37, 13],
    [11, 36, 12, 7, 37, 13], [3, 45, 15, 13, 46, 16],
    [2, 42, 14, 17, 43, 15], [2, 42, 14, 19, 43, 15],
    [9, 39, 13, 16, 40, 14], [15, 43, 15, 10, 44, 16],
    [19, 46, 16, 6, 47, 17], [34, 37, 13],
    [16, 45, 15, 14, 46, 16], [30, 46, 16, 2, 47, 17],
    [22, 45, 15, 13, 46, 16], [33, 46, 16, 4, 47, 17],
    [12, 45, 15, 28, 46, 16], [11, 45, 15, 31, 46, 16],
    [19, 45, 15, 26, 46, 16], [23, 45, 15, 25, 46, 16],
    [23, 45, 15, 28, 46, 16], [19, 45, 15, 35, 46, 16],
    [11, 45, 15, 46, 46, 16], [59, 46, 16, 1, 47, 17],
    [22, 45, 15, 41, 46, 16], [2, 45, 15, 64, 46, 16],
    [24, 45, 15, 46, 46, 16], [42, 45, 15, 32, 46, 16],
    [10, 45, 15, 67, 46, 16], [20, 45, 15, 61, 46, 16],
  ],
}

// 各版本总码字数（数据 + 纠错），版本 1-40
const TOTAL_CODEWORDS = [
  26, 44, 70, 100, 134, 172, 196, 242, 292, 346,
  404, 466, 532, 581, 655, 733, 815, 901, 991, 1085,
  1156, 1258, 1364, 1474, 1588, 1706, 1828, 1921, 2051, 2185,
  2323, 2465, 2611, 2761, 2876, 3034, 3196, 3362, 3532, 3706,
]

// 对齐图案中心坐标（版本 1-40）
const ALIGN_POS = [
  [], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34],
  [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50], [6, 30, 54],
  [6, 32, 58], [6, 34, 62], [6, 26, 46, 66], [6, 26, 48, 70], [6, 26, 50, 74],
  [6, 30, 54, 78], [6, 30, 56, 82], [6, 30, 58, 86], [6, 34, 62, 90],
  [6, 28, 50, 72, 94], [6, 26, 50, 74, 98], [6, 30, 54, 78, 102],
  [6, 28, 54, 80, 106], [6, 32, 58, 84, 110], [6, 30, 58, 86, 114],
  [6, 34, 62, 90, 118], [6, 26, 50, 74, 98, 122], [6, 30, 54, 78, 102, 126],
  [6, 26, 52, 78, 104, 130], [6, 30, 56, 82, 108, 134],
  [6, 34, 60, 86, 112, 138], [6, 30, 58, 86, 114, 142],
  [6, 34, 62, 90, 118, 146], [6, 30, 54, 78, 102, 126, 150],
  [6, 24, 50, 76, 102, 128, 154], [6, 28, 54, 80, 106, 132, 158],
  [6, 32, 58, 84, 110, 136, 162], [6, 26, 54, 82, 110, 138, 166],
  [6, 30, 58, 86, 114, 142, 170],
]

// 版本信息（版本 7-40）
const VERSION_INFO = [
  0x07c94, 0x085bc, 0x09a99, 0x0a4d3, 0x0bbf6, 0x0c762, 0x0d847, 0x0e60d,
  0x0f928, 0x10b78, 0x1145d, 0x12a17, 0x13532, 0x149a6, 0x15683, 0x168c9,
  0x177ec, 0x18ec4, 0x191e1, 0x1afab, 0x1b08e, 0x1cc1a, 0x1d33f, 0x1ed75,
  0x1f250, 0x209d5, 0x216f0, 0x228ba, 0x2379f, 0x24b0b, 0x2542e, 0x26a64,
  0x27541, 0x28c69,
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
  const { version, groups } = findVersion(bytes.length, ecLevel)
  const totalCw = TOTAL_CODEWORDS[version - 1]
  const dataCw = groups.reduce((n, g) => n + g.count * g.dataLen, 0)
  if (dataCw + groups.reduce((n, g) => n + g.count * g.ecLen, 0) !== totalCw) {
    throw new Error('QR 分块表与总码字数不一致（内部错误）')
  }

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
  const blocks = splitBlocks(data, groups)
  for (const blk of blocks) blk.ec = rsEncode(blk.data, blk.ecLen)
  return { version, codewords: interleave(blocks), ecPerBlock: groups[0].ecLen, blockCount: blocks.length, dataCw }
}

function splitBlocks(data, groups) {
  const blocks = []
  let offset = 0
  for (const g of groups) {
    for (let i = 0; i < g.count; i++) {
      blocks.push({ data: data.slice(offset, offset + g.dataLen), ec: null, ecLen: g.ecLen })
      offset += g.dataLen
    }
  }
  return blocks
}

function interleave(blocks) {
  const out = []
  const maxData = Math.max(...blocks.map((b) => b.data.length))
  for (let i = 0; i < maxData; i++) {
    for (const b of blocks) if (i < b.data.length) out.push(b.data[i])
  }
  const maxEc = Math.max(...blocks.map((b) => b.ec.length))
  for (let i = 0; i < maxEc; i++) {
    for (const b of blocks) if (i < b.ec.length) out.push(b.ec[i])
  }
  return out
}

// 解析 RS_BLOCKS 的一行，展平成 [{count, ecLen, dataLen}]
function parseGroups(row) {
  const groups = []
  for (let i = 0; i < row.length; i += 3) {
    const [count, total, dataLen] = [row[i], row[i + 1], row[i + 2]]
    groups.push({ count, ecLen: total - dataLen, dataLen })
  }
  return groups
}

function findVersion(byteLen, ecLevel) {
  const table = RS_BLOCKS[ecLevel]
  for (let v = 1; v <= 40; v++) {
    const groups = parseGroups(table[v - 1])
    const dataCw = groups.reduce((n, g) => n + g.count * g.dataLen, 0)
    const ccBits = v <= 9 ? 8 : 16
    // 字符计数指示符自身的位数也要占位
    const need = 4 + ccBits + byteLen * 8
    if (need <= dataCw * 8) {
      return { version: v, groups }
    }
  }
  throw new Error('内容过长，超出 QR 版本 40 容量')
}

// ---------------------------------------------------------------- 矩阵构建
function buildMatrix(version, codewords, ecLevel) {
  const size = version * 4 + 17
  const m = Array.from({ length: size }, () => new Uint8Array(size).fill(0))
  const reserved = Array.from({ length: size }, () => new Uint8Array(size).fill(0))

  placeFinder(m, reserved, 0, 0, size)
  placeFinder(m, reserved, size - 7, 0, size)
  placeFinder(m, reserved, 0, size - 7, size)
  // 顺序至关重要：对齐图案必须先于时序图案摆放。
  // 标准实现（python-qrcode）先 setup_position_adjust_pattern 再 setup_timing_pattern，
  // 这样时序线会「跳过」已被对齐图案占用的格子；反之对齐图案会被时序线挡住而整块丢失。
  placeAlignment(m, reserved, version, size)
  placeTiming(m, reserved, size)
  // 先占位（置 reserved），保证 placeData 不会侵入功能图案区。
  if (version >= 7) placeVersionInfo(m, reserved, version, size)
  placeFormatInfo(m, reserved, ecLevel, size, 0)
  placeData(m, reserved, codewords, size)

  // 选择最优掩码：对每个掩码先套用掩码，再写入该掩码对应的格式信息（覆盖上一轮的），
  // 然后按标准惩罚规则打分。格式位已在 reserved 中置位，故掩码不会翻转它们。
  let best = null
  let bestScore = Infinity
  for (let mask = 0; mask < 8; mask++) {
    const test = applyMask(m, reserved, mask, size)
    placeFormatInfo(test, reserved, ecLevel, size, mask)
    const score = penalty(test, size)
    if (score < bestScore) {
      bestScore = score
      best = { mask, matrix: test }
    }
  }
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
      // 与标准实现一致：仅当中心已被占用（落在探测图案区/分隔符里）时整体跳过。
      // 不能按「靠近角落就跳过」的规则——那会误删位于时序线附近的合法对齐图案
      // （例如版本 7 的 (6,22) 与 (22,6)），正是导致 v>=7 无法扫描的根因。
      if (r[cy][cx]) continue
      for (let dy = -2; dy <= 2; dy++) {
        for (let dx = -2; dx <= 2; dx++) {
          // 5x5 对齐图案：外框(∣dy∣=2 或 ∣dx∣=2)深色，中心(0,0)深色，
          // 中间一圈浅色 —— 即 max(|dy|,|dx|) !== 1。
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

  // 第一副本（沿左上角探测图案的两条边）：
  //   bit 0..5  -> 列 8，行 0..5（竖排，自下向上跳过时序行 6）
  //   bit 6     -> (8, 7)
  //   bit 7     -> (8, 8)
  //   bit 8     -> (7, 8)
  //   bit 9..14 -> 行 8，列 5..0（横排）
  // 注意：不能把 bit 8..14 摆成竖排——竖排那一段属于第二副本。
  for (let i = 0; i <= 5; i++) setFmt(m, r, 8, i, (fmt >> i) & 1, size)
  setFmt(m, r, 8, 7, (fmt >> 6) & 1, size)
  setFmt(m, r, 8, 8, (fmt >> 7) & 1, size)
  setFmt(m, r, 7, 8, (fmt >> 8) & 1, size)
  for (let i = 9; i < 15; i++) setFmt(m, r, 14 - i, 8, (fmt >> i) & 1, size)

  // 第二副本：
  //   bit 0..7  -> 行 8，列 size-1 .. size-8（横排，自右向左）
  //   bit 8..14 -> 列 8，行 size-7 .. size-1（竖排，自上向下）
  for (let i = 0; i < 8; i++) setFmt(m, r, size - 1 - i, 8, (fmt >> i) & 1, size)
  for (let i = 8; i < 15; i++) setFmt(m, r, 8, size - 15 + i, (fmt >> i) & 1, size)
  // 固定深色模块
  m[size - 8][8] = 1
  r[size - 8][8] = 1
}

function setFmt(m, r, x, y, v, size) {
  if (x < 0 || y < 0 || x >= size || y >= size) return
  m[y][x] = v
  r[y][x] = 1
}

function placeVersionInfo(m, r, version, size) {
  const info = VERSION_INFO[version - 7]
  // 版本信息为 18 位，摆放为 3x6 的两个矩形（右上角与左下角）。
  // 坐标：行 = i%3 + size-11，列 = floor(i/3)；
  //      另一副本交换行列：行 = floor(i/3)，列 = i%3 + size-11。
  for (let i = 0; i < 18; i++) {
    const bit = (info >> i) & 1
    const a = Math.floor(i / 3)
    const b = (i % 3) + size - 11
    m[a][b] = bit
    r[a][b] = 1
    m[b][a] = bit
    r[b][a] = 1
  }
}

function placeData(m, r, codewords, size) {
  let bitIndex = 0 // 已写入的位计数
  let upward = true
  // 数据从右下角开始，按两列一组蛇形填充；每列内部按方向遍历行。
  // 第 6 列是竖向时序图案：列号一旦 <= 6 就左移一列，跳过该列。
  // 关键：必须把左移结果写回循环变量 col，使下一次迭代的 -=2 从新值继续，
  // 从而自然收敛到 [2,1] 这一对（与标准实现一致）。
  for (let col = size - 1; col > 0; col -= 2) {
    if (col <= 6) col -= 1
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
  // 掩码以 (行 i, 列 j) 为参数；标准 8 个掩码条件如下。
  const fn = [
    (i, j) => (i + j) % 2 === 0,
    (i, j) => i % 2 === 0,
    (i, j) => j % 3 === 0,
    (i, j) => (i + j) % 3 === 0,
    (i, j) => (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0,
    (i, j) => ((i * j) % 2) + ((i * j) % 3) === 0,
    (i, j) => (((i * j) % 2) + ((i * j) % 3)) % 2 === 0,
    (i, j) => (((i * j) % 3) + ((i + j) % 2)) % 2 === 0,
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
  // 规则 3：1:1:3:1:1 比率（两侧各有 4 个浅色模块）
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
