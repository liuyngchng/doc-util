#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────────
# Build the GUI client binaries (Linux + Windows) with BuildKit.
#   ./build/build_cli.sh                 # Linux GUI with OCR
#
# BuildKit manages Go build caches internally; no more host-side
# gocache/gomodcache directories needed.
#
# Linux binary is built with full OCR support (OpenCV + ONNX Runtime).
# Windows cross-compile uses -tags noocr (no OpenCV cross-link), but
# MuPDF text extraction is always available for text-mode DOCX output.
#
# Windows native OCR build must be done on Windows:
#   build/build.bat    (requires MSYS2 UCRT64)
# ────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_IMAGE="doc-util-build:latest"

# ── Usage ───────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
用法: ./build/build_cli.sh [参数...]

参数说明:
  --no-obfuscate          关闭 Windows 构建的 garble 代码混淆（默认开启）
  -x | verbose            输出 go build 的详细编译日志（--progress=plain）
  -h | --help             显示本帮助并退出

代理参数（可选，均不带 = 时也会继承环境变量）:
  http_proxy=<url>        设置 HTTP 代理
  https_proxy=<url>       设置 HTTPS 代理
  no_proxy=<url>          设置不走代理的地址列表

示例:
  ./build/build_cli.sh                  # 默认构建（Linux OCR + Windows 混淆）
  ./build/build_cli.sh --no-obfuscate   # 不混淆 Windows 构建

说明:
  Linux 构建始终集成 OCR（OpenCV + ONNX Runtime），产出含 .so + models 的完整包。
  Windows 交叉编译始终使用 -tags noocr（不支持 Windows OCR 交叉编译）。
  Windows OCR 版本需在 Windows 上运行 build/build.bat 编译（需要 MSYS2 UCRT64）。
EOF
}

# ── Parse args ──────────────────────────────────────────────────
HTTP_PROXY_VAL=""
HTTPS_PROXY_VAL=""
NO_PROXY_VAL=""
PROGRESS=""
OBFUSCATE=true
for arg in "$@"; do
  case "$arg" in
    http_proxy=*|HTTP_PROXY=*)   HTTP_PROXY_VAL="${arg#*=}" ;;
    https_proxy=*|HTTPS_PROXY=*) HTTPS_PROXY_VAL="${arg#*=}" ;;
    no_proxy=*|NO_PROXY=*)       NO_PROXY_VAL="${arg#*=}" ;;
    -x|verbose)                  PROGRESS="--progress=plain" ;;
    --no-obfuscate)              OBFUSCATE=false ;;
    -h|--help|help)              usage; exit 0 ;;
    *)                           echo "WARNING: 未知参数 '$arg'，已忽略（用 -h 查看用法）" ;;
  esac
done

if [[ -t 1 ]]; then
  usage
fi

# ── Print build params ──────────────────────────────────────────
echo ""
echo "────────────────────────────────────────────────────────"
echo "  构建参数:"
echo "    OBFUSCATE      = $OBFUSCATE    (仅 Windows 构建)"
echo "    PROGRESS       = ${PROGRESS:-(auto)}"
echo "    HTTP_PROXY     = ${HTTP_PROXY_VAL:-(未设置)}"
echo "    HTTPS_PROXY    = ${HTTPS_PROXY_VAL:-(未设置)}"
echo "────────────────────────────────────────────────────────"
echo ""

HTTP_PROXY_VAL="${HTTP_PROXY_VAL:-${HTTP_PROXY:-${http_proxy:-}}}"
HTTPS_PROXY_VAL="${HTTPS_PROXY_VAL:-${HTTPS_PROXY:-${https_proxy:-}}}"
NO_PROXY_VAL="${NO_PROXY_VAL:-${NO_PROXY:-${no_proxy:-}}}"

add_scheme() { local v="$1"; [[ -z "$v" || "$v" == *"://"* ]] && { printf '%s' "$v"; return; }; printf 'http://%s' "$v"; }
HTTP_PROXY_VAL="$(add_scheme "$HTTP_PROXY_VAL")"
HTTPS_PROXY_VAL="$(add_scheme "$HTTPS_PROXY_VAL")"

BUILD_ARGS=()
if [[ -n "$HTTP_PROXY_VAL" || -n "$HTTPS_PROXY_VAL" ]]; then
  export HTTP_PROXY="$HTTP_PROXY_VAL" HTTPS_PROXY="$HTTPS_PROXY_VAL"
  export http_proxy="$HTTP_PROXY_VAL" https_proxy="$HTTPS_PROXY_VAL"
  export NO_PROXY="$NO_PROXY_VAL"     no_proxy="$NO_PROXY_VAL"
  BUILD_ARGS+=(--build-arg "HTTP_PROXY=$HTTP_PROXY_VAL" --build-arg "HTTPS_PROXY=$HTTPS_PROXY_VAL" --build-arg "NO_PROXY=$NO_PROXY_VAL")
fi

cd "$SCRIPT_DIR"

if [[ ! -f "main.go" ]] || [[ ! -f "go.mod" ]]; then
  echo "ERROR: run this script from the docutil directory"
  exit 1
fi

if ! command -v docker &>/dev/null; then
  echo "ERROR: docker not found"
  exit 1
fi

# ── Go SDK + ONNX Runtime SDK (download if missing) ─────────────
GO_VERSION="1.24.13"
export GO_VERSION
"$SCRIPT_DIR/build/ensure_go_sdk.sh"
"$SCRIPT_DIR/build/ensure_onnxruntime.sh" \
  ${HTTP_PROXY_VAL:+"http_proxy=$HTTP_PROXY_VAL"} \
  ${HTTPS_PROXY_VAL:+"https_proxy=$HTTPS_PROXY_VAL"}

# ── Build Docker image if missing ───────────────────────────────
if ! docker image inspect "$BUILD_IMAGE" &>/dev/null; then
  echo "Building Docker image $BUILD_IMAGE ..."
  docker buildx build \
    --load \
    "${BUILD_ARGS[@]}" \
    --build-arg "GO_VERSION=$GO_VERSION" \
    -t "$BUILD_IMAGE" \
    -f build/Dockerfile \
    .
  echo "Docker image $BUILD_IMAGE built"
else
  echo "Docker image $BUILD_IMAGE ready"
fi

# ── Clean old output ────────────────────────────────────────────
rm -rf "$SCRIPT_DIR/dist/docutil" "$SCRIPT_DIR/dist/docutil.exe" \
       "$SCRIPT_DIR"/dist/libopencv_*.so.* "$SCRIPT_DIR"/dist/libtbb.so* \
       "$SCRIPT_DIR"/dist/libonnxruntime.so* "$SCRIPT_DIR/dist/models"

mkdir -p "$SCRIPT_DIR/dist"

# ── 1. Linux GUI (always OCR) ───────────────────────────────────
echo ""
echo "=== Building docutil (Linux GUI, OCR) ==="
docker buildx build \
  --target cli-linux \
  --output "type=local,dest=$SCRIPT_DIR/dist" \
  ${PROGRESS:+"$PROGRESS"} \
  -f build/Dockerfile.cli \
  .

echo "  dist/docutil      ($(du -h "$SCRIPT_DIR/dist/docutil" | cut -f1))"
echo "  bundled: dist/libonnxruntime.so*"
echo "  bundled: dist/libopencv_*.so.406"
echo "  bundled: dist/models/"

# ── 2. Windows GUI (cross-compile, no OCR) ──────────────────────
if $OBFUSCATE; then
  WIN_TARGET="cli-windows-obfuscated"
  WIN_LABEL=" (obfuscated)"
else
  WIN_TARGET="cli-windows"
  WIN_LABEL=""
fi

echo ""
echo "=== Building docutil.exe (Windows GUI)${WIN_LABEL} ==="
docker buildx build \
  --target "$WIN_TARGET" \
  --output "type=local,dest=$SCRIPT_DIR/dist" \
  ${PROGRESS:+"$PROGRESS"} \
  -f build/Dockerfile.cli \
  .

echo "  dist/docutil.exe  ($(du -h "$SCRIPT_DIR/dist/docutil.exe" | cut -f1))"

# ── Package into tar for distribution ───────────────────────────
echo ""
echo "--- Packaging ---"
ARCHIVE_LINUX="docutil-linux-amd64.tar"
ARCHIVE_WIN="docutil-windows-amd64.tar"

rm -f "$SCRIPT_DIR/dist/$ARCHIVE_LINUX" "$SCRIPT_DIR/dist/$ARCHIVE_WIN"

# Linux: bundle .so + models into a staged directory then tar.
STAGE_DIR="$SCRIPT_DIR/dist/docutil-linux-amd64"
rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR"
cp -a \
  "$SCRIPT_DIR/dist/docutil" \
  "$SCRIPT_DIR"/dist/libonnxruntime.so* \
  "$SCRIPT_DIR/dist/libopencv_imgproc.so.406" \
  "$SCRIPT_DIR/dist/libopencv_core.so.406" \
  "$SCRIPT_DIR/dist/libtbb.so.12" \
  "$SCRIPT_DIR/dist/models" \
  "$STAGE_DIR/"
tar -cf "$SCRIPT_DIR/dist/$ARCHIVE_LINUX" -C "$SCRIPT_DIR/dist" docutil-linux-amd64
rm -rf "$STAGE_DIR"
echo "  $ARCHIVE_LINUX  ($(du -h "$SCRIPT_DIR/dist/$ARCHIVE_LINUX" | cut -f1)) — Linux OCR 完整包"

rm -rf \
  "$SCRIPT_DIR/dist/docutil" \
  "$SCRIPT_DIR"/dist/libonnxruntime.so* \
  "$SCRIPT_DIR/dist/libopencv_imgproc.so.406" "$SCRIPT_DIR/dist/libopencv_core.so.406" \
  "$SCRIPT_DIR/dist/libtbb.so.12" \
  "$SCRIPT_DIR/dist/models"

# Windows: single exe.
cd "$SCRIPT_DIR/dist"
tar -cf "$ARCHIVE_WIN" --transform='s,^,docutil-windows-amd64/,' docutil.exe
cd "$SCRIPT_DIR"
echo "  $ARCHIVE_WIN  ($(du -h "$SCRIPT_DIR/dist/$ARCHIVE_WIN" | cut -f1))"
rm -f "$SCRIPT_DIR/dist/docutil.exe"

echo ""
echo "分发文件（做好了的压缩包）:"
echo "  dist/$ARCHIVE_LINUX"
echo "  dist/$ARCHIVE_WIN"
echo ""
echo "Linux OCR 包内容: docutil + .so 库 + models/ 模型目录"
echo "解包后进入 docutil-linux-amd64/ 目录，运行 ./docutil 即可使用。"
echo "Windows 包内容: docutil.exe（支持 MuPDF 文本提取，OCR 请用 build/build.bat 原生编译）"