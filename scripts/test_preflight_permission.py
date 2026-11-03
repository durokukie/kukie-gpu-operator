"""preflight.sh의 permission 검사가 API 서버의 답과 '확인 못 함'을 구분하는지 본다. kubectl·curl은 가짜를 쓴다."""
import json
import os
import pathlib
import stat
import subprocess
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).resolve().parents[1] / "charts" / "kukie-gpu" / "files" / "preflight.sh"

FAKE_KUBECTL = """#!/bin/sh
# create -f - : the SubjectAccessReview. Everything else succeeds with no output.
for arg in "$@"; do
  if [ "$arg" = "--patch-file" ]; then shift_next=1; continue; fi
  if [ "${shift_next:-}" = 1 ]; then cp "$arg" "$RESULT_COPY"; shift_next=; fi
done
if [ "$1" = create ]; then
  cat > /dev/null
  case "$SAR_MODE" in
    true) printf true ;;
    false) printf false ;;
    error) echo 'subjectaccessreviews.authorization.k8s.io is forbidden' >&2; exit 1 ;;
  esac
fi
# The test PVC is gone as soon as it is asked for, so delete_pvc does not wait.
case " $* " in *" get pvc "*) exit 1 ;; esac
exit 0
"""


def run_preflight(mode):
    with tempfile.TemporaryDirectory() as tmp:
        bin_dir = pathlib.Path(tmp)
        for name, body in {"kubectl": FAKE_KUBECTL, "curl": "#!/bin/sh\nprintf 200\n"}.items():
            path = bin_dir / name
            path.write_text(body)
            path.chmod(path.stat().st_mode | stat.S_IEXEC)
        copy = bin_dir / "result.yaml"
        env = {
            "PATH": f"{bin_dir}:{os.environ['PATH']}",
            "SAR_MODE": mode,
            "RESULT_COPY": str(copy),
            "RELEASE_NAMESPACE": "kukie-gpu",
            "DCGM_NAMESPACE": "gpu-metrics",
            "PERMISSION_NAMESPACE": "llm",
            "PERMISSION_SERVICE_ACCOUNT": "operator",
        }
        subprocess.run(["sh", str(SCRIPT)], env=env, capture_output=True, text=True, check=True, timeout=60)
        line = next(x for x in copy.read_text().splitlines() if x.strip().startswith('{"checkedAt"'))
    return next(c for c in json.loads(line)["checks"] if c["name"] == "permission")


class PermissionCheckTest(unittest.TestCase):
    def test_all_allowed_passes(self):
        self.assertEqual(run_preflight("true")["status"], "pass")

    def test_denied_fails_with_the_rbac_hint(self):
        check = run_preflight("false")
        self.assertEqual(check["status"], "fail")
        self.assertIn("RBAC", check["hint"])

    def test_failed_review_is_a_skip_with_the_reason(self):
        check = run_preflight("error")
        self.assertEqual(check["status"], "skip")
        self.assertIn("확인하지 못함", check["detail"])
        self.assertIn("forbidden", check["detail"])


if __name__ == "__main__":
    unittest.main()
