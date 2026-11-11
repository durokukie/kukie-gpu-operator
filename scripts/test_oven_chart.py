"""차트의 oven 스위치와, files/oven 복사본이 oven/config 생성물과 같은지 본다. helm만 쓰고 클러스터는 건드리지 않는다."""
import json
import pathlib
import subprocess
import unittest

import yaml

from set_image import set_image

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

    def generated_rules(self):
        generated = {d["kind"]: d for d in yaml.safe_load_all((ROOT / "oven/config/rbac/role.yaml").read_text()) if d}
        self.assertEqual(set(generated), {"ClusterRole", "Role"})
        return generated["ClusterRole"]["rules"], generated["Role"]["rules"]

    def oven_roles(self, items):
        return {d["metadata"]["namespace"]: d for d in items if d["kind"] == "Role" and d["metadata"]["name"] == "kukie-oven"}

    def test_the_oven_creates_the_llm_namespace_its_role_goes_in(self):
        # Without rbac or the demo, only the oven needs llm. Its Role there would fail with "namespaces not found".
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", "rbac.enabled=false",
                     "--set", "demoVllm.enabled=false")
        llm = find(items, "Namespace", "llm")
        self.assertEqual(llm["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertEqual(set(self.oven_roles(items)), {"llm"})
        off = docs("--set", "rbac.enabled=false", "--set", "demoVllm.enabled=false")
        self.assertFalse([d for d in off if d["kind"] == "Namespace" and d["metadata"]["name"] == "llm"])

    def test_rendered_rules_match_the_generated_role(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        cluster_rules, namespaced_rules = self.generated_rules()
        self.assertEqual(find(items, "ClusterRole", "kukie-oven")["rules"], cluster_rules)
        roles = self.oven_roles(items)
        self.assertEqual(set(roles), {"llm"})
        self.assertEqual(roles["llm"]["rules"], namespaced_rules)

    def test_cluster_wide_the_oven_only_gets_nodes(self):
        # Nodes: one read of the measured node's GPU type label. No list or watch: the manager reads Nodes without a cache.
        # Everything else, writes included, is in the per-namespace Role (AGENTS.md: 2·3층은 namespace 안의 쓰기만).
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        self.assertEqual(find(items, "ClusterRole", "kukie-oven")["rules"],
                         [{"apiGroups": [""], "resources": ["nodes"], "verbs": ["get"]}])

    def test_the_events_permission_is_list_only_and_namespaced(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        rules = self.oven_roles(items)["llm"]["rules"]
        self.assertEqual([r for r in rules if "events" in r["resources"]],
                         [{"apiGroups": [""], "resources": ["events"], "verbs": ["list"]}])
        self.assertFalse([r for r in find(items, "ClusterRole", "kukie-oven")["rules"] if "events" in r["resources"]])

    def test_the_log_permission_is_get_only(self):
        rules = self.oven_roles(docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x"))["llm"]["rules"]
        self.assertEqual([r for r in rules if "pods/log" in r["resources"]],
                         [{"apiGroups": [""], "resources": ["pods/log"], "verbs": ["get"]}])

    def test_the_role_grants_no_more_than_the_old_cluster_role(self):
        # What 0.12.0 granted cluster-wide. Splitting it must not add a verb or a resource anywhere.
        before = {
            ("", "configmaps"): {"create", "delete", "get", "list", "patch"},
            ("", "nodes"): {"get"}, ("", "pods/log"): {"get"},
            ("", "persistentvolumeclaims"): {"create", "get", "list", "patch", "watch"},
            ("", "services"): {"create", "get", "list", "patch", "watch"},
            ("", "pods"): {"get", "list", "watch"},
            ("apps", "deployments"): {"create", "get", "list", "patch", "watch"},
            ("batch", "jobs"): {"create", "delete", "get", "list", "patch", "watch"},
            ("serving.kukie.io", "servedmodels"): {"get", "list", "watch"},
            ("serving.kukie.io", "servedmodels/finalizers"): {"update"},
            ("serving.kukie.io", "servedmodels/status"): {"get", "patch", "update"},
            # 0.14.0 (DURO-171): the failure pattern table reads a failed Pod's Warning events. List only, in the namespace Role.
            ("", "events"): {"list"},
        }
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        after = {}
        for rule in find(items, "ClusterRole", "kukie-oven")["rules"] + self.oven_roles(items)["llm"]["rules"]:
            for group in rule["apiGroups"]:
                for resource in rule["resources"]:
                    after.setdefault((group, resource), set()).update(rule["verbs"])
        self.assertEqual(after, before)

    def test_the_watched_namespaces_default_to_the_llm_namespace(self):
        self.assertIn("--watch-namespaces=llm", self.manager_args())
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", "namespaces.llm=models")
        self.assertEqual(set(self.oven_roles(items)), {"models"})
        args = find(items, "Deployment", "kukie-oven")["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertIn("--watch-namespaces=models", args)

    def test_each_listed_namespace_gets_a_role_and_a_binding(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", "oven.namespaces={llm,team-a,llm}")
        _, namespaced_rules = self.generated_rules()
        roles = self.oven_roles(items)
        self.assertEqual(set(roles), {"llm", "team-a"})
        for ns, role in roles.items():
            self.assertEqual(role["rules"], namespaced_rules)
            binding = next(d for d in items if d["kind"] == "RoleBinding" and d["metadata"]["name"] == "kukie-oven"
                           and d["metadata"]["namespace"] == ns)
            self.assertEqual(binding["roleRef"], {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "kukie-oven"})
            self.assertEqual(binding["subjects"], [{"kind": "ServiceAccount", "name": "kukie-oven", "namespace": "kukie-gpu"}])
        args = find(items, "Deployment", "kukie-oven")["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertIn("--watch-namespaces=llm,team-a", args)

    def test_null_namespaces_fall_back_to_the_llm_namespace(self):
        self.assertIn("--watch-namespaces=llm", self.manager_args("--set", "oven.namespaces=null"))

    def test_bad_namespaces_fail_the_render(self):
        for value in ["oven.namespaces=llm", "oven.namespaces={Team}", "oven.namespaces={a_b}", "oven.namespaces={-a}",
                      "oven.namespaces={" + "a" * 64 + "}"]:
            result = render("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", value)
            self.assertNotEqual(result.returncode, 0, value)
            self.assertIn("oven.namespaces", result.stderr, value)

    def test_an_upgrade_with_the_old_values_drops_the_cluster_wide_writes(self):
        # helm upgrade --reset-then-reuse-values: this chart's defaults under the values a 0.12.0 install was given, which
        # have no oven.namespaces. The ClusterRole keeps its name, so the upgrade replaces its rules in place.
        old_values = {"oven": {"enabled": True, "image": {"tag": "0.10.0", "digest": DIGEST},
                               "imagePullSecrets": [{"name": "ghcr-pull"}]}}
        items = docs("--set-json", "oven=" + json.dumps(old_values["oven"]))
        self.assertEqual(find(items, "ClusterRole", "kukie-oven")["rules"],
                         [{"apiGroups": [""], "resources": ["nodes"], "verbs": ["get"]}])
        self.assertEqual(set(self.oven_roles(items)), {"llm"})

    def test_crd_has_the_download_progress_fields(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        crd = find(items, "CustomResourceDefinition", "servedmodels.serving.kukie.io")
        schema = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]
        download = schema["status"]["properties"]["download"]
        self.assertEqual(download["required"], ["percent"])
        for field in ["downloadedBytes", "totalBytes"]:
            self.assertEqual(download["properties"][field]["format"], "int64")

    def test_crd_has_the_measure_request_field(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        crd = find(items, "CustomResourceDefinition", "servedmodels.serving.kukie.io")
        schema = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]
        self.assertEqual(schema["status"]["properties"]["measureRequest"]["type"], "string")

    def test_crd_has_the_reused_baseline_field(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        crd = find(items, "CustomResourceDefinition", "servedmodels.serving.kukie.io")
        schema = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["properties"]
        self.assertEqual(schema["status"]["properties"]["baseline"]["properties"]["reusedFrom"]["type"], "string")

    def test_reuse_values_reach_the_manager_flags(self):
        defaults = self.manager_args()
        self.assertIn("--measure-reuse-max-age=720h", defaults)
        self.assertIn("--gpu-type-labels=nvidia.com/gpu.product,node.kubernetes.io/instance-type", defaults)
        args = self.manager_args("--set", "oven.measure.reuseMaxAge=0", "--set-json", 'oven.measure.gpuTypeLabels=["a.io/gpu"]')
        self.assertIn("--measure-reuse-max-age=0", args)
        self.assertIn("--gpu-type-labels=a.io/gpu", args)

    def test_bad_reuse_age_fails_the_render(self):
        result = render("--set", "oven.enabled=true", "--set", "oven.image.tag=x", "--set", "oven.measure.reuseMaxAge=30d")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("oven.measure.reuseMaxAge", result.stderr)

    def test_configmap_rules_are_what_the_measurement_results_need(self):
        items = docs("--set", "oven.enabled=true", "--set", "oven.image.tag=x")
        rules = [r for r in self.oven_roles(items)["llm"]["rules"] if "configmaps" in r["resources"]]
        self.assertEqual(rules, [{
            "apiGroups": [""],
            "resources": ["configmaps"],
            # list and delete keep the last 20 results. No watch: the manager reads ConfigMaps without a cache.
            "verbs": ["create", "delete", "get", "list", "patch"],
        }])

    def test_chart_copies_match_the_generated_files(self):
        for source in [
            "oven/config/crd/bases/serving.kukie.io_servedmodels.yaml",
            "oven/config/rbac/role.yaml",
            "oven/config/rbac/leader_election_role.yaml",
        ]:
            path = ROOT / source
            self.assertEqual((CHART / "files/oven" / path.name).read_text(), path.read_text(), f"make -C oven manifests: {source}")

    def test_failure_patterns_config_map(self):
        """패턴표 ConfigMap은 기본 표 그대로이고, extra는 앞에 붙는다. 오븐은 그 파일을 마운트해 읽는다."""
        base = ["--set", "oven.enabled=true", "--set", "oven.image.tag=0.3.0"]
        table = yaml.safe_load((CHART / "files" / "oven" / "failure-patterns.yaml").read_text())["patterns"]

        def rendered(*args):
            items = docs(*base, *args)
            data = find(items, "ConfigMap", "kukie-oven-failure-patterns")["data"]["patterns.yaml"]
            return yaml.safe_load(data)["patterns"], find(items, "Deployment", "kukie-oven")["spec"]["template"]["spec"]

        rows, spec = rendered()
        self.assertEqual(rows, table)
        args = spec["containers"][0]["args"]
        self.assertIn("--failure-patterns=/etc/kukie-oven/failure-patterns/patterns.yaml", args)
        self.assertIn("--still-loading-after=15m", args)
        self.assertEqual(spec["volumes"], [{"name": "failure-patterns", "configMap": {"name": "kukie-oven-failure-patterns"}}])
        self.assertEqual(spec["containers"][0]["volumeMounts"][0]["mountPath"], "/etc/kukie-oven/failure-patterns")

        extra = {"reason": "NcclTimeout", "stage": "engine", "in": ["log"], "match": "NCCL.*timed out", "message": "m", "hint": "h"}
        rows, _ = rendered("--set-json", "oven.failurePatterns.extra=" + json.dumps([extra]))
        self.assertEqual(rows, [extra] + table)

        # 0 turns it off; helm's default would have made it 15m (review of #44).
        _, spec = rendered("--set", "oven.failurePatterns.stillLoadingAfter=0")
        self.assertIn("--still-loading-after=0", spec["containers"][0]["args"])

        bad = render(*base, "--set", "oven.failurePatterns.stillLoadingAfter=soon")
        self.assertNotEqual(bad.returncode, 0)
        self.assertIn("oven.failurePatterns.stillLoadingAfter", bad.stderr)


class SetImageTest(unittest.TestCase):
    TEXT = 'dcgm:\n  image:\n    tag: ""\n    digest: ""\n\noven:\n  image:\n    tag: ""\n    digest: ""\n'

    def test_changes_only_the_oven_block(self):
        out = set_image(self.TEXT, "oven", "0.3.0", DIGEST)
        self.assertEqual(yaml.safe_load(out)["dcgm"]["image"], {"tag": "", "digest": ""})
        self.assertEqual(yaml.safe_load(out)["oven"]["image"], {"tag": "0.3.0", "digest": DIGEST})

    def test_rejects_a_bad_digest(self):
        with self.assertRaises(ValueError):
            set_image(self.TEXT, "oven", "0.3.0", "latest")

    def test_works_on_the_real_values(self):
        out = set_image((CHART / "values.yaml").read_text(encoding="utf-8"), "oven", "0.3.0", DIGEST)
        self.assertEqual(yaml.safe_load(out)["oven"]["image"]["digest"], DIGEST)


if __name__ == "__main__":
    unittest.main()
