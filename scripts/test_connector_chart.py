"""차트의 connector 스위치, 값 검사, 카운터 키 자동 생성, 권한을 본다. helm만 쓰고 클러스터는 건드리지 않는다."""
import base64
import hashlib
import json
import pathlib
import subprocess
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
CHART = ROOT / "charts" / "kukie-gpu"
CLUSTER_ID = "01a19c31-1518-7a93-b6da-4340bfe8da0f"
GATEWAY = "https://kukie.example/connector/v1/connect"


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


def enabled_args(*extra):
    return ["--set", "counter.enabled=true", "--set", "counter.image.tag=x", "--set", "counter.discovery.enabled=true",
            "--set", "connector.enabled=true", "--set", "connector.image.tag=x",
            "--set", f"connector.gateway={GATEWAY}", "--set", f"connector.clusterId={CLUSTER_ID}", *extra]


def find(items, kind, name):
    return next(d for d in items if d["kind"] == kind and d["metadata"]["name"] == name)


def secret_text(items, name, key):
    return base64.b64decode(find(items, "Secret", name)["data"][key]).decode()


class OffTest(unittest.TestCase):
    def test_off_by_default_renders_nothing(self):
        self.assertFalse([d for d in docs() if "connector" in d["metadata"]["name"]])

    def test_nil_values_render_the_same_as_off(self):
        self.assertEqual(render("--set", "connector=null").stdout, render().stdout)


class EnabledTest(unittest.TestCase):
    def test_renders_these_objects_in_the_release_namespace(self):
        items = docs(*enabled_args())
        names = {(d["kind"], d["metadata"]["name"]) for d in items
                 if "connector" in d["metadata"]["name"] or d["metadata"]["name"] == "kukie-counter-keys"}
        self.assertEqual(names, {
            ("ServiceAccount", "kukie-connector"), ("Role", "kukie-connector-credential"),
            ("RoleBinding", "kukie-connector-credential"), ("Deployment", "kukie-connector"),
            ("Secret", "kukie-connector-counter-key"), ("Secret", "kukie-counter-keys"),
        })

    def test_the_counter_key_matches_the_hash_the_counter_reads(self):
        items = docs(*enabled_args())
        key = secret_text(items, "kukie-connector-counter-key", "counter-key")
        keys = json.loads(secret_text(items, "kukie-counter-keys", "keys.json"))
        self.assertTrue(key.startswith("kk-"))
        self.assertEqual(keys, {"keys": [{"id": "kukie-connector", "name": "Kukie connector",
                                          "sha256": hashlib.sha256(key.encode()).hexdigest(), "disabled": False}]})
        counter = find(items, "Deployment", "kukie-counter")["spec"]["template"]["spec"]
        self.assertEqual(next(v for v in counter["volumes"] if v["name"] == "keys")["secret"]["secretName"],
                         "kukie-counter-keys")

    def test_the_credential_never_comes_from_values(self):
        items = docs(*enabled_args())
        names = {d["metadata"]["name"] for d in items if d["kind"] == "Secret"}
        self.assertNotIn("kukie-connector", names)

    def test_args_point_at_the_counter_in_the_same_namespace(self):
        container = find(docs(*enabled_args()), "Deployment", "kukie-connector")["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["args"], [
            f"--gateway={GATEWAY}", f"--cluster-id={CLUSTER_ID}",
            "--credential-file=/etc/kukie-connector/credential",
            "--counter-url=http://kukie-counter.kukie-gpu.svc:8080/v1",
            "--counter-credential-file=/etc/kukie-connector/counter-key",
            "--workers=4", "--credential-secret=kukie-connector", "--credential-secret-key=credential",
        ])
        self.assertEqual(container["image"], "ghcr.io/durokukie/kukie-connector:x")

    def test_the_role_reaches_only_the_credential_secret(self):
        role = find(docs(*enabled_args()), "Role", "kukie-connector-credential")
        self.assertEqual(role["rules"], [{"apiGroups": [""], "resources": ["secrets"],
                                         "resourceNames": ["kukie-connector"], "verbs": ["get", "update"]}])

    def test_pod_runs_without_root_and_mounts_without_subpath(self):
        spec = find(docs(*enabled_args()), "Deployment", "kukie-connector")["spec"]["template"]["spec"]
        self.assertTrue(spec["securityContext"]["runAsNonRoot"])
        self.assertTrue(spec["automountServiceAccountToken"])
        mount = spec["containers"][0]["volumeMounts"][0]
        self.assertNotIn("subPath", mount)

    def test_own_counter_key_secret_leaves_secrets_to_the_user(self):
        items = docs(*enabled_args("--set", "counter.keysSecret=my-keys", "--set", "connector.counterKeySecret=my-key"))
        self.assertNotIn("Secret", {d["kind"] for d in items})
        volume = find(items, "Deployment", "kukie-connector")["spec"]["template"]["spec"]["volumes"][0]
        self.assertEqual(volume["projected"]["sources"][1]["secret"]["name"], "my-key")


class ValidateTest(unittest.TestCase):
    def assert_fails(self, needle, *args):
        result = render(*args)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(needle, result.stderr)
        return result.stderr

    def test_needs_the_counter(self):
        self.assert_fails("counter.enabled=true", "--set", "connector.enabled=true")

    def test_keys_are_either_both_generated_or_both_given(self):
        self.assert_fails("connector.counterKeySecret", *enabled_args("--set", "counter.keysSecret=my-keys"))

    def test_gateway_and_cluster_id_shapes(self):
        self.assert_fails("connector.gateway", *enabled_args("--set", "connector.gateway=http://kukie.example/x"))
        self.assert_fails("connector.clusterId", *enabled_args("--set", "connector.clusterId=prod"))

    def test_a_secret_value_in_the_name_is_rejected_without_echoing_it(self):
        stderr = self.assert_fails("connector.credentialSecret",
                                   *enabled_args("--set", "connector.credentialSecret=kk-AbC_secret"))
        self.assertNotIn("kk-AbC_secret", stderr)

    def test_workers_range_and_unknown_values(self):
        self.assert_fails("connector.workers", *enabled_args("--set", "connector.workers=9"))
        self.assert_fails("connector.token", *enabled_args("--set", "connector.token=x"))


if __name__ == "__main__":
    unittest.main()
