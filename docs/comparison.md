# 方案对比：为什么是"编排层 + llama.cpp"

调研时间：2026-10。活跃度以当时观察为准。

| 方案 | 切分方式 | 平台覆盖 | 节点数约束 | 优点 | 对我们场景的不足 |
| --- | --- | --- | --- | --- | --- |
| **SPARK On-premises（本方案）** | 编排层：单机放置 / llama.cpp RPC 切分 | Win/Linux/macOS/Android(Termux)/树莓派 | 无 | 异构感知放置、自动发现、OpenAI 兼容统一入口、共通部署方案 | 推理性能上限受制于 llama.cpp RPC |
| [llama.cpp RPC](https://github.com/ggml-org/llama.cpp/tree/master/examples/rpc) | 按各节点内存比例分布层与 KV | 全（llama.cpp 覆盖面） | 无 | 最通用的分布式推理基座 | 无编排：手工配 IP/端口/模型分发，无自愈，无统一 API 入口 |
| [exo](https://github.com/exo-explore/exo) | 拓扑感知 pipeline/tensor 并行（MLX） | macOS（GPU）/ Linux（CPU） | 无 | 设备发现、Mac 生态性能极佳（TB5 RDMA） | 无 Windows；MLX 绑定 Apple；Linux 仅 CPU |
| [Distributed Llama](https://github.com/b4rtaz/distributed-llama) | 张量并行（自研） | Linux/macOS/Windows（CPU 为主） | 必须 2^n，受 KV head 数限制 | CPU 张量并行、树莓派友好 | 节点数硬约束；量化仅 q40；模型覆盖窄；强依赖以太网 |
| Ollama / llama-server 单机 | 不切分 | 全 | 1 | 最省心 | 单机内存天花板，无法利用多设备 |
| Petals | 参数服务器 + 流水线 | Python 生态 | 无 | 可跑超大模型 | 维护趋于停滞、面向 Transformers 大显存节点、延迟高 |

**结论**：llama.cpp RPC 是"覆盖面 × 成熟度"最优的推理原语，但它是**手动挡**——本项目的价值就是把手动挡做成自动挡（发现/放置/自愈/共通部署），并把"何时该切分、何时不该"的决策显式化为可测试的规划器。
