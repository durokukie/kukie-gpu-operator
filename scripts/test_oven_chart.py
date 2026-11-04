"""차트의 oven 스위치와, files/oven 복사본이 oven/config 생성물과 같은지 본다. helm만 쓰고 클러스터는 건드리지 않는다."""
import pathlib
import subprocess
import unittest

import yaml

from set_oven_image import set_image

ROOT = pathlib.Path(__file__).resolve().parents[1]
CHART = ROOT / "charts" / "kukie-gpu"
DIGEST = "sha256:" + "a" * 64


def render(*args):
    return subprocess.run(
        ["helm", "template", "kukie-gpu", str(CHART), "--namespace", "kukie-gpu", *args],
        capture_output=True, text=True, timeout=60,
    )


def docs(*args):
    result = render(*args)
    if result.returncode != 0:
        raise AssertionError(result.stderr)
    return [d for d in yaml.safe_load_all(result.stdout) if d]


def find(items, kind, name):
    return next(d for d in items if d["kind"] == kind and d["metadata"]["name"] == name)


class OvenChartTest(unittest.TestCase):
    def test_off_by_default(self):
        names = {(d["kind"], d["metadata"]["name"]) for d in docs()}
        self.assertNotIn(("Deployment", "kukie-oven"), names)
        self.assertFalse([k for k, _ in names if k == "CustomResourceDefinition"])

    def test_enabled_needs_an_image(self):
        result = render("--set", "oven.enabled=true")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("oven.image.tag", result.stderr)

    def test_enabled_renders_the_deployment_with_its_name_and_image(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=0.3.0", "--set", f"oven.image.digest={DIGEST}")
        spec = find(items, "Deployment", "kukie-oven")["spec"]["template"]["spec"]
        self.assertEqual(spec["containers"][0]["image"], f"ghcr.io/durokukie/kukie-oven:0.3.0@{DIGEST}")
        self.assertEqual(spec["serviceAccountName"], "kukie-oven")
        self.assertIn("--gpu-node-selector=kukie.io/accelerator=nvidia-l40s", spec["containers"][0]["args"])

    def test_measure_values_reach_the_manager_flags(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", "oven.measure.deadline=30m",
                     "--set", "oven.measure.ttftMs=800")
        args = find(items, "Deployment", "kukie-oven")["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertIn("--measure-deadline=30m", args)
        self.assertIn("--measure-ttft-ms=800", args)
        self.assertIn("--measure-concurrency=1 2 4 8 16 32 64", args)

    def manager_args(self, *args):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x", *args)
        return find(items, "Deployment", "kukie-oven")["spec"]["template"]["spec"]["containers"][0]["args"]

    def test_run_time_limit_defaults_reach_the_manager_flags(self):
        args = self.manager_args()
        self.assertIn("--max-runtime=2h", args)
        self.assertIn("--node-wait-timeout=15m", args)

    def test_run_time_limit_values_reach_the_manager_flags(self):
        args = self.manager_args("--set", "oven.maxRuntime=4h", "--set", "oven.nodeWaitTimeout=30m")
        self.assertIn("--max-runtime=4h", args)
        self.assertIn("--node-wait-timeout=30m", args)

    def test_zero_is_kept_not_dropped(self):
        # 0 means "never stop". A template that treats it as empty would silently fall back to the 2h default.
        args = self.manager_args("--set", "oven.maxRuntime=0", "--set", "oven.nodeWaitTimeout=0")
        self.assertIn("--max-runtime=0", args)
        self.assertIn("--node-wait-timeout=0", args)

    def test_bad_duration_fails_the_render(self):
        for value in ["7200", "2hours", "1d", "-1h"]:
            result = render("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set-string", f"oven.maxRuntime={value}")
            self.assertNotEqual(result.returncode, 0, value)
            self.assertIn("oven.maxRuntime", result.stderr, value)
        result = render("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set-string", "oven.nodeWaitTimeout=soon")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("oven.nodeWaitTimeout", result.stderr)

    def test_null_duration_fails_the_render(self):
        result = render("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", "oven.maxRuntime=null")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("oven.maxRuntime", result.stderr)

    def test_crd_has_the_run_time_limit_fields(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        crd = find(items, "CustomResourceDefinition", "servedmodels.serving.kukie.io")
        schema = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]
        self.assertIn("maxRuntime", schema["spec"]["properties"])
        self.assertIn("startedAt", schema["status"]["properties"])
        self.assertIn("stopsAt", schema["status"]["properties"])
        self.assertIn("Stopped", schema["status"]["properties"]["phase"]["enum"])

    def test_empty_gpu_selector_drops_the_flag(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", "gpuNode.nodeSelector=null")
        args = find(items, "Deployment", "kukie-oven")["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertFalse([a for a in args if a.startswith("--gpu-node-selector")])

    def test_crd_survives_uninstall(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        crd = find(items, "CustomResourceDefinition", "servedmodels.serving.kukie.io")
        self.assertEqual(crd["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")

    def test_llm_namespace_survives_uninstall_only_with_the_oven(self):
        on = find(docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x"), "Namespace", "llm")
        self.assertEqual(on["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertNotIn("annotations", find(docs(), "Namespace", "llm")["metadata"])

    def test_rendered_rules_match_the_generated_role(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        generated = yaml.safe_load((ROOT / "oven/config/rbac/role.yaml").read_text())
        self.assertEqual(find(items, "ClusterRole", "kukie-oven")["rules"], generated["rules"])

    def test_the_log_permission_is_get_only(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        rules = [r for r in find(items, "ClusterRole", "kukie-oven")["rules"] if "pods/log" in r["resources"]]
        self.assertEqual(rules, [{"apiGroups": [""], "resources": ["pods/log"], "verbs": ["get"]}])

    def test_crd_has_the_download_progress_fields(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        crd = find(items, "CustomResourceDefinition", "servedmodels.serving.kukie.io")
        schema = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]
        download = schema["status"]["properties"]["download"]
        self.assertEqual(download["required"], ["percent"])
        for field in ["downloadedBytes", "totalBytes"]:
            self.assertEqual(download["properties"][field]["format"], "int64")

    def test_chart_copies_match_the_generated_files(self):
        for source in [
            "oven/config/crd/bases/serving.kukie.io_servedmodels.yaml",
            "oven/config/rbac/role.yaml",
            "oven/config/rbac/leader_election_role.yaml",
        ]:
            path = ROOT / source
            self.assertEqual((CHART / "files/oven" / path.name).read_text(), path.read_text(), f"make -C oven manifests: {source}")


class SetImageTest(unittest.TestCase):
    TEXT = 'dcgm:\n  image:\n    tag: ""\n    digest: ""\n\noven:\n  image:\n    tag: ""\n    digest: ""\n'

    def test_changes_only_the_oven_block(self):
        out = set_image(self.TEXT, "0.3.0", DIGEST)
        self.assertEqual(yaml.safe_load(out)["dcgm"]["image"], {"tag": "", "digest": ""})
        self.assertEqual(yaml.safe_load(out)["oven"]["image"], {"tag": "0.3.0", "digest": DIGEST})

    def test_rejects_a_bad_digest(self):
        with self.assertRaises(ValueError):
            set_image(self.TEXT, "0.3.0", "latest")

    def test_works_on_the_real_values(self):
        out = set_image((CHART / "values.yaml").read_text(encoding="utf-8"), "0.3.0", DIGEST)
        self.assertEqual(yaml.safe_load(out)["oven"]["image"]["digest"], DIGEST)


if __name__ == "__main__":
    unittest.main()
