#!/usr/bin/env bash
# =============================================================
# Web 前端静态资源预压缩（.br / .gz）
#
# 用法: bash scripts/deploy/precompress-web.sh [dist 目录]
#   默认目录: web/dist
#
# 为什么需要:
#   前端主包压缩前约 870KB，gzip 后约 283KB、brotli 更小。服务端
#   （server/main.go serveSPA）会优先回传预压缩产物，省去每次请求的压缩开销，
#   更重要的是弱网/移动端首屏体积直接降到 1/3 —— 这是"移动端加载特别慢"
#   最直接的解法。
#
# 依赖: gzip（必装）；brotli 可选
#   Debian/Ubuntu: apt install brotli
#   RHEL 系:       dnf install brotli
#   缺失时只生成 .gz，不影响可用性（服务端按 Accept-Encoding 协商）。
#
# 幂等：每次重新生成，不会残留上一次的文件（先清掉自己的产物）。
# =============================================================
set -e

DIST="${1:-web/dist}"
if [ ! -d "${DIST}" ]; then
  echo "未找到前端产物目录: ${DIST}（请先执行 cd web && npm run build）" >&2
  exit 1
fi

# 先清掉旧产物：文件名带内容哈希，重新构建后旧文件不该留在 dist 里
find "${DIST}" -type f \( -name '*.gz' -o -name '*.br' \) -delete 2>/dev/null || true

# 只压缩文本类资源；图片/字体本身已压缩，再压只会白费 CPU 与磁盘
EXT_RE='\.(js|mjs|css|html|json|svg|txt|map|xml)$'

have_brotli=0
command -v brotli >/dev/null 2>&1 && have_brotli=1
[ "${have_brotli}" = 0 ] && echo "提示: 未安装 brotli，本次仅生成 .gz（安装 brotli 后可再跑一次生成 .br）"

count=0
br=0
while IFS= read -r -d '' f; do
  case "$f" in *.br|*.gz) continue ;; esac
  echo "$f" | grep -Eq "${EXT_RE}" || continue
  # -n：不写入原始文件名与时间戳，保证同内容产物字节一致（利于缓存/校验）
  gzip -9 -c -n "$f" > "$f.gz"
  if [ "${have_brotli}" = 1 ]; then
    brotli -q 11 -f -o "$f.br" "$f"
    br=$((br + 1))
  fi
  count=$((count + 1))
done < <(find "${DIST}" -type f -print0)

if [ "${count}" = 0 ]; then
  echo "⚠️  ${DIST} 下没有找到可压缩的资源（是不是还没构建？）" >&2
  exit 1
fi
echo "✅ 预压缩完成: ${DIST}（${count} 个 .gz，${br} 个 .br）"
