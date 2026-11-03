# Kukie GPU 오퍼레이터

사용자 GPU 클러스터에 끼우는 Kukie GPU 애드온이다. GPU 상태를 보고(관측), SLA 안에서 받을 수 있는 동시 요청 수를 재고(측정), 허깅페이스 모델 이름만 적어 vLLM으로 띄운다(서빙). 설계는 Linear Kukie Serving 프로젝트의 `[결정] GPU 서빙 애드온` 문서가 기준이다.

우리 클러스터 `kukie-gpu`의 Terraform, 측정 스크립트, 모델 후보 수집은 [kukie-gpu-infra](https://github.com/durokukie/kukie-gpu-infra)에 있다. 이 저장소에는 사용자 클러스터에 설치되는 것만 둔다.

## 구성과 적용 방식

| 경로 | 담당 | 적용 방식 | 문서 |
| --- | --- | --- | --- |
| `.github/workflows` | Claude PR 리뷰 | | |
| `history` | 적용 결과 기록 | | |
| `operator` | 애드온 3층(서빙) Go 오퍼레이터: `ServedModel`(`serving.kukie.io/v1alpha1`)에 허깅페이스 모델 이름을 적으면 PVC → 다운로드 Job → vLLM Deployment·Service → `/health` → status 기록을 돌린다. CI는 `operator.yml` | 코드만 있음. 이미지 레지스트리 미정 | [operator](runbooks/operator.md) |

## 아직 적용하지 않은 것

- 애드온은 아직 어느 클러스터에도 설치하지 않았다.
- `operator`는 이미지를 올리지 않았고 kukie-gpu에 배포하지 않았다. kukie-gpu에는 PVC를 묶을 EBS CSI 드라이버가 아직 없다(Linear DURO-178). 측정 Job, 실패 원인 분류, 어드미션 프록시, Helm 차트 포장은 아직이다.
