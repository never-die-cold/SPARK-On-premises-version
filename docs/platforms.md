# 多平台部署指南

SPARK 的目标是一套方案覆盖所有个人闲置设备。各平台的差异被收敛到两处：
**安装 agent**（本文）与**网络发现方式**（文末）。master 侧在任何平台都一样：
`pip install -e ".[master]" && spark master`。

## 总览

| 平台 | agent 安装 | 常驻方式 | 引擎来源 |
| --- | --- | --- | --- |
| Windows x64 | `deploy\windows\install.ps1` | 计划任务（登录自启） | llama.cpp win-cpu-x64 预构建 |
| Linux x64/arm64（含树莓派） | `deploy/linux/install.sh` | systemd 用户服务 | ubuntu-x64 / ubuntu-arm64 预构建 |
| macOS x64/arm64 | `deploy/macos/install.sh` | launchd | macos 预构建（Metal CPU 构建） |
| Android（Termux） | `deploy/android-termux/bootstrap.sh` | termux-wake-lock + Termux:Boot | android-arm64 预构建或源码构建 |
| 任意（容器化） | `deploy/docker/docker-compose.yml` | docker restart 策略 | ubuntu 预构建 |

> **GPU 加速（自动）**：agent 启动时探测 NVIDIA GPU，自动下载 CUDA 引擎 + cudart 运行时；master 自动附加 `-ngl 99`。
> 实测 RTX 5060：0.5B 模型解码从 75 → 330+ tok/s（约 4.5×）。覆盖/强制选项：
> `SPARK_LLAMA_FLAVOR=cpu|cuda|vulkan`（Vulkan 面向 AMD/Intel 显卡）、`SPARK_NGL=<层数>`。
> 还需设备装有 NVIDIA 驱动；CUDA 构建体积较大（约 260MB + 运行时 200MB）。

## Windows

```powershell
powershell -ExecutionPolicy Bypass -File deploy\windows\install.ps1 -Name "我的旧电脑"
```

- 安装到 `%LOCALAPPDATA%\SPARK\spark-agent.exe`，注册当前用户登录自启的计划任务。
- 防火墙：脚本尝试添加规则（需管理员）；失败时 Windows 首次运行弹窗勾选"允许"即可。
  手动放行（管理员）：
  `netsh advfirewall firewall add rule name="SPARK agent" dir=in program="<exe路径>" action=allow`
- 数据目录 `%USERPROFILE%\.spark-agent\`（引擎/模型/日志）。

## Linux / 树莓派

```bash
curl -fsSL https://raw.githubusercontent.com/never-die-cold/SPARK-On-premises-version/main/deploy/linux/install.sh | bash
# 或克隆本仓库后: bash deploy/linux/install.sh
```

- systemd 用户服务 `spark-agent.service`；无人值守设备建议 `loginctl enable-linger $USER`。
- 树莓派：使用 `bin-ubuntu-arm64` 预构建（glibc 通用）；1GB 内存的 Pi 只能当 rpc 从节点。
- 防火墙：`sudo ufw allow from 192.168.1.0/24 to any port 3971 proto tcp`（按网段调整）。

## macOS

```bash
bash deploy/macos/install.sh
```

- launchd `com.spark.agent.plist`，安装到 `~/.local/bin/spark-agent`。
- 合盖睡眠的 Mac 不响应心跳——插电 + 外接屏幕或 `caffeinate` 保持运行。

## Android（Termux）

```bash
pkg install wget        # 确保 Termux 基础可用
bash bootstrap.sh
spark-agent --name 我的旧手机
```

注意事项（都和 Android 杀后台有关）：
1. 系统设置中把 Termux 加入电池优化白名单；
2. 保持 `termux-wake-lock`；
3. 开机自启：`pkg install termux-boot`，把 `spark-agent --master <master-ip>:8000` 写入 `~/.termux/boot/spark.sh`；
4. 手机发热属正常（llama.cpp 会吃满所有核心）。

## Docker

```bash
cd deploy/docker
docker compose up -d          # Linux: host 网络，开箱即用
```

Docker Desktop（Windows/macOS）注意：容器 NAT 会吞 UDP 广播，给 agent 加环境变量
`SPARK_MASTER=<宿主机IP>:8000` 走静态指定；master 容器改用 `ports` 映射而不是 host 网络。

## 网络发现的三种方式

| 方式 | 适用 | 说明 |
| --- | --- | --- |
| UDP 广播（默认） | 家庭局域网 | 零配置。要求路由器/AP 不隔离广播（多数家用环境满足） |
| 静态指定 | AP 隔离/Docker NAT/跨网段 | `spark-agent --master 192.168.1.10:8000` 或环境变量 `SPARK_MASTER` |
| 心跳直连 | 任意 | agent 只要能访问 master 的 8000 端口即可被注册 |

## 网络环境建议

- **切分模式对带宽敏感**：rpc 链路每 token 都有跨节点流量。千兆有线舒适，Wi-Fi 5/6 可用，
  百兆/隔离网络建议只用单机模式（M2 引入带宽探测后规划器才会自动规避）。
- 模型下载走 HuggingFace：国内网络**强烈建议**在 master 上设置
  `SPARK_HF_ENDPOINT=https://hf-mirror.com`（实测：被污染的 DNS 会让直连下载卡死，
  镜像 + 断点续传稳定完成；该地址随下载指令自动下发到 agent）。
- **VPN / Clash TUN 用户注意**：TUN 会成为默认路由（agent 按默认路由选广播地址可能选中 TUN）。
  若 master 看到的设备地址不可达，agent 侧加 `--advertise <真实局域网IP>`，
  或依赖 master 的心跳来源地址自动修正（TCP 观测优先，已内置）。
- 引擎下载走 GitHub Releases：如受限，可手动下载对应 tarball 解压后设置 `SPARK_LLAMA_PATH`。
- Windows 下杀掉 agent 后如端口仍被占用，检查是否有孤儿进程：`tasklist | findstr spark-agent`。
- **切分模式的带宽门控**：部署 split 前 master 会探测各节点带宽（`/netprobe`）。低于 30Mbps 的链路会拒绝切分（建议改有线）；80-300Mbps 标记为 Wi-Fi 并警告解码可能受限。阈值可在 `spark/master/netprobe.py` 调整。
- **空闲自动休眠**（可选）：master 设 `SPARK_IDLE_SLEEP_MIN=30` 后，无部署且无运行 worker 的设备空闲 30 分钟自动休眠（master 本机与有部署的设备永不触发）；`spark wake <device>` 唤醒。

## 故障排查

| 症状 | 排查 |
| --- | --- |
| master 看不到设备 | agent 日志 `~/.spark-agent/logs`；Windows 防火墙弹窗；AP 广播隔离 → 改静态指定 |
| 部署失败: engine ready 超时 | GitHub 不可达 → 手动装引擎 + `SPARK_LLAMA_PATH` |
| 部署失败: model download | HF 不可达 → master 设置 `SPARK_HF_ENDPOINT` |
| active 但请求 502 | driver 的 llama-server 挂了；查 agent worker 日志（状态里带路径） |
| degraded | 有节点心跳丢失；恢复设备网络后如未自动恢复，undeploy + 重新 deploy |
