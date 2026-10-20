import React, { useEffect, useRef } from 'react'

// 通用模态框：深色控制台里原先所有"展开一行看细节"的做法都会把表格撑长，
// 弹窗能保持列表位置不动。Esc / 点遮罩关闭，打开期间锁背景滚动。
export default function Modal({ title, subtitle, onClose, children, footer, wide = false }) {
  const closeRef = useRef(onClose)
  closeRef.current = onClose

  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') closeRef.current?.() }
    document.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [])

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(e) => { if (e.target === e.currentTarget) closeRef.current?.() }}
    >
      <div className={`modal-card ${wide ? 'wide' : ''}`} role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head">
          <div style={{ minWidth: 0 }}>
            <h3>{title}</h3>
            {subtitle ? <div className="sub">{subtitle}</div> : null}
          </div>
          <button className="modal-close" onClick={() => closeRef.current?.()} aria-label="关闭">✕</button>
        </div>
        <div className="modal-body">{children}</div>
        {footer ? <div className="modal-foot">{footer}</div> : null}
      </div>
    </div>
  )
}
