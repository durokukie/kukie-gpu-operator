# Kukie GPU 화면 데모: kukie-gpu에 지표와 서빙 올리기 (관리자 적용)

Kukie 앱의 GPU 화면(웹 `GpuView`, agent `kukie_agent/clusters/gpu.py`)은 연결된 클러스터에서 세 가지를 읽는다.

| 칸 | 읽는 것 | 이 저장소에서 올리는 것 |
| --- | --- | --- |
| GPU 노드 | 노드 목록의 `nvidia.com/gpu` 자원, `nvidia.com/gpu.product` 라벨 | 장치 플러그인 `kubernetes/nvidia/device-plugin.yaml`(이미 있음) |
| 실시간 사용률·메모리·전력·온도 | GPU 노드 위 DCGM exporter Pod의 `:9400/metrics` | `kubernetes/gpu-metrics/dcgm-exporter.yaml` (namespace `gpu-metrics`) |
| 서빙 중인 모델 (대기·실행 요청, KV 캐시, TTFT·ITL p95) | GPU 노드 위 vLLM Pod의 `:8000/metrics` | `kubernetes/llm/vllm-gpt-oss-20b.yaml` (namespace `llm`) |

지표는 Prometheus 없이 **API 서버 Pod 프록시**로 읽는다. 그래서 Kukie 연결 역할에는 kukie-infra의 GPU 읽기 권한이 필요하다(템플릿 v4 `GpuRead=true` + `gpu-read-rbac-v1.yaml`, kukie-infra `runbooks/github-actions-foundation.md` '템플릿 v4'). 그 RBAC는 노드·namespace 목록만 클러스터 전체에 주고, 파드 목록과 프록시는 **아래 두 namespace에만** RoleBinding으로 준다.

## 비용

GPU 노드가 떠 있는 동안만 g6e.xlarge 온디맨드 $2.288/시간이다. 두 매니페스트는 GPU 노드에만 뜨므로 노드가 0대면 DCGM Pod는 0개, vLLM Pod는 Pending이고 요금이 없다(자동 확장기가 아직 없어 Pending Pod가 노드를 띄우지 않는다). **보고 나면 바로 0대로 내린다.**

## 처음 한 번 (GPU 노드 없이)

```sh
export AWS_PROFILE=kukie AWS_REGION=ap-northeast-2
kubectl --context kukie-gpu apply -f kubernetes/nvidia/device-plugin.yaml
kubectl --context kukie-gpu apply -f kubernetes/gpu-metrics/dcgm-exporter.yaml
kubectl --context kukie-gpu apply -f kubernetes/llm/vllm-gpt-oss-20b.yaml
# Kukie 연결 역할이 두 namespace의 파드 목록·프록시를 읽게 (그룹 kukie-gpu-reader, ClusterRole kukie-gpu-metrics는 kukie-infra RBAC가 만든다)
kubectl --context kukie-gpu -n gpu-metrics create rolebinding kukie-gpu-metrics --clusterrole=kukie-gpu-metrics --group=kukie-gpu-reader
kubectl --context kukie-gpu -n llm create rolebinding kukie-gpu-metrics --clusterrole=kukie-gpu-metrics --group=kukie-gpu-reader
kubectl --context kukie-gpu -n llm get pods   # vllm-gpt-oss-20b-… Pending (GPU 노드 없음)
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

매니페스트는 남겨 둔다(노드가 없으면 요금이 없다). 지우려면 `kubectl delete -f`로 두 파일을 지운다.

## 위험

- vLLM에는 API 키가 없다. Ingress·LoadBalancer를 두지 않아 클러스터 안(Service)과 API 서버 Pod 프록시로만 닿는다. Kukie 역할의 프록시 권한은 GET뿐이라(kukie-infra RBAC) 추론 요청(POST)은 못 보낸다.
- DCGM exporter는 upstream 기본대로 root와 `SYS_ADMIN`으로 돈다(프로파일링 카운터). GPU 노드에만 뜬다.
- `llm`·`gpu-metrics`에는 이 두 가지 말고 다른 것을 두지 않는다. Kukie 역할이 그 namespace Pod의 아무 포트에 GET을 보낼 수 있기 때문이다.
