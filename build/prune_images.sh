#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────────
# 清理悬空镜像（dangling images）。
#
# 只删除无标签且未被任何容器引用的镜像（docker images 里的
# <none>:<none>），不会触碰任何有标签的镜像（包括其他项目的
# dp-agt-dsh、api-gate、rd-cli 等）。
#
# 各构建脚本默认在成功后调用本脚本；传 --no-prune 可跳过。
# ────────────────────────────────────────────────────────────────

if ! command -v docker &>/dev/null; then
  echo "docker not found, skip prune"
  exit 0
fi

echo ""
echo "=== 清理悬空镜像 (docker image prune -f) ==="
# 不加 -a：只清理悬空镜像，避免误删其他项目的已打标签镜像。
docker image prune -f
