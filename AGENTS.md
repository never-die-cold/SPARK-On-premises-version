# AGENTS.md — 仓库导览（供 AI 协作代理阅读）

SPARK On-premises：把个人闲置设备编排成一个 OpenAI 兼容的 LLM 集群。
姐妹仓库 [prairie-fire](https://github.com/never-die-cold/single-spark-can-start-a-prairie-fire) 是训练侧研究实现；本仓库是推理/部署侧工程实现。

## 布局

- `agent/` — Go（module 名 `sparkagent`，无域名前缀）。设备侧守护进程 `spark-agent`：
  - `discovery.go` UDP 广播（agent 监听 3970/udp 收 master beacon；向 3969/udp 广播 announce）
  - `inventory.go` 硬件清单（gopsutil）+ 默认路由源 IP 选择（VPN/多网卡友好）
  - `engine.go` llama.cpp 发布资产解析/下载/解压（**本地优先** + tag 落盘缓存；rpc-server 在新版本叫 `ggml-rpc-server`，用 `locateBin` 兼容两者）
  - `download.go` 模型下载：HTTP Range 断点续传 + 有限重试 + HF 镜像重写（`RewriteHFEndpoint`）
  - `worker.go` 进程监管：崩溃自动重启（上限 3 次、指数退避）；worker id：`rpc`（独占，端口 50052）、`server-<port>`（每部署一个，可多开）
  - `control.go` master→agent 的 HTTP 控制面（默认 3971/tcp）
- `spark/` — Python 包（FastAPI）
  - `master/registry.py` 设备注册表（TTL 15s；**TCP 心跳来源 IP 是粘性强信号**，用于修正 agent 广播地址在多网卡/VPN 下的漂移）
  - `master/planner.py` 纯函数放置规划器（single/split；系数见 `protocol.py`，未校准请勿当精确值）
  - `master/deployer.py` 部署编排线程：engine→rpc→download→server→health；清理按 `server-<port>`/`rpc` 停 worker
  - `master/app.py` OpenAI 兼容网关（/v1/* 流式代理到 driver）+ /api/* 集群管理 + 面板
  - `master/discovery.py` master 侧 UDP（3969/udp 收 announce，向 3970/udp 广播 beacon）
  - `master/dashboard/index.html` 无构建单文件面板（vanilla JS）
  - `cli.py` spark master/status/deploy/undeploy/models/bench/agent
  - `models.yaml` 模型注册表（只收**单文件** GGUF；新增条目先验证直链可 200）
- `deploy/` 各平台安装脚本（Windows 计划任务 / systemd 用户服务 / launchd / Termux / Docker）
- `benchmarks/` 基准方法与可复现结果（诚实声明原则：未实测的结论必须标注）
- `tests/` pytest；Go 测试与源码同目录（`*_test.go`）

## 约定

- 端口：8000 master HTTP、3970/3969 发现、3971 agent 控制、50052 rpc、8300+ llama-server
- v1 信任模型：可信局域网；`SPARK_TOKEN` 可选共享令牌
- 环境：`SPARK_MASTER`（agent 静态指定）、`SPARK_HF_ENDPOINT`（镜像，master 下发）、`SPARK_LLAMA_PATH`（本地引擎目录）、`SPARK_LLAMA_TAG`
- 文档中的性能声明只引用 `benchmarks/results/` 实测；网络相关结论注意标注（本机实测遇到：DNS 污染使 HF 直连卡死→镜像+续传解决）

## 构建/测试

- Go：`cd agent && go build ./... && go test ./...`
- Python：`pip install -e ".[dev]" && pytest`
- 端到端冒烟：master + agent 同机跑通 deploy→chat→undeploy（见 benchmarks/README.md）
