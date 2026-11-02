# kukie-gpu — Kukie GPU 애드온 1층(관측)

GPU 노드의 DCGM exporter와 Kukie가 지표를 읽을 RoleBinding을 한 번에 설치한다. 선택으로 NVIDIA 장치 플러그인과 데모 vLLM(`openai/gpt-oss-20b`)도 올린다. 2층(측정 Job)·3층(ServedModel 오퍼레이터)은 다음 PR이다.

```sh
helm upgrade --install kukie-gpu charts/kukie-gpu --namespace gpu-metrics --create-namespace
```

| 값 | 기본 | 뜻 |
| --- | --- | --- |
| `image.registry` | `""` | 모든 이미지의 레지스트리를 한 곳으로(사설 미러). 비우면 원래 주소 |
| `gpuNode.nodeSelector` / `gpuNode.tolerations` | kukie-gpu 라벨·taint | GPU 노드 고르기. 모든 Pod가 같이 쓴다 |
| `namespaces.create` | `true` | namespace를 차트가 만든다(설치 대상은 제외) |
| `namespaces.metrics` / `namespaces.llm` | `gpu-metrics` / `llm` | 같은 이름이면 하나로 합친다 |
| `dcgm.enabled` | `true` | DCGM exporter DaemonSet(GPU 노드만) |
| `dcgm.service.enabled` | `true` | `helm test`용 ClusterIP |
| `devicePlugin.enabled` | `false` | NVIDIA 장치 플러그인. GPU Operator가 있으면 끔 |
| `rbac.enabled` / `rbac.readerGroup` / `rbac.clusterRole` | `true` / `kukie-gpu-reader` / `kukie-gpu-metrics` | 두 namespace에 RoleBinding. ClusterRole은 kukie-infra RBAC가 만든다 |
| `demoVllm.enabled` | `false` | 데모 vLLM Deployment·Service |

전체 값과 설명은 [values.yaml](values.yaml), 설치·확인·기존 리소스 넘기기는 [runbooks/gpu-view.md](../../runbooks/gpu-view.md). 기본값 렌더가 `kubernetes/` YAML과 같은지는 `python3 scripts/chart_diff.py`로 확인한다.
