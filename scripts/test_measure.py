"""oven/internal/controller/measure.py: 측정 단계, SLA 판정, 서빙 중 거부, 종료 메시지 크기."""

import http.server
import importlib.util
import json
import os
import stat
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

SCRIPT = Path(__file__).resolve().parent.parent / "oven/internal/controller/measure.py"
spec = importlib.util.spec_from_file_location("measure", SCRIPT)
measure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(measure)

# 진짜 vllm bench serve 자리에 놓는 가짜. 동시 수마다 FAKE_RESULTS의 값을 결과 파일로 남긴다.
FAKE_VLLM = """#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
get = lambda name: args[args.index(name) + 1]
c = get("--max-concurrency")
with open(os.environ["FAKE_CALLS"], "a") as f:
    f.write(c + "\\n")
with open(os.environ["FAKE_CALLS"] + ".seeds", "a") as f:
    f.write(get("--seed") + "\\n")
result = json.loads(os.environ["FAKE_RESULTS"]).get(c)
if result is None:
    sys.exit(7)
result.setdefault("completed", int(get("--num-prompts")))
with open(os.path.join(get("--result-dir"), get("--result-filename")), "w") as f:
    json.dump(result, f)
"""


def server(metrics):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = metrics["text"].encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    return httpd


def gauges(running=0, waiting=0, kv=0.0):
    return (
        "# HELP x\n"
        'vllm:num_requests_running{model_name="m"} %s\n'
        'vllm:num_requests_waiting{model_name="m"} %s\n'
        'vllm:kv_cache_usage_perc{model_name="m"} %s\n' % (running, waiting, kv)
    )


class MeasureTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        tmp = Path(self.tmp.name)
        cli = tmp / "bin" / "vllm"
        cli.parent.mkdir()
        cli.write_text(FAKE_VLLM)
        cli.chmod(cli.stat().st_mode | stat.S_IEXEC)
        self.calls = tmp / "calls"
        self.calls.write_text("")
        self.metrics = {"text": gauges()}
        self.httpd = server(self.metrics)
        self.addCleanup(self.httpd.server_close)
        self.addCleanup(self.httpd.shutdown)
        old = dict(os.environ)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(old)))
        os.environ["PATH"] = str(cli.parent) + os.pathsep + os.environ["PATH"]
        os.environ["FAKE_CALLS"] = str(self.calls)
        self.cfg = {
            "base_url": "http://127.0.0.1:%d" % self.httpd.server_address[1],
            "model": "org/model", "out": str(tmp / "out"), "concurrency": [1, 2, 4, 8],
            "input_len": 1000, "output_len": 200, "ttft_ms": 1000.0, "itl_ms": 50.0, "deadline": 3600.0,
            "termination_log": str(tmp / "termination"), "seed": 1000,
        }

    def results(self, table):
        os.environ["FAKE_RESULTS"] = json.dumps({str(c): r for c, r in table.items()})

    @staticmethod
    def ok(ttft=100, itl=20, tps=500):
        return {"p95_ttft_ms": ttft, "p95_itl_ms": itl, "output_throughput": tps}

    def called(self):
        return self.calls.read_text().split()

    def seeds(self):
        return Path(str(self.calls) + ".seeds").read_text().split()

    def test_every_step_sends_new_prompts(self):
        # 같은 seed면 앞 단계의 프롬프트를 다시 보내 prefix cache가 받는다. 그러면 서버가 아니라 캐시를 잰다
        self.results({1: self.ok(), 2: self.ok(), 4: self.ok(), 8: self.ok(), 16: self.ok(ttft=1500)})
        self.cfg["concurrency"] = [1, 2, 4, 8, 16]
        code, result = measure.measure(self.cfg)
        self.assertEqual(code, measure.EXIT_OK)
        self.assertEqual(self.seeds(), ["1001", "1002", "1004", "1008", "1016"])
        self.assertEqual(result["seed"], 1000, "결과의 seed로 같은 측정을 다시 돌린다")

    def test_runs_do_not_share_a_seed_unless_asked(self):
        env = {"BASE_URL": "http://x", "MODEL": "m"}
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertNotEqual(measure.config()["seed"], measure.config()["seed"])
        with mock.patch.dict(os.environ, {**env, "SEED": "7"}, clear=True):
            self.assertEqual(measure.config()["seed"], 7)

    def test_stops_at_first_step_over_the_sla_and_reports_the_step_before(self):
        self.results({1: self.ok(), 2: self.ok(tps=900), 4: self.ok(ttft=1500), 8: self.ok()})
        code, result = measure.measure(self.cfg)
        self.assertEqual(code, measure.EXIT_OK)
        self.assertTrue(result["ok"])
        self.assertEqual(result["maxConcurrency"], 2)
        self.assertEqual(self.called(), ["1", "2", "4"], "8은 돌리지 않는다")
        self.assertEqual([s["sla"] for s in result["steps"]], [True, True, False])
        self.assertIn("900", result["message"])

    def test_itl_also_breaks_the_sla(self):
        self.results({1: self.ok(), 2: self.ok(itl=51)})
        _, result = measure.measure(self.cfg)
        self.assertEqual(result["maxConcurrency"], 1)

    def test_a_step_with_failed_requests_does_not_pass(self):
        self.results({1: self.ok(), 2: dict(self.ok(), completed=3)})
        _, result = measure.measure(self.cfg)
        self.assertEqual(result["maxConcurrency"], 1)

    def test_baseline_is_what_was_seen_at_the_best_step(self):
        self.metrics["text"] = gauges()
        self.results({1: self.ok(), 2: self.ok(ttft=5000)})
        original = measure.Sampler.run

        def fake_run(sampler):
            sampler.waiting, sampler.kv = 3.0, 0.615

        measure.Sampler.run = fake_run
        self.addCleanup(lambda: setattr(measure.Sampler, "run", original))
        _, result = measure.measure(self.cfg)
        self.assertEqual((result["maxWaiting"], result["maxKvPercent"]), (3, 62))

    def test_refuses_while_serving(self):
        self.metrics["text"] = gauges(running=2)
        self.results({1: self.ok()})
        code, result = measure.measure(self.cfg)
        self.assertEqual(code, measure.EXIT_BUSY)
        self.assertEqual(result["reason"], "EngineBusy")
        self.assertEqual(self.called(), [], "서빙 중이면 요청을 보내지 않는다")

    def test_waiting_requests_also_count_as_serving(self):
        self.metrics["text"] = gauges(waiting=1)
        self.assertEqual(measure.measure(self.cfg)[0], measure.EXIT_BUSY)

    def test_no_step_within_the_sla(self):
        self.results({1: self.ok(ttft=2000)})
        code, result = measure.measure(self.cfg)
        self.assertEqual(code, measure.EXIT_NO_PASS)
        self.assertFalse(result["ok"])
        self.assertEqual(result["reason"], "NoStepWithinSLA")
        self.assertNotIn("maxConcurrency", result)

    def test_client_failure_keeps_what_was_measured(self):
        self.results({1: self.ok()})  # 2에서 가짜 CLI가 7로 끝난다
        code, result = measure.measure(self.cfg)
        self.assertEqual(code, measure.EXIT_OK)
        self.assertEqual(result["maxConcurrency"], 1)
        self.assertEqual(result["reason"], "MeasuredPartial")
        self.assertTrue(result["partial"])
        self.assertIn("exited 7", result["message"])

    def test_stopping_at_the_time_limit_is_partial(self):
        self.results({1: self.ok(), 2: self.ok()})
        self.cfg["deadline"] = 1000.0
        clock = iter([0.0, 0.0, 2000.0])
        code, result = measure.measure(self.cfg, now=lambda: next(clock))
        self.assertEqual(code, measure.EXIT_OK)
        self.assertEqual(result["reason"], "MeasuredPartial")
        self.assertEqual(self.called(), ["1"])

    def test_a_complete_run_is_not_partial(self):
        self.results({1: self.ok(), 2: self.ok(ttft=5000)})
        _, result = measure.measure(self.cfg)
        self.assertEqual(result["reason"], "Measured")
        self.assertFalse(result["partial"])

    def test_server_gone(self):
        self.cfg["base_url"] = "http://127.0.0.1:9"
        code, result = measure.measure(self.cfg)
        self.assertEqual(code, measure.EXIT_ERROR)
        self.assertEqual(result["reason"], "EngineUnreachable")

    def test_no_time_left(self):
        self.results({1: self.ok()})
        self.cfg["deadline"] = 10.0
        code, result = measure.measure(self.cfg)
        self.assertEqual(code, measure.EXIT_NO_PASS)
        self.assertIn("시간 상한", result["message"])
        self.assertEqual(self.called(), [])

    def test_encode_keeps_the_baseline_within_the_limit(self):
        steps = [{"c": i, "pad": "x" * 200} for i in range(40)]
        text = measure.encode({"ok": True, "reason": "Measured", "maxConcurrency": 8, "steps": steps})
        self.assertLessEqual(len(text.encode()), measure.TERMINATION_LIMIT)
        self.assertEqual(json.loads(text)["maxConcurrency"], 8)

    def test_main_writes_the_termination_message(self):
        self.results({1: self.ok()})
        cfg = self.cfg
        os.environ.update({
            "BASE_URL": cfg["base_url"], "MODEL": cfg["model"], "OUT": cfg["out"], "CONCURRENCY": "1",
            "TERMINATION_LOG": cfg["termination_log"],
        })
        self.assertEqual(measure.main(), 0)
        self.assertTrue(json.loads(Path(cfg["termination_log"]).read_text())["ok"])


if __name__ == "__main__":
    unittest.main()
