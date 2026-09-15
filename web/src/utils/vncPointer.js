// 远程桌面「虚拟鼠标」（移动端）指针控制器。
//
// 为什么单独抽成一个模块：这段逻辑全是「手势状态 + 指针位置 + 该发出什么鼠标事件」的
// 纯计算，与 React/DOM 无关。抽出来之后可以脱离浏览器直接跑断言
// （web/scripts/check-vnc-pointer.mjs），把几个易错点钉死：
//   · 轻点必须发出一次左键单击，拖动则只移动
//   · 手势被【取消】时绝不能点击（系统手势/来电打断时最容易误触）
//   · 面板关闭 / 断开 / 卸载时必须释放「按住的左键」，否则远端会一直是按下状态
//   · 指针不得越出画布
//
// 组件侧只负责：把 DOM 事件转发进来、把 emit 出来的坐标换算成 client 坐标派发给
// noVNC 的 canvas（由 noVNC 自己的 _handleMouse 完成到远端坐标的换算）。

// 轻点判定：手指按下到抬起之间允许的最大累计移动量（CSS px）。
// 若太小（例如 8），手指的正常抖动就会被判成拖动 —— 表现为「点了没反应」。
export const TAP_SLOP = 12

// noVNC 的 WHEEL_STEP = 50：一次滚轮事件送 ±50 恰好等于一格。
export const WHEEL_STEP_DELTA = 50

// 双击两次单击之间的间隔（毫秒）。远端的双击时限通常是 500ms，80ms 足够稳。
export const DOUBLE_CLICK_DELAY = 80

export function clamp(v, lo, hi) {
  return Math.max(lo, Math.min(hi, v))
}

// createVncPointer 创建一个指针控制器。
//   getRect()       -> {width, height} | null   画布当前可见尺寸（CSS px）
//   emit(evt)       -> void   evt: {type, x, y, buttons, deltaY?}，x/y 为画布内坐标
//   onHoldChange(b) -> void   「按住左键」状态变化时回调（供 UI 显示按钮文案）
//   now()           -> 毫秒时间戳（可注入，便于测试）
export function createVncPointer({ getRect, emit, onHoldChange, now = () => Date.now() } = {}) {
  let pos = { x: 0, y: 0 }
  let holding = false
  let gesture = null // { x, y, moved }

  const rectOf = () => {
    const r = getRect ? getRect() : null
    if (!r || !(r.width > 0) || !(r.height > 0)) return null
    return r
  }

  const emitMove = () => emit({ type: 'mousemove', x: pos.x, y: pos.y, buttons: holding ? 1 : 0 })

  const setHolding = (v) => {
    if (holding === v) return
    holding = v
    if (onHoldChange) onHoldChange(v)
  }

  const clampInto = (x, y, r) => ({
    x: clamp(x, 0, r.width - 1),
    y: clamp(y, 0, r.height - 1),
  })

  const api = {
    isHolding: () => holding,
    position: () => ({ ...pos }),

    // 把指针放到画布中心并发出一次移动：远端指针的真实位置客户端无从得知，
    // 先建立共同基准，之后的相对移动才不会出现「一拖就跳」。
    syncToCenter() {
      const r = rectOf()
      if (!r) return false
      pos = clampInto(r.width / 2, r.height / 2, r)
      emitMove()
      return true
    },

    // 相对移动（触控板拖动）。越界时钳制在画布内。
    moveBy(dx, dy) {
      if (dx === 0 && dy === 0) return
      const r = rectOf()
      if (!r) return
      pos = clampInto(pos.x + dx, pos.y + dy, r)
      emitMove()
    },

    // 在当前位置单击。button: 1=左键, 2=右键。
    click(button = 1) {
      emit({ type: 'mousedown', x: pos.x, y: pos.y, buttons: button })
      emit({ type: 'mouseup', x: pos.x, y: pos.y, buttons: 0 })
    },

    doubleClick() {
      api.click(1)
      setTimeout(() => api.click(1), DOUBLE_CLICK_DELAY)
    },

    // 滚轮：±50 对应一格（见 WHEEL_STEP_DELTA）
    wheel(deltaY = WHEEL_STEP_DELTA) {
      emit({ type: 'wheel', x: pos.x, y: pos.y, buttons: 0, deltaY })
    },

    // 按住/松开左键：拖拽窗口、圈选文本等需要保持按下的操作。
    toggleHold() {
      if (holding) {
        api.releaseHold()
        return
      }
      emit({ type: 'mousedown', x: pos.x, y: pos.y, buttons: 1 })
      setHolding(true)
    },

    releaseHold() {
      if (!holding) return
      emit({ type: 'mouseup', x: pos.x, y: pos.y, buttons: 0 })
      setHolding(false)
    },

    // ---- 触控板手势 ----
    gestureStart(x, y) {
      gesture = { x, y, moved: 0, t: now() }
    },

    gestureMove(x, y) {
      if (!gesture) return
      const dx = x - gesture.x
      const dy = y - gesture.y
      if (dx === 0 && dy === 0) return
      gesture.x = x
      gesture.y = y
      gesture.moved += Math.abs(dx) + Math.abs(dy)
      api.moveBy(dx, dy)
    },

    // 正常抬手：累计移动量小于阈值才算「轻点」，发出一次左键单击。
    gestureEnd() {
      const g = gesture
      gesture = null
      if (g && g.moved < TAP_SLOP) api.click(1)
    },

    // 手势被取消（iOS 边缘返回、下拉通知、来电等）：
    // 只清理状态，绝不点击 —— 否则会在远端桌面上凭空多出一次左键点击。
    gestureCancel() {
      gesture = null
    },

    // 面板关闭 / 连接断开 / 组件卸载时调用：把手势与「按住左键」一并收尾，
    // 保证不把远端鼠标留在按下状态。
    detach() {
      gesture = null
      api.releaseHold()
    },
  }

  return api
}
