#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────────
# Build the production Docker image for the headless HTTP server.
#   ./build/build.sh
#
# Uses BuildKit multi-stage build: the server binary is compiled and
# packaged into a slim production image in one buildx invocation.
# BuildKit manages Go caches internally (no host gocache needed).
# ────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(cat "$SCRIPT_DIR/VERSION")"

# ── Usage ───────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
用法: ./build/build.sh [参数...]

参数说明:
  -x | verbose            输出 go build 的详细编译日志（--progress=plain）
  -h | --help             显示本帮助并退出

代理参数（可选，不带 = 时继承环境变量）:
  http_proxy=<url>        设置 HTTP 代理
  https_proxy=<url>       设置 HTTPS 代理
  no_proxy=<url>          设置不走代理的地址列表

示例:
  ./build/build.sh http_proxy=http://127.0.0.1:7890 https_proxy=http://127.0.0.1:7890

说明:
  直接构建生产 Docker 镜像 doc-util:$VERSION（Server 始终集成 OCR）。
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

# Generate self-signed TLS certificate (idempotent) and stage into dist/.
"$SCRIPT_DIR/build/gen_cert.sh"
mkdir -p "$SCRIPT_DIR/dist"
cp -a "$SCRIPT_DIR/certs/cert.pem" "$SCRIPT_DIR/certs/key.pem" "$SCRIPT_DIR/dist/"

# ── Build production image ──────────────────────────────────────
echo ""
echo "=== Building doc-util:$VERSION (multi-stage, BuildKit) ==="
docker buildx build \
  --load \
  "${BUILD_ARGS[@]}" \
  ${PROGRESS:+"$PROGRESS"} \
  -t "doc-util:$VERSION" \
  -f build/Dockerfile.server \
  .

# Clean up staged certs from dist/ (keep certs/ as durable assets).
rm -f "$SCRIPT_DIR/dist/cert.pem" "$SCRIPT_DIR/dist/key.pem"

echo ""
echo "=== Done ==="
echo "  Docker image: doc-util:$VERSION"
echo ""
echo "Run with:"
echo "  docker run -d --rm --name doc-util -p 20002:8080 \\"
echo "    -v \$(pwd)/logs:/app/logs \\"
echo "    doc-util:$VERSION"
echo "  docker logs -f doc-util"
echo ""
echo "HTTPS enabled by default (self-signed cert for localhost)."
echo "  Health check: curl -k https://localhost:20002/health"
echo "  Logs on host:  cat logs/server.log"