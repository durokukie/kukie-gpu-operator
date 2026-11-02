# Kukie GPU 화면 데모: kukie-gpu에 지표와 서빙 올리기 (관리자 적용)

Kukie 앱의 GPU 화면(웹 `GpuView`, agent `kukie_agent/clusters/gpu.py`)은 연결된 클러스터에서 세 가지를 읽는다.

| 칸 | 읽는 것 | 이 저장소에서 올리는 것 |
| --- | --- | --- |
| GPU 노드 | 노드 목록의 `nvidia.com/gpu` 자원, `nvidia.com/gpu.product` 라벨 | 장치 플러그인 — 차트 `devicePlugin.enabled=true` (kukie-gpu에는 이미 있음) |
| 실시간 사용률·메모리·전력·온도 | GPU 노드 위 DCGM exporter Pod의 `:9400/metrics` | 차트 `dcgm.enabled=true` (namespace `gpu-metrics`) |
| 서빙 중인 모델 (대기·실행 요청, KV 캐시, TTFT·ITL p95) | GPU 노드 위 vLLM Pod의 `:8000/metrics` | 차트 `demoVllm.enabled=true` (namespace `llm`). 3층 ServedModel이 오면 그쪽이 대신한다 |

세 가지와 Kukie 읽기 RoleBinding은 **Helm 차트 `charts/kukie-gpu`** 하나로 설치한다(애드온 1층 관측). `kubernetes/` 아래 YAML은 손 apply 하던 원본이고, 이제는 차트가 같은 것을 만드는지 CI가 비교하는 기준으로만 남아 있다(`scripts/chart_diff.py`). 다른 클러스터에 깔 때는 values만 바꾼다. 이미 Prometheus·GPU Operator가 있는 클러스터의 지표에 "붙기만 하는" 모드는 아직 없다(Linear `[검토] GPU 애드온 설계 — 현업 시각 지적 사항` ②).

지표는 Prometheus 없이 **API 서버 Pod 프록시**로 읽는다. 그래서 Kukie 연결 역할에는 kukie-infra의 GPU 읽기 권한이 필요하다(템플릿 v4 `GpuRead=true` + `gpu-read-rbac-v1.yaml`, kukie-infra `runbooks/github-actions-foundation.md` '템플릿 v4'). 그 RBAC는 노드·namespace 목록만 클러스터 전체에 주고, 파드 목록과 프록시는 **아래 두 namespace에만** RoleBinding으로 준다.

## 비용

GPU 노드가 떠 있는 동안만 g6e.xlarge 온디맨드 $2.288/시간이다. 두 매니페스트는 GPU 노드에만 뜨므로 노드가 0대면 DCGM Pod는 0개, vLLM Pod는 Pending이고 요금이 없다(자동 확장기가 아직 없어 Pending Pod가 노드를 띄우지 않는다). **보고 나면 바로 0대로 내린다.**

## 처음 한 번 (GPU 노드 없이)

```sh
export AWS_PROFILE=kukie AWS_REGION=ap-northeast-2
helm --kube-context kukie-gpu upgrade --install kukie-gpu charts/kukie-gpu \
  --namespace gpu-metrics --create-namespace \
  --set devicePlugin.enabled=true --set demoVllm.enabled=true
kubectl --context kukie-gpu -n llm get pods   # vllm-gpt-oss-20b-… Pending (GPU 노드 없음)
```

한 줄이 만드는 것: namespace `llm`(설치 대상 `gpu-metrics`는 `--create-namespace`가 만든다), DCGM exporter DaemonSet과 ClusterIP, 두 namespace의 RoleBinding `kukie-gpu-metrics`(그룹 `kukie-gpu-reader`; ClusterRole은 kukie-infra RBAC가 만든다), 장치 플러그인 DaemonSet(`kube-system`), 데모 vLLM Deployment·Service. 끝에 나오는 NOTES에 확인 명령이 있다.

values 예시(다른 클러스터): GPU 노드 라벨·taint가 다르면 `gpuNode.nodeSelector`·`gpuNode.tolerations`, 사설 미러면 `image.registry`, 장치 플러그인이 이미 있으면 `devicePlugin.enabled=false`(기본), namespace를 하나로 합치려면 `--namespace kukie-gpu --set namespaces.metrics=kukie-gpu --set namespaces.llm=kukie-gpu`. 전체 목록과 설명은 `charts/kukie-gpu/values.yaml`.

**kukie-gpu처럼 이미 손 apply가 올라가 있는 클러스터.** Helm은 자기가 만들지 않은 리소스를 덮지 않고 "already exists"로 멈춘다. 설치 전에 기존 리소스를 이 릴리스 소유로 표시한다(값은 안 바뀌고 메타데이터만 붙는다). 그 뒤 위 `upgrade --install`을 그대로 실행한다.

```sh
rel="meta.helm.sh/release-name=kukie-gpu meta.helm.sh/release-namespace=gpu-metrics"
for r in "ns/llm" "-n gpu-metrics ds/dcgm-exporter" "-n gpu-metrics rolebinding/kukie-gpu-metrics" \
         "-n llm deploy/vllm-gpt-oss-20b" "-n llm svc/vllm-gpt-oss-20b" "-n llm rolebinding/kukie-gpu-metrics" \
         "-n kube-system ds/nvidia-device-plugin-daemonset"; do
  kubectl --context kukie-gpu annotate $r $rel --overwrite
  kubectl --context kukie-gpu label $r app.kubernetes.io/managed-by=Helm --overwrite
done
```

## 볼 때마다

```sh
# 1) GPU 노드 1대
aws eks update-nodegroup-config --cluster-name kukie-gpu --nodegroup-name kukie-gpu-g6e \
  --scaling-config minSize=0,maxSize=2,desiredSize=1
until kubectl --context kukie-gpu get node -l kukie.io/accelerator=nvidia-l40s -o name | grep -q .; do sleep 20; done
kubectl --context kukie-gpu wait --for=condition=Ready node -l kukie.io/accelerator=nvidia-l40s --timeout=15m

# 2) 지표와 서빙이 뜨기를 기다린다 (vLLM은 모델 약 13GB를 NAT로 받아 몇 분 걸린다)
kubectl --context kukie-gpu -n gpu-metrics rollout status daemonset/dcgm-exporter --timeout=10m
kubectl --context kukie-gpu -n llm rollout status deployment/vllm-gpt-oss-20b --timeout=45m

# 3) 관리자 권한으로 지표가 나오는지 (Kukie 화면과 같은 경로)
pod=$(kubectl --context kukie-gpu -n gpu-metrics get pod -l app.kubernetes.io/name=dcgm-exporter -o name | head -1)
kubectl --context kukie-gpu get --raw "/api/v1/namespaces/gpu-metrics/pods/${pod#pod/}:9400/proxy/metrics" | grep -m3 DCGM_FI_DEV_GPU_UTIL
pod=$(kubectl --context kukie-gpu -n llm get pod -l app.kubernetes.io/name=vllm -o name | head -1)
kubectl --context kukie-gpu get --raw "/api/v1/namespaces/llm/pods/${pod#pod/}:8000/proxy/metrics" | grep -m3 'vllm:num_requests'
```

4) Kukie 웹에서 kukie-gpu 클러스터를 열면 활동 막대에 GPU 아이콘이 생기고, GPU 화면에 GPU 노드 1대와 `openai/gpt-oss-20b` 한 줄이 보인다. GPU 이름 라벨(`nvidia.com/gpu.product`)은 GPU Feature Discovery가 붙이는데 아직 없으므로 이름은 인스턴스 종류(`g6e.xlarge`)로, 메모리는 DCGM 값으로 나온다. 요청이 없으면 사용률 0%, TTFT·ITL은 비어 있다. 숫자를 움직이려면 클러스터 안에서 짧게 요청을 보낸다.

```sh
kubectl --context kukie-gpu -n llm run load --rm -i --restart=Never --image=curlimages/curl:8.11.1 --command -- \
  sh -c 'for i in $(seq 1 20); do curl -s http://vllm-gpt-oss-20b:8000/v1/completions -H "Content-Type: application/json" \
    -d "{\"model\":\"openai/gpt-oss-20b\",\"prompt\":\"Kubernetes 를 한 문단으로 설명해 줘.\",\"max_tokens\":200}" >/dev/null & done; wait'
```

## 끝나면 (바로)

```sh
aws eks update-nodegroup-config --cluster-name kukie-gpu --nodegroup-name kukie-gpu-g6e \
  --scaling-config minSize=0,maxSize=2,desiredSize=0
kubectl --context kukie-gpu get node -l kukie.io/accelerator=nvidia-l40s   # 몇 분 뒤 No resources found
```

릴리스는 남겨 둔다(노드가 없으면 요금이 없다). 지우려면 `helm --kube-context kukie-gpu uninstall kukie-gpu -n gpu-metrics`. namespace `llm`은 릴리스가 만든 것이라 같이 지워지고, `gpu-metrics`는 남는다.

## 위험

- vLLM에는 API 키가 없다. Ingress·LoadBalancer를 두지 않아 클러스터 안(Service)과 API 서버 Pod 프록시로만 닿는다. Kukie 역할의 프록시 권한은 GET뿐이라(kukie-infra RBAC) 추론 요청(POST)은 못 보낸다.
- DCGM exporter는 upstream 기본대로 root와 `SYS_ADMIN`으로 돈다(프로파일링 카운터). GPU 노드에만 뜬다.
- `llm`·`gpu-metrics`에는 이 두 가지 말고 다른 것을 두지 않는다. Kukie 역할이 그 namespace Pod의 아무 포트에 GET을 보낼 수 있기 때문이다.
