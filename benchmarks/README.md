# 基准测试方法与结果

## 工具

```bash
spark bench --model <name> [--rounds N] [--prompt "..."]
```

- 请求路径与真实用户完全一致：客户端 → master 网关(8000) → driver 节点 llama-server
- 流式（`stream: true`），逐 SSE chunk 计数
- 指标：
  - **TTFT**（Time To First Token）：请求发出到第一个内容 chunk
  - **decode tokens/s**：内容 token 数 / (总耗时 − TTFT)
- 每轮结果写入 `results/bench-<时间戳>-r<轮次>.json`（含 prompt、master 地址，可复现对账）

## 已有结果（诚实声明）

| 日期 | 模型 | 模式 | 引擎 | 节点 | TTFT | decode | 环境 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-10-26 | qwen2.5-0.5b-instruct q4_k_m | single | cpu | 1× Windows 笔记本（i7-13650HX 20核, RTX 5060） | 21-88 ms | 74-75 tok/s | master/agent 同机 loopback |
| 2026-10-26 | qwen2.5-0.5b-instruct q4_k_m | single | **cuda**（`-ngl 99`） | 同上 | 28-58 ms | **326-345 tok/s** | 同上；GPU 显存占用 ≈3.0GB，采样到 74% 利用率 |

**GPU vs CPU ≈ 4.5×（解码吞吐）**，同机同模型对照；引擎由 agent 自动选择（检测到 NVIDIA GPU → 下载 CUDA 13.4 构建 + cudart 运行时），
master 对有 GPU 的驱动节点自动附加 `-ngl 99`（`SPARK_NGL` 可覆盖）。

- **切分模式（split）尚无实测数据**。多节点 TTFT/吞吐取决于节点间带宽，
  待第二台设备入网后补充 2 节点 × qwen3-8b 的矩阵（有线/Wi-Fi 各一组）。
- 单机数据仅供量级参考：不同 CPU/温度/电源策略差异很大。

## 复现

```bash
# 1. 启动 master 与至少一台设备 agent
# 2. 部署被测模型
spark deploy qwen2.5-0.5b-instruct
# 3. 跑基准
spark bench --model qwen2.5-0.5b-instruct --rounds 5
```
