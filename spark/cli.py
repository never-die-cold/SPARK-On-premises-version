"""spark CLI: master / status / deploy / undeploy / models / bench."""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import httpx

MASTER_URL = os.environ.get("SPARK_MASTER_URL", "http://127.0.0.1:8000")


def _client() -> httpx.Client:
    return httpx.Client(base_url=MASTER_URL, timeout=60.0)


def _check_master() -> None:
    try:
        with _client() as c:
            c.get("/api/summary")
    except httpx.HTTPError as exc:
        sys.exit(f"无法连接 master（{MASTER_URL}）：{exc}\n"
                 f"先在某台设备上运行: spark master   （或设置 SPARK_MASTER_URL）")


# ---------------- commands ----------------

def cmd_master(args: argparse.Namespace) -> None:
    import uvicorn

    from spark.master.app import create_app

    app = create_app(start_discovery=True, http_port=args.port)
    print(f"SPARK master -> http://{args.host}:{args.port}  （Web 面板同地址）")
    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


def cmd_status(args: argparse.Namespace) -> None:
    _check_master()
    with _client() as c:
        s = c.get("/api/summary").json()

    print(f"master: {MASTER_URL}")
    print(f"设备: {s['devices_online']}/{s['devices_total']} 在线   "
          f"活跃部署: {s['deployments_active']}")
    print()
    if not s["devices"]:
        print("  （还没有设备加入；在各设备上运行 spark-agent）")
    for d in s["devices"]:
        inv = d.get("inventory", {})
        mark = "●" if d["online"] else "○"
        mem = inv.get("mem_available", 0) / 1024**3
        tot = inv.get("mem_total", 0) / 1024**3
        gpu = ",".join(g.get("name", "?") for g in inv.get("gpus", [])) or "CPU"
        workers = ",".join(f"{w['id']}:{w['state']}" for w in d.get("workers", [])) or "-"
        print(f"  {mark} {d['name']:<18} {inv.get('os','?')}/{inv.get('arch','?'):<6} "
              f"{inv.get('cpu_cores','?')}核  {mem:5.1f}/{tot:5.1f}GB  [{gpu}]  worker:{workers}")

    print()
    if not s["deployments"]:
        print("  （没有部署；spark deploy <model>）")
    for dep in s["deployments"]:
        line = f"  {dep['model']:<24} {dep['status']:<12} {dep.get('mode','') or '-':<7}"
        if dep.get("driver_device_id"):
            line += f" driver={dep['driver_device_id']}"
        if dep.get("worker_device_ids"):
            line += f" rpc={','.join(dep['worker_device_ids'])}"
        if dep.get("error"):
            line += f"  ! {dep['error']}"
        print(line)


def cmd_models(args: argparse.Namespace) -> None:
    _check_master()
    with _client() as c:
        models = c.get("/api/models").json()
    print(f"{'模型':<26}{'体积':>7}  {'上下文':>6}  说明")
    for m in models:
        print(f"{m['name']:<26}{m['approx_gb']:>5.2f}GB  {m['ctx']:>6}  {m['desc']}")
    print("\n自定义模型：设置 SPARK_MODELS_FILE 指向你自己的 models.yaml")


def _wait_deployment(model: str) -> None:
    print(f"等待 {model} 就绪", end="", flush=True)
    deadline = time.time() + 3700
    with _client() as c:
        last = ""
        while time.time() < deadline:
            deps = {d["model"]: d for d in c.get("/api/deployments").json()}
            dep = deps.get(model)
            if dep is None:
                sys.exit(f"部署记录消失: {model}")
            status = dep["status"]
            if status != last:
                print(f"\n  [{status}] {(dep.get('reasons') or [''])[-1]}", end="", flush=True)
                last = status
            if status == "active":
                print(f"\n✔ {model} 已上线: {dep['driver_base_url']}（经 master 网关访问）")
                return
            if status in ("failed",):
                sys.exit(f"\n✘ 部署失败: {dep.get('error')}")
            print(".", end="", flush=True)
            time.sleep(3)
    sys.exit("等待超时")


def cmd_deploy(args: argparse.Namespace) -> None:
    _check_master()
    with _client() as c:
        try:
            r = c.post("/api/deploy", json={"model": args.model})
            r.raise_for_status()
            dep = r.json()
        except httpx.HTTPStatusError as exc:
            sys.exit(f"部署被拒绝: {exc.response.text}")
    print(f"规划: {dep['mode']} 模式  driver={dep['driver_device_id']} "
          f"rpc={dep['worker_device_ids'] or '-'}")
    for reason in dep["reasons"]:
        print(f"  · {reason}")
    _wait_deployment(args.model)


def cmd_undeploy(args: argparse.Namespace) -> None:
    _check_master()
    with _client() as c:
        r = c.post("/api/undeploy", json={"model": args.model})
        if r.status_code == 404:
            sys.exit(f"没有 {args.model} 的部署")
    print(f"{args.model} 停止中（worker 已在对应节点上关闭）")


def cmd_bench(args: argparse.Namespace) -> None:
    _check_master()
    prompt = args.prompt or "用一段话介绍分布式系统的CAP定理。"
    rounds = args.rounds

    with _client() as c:
        for i in range(rounds):
            t0 = time.time()
            ttft = None
            ntok = 0
            with c.stream("POST", "/v1/chat/completions",
                          json={"model": args.model, "stream": True,
                                "messages": [{"role": "user", "content": prompt}]}) as r:
                if r.status_code >= 400:
                    sys.exit(f"请求失败 HTTP {r.status_code}: {r.read().decode(errors='replace')[:300]}")
                for line in r.iter_lines():
                    if not line.startswith("data: ") or line == "data: [DONE]":
                        continue
                    try:
                        chunk = json.loads(line[6:])
                    except json.JSONDecodeError:
                        continue
                    delta = (chunk.get("choices") or [{}])[0].get("delta", {})
                    if delta.get("content"):
                        if ttft is None:
                            ttft = time.time() - t0
                        ntok += 1
            elapsed = time.time() - t0
            if ttft is None:
                ttft = elapsed
            tps = ntok / max(elapsed - ttft, 1e-6) if ntok else 0.0
            print(f"  round {i+1}: TTFT {ttft*1000:7.0f} ms | {ntok:4d} tokens | "
                  f"{tps:6.1f} tokens/s (decode)")

            result = {
                "ts": datetime.now(timezone.utc).isoformat(),
                "model": args.model,
                "round": i + 1,
                "ttft_s": round(ttft, 4),
                "tokens": ntok,
                "decode_tokens_per_s": round(tps, 2),
                "prompt": prompt,
                "master": MASTER_URL,
            }
            out_dir = Path(__file__).resolve().parent.parent / "benchmarks" / "results"
            out_dir.mkdir(parents=True, exist_ok=True)
            stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
            (out_dir / f"bench-{stamp}-r{i+1}.json").write_text(
                json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    print("结果已写入 benchmarks/results/")


def cmd_agent(args: argparse.Namespace) -> None:
    print("agent 是独立的 Go 单文件程序，安装方式见 docs/platforms.md：\n"
          "  Windows:      deploy\\windows\\install.ps1\n"
          "  Linux/树莓派: curl -fsSL deploy/linux/install.sh | bash\n"
          "  macOS:        bash deploy/macos/install.sh\n"
          "  Android:      Termux 中执行 bash deploy/android-termux/bootstrap.sh\n"
          "或从 GitHub Releases 下载对应平台的 spark-agent 后直接运行:\n"
          "  spark-agent --name my-device")


def cmd_wake(args: argparse.Namespace) -> None:
    _check_master()
    with _client() as c:
        r = c.post("/api/wake", json={"device_id": args.device_id})
        if r.status_code != 200:
            sys.exit(f"唤醒失败: {r.json().get('detail', r.text)}")
        j = r.json()
    print(f"已发送唤醒包 -> {j['device_id']} ({j['mac']}, {j['bytes_sent']} bytes)")
    print("等待设备上线后 spark status 查看（约 30-90 秒）")


def cmd_sleep(args: argparse.Namespace) -> None:
    _check_master()
    with _client() as c:
        r = c.post("/api/sleep", json={"device_id": args.device_id})
        if r.status_code != 200:
            sys.exit(f"休眠指令被拒绝: {r.json().get('detail', r.text)}")
    print(f"休眠指令已发送 -> {args.device_id}")


# ---------------- parser ----------------

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="spark", description="SPARK On-premises 控制台")
    sub = p.add_subparsers(dest="cmd", required=True)

    m = sub.add_parser("master", help="启动集群 master（含 Web 面板与 OpenAI 兼容网关）")
    m.add_argument("--host", default="0.0.0.0")
    m.add_argument("--port", type=int, default=int(os.environ.get("SPARK_MASTER_PORT", 8000)))
    m.set_defaults(func=cmd_master)

    s = sub.add_parser("status", help="查看设备与部署状态")
    s.set_defaults(func=cmd_status)

    m2 = sub.add_parser("models", help="列出可用模型")
    m2.set_defaults(func=cmd_models)

    d = sub.add_parser("deploy", help="部署一个模型（自动决策单机/切分）")
    d.add_argument("model")
    d.set_defaults(func=cmd_deploy)

    u = sub.add_parser("undeploy", help="卸载一个模型")
    u.add_argument("model")
    u.set_defaults(func=cmd_undeploy)

    b = sub.add_parser("bench", help="基准测试：TTFT 与解码吞吐，结果落盘")
    b.add_argument("--model", required=True)
    b.add_argument("--rounds", type=int, default=3)
    b.add_argument("--prompt", default=None)
    b.set_defaults(func=cmd_bench)

    a = sub.add_parser("agent", help="如何安装设备侧 agent")
    a.set_defaults(func=cmd_agent)

    w = sub.add_parser("wake", help="Wake-on-LAN 唤醒一台离线设备")
    w.add_argument("device_id")
    w.set_defaults(func=cmd_wake)

    sl = sub.add_parser("sleep", help="让一台空闲设备休眠省电（有部署时会被拒绝）")
    sl.add_argument("device_id")
    sl.set_defaults(func=cmd_sleep)
    return p


def main() -> None:
    args = build_parser().parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
