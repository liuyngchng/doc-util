#!/usr/bin/env bash
# Launch a dev container for OCR development.
# Maps the project directory into /workspace.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$SCRIPT_DIR"
IMAGE="doc-util-build:latest"

# ── Usage ───────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
用法: ./build/dev.sh [代理参数...]

启动 OCR 开发容器（交互式 bash），挂载项目目录到 /workspace。

代理参数（可选，不带 = 时继承环境变量）:
  http_proxy=<url>        设置 HTTP 代理
  https_proxy=<url>       设置 HTTPS 代理
  no_proxy=<url>          设置不走代理的地址列表

示例:
  ./build/dev.sh
  ./build/dev.sh http_proxy=http://127.0.0.1:7890 https_proxy=http://127.0.0.1:7890
EOF
}

# ── 参数解析 ────────────────────────────────────────────────────
HTTP_PROXY_VAL=""
HTTPS_PROXY_VAL=""
NO_PROXY_VAL=""

for arg in "$@"; do
  case "$arg" in
    http_proxy=*|HTTP_PROXY=*)   HTTP_PROXY_VAL="${arg#*=}" ;;
    https_proxy=*|HTTPS_PROXY=*) HTTPS_PROXY_VAL="${arg#*=}" ;;
    no_proxy=*|NO_PROXY=*)       NO_PROXY_VAL="${arg#*=}" ;;
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

# ── Docker build args / run env ───────────────────────────────────
BUILD_ARGS=()
RUN_ENV=()
if [[ -n "$HTTP_PROXY_VAL" || -n "$HTTPS_PROXY_VAL" ]]; then
  BUILD_ARGS+=(--build-arg "HTTP_PROXY=$HTTP_PROXY_VAL" --build-arg "HTTPS_PROXY=$HTTPS_PROXY_VAL" --build-arg "NO_PROXY=$NO_PROXY_VAL")
  RUN_ENV=(
    -e "HTTP_PROXY=$HTTP_PROXY_VAL" -e "HTTPS_PROXY=$HTTPS_PROXY_VAL" -e "NO_PROXY=$NO_PROXY_VAL"
    -e "http_proxy=$HTTP_PROXY_VAL" -e "https_proxy=$HTTPS_PROXY_VAL" -e "no_proxy=$NO_PROXY_VAL"
  )
fi

if [[ -t 1 ]]; then
  usage
fi

# ── Go SDK + ONNX Runtime SDK (download if missing) ─────────────
GO_VERSION="1.24.13"
export GO_VERSION
"$SCRIPT_DIR/build/ensure_go_sdk.sh"
"$SCRIPT_DIR/build/ensure_onnxruntime.sh" \
  ${HTTP_PROXY_VAL:+"http_proxy=$HTTP_PROXY_VAL"} \
  ${HTTPS_PROXY_VAL:+"https_proxy=$HTTPS_PROXY_VAL"} \
  ${NO_PROXY_VAL:+"no_proxy=$NO_PROXY_VAL"}

# ── Persistent dev caches (host volumes → container; survives
#    docker run --rm, so incremental dev builds stay fast) ────────
GOCACHE_DIR="$SCRIPT_DIR/dist/cache/gocache_dev"
GOMODCACHE_DIR="$SCRIPT_DIR/dist/cache/gomodcache"
mkdir -p "$GOCACHE_DIR" "$GOMODCACHE_DIR"

# Build dev image if not present
if ! docker image inspect "$IMAGE" &>/dev/null; then
  echo "Building dev image $IMAGE ..."
  docker buildx build \
    --load \
    "${BUILD_ARGS[@]}" \
    --build-arg "GO_VERSION=$GO_VERSION" \
    -t "$IMAGE" \
    -f build/Dockerfile \
    .
  echo "Done."
fi

echo "Starting dev container. Use 'go build' or 'go test' inside."
echo "  OpenCV:       pkg-config --cflags --libs opencv4"
echo "  ONNX Runtime: build/deps/onnxruntime (cgo paths are in internal/ocr/onnx_api.go)"
echo ""

docker run --rm -it \
  -v "$SCRIPT_DIR":/workspace \
  -v "$GOCACHE_DIR":/tmp/gocache \
  -v "$GOMODCACHE_DIR":/go/pkg/mod \
  -w /workspace \
  "${RUN_ENV[@]+"${RUN_ENV[@]}"}" \
  -e GOCACHE=/tmp/gocache \
  -e GOMODCACHE=/go/pkg/mod \
  -e GOPROXY=https://goproxy.cn,direct \
  "$IMAGE" \
  bash