#!/usr/bin/env python3
"""Check that the kukie-gpu chart renders the same objects as the hand-applied YAML under kubernetes/.

The chart is the source of truth from now on; the YAML files stay until the next bundle so this script can prove the
chart did not change anything (image digests, labels, selectors, resource requests, node scheduling). CI runs it on
every PR (terraform-plan.yml "Chart checks").

  python3 scripts/chart_diff.py            # exit 1 on any difference
  python3 scripts/chart_diff.py --verbose  # also list objects that matched
  python3 scripts/chart_diff.py --set dcgm.resources.requests.cpu=60m   # must fail: proves the check sees a change

Compared: every object in kubernetes/**/*.yaml except the one-off check Pod. Each object is looked up in the chart
render by (apiVersion, kind, namespace, name) and compared as parsed data, so comments, key order and quoting do not
matter. Objects the chart adds on top (RoleBindings, the DCGM Service, the helm test Pod) are allowed only when listed
in EXTRA_ALLOWED; anything else the chart renders is an error too, so a new template cannot slip in unnoticed.
"""
import argparse
import difflib
import pathlib
import subprocess
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - CI installs PyYAML; the message tells a developer what to do
    sys.exit("PyYAML이 필요합니다: python3 -m pip install pyyaml")

ROOT = pathlib.Path(__file__).resolve().parents[1]
CHART = ROOT / "charts" / "kukie-gpu"
MANIFEST_DIR = ROOT / "kubernetes"
# Hand-applied files that are not part of the chart.
SKIP_FILES = {MANIFEST_DIR / "nvidia" / "check.yaml"}
# Values that turn on every template, so the render covers all hand-applied objects.
RENDER_ARGS = ["--set", "devicePlugin.enabled=true", "--set", "demoVllm.enabled=true"]
# Objects the chart renders that have no hand-applied counterpart: (kind, namespace, name).
EXTRA_ALLOWED = {
    ("RoleBinding", "gpu-metrics", "kukie-gpu-metrics"),   # was `kubectl create rolebinding` in runbooks/gpu-view.md
    ("RoleBinding", "llm", "kukie-gpu-metrics"),
    ("Service", "gpu-metrics", "dcgm-exporter"),            # ClusterIP for `helm test`
    ("Pod", "gpu-metrics", "kukie-gpu-test-dcgm"),          # the helm test hook itself
    # Preflight Job (templates/preflight.yaml): hook Job, its script and result ConfigMaps, and its RBAC.
    # Release namespace is "default" in this render (see rendered()).
    ("Job", "default", "kukie-gpu-preflight"),
    ("ConfigMap", "default", "kukie-gpu-preflight"),
    ("ConfigMap", "default", "kukie-gpu-preflight-script"),
    ("ServiceAccount", "default", "kukie-gpu-preflight"),
    ("Role", "default", "kukie-gpu-preflight"),
    ("RoleBinding", "default", "kukie-gpu-preflight"),
    ("Role", "gpu-metrics", "kukie-gpu-preflight-dcgm"),
    ("RoleBinding", "gpu-metrics", "kukie-gpu-preflight-dcgm"),
    ("ClusterRole", "", "kukie-gpu-preflight"),
    ("ClusterRoleBinding", "", "kukie-gpu-preflight"),
}


def key(doc):
    meta = doc.get("metadata", {})
    return (doc.get("apiVersion", ""), doc.get("kind", ""), meta.get("namespace", ""), meta.get("name", ""))


def load_all(text):
    return {key(d): d for d in yaml.safe_load_all(text) if isinstance(d, dict) and d.get("kind")}


def hand_applied():
    docs = {}
    for path in sorted(MANIFEST_DIR.rglob("*.yaml")):
        if path in SKIP_FILES:
            continue
        docs.update(load_all(path.read_text()))
    return docs


def rendered(extra_sets):
    # --namespace default: without it helm takes the kubeconfig context namespace as .Release.Namespace, and the chart
    # skips creating a Namespace equal to it, so a shell set to gpu-metrics would see a false "missing Namespace".
    command = ["helm", "template", "kukie-gpu", str(CHART), "--namespace", "default", *RENDER_ARGS]
    for item in extra_sets:
        command += ["--set", item]
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if result.returncode != 0:
        sys.exit(f"helm template 실패:\n{result.stderr}")
    return load_all(result.stdout)


def dump(doc):
    return yaml.safe_dump(doc, sort_keys=True, default_flow_style=False, allow_unicode=True).splitlines()


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--verbose", action="store_true", help="list matching objects too")
    parser.add_argument("--set", action="append", default=[], metavar="KEY=VALUE",
                        help="extra helm --set for a manual negative test (CI passes none)")
    args = parser.parse_args()

    expected, actual = hand_applied(), rendered(args.set)
    problems = []
    for k, doc in sorted(expected.items()):
        label = f"{k[1]} {k[2]}/{k[3]}" if k[2] else f"{k[1]} {k[3]}"
        if k not in actual:
            problems.append(f"차트에 없음: {label}")
            continue
        if actual[k] != doc:
            diff = difflib.unified_diff(dump(doc), dump(actual[k]), "kubernetes/ (hand-applied)", "helm template",
                                        lineterm="", n=2)
            problems.append(f"다름: {label}\n" + "\n".join(diff))
        elif args.verbose:
            print(f"같음: {label}")
    for k in sorted(set(actual) - set(expected)):
        if (k[1], k[2], k[3]) not in EXTRA_ALLOWED:
            problems.append(f"차트에만 있음(허용 목록 밖): {k[1]} {k[2]}/{k[3]} — 의도한 추가면 EXTRA_ALLOWED에 넣으세요")

    if problems:
        print("\n\n".join(problems))
        print(f"\n{len(problems)}개 차이. 차트가 원본이니 kubernetes/ YAML이 아니라 templates/를 고치고, "
              "손 apply 결과를 바꾸려는 것이면 kubernetes/ YAML도 같이 바꾸세요.", file=sys.stderr)
        return 1
    print(f"차트 렌더가 kubernetes/ 의 {len(expected)}개 오브젝트와 같습니다 "
          f"(추가 {len(set(actual) - set(expected))}개는 허용 목록).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
