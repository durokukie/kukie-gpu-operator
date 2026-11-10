# kukie-gpu — Kukie GPU 애드온 1층(관측)

GPU 노드의 DCGM exporter와 Kukie가 지표를 읽을 RoleBinding을 한 번에 설치하고, 설치 직후 preflight Job이 빠진 것(GPU·드라이버·허깅페이스·스토리지·권한)을 ConfigMap에 남긴다. 선택으로 NVIDIA 장치 플러그인과 데모 vLLM(`openai/gpt-oss-20b`)도 올린다. 2층(측정 Job)·3층(ServedModel 오퍼레이터)은 다음 PR이다.

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
| `preflight.enabled` | `true` | 설치·업그레이드 직후 점검 Job. 결과는 ConfigMap `kukie-gpu-preflight` |
| `preflight.storage.storageClassName` | `""` | 스토리지 검사용 테스트 PVC의 StorageClass. 비우면 기본 StorageClass |
| `preflight.extraEnv` | `[]` | 프록시가 필요한 클러스터의 `HTTPS_PROXY` 같은 환경변수 |
| `preflight.permission.serviceAccount` | `""` | 권한 검사 대상 ServiceAccount(2·3층이 쓸 것). 비우면 건너뜀 |
| `oven.enabled` | `false` | 3층 오퍼레이터 `kukie-oven`과 CRD를 올린다. 저장소 차트로 설치할 때는 `oven.image.tag`도 필요하다([runbook](../../runbooks/oven.md)) |
| `oven.maxRuntime` | `2h` | 모델 한 개의 실행 시간 상한. 지나면 다운로드와 vLLM을 멈추고 PVC는 남긴다. `0`이면 저절로 멈추지 않는다. 모델마다 `spec.maxRuntime`으로 바꾼다 |
| `oven.nodeWaitTimeout` | `15m` | 파드가 노드를 못 받을 때 `Pending/WaitingForNode`로 기다려 주는 시간. 넘으면 `Failed/Unschedulable` |
| `oven.measure` | 아래 값 | 측정 Job의 시간 상한(`deadline: 1h`), 단계, 토큰 수, SLA. annotation으로 사람이 시작한다. `reuseMaxAge: 720h`(`0`이면 끔)와 `gpuTypeLabels`는 기준선이 없는 모델에 같은 조건의 측정 기록을 채우는 재사용 설정이다 |
| `counter.enabled` | `false` | 키 발급제 API 입구 `kukie-counter`를 설치 namespace에 올린다. 켜면 아래 두 값이 필수다. 저장소 차트로 설치할 때는 `counter.image.tag`도 필요하다([runbook](../../runbooks/counter.md)) |
| `counter.keysSecret` | `""` | 미리 만든 Secret 이름(data 키 `keys.json`). 차트는 Secret을 만들지 않고 키나 해시를 values로 받지 않는다 |
| `counter.models` | `{}` | 모델 이름별 `upstream`과 `maxConcurrent`. 값이 틀리면 설치 시점에 실패한다 |
| `counter.discovery` | 꺼짐 | ServedModel 기준선으로 한도 정하기. `adaptive.mode`(기본 `observe`)는 스트리밍 응답의 TTFT·ITL로 적응형 한도를 판단만 하고, `on`이면 그 한도를 쓰고 vLLM 대기·KV 브레이크를 건다(`runbooks/counter.md` "적응형 한도") |
| `counter.ingress` | 꺼짐 | ALB 공개 입구. `enabled`, `host`, `certificateArn`, `groupName`. AWS Load Balancer Controller가 있어야 한다(`runbooks/counter.md`) |
| `counter.retryAfterSeconds` / `counter.maxBodyBytes` | `2` / `16777216` | 429·503의 `Retry-After`, 요청 본문 상한 |
| `counter.image` / `counter.imagePullSecrets` / `counter.nodeSelector` / `counter.tolerations` / `counter.resources` | 오븐과 별도 | 이미지와 배치. Pod 수는 값이 아니라 1로 고정이다 |

전체 값과 설명은 [values.yaml](values.yaml), 설치·확인·기존 리소스 넘기기는 [runbooks/gpu-view.md](../../runbooks/gpu-view.md). 기본값 렌더가 `kubernetes/` YAML과 같은지는 `python3 scripts/chart_diff.py`로 확인한다.

## preflight 결과 읽기

설치·업그레이드 뒤 Job `kukie-gpu-preflight`(post-install·post-upgrade hook)가 한 번 돌아 결과를 설치 namespace의 ConfigMap `kukie-gpu-preflight`에 남긴다. 검사가 실패해도 Job은 성공으로 끝나 설치를 막지 않는다.

```sh
kubectl -n gpu-metrics get configmap kukie-gpu-preflight -o jsonpath='{.data.summary}{"\n"}'          # 예: 2/4 통과
kubectl -n gpu-metrics get configmap kukie-gpu-preflight -o jsonpath='{.data.result\.json}' | python3 -m json.tool
kubectl -n gpu-metrics logs job/kukie-gpu-preflight   # 같은 내용을 사람이 읽는 형태로 (Job은 하루 뒤 지워진다)
```

`result.json`은 `{"checkedAt", "summary", "checks": [{"name", "ok", "status", "detail", "hint"}]}`다. `ok`는 통과 `true`, 실패 `false`, 건너뜀 `null`(`status`가 `pass`/`fail`/`skip`으로 같은 뜻). `summary`의 분모에는 건너뛴 검사가 들어가지 않는다.

| `name` | 통과 기준 | 실패·건너뜀 때 `hint` |
| --- | --- | --- |
| `gpu` | 노드에 `nvidia.com/gpu` allocatable > 0 | GPU 노드 켜기, 장치 플러그인이 없으면 `devicePlugin.enabled=true` |
| `driver` | GPU 노드 위 DCGM exporter Pod의 `:9400/metrics`에서 `DCGM_FI_DRIVER_VERSION`을 읽음 | GPU 노드가 없거나 `dcgm.enabled=false`거나 Pod가 아직 이미지를 받는 중이면 건너뜀. 이미지를 못 받거나 Pod가 죽으면 실패(레지스트리·노드 이미지의 드라이버 확인) |
| `huggingface` | `GET https://huggingface.co/api/models?limit=1`이 10초 안에 200 | `preflight.extraEnv`로 프록시 설정, 또는 모델 출처를 S3·PVC로 |
| `storage` | StorageClass가 있고 그 CSI 드라이버가 설치돼 있으며 테스트 PVC(1Gi)가 Bound. `WaitForFirstConsumer`는 Pod가 붙어야 볼륨이 생기므로 StorageClass·CSI 드라이버 존재까지만 보고 건너뜀 | `preflight.storage.storageClassName` 지정, EKS면 `aws-ebs-csi-driver` 애드온 설치 |
| `permission` | `preflight.permission.serviceAccount`가 `namespaces.llm`에 Deployment·Job·PVC를 만들 수 있음(SubjectAccessReview) | 비어 있으면 건너뜀(1층은 생성 권한이 필요 없음). 2·3층 ServiceAccount가 생기면 지정. SubjectAccessReview 생성 자체가 실패하면 건너뜀이고 `detail`에 이유를 적는다 |

다시 돌리려면 같은 차트 버전으로 `helm upgrade`를 한 번 더 한다(값을 다시 주거나 `--reuse-values`). 차트 버전을 올릴 때는 `--reset-then-reuse-values`를 쓴다([이유](../../runbooks/counter.md#업그레이드-옵션)). Job 자체가 못 돌면(이미지를 못 받는 폐쇄망 등) `helm install`이 실패하니 `--set preflight.enabled=false`로 설치한다. Job은 `preflight.activeDeadlineSeconds`(기본 270초, helm `--timeout` 기본 5분보다 짧게) 안에 끝나야 하며, 대기 값(`dcgmWaitSeconds`·`storage.waitSeconds`)을 올리면 이 값도 같이 올린다. 중간에 죽어도 테스트 PVC는 지워지고(Job Pod가 소유자) 그때까지의 결과가 `(중단됨)` 표시와 함께 남는다. Job 전용 ServiceAccount `kukie-gpu-preflight`의 권한은 노드·StorageClass·CSIDriver 읽기, metrics namespace의 Pod 목록, 설치 namespace의 테스트 PVC 하나와 결과 ConfigMap(이름 고정), 그리고 `permission.serviceAccount`를 지정했을 때만 SubjectAccessReview 생성이다.
