#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────────
# Build the production Docker image for the MCP server.
#   ./build/build_mcp.sh
#
# Uses BuildKit multi-stage build: the MCP binary is compiled and
# packaged into a slim production image in one buildx invocation.
# ────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(cat "$SCRIPT_DIR/VERSION")"

# ── Usage ───────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
用法: ./build/build_mcp.sh [参数...]

参数说明:
  -x | verbose            输出 go build 的详细编译日志（--progress=plain）
  -h | --help             显示本帮助并退出

代理参数（可选，不带 = 时继承环境变量）:
  http_proxy=<url>        设置 HTTP 代理
  https_proxy=<url>       设置 HTTPS 代理
  no_proxy=<url>          设置不走代理的地址列表

示例:
  ./build/build_mcp.sh http_proxy=http://127.0.0.1:7890 https_proxy=http://127.0.0.1:7890

说明:
  直接构建生产 Docker 镜像 doc-util-mcp:$VERSION（MCP 始终集成 OCR）。
EOF
}

cd "$SCRIPT_DIR"

if [[ ! -f "go.mod" ]]; then
  echo "ERROR: run this script from the docutil directory"
  exit 1
fi

if ! command -v docker &>/dev/null; then
  echo "ERROR: docker not found"
  exit 1
fi

# ── 参数解析 ────────────────────────────────────────────────────
PROGRESS=""
HTTP_PROXY_VAL=""
HTTPS_PROXY_VAL=""
NO_PROXY_VAL=""

for arg in "$@"; do
  case "$arg" in
    http_proxy=*|HTTP_PROXY=*)   HTTP_PROXY_VAL="${arg#*=}" ;;
    https_proxy=*|HTTPS_PROXY=*) HTTPS_PROXY_VAL="${arg#*=}" ;;
    no_proxy=*|NO_PROXY=*)       NO_PROXY_VAL="${arg#*=}" ;;
    -x|verbose)                  PROGRESS="--progress=plain" ;;
    -h|--help|help)              usage; exit 0 ;;
    *)                           echo "WARNING: 未知参数 '$arg'，已忽略（用 -h 查看用法）" ;;
  esac
done

# 不带 = 时继承环境变量
HTTP_PROXY_VAL="${HTTP_PROXY_VAL:-${HTTP_PROXY:-${http_proxy:-}}}"
HTTPS_PROXY_VAL="${HTTPS_PROXY_VAL:-${HTTPS_PROXY:-${https_proxy:-}}}"
NO_PROXY_VAL="${NO_PROXY_VAL:-${NO_PROXY:-${no_proxy:-}}}"

add_scheme() { local v="$1"; [[ -z "$v" || "$v" == *"://"* ]] && { printf '%s' "$v"; return; }; printf 'http://%s' "$v"; }
HTTP_PROXY_VAL="$(add_scheme "$HTTP_PROXY_VAL")"
HTTPS_PROXY_VAL="$(add_scheme "$HTTPS_PROXY_VAL")"

if [[ -n "$HTTP_PROXY_VAL" || -n "$HTTPS_PROXY_VAL" ]]; then
  export HTTP_PROXY="$HTTP_PROXY_VAL" HTTPS_PROXY="$HTTPS_PROXY_VAL"
  export http_proxy="$HTTP_PROXY_VAL" https_proxy="$HTTPS_PROXY_VAL"
  export NO_PROXY="$NO_PROXY_VAL"     no_proxy="$NO_PROXY_VAL"
  echo "代理: http=$HTTP_PROXY_VAL https=$HTTPS_PROXY_VAL no_proxy=$NO_PROXY_VAL"
fi

# ── Docker build args ───────────────────────────────────────────
BUILD_ARGS=()
if [[ -n "$HTTP_PROXY_VAL" || -n "$HTTPS_PROXY_VAL" ]]; then
  BUILD_ARGS+=(--build-arg "HTTP_PROXY=$HTTP_PROXY_VAL" --build-arg "HTTPS_PROXY=$HTTPS_PROXY_VAL" --build-arg "NO_PROXY=$NO_PROXY_VAL")
fi

if [[ -t 1 ]]; then
  usage
fi

# ── Go SDK + ONNX Runtime (download if missing) ─────────────────
GO_VERSION="1.24.13"
export GO_VERSION
"$SCRIPT_DIR/build/ensure_go_sdk.sh"
"$SCRIPT_DIR/build/ensure_onnxruntime.sh" \
  ${HTTP_PROXY_VAL:+"http_proxy=$HTTP_PROXY_VAL"} \
  ${HTTPS_PROXY_VAL:+"https_proxy=$HTTPS_PROXY_VAL"} \
  ${NO_PROXY_VAL:+"no_proxy=$NO_PROXY_VAL"}

# ── Build image must exist ──────────────────────────────────────
BUILD_IMAGE="docutil_build:latest"
if ! docker image inspect "$BUILD_IMAGE" &>/dev/null; then
  echo "Building image $BUILD_IMAGE ..."
  docker buildx build \
    --load \
    "${BUILD_ARGS[@]}" \
    --build-arg "GO_VERSION=$GO_VERSION" \
    -t "$BUILD_IMAGE" \
    -f build/Dockerfile \
    .
  echo "Done."
fi

# ── Build production image ──────────────────────────────────────
echo ""
echo "=== Building doc-util-mcp:$VERSION (multi-stage, BuildKit) ==="
docker buildx build \
  --load \
  "${BUILD_ARGS[@]}" \
  ${PROGRESS:+"$PROGRESS"} \
  -t "doc-util-mcp:$VERSION" \
  -f build/Dockerfile.mcp \
  .

echo ""
echo "=== Done ==="
echo "  Docker image: doc-util-mcp:$VERSION"
echo ""
echo "Run with:"
echo "  docker run -i --rm doc-util-mcp:$VERSION"
echo ""
echo "Or register directly with an MCP client:"
echo "  {"
echo "    \"command\": \"/path/to/doc-util-mcp\""
echo "  }"