"""oven/internal/controller/download.py: 받은 용량 계산, 전체 크기, 진행률 줄, hf 종료 코드."""

import http.server
import importlib.util
import io
import json
import os
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "oven/internal/controller/download.py"
spec = importlib.util.spec_from_file_location("download", SCRIPT)
download = importlib.util.module_from_spec(spec)
spec.loader.exec_module(download)

MODEL = "org/model"


def write(path, size):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(b"x" * size)


class CacheLayout(unittest.TestCase):
    """huggingface_hub 2.2.0·hf_xet 1.7.0에서 실제로 본 캐시 구조를 그대로 만든다."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.hub = Path(tmp.name) / "hub"
        self.blobs = self.hub / "models--org--model" / "blobs"
        self.blobs.mkdir(parents=True)

    def measured(self):
        return download.downloaded_bytes(str(self.blobs))

    def shared(self, xet_hash, size, etag):
        """끝난 Xet 파일: 공용 저장소의 실제 파일과 모델 폴더의 심볼릭 링크."""
        store = self.hub / "blobs" / xet_hash[:2] / xet_hash
        write(store, size)
        (self.blobs / etag).symlink_to(os.path.relpath(store, self.blobs))

    def test_the_folder_of_the_job_env(self):
        self.assertEqual(download.blobs_dir({"HF_HOME": "/models"}, MODEL), "/models/hub/models--org--model/blobs")
        self.assertEqual(download.blobs_dir({"HF_HUB_CACHE": "/c", "HF_HOME": "/models"}, MODEL), "/c/models--org--model/blobs")

    def test_no_folder_yet_is_zero(self):
        self.assertEqual(download.downloaded_bytes(str(self.blobs / "not-there")), 0)

    def test_regular_files_count(self):
        write(self.blobs / "aaa", 10)
        write(self.blobs / "bbb", 20)
        self.assertEqual(self.measured(), 30)

    def test_a_file_moved_to_the_shared_store_still_counts(self):
        # 폴더 자체의 크기만 재면 Xet 파일이 끝나는 순간 받은 용량이 줄어든다. 실제로 그렇게 움직이는 것을 봤다.
        self.shared("ff00", 1000, "etag1")
        write(self.blobs / "cfg", 5)
        self.assertEqual(self.measured(), 1005)

    def test_the_file_being_written_counts(self):
        write(self.blobs / "etag1.a1b2c3d4.incomplete", 300)
        self.assertEqual(self.measured(), 300)

    def test_old_incomplete_name_counts(self):
        write(self.blobs / "etag1.incomplete", 300)
        self.assertEqual(self.measured(), 300)

    def test_a_killed_attempt_is_not_counted_twice(self):
        # 시도마다 임시 이름이 다르고, 강제 종료된 시도의 파일은 남는다.
        write(self.blobs / "etag1.11111111.incomplete", 300)
        write(self.blobs / "etag1.22222222.incomplete", 100)
        self.assertEqual(self.measured(), 300)

    def test_a_finished_file_outweighs_its_partial_files(self):
        write(self.blobs / "etag1.11111111.incomplete", 300)
        self.shared("ff01", 1000, "etag1")
        self.assertEqual(self.measured(), 1000)

    def test_files_from_before_this_attempt_count(self):
        self.shared("ff02", 1000, "done")
        write(self.blobs / "cfg", 5)
        write(self.blobs / "next.11111111.incomplete", 7)
        self.assertEqual(self.measured(), 1012)

    def test_a_dangling_link_is_skipped(self):
        (self.blobs / "gone").symlink_to("../../blobs/ff/gone")
        write(self.blobs / "cfg", 5)
        self.assertEqual(self.measured(), 5)

    @unittest.skipIf(os.geteuid() == 0, "root reads everything")
    def test_an_unreadable_folder_is_zero_not_an_error(self):
        self.blobs.chmod(0)
        self.addCleanup(self.blobs.chmod, stat.S_IRWXU)
        self.assertEqual(self.measured(), 0)


class RepoSize(unittest.TestCase):
    def test_sums_lfs_and_regular_files(self):
        siblings = [
            {"rfilename": "config.json", "blobId": "c1", "size": 10},
            {"rfilename": "model.safetensors", "blobId": "p1", "size": 100, "lfs": {"sha256": "s1", "size": 100, "pointerSize": 133}},
        ]
        self.assertEqual(download.repo_size(siblings), 110)

    def test_the_same_content_is_one_blob(self):
        # sentence-transformers/all-MiniLM-L6-v2: 파일 30개, blob 28개. 그냥 더하면 끝나도 95%에 머문다.
        siblings = [
            {"rfilename": "onnx/a.onnx", "blobId": "p1", "size": 100, "lfs": {"sha256": "s1", "size": 100}},
            {"rfilename": "onnx/b.onnx", "blobId": "p1", "size": 100, "lfs": {"sha256": "s1", "size": 100}},
        ]
        self.assertEqual(download.repo_size(siblings), 100)

    def test_a_missing_size_makes_it_unknown(self):
        self.assertIsNone(download.repo_size([{"rfilename": "a", "blobId": "b1", "size": 1}, {"rfilename": "c", "blobId": "b2"}]))

    def test_nothing_is_unknown(self):
        self.assertIsNone(download.repo_size([]))


class Hub(http.server.BaseHTTPRequestHandler):
    """가짜 허깅페이스. answers에 경로별 (코드, 본문)을 둔다."""

    answers = {}
    seen = []

    def do_GET(self):
        Hub.seen.append((self.path, self.headers.get("Authorization")))
        code, body, *location = Hub.answers.get(self.path, (404, b"{}"))
        self.send_response(code)
        if location:
            self.send_header("Location", location[0])
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class FetchTotal(unittest.TestCase):
    PATH = "/api/models/org/model?blobs=true"

    def setUp(self):
        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Hub)
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.addCleanup(self.httpd.server_close)
        self.addCleanup(self.httpd.shutdown)
        Hub.answers, Hub.seen = {}, []
        self.env = {"HF_ENDPOINT": f"http://127.0.0.1:{self.httpd.server_address[1]}/"}

    def answer(self, siblings):
        Hub.answers[self.PATH] = (200, json.dumps({"siblings": siblings}).encode())

    def test_reads_the_size(self):
        self.answer([{"rfilename": "a", "blobId": "b", "size": 7}])
        self.assertEqual(download.fetch_total(MODEL, self.env), 7)
        self.assertEqual(Hub.seen, [(self.PATH, None)])

    def test_sends_the_token_when_there_is_one(self):
        self.answer([{"rfilename": "a", "blobId": "b", "size": 7}])
        download.fetch_total(MODEL, dict(self.env, HF_TOKEN="tok"))
        self.assertEqual(Hub.seen[0][1], "Bearer tok")

    def test_the_token_does_not_follow_a_redirect(self):
        self.answer([{"rfilename": "a", "blobId": "b", "size": 7}])
        Hub.answers["/api/models/old/name?blobs=true"] = (307, b"", self.PATH)
        self.assertEqual(download.fetch_total("old/name", dict(self.env, HF_TOKEN="tok")), 7)
        self.assertEqual([auth for _, auth in Hub.seen], ["Bearer tok", None])

    def test_an_error_answer_raises_for_the_caller(self):
        Hub.answers[self.PATH] = (401, b"{}")
        with self.assertRaises(Exception):
            download.fetch_total(MODEL, self.env)


class ReporterLines(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = Path(tmp.name)
        self.blobs = self.home / "hub/models--org--model/blobs"
        self.env = {"HF_HOME": tmp.name, "HF_ENDPOINT": "http://127.0.0.1:9"}
        self.out = io.StringIO()
        self.reporter = download.Reporter(MODEL, self.env, out=self.out, total_retry=0.0)

    def lines(self):
        return [json.loads(line) for line in self.out.getvalue().splitlines()]

    def quietly(self, call):
        stderr, sys.stderr = sys.stderr, io.StringIO()
        try:
            call()
            return sys.stderr.getvalue()
        finally:
            sys.stderr = stderr

    def with_total(self, total):
        original = download.fetch_total
        self.addCleanup(setattr, download, "fetch_total", original)
        download.fetch_total = total

    def test_line_shape_with_a_total(self):
        write(self.blobs / "aaa", 10)
        self.with_total(lambda *a, **k: 100)
        self.reporter.pass_once()
        self.assertEqual(self.lines(), [{"event": "download_progress", "downloadedBytes": 10, "totalBytes": 100}])

    def test_unknown_total_still_writes_and_tries_again(self):
        answers = iter([OSError("no route"), 100])

        def total(*a, **k):
            answer = next(answers)
            if isinstance(answer, Exception):
                raise answer
            return answer

        self.with_total(total)
        self.quietly(self.reporter.pass_once)
        self.reporter.pass_once()
        self.assertEqual([line["totalBytes"] for line in self.lines()], [None, 100])

    def test_the_failure_message_leaves_out_what_the_error_says(self):
        def total(*a, **k):
            raise OSError("Authorization: Bearer secret")

        self.with_total(total)
        said = self.quietly(self.reporter.pass_once)
        self.assertIn("OSError", said)
        self.assertNotIn("secret", said)

    def test_a_known_total_is_not_asked_again(self):
        calls = []
        self.with_total(lambda *a, **k: calls.append(1) or 100)
        self.reporter.pass_once()
        self.reporter.pass_once()
        self.assertEqual(len(calls), 1)

    def test_the_last_line_does_not_ask_the_hub(self):
        # hf가 끝난 뒤에는 2초만 기다린다. 여기서 Hub를 기다리면 마지막 줄을 잃는다.
        calls = []
        self.with_total(lambda *a, **k: calls.append(1))
        self.reporter.stopping.set()
        self.reporter.run()
        self.assertEqual(len(calls), 1, "the first pass asks, the last does not")
        self.assertEqual(len(self.lines()), 2)

    def test_a_failure_while_measuring_costs_one_line_only(self):
        self.with_total(lambda *a, **k: 100)
        original = download.downloaded_bytes
        self.addCleanup(setattr, download, "downloaded_bytes", original)

        def broken(_):
            raise RuntimeError("the volume went away")

        download.downloaded_bytes = broken
        self.reporter.pass_once()
        self.assertEqual(self.lines(), [])
        download.downloaded_bytes = original
        self.reporter.pass_once()
        self.assertEqual(len(self.lines()), 1)

    def test_a_closed_stdout_is_not_fatal(self):
        self.with_total(lambda *a, **k: 100)
        self.out.close()
        self.reporter.pass_once()


FAKE_HF = """#!/usr/bin/env python3
import os, sys, time
with open(os.environ["FAKE_ARGS"], "w") as f:
    f.write(" ".join(sys.argv[1:]) + "\\n" + os.environ.get("HF_HUB_DISABLE_PROGRESS_BARS", "") + "\\n" + os.environ.get("HF_TOKEN", ""))
blobs = os.path.join(os.environ["HF_HOME"], "hub", "models--org--model", "blobs")
os.makedirs(blobs, exist_ok=True)
with open(os.path.join(blobs, "aaa"), "w") as f:
    f.write("x" * 50)
time.sleep(float(os.environ.get("FAKE_SLEEP", "0")))
print("Downloaded")
sys.exit(int(os.environ.get("FAKE_EXIT", "0")))
"""


class WholeScript(unittest.TestCase):
    """실제 실행처럼 python3 -c로 돌린다. Hub는 닿지 않는 주소라 전체 크기를 못 구하는 경우가 그대로 나온다."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        hf = self.dir / "bin" / "hf"
        hf.parent.mkdir()
        hf.write_text(FAKE_HF)
        hf.chmod(0o755)
        self.env = {
            "PATH": f"{hf.parent}:{os.environ['PATH']}",
            "HF_HOME": str(self.dir / "models"),
            "HF_ENDPOINT": "http://127.0.0.1:9",
            "MODEL": MODEL,
            "HF_TOKEN": "tok",
            "FAKE_ARGS": str(self.dir / "args"),
        }

    def popen(self, **extra):
        return subprocess.Popen(
            [sys.executable, "-c", SCRIPT.read_text()],
            env=dict(self.env, **extra),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

    def progress(self, stdout):
        return [json.loads(line) for line in stdout.splitlines() if line.startswith("{")]

    def test_runs_hf_download_and_reports_what_it_stored(self):
        process = self.popen()
        stdout, _ = process.communicate(timeout=60)
        self.assertEqual(process.returncode, 0)
        self.assertEqual((self.dir / "args").read_text(), "download org/model\n1\ntok")
        self.assertIn("Downloaded", stdout)
        lines = self.progress(stdout)
        self.assertEqual(lines[-1], {"event": "download_progress", "downloadedBytes": 50, "totalBytes": None})

    def test_the_exit_code_is_the_one_of_hf(self):
        process = self.popen(FAKE_EXIT="3")
        process.communicate(timeout=60)
        self.assertEqual(process.returncode, 3)

    def test_a_hung_hub_does_not_hold_up_the_exit_code(self):
        # 조회가 막힌 채 hf가 끝나도 곧바로 hf의 종료 코드로 끝난다.
        with socket.socket() as hung:
            hung.bind(("127.0.0.1", 0))
            hung.listen()
            started = time.monotonic()
            process = self.popen(FAKE_EXIT="3", HF_ENDPOINT=f"http://127.0.0.1:{hung.getsockname()[1]}")
            process.communicate(timeout=60)
        self.assertEqual(process.returncode, 3)
        self.assertLess(time.monotonic() - started, download.API_TIMEOUT_SECONDS)

    def test_no_hf_is_a_failure_with_a_message(self):
        process = self.popen(PATH="/nonexistent")
        _, stderr = process.communicate(timeout=60)
        self.assertEqual(process.returncode, download.EXIT_NO_HF)
        self.assertIn("cannot start hf", stderr)

    def test_sigterm_reaches_hf(self):
        process = self.popen(FAKE_SLEEP="60")
        deadline = time.monotonic() + 30
        while not (self.dir / "args").exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=20)
        self.assertEqual(process.returncode, 128 + signal.SIGTERM)

    def test_no_line_holds_the_token(self):
        process = self.popen()
        stdout, stderr = process.communicate(timeout=60)
        self.assertNotIn("tok", self.progress_text(stdout) + stderr)

    def progress_text(self, stdout):
        return "\n".join(line for line in stdout.splitlines() if line.startswith("{"))


if __name__ == "__main__":
    unittest.main()
