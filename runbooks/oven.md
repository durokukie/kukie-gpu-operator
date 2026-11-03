# kukie-oven: ServedModel 오퍼레이터 (애드온 3층 서빙, 아직 실기 전)

`oven/`(`kukie-oven`)은 `ServedModel`(`serving.kukie.io/v1alpha1`)에 허깅페이스 모델 이름을 적으면 PVC → 다운로드 Job → vLLM
Deployment·Service → `/health` 확인 → status 기록을 사람 손 없이 돌리는 Go 오퍼레이터(kubebuilder·controller-runtime)다.
설계는 Linear `[설계] ServedModel 오퍼레이터 · 실패 진단 · 어드미션 프록시` 1·2절, 상위 결정은 `[결정] GPU 서빙 애드온` 3·6절.

**지금 상태**: 차트에 들어갔고 kind에서 설치까지 확인했다. 이미지와 차트 `0.3.0`을 GHCR에 비공개로 올렸다(아래 "릴리스"). `kukie-gpu`에
배포하지 않았고, GPU에서 모델을 띄우는 실기 확인은 아직이다(아래 "kukie-gpu에서 실기 확인").

## 상태 흐름

| phase | 뜻 | 다음 |
| --- | --- | --- |
| `Pending` | 방금 만들어짐. PVC와 다운로드 Job을 만든다 | `Downloading` |
| `Downloading` | 다운로드 Job 실행 중. `status.download.percent`는 0, 끝나면 100 (세부 진행률은 후속). 한 시도가 0이 아닌 코드로 끝나 Job이 다시 시도 중이면 `reason: Retrying`에 종료 코드를 적는다 | `Loading` / `Failed` |
| `Loading` | vLLM Deployment를 만들었고 Pod가 `/health`에 답하기를 기다린다 | `Ready` / `Failed` |
| `Ready` | Pod의 readinessProbe(`GET /health`)가 통과했다 | `Measuring` (annotation) |
| `Measuring` | annotation `serving.kukie.io/measure: "true"`를 봤다. 측정 Job은 DURO-169에서 만든다. annotation을 지우면 `Ready` | `Ready` |
| `Failed` | 다운로드 Job이 재시도(backoffLimit 2)를 다 쓰고 실패(`DownloadFailed`, 마지막 시도의 종료 코드 포함), 또는 다운로드·vLLM Pod가 Unschedulable·ImagePullBackOff·CrashLoopBackOff·비정상 종료. `status.reason`은 쿠버네티스가 준 이유 그대로, `status.message`는 그 메시지 그대로 | Pod가 살아나면 자동으로 `Downloading`/`Loading`→`Ready`. 원인 분류·힌트는 DURO-171 |

`Failed`는 Pod 상태만 보고 정한다(이벤트·로그는 DURO-171). 다운로드 Job의 Pod와 vLLM Pod 둘 다 본다. GPU 노드가 0대일 때
scheduler 메시지가 `status.message`에 그대로 들어가는데, 둘이 다르다: 다운로드 Pod는 GPU를 요청하지 않고 nodeSelector로만
GPU 노드에 가므로 `0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.`이고, vLLM Pod는
`nvidia.com/gpu`를 요청하므로 `… Insufficient nvidia.com/gpu.`다. 노드가 올라오면 혼자 풀린다. PVC가 묶이지 않으면
`pod has unbound immediate PersistentVolumeClaims`가 들어간다. 단, PVC 프로비저닝 자체의 실패(StorageClass는 있는데 CSI
드라이버 오류 등)는 이벤트에만 남아 아직 안 보인다.

## 하위 리소스

ServedModel `NAME`(namespace `NS`)마다 다음을 만든다. 전부 label `serving.kukie.io/servedmodel=NAME`을 달고
server-side apply(field manager `kukie-oven`)로 "있어야 할 모양"에 맞춘다. 같은 spec이면 몇 번을 돌아도 같은 결과다.

| 리소스 | 이름 | ownerReference | 비고 |
| --- | --- | --- | --- |
| PVC | `NAME-models` | `spec.storage.retainOnDelete: false`일 때만 | ReadWriteOnce, `spec.storage.size`. 만든 뒤 spec은 건드리지 않는다(storageClassName 불변, 크기는 사용자가 직접 늘린다). `retainOnDelete`를 바꾸면 ownerReference만 붙였다 뗀다 |
| Job | `NAME-download-<해시8>` | 있음 | `hf download <모델>`을 `HF_HOME=/models`로. 해시는 모델·tokenSecretRef·다운로드 이미지에서 나오므로 출처가 바뀌면 새 Job을 만들고 옛 Job은 지운다. 끝난 Job은 "받았다"는 표시로 남긴다 |
| Deployment | `NAME` | 있음 | `kubernetes/llm/vllm-gpt-oss-20b.yaml`과 같은 모양: Recreate, GPU taint toleration, `/health` startupProbe(40분)·readinessProbe, `/dev/shm` 2Gi. args는 `--model`, `--tensor-parallel-size`(=gpus), `--port 8000` 뒤에 `spec.engine.args`. `HF_HUB_OFFLINE=1`로 PVC 파일만 쓴다. 어드미션 프록시 사이드카 자리는 비어 있다 |
| Service | `NAME` | 있음 | `:8000` → vLLM. 사이드카가 들어오면 8080으로 바꾼다 |

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
| `--download-image` | 비어 있음 (= `spec.engine.image`) | `hf` CLI가 있는 다운로드용 이미지 |
| `--download-deadline` | `2h` | 다운로드 Job 하나의 시간 상한(재시도 포함). 넘으면 `Failed/DownloadFailed`(`DeadlineExceeded`). 멈춘 다운로드가 GPU 노드를 계속 잡지 않게 한다. 아주 큰 모델은 올린다 |

## 설치

`oven.enabled`를 켜면 차트가 CRD, ServiceAccount, ClusterRole, 리더 선출 Role, Deployment `kukie-oven`을 설치 namespace에 올린다.
기본은 꺼 둔다. 파드는 `kukie-oven-<해시>-<무작위>`로 뜬다.

```sh
# GHCR에 올린 차트: 이미지 digest가 차트에 들어 있다 (패키지가 비공개면 helm registry login이 먼저 필요하다)
helm install kukie-gpu oci://ghcr.io/durokukie/charts/kukie-gpu --namespace kukie-gpu --create-namespace --set oven.enabled=true

# 저장소의 차트: 이미지를 직접 지정한다
helm install kukie-gpu charts/kukie-gpu --namespace kukie-gpu --create-namespace \
  --set oven.enabled=true --set oven.image.tag=<버전>

kubectl -n kukie-gpu rollout status deployment/kukie-oven
```

비공개 패키지면 설치 namespace에 pull Secret을 만들고 `--set 'oven.imagePullSecrets[0].name=<Secret 이름>'`을 더한다.
`--gpu-node-selector`는 `gpuNode.nodeSelector`에서, `--download-deadline`·`--download-image`는 `oven.*` 값에서 만든다.

**CRD는 uninstall해도 남는다**(`helm.sh/resource-policy: keep`). CRD가 지워지면 사용자의 `ServedModel`이 전부 함께 지워지기 때문이다.
같은 이유로 오븐을 켜면 차트가 만든 `namespaces.llm`도 남긴다. namespace가 지워지면 그 안의 ServedModel과 모델 PVC가 함께 사라진다.
완전히 지우려면 `kubectl delete crd servedmodels.serving.kukie.io`를 직접 한다. 그러면 ServedModel과 그 소유 리소스가 지워진다.

차트의 CRD·권한은 `charts/kukie-gpu/files/oven/`의 복사본이다. `make -C oven manifests`가 함께 갱신하고, CI가 어긋나면 실패한다.

로컬에서만 돌려 보려면 `make run`(현재 kubeconfig로 매니저를 호스트에서 실행, 플래그는 `go run ./cmd --gpu-node-selector=...`).
`make deploy`(kustomize, namespace `kukie-oven-system`)는 kubebuilder 기본 경로로 남아 있지만 사용자 설치 경로는 차트다.

## 릴리스

`charts/kukie-gpu/Chart.yaml`의 `version`을 올린 커밋에 태그 `v<그 버전>`을 달면 `.github/workflows/release.yml`이 돈다.

1. 태그와 차트 버전이 다르면 실패한다.
2. 이미지 `ghcr.io/durokukie/kukie-oven:<버전>`(`linux/amd64`)을 올린다.
3. 그 digest를 `values.yaml`에 채워(`scripts/set_oven_image.py`) 차트 `oci://ghcr.io/durokukie/charts/kukie-gpu`를 올린다.

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

측정 요청: `kubectl -n llm annotate sm gpt-oss-20b serving.kukie.io/measure=true` → `Measuring`. 지금은 Job을 만들지
않는다(DURO-169). `kubectl annotate ... serving.kukie.io/measure-`로 지우면 `Ready`.

## kukie-gpu에서 실기 확인 (사용자가 켠다)

**전제 — 스토리지.** `kukie-gpu`에는 EBS CSI 애드온과 기본 클래스 `ebs-csi-default-sc`(`volumeBindingMode: WaitForFirstConsumer`, 실측)가 있다(2026-11-03 적용, kukie-gpu-infra `runbooks/cluster.md` '스토리지'). 볼륨은 AZ 하나에 묶이므로, 모델 볼륨의 AZ와 다음 GPU 노드의 AZ가 다르면 vLLM Pod가 `volume node affinity conflict`로 Pending에 머문다.

GPU 노드 g6e.xlarge는 시간당 $2.288이다. [gpu-view](gpu-view.md) "볼 때마다"대로 1대 켜고, 끝나면 바로 0대로 내린다.

1. 위 "설치"대로 차트로 올린다(먼저 `kubectl config use-context kukie-gpu`).
2. `kubectl -n llm apply -f oven/config/samples/serving_v1alpha1_servedmodel.yaml`. 노드가 0대면 다운로드 Job Pod가
   Pending이고 phase는 `Failed/Unschedulable`, 메시지는 `node(s) didn't match Pod's node affinity/selector`(nodeSelector
   `kukie.io/accelerator=nvidia-l40s`에 맞는 노드가 없다는 뜻이지 설정 오류가 아니다). 노드가 뜨면 혼자 `Downloading`으로 돌아간다.
   `Insufficient nvidia.com/gpu`는 3번 이후 vLLM Pod에서만 나온다.
3. 노드 1대를 켜면 다운로드(13GB, NAT 약 100MB/s) → `Loading` → `Ready`. `kubectl -n llm get sm -w`로 본다.
4. 기존 `vllm-gpt-oss-20b` Deployment와 같은 노드의 GPU 1장을 두고 겨루므로 확인 중에는 기존 Deployment를 0으로 줄인다:
   `kubectl -n llm scale deploy vllm-gpt-oss-20b --replicas=0`.
5. 끝나면 ServedModel을 지우고(PVC는 `retainOnDelete: true`라 남는다. 지우려면 `kubectl -n llm delete pvc gpt-oss-20b-models`)
   노드를 0대로 내린다.

## 권한

매니저 ClusterRole(`oven/config/rbac/role.yaml`, controller-gen 생성): `servedmodels`는 읽기와 status 쓰기만,
`persistentvolumeclaims`·`services`·`deployments` get/list/watch/create/patch, `jobs`는 delete까지, `pods`는 읽기만.
Secret은 읽지 않는다(토큰은 kubelet이 Job Pod에 넣는다). 지우는 건 옛 다운로드 Job뿐이고 나머지는 가비지 컬렉션에 맡긴다.

**아직 ClusterRole이라 쓰기 권한이 클러스터 전체에 열려 있다.** AGENTS.md의 "2·3층은 namespace 안의 쓰기만"에 아직 못 맞췄다(아래 "정할 것"). 지금은 코드만 있고 어디에도 배포하지 않았다.

## 검증

```sh
cd oven
make manifests generate && git diff --exit-code -- . ../charts/kukie-gpu/files/oven   # 생성물 최신 여부 (CI와 같음)
go vet ./...
make test                                         # envtest: PVC→Job→Deployment→Ready, Failed 분기, 출처 변경, retainOnDelete 토글
```

envtest에는 kube-controller-manager·kubelet이 없어 테스트가 Job 완료와 Pod 상태를 직접 써 넣는다. 가비지 컬렉션(삭제 시 정리)은
envtest에서 돌지 않으므로 ownerReference 유무로 검사하고, 실제 삭제는 kind 또는 kukie-gpu에서 본다.

## 범위 밖 (처음 버전)

* 실패 원인 분류·힌트(DURO-171), 측정 Job·baseline ConfigMap(DURO-169), 어드미션 프록시 사이드카, Kukie 화면(DURO-173)
* S3·PVC 출처, 멀티노드, vLLM 이외 엔진, 모델 여러 개 동시, 모델 파일 GC, CRD 버전 올리기, 다운로드 세부 진행률
* PVC 프로비저닝 실패(이벤트에만 남는 것)의 status 반영

## 정할 것

| 항목 | 지금 기본값 |
| --- | --- |
| CRD 그룹 | `serving.kukie.io` (설계 문서 예시 그대로) |
| 다운로드 컨테이너 이미지 | `spec.engine.image`(vLLM 이미지의 `hf` CLI). 작은 전용 이미지로 바꾸려면 `--download-image` |
| 어드미션 프록시가 들어올 때 Service 포트 | 8000 → 8080 |
| 권한 범위 | ClusterRole. 차트에서 `oven.enabled`를 기본 꺼 둔 이유 중 하나다. 공개 전에 설치 namespace의 Role·RoleBinding과 감시 namespace 제한으로 좁힌다(Linear DURO-179) |
