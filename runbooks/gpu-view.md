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

**kukie-gpu는 손 apply가 이미 올라가 있다.** Helm은 자기가 만들지 않은 리소스를 덮지 않고 `exists and cannot be imported`로 멈추므로, **설치 전에** 기존 리소스를 이 릴리스 소유로 표시한다(값은 안 바뀌고 라벨·annotation만 붙는다). 손 apply가 없는 새 클러스터는 이 블록을 건너뛴다.

```sh
export AWS_PROFILE=kukie AWS_REGION=ap-northeast-2
rel="meta.helm.sh/release-name=kukie-gpu meta.helm.sh/release-namespace=gpu-metrics"
for r in "ns/llm" "-n gpu-metrics ds/dcgm-exporter" "-n gpu-metrics rolebinding/kukie-gpu-metrics" \
         "-n llm deploy/vllm-gpt-oss-20b" "-n llm svc/vllm-gpt-oss-20b" "-n llm rolebinding/kukie-gpu-metrics" \
         "-n kube-system ds/nvidia-device-plugin-daemonset"; do
  kubectl --context kukie-gpu annotate $r $rel --overwrite
  kubectl --context kukie-gpu label $r app.kubernetes.io/managed-by=Helm --overwrite
done
```

그 다음 설치한다.

```sh
helm --kube-context kukie-gpu upgrade --install kukie-gpu charts/kukie-gpu \
  --namespace gpu-metrics --create-namespace \
  --set devicePlugin.enabled=true --set demoVllm.enabled=true
kubectl --context kukie-gpu -n llm get pods   # vllm-gpt-oss-20b-… Pending (GPU 노드 없음)
```

한 줄이 만드는 것: namespace `llm`(설치 대상 `gpu-metrics`는 `--create-namespace`가 만든다), DCGM exporter DaemonSet과 ClusterIP, 두 namespace의 RoleBinding `kukie-gpu-metrics`(그룹 `kukie-gpu-reader`; ClusterRole은 kukie-infra RBAC가 만든다), 장치 플러그인 DaemonSet(`kube-system`), 데모 vLLM Deployment·Service. 끝에 나오는 NOTES에 확인 명령이 있다.

설치가 끝나면 preflight Job이 한 번 돌아 **빠진 것 목록**을 ConfigMap에 남긴다. 아래 '설치 직후 점검'에서 읽는다.

소유 표시를 빼먹고 설치해 실패했으면 `failed` 리비전이 남아 다음 `upgrade --install`이 `has no deployed releases`로 멈출 수 있다. 그때는 `helm --kube-context kukie-gpu uninstall kukie-gpu -n gpu-metrics`로 실패 릴리스를 지우고(소유 표시가 안 된 기존 리소스는 건드리지 않는다), 위 순서대로 다시 한다.

values 예시(다른 클러스터): GPU 노드 라벨·taint가 다르면 `gpuNode.nodeSelector`·`gpuNode.tolerations`, 사설 미러면 `image.registry`, 장치 플러그인이 이미 있으면 `devicePlugin.enabled=false`(기본), namespace를 하나로 합치려면 `--namespace kukie-gpu --set namespaces.metrics=kukie-gpu --set namespaces.llm=kukie-gpu`. 전체 목록과 설명은 `charts/kukie-gpu/values.yaml`.

## 설치 직후 점검 (preflight)

차트는 `helm install`·`helm upgrade` 직후 Job `kukie-gpu-preflight`(post-install·post-upgrade hook, `preflight.enabled` 기본 `true`)를 한 번 돌려 GPU 자원·드라이버·허깅페이스 접속·스토리지·권한을 보고, 결과를 설치 namespace의 ConfigMap `kukie-gpu-preflight`에 JSON으로 남긴다. 검사가 실패해도 Job은 성공으로 끝나 설치를 막지 않는다. 사용자 클러스터가 제각각이라 "우리가 디버깅"을 "사용자가 목록 보고 고침"으로 바꾸는 장치다. Kukie 에이전트가 이 ConfigMap을 읽어 화면에 보여 주는 것은 별도 이슈다.

```sh
kubectl --context kukie-gpu -n gpu-metrics get configmap kukie-gpu-preflight -o jsonpath='{.data.summary}{"\n"}'
kubectl --context kukie-gpu -n gpu-metrics get configmap kukie-gpu-preflight -o jsonpath='{.data.result\.json}' | python3 -m json.tool
kubectl --context kukie-gpu -n gpu-metrics logs job/kukie-gpu-preflight   # 사람이 읽는 형태. Job은 하루 뒤 지워진다
```

검사와 힌트(`checks[].name` / `hint`):

| 검사 | 통과 기준 | 실패·건너뜀 때 힌트 |
| --- | --- | --- |
| `gpu` | 노드에 `nvidia.com/gpu` allocatable > 0 | GPU 노드 켜기(아래 '볼 때마다'), 장치 플러그인이 없으면 `devicePlugin.enabled=true` |
| `driver` | GPU 노드 위 DCGM exporter Pod의 `:9400/metrics`에 `DCGM_FI_DRIVER_VERSION`이 있음 (GPU를 잡는 Pod를 따로 띄우지 않는다) | GPU 노드가 없거나 `dcgm.enabled=false`면 `skip`. Pod가 60초 안에 Running이 안 되면 노드 이미지의 드라이버 확인(`kubernetes/nvidia/check.yaml`) |
| `huggingface` | `GET https://huggingface.co/api/models?limit=1`이 10초 안에 200 | `preflight.extraEnv`로 `HTTPS_PROXY` 설정, 또는 모델 출처를 S3·PVC로 |
| `storage` | StorageClass가 있고(`preflight.storage.storageClassName`, 비우면 기본) 그 CSI 드라이버가 설치돼 있으며, 테스트 PVC 1Gi가 Bound(`Immediate`) 또는 생성됨(`WaitForFirstConsumer`는 Pod가 붙어야 바인딩되므로 생성까지만 본다) | `storageClassName` 지정, EKS면 `aws-ebs-csi-driver` 애드온 설치 |
| `permission` | preflight ServiceAccount가 `namespaces.llm`에 Deployment·Job·PVC를 만들 수 있음 | 1층만 쓰면 무시(정보성, 지금은 늘 실패). 2층 측정 Job·3층 ServedModel이 오면 그 ServiceAccount 기준으로 바꾼다 |

예상 결과: GPU 노드 0대인 kukie-gpu는 `gpu` 실패·`driver` 건너뜀, `storage`는 기본 StorageClass `gp2`(`kubernetes.io/aws-ebs`)의 EBS CSI 드라이버가 아직 없어 실패한다(3층 PVC를 쓰려면 애드온을 넣어야 한다는 뜻이다). GPU 없는 kind는 `gpu` 실패·`driver` 건너뜀·`huggingface`·`storage` 통과·`permission` 실패로 `2/4 통과`가 나온다(DURO-167 끝 기준).

다시 돌리기: `helm upgrade`를 한 번 더 한다(`--set`을 다시 주거나 `--reuse-values`; 값 없이 `helm upgrade`만 하면 이전 값이 그대로 쓰인다). Job 이미지(`alpine/kubectl`, Docker Hub)를 못 받는 클러스터는 hook이 실패해 `helm install`도 실패하니 `--set preflight.enabled=false`로 설치한다. Job 전용 ServiceAccount `kukie-gpu-preflight`의 권한은 노드·StorageClass·CSIDriver 읽기, `gpu-metrics`의 Pod 목록, 설치 namespace의 테스트 PVC 하나(`kukie-gpu-preflight-test`)와 결과 ConfigMap(이름 고정)뿐이다. `helm uninstall`은 ConfigMap과 RBAC를 지우고 끝난 Job만 하루 뒤 TTL로 사라진다.

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

릴리스는 남겨 둔다(노드가 없으면 요금이 없다). 지우려면 `helm --kube-context kukie-gpu uninstall kukie-gpu -n gpu-metrics`. 릴리스에 든 것이 다 지워진다: DCGM·RoleBinding·데모 vLLM, namespace `llm`, 그리고 `devicePlugin.enabled=true`로 설치했다면 `kube-system`의 장치 플러그인 DaemonSet까지. 설치 대상 `gpu-metrics` namespace는 남는다.

## 위험

- vLLM에는 API 키가 없다. Ingress·LoadBalancer를 두지 않아 클러스터 안(Service)과 API 서버 Pod 프록시로만 닿는다. Kukie 역할의 프록시 권한은 GET뿐이라(kukie-infra RBAC) 추론 요청(POST)은 못 보낸다.
- DCGM exporter는 upstream 기본대로 root와 `SYS_ADMIN`으로 돈다(프로파일링 카운터). GPU 노드에만 뜬다.
- `llm`·`gpu-metrics`에는 이 두 가지 말고 다른 것을 두지 않는다. Kukie 역할이 그 namespace Pod의 아무 포트에 GET을 보낼 수 있기 때문이다. preflight Job Pod는 `gpu-metrics`에서 몇 초 돌고 끝나며 포트를 열지 않는다.
