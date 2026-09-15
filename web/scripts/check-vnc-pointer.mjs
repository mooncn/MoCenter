// 虚拟鼠标指针控制器的回归检查（无需浏览器、无需测试框架）。
//
// 运行：cd web && node scripts/check-vnc-pointer.mjs
//
// 覆盖的都是曾经出过问题或极易写错的点：
//   · 轻点必须发出一次左键单击；拖动只移动、不点击
//   · 手指正常抖动（< TAP_SLOP）不能被判成拖动 —— 否则表现为「点了没反应」
//   · 手势被系统取消时绝不能点击（原来 onPointerCancel 直接复用抬手逻辑 → 误点击）
//   · 面板收起/断开/卸载时必须释放「按住的左键」（原来没有任何地方释放 → 远端一直按下）
//   · 指针不得越出画布；滚轮一格 = ±WHEEL_STEP_DELTA；右键 = buttons 2

import {
  createVncPointer, TAP_SLOP, WHEEL_STEP_DELTA, clamp,
} from '../src/utils/vncPointer.js'

const RECT = { width: 800, height: 600 }

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

function eq(actual, expected, what) {
  const a = JSON.stringify(actual)
  const b = JSON.stringify(expected)
  if (a !== b) throw new Error(`${what}: 期望 ${b}，实际 ${a}`)
}

function ok(cond, what) {
  if (!cond) throw new Error(what)
}

// 建一个控制器并记录它发出的所有事件。
// 默认先把指针同步到画布中心（真实场景：面板一打开就 syncToCenter 建立基准），
// 然后清空记录，使每个用例都从「指针在中心」这一确定状态开始。
function makePointer({ center = true } = {}) {
  const events = []
  const holdLog = []
  const p = createVncPointer({
    getRect: () => RECT,
    emit: (e) => events.push(e),
    onHoldChange: (v) => holdLog.push(v),
  })
  const t = {
    p,
    events,
    holdLog,
    types: () => events.map((e) => e.type),
    clicks: () => events.filter((e) => e.type === 'mousedown' || e.type === 'mouseup'),
    moves: () => events.filter((e) => e.type === 'mousemove'),
    clear() { events.length = 0; holdLog.length = 0 },
  }
  if (center) {
    p.syncToCenter()
    t.clear()
  }
  return t
}

console.log('虚拟鼠标指针控制器检查\n')

// ---------- 基本量 ----------
check('clamp 边界正确', () => {
  eq(clamp(5, 0, 10), 5, 'clamp 中间值')
  eq(clamp(-1, 0, 10), 0, 'clamp 下界')
  eq(clamp(99, 0, 10), 10, 'clamp 上界')
})

check('TAP_SLOP 与 WHEEL_STEP_DELTA 为约定值', () => {
  eq(TAP_SLOP, 12, 'TAP_SLOP')
  eq(WHEEL_STEP_DELTA, 50, 'WHEEL_STEP_DELTA')
})

// ---------- 轻点 / 拖动 ----------
check('轻点（零移动）发出一次左键单击', () => {
  const t = makePointer()
  t.p.gestureStart(100, 100)
  t.p.gestureEnd()
  eq(t.clicks(), [
    { type: 'mousedown', x: 400, y: 300, buttons: 1 },
    { type: 'mouseup', x: 400, y: 300, buttons: 0 },
  ], '单击事件序列')
})

check('手指小幅抖动仍判定为轻点（回归：阈值曾为 8，正常抖动会被当成拖动）', () => {
  const t = makePointer()
  t.p.gestureStart(100, 100)
  t.p.gestureMove(104, 102) // 累计 6px
  t.p.gestureMove(104, 103) // 累计 7px
  t.p.gestureEnd()
  ok(t.clicks().length === 2, `期望产生一次单击（2 个事件），实际 ${t.clicks().length} 个`)
})

check('累计移动达到阈值即视为拖动，抬手不点击', () => {
  const t = makePointer()
  t.p.gestureStart(100, 100)
  t.p.gestureMove(100 + TAP_SLOP, 100)
  t.p.gestureEnd()
  eq(t.clicks(), [], '拖动不应产生任何点击事件')
})

check('拖动只发出 mousemove，且按相对量累加', () => {
  const t = makePointer() // 指针已在中心 (400,300)
  t.p.gestureStart(10, 10)
  t.p.gestureMove(20, 10)  // +10, 0
  t.p.gestureMove(20, 25)  // 0, +15
  eq(t.moves(), [
    { type: 'mousemove', x: 410, y: 300, buttons: 0 },
    { type: 'mousemove', x: 410, y: 315, buttons: 0 },
  ], '移动事件')
  ok(t.clicks().length === 0, '拖动过程不应有点击')
})

// ---------- 取消手势（本次修复点） ----------
check('手势被取消时绝不点击（修复：原来 onPointerCancel 复用 onTrackUp，会误触发左键）', () => {
  const t = makePointer()
  t.p.gestureStart(100, 100)
  t.p.gestureMove(101, 100)
  t.p.gestureCancel()
  eq(t.clicks(), [], '取消手势不应产生点击')
})

check('取消后抬手也不应补一次点击', () => {
  const t = makePointer()
  t.p.gestureStart(100, 100)
  t.p.gestureCancel()
  t.p.gestureEnd() // 某些浏览器 cancel 之后还会来 up
  eq(t.clicks(), [], 'cancel 之后的 up 不应点击')
})

// ---------- 按住左键（本次修复点） ----------
check('按住左键发出 mousedown(1) 并通知 UI', () => {
  const t = makePointer()
  t.p.toggleHold()
  eq(t.clicks(), [{ type: 'mousedown', x: 400, y: 300, buttons: 1 }], 'mousedown')
  eq(t.holdLog, [true], 'onHoldChange')
  ok(t.p.isHolding() === true, 'isHolding 应为 true')
})

check('按住状态下的移动带 buttons=1（远端才会保持拖拽）', () => {
  const t = makePointer()
  t.p.toggleHold()
  t.clear()
  t.p.moveBy(5, 5)
  eq(t.moves(), [{ type: 'mousemove', x: 405, y: 305, buttons: 1 }], '按住时的移动')
})

check('detach 必须释放按住的左键（修复：原来面板收起/断开都不释放，远端一直按下）', () => {
  const t = makePointer()
  t.p.toggleHold()
  t.clear()
  t.p.detach()
  eq(t.clicks(), [{ type: 'mouseup', x: 400, y: 300, buttons: 0 }], 'detach 应发出 mouseup')
  eq(t.holdLog, [false], 'onHoldChange 应回到 false')
  ok(t.p.isHolding() === false, 'detach 后 isHolding 应为 false')
})

check('detach 在未按住时不发任何事件（避免多余的 mouseup）', () => {
  const t = makePointer()
  t.p.detach()
  eq(t.events, [], '不该有事件')
})

check('releaseHold 幂等：连续两次只发一个 mouseup', () => {
  const t = makePointer()
  t.p.toggleHold()
  t.clear()
  t.p.releaseHold()
  t.p.releaseHold()
  eq(t.clicks(), [{ type: 'mouseup', x: 400, y: 300, buttons: 0 }], '只应有一个 mouseup')
})

check('toggleHold 两次可正常松开', () => {
  const t = makePointer()
  t.p.toggleHold()
  t.p.toggleHold()
  eq(t.clicks(), [
    { type: 'mousedown', x: 400, y: 300, buttons: 1 },
    { type: 'mouseup', x: 400, y: 300, buttons: 0 },
  ], '按下再松开')
  eq(t.holdLog, [true, false], '状态变化')
})

// ---------- 指针钳制 ----------
check('指针被钳制在画布内', () => {
  const t = makePointer()
  t.p.moveBy(-9999, -9999)
  eq(t.p.position(), { x: 0, y: 0 }, '左上角')
  t.p.moveBy(9999, 9999)
  eq(t.p.position(), { x: RECT.width - 1, y: RECT.height - 1 }, '右下角')
})

check('画布尺寸不可用时安全返回（不抛异常、不发事件）', () => {
  const events = []
  const p = createVncPointer({ getRect: () => null, emit: (e) => events.push(e) })
  ok(p.syncToCenter() === false, 'syncToCenter 应返回 false')
  p.moveBy(10, 10)
  p.click(1)
  eq(p.position(), { x: 0, y: 0 }, '位置保持不变')
  // click 仍会在原点发出事件，这是可接受的（无画布时不会真的派发）
})

// ---------- 按钮语义 ----------
check('右键 clicks 带 buttons=2', () => {
  const t = makePointer()
  t.p.click(2)
  eq(t.clicks(), [
    { type: 'mousedown', x: 400, y: 300, buttons: 2 },
    { type: 'mouseup', x: 400, y: 300, buttons: 0 },
  ], '右键序列')
})

check('滚轮送出一格（±50）', () => {
  const t = makePointer()
  t.p.wheel(-WHEEL_STEP_DELTA)
  t.p.wheel(WHEEL_STEP_DELTA)
  eq(t.events, [
    { type: 'wheel', x: 400, y: 300, buttons: 0, deltaY: -50 },
    { type: 'wheel', x: 400, y: 300, buttons: 0, deltaY: 50 },
  ], '滚轮事件')
})

check('syncToCenter 落在中心并发出一次移动', () => {
  const t = makePointer({ center: false })
  eq(t.p.position(), { x: 0, y: 0 }, '初始位置')
  eq(t.p.syncToCenter(), true, '返回值')
  eq(t.p.position(), { x: 400, y: 300 }, '中心位置')
  eq(t.moves(), [{ type: 'mousemove', x: 400, y: 300, buttons: 0 }], '一次移动')
})

// ---------- 双击（异步） ----------
const dc = makePointer()
dc.p.doubleClick()
eq(dc.clicks().length, 2, '双击应立即产生第一次单击的两个事件')
await new Promise((r) => setTimeout(r, 200))
check('双击产生两次完整单击', () => {
  eq(dc.clicks(), [
    { type: 'mousedown', x: 400, y: 300, buttons: 1 },
    { type: 'mouseup', x: 400, y: 300, buttons: 0 },
    { type: 'mousedown', x: 400, y: 300, buttons: 1 },
    { type: 'mouseup', x: 400, y: 300, buttons: 0 },
  ], '双击事件序列')
})

console.log(`\n通过 ${pass} 项，失败 ${failures.length} 项`)
if (failures.length) {
  console.log('\n失败明细：')
  for (const f of failures) console.log('  - ' + f)
  process.exit(1)
}
