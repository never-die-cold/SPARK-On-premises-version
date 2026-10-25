#!/usr/bin/env bash
# SPARK agent installer for Linux (x86_64/amd64, arm64/树莓派) and macOS.
# Installs the binary and registers a system service (systemd / launchd).
set -euo pipefail

REPO="never-die-cold/SPARK-On-premises-version"
BASE_URL="https://github.com/${REPO}/releases/latest/download"
INSTALL_DIR="${SPARK_INSTALL_DIR:-$HOME/.local/bin}"
NAME="${SPARK_AGENT_NAME:-}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"   # linux | darwin
case "$(uname -m)" in
  x86_64)        arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "不支持的架构: $(uname -m)"; exit 1 ;;
esac
target="spark-agent-${os}-${arch}"

echo "==> 平台: ${os}/${arch}"

# 1) 获取二进制：优先 Releases，其次本地源码构建
if command -v spark-agent >/dev/null 2>&1; then
  echo "==> 已存在 spark-agent: $(command -v spark-agent)"
elif curl -fsSL -o /tmp/spark-agent "${BASE_URL}/${target}" 2>/dev/null; then
  mkdir -p "${INSTALL_DIR}"
  install -m 0755 /tmp/spark-agent "${INSTALL_DIR}/spark-agent"
  echo "==> 已安装 ${INSTALL_DIR}/spark-agent"
  case ":$PATH:" in *":${INSTALL_DIR}:"*) ;; *)
    echo "    提示: 把 ${INSTALL_DIR} 加入 PATH" ;;
  esac
else
  echo "==> Releases 中暂无 ${target}，尝试从源码构建（需要 go >= 1.23）…"
  if ! command -v go >/dev/null 2>&1; then
    echo "    没有 go。请安装 go 后重试，或手动下载: ${BASE_URL}/${target}"
    exit 1
  fi
  SRC="${SPARK_SRC_DIR:-$(mktemp -d)/src}"
  if [ ! -d "${SRC}/agent" ]; then
    git clone --depth 1 "https://github.com/${REPO}.git" "${SRC}"
  fi
  mkdir -p "${INSTALL_DIR}"
  (cd "${SRC}/agent" && CGO_ENABLED=0 go build -o "${INSTALL_DIR}/spark-agent" .)
  echo "==> 已构建 ${INSTALL_DIR}/spark-agent"
fi

# 2) 常驻服务
AGENT_BIN="$(command -v spark-agent || echo "${INSTALL_DIR}/spark-agent")"
if [ "${os}" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
  mkdir -p ~/.config/systemd/user
  unit=~/.config/systemd/user/spark-agent.service
  cat > "${unit}" <<EOF
[Unit]
Description=SPARK On-premises agent
After=network-online.target

[Service]
ExecStart=${AGENT_BIN} ${NAME:+--name ${NAME}}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now spark-agent.service
  echo "==> systemd 用户服务已启动: systemctl --user status spark-agent"
  echo "    （长期无人值守建议: loginctl enable-linger $USER）"
elif [ "${os}" = "darwin" ]; then
  plist=~/Library/LaunchAgents/com.spark.agent.plist
  mkdir -p ~/Library/LaunchAgents
  cat > "${plist}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.spark.agent</string>
  <key>ProgramArguments</key><array>
    <string>${AGENT_BIN}</string>
    ${NAME:+<string>--name</string><string>${NAME}</string>}
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "${plist}" >/dev/null 2>&1 || true
  launchctl load "${plist}"
  echo "==> launchd 服务已启动: launchctl list | grep spark"
else
  echo "==> 未检测到 systemd/launchd；手动运行: ${AGENT_BIN} ${NAME:+--name ${NAME}}"
fi

echo "==> 完成。日志: ~/.spark-agent/logs/；master 侧运行 spark status 查看设备"
