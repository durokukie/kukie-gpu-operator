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
| `connector` | 고객 클러스터에서 Kukie로 TLS WebSocket을 유지하고 기존 카운터에 전달하는 Go 모듈. 개인 키와 분리된 자격, 취소·재연결 | 독립 이미지/CLI. Helm 자동 설치·릴리스는 후속 | [connector](runbooks/connector.md) |
| `counter` | 초대제 API 입구 Go 게이트웨이 `kukie-counter`: API 키 확인, 모델별 동시 수 한도(넘으면 429, 켜면 ServedModel 기준선에서 정함), 스트리밍 전달, 지표. 기준선을 켤 때만 쿠버네티스 API에서 ServedModel을 읽는다. CI는 `counter.yml` | 차트 `counter.enabled`(기본 꺼짐). 오븐과 함께 태그 `v<차트 버전>`으로 GHCR에 릴리스 | [counter](runbooks/counter.md) |

## 아직 적용하지 않은 것

- 패키지는 GHCR에 비공개다(`0.14.1`까지 올렸다). kukie-gpu에는 오븐과 카운터를 차트로 설치했고(릴리스는 `0.14.1`, 오븐 이미지는 `0.14.1`·카운터 이미지는 `0.14.0`에 고정, [history](history/2026-11.md)). 카운터 적응형 한도는 `on`(조절한 한도를 쓰고 브레이크를 건다)으로 켰다. 내리기와 브레이크는 2026-11-11에 실제로 걸리는 것을 봤지만, 긴 입력에서 내리기가 연쇄로 이어지는 것과 카운터를 거칠 때만 생기는 간헐 지연(ITL p95 40ms)의 원인은 아직이다. TTFT 표본이 부족할 때 올리지 않게 한 수정(#43)은 0.14.0으로 클러스터에 올렸지만 부하로 확인하지는 않았다(DURO-214). 실패 원인 패턴표는 설치했고 2026-11-11에 실제 GPU 실패로 확인했다. 0이 아닌 429 비율을 화면에서 본 적은 아직 없다. 어드미션 프록시는 아직이다.
- Karpenter로 노드가 뜨고 내려가는 실제 시험은 아직이다. preflight의 Karpenter NodePool 검사는 preflight에 클러스터 읽기 권한을 더해야 해서 후속으로 둔다.
- 카운터(`kukie-counter`)를 실제 OpenAI SDK로 스트리밍하는 확인은 아직이다. 기준선 한도(`counter.discovery`)와 한도 초과 429는 2026-11-10에 실제 경로(curl)로 확인했다.
