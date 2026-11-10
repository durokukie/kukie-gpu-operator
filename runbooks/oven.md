# kukie-oven: ServedModel 오퍼레이터 (애드온 3층 서빙)

`oven/`(`kukie-oven`)은 `ServedModel`(`serving.kukie.io/v1alpha1`)에 허깅페이스 모델 이름을 적으면 PVC → 다운로드 Job → vLLM
Deployment·Service → `/health` 확인 → status 기록을 사람 손 없이 돌리는 Go 오퍼레이터(kubebuilder·controller-runtime)다.
설계는 Linear `[설계] ServedModel 오퍼레이터 · 실패 진단 · 어드미션 프록시` 1·2절, 상위 결정은 `[결정] GPU 서빙 애드온` 3·6절.

**지금 상태**: `kukie-gpu`의 namespace `kukie-gpu`에 오븐과 카운터가 같은 릴리스 `kukie-gpu`로 차트 설치돼 있다. 1층(DCGM·RoleBinding·장치 플러그인·preflight)도 2026-11-11부터 같은 릴리스에 있다(revision 19, [gpu-view](gpu-view.md)). 2026-11-03에 GHCR 이미지로 설치해 L40S에서 `gpt-oss-20b`를 `Ready`까지 띄우고 실제 요청을 확인했다. 결과는 `history/2026-11.md`. 2026-11-10에 차트 `0.10.0`(오븐 `0.10.0`, 기준선 재사용)으로 올렸다. 같은 날 GPU로 재사용을 실제로 확인했다(지웠다 다시 만든 모델이 측정 없이 기준선 16, 카운터 한도 12). 2026-11-11에 차트 `0.13.0`(revision 18, 오븐 이미지 `0.13.0`)으로 올렸다. 오븐은 `--watch-namespaces=llm`으로 `llm`의 ServedModel만 처리하고, 권한은 `llm`의 Role과 노드 get(ClusterRole)뿐이다(아래 "권한"). 지금은 revision 19(같은 차트, 1층을 켬)다. 오븐 권한 변경만 되돌리려면 `helm rollback kukie-gpu 17`(차트 `0.12.0`, 오븐 `0.10.0`, 클러스터 전체 권한)이지만, revision 18 이하로 되돌리면 1층(DCGM·RoleBinding 2개·장치 플러그인)이 지워지니 [gpu-view](gpu-view.md) "kukie-gpu: 손 apply한 1층을 릴리스로 옮기기"의 되돌리기대로 바로 다시 올린다.

## 상태 흐름

| phase | 뜻 | 다음 |
| --- | --- | --- |
| `Pending` | 방금 만들어짐. PVC와 다운로드 Job을 만든다 | `Downloading` |
| `Downloading` | 다운로드 Job이 돈다. `reason`으로 나눈다: `JobCreated`(방금 만듦), `PreparingPod`(Pod가 없거나 컨테이너가 아직 시작 전, 보통 이미지를 받는 중), `JobRunning`(컨테이너 실행 중, [내려받기 진행률](#내려받기-진행률)이 움직인다), `Retrying`(한 시도가 0이 아닌 코드로 끝나 Job이 다시 시도 중, 종료 코드를 적는다) | `Loading` / `Failed` |
| `Loading` | vLLM Deployment를 만들었고 Pod가 `/health`에 답하기를 기다린다. `reason`은 Pod가 없으면 `NoPod`, 있으면 시작 전이든 실행 중이든 `Starting`이다(둘은 `status.message`의 Pod phase로만 나뉜다) | `Ready` / `Failed` |
| `Ready` | Pod의 readinessProbe(`GET /health`)가 통과했다 | `Measuring` (annotation) |
| `Measuring` | annotation `serving.kukie.io/measure`(빈 값이 아닌 값)로 측정 Job이 돌고 있다. 모델은 계속 서빙 상태다 | `Ready` |
| `Pending` (`reason: WaitingForNode`) | 다운로드·vLLM Pod가 노드를 받지 못했고 아직 `--node-wait-timeout`(기본 15분) 전이다. `status.message`는 scheduler 메시지 그대로 | 노드가 뜨면 `Downloading`/`Loading`, 시간이 넘으면 `Failed/Unschedulable` |
| `Failed` | 다운로드 Job이 재시도(backoffLimit 2)를 다 쓰고 실패(`DownloadFailed`, 마지막 시도의 종료 코드 포함), 또는 다운로드·vLLM Pod가 `--node-wait-timeout`을 넘겨서도 Unschedulable, ImagePullBackOff·CrashLoopBackOff·비정상 종료. `status.reason`은 쿠버네티스가 준 이유 그대로, `status.message`는 그 메시지 그대로 | Pod가 살아나면 자동으로 `Downloading`/`Loading`→`Ready`. 원인 분류·힌트는 DURO-171 |
| `Stopped` (`reason: MaxRuntimeReached`) | 실행 시간 상한이 지나 다운로드 Job과 vLLM을 멈췄다. PVC는 남는다. [실행 시간 상한](#실행-시간-상한) 참고 | 끝. 다시 시작은 지우고 다시 만든다 |

`Failed`는 Pod 상태만 보고 정한다(이벤트·로그는 DURO-171). 다운로드 Job의 Pod와 vLLM Pod 둘 다 본다. GPU 노드가 0대일 때
scheduler 메시지가 `status.message`에 그대로 들어가는데, 둘이 다르다: 다운로드 Pod는 GPU를 요청하지 않고 nodeSelector로만
GPU 노드에 가므로 `0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.`이고, vLLM Pod는
`nvidia.com/gpu`를 요청하므로 `… Insufficient nvidia.com/gpu.`다. 노드가 올라오면 혼자 풀린다. PVC가 묶이지 않으면
`pod has unbound immediate PersistentVolumeClaims`가 들어간다. 단, PVC 프로비저닝 자체의 실패(StorageClass는 있는데 CSI
드라이버 오류 등)는 이벤트에만 남아 아직 안 보인다.

**노드 대기와 설정 오류.** Karpenter는 Unschedulable Pod를 보고 노드를 띄우므로 이 상태는 처음에는 실패가 아니다. 그러나 scheduler 메시지만으로는 "노드가 아직 없음"과 "어떤 노드도 맞지 않는 설정"을 믿을 만하게 나눌 수 없어, 원인과 상관없이 Pod의 `PodScheduled=False` 시각부터 `--node-wait-timeout`이 지나면 `Failed/Unschedulable`로 적는다. 진짜 설정 오류가 `Pending`에 영원히 숨지 않게 하려는 것이다. 그 뒤에 노드가 생기면 예전처럼 혼자 풀린다. 다운로드 Pod와 vLLM Pod는 각자 따로 잰다.

## 하위 리소스

ServedModel `NAME`(namespace `NS`)마다 다음을 만든다. 측정 기록을 뺀 전부가 label `serving.kukie.io/servedmodel=NAME`을 달고
server-side apply(field manager `kukie-oven`)로 "있어야 할 모양"에 맞춘다. 같은 spec이면 몇 번을 돌아도 같은 결과다.

| 리소스 | 이름 | ownerReference | 비고 |
| --- | --- | --- | --- |
| PVC | `NAME-models` | `spec.storage.retainOnDelete: false`일 때만 | ReadWriteOnce, `spec.storage.size`. 만든 뒤 spec은 건드리지 않는다(storageClassName 불변, 크기는 사용자가 직접 늘린다). `retainOnDelete`를 바꾸면 ownerReference만 붙였다 뗀다 |
| Job | `NAME-download-<해시8>` | 있음 | `hf download <모델>`을 `HF_HOME=/models`로, `download.py`로 감싸 돌린다. 레포 최상위에 safetensors가 있으면 그 밖의 가중치 파일(폴더 안 사본, 다른 형식, 분할 파일이 있을 때의 최상위 `consolidated*`)은 뺀다. 그래서 `spec.engine.args`에 Mistral 형식(`--load-format mistral` 등)을 쓰면 파일이 없어 뜨지 않는다. 파일 목록을 못 받으면 전부 받는다([내려받기 진행률](#내려받기-진행률)). 해시는 모델·tokenSecretRef·다운로드 이미지에서 나오므로 출처가 바뀌면 새 Job을 만들고 옛 Job은 지운다. 끝난 Job은 "받았다"는 표시로 남긴다 |
| Deployment | `NAME` | 있음 | `kubernetes/llm/vllm-gpt-oss-20b.yaml`과 같은 모양: Recreate, GPU taint toleration, `/health` startupProbe(40분)·readinessProbe, `/dev/shm` 2Gi. args는 `--model`, `--tensor-parallel-size`(=gpus), `--port 8000` 뒤에 `spec.engine.args`. `HF_HUB_OFFLINE=1`로 PVC 파일만 쓴다. 어드미션 프록시 사이드카 자리는 비어 있다 |
| Service | `NAME` | 있음 | `:8000` → vLLM. 사이드카가 들어오면 8080으로 바꾼다 |
| Job | `NAME-measure-<세대>`, 값이 `true`가 아니면 `NAME-measure-<세대>-<값 해시8>` | 있음 | annotation이 있을 때만. [측정](#측정) 참고 |
| ConfigMap | `kukie-gpu-baseline-NAME` | 있음 | 마지막 측정 결과(프록시용). 지우지 않으면 다음 측정까지 남는다 |
| ConfigMap | `kukie-gpu-measure-NAME-<UTC yyyymmddhhmmss>` | **없음** | 실행마다 하나. 모델을 지워도 남는다. 위 라벨을 달지 않는다. [측정 기록](#측정-기록) 참고 |

* ServedModel을 지우면 ownerReference를 단 것은 가비지 컬렉션으로 같이 지워진다. `retainOnDelete: true`면 PVC만 남는다.
* 다운로드 Job과 Deployment에 같은 nodeSelector(`--gpu-node-selector`)와 GPU toleration을 준다. 다운로드 이미지 기본값이
  vLLM 이미지라 다운로드 중에 GPU 노드가 이미지를 미리 받는 효과가 있고, 존(zone) 단위 스토리지(gp3 등)라면 PVC가 vLLM이
  뜰 노드와 같은 존에 묶인다. 대신 다운로드 동안 GPU 노드가 떠 있어야 한다(지금 매니페스트도 그렇다).
* 다운로드 Job은 한 시도가 실패하면 스스로 2번 더 시도한다(그동안 `Downloading/Retrying`). 다 실패해 `Failed/DownloadFailed`가
  된 뒤 같은 spec으로 다시 받으려면 그 Job을 지운다. 오퍼레이터가 같은 이름으로 다시 만든다.
* `spec.engine.*`을 바꾸면 Deployment가 바뀌어 Pod가 다시 뜬다(Recreate). `spec.source`를 바꾸면 새 다운로드 Job부터 다시 가고, 그동안 vLLM을 0대로 내린다(RWO PVC를 비워야 다운로드 Pod가 붙는다). **그동안 서빙이 멈춘다.** 옛 모델 파일은 PVC에 남으니 `spec.storage.size`는 두 모델을 합친 크기여야 한다.
* 다운로드 이미지 기본값이 엔진 이미지라 `spec.engine.image`를 바꿔도 다운로드 Job 이름이 바뀌어 같은 흐름을 탄다. 파일이 이미 있으면 `hf download`는 금방 끝난다.
* **이름 충돌**: 같은 이름의 Deployment·Service(소유자가 이 ServedModel이 아님)나 PVC `NAME-models`(라벨이 이 ServedModel이 아님)가 있으면 아무것도 만들지 않고 `Failed/NameConflict`로 적는다. 덮어쓰면 ServedModel을 지울 때 남의 리소스까지 지워지기 때문이다. 손으로 만든 `vllm-gpt-oss-20b`와 같은 이름을 쓰면 이렇게 된다. 같은 이름의 ServedModel을 지우고 바로 다시 만들면(`kubectl replace --force`, GitOps 재동기화) 옛 ServedModel이 남긴 Deployment·Service·다운로드 Job·PVC는 충돌이 아니라 정리 중이므로 `Pending/WaitingForOldResources`로 사라지기를 기다린다.
* **API 서버가 거부한 경우**: `spec.engine.resources`의 requests가 limits보다 큰 경우처럼 다시 해도 안 되는 거부는 `Failed/ApplyRejected`로 `status.message`에 그대로 적는다. 일시적인 API 오류는 phase를 바꾸지 않고 다시 시도하며, 그동안 `observedGeneration`은 오르지 않는다.
* `spec.engine.resources`는 기본값 위에 키별로 덮는다. requests만 적어 기본 limit을 넘으면 limit을 함께 올리고, limits만 적어 기본 request 아래로 내리면 request를 함께 내린다.

## 매니저 플래그

| 플래그 | 기본값 | 뜻 |
| --- | --- | --- |
| `--gpu-node-selector` | 비어 있음 | 다운로드 Job·Deployment의 nodeSelector(`key=value[,key=value]`). `config/manager/manager.yaml`은 kukie-gpu용 `kukie.io/accelerator=nvidia-l40s`를 넣어 둔다. 사용자 클러스터에서는 비우거나 그 클러스터 라벨로 바꾼다 |
| `--download-image` | 비어 있음 (= `spec.engine.image`) | `python3`와 `hf` CLI가 있는 다운로드용 이미지 |
| `--download-deadline` | `2h` | 다운로드 Job 하나의 시간 상한(재시도 포함). 넘으면 `Failed/DownloadFailed`(`DeadlineExceeded`). 멈춘 다운로드가 GPU 노드를 계속 잡지 않게 한다. 아주 큰 모델은 올린다 |
| `--max-runtime` | `2h` | `spec.maxRuntime`이 빈 모델의 실행 시간 상한(차트 `oven.maxRuntime`). `0`이면 저절로 멈추지 않는다 |
| `--node-wait-timeout` | `15m` | Unschedulable Pod를 `Pending/WaitingForNode`로 두는 시간(차트 `oven.nodeWaitTimeout`). 넘으면 `Failed/Unschedulable`. `0`이면 기다리지 않는다 |
| `--measure-deadline` | `1h` | 측정 Job 하나의 시간 상한 |
| `--measure-concurrency` | `1 2 4 8 16 32 64` | 측정의 동시 요청 단계 |
| `--measure-input-tokens`, `--measure-output-tokens` | `1000`, `200` | 측정 요청의 입력·출력 토큰 수 |
| `--measure-ttft-ms`, `--measure-itl-ms` | `1000`, `50` | SLA: TTFT p95, ITL p95(밀리초) |
| `--measure-reuse-max-age` | `720h` | 기준선 재사용(아래 "기준선 재사용")에 쓸 측정 기록의 최대 나이(차트 `oven.measure.reuseMaxAge`). `0`이면 재사용하지 않는다 |
| `--gpu-type-labels` | `nvidia.com/gpu.product,node.kubernetes.io/instance-type` | GPU 종류를 읽을 노드 라벨, 앞의 것이 먼저(차트 `oven.measure.gpuTypeLabels`) |
| `--watch-namespaces` | 비어 있음 (= 모든 namespace) | ServedModel을 처리할 namespace, 쉼표 구분(차트 `oven.namespaces`, 비우면 `namespaces.llm`). 캐시가 이 namespace만 보므로 namespace마다 Role이면 된다. 비우면 클러스터 전체를 보므로 ClusterRole이 필요하다. 차트는 늘 채운다 |

## 설치

`oven.enabled`를 켜면 차트가 CRD, ServiceAccount, ClusterRole(노드 get만), 리더 선출 Role, Deployment `kukie-oven`을 설치 namespace에 올리고,
`oven.namespaces`(비우면 `namespaces.llm`)의 namespace마다 Role·RoleBinding `kukie-oven`을 만든다(아래 "권한"). 기본은 꺼 둔다. 파드는 `kukie-oven-<해시>-<무작위>`로 뜬다.

```sh
# GHCR에 올린 차트: 이미지 digest가 차트에 들어 있다 (패키지가 비공개면 helm registry login이 먼저 필요하다)
helm install kukie-gpu oci://ghcr.io/durokukie/charts/kukie-gpu --namespace kukie-gpu --create-namespace --set oven.enabled=true

# 저장소의 차트: 이미지를 직접 지정한다
helm install kukie-gpu charts/kukie-gpu --namespace kukie-gpu --create-namespace \
  --set oven.enabled=true --set oven.image.tag=<버전>

kubectl -n kukie-gpu rollout status deployment/kukie-oven
```

비공개 패키지면 설치 namespace에 pull Secret을 만들고 `--set 'oven.imagePullSecrets[0].name=<Secret 이름>'`을 더한다.
`--gpu-node-selector`는 `gpuNode.nodeSelector`에서, `--download-deadline`·`--download-image`·`--max-runtime`·`--node-wait-timeout`은 `oven.*` 값에서 만든다.

**0.5.0으로 올릴 때.** 이미 떠 있는 모델도 업그레이드한 때부터 2시간 뒤에 멈춘다(`status.startedAt`이 없으면 그때 기록한다). 계속 돌려야 하는 모델은 올리기 전에 `spec.maxRuntime: "0"`을 적는다.

**0.5.1로 올릴 때.** 끝난 다운로드 Job은 표시로 그대로 쓰므로 다시 받지 않는다. 업그레이드 전에 시작한 Job은 진행률 줄을 쓰지 않아 끝날 때까지 0이다. `--download-image`를 쓰면 그 이미지에 `python3`가 있어야 한다.

**0.6.0으로 올릴 때.** 차트가 CRD(`status.measureRequest`)와 ClusterRole(`configmaps`에 `list`·`delete` 추가)을 함께 올린다. 예전 오븐이 `"true"`로 끝낸 측정은 처리한 것으로 그대로 두어 다시 돌지 않는다. 측정 기록은 올린 뒤의 실행부터 생긴다.

**0.13.0으로 올릴 때.** ClusterRole `kukie-oven`의 규칙이 노드 get 하나로 줄고, 나머지는 `llm`의 Role `kukie-oven`으로 옮겨진다. 이름이 같아 helm이 규칙을 그 자리에서 바꾼다. 매니저는 `--watch-namespaces=llm`으로 다시 떠서 `llm` 밖의 ServedModel은 처리하지 않는다. 다른 namespace에 ServedModel이 있으면 올리기 전에 `oven.namespaces`에 그 namespace를 함께 적는다(예: `--set 'oven.namespaces={llm,team-a}'`). 오븐을 켜면 차트는 `namespaces.create`일 때 `namespaces.llm`을 만든다(`rbac.enabled`와 상관없이). 그 밖의 namespace, 또는 `namespaces.create=false`일 때의 `llm`은 미리 있어야 한다. **오븐 이미지도 0.13.0 이상으로 함께 올린다.** 0.12.0 이하 이미지는 `--watch-namespaces`를 몰라 뜨지 못한다. `--reset-then-reuse-values`는 `--set`으로 넣었던 `oven.image.*`를 남기므로 `--set oven.image.tag=null --set oven.image.digest=null`로 지워 차트에 든 digest를 쓰게 한다.

**CRD는 uninstall해도 남는다**(`helm.sh/resource-policy: keep`). CRD가 지워지면 사용자의 `ServedModel`이 전부 함께 지워지기 때문이다.
같은 이유로 오븐을 켜면 차트가 만든 `namespaces.llm`도 남긴다. namespace가 지워지면 그 안의 ServedModel과 모델 PVC가 함께 사라진다.
완전히 지우려면 `kubectl delete crd servedmodels.serving.kukie.io`를 직접 한다. 그러면 ServedModel과 그 소유 리소스가 지워진다.

차트의 CRD·권한은 `charts/kukie-gpu/files/oven/`의 복사본이다. `make -C oven manifests`가 함께 갱신하고, CI가 어긋나면 실패한다.

**차트를 올릴 때는 `--reuse-values` 대신 `--reset-then-reuse-values`를 쓴다**(이유는 [counter](counter.md) "업그레이드 옵션"). `--set`으로 넣은 `oven.image.*`는 어느 쪽이든 남아서, 새 차트의 digest를 받으려면 값을 새로 주거나 `null`로 지운다.

로컬에서만 돌려 보려면 `make run`(현재 kubeconfig로 매니저를 호스트에서 실행, 플래그는 `go run ./cmd --gpu-node-selector=...`).
`make deploy`(kustomize, namespace `kukie-oven-system`)는 kubebuilder 기본 경로로 남아 있지만 사용자 설치 경로는 차트다.

## 릴리스

`charts/kukie-gpu/Chart.yaml`의 `version`을 올린 커밋에 태그 `v<그 버전>`을 달면 `.github/workflows/release.yml`이 돈다.

1. 태그와 차트 버전이 다르면 실패한다.
2. 이미지 `ghcr.io/durokukie/kukie-oven:<버전>`과 `ghcr.io/durokukie/kukie-counter:<버전>`(`linux/amd64`)을 올린다.
3. 각 digest를 `values.yaml`에 채워(`scripts/set_image.py`) 차트 `oci://ghcr.io/durokukie/charts/kukie-gpu`를 올린다.

패키지는 공개 전까지 비공개로 둔다. 공개는 GitHub 패키지 설정에서 차트와 이미지를 각각 바꾼다. 그러면 위의 `helm install` 한 줄이 로그인 없이 된다.

## 예시

```sh
kubectl -n llm apply -f oven/config/samples/serving_v1alpha1_servedmodel.yaml
kubectl -n llm get servedmodels            # NAME  MODEL  PHASE  REASON  AGE
kubectl -n llm get sm gpt-oss-20b -o jsonpath='{.status}' | jq
kubectl -n llm get pvc,job,deploy,svc -l serving.kukie.io/servedmodel=gpt-oss-20b
```

gated 모델은 `kubectl -n llm create secret generic hf-token --from-literal=token=<HF 토큰>` 뒤
`spec.source.tokenSecretRef: hf-token`. 토큰은 Job의 `HF_TOKEN` 환경변수로만 들어가고 status·로그에 쓰지 않는다.

## 내려받기 진행률

`status.download`는 다른 레포(에이전트 `gpu.served_models[]`, 웹)가 읽는 약속이다. 이름이나 뜻을 바꾸려면 함께 고친다.

| 필드 | 뜻 |
| --- | --- |
| `percent` | 받은 용량 ÷ 전체 용량, 버림. Job이 성공하기 전에는 최대 99, 성공하면 100. 전체를 모르면 0 |
| `downloadedBytes` | 마지막으로 읽은 받은 용량(선택). 아직 못 읽었으면 없다. 전체를 넘으면 전체로 줄인다 |
| `totalBytes` | 허깅페이스의 전체 용량(선택). 모르면 없다 |

| 상황 | `status.download` |
| --- | --- |
| Job을 막 만듦, 새 출처로 다시 받음 | `percent: 0`만 |
| 로그를 못 읽음, 줄이 깨짐, 시도 실패 후 재시도, 노드 대기 | 이전 값 유지. phase는 바꾸지 않는다 |
| 전체를 모른 채 받는 중 | `percent: 0`과 `downloadedBytes`. 앞서 안 전체가 있으면 그 값을 계속 쓴다(시도마다 다시 묻기 때문이다) |
| Job 성공, 전체를 앎 | `percent: 100`, 두 값 모두 전체 용량 |
| Job 성공, 전체를 모름 | `percent: 100`만 |
| Job 실패(`DownloadFailed`) | `percent: 0`만 |
| `Stopped` | 멈춘 때의 값 그대로 |

**Job 쪽 (`oven/internal/controller/download.py`).** `hf download`를 자식으로 돌리고, 5초마다 다음 한 줄을 stdout에 쓴다. 종료 코드는 `hf`의 것이다. 크기를 재다 실패해도, 허깅페이스 API가 안 돼도 그 줄만 잃고 내려받기는 그대로 간다. API 호출이 실패하면 60초 뒤 다시 묻는다. 줄에는 숫자만 넣고 토큰은 넣지 않는다.

```json
{"event": "download_progress", "downloadedBytes": 123, "totalBytes": 456}
```

* 전체 용량: `GET $HF_ENDPOINT/api/models/<모델>?blobs=true`의 `siblings`. 내용이 같은 파일은 캐시에 한 번만 저장되므로 `lfs.sha256`(없으면 `blobId`) 기준으로 한 번만 더한다. 그냥 더하면 끝나도 100%에 닿지 않는다(`sentence-transformers/all-MiniLM-L6-v2`는 977MB와 931MB로 5% 차이).
* 받은 용량: `$HF_HOME/hub/models--<org>--<name>/blobs/`를 심볼릭 링크를 따라 잰다. 폴더 크기를 그대로 재면 안 된다. vLLM `v0.32.0` 이미지의 `huggingface_hub` 2.2.0·`hf_xet` 1.7.0은 파일이 끝나면 실제 내용을 `hub/blobs/<앞 2자>/<xet 해시>` 공용 저장소로 옮기고 모델 폴더에는 링크만 남겨서, 폴더 크기는 파일이 끝날 때마다 줄어든다.

  | 경로 | 내용 |
  | --- | --- |
  | `blobs/<etag>` | 끝난 파일. Xet이 아닌 파일은 실제 파일, Xet 파일은 공용 저장소로 가는 링크 |
  | `blobs/<etag>.<임시 8자>.incomplete` | 쓰는 중인 파일. 받는 동안 크기가 는다(실제 할당과 같아 희소 파일이 아니다) |

* 시도마다 임시 이름이 달라 이어받지 않는다. 강제 종료된 시도의 파일은 남으므로 etag마다 한 번만 센다(끝난 파일이 있으면 그것, 없으면 가장 긴 임시 파일). 그래서 재시도 직후에는 새 파일이 예전 길이를 넘을 때까지 숫자가 그대로다.
* 진행률 막대는 `HF_HUB_DISABLE_PROGRESS_BARS=1`로 끈다. 로그에서 쓸모가 없고, 줄 끝이 없으면 다음 진행률 줄을 망친다.
* 같은 모델의 예전 리비전 파일도 폴더에 있으면 함께 세어 실제보다 커 보일 수 있다. 전체를 넘으면 전체로 줄이고 99에서 멈춘다.

**오븐 쪽 (`download.go`).** 컨테이너가 실행 중인 `JobRunning`에서만 파드 로그 끝 10줄(최대 64KiB)을 읽는다. 다른 때(`PreparingPod`, `Retrying`, `WaitingForNode`, Job이 끝난 뒤, 서빙 중, `Stopped`)에는 읽지 않아 평소 서빙에 부담이 없다. 모델마다 10초에 한 번이다. status가 바뀌면 reconcile이 다시 깨어나지만 그때는 읽지 않는다. 한 번 읽는 데 5초를 넘기면 포기한다. reconcile은 한 번에 하나만 돌아 kubelet이 답하지 않아도 다른 모델을 오래 막지 않게 하려는 것이다. 로그를 읽는 데 실패하면 오븐 로그에만 남긴다.

`kubectl -n llm get sm gpt-oss-20b -o jsonpath='{.status.download}'`, 원본 줄은 `kubectl -n llm logs job/<다운로드 Job>`.

## 실행 시간 상한

모델을 켜 놓고 잊으면 GPU 노드 요금이 계속 나간다. 노드 그룹 자동 꺼짐 Lambda는 관리형 노드 그룹만 보고 Karpenter 노드를 보지 못한다. 그래서 오븐이 모델을 내리고, GPU 파드가 없어지면 노드 자동 확장기가 노드를 내린다. Kukie 서버가 멈춰도 돈다.

| 항목 | 동작 |
| --- | --- |
| 상한 | `spec.maxRuntime`. 비면 `--max-runtime`(차트 `oven.maxRuntime`, 기본 `2h`). `"0"`이면 저절로 멈추지 않는다. `spec.maxRuntime`은 시·분·초만 받는다(`2h`, `1h30m`, `90m`). `1d`, `1.5h`, `1h90m`은 API 서버가 거부한다 |
| 시작 시각 | `status.startedAt`. 오븐이 처음 처리할 때 한 번 적고 바꾸지 않는다. 오븐이 재시작해도 그대로다. 노드 대기·다운로드·적재 시간도 상한에 들어간다. 모델을 처음 받는 경우 상한이 다운로드보다 짧으면 끝까지 못 받으니 크게 잡는다 |
| 멈추는 시각 | `status.stopsAt`. `startedAt + 상한`과 keep-until 중 늦은 쪽. 상한이 없으면 비어 있다 |
| 멈출 때 | 끝나지 않은 Job(다운로드·측정)을 지우고 vLLM을 0대로 내린 뒤 `Stopped/MaxRuntimeReached`. PVC·Service·끝난 Job·기준선은 남는다. 어떤 phase에서든(`Failed`, `Pending/WaitingForNode` 포함) 멈춘다. `Failed`였으면 그 이유를 `status.message`에 이어 적는다 |
| 연장 | annotation `serving.kukie.io/keep-until`=RFC3339 시각. 그 시각 전에는 멈추지 않는다. 앞당기지는 못한다. 형식이 틀리면 무시하고 오븐 로그에만 남으니 `status.stopsAt`이 바뀌었는지 본다 |
| 측정 중 | 돌고 있는 측정 Job이 있으면 끝날 때까지 기다린다(그 Job의 시간 상한 `--measure-deadline` 안에 끝난다). 결과를 기록하고 바로 멈춘다. 상한이 지난 뒤에는 새 측정을 시작하지 않는다. annotation을 지우면 기다리지 않는다 |
| 멈춘 뒤 | 그대로 멈춰 있다. keep-until, `spec.maxRuntime`, 출처를 바꿔도 다시 뜨지 않고, 누가 Deployment를 1대로 올려도 0대로 내린다. `spec.storage.retainOnDelete`만 따른다 |
| 다시 시작 | ServedModel을 지우고 다시 만든다. 그때 `startedAt`을 새로 적는다. `retainOnDelete: true`면 받은 파일을 다시 쓴다. 아니면 PVC가 함께 지워져 다시 받는다 |

**`status.stopsAt`은 다른 레포가 읽는 약속이다.** 에이전트 개요의 `served_models[].stops_at`과 웹의 남은 시간 표시가 이 값을 쓴다. 상한이 매니저 플래그에서 올 수 있어 밖에서는 계산할 수 없기 때문이다. 이름을 바꾸거나 뜻을 바꾸려면 에이전트와 웹을 함께 고친다.

| 값 | 뜻 |
| --- | --- |
| RFC3339 시각(초 단위) | 그 시각에 멈춘다. `keep-until`이나 `spec.maxRuntime`을 바꾸면 다음 처리에서 다시 계산한다 |
| 비어 있음 | 저절로 멈추지 않는다(상한 `0`). 오븐이 아직 처음 처리하지 않은 모델도 비어 있다 |
| 지난 시각, phase `Measuring` | 측정이 끝나기를 기다리는 중이다. 끝나면 바로 멈춘다 |
| 지난 시각, phase `Stopped` | 마지막 값이 그대로 남는다. 멈춘 정확한 시각은 아니고, 오븐이 내려가 있었거나 측정을 기다렸으면 실제로는 더 늦게 멈췄다 |

```bash
kubectl -n llm get sm gpt-oss-20b -o jsonpath='{.status.startedAt}{" "}{.status.stopsAt}{"\n"}'
kubectl -n llm annotate sm gpt-oss-20b serving.kukie.io/keep-until=2026-11-05T09:00:00+09:00 --overwrite   # 연장
kubectl -n llm patch sm gpt-oss-20b --type merge -p '{"spec":{"maxRuntime":"0"}}'                          # 저절로 멈추지 않게
```

오븐이 내려가 있는 동안에는 멈추지 못한다. 다시 뜨면 첫 처리에서 바로 멈춘다.

## 측정

모델이 동시에 몇 요청까지 SLA를 지키는지 재서, 나중에 어드미션 프록시가 쓸 기준선을 남긴다. GPU를 가득 채우고
그동안 실제 요청이 느려지므로 사람이 달아야 돈다.

```bash
kubectl -n llm annotate sm gpt-oss-20b serving.kukie.io/measure=true   # 시작
kubectl -n llm get sm gpt-oss-20b -w                                    # Measuring → Ready
kubectl -n llm get sm gpt-oss-20b -o jsonpath='{.status.baseline}'      # 결과
kubectl -n llm get cm kukie-gpu-baseline-gpt-oss-20b -o yaml            # 단계별 표는 baseline.json
kubectl -n llm get cm -l serving.kukie.io/measure-result=true,serving.kukie.io/servedmodel-name=gpt-oss-20b   # 측정 기록
kubectl -n llm logs job/gpt-oss-20b-measure-<generation>                # 자세한 기록
kubectl -n llm annotate sm gpt-oss-20b serving.kukie.io/measure=m-2 --overwrite   # 다시 재기: 값을 바꾼다
kubectl -n llm annotate sm gpt-oss-20b serving.kukie.io/measure-        # 끝내기, 돌던 측정도 멈춘다
```

| 항목 | 값 |
| --- | --- |
| 요청 모양 | 입력 약 1,000토큰, 출력 200토큰, 단계마다 최소 40개 또는 동시 수의 8배. **단계마다 새 프롬프트**(아래) |
| 단계 | 동시 1·2·4·8·16·32·64. SLA를 넘는 첫 단계에서 멈춘다 |
| SLA | TTFT p95 1000ms 이하, ITL p95 50ms 이하, 요청 전부 완료 |
| 시간 상한 | 1시간(`oven.measure.deadline`). 넘기기 전에 멈추고 잰 데까지 결과를 남긴다 |
| 기준선 | SLA 안 마지막 단계의 동시 수, 그때 본 대기 요청 수 최댓값, KV 캐시 % 최댓값 |

- 값은 `oven.measure` values로 바꾼다.
- **단계마다, 측정마다 다른 seed를 쓴다.** 차트 `0.6.1`부터다. `vllm bench serve`의 seed는 기본이 고정이라, 그 전 측정은 단계마다 앞 단계의 프롬프트를 다시 보내 vLLM prefix cache가 받았다. **`0.6.0` 이하로 잰 기준선은 서버 한계가 아니다**(`gpt-oss-120b`의 8·16, `gpt-oss-20b`의 32). 지금 값은 프롬프트가 겹치지 않는 경우라 실제 요청보다 나쁜 쪽이다. 결과의 `seed`에 동시 수를 더한 값이 그 단계의 seed다. 오븐이 만드는 Job은 매번 새 값을 뽑으므로 같은 프롬프트로 다시 잴 수 없고, 환경 변수 `SEED`는 `measure.py`를 직접 돌릴 때만 쓴다.
- **annotation 값이 요청이다.** 빈 값이 아니면 모두 요청이고(`true`도 된다) 빈 값은 annotation이 없는 것과 같다. 화면의 측정 버튼은 서버를 거쳐 `m-<epoch 초>`를 적는다.
- **한 값, 한 spec에 한 번만 돈다.** 측정이 끝나면(성공·실패 모두) `status.measureRequest`에 그 값을 적는다. 이 값이 annotation과 같고 조건 `Measured`의 세대가 지금 세대이면 처리한 것이다. 그래서 값을 그대로 두면 실패했어도, Job을 지워도 다시 돌지 않는다. 다시 재려면 값을 바꾸거나 annotation을 지웠다가 다시 단다. 지우면 `status.measureRequest`도 비운다. spec이 바뀌면(세대가 오르면) 다시 돌고, 그 전까지 `status.baseline.generation`이 예전 값이면 예전 설정의 수치다.
- **측정 중에 값을 바꾸면** 돌던 Job을 지우고 새 Job을 만든다. 지운 Job의 부하가 아직 남아 있으면 새 측정이 `EngineBusy`로 끝날 수 있다. 그러면 요청이 빠진 뒤 값을 한 번 더 바꾼다. 끝났지만 아직 읽지 않은 Job은 지우기 전에 결과를 먼저 적는다(그 Job의 세대가 지금 세대일 때만).
- **처리 중이거나 대기 중인 요청이 있으면 거절한다**(`EngineBusy`). 서빙 중인 모델의 측정 금지.
- 측정이 실패해도 모델은 `Failed`가 아니다. phase는 `Ready`로 돌아오고 `reason`과 조건 `Measured`에 이유가 남는다.

| reason | 뜻 |
| --- | --- |
| `Measured` | 끝났다. 기준선 기록 완료 |
| `MeasuredPartial` | 클라이언트 오류나 시간 상한으로 도중에 끝났다. 잰 데까지 기준선을 적고 `status.baseline.partial`과 ConfigMap `partial`이 `true`다. 실제 한계보다 낮은 값일 수 있으니 다시 잰다 |
| `EngineBusy` | 처리 중이거나 대기 중인 요청이 있어 시작하지 않았다 |
| `NoStepWithinSLA` | 동시 1부터 SLA를 넘었다. 기준선 없음 |
| `EngineUnreachable` | vLLM `/metrics`를 읽지 못했다 |
| `MeasureFailed` | Job이 결과 없이 끝났다(시간 상한, 메모리 등). 로그 확인 |
| `MeasureJobGone` | 측정 Job이 끝나기 전에 사라졌다. 저절로 다시 돌지 않는다 |
| `BaselineConflict` | 같은 이름의 ConfigMap이 있고 이 ServedModel 것이 아니다. 건드리지 않는다. Job이 남아 있으면 30초마다 다시 보고, 치우면 기록한다. 측정 기록은 이미 남아 있다 |

측정 Pod는 vLLM과 **같은 노드**에 뜬다(podAffinity). 그 노드에 이미지가 있고, 토크나이저가 든 RWO 볼륨을 읽기 전용으로 같이 쓰며, 1단계 측정도 같은 조건이었다. 측정 부하가 같은 노드의 CPU를 쓰므로 노드 CPU가 모자라면 수치가 낮게 나올 수 있다. GPU는 요청하지 않고 Kubernetes 토큰도 받지 않는다. 결과는 파드의 termination message(최대 4096바이트)로 나가고, 오븐이 읽어 `status.baseline`, ConfigMap `kukie-gpu-baseline-<모델>`, 측정 기록에 적는다.

### 측정 기록

`Measured`, `MeasuredPartial`로 끝난 실행마다 ConfigMap을 하나 남긴다. 저장 위치는 고객 클러스터이고 Kukie 서버 DB에 넣지 않는다. 에이전트가 읽어 `gpu.benchmarks`로 보내므로, 라벨과 data 키는 다른 레포와의 약속이다. 바꾸려면 함께 고친다.

| 항목 | 값 |
| --- | --- |
| 이름 | `kukie-gpu-measure-<모델>-<UTC yyyymmddhhmmss>`. 시각은 Job이 끝난 시각이라 같은 실행을 다시 처리해도 이름이 같고, 또 만들지 않는다 |
| 라벨 | `serving.kukie.io/measure-result: "true"`, `serving.kukie.io/servedmodel-name: <모델>`. `serving.kukie.io/servedmodel`은 달지 않는다 |
| 주인 | 없음. 모델을 지워도 남고, 같은 이름으로 다시 만들어도 예전 잔재로 치지 않는다 |
| data | `baseline.json`(측정 결과 원문), `model`, `gpus`, `engineImage`, `engineArgs`(JSON 배열), `measuredAt`(RFC3339), `partial`, `maxConcurrency`, `gpuType`(`라벨=값`, 차트 `0.10.0`부터), `measureConcurrency`(그 Job의 동시 단계, `0.10.0`부터) |
| 보관 | 모델 이름별 최근 20개. 새 기록을 만들 때 오래된 것을 지운다. 지우다 실패하면 오븐 로그에만 남고 다음 기록 때 다시 지운다 |

`EngineBusy`처럼 잰 값이 없는 실패는 기록을 남기지 않는다. 이유는 `status.reason`에 있다. 기록까지 모두 지우려면 `kubectl -n llm delete cm -l serving.kukie.io/measure-result=true,serving.kukie.io/servedmodel-name=<모델>`.

`gpuType`은 측정 Pod가 뜬 노드(= vLLM 노드)의 라벨에서 읽는다. `--gpu-type-labels` 중 노드에 있는 첫 라벨을 `nvidia.com/gpu.product=NVIDIA-L40S`처럼 적는다. 노드가 이미 없거나 어느 라벨도 없으면 빈 값이고, 그 기록은 재사용하지 않는다.

### 기준선 재사용

ServedModel을 지우면 `status.baseline`도 사라진다. 같은 모델을 다시 띄울 때마다 재면 GPU를 1시간 가까이 채우는데, 같은 조건 두 번 측정이 동시 16/16, TTFT p95 486/474ms, ITL p95 16.7/16.7ms로 거의 같았다(2026-11-10, kukie-gpu L40S). 그래서 오븐은 기준선이 없는 모델에 남은 측정 기록을 채운다. 카운터(discovery)는 다음 list(30초 안)에 그 값으로 한도를 정한다.

- **언제**: 모델이 `Ready`이고, measure annotation이 없고, `status.baseline`이 없거나 `generation`이 지금 세대보다 낮을 때. vLLM Pod의 노드를 알아야 하므로 `Ready` 전에는 하지 않는다. 그 Pod의 이미지와 인자가 지금 spec과 같아야 한다. spec을 바꾼 직후 아직 보이는 옛 Pod의 노드로 GPU 종류를 정하지 않으려는 것이다.
- **SLA 실패 뒤 같은 세대에서는 안 한다**: 측정이 첫 단계부터 SLA를 넘으면(`NoStepWithinSLA`) 그 세대를 `status.reuseBlockedGeneration`에 남긴다. 그 세대에서는 재사용하지 않고, annotation을 지워 조건 `Measured`가 사라져도 남는다. 그때 기준선이 재사용 값(`reusedFrom`)이었으면 `status.baseline`과 `kukie-gpu-baseline-<모델>`도 지운다(카운터는 기본값으로 돌아간다). 직접 잰 기준선은 지금처럼 그대로 둔다. 첫 단계도 끝내지 못하고 멈춘 실행(클라이언트 오류, 시간 상한)은 reason이 같아도 `measure.py`가 `partial: true`를 붙이고, 오븐은 막지 않는다. `EngineBusy`, `MeasureFailed` 같은 다른 실패도 서버 한계를 말하지 않으므로 막지 않는다. **한계**: ServedModel을 다시 만들거나 측정과 상관없는 spec(예: `maxRuntime`)을 바꿔 세대가 오르면 막힘이 풀리고, 같은 조건의 예전 성공 기록이 다시 쓰인다. SLA 실패를 의심하면 재사용을 끄거나(`oven.measure.reuseMaxAge=0`) 다시 잰다. 조건 기준으로 기억하는 일은 Linear DURO-211.
- **무엇을**: 같은 namespace에서 라벨 `serving.kukie.io/servedmodel-name`이 같은 이름인 측정 기록 중 아래가 **모두 같은** 가장 최근 것.

| 비교 | 지금 값 | 기록 |
| --- | --- | --- |
| 모델 | `spec.source.huggingface` | `model` |
| 엔진 이미지(digest 포함) | `spec.engine.image` | `engineImage` |
| GPU 수 | `spec.engine.gpus` | `gpus` |
| 엔진 인자(순서까지) | `spec.engine.args` | `engineArgs` |
| GPU 종류 | vLLM Pod 노드의 `--gpu-type-labels` | `gpuType` |
| 동시 단계 | `--measure-concurrency` | `measureConcurrency` |
| SLA·요청 모양 | `--measure-ttft-ms`, `--measure-itl-ms`, `--measure-input-tokens`, `--measure-output-tokens` | `baseline.json`의 `sla` |

- **쓰지 않는 기록**: `partial: true`, `gpuType`이나 `measureConcurrency`가 없는 옛 기록(`0.10.0` 전), `--measure-reuse-max-age`(기본 30일)보다 오래된 기록. partial은 시간 상한이나 클라이언트 오류로 도중에 끊긴 하한값이라, 복사하면 다시 잴 계기 없이 낮은 한도가 계속 남는다. 다시 재면 끝까지 잴 수 있으니 재사용하지 않는다. 30일은 노드 드라이버·커널 같은 오븐이 볼 수 없는 변화가 오래 남지 않게 둔 상한이다.
- **표시**: `status.baseline.reusedFrom`에 원본 ConfigMap 이름, `measuredAt`은 원본 측정 시각, `generation`은 지금 `metadata.generation`. ConfigMap `kukie-gpu-baseline-<모델>`에도 `reusedFrom`이 들어간다. 재사용은 새 측정 기록을 만들지 않고 조건 `Measured`도 바꾸지 않는다.
- **annotation이 우선이다.** measure annotation이 있으면 재사용하지 않고 지금처럼 새로 잰다. 새 측정이 끝나면 `reusedFrom`은 비워진다.
- 맞는 기록이 없으면 기준선은 비거나 예전 세대 값 그대로다(카운터는 기본 4). 같은 세대·같은 노드에서는 한 번만 찾는다(오븐 메모리, 재시작하면 한 번 더). 찾다가 오류가 나거나 `kukie-gpu-baseline-<모델>` 자리에 남의 ConfigMap이 있으면 모델은 `Ready` 그대로 두고 오븐 로그에 남긴 뒤 30초 뒤 다시 본다(ConfigMap·Node 변화는 오븐을 깨우지 않는다).
- 끄기: `oven.measure.reuseMaxAge=0`.

```bash
kubectl -n llm get sm gpt-oss-20b -o jsonpath='{.status.baseline.reusedFrom}'   # 비어 있으면 직접 잰 값
```


## kukie-gpu에서 실기 확인 (사용자가 켠다)

**전제 — 스토리지.** `kukie-gpu`에는 EBS CSI 애드온과 기본 클래스 `ebs-csi-default-sc`(`volumeBindingMode: WaitForFirstConsumer`, 실측)가 있다(2026-11-03 적용, kukie-gpu-infra `runbooks/cluster.md` '스토리지'). 볼륨은 AZ 하나에 묶이므로, 모델 볼륨의 AZ와 다음 GPU 노드의 AZ가 다르면 vLLM Pod가 `volume node affinity conflict`로 Pending에 머문다.

GPU 노드 g6e.xlarge는 시간당 $2.288이다. [gpu-view](gpu-view.md) "볼 때마다"대로 1대 켜고, 끝나면 바로 0대로 내린다.

1. 대상 클러스터를 고르고(`kubectl config use-context kukie-gpu`), `ghcr-pull` Secret을 namespace `kukie-gpu`에 만든 뒤 차트로 올린다. 아래 명령은 1층이 아직 손 apply였던 **2026-11-03의 첫 설치 기록**이다. 1층을 끄는 값이 들어 있고 `--reuse-values`가 없으니 **지금 kukie-gpu에서 다시 돌리지 않는다**(1층 네 리소스가 지워진다). 지금은 `--reset-then-reuse-values`로 바꿀 값만 준다(위 "차트를 올릴 때").

   ```sh
   helm upgrade --install kukie-gpu charts/kukie-gpu --namespace kukie-gpu \
     --set dcgm.enabled=false --set rbac.enabled=false --set preflight.enabled=false --set namespaces.create=false \
     --set oven.enabled=true --set oven.image.tag=0.4.1 \
     --set oven.image.digest=sha256:82c087f8f84bbbe1a932dee5959adc41196c0f59bded905890f8a1d9bc87dc70 \
     --set 'oven.imagePullSecrets[0].name=ghcr-pull' --wait --timeout 120s
   ```
2. `kubectl -n llm apply -f oven/config/samples/serving_v1alpha1_servedmodel.yaml`. 노드가 0대면 다운로드 Job Pod가
   Pending이고 phase는 `Pending/WaitingForNode`(0.4.x는 `Failed/Unschedulable`), 메시지는 `node(s) didn't match Pod's node affinity/selector`(nodeSelector
   `kukie.io/accelerator=nvidia-l40s`에 맞는 노드가 없다는 뜻이지 설정 오류가 아니다). 노드가 뜨면 혼자 `Downloading`으로 돌아간다.
   `Insufficient nvidia.com/gpu`는 3번 이후 vLLM Pod에서만 나온다.
3. 노드 1대를 켜면 다운로드(13GB, NAT 약 100MB/s) → `Loading` → `Ready`. `kubectl -n llm get sm -w`로 본다.
4. 기존 `vllm-gpt-oss-20b` Deployment와 같은 노드의 GPU 1장을 두고 겨루므로 확인 중에는 기존 Deployment를 0으로 줄인다:
   `kubectl -n llm scale deploy vllm-gpt-oss-20b --replicas=0`.
5. 끝나면 ServedModel을 지우고(PVC는 `retainOnDelete: true`라 남는다. 지우려면 `kubectl -n llm delete pvc gpt-oss-20b-models`)
   노드를 0대로 내린다.

## 권한

권한은 `oven/config/rbac/role.yaml`(controller-gen 생성)에 둘로 나뉜다. 차트가 이 파일을 그대로 읽는다.

- **ClusterRole `kukie-oven`**: `nodes` get 하나. 측정한 노드와 vLLM 노드의 GPU 종류 라벨을 읽는다(캐시하지 않아 list·watch가 필요 없다). 노드를 만들거나 바꾸지 않는다. GPU 노드를 켜고 끄는 것은 Karpenter가 Pending Pod를 보고 한다.
- **Role `kukie-oven`(감시하는 namespace마다)**: 아래 나머지 전부. 오븐은 ServedModel과 **같은 namespace**에만 PVC·Job·Deployment·Service·ConfigMap을 만든다(ownerReference가 같은 namespace에서만 걸린다). 그래서 쓰기는 그 namespace 안에서 끝난다.

Role의 규칙: `servedmodels`는 읽기와 status 쓰기만,
`persistentvolumeclaims`·`services`·`deployments` get/list/watch/create/patch, `jobs`는 delete까지, `pods`는 읽기만, `pods/log`는 get만(내려받는 동안 다운로드 Pod의 로그를 읽는 데만 쓴다).
`configmaps` get/create/patch는 기준선과 측정 기록을 적는 데, list는 기준선 재사용에 맞는 기록을 찾는 데와 측정 기록을 최근 20개만 남기는 데(delete와 함께) 쓴다. 지우는 것은 측정 기록 라벨이 달린 그 모델의 ConfigMap뿐이다(캐시하지 않아 클러스터의 ConfigMap을 담아 두지 않고, 그래서 watch도 필요 없다). Secret은 읽지 않는다(토큰은 kubelet이 Job Pod에 넣는다). 지우는 건 옛 다운로드 Job과 측정 Job, 실행 시간 상한이 지났을 때 끝나지 않은 Job뿐이고 나머지는 가비지 컬렉션에 맡긴다. 실행 시간 상한은 새 권한을 쓰지 않는다(Job delete, Deployment patch).

**감시할 namespace는 차트 값 `oven.namespaces`로 정한다.** 비우면 `namespaces.llm`(기본 `llm`) 하나다. 차트는 적은 namespace마다 Role·RoleBinding을 만들고, 같은 목록을 `--watch-namespaces`로 넘긴다. 매니저의 캐시는 그 namespace만 list·watch하므로 클러스터 범위의 읽기 권한이 필요 없다. 목록 밖 namespace의 ServedModel은 처리하지 않고(상태도 쓰지 않는다) 그대로 둔다. namespace 이름 형식이 틀리면 렌더가 실패한다. 리더 선출 Lease는 그대로 설치 namespace의 Role(`kukie-oven-leader-election`)을 쓴다.

0.12.0까지는 위 Role 규칙이 전부 ClusterRole에 있어 쓰기와 파드 로그 읽기가 클러스터 전체에 열려 있었다(Linear DURO-179에서 좁힘). 줄이면서 동사나 리소스는 하나도 늘리지 않았다(`scripts/test_oven_chart.py`가 0.12.0 목록과 대조한다).

`make test`의 envtest는 매니저를 이 생성 규칙만 가진 사용자로 띄운다. 클러스터 범위는 ClusterRole, 미리 만든 namespace들에는 Role만 주고 캐시도 그 namespace로 좁힌다. 그래서 다운로드·띄우기·측정·기록 정리·기준선 재사용 시험이 모두 이 권한만으로 돈다. 목록 밖 namespace의 ServedModel을 건드리지 않는 것, 그 namespace에 쓰기와 노드 list가 거부되는 것도 따로 본다. 파드 로그는 envtest에 kubelet이 없어 가짜로 읽으므로, `pods/log` get은 차트 테스트가 Role에 있는지만 본다.

## 검증

```sh
cd oven
make manifests generate && git diff --exit-code -- . ../charts/kukie-gpu/files/oven   # 생성물 최신 여부 (CI와 같음)
go vet ./...
make test                                         # envtest(생성 Role·ClusterRole만 가진 사용자, 감시 namespace 밖 무시): PVC→Job→Deployment→Ready, Failed 분기, 노드 대기, 실행 시간 상한, 내려받기 진행률, 출처 변경, retainOnDelete 토글, 측정 요청·기록, 기준선 재사용
python3 -m unittest scripts/test_download.py      # download.py: 캐시 구조, 전체 용량, 진행률 줄, 종료 코드
```

envtest에는 kube-controller-manager·kubelet이 없어 테스트가 Job 완료와 Pod 상태를 직접 써 넣는다. 가비지 컬렉션(삭제 시 정리)은
envtest에서 돌지 않으므로 ownerReference 유무로 검사하고, 실제 삭제는 kind 또는 kukie-gpu에서 본다. ReplicaSet 컨트롤러도 없어 "파드가 사라진다"는 Deployment `replicas: 0`까지만 본다. 시각은 `ServedModelReconciler.Now`로 주입한다. kubelet이 없어 파드 로그도 읽을 수 없으므로 `ServedModelReconciler.Logs`(`PodLogReader`)에 가짜를 꽂는다. 진짜 구현은 가짜 clientset으로 요청 옵션만 확인한다.

## 범위 밖 (처음 버전)

* 실패 원인 분류·힌트(DURO-171), 어드미션 프록시 사이드카, Kukie 화면(DURO-173)
* S3·PVC 출처, 멀티노드, vLLM 이외 엔진, 모델 여러 개 동시, 모델 파일 GC, CRD 버전 올리기
* PVC 프로비저닝 실패(이벤트에만 남는 것)의 status 반영
* preflight의 Karpenter NodePool 검사: preflight ServiceAccount에 `nodepools.karpenter.sh` 클러스터 읽기 권한을 더해야 한다. 차트의 권한을 늘리지 않으려고 후속으로 둔다. 그동안 Karpenter 유무는 에이전트 개요(`gpu.node_pools`)가 Kukie 연결 권한으로 읽는다

## 정할 것

| 항목 | 지금 기본값 |
| --- | --- |
| CRD 그룹 | `serving.kukie.io` (설계 문서 예시 그대로) |
| 다운로드 컨테이너 이미지 | `spec.engine.image`(vLLM 이미지의 `hf` CLI). 작은 전용 이미지로 바꾸려면 `--download-image` |
| 어드미션 프록시가 들어올 때 Service 포트 | 8000 → 8080 |
| 권한 범위 | 감시 namespace(`oven.namespaces`, 기본 `llm`)마다 Role, 클러스터 범위는 노드 get만(0.13.0, DURO-179) |
