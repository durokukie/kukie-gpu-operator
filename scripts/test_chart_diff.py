"""chart_diff.py가 같은 렌더는 통과시키고 달라진 렌더는 잡는지 본다. helm과 PyYAML이 필요하다."""
import pathlib
import shutil
import subprocess
import sys
import unittest

SCRIPT = pathlib.Path(__file__).resolve().with_name("chart_diff.py")


def run(*args):
    return subprocess.run([sys.executable, str(SCRIPT), *args], capture_output=True, text=True, check=False)


@unittest.skipUnless(shutil.which("helm"), "helm이 없다")
class ChartDiffTest(unittest.TestCase):
    def test_default_render_matches_baseline(self):
        result = run()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_changed_value_is_reported(self):
        result = run("--set", "dcgm.resources.requests.cpu=60m")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("다름: DaemonSet gpu-metrics/dcgm-exporter", result.stdout)

    def test_object_outside_allow_list_is_reported(self):
        result = run("--set", "namespaces.llm=other-llm")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("차트에만 있음(허용 목록 밖)", result.stdout)


if __name__ == "__main__":
    unittest.main()
