#!/data/data/com.termux/files/usr/bin/bash
# SPARK agent bootstrap for Android (Termux).
# 用法: 在 Termux 中执行  bash bootstrap.sh
#
# 说明:
#  - llama.cpp 引擎: agent 会优先下载官方 android-arm64 预构建包；
#    若不可用，本脚本用源码构建（见下方 build_llama 段）。
#  - agent 本体: 优先 Releases 的 android-arm64 二进制，兜底源码构建。
#  - 建议: termux-wake-lock 保持运行; 安装 Termux:Boot 实现开机自启。
set -euo pipefail

REPO="never-die-cold/SPARK-On-premises-version"
BASE_URL="https://github.com/${REPO}/releases/latest/download"
PREFIX_BIN="$PREFIX/bin"

echo "==> 安装基础依赖"
pkg update -y
pkg install -y golang git wget

echo "==> 获取 spark-agent"
if curl -fsSL -o "$PREFIX_BIN/spark-agent" "${BASE_URL}/spark-agent-android-arm64" 2>/dev/null; then
  chmod +x "$PREFIX_BIN/spark-agent"
  echo "==> 已安装 $PREFIX_BIN/spark-agent"
else
  echo "==> Releases 中暂无 android 二进制，从源码构建（约几分钟）…"
  SRC="${TMPDIR}/spark-src"
  rm -rf "$SRC"
  git clone --depth 1 "https://github.com/${REPO}.git" "$SRC"
  (cd "$SRC/agent" && CGO_ENABLED=0 go build -o "$PREFIX_BIN/spark-agent" .)
  echo "==> 已构建 $PREFIX_BIN/spark-agent"
fi

echo "==> 预热 llama.cpp 引擎（预构建包，约 30-80MB）"
mkdir -p ~/.spark-agent/engines
if ! curl -fsSL -o /tmp/llama-android.tar.gz \
     "https://github.com/ggml-org/llama.cpp/releases/download/b11878/llama-b11878-bin-android-arm64.tar.gz"; then
  echo "   预构建包下载失败；改用源码构建（需要 cmake clang，耗时较长）…"
  pkg install -y cmake clang
  git clone --depth 1 https://github.com/ggml-org/llama.cpp.git /tmp/llama.cpp
  cmake -S /tmp/llama.cpp -B /tmp/llama.cpp/build \
        -DCMAKE_BUILD_TYPE=Release -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
        -DLLAMA_BUILD_TOOLS=ON
  cmake --build /tmp/llama.cpp/build --target llama-server rpc-server -j"$(nproc)"
  mkdir -p ~/.spark-agent/engines/custom
  cp /tmp/llama.cpp/build/bin/llama-server /tmp/llama.cpp/build/bin/rpc-server \
     ~/.spark-agent/engines/custom/ 2>/dev/null || \
  cp /tmp/llama.cpp/build/llama-server /tmp/llama.cpp/build/rpc-server \
     ~/.spark-agent/engines/custom/ 2>/dev/null || true
  export SPARK_LLAMA_PATH="$HOME/.spark-agent/engines/custom"
  echo "   源码构建完成: $SPARK_LLAMA_PATH"
fi

echo "==> 保持唤醒（可选但推荐）"
command -v termux-wake-lock >/dev/null 2>&1 && termux-wake-lock || true

echo ""
echo "完成！启动 agent:"
echo "  spark-agent --name 我的旧手机"
echo "提示:"
echo "  - 手机设置中关闭 Termux 的电池优化，否则后台会被杀"
echo "  - 开机自启: pkg install termux-boot 后把启动命令放入 ~/.termux/boot/"
echo "  - master 若无法通过广播发现本机，用静态指定:"
echo "      spark-agent --master 192.168.x.x:8000"
