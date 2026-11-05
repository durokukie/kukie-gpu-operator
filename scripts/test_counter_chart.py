"""차트의 counter 스위치, 값 검사, nil 값, 배치와 권한을 본다. helm만 쓰고 클러스터는 건드리지 않는다."""
import json
import pathlib
import shutil
import subprocess
import tempfile
import unittest

import yaml

from set_image import set_image

ROOT = pathlib.Path(__file__).resolve().parents[1]
CHART = ROOT / "charts" / "kukie-gpu"
DIGEST = "sha256:" + "b" * 64
MODELS = {"org/model.v1": {"upstream": "http://model.llm.svc:8000", "maxConcurrent": 8}}
REUSE_HINT = "--reset-then-reuse-values"


def render(*args, chart=CHART):
    return subprocess.run(
        ["helm", "template", "kukie-gpu", str(chart), "--namespace", "kukie-gpu", *args],
        capture_output=True, text=True, timeout=60,
    )


def docs(*args, chart=CHART):
    result = render(*args, chart=chart)
    if result.returncode != 0:
        raise AssertionError(result.stderr)
    return [d for d in yaml.safe_load_all(result.stdout) if d]


def enabled_args(models=None):
    return ["--set", "counter.enabled=true", "--set", "counter.image.tag=x", "--set", "counter.keysSecret=my-keys",
            "--set-json", "counter.models=" + json.dumps(MODELS if models is None else models)]


def counter_objects(items):
    return {(d["kind"], d["metadata"]["name"]) for d in items if "counter" in d["metadata"]["name"]}


def find(items, kind):
    return next(d for d in items if d["kind"] == kind and d["metadata"]["name"] == "kukie-counter")


def pod_spec(*args):
    return find(docs(*enabled_args(), *args), "Deployment")["spec"]["template"]["spec"]


class _ChartWithoutCounter:
    """The values helm sees on --reuse-values: the old release's, with no counter block."""
    path = None


def setUpModule():
    _ChartWithoutCounter.path = pathlib.Path(tempfile.mkdtemp(prefix="counter-chart-")) / "kukie-gpu"
    shutil.copytree(CHART, _ChartWithoutCounter.path)
    values = yaml.safe_load((CHART / "values.yaml").read_text(encoding="utf-8"))
    del values["counter"]
    (_ChartWithoutCounter.path / "values.yaml").write_text(yaml.safe_dump(values), encoding="utf-8")


def tearDownModule():
    shutil.rmtree(_ChartWithoutCounter.path.parent)


class OffTest(unittest.TestCase):
    def test_off_by_default_renders_nothing(self):
        self.assertEqual(counter_objects(docs()), set())

    def test_nil_values_render_the_same_as_off(self):
        # helm upgrade --reuse-values from a chart without this block leaves .Values.counter nil.
        self.assertEqual(render("--set", "counter=null").stdout, render().stdout)

    def test_a_chart_without_the_block_still_renders(self):
        self.assertEqual(render(chart=_ChartWithoutCounter.path).stdout, render().stdout)

    def test_off_does_not_look_at_the_other_values(self):
        self.assertEqual(counter_objects(docs("--set", "counter.maxBodyBytes=nonsense")), set())


class ReuseValuesTest(unittest.TestCase):
    def test_turning_it_on_tells_what_to_use_instead_of_a_nil_pointer(self):
        result = render("--set", "counter.enabled=true", chart=_ChartWithoutCounter.path)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(REUSE_HINT, result.stderr)
        self.assertNotIn("nil pointer", result.stderr)

    def test_a_values_file_without_the_new_defaults_is_stopped_too(self):
        result = render(*enabled_args(), "--set", "counter.image.repository=durokukie/kukie-counter",
                        chart=_ChartWithoutCounter.path)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(REUSE_HINT, result.stderr)
        self.assertNotIn("nil pointer", result.stderr)


class EnabledTest(unittest.TestCase):
    def test_renders_exactly_these_objects_in_the_release_namespace(self):
        items = [d for d in docs(*enabled_args()) if "counter" in d["metadata"]["name"]]
        self.assertEqual({(d["kind"], d["metadata"]["name"]) for d in items}, {
            ("ServiceAccount", "kukie-counter"), ("ConfigMap", "kukie-counter"),
            ("Deployment", "kukie-counter"), ("Service", "kukie-counter"),
        })
        for item in items:
            self.assertEqual(item["metadata"]["namespace"], "kukie-gpu")

    def test_the_chart_never_makes_a_secret_or_an_exposure(self):
        kinds = {d["kind"] for d in docs(*enabled_args())}
        self.assertNotIn("Secret", kinds)
        self.assertNotIn("Ingress", kinds)
        self.assertNotIn("PodDisruptionBudget", kinds)

    def test_config_is_exactly_what_the_counter_reads(self):
        config = json.loads(find(docs(*enabled_args()), "ConfigMap")["data"]["config.json"])
        self.assertEqual(config, {"models": MODELS, "retryAfterSeconds": 2, "maxBodyBytes": 16777216})

    def test_big_numbers_are_not_written_in_exponent_form(self):
        # Values files give float64 and the counter decodes into int64: 1.6777216e+07 would stop it from starting.
        with tempfile.NamedTemporaryFile("w", prefix="counter-chart-", suffix=".yaml") as values:
            yaml.safe_dump({"counter": {"maxBodyBytes": 33554432, "models": {"m": {"upstream": "http://m:8000", "maxConcurrent": 64}}}}, values)
            values.flush()
            args = ["--set", "counter.enabled=true", "--set", "counter.image.tag=x", "--set", "counter.keysSecret=k"]
            text = find(docs(*args, "-f", values.name), "ConfigMap")["data"]["config.json"]
        self.assertIn('"maxBodyBytes": 33554432', text)
        self.assertIn('"maxConcurrent": 64', text)

    def test_values_reach_the_config(self):
        args = enabled_args() + ["--set", "counter.retryAfterSeconds=5", "--set", "counter.maxBodyBytes=1024"]
        config = json.loads(find(docs(*args), "ConfigMap")["data"]["config.json"])
        self.assertEqual((config["retryAfterSeconds"], config["maxBodyBytes"]), (5, 1024))

    def test_image_keys_are_separate_from_the_oven(self):
        spec = pod_spec("--set", f"counter.image.digest={DIGEST}", "--set", "oven.image.repository=other/oven")
        self.assertEqual(spec["containers"][0]["image"], f"ghcr.io/durokukie/kukie-counter:x@{DIGEST}")

    def test_private_mirror_applies(self):
        spec = pod_spec("--set", "image.registry=mirror.example")
        self.assertEqual(spec["containers"][0]["image"], "mirror.example/durokukie/kukie-counter:x")

    def test_pull_secrets_are_passed(self):
        spec = pod_spec("--set", "counter.imagePullSecrets[0].name=pull")
        self.assertEqual(spec["imagePullSecrets"], [{"name": "pull"}])

    def test_config_change_restarts_the_pod_and_nothing_else_does(self):
        def checksum(*args):
            deployment = find(docs(*enabled_args(), *args), "Deployment")
            return deployment["spec"]["template"]["metadata"]["annotations"]["checksum/config"]
        self.assertNotEqual(checksum(), checksum("--set", "counter.retryAfterSeconds=9"))
        self.assertEqual(checksum(), checksum("--set", "counter.keysSecret=other-keys"))

    def test_one_replica(self):
        self.assertEqual(find(docs(*enabled_args()), "Deployment")["spec"]["replicas"], 1)

    def test_no_token_and_the_pod_security_restricted_shape(self):
        spec = pod_spec()
        self.assertIs(spec["automountServiceAccountToken"], False)
        self.assertIs(find(docs(*enabled_args()), "ServiceAccount")["automountServiceAccountToken"], False)
        self.assertEqual(spec["securityContext"]["seccompProfile"], {"type": "RuntimeDefault"})
        self.assertIs(spec["securityContext"]["runAsNonRoot"], True)
        container = spec["containers"][0]["securityContext"]
        self.assertIs(container["readOnlyRootFilesystem"], True)
        self.assertIs(container["allowPrivilegeEscalation"], False)
        self.assertEqual(container["capabilities"], {"drop": ["ALL"]})

    def test_flags_and_mounts_point_at_the_same_files(self):
        spec = pod_spec()
        container = spec["containers"][0]
        self.assertEqual(container["args"][0], "serve")
        mounts = {m["name"]: m for m in container["volumeMounts"]}
        self.assertIn(f"--config={mounts['config']['mountPath']}/config.json", container["args"])
        self.assertIn(f"--keys={mounts['keys']['mountPath']}/keys.json", container["args"])
        volumes = {v["name"]: v for v in spec["volumes"]}
        self.assertEqual(volumes["keys"]["secret"]["secretName"], "my-keys")
        self.assertEqual(volumes["keys"]["secret"]["items"], [{"key": "keys.json", "path": "keys.json"}])

    def test_keys_are_not_mounted_with_subpath(self):
        # A subPath mount never sees Secret updates, so a revoked key would keep working until a restart.
        for mount in pod_spec()["containers"][0]["volumeMounts"]:
            self.assertNotIn("subPath", mount)

    def test_grace_period_is_longer_than_the_shutdown_timeout(self):
        spec = pod_spec()
        self.assertIn("--shutdown-delay=5s", spec["containers"][0]["args"])
        self.assertIn("--shutdown-timeout=25s", spec["containers"][0]["args"])
        self.assertGreater(spec["terminationGracePeriodSeconds"], 5 + 25)

    def test_probes_use_healthz(self):
        container = pod_spec()["containers"][0]
        for probe in ["livenessProbe", "readinessProbe"]:
            self.assertEqual(container[probe]["httpGet"], {"path": "/healthz", "port": "http"})

    def test_service_is_cluster_ip_and_has_only_the_api_port(self):
        spec = find(docs(*enabled_args()), "Service")["spec"]
        self.assertEqual(spec["type"], "ClusterIP")
        self.assertEqual([(p["name"], p["port"]) for p in spec["ports"]], [("http", 8080)])
        ports = {p["name"]: p["containerPort"] for p in pod_spec()["containers"][0]["ports"]}
        self.assertEqual(ports, {"http": 8080, "metrics": 9090})


class PlacementTest(unittest.TestCase):
    def test_does_not_tolerate_the_gpu_taint_by_default(self):
        spec = pod_spec()
        self.assertNotIn("tolerations", spec)
        self.assertNotIn("nodeSelector", spec)

    def test_avoids_the_gpu_nodes_even_when_they_have_no_taint(self):
        terms = pod_spec()["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        self.assertEqual(terms, [{"matchExpressions": [
            {"key": "kukie.io/accelerator", "operator": "NotIn", "values": ["nvidia-l40s"]},
        ]}])

    def test_several_gpu_labels_mean_not_all_of_them(self):
        spec = pod_spec("--set", "gpuNode.nodeSelector.other=yes")
        terms = spec["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        self.assertEqual(len(terms), 2)

    def test_no_gpu_selector_means_no_affinity(self):
        self.assertNotIn("affinity", pod_spec("--set", "gpuNode.nodeSelector=null"))

    def test_can_be_moved_off_any_node(self):
        # No do-not-disrupt, no local data: a node autoscaler may drain it.
        deployment = find(docs(*enabled_args()), "Deployment")
        self.assertNotIn("karpenter.sh/do-not-disrupt", deployment["spec"]["template"]["metadata"]["annotations"])
        self.assertFalse([v for v in deployment["spec"]["template"]["spec"]["volumes"] if "emptyDir" in v or "hostPath" in v])

    def test_own_placement_values_are_used(self):
        spec = pod_spec("--set", "counter.nodeSelector.pool=system",
                        "--set-json", 'counter.tolerations=[{"key":"system","operator":"Exists"}]')
        self.assertEqual(spec["nodeSelector"], {"pool": "system"})
        self.assertEqual(spec["tolerations"], [{"key": "system", "operator": "Exists"}])


class FailTest(unittest.TestCase):
    def assert_fails(self, needle, *args):
        result = render(*args)
        self.assertNotEqual(result.returncode, 0, args)
        self.assertIn(needle, result.stderr, args)
        return result.stderr

    def test_missing_image(self):
        self.assert_fails("counter.image.tag", "--set", "counter.enabled=true", "--set", "counter.keysSecret=k",
                          "--set-json", "counter.models=" + json.dumps(MODELS))

    def test_missing_keys_secret(self):
        self.assert_fails("counter.keysSecret", "--set", "counter.enabled=true", "--set", "counter.image.tag=x",
                          "--set-json", "counter.models=" + json.dumps(MODELS))

    def test_a_key_put_where_the_name_goes_fails_without_echoing_it(self):
        key = "kk-SomeSecretValue_123"
        stderr = self.assert_fails("counter.keysSecret", *enabled_args(), "--set", f"counter.keysSecret={key}")
        self.assertNotIn(key, stderr)

    def test_there_is_no_path_for_keys_in_values(self):
        for name in ["keys", "key", "keysJson", "apiKeys"]:
            self.assert_fails(f"counter.{name}", *enabled_args(), "--set", f"counter.{name}=abc")

    def test_replicas_is_refused_with_the_reason(self):
        self.assert_fails("counter.replicas", *enabled_args(), "--set", "counter.replicas=2")

    def test_misspelled_top_level_value(self):
        self.assert_fails("counter.maxBodyByte", *enabled_args(), "--set", "counter.maxBodyByte=1024")

    def test_empty_models(self):
        self.assert_fails("counter.models", *enabled_args({}))

    def test_bad_model_names(self):
        self.assert_fails("unknown", *enabled_args({"unknown": MODELS["org/model.v1"]}))

    def test_model_shape(self):
        ok = "http://m:8000"
        self.assert_fails('counter.models["m"]', *enabled_args({"m": "http://m:8000"}))
        self.assert_fails('counter.models["m"].maxConcurrnt',
                          *enabled_args({"m": {"upstream": ok, "maxConcurrent": 1, "maxConcurrnt": 2}}))

    def test_bad_upstreams_fail_without_echoing_them(self):
        for upstream in ["", "m:8000", "ftp://m", "http://", "http://user:secret@m:8000", "http://m:8000/?token=secret",
                         "http://m:8000/#secret", "http://m:port", 8000]:
            stderr = self.assert_fails("upstream", *enabled_args({"m": {"upstream": upstream, "maxConcurrent": 1}}))
            if "secret" in str(upstream):
                self.assertNotIn("secret", stderr, upstream)

    def test_good_upstreams_pass(self):
        for upstream in ["http://m.llm.svc:8000", "https://m.example", "http://10.0.0.1:8000/base", "http://[::1]:8000"]:
            docs(*enabled_args({"m": {"upstream": upstream, "maxConcurrent": 1}}))

    def test_bad_max_concurrent(self):
        for value in [0, -1, 1.5, "8", None]:
            self.assert_fails("maxConcurrent", *enabled_args({"m": {"upstream": "http://m:8000", "maxConcurrent": value}}))

    def test_bad_numbers(self):
        for name in ["retryAfterSeconds", "maxBodyBytes"]:
            for value in ["0", "-1", "1.5"]:
                self.assert_fails(f"counter.{name}", *enabled_args(), "--set", f"counter.{name}={value}")
            self.assert_fails(f"counter.{name}", *enabled_args(), "--set-string", f"counter.{name}=8")
            self.assert_fails(f"counter.{name}", *enabled_args(), "--set", f"counter.{name}=null")


class SetImageTest(unittest.TestCase):
    TEXT = 'oven:\n  image:\n    tag: ""\n    digest: ""\n\n# 주석\ncounter:\n  image:\n    tag: ""\n    digest: ""\n'
    OVEN = "sha256:" + "a" * 64

    def test_fills_each_block_alone(self):
        out = yaml.safe_load(set_image(self.TEXT, "counter", "0.7.0", DIGEST))
        self.assertEqual(out["counter"]["image"], {"tag": "0.7.0", "digest": DIGEST})
        self.assertEqual(out["oven"]["image"], {"tag": "", "digest": ""})

    def test_a_filled_block_does_not_spill_into_the_next_one(self):
        once = set_image(self.TEXT, "oven", "0.7.0", self.OVEN)
        with self.assertRaises(ValueError):
            set_image(once, "oven", "0.7.0", self.OVEN)
        self.assertEqual(yaml.safe_load(once)["counter"]["image"]["digest"], "")

    def test_unknown_block(self):
        with self.assertRaises(ValueError):
            set_image(self.TEXT, "nope", "0.7.0", DIGEST)

    def test_both_on_the_real_values_keep_the_comments(self):
        text = (CHART / "values.yaml").read_text(encoding="utf-8")
        out = set_image(set_image(text, "oven", "0.7.0", self.OVEN), "counter", "0.7.0", DIGEST)
        values = yaml.safe_load(out)
        self.assertEqual(values["oven"]["image"]["digest"], self.OVEN)
        self.assertEqual(values["counter"]["image"]["digest"], DIGEST)
        self.assertEqual(len(out.splitlines()), len(text.splitlines()))


if __name__ == "__main__":
    unittest.main()
