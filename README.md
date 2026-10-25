# SPARK · On-premises version

**星火·本地部署版** —— 把你家里/手头闲置的电子设备（旧电脑、Mac、树莓派、安卓手机）组成一个推理集群，联合运行一个 OpenAI 兼容的 LLM 服务。

> 姐妹项目：[single-spark-can-start-a-prairie-fire](https://github.com/never-die-cold/single-spark-can-start-a-prairie-fire)（星火燎原，训练侧研究项目）。
> 本仓库是 SPARK 的**推理与部署侧工程实现**：一个可以在个人闲置设备上联合部署 LLM 的编排系统，并提出一套"降本升效"的架构方案。

## 它解决什么问题

个人手头往往有多台性能各异的闲置设备：一台 16GB 的旧笔记本、一台 8GB 的 Mac mini、一块树莓派、一部旧手机。它们各自都跑不动太大的模型，但：

- **单机内存不够** —— 14B/32B 级别的量化模型装不进任何一台设备的内存；
- **算力被浪费** —— 每台设备只跑小模型时，其余设备在空转耗电；
- **部署不共通** —— Windows / Linux / macOS / Android 各有各的装法，维护成本高。

SPARK On-premises 用一个**编排层**解决这三件事：设备自动组网、模型自动放置（单机承载或多机切分）、统一 OpenAI 兼容 API + 统一的多平台部署方案。

## 架构一览

```
┌────────────────────────── 局域网 ──────────────────────────────┐
│                                                               │
│  用户/应用 ── OpenAI 兼容 API ──▶ spark-master (Python)        │
│                                    ├── 集群注册表 / 心跳         │
│                                    ├── 放置规划器（单机/切分）    │
│                                    ├── 请求路由 / 故障降级       │
│                                    └── Web 面板（轻量）          │
│            UDP 广播自动发现 + HTTP 控制通道                      │
│   ┌──────────┬──────────┬──────────┬──────────┬──────────┐    │
│   │spark-agent│spark-agent│spark-agent│spark-agent│spark-agent│   │
│   │ Windows  │  Linux   │  macOS   │ 树莓派    │ Android   │   │
│   │ (Go 单文件二进制，监管 llama.cpp worker)      │ (Termux)  │   │
│   └──────────┴──────────┴──────────┴──────────┴──────────┘    │
└───────────────────────────────────────────────────────────────┘
```

**"降本升效"四机制**（详见 [docs/architecture.md](docs/architecture.md)）：

1. **分级放置** —— 小模型部署到最优单节点（零网络开销）；大模型按内存比例切分到多节点（llama.cpp RPC）；多模型并行承载、按请求路由。
2. **闲置设备生命周期管理** —— 心跳监测 + Wake-on-LAN 按需唤醒 + 空闲休眠策略（省电即降本）。
3. **异构感知** —— 硬件清单（内存/CPU/GPU/磁盘）驱动放置决策，不同设备选择不同推理后端。
4. **故障自愈** —— 节点掉线自动降级/重规划，网关返回明确的重试语义。

## 快速开始

需要：同一局域网内的 2 台以上设备（1 台也能跑单机模式），其中一台作为 master。

**1) master 节点（任何一台电脑，Python 3.10+）：**

```bash
pip install -e .[master]
spark master            # 启动集群管理器，Web 面板 http://localhost:8000/
```

**2) 每台设备（含 master 本机）安装 agent（Go 单文件二进制，零依赖）：**

- Windows: `deploy\windows\install.ps1`
- Linux/树莓派: `curl -fsSL deploy/linux/install.sh | bash`
- macOS: `bash deploy/macos/install.sh`
- Android (Termux): `bash deploy/android-termux/bootstrap.sh`

或从 [Releases](../../releases) 下载对应平台的 `spark-agent` 直接运行：

```bash
spark-agent --name my-old-laptop
```

**3) 部署一个模型：**

```bash
spark status                       # 查看已组网的设备
spark deploy qwen2.5-7b-instruct   # 自动决策：单机 or 多机切分
curl http://localhost:8000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct","messages":[{"role":"user","content":"你好，星火"}]}'
```

## 仓库结构

```
agent/        Go 源码：spark-agent（发现/清单/心跳/worker 监管）
spark/        Python 包：master（注册表/规划器/网关/面板）+ CLI
deploy/       各平台部署脚本（Windows/Linux/macOS/Termux/Docker）
benchmarks/   基准脚本与可复现结果（results/*.json）
docs/         架构提案、方案对比、平台指南、路线图
tests/        pytest 单元测试（Go 侧测试在 agent/ 内）
```

## 性能与诚实声明

本仓库文档中的所有性能结论只引用 `benchmarks/results/` 下可复现脚本产出的实测数据；未实测的部分明确标注"未实测"。多机切分在 Wi-Fi 环境下的吞吐受带宽制约，预期参见 [docs/architecture.md](docs/architecture.md#性能模型) 的定性分析——我们的主张是**用编排换总拥有成本**，不是"分布式一定更快"。

## Roadmap

M0 骨架与文档 → **M1 双节点 demo（进行中）** → M2 故障重分片/WoL/鉴权 → M3 全平台发布流水线 → M4 基准报告与面板完善。详见 [docs/roadmap.md](docs/roadmap.md)。

## License

MIT
