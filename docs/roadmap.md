# Roadmap

## M0 — 骨架与提案（已完成）
- [x] 仓库结构、README、LICENSE、CI 骨架
- [x] docs/architecture.md 架构提案、docs/comparison.md 方案对比

## M1 — 双节点 demo（当前里程碑）
- [x] spark-agent（Go）：UDP 发现、硬件清单、心跳、llama.cpp 下载与 worker 监管
- [x] spark-master（Python）：注册表（TTL）、放置规划器（单机/切分）、OpenAI 兼容网关、spark CLI（status/deploy/bench）
- [x] 模型注册表 models.yaml + HF 镜像（HF_ENDPOINT）支持
- [x] Windows / Linux / macOS / Termux / Docker 部署脚本
- [x] CI（pytest + go test + 六平台交叉编译）
- [x] 单机端到端验证（发现→部署→网关对话→基准→卸载，见 benchmarks/README.md）
- [ ] 两台真实设备端到端实测（split 模式的 TTFT/吞吐矩阵）

## M2 — 生命周期与安全
- [x] 故障自动重分片（节点掉线 → 20s 宽限 → 剩余节点重规划，上限 2 次；节点回归 → 部署自动复活）
- [x] Wake-on-LAN 唤醒（spark wake）+ 休眠命令（spark sleep，有部署时拒绝）
- [x] 控制通道共享令牌（SPARK_TOKEN，master+agent 双侧校验）
- [x] 空闲自动休眠策略（`SPARK_IDLE_SLEEP_MIN>0` 启用；本地 master 主机与有部署/运行 worker 的设备永不休眠；默认关闭）
- [x] GPU 探测与 -ngl 自动设置（实测 0.5B CPU→GPU 4.5×；CUDA 13.4 自动下载 + cudart 运行时合并）
- [x] 节点间带宽探测进规划器（/netprobe 探测 + wired/wifi/slow/poor 分类；poor 链路拒绝 split，wifi/slow 警告）
- [x] 后端进程健康探测（master 每 10s 探 driver /health，连续 3 次失败 → degraded(backend_dead) → 自愈重部署；实测：耗尽 agent 重启配额后 30s 检测、自愈后对话恢复）

## M3 — 发布流水线
- [ ] GitHub Actions 多目标构建：windows/amd64、linux/amd64+arm64、darwin/amd64+arm64、android/arm64
- [ ] 各平台安装脚本对 Releases 产物的自动拉取与升级（spark-agent --upgrade）
- [ ] Docker 镜像发布（master 与 agent 两种）

## M4 — 面板与基准
- [ ] Web 面板完善：部署拓扑图、tokens/s 实时曲线
- [ ] 基准报告：单机 vs 2/4 节点 × 有线/Wi-Fi 的 TTFT 与吞吐矩阵
- [ ] 功耗侧量测方案（休眠策略的降本论据）

## M5+ — 与训练侧互通（愿景）
- [ ] 与 prairie-fire 的参数分发协议对接：推理集群兼做训练参数面
- [ ] KV cache 跨节点放置策略探索
