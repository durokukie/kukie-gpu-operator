# Kukie GPU 애드온

GPU가 있는 쿠버네티스 클러스터에 까는 Kukie 애드온이다. 하는 일은 세 가지다.

- **보기**: GPU 사용률, 메모리, 전력을 Kukie 화면에서 본다.
- **재기**: 이 GPU와 모델로 응답 속도 기준을 지키며 동시에 몇 개까지 받을 수 있는지 잰다.
- **띄우기**: 허깅페이스 모델 이름만 적으면 받아서 vLLM으로 띄우고, 다 쓰면 내린다.

애드온을 깔면 클러스터 안의 커넥터가 Kukie로 먼저 연결을 연다. 그래서 클러스터 쪽에 공개 주소나 열린 포트가 없어도 Kukie API로 모델을 부를 수 있다.

## 설치

차트와 이미지는 GHCR에 공개돼 있어서 로그인 없이 받는다. 커넥터 자격은 Kukie 화면(키 관리 → 연결 설정)에서 발급하고, 같은 화면이 아래 명령을 클러스터에 맞게 채워 준다.

```sh
kubectl create namespace kukie-gpu
read -rs CREDENTIAL && kubectl -n kukie-gpu create secret generic kukie-connector \
  --from-literal=credential="$CREDENTIAL" && unset CREDENTIAL
helm upgrade --install kukie-gpu oci://ghcr.io/durokukie/charts/kukie-gpu --version 0.15.0 -n kukie-gpu \
  --set oven.enabled=true --set counter.enabled=true --set counter.discovery.enabled=true \
  --set connector.enabled=true \
  --set connector.gateway=https://<Kukie 주소>/connector/v1/connect \
  --set connector.clusterId=<클러스터 ID>
```

자격은 values로 넣지 않는다. helm 기록에 남지 않게 Secret으로 따로 넣고, 커넥터가 처음 붙을 때 새 자격으로 바꾼다.

## 구성

| 경로 | 하는 일 | 문서 |
| --- | --- | --- |
| `charts/kukie-gpu` | 애드온 Helm 차트. GPU 지표(DCGM exporter), Kukie 읽기 권한, 설치 뒤 빠진 것을 알려 주는 점검 Job이 기본으로 깔리고, 아래 셋은 켜야 깔린다 | [gpu-view](runbooks/gpu-view.md) |
| `oven` | 오븐. `ServedModel`에 모델 이름을 적으면 저장소 → 다운로드 → vLLM → 상태 기록까지 한다. 실행 시간 상한, 측정, 실패 원인 분류도 여기서 한다 | [oven](runbooks/oven.md) |
| `counter` | 카운터. 모델 앞에서 키를 확인하고 동시 요청 수를 제한한다. 넘치면 429로 돌려보낸다. 한도는 측정값에서 시작하고, 적응형 한도를 켜면 실제 지연을 보며 조금씩 움직인다 | [counter](runbooks/counter.md) |
| `connector` | 커넥터. Kukie 게이트웨이로 연결을 유지하고, 받은 요청을 카운터로 넘긴다. 자격은 스스로 갱신한다 | [connector](runbooks/connector.md) |
| `kubernetes`, `scripts` | 예전에 손으로 apply 하던 YAML과, 차트 렌더가 그것과 같은지 보는 CI 스크립트 | |
| `history` | 우리 클러스터에 적용한 기록 | |

오븐·카운터·커넥터 이미지와 차트는 태그 `v<차트 버전>`을 달면 함께 GHCR에 올라간다.

우리 GPU 클러스터의 Terraform과 측정 스크립트는 [kukie-gpu-infra](https://github.com/durokukie/kukie-gpu-infra)에 있다. 이 저장소에는 사용자 클러스터에 깔리는 것만 둔다.

## 아직 안 된 것

- Karpenter로 GPU 노드가 뜨고 내려가는 것을 실제로 시험하지 않았다. 점검 Job의 Karpenter 검사도 클러스터 읽기 권한이 더 필요해서 미뤘다.
- 카운터를 실제 OpenAI SDK로 스트리밍해 보지 않았다. 지금까지는 curl과 자체 부하 스크립트로만 확인했다.
- 카운터 한도 조절에 남은 문제가 둘 있다. 긴 입력에서 한도가 연달아 내려가는 것과, 카운터를 거칠 때만 가끔 지연이 튀는 것이다. 원인은 아직 모른다.
- 우리 클러스터 `kukie-gpu`의 커넥터는 아직 손으로 올린 것이다. 차트 커넥터는 다른 시험 클러스터에서만 깔아 봤다.
