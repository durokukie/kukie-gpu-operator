# Kukie GPU 오퍼레이터

사용자 GPU 클러스터에 끼우는 Kukie GPU 애드온이다. GPU 상태를 보고(관측), SLA 안에서 받을 수 있는 동시 요청 수를 재고(측정), 허깅페이스 모델 이름만 적어 vLLM으로 띄운다(서빙). 설계는 Linear Kukie Serving 프로젝트의 `[결정] GPU 서빙 애드온` 문서가 기준이다.

우리 클러스터 `kukie-gpu`의 Terraform, 측정 스크립트, 모델 후보 수집은 [kukie-gpu-infra](https://github.com/durokukie/kukie-gpu-infra)에 있다. 이 저장소에는 사용자 클러스터에 설치되는 것만 둔다.

## 구성과 적용 방식

| 경로 | 담당 | 적용 방식 | 문서 |
| --- | --- | --- | --- |
| `charts/kukie-gpu` | 애드온 1층(관측) Helm 차트: DCGM exporter(GPU 지표), Kukie 읽기 RoleBinding, 설치 직후 빠진 것(GPU·드라이버·허깅페이스·스토리지·권한)을 ConfigMap에 남기는 preflight Job, 선택으로 NVIDIA 장치 플러그인과 데모 vLLM. 기본값은 kukie-gpu 기준이고 다른 클러스터는 values로 맞춘다 | 관리자 `helm install` | [gpu-view](runbooks/gpu-view.md) |
| `kubernetes`, `scripts/chart_diff.py` | 손으로 apply 하던 YAML과, 차트 렌더가 그 YAML과 같은지 CI에서 비교하는 스크립트. 차트가 원본이고 이 YAML은 비교 기준이다 | (설치하지 않음) | [gpu-view](runbooks/gpu-view.md) |
| `.github/workflows` | Claude PR 리뷰, 차트 검사(`chart.yml`), 오븐 검사(`oven.yml`), 카운터 검사(`counter.yml`), GHCR 릴리스(`release.yml`) | | |
| `history` | 적용 결과 기록 | | |
| `oven` | 애드온 3층(서빙) Go 오퍼레이터 `kukie-oven`: `ServedModel`(`serving.kukie.io/v1alpha1`)에 허깅페이스 모델 이름을 적으면 PVC → 다운로드 Job → vLLM Deployment·Service → `/health` → status 기록을 돌린다. CI는 `oven.yml` | 차트 `oven.enabled`(기본 꺼짐). 태그 `v<차트 버전>`으로 GHCR에 릴리스 | [oven](runbooks/oven.md) |
| `counter` | 초대제 API 입구 Go 게이트웨이 `kukie-counter`: API 키 확인, 모델별 동시 수 한도(넘으면 429), 스트리밍 전달, 지표. 쿠버네티스 API를 읽지 않는다. CI는 `counter.yml` | 차트 `counter.enabled`(기본 꺼짐). 오븐과 함께 태그 `v<차트 버전>`으로 GHCR에 릴리스 | [counter](runbooks/counter.md) |

## 아직 적용하지 않은 것

- kukie-gpu에는 같은 내용이 손 apply로 올라가 있어, 차트를 설치할 때 기존 리소스를 Helm 소유로 넘긴다(runbooks/gpu-view.md).
- 패키지는 GHCR에 비공개다(`0.6.1`까지 올렸다). kukie-gpu에는 오븐만 차트로 설치했고(`0.6.1`까지, [history](history/2026-11.md)), 1층은 아직 손 apply 그대로다. 실패 원인 분류, 어드미션 프록시는 아직이다.
- Karpenter로 노드가 뜨고 내려가는 실제 시험은 아직이다. preflight의 Karpenter NodePool 검사는 preflight에 클러스터 읽기 권한을 더해야 해서 후속으로 둔다.
- 카운터(`kukie-counter`)는 차트에 넣었지만(`0.7.0`) 아직 릴리스·설치하지 않았고, kind와 가짜 vLLM으로만 시험했다. 공개 입구(`counter.ingress`, `0.8.0`)는 코드만 있고 쓰지 않았다.
