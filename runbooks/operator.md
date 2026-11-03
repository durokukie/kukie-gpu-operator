# ServedModel 오퍼레이터 (애드온 3층 서빙, 코드만 있음)

`operator/`는 `ServedModel`(`serving.kukie.io/v1alpha1`)에 허깅페이스 모델 이름을 적으면 PVC → 다운로드 Job → vLLM
Deployment·Service → `/health` 확인 → status 기록을 사람 손 없이 돌리는 Go 오퍼레이터(kubebuilder·controller-runtime)다.
설계는 Linear `[설계] ServedModel 오퍼레이터 · 실패 진단 · 어드미션 프록시` 1·2절, 상위 결정은 `[결정] GPU 서빙 애드온` 3·6절.

**지금 상태**: 코드와 envtest만 있다. 이미지를 레지스트리에 올리지 않았고 `kukie-gpu`에 배포하지 않았다. GPU 노드가
0대라 실기 확인은 아직이다(아래 "kukie-gpu에서 실기 확인").

## 상태 흐름

| phase | 뜻 | 다음 |
| --- | --- | --- |
| `Pending` | 방금 만들어짐. PVC와 다운로드 Job을 만든다 | `Downloading` |
| `Downloading` | 다운로드 Job 실행 중. `status.download.percent`는 0, 끝나면 100 (세부 진행률은 후속) | `Loading` / `Failed` |
| `Loading` | vLLM Deployment를 만들었고 Pod가 `/health`에 답하기를 기다린다 | `Ready` / `Failed` |
| `Ready` | Pod의 readinessProbe(`GET /health`)가 통과했다 | `Measuring` (annotation) |
| `Measuring` | annotation `serving.kukie.io/measure: "true"`를 봤다. 측정 Job은 DURO-169에서 만든다. annotation을 지우면 `Ready` | `Ready` |
| `Failed` | 다운로드 Job 실패, 또는 Pod가 Unschedulable·ImagePullBackOff·CrashLoopBackOff·비정상 종료. `status.reason`은 쿠버네티스가 준 이유 그대로, `status.message`는 그 메시지 그대로 | Pod가 살아나면 자동으로 `Loading`→`Ready`. 원인 분류·힌트는 DURO-171 |

`Failed`는 Pod 상태만 보고 정한다(이벤트·로그는 DURO-171). 다운로드 Job의 Pod와 vLLM Pod 둘 다 본다. 예를 들어 GPU 노드가
0대면 scheduler 메시지 `0/2 nodes are available: 2 Insufficient nvidia.com/gpu.`가 `status.message`에 그대로 들어가고, 노드가
올라오면 혼자 풀린다. PVC가 묶이지 않으면 `pod has unbound immediate PersistentVolumeClaims`가 들어간다. 단, PVC 프로비저닝
자체의 실패(StorageClass는 있는데 CSI 드라이버 오류 등)는 이벤트에만 남아 아직 안 보인다.

## 하위 리소스

ServedModel `NAME`(namespace `NS`)마다 다음을 만든다. 전부 label `serving.kukie.io/servedmodel=NAME`을 달고
server-side apply(field manager `kukie-serving-operator`)로 "있어야 할 모양"에 맞춘다. 같은 spec이면 몇 번을 돌아도 같은 결과다.

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
* 다운로드 Job이 실패했을 때 같은 spec으로 다시 받으려면 그 Job을 지운다. 오퍼레이터가 같은 이름으로 다시 만든다.
* `spec.engine.*`을 바꾸면 Deployment가 바뀌어 Pod가 다시 뜬다(Recreate). `spec.source`를 바꾸면 새 다운로드 Job부터 다시 간다.

## 매니저 플래그

| 플래그 | 기본값 | 뜻 |
| --- | --- | --- |
| `--gpu-node-selector` | 비어 있음 | 다운로드 Job·Deployment의 nodeSelector(`key=value[,key=value]`). `config/manager/manager.yaml`은 kukie-gpu용 `kukie.io/accelerator=nvidia-l40s`를 넣어 둔다. 사용자 클러스터에서는 비우거나 그 클러스터 라벨로 바꾼다 |
| `--download-image` | 비어 있음 (= `spec.engine.image`) | `hf` CLI가 있는 다운로드용 이미지 |

## 설치 (kubectl 컨텍스트가 대상 클러스터를 가리킬 때)

```sh
cd operator
make install                                  # CRD servedmodels.serving.kukie.io
make docker-build IMG=<레지스트리>/kukie-serving-operator:<태그>   # 레지스트리는 미정 (아래 결정 목록)
docker push <레지스트리>/kukie-serving-operator:<태그>
make deploy IMG=<레지스트리>/kukie-serving-operator:<태그>         # namespace kukie-serving-operator-system
kubectl -n kukie-serving-operator-system rollout status deployment/kukie-serving-operator-controller-manager
```

로컬에서만 돌려 보려면 `make run`(현재 kubeconfig로 매니저를 호스트에서 실행, 플래그는 `go run ./cmd --gpu-node-selector=...`).
Helm 차트(`charts/kukie-gpu`)에 넣는 건 다음 이슈다.

## 예시

```sh
kubectl -n llm apply -f operator/config/samples/serving_v1alpha1_servedmodel.yaml
kubectl -n llm get servedmodels            # NAME  MODEL  PHASE  REASON  AGE
kubectl -n llm get sm gpt-oss-20b -o jsonpath='{.status}' | jq
kubectl -n llm get pvc,job,deploy,svc -l serving.kukie.io/servedmodel=gpt-oss-20b
```

gated 모델은 `kubectl -n llm create secret generic hf-token --from-literal=token=<HF 토큰>` 뒤
`spec.source.tokenSecretRef: hf-token`. 토큰은 Job의 `HF_TOKEN` 환경변수로만 들어가고 status·로그에 쓰지 않는다.

측정 요청: `kubectl -n llm annotate sm gpt-oss-20b serving.kukie.io/measure=true` → `Measuring`. 지금은 Job을 만들지
않는다(DURO-169). `kubectl annotate ... serving.kukie.io/measure-`로 지우면 `Ready`.

## kukie-gpu에서 실기 확인 (사용자가 켠다)

**전제 — PVC를 묶을 스토리지가 아직 없다.** `kukie-gpu`(EKS 1.36, `terraform/dev/cluster`)에는 `aws-ebs-csi-driver` 애드온도,
StorageClass도 없다(EKS 1.30부터 기본 `gp2` StorageClass를 만들지 않고, 기존 vLLM 매니페스트는 `emptyDir`라 PVC가 필요한 적이
없었다). 그대로 apply하면 PVC `gpt-oss-20b-models`가 `Pending`(`no storage class is set`)에 머물고 다운로드 Job Pod는
`pod has unbound immediate PersistentVolumeClaims`로 스케줄되지 않는다(이때 phase는 `Failed`, `status.message`에 그 문장이
들어간다). 먼저 `terraform/dev/cluster`에 ① `aws-ebs-csi-driver` 애드온, ② 드라이버용 IAM 역할(Pod Identity 또는 IRSA),
③ `gp3` StorageClass(`volumeBindingMode: WaitForFirstConsumer`)를 더해 적용해야 한다. Terraform root 변경이라 별도 PR이다.

GPU 노드 g6e.xlarge는 시간당 $2.288이다. [gpu-view](gpu-view.md) "볼 때마다"대로 1대 켜고, 끝나면 바로 0대로 내린다.

1. 위 "설치"대로 CRD·매니저를 올린다(먼저 `kubectl --context kukie-gpu`).
2. `kubectl -n llm apply -f operator/config/samples/serving_v1alpha1_servedmodel.yaml`. 노드가 0대면 Job Pod가 Pending이고
   phase는 `Failed/Unschedulable`(`Insufficient nvidia.com/gpu`)이다. 노드가 뜨면 혼자 `Downloading`으로 돌아간다.
3. 노드 1대를 켜면 다운로드(13GB, NAT 약 100MB/s) → `Loading` → `Ready`. `kubectl -n llm get sm -w`로 본다.
4. 기존 `vllm-gpt-oss-20b` Deployment와 같은 노드의 GPU 1장을 두고 겨루므로 확인 중에는 기존 Deployment를 0으로 줄인다:
   `kubectl -n llm scale deploy vllm-gpt-oss-20b --replicas=0`.
5. 끝나면 ServedModel을 지우고(PVC는 `retainOnDelete: true`라 남는다. 지우려면 `kubectl -n llm delete pvc gpt-oss-20b-models`)
   노드를 0대로 내린다.

## 권한

매니저 ClusterRole(`operator/config/rbac/role.yaml`, controller-gen 생성): `servedmodels`(+status) 전부,
`persistentvolumeclaims`·`services`·`deployments` get/list/watch/create/patch, `jobs`는 delete까지, `pods`는 읽기만.
Secret은 읽지 않는다(토큰은 kubelet이 Job Pod에 넣는다). 지우는 건 옛 다운로드 Job뿐이고 나머지는 가비지 컬렉션에 맡긴다.

## 검증

```sh
cd operator
make manifests generate && git diff --exit-code   # 생성물 최신 여부 (CI와 같음)
go vet ./...
make test                                         # envtest: PVC→Job→Deployment→Ready, Failed 분기, 출처 변경, retainOnDelete 토글
```

envtest에는 kube-controller-manager·kubelet이 없어 테스트가 Job 완료와 Pod 상태를 직접 써 넣는다. 가비지 컬렉션(삭제 시 정리)은
envtest에서 돌지 않으므로 ownerReference 유무로 검사하고, 실제 삭제는 kind 또는 kukie-gpu에서 본다.

## 범위 밖 (처음 버전)

* 실패 원인 분류·힌트(DURO-171), 측정 Job·baseline ConfigMap(DURO-169), 어드미션 프록시 사이드카, Helm 차트 포장, Kukie 화면(DURO-173)
* S3·PVC 출처, 멀티노드, vLLM 이외 엔진, 모델 여러 개 동시, 모델 파일 GC, CRD 버전 올리기, 다운로드 세부 진행률
* PVC 프로비저닝 실패(이벤트에만 남는 것)의 status 반영

## 정할 것

| 항목 | 지금 기본값 |
| --- | --- |
| 레포 위치 | `kukie-gpu-infra/operator/` (결정 문서 제안 그대로) |
| 이미지 레지스트리·태그 규칙 | 미정. `make docker-build IMG=...`만 있고 푸시하지 않는다 |
| CRD 그룹 | `serving.kukie.io` (설계 문서 예시 그대로) |
| 다운로드 컨테이너 이미지 | `spec.engine.image`(vLLM 이미지의 `hf` CLI). 작은 전용 이미지로 바꾸려면 `--download-image` |
| 어드미션 프록시가 들어올 때 Service 포트 | 8000 → 8080 |
