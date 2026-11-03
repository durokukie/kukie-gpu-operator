"""Measure one running vLLM server (DURO-169). The oven runs this in a Job when a ServedModel asks for it.

Raises concurrent requests step by step with `vllm bench serve` and stops at the first step that breaks the SLA, because
every further step only keeps the GPU busy. The highest step within the SLA gives the baseline: the waiting requests and
KV cache usage seen there are what the admission proxy will later compare against.

The Pod has no Kubernetes token. It hands the result to the oven through its termination message (at most 4096 bytes)
as one JSON object; the oven writes status.baseline and the ConfigMap. Exit code: 0 measured, 3 the server is serving
requests, 4 no step met the SLA, 1 anything else.

Needs the vllm CLI and python3, both in the vllm/vllm-openai image. Standard library only.
"""

import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.request

EXIT_OK, EXIT_ERROR, EXIT_BUSY, EXIT_NO_PASS = 0, 1, 3, 4
TERMINATION_LIMIT = 4096
# vllm stops the step at this margin before the Job's deadline, so a partial result still gets written
DEADLINE_MARGIN_SECONDS = 60
METRIC_LINE = re.compile(r"^vllm:(\w+)(?:\{[^}]*\})? ([0-9.eE+-]+)$")


def config():
    return {
        "base_url": os.environ["BASE_URL"].rstrip("/"),
        "model": os.environ["MODEL"],
        "out": os.environ.get("OUT", "/out"),
        "concurrency": [int(c) for c in os.environ.get("CONCURRENCY", "1 2 4 8 16 32 64").split()],
        "input_len": int(os.environ.get("INPUT_LEN", "1000")),
        "output_len": int(os.environ.get("OUTPUT_LEN", "200")),
        "ttft_ms": float(os.environ.get("TTFT_MS", "1000")),
        "itl_ms": float(os.environ.get("ITL_MS", "50")),
        "deadline": float(os.environ.get("DEADLINE_SECONDS", "3600")),
        "termination_log": os.environ.get("TERMINATION_LOG", "/dev/termination-log"),
    }


def read_metrics(base_url):
    """vLLM's gauges, summed across labels. KV cache has an older name, gpu_cache_usage_perc."""
    with urllib.request.urlopen(base_url + "/metrics", timeout=5) as response:
        text = response.read().decode()
    values = {}
    for line in text.splitlines():
        match = METRIC_LINE.match(line)
        if match:
            values[match.group(1)] = values.get(match.group(1), 0.0) + float(match.group(2))
    return {
        "running": values.get("num_requests_running", 0.0),
        "waiting": values.get("num_requests_waiting", 0.0),
        "kv": values.get("kv_cache_usage_perc", values.get("gpu_cache_usage_perc", 0.0)),
    }


class Sampler(threading.Thread):
    """Keeps the highest waiting and KV values while one step runs. A failed scrape only loses that one sample."""

    def __init__(self, base_url, interval=2.0):
        super().__init__(daemon=True)
        self.base_url, self.interval = base_url, interval
        self.waiting, self.kv = None, None
        self.stopping = threading.Event()

    def run(self):
        while not self.stopping.is_set():
            try:
                metrics = read_metrics(self.base_url)
                self.waiting = max(self.waiting or 0.0, metrics["waiting"])
                self.kv = max(self.kv or 0.0, metrics["kv"])
            except Exception:  # noqa: BLE001 - see the class comment
                pass
            self.stopping.wait(self.interval)

    def stop(self):
        self.stopping.set()
        self.join(timeout=self.interval + 6)


def run_step(cfg, c, remaining):
    """One concurrency step. At least 40 requests and 8 rounds of the concurrency, so p95 has enough samples."""
    prompts = max(c * 8, 40)
    result_path = os.path.join(cfg["out"], "c%d.json" % c)
    command = [
        "vllm", "bench", "serve", "--model", cfg["model"], "--base-url", cfg["base_url"],
        "--dataset-name", "random", "--random-input-len", str(cfg["input_len"]),
        "--random-output-len", str(cfg["output_len"]), "--ignore-eos",
        "--num-prompts", str(prompts), "--max-concurrency", str(c),
        "--percentile-metrics", "ttft,tpot,itl,e2el", "--metric-percentiles", "50,95,99",
        "--save-result", "--result-dir", cfg["out"], "--result-filename", "c%d.json" % c,
    ]
    sampler = Sampler(cfg["base_url"])
    sampler.start()
    try:
        done = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=remaining, check=False,
                              universal_newlines=True)
    finally:
        sampler.stop()
    # Kubectl logs is where the details stay; the termination message has no room for them.
    sys.stdout.write(done.stdout[-4000:])
    if done.returncode != 0:
        raise RuntimeError("concurrency %d: vllm bench serve exited %d" % (c, done.returncode))
    with open(result_path) as f:
        raw = json.load(f)
    ttft, itl = raw.get("p95_ttft_ms"), raw.get("p95_itl_ms")
    completed = raw.get("completed") or 0
    sla = completed >= prompts and ttft is not None and itl is not None and ttft <= cfg["ttft_ms"] and itl <= cfg["itl_ms"]
    return {
        "c": c, "done": completed, "total": prompts,
        "ttft95": round(ttft) if ttft is not None else None,
        "itl95": round(itl, 1) if itl is not None else None,
        "tps": round(raw.get("output_throughput") or 0),
        "waiting": None if sampler.waiting is None else round(sampler.waiting),
        "kv": None if sampler.kv is None else round(sampler.kv * 100),
        "sla": sla,
    }


def measure(cfg, now=time.monotonic):
    """Returns (exit code, result). Never raises for a measurement that goes wrong: the oven needs the reason."""
    start = now()
    os.makedirs(cfg["out"], exist_ok=True)
    sla = {"ttftMs": cfg["ttft_ms"], "itlMs": cfg["itl_ms"], "inputTokens": cfg["input_len"], "outputTokens": cfg["output_len"]}
    try:
        before = read_metrics(cfg["base_url"])
    except Exception as err:  # noqa: BLE001
        return EXIT_ERROR, {"ok": False, "reason": "EngineUnreachable", "message": "vLLM 지표를 읽지 못했다: %s" % err}
    if before["running"] > 0 or before["waiting"] > 0:
        return EXIT_BUSY, {
            "ok": False, "reason": "EngineBusy",
            "message": "처리 중 %d, 대기 %d. 서빙 중에는 측정하지 않는다. 요청이 없을 때 annotation을 지웠다가 다시 단다"
                       % (before["running"], before["waiting"]),
        }

    steps, stopped = [], ""
    for c in cfg["concurrency"]:
        remaining = cfg["deadline"] - DEADLINE_MARGIN_SECONDS - (now() - start)
        if remaining <= 0:
            stopped = "시간 상한으로 동시 %d 전에 멈췄다" % c
            break
        try:
            step = run_step(cfg, c, remaining)
        except subprocess.TimeoutExpired:
            stopped = "시간 상한으로 동시 %d 중에 멈췄다" % c
            break
        except Exception as err:  # noqa: BLE001
            stopped = str(err)[:300]
            break
        steps.append(step)
        print("concurrency %d: %s" % (c, json.dumps(step)), flush=True)
        if not step["sla"]:
            break

    passing = [step for step in steps if step["sla"]]
    if not passing:
        return EXIT_NO_PASS, {
            "ok": False, "reason": "NoStepWithinSLA", "sla": sla, "steps": steps,
            "message": "SLA 안에 든 단계가 없다. %s" % stopped if stopped else "SLA 안에 든 단계가 없다",
        }
    best = passing[-1]
    message = "동시 %d까지 SLA 안, 그때 출력 %d 토큰/초" % (best["c"], best["tps"])
    if stopped:
        message += ". %s" % stopped
    return EXIT_OK, {
        "ok": True, "reason": "Measured", "message": message, "sla": sla, "steps": steps,
        "maxConcurrency": best["c"], "maxWaiting": best["waiting"] or 0, "maxKvPercent": best["kv"] or 0,
    }


def encode(result):
    """Compact JSON within the termination message limit. Steps are the first thing to go; the baseline never does."""
    text = json.dumps(result, separators=(",", ":"), ensure_ascii=False)
    if len(text.encode()) <= TERMINATION_LIMIT:
        return text
    slim = dict(result)
    slim.pop("steps", None)
    return json.dumps(slim, separators=(",", ":"), ensure_ascii=False)


def main():
    cfg = config()
    code, result = measure(cfg)
    text = encode(result)
    print(text, flush=True)
    with open(cfg["termination_log"], "w") as f:
        f.write(text)
    return code


if __name__ == "__main__":
    sys.exit(main())
