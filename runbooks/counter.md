# kukie-counter: 키 발급제 API 입구 (DURO-190)

`counter/`(`kukie-counter`)는 OpenAI 호환 모델 서버(vLLM) 앞에 서는 Go 게이트웨이다. 비유하면 빵집 카운터다. 키를 확인하고 주방이 차면 돌려보낸다. 기본은 설정 파일과 키 파일만 읽는다. `discovery`를 켜면 같은 클러스터의 ServedModel을 읽어 주방 크기(측정 기준선)에 맞춰 한도를 스스로 정한다(아래 "기준선으로 한도 정하기").

**지금 상태**: 우리 클러스터 `kukie-gpu`(namespace `kukie-gpu`)에 차트 `0.9.0`으로 설치돼 있고 `counter.discovery`를 켰다(2026-11-10). 모델은 설정에 적은 `openai/gpt-oss-120b` 하나(`maxConcurrent: 4`, 사람 값이 이김)다. 2026-11-10에 ServedModel `gpt-oss-20b`를 띄워 측정하자 카운터가 스스로 경로를 만들고 한도를 4(`no_baseline`)에서 12(`baseline`, 측정 16 × 0.8)로 바꾸는 것을 실제로 봤다(history). 같은 날 임시 키로 한도 12를 채우고 13번째가 429(`Retry-After: 2`)를 받는 것도 확인했다. 실제 OpenAI SDK 스트리밍 확인은 아직이다.

## 실행

```sh
counter serve --config config.json --keys keys.json
```

| 플래그 | 기본값 | 뜻 |
| --- | --- | --- |
| `--config` | (필수) | 설정 파일. ConfigMap 마운트를 가정한다. 시작 때 한 번 읽는다 |
| `--keys` | (필수) | 키 파일. Secret 마운트를 가정한다. 주기적으로 다시 읽는다 |
| `--addr` | `:8080` | API 포트 |
| `--metrics-addr` | `:9090` | 지표 포트(`/metrics`). 밖으로 내지 않는다 |
| `--keys-reload-interval` | `10s` | 키 파일을 다시 읽는 주기 |
| `--shutdown-delay` | `5s` | SIGTERM 뒤 새 연결을 계속 받는 시간. 쿠버네티스가 이 파드로 요청을 보내지 않게 될 때까지 기다린다 |
| `--shutdown-timeout` | `25s` | SIGTERM 뒤 처리 중인 요청을 기다리는 시간. 지나면 끊는다. Pod의 `terminationGracePeriodSeconds`보다 짧게 둔다 |
| `--upstream-response-header-timeout` | `10m` | 모델이 응답 헤더를 줄 때까지 기다리는 시간. 넘으면 503이다. `0`은 제한 없음. 일반(비스트림) 응답은 생성이 끝나야 헤더가 오므로 가장 긴 생성보다 길게 둔다 |

설정이나 키 파일을 처음부터 읽지 못하면 시작하지 않는다.

## 차트로 설치

차트의 `counter.enabled=true`가 설치 namespace에 ServiceAccount, ConfigMap, Deployment, Service(ClusterIP `kukie-counter:8080`)를 만든다. 키 Secret은 사용자가 먼저 만들고, Ingress는 `counter.ingress.enabled=true`일 때만 만든다(아래 "공개 입구"). 아래 `<ns>`는 차트를 설치한 namespace다.

**1. 키를 만든다.** 키는 이 출력에만 있다. 파일 권한을 좁히고 끝나면 지운다.

```sh
umask 077
# 이미지로: docker run --rm ghcr.io/durokukie/kukie-counter:<버전> genkey --name <이름> > counter-genkey.txt
(cd counter && go run . genkey --name <이름>) > counter-genkey.txt
sed -n 1p counter-genkey.txt                                      # 키. 사용자에게 전달한다
printf '{"keys":[%s]}\n' "$(sed -n 2p counter-genkey.txt)" > keys.json   # 해시만 들어 있다
```

**2. Secret을 만든다.** data 키는 `keys.json`이어야 한다. 이름은 values의 `counter.keysSecret`과 같게 둔다.

```sh
kubectl -n <ns> create secret generic kukie-counter-keys --from-file=keys.json=keys.json
rm counter-genkey.txt keys.json
```

**3. 값 파일을 쓴다.** 모델 이름에 `.`이 들어가면 `--set`이 경로로 읽으므로 파일을 쓴다. `upstream`은 클러스터마다 달라 기본값이 없다. 오븐이 띄운 모델의 Service는 `:8000`이다.

```yaml
# counter-values.yaml
counter:
  enabled: true
  keysSecret: kukie-counter-keys
  imagePullSecrets:          # 비공개 패키지일 때. Secret은 <ns>에 있어야 한다
    - name: <pull Secret 이름>
  models:
    <요청의 model 값>:
      upstream: http://<Service>.<namespace>.svc:8000
      maxConcurrent: <사람이 정한 한도>   # discovery를 켜면 빼도 된다
```

오븐이 띄운 모델만 쓴다면 `models`를 비우고 `discovery`만 켜도 된다. 모델 이름, upstream, 한도를 ServedModel에서 정한다.

```yaml
counter:
  enabled: true
  keysSecret: kukie-counter-keys
  discovery:
    enabled: true          # namespace를 비우면 namespaces.llm(기본 llm)의 ServedModel을 읽는다
```

**4. helm으로 올린다.** 값이 틀리면(모델 없음, upstream 형식, 정수가 아닌 숫자, 모르는 값) 설치 시점에 실패한다. `<pull Secret 이름>`의 토큰이 새 패키지 `kukie-counter`도 읽을 수 있어야 한다.

```sh
# 이미 설치한 릴리스에 더할 때. --reuse-values가 아니다(아래 "업그레이드 옵션")
helm upgrade kukie-gpu oci://ghcr.io/durokukie/charts/kukie-gpu --version <버전> -n <ns> \
  --reset-then-reuse-values -f counter-values.yaml --wait --timeout 120s

# 처음 설치할 때
helm install kukie-gpu oci://ghcr.io/durokukie/charts/kukie-gpu --version <버전> -n <ns> --create-namespace \
  -f counter-values.yaml --wait --timeout 120s

# 저장소의 차트로 설치할 때는 이미지를 직접 지정한다: --set counter.image.tag=<버전> --set counter.image.digest=<digest>
```

### 확인

```sh
kubectl -n <ns> rollout status deployment/kukie-counter
kubectl -n <ns> logs deployment/kukie-counter | head -2      # "keys loaded", "counter started"
kubectl -n <ns> get pod -l app.kubernetes.io/name=kukie-counter -o wide   # GPU 노드가 아니어야 한다
```

Pod가 `ContainerCreating`에 머물면 `kubectl -n <ns> describe pod`의 `FailedMount`를 본다. Secret이 없거나(`secret "..." not found`) data 키가 `keys.json`이 아닌 경우다. Secret을 고치면 혼자 뜬다. `CrashLoopBackOff`면 로그에 이유가 있다(키 파일 JSON 오류 등).

### 포트포워드로 호출

```sh
kubectl -n <ns> port-forward svc/kukie-counter 8080:8080 &
read -rs KEY                                                 # 키를 붙여 넣는다. 셸 기록에 남지 않는다
curl -s localhost:8080/healthz
curl -s -H "Authorization: Bearer $KEY" localhost:8080/v1/models
curl -N -H "Authorization: Bearer $KEY" localhost:8080/v1/chat/completions \
  -d '{"model":"<요청의 model 값>","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"안녕"}]}'

# 지표는 Service에 없다. Pod로 직접 간다
kubectl -n <ns> port-forward deployment/kukie-counter 9090:9090 &
curl -s localhost:9090/metrics | grep kukie_counter_
```

### 키 추가와 폐기

Secret을 받아 고쳐 다시 올린다. `kubectl apply`는 last-applied annotation에 내용을 한 벌 더 남기므로 `replace`를 쓴다. 재시작은 필요 없고 1분 안팎이면 반영된다.

```sh
umask 077
kubectl -n <ns> get secret kukie-counter-keys -o jsonpath='{.data.keys\.json}' | base64 -d > keys.json
# keys에 줄을 더하거나 disabled를 true로 고친다
kubectl -n <ns> create secret generic kukie-counter-keys --from-file=keys.json=keys.json --dry-run=client -o yaml \
  | kubectl -n <ns> replace -f -
rm keys.json
```

### 업그레이드 옵션

`helm upgrade`의 값 옵션에 따라 결과가 다르다. Helm `v4.2.2`와 kind에서 차트 `0.6.1`을 설치해 `0.7.0`으로 올려 확인했다.

| 옵션 | 결과 |
| --- | --- |
| `--reset-then-reuse-values` | **이것을 쓴다.** 새 차트의 기본값 + 이전에 사용자가 넣은 값 + 이번 값 |
| `--reuse-values` | 새 차트의 기본값을 가져오지 않아 `.Values.counter`가 없다. 차트는 이때 카운터를 꺼진 것으로 보아 업그레이드는 통과한다. 이 옵션으로 카운터를 켜면 이유를 적은 메시지로 실패한다 |
| 옵션 없이 `-f`·`--set` | 이전에 넣은 값을 **버린다.** `kukie-gpu`처럼 오븐만 켠 릴리스는 오븐이 지워지고 1층이 켜진다. 쓰지 않는다 |

**`kubectl patch`로 고친 필드가 있으면** Helm 4(server-side apply)가 `conflict with "kubectl-patch"`로 실패한다. 그 전에 같은 릴리스의 다른 오브젝트(ConfigMap, Role 등)는 이미 바뀌어 있을 수 있다. 카운터 설정에 새 필드가 들어갔는데 옛 이미지가 남으면, 그 Pod가 재시작할 때 모르는 필드로 뜨지 못하므로 바로 마무리한다. 먼저 `helm get manifest`와 실제 Deployment를 비교해 다른 필드가 의도한 것뿐인지 본 뒤, 같은 명령에 `--force-conflicts`를 붙여 다시 올린다(2026-11-10 이미지 필드 하나로 겪었다).

`--reset-then-reuse-values`도 사용자가 넣은 값은 남긴다. `--set`으로 고정한 `oven.image.*`가 있으면 새 차트의 digest로 바뀌지 않는다. 지우려면 값을 `null`로 준다. 모델을 지울 때도 같다.

```yaml
counter:
  models:
    <지울 모델>: null
```

### 정한 것과 이유

| 항목 | 정한 것 | 이유 |
| --- | --- | --- |
| Pod 수 | 1 고정, values에 없음 | 자리 수를 Pod끼리 나누지 않아 Pod가 N개면 실제 한도가 N배가 된다. 값으로 두면 올리는 순간 한도가 조용히 깨진다. `counter.replicas`를 주면 실패한다 |
| 롤링 | `maxSurge: 1`, `maxUnavailable: 0` | 새 Pod가 준비된 뒤 옛 Pod를 내린다. 옛 Pod의 남은 스트림과 겹치는 동안(최대 25초) 한도를 넘을 수 있지만 초과분은 vLLM 대기열이 받는다. `Recreate`는 그 시간 동안 입구 전체가 닫힌다 |
| namespace | 설치 namespace(`namespaces.llm`이 아님) | 키 Secret과 pull Secret을 오븐과 같은 곳에 두고, 모델 namespace의 사용자와 나눈다. `llm`에는 Kukie 읽기 그룹의 Pod 프록시 권한이 묶이고, `namespaces.create=false`면 차트가 만들지도 않는다. upstream은 Service DNS라 namespace를 넘어도 된다. 다만 사용자 클러스터에 NetworkPolicy가 있으면 이 경로를 열어야 한다. 설치 namespace를 `gpu-metrics`로 두면 같은 이유로 Pod 프록시 권한이 닿으니 나눠 설치한다 |
| 지표 포트 9090 | Pod 포트로만, Service에 없음 | 인증이 없고 키 `id`와 모델 이름이 라벨로 나온다. Service는 나중에 밖으로 나갈 수 있는 입구라 API 포트만 둔다. 수집기는 Pod를 직접 긁는다(PodMonitor 등) |
| 인터넷 노출 | 기본 꺼짐. `counter.ingress`를 켜면 ALB로 낸다 | HTTPS만 받고 지표 포트는 나가지 않는다(아래 "공개 입구") |
| GPU 노드 | GPU taint를 허용하지 않고, `gpuNode.nodeSelector`에 맞는 노드를 affinity로 뺀다 | 오븐 파드와 같은 배치에 taint 없는 클러스터 대비를 더했다. 라벨이 여러 개면 전부 맞는 노드만 뺀다 |
| 노드 자동 확장기 | `do-not-disrupt`, PDB, 로컬 볼륨 없음 | 카운터가 GPU 노드에 뜨지 않으니 GPU 노드의 빈 노드 판단에 들어가지 않는다. Pending이어도 GPU taint를 허용하지 않아 GPU NodePool이 노드를 띄울 이유가 없다. 일반 노드에서는 언제든 옮겨진다. Karpenter로 실제 돌려 본 것은 아니고 kind에서 배치와 taint·affinity 거절까지만 봤다. Pod 1개에 PDB를 걸면 노드 비우기가 막히므로 두지 않는다 |
| 키 Secret 마운트 | `subPath` 없이 디렉터리로 | `subPath`로 올린 Secret은 고쳐도 Pod에 반영되지 않아 폐기가 재시작 전까지 먹지 않는다 |
| 재시작 | ConfigMap 해시만 Pod annotation에 | 설정은 시작 때 한 번 읽고, 키는 카운터가 다시 읽는다. ServedModel 기준선은 재시작 없이 반영된다 |
| 쿠버네티스 권한 | `discovery`를 켤 때만, 그 namespace의 `servedmodels` `get`·`list` Role 하나. 토큰은 Pod에만 올린다(ServiceAccount는 `automountServiceAccountToken: false` 그대로) | 주기 list라 `watch`가 필요 없다. Secret·Pod·ConfigMap은 읽지 않는다. 끄면 Role도 토큰도 없다 |
| CPU limit | 없음, 메모리 limit 512Mi | 쓰로틀링은 곧 스트림 지연이다. 메모리는 본문을 상한(기본 16MiB)까지 읽으니 큰 본문이 동시에 몰릴 때를 본 값이며 실측은 아니다 |
| 종료 | `--shutdown-delay=5s`, `--shutdown-timeout=25s`, grace 35초 | 스트림을 끝낼 시간을 다 쓰게 한다. 25초 안에 안 끝나는 스트림은 재시작 때 끊긴다 |

## 설정

```json
{
  "models": {
    "org/model-name": {"upstream": "http://model-name.my-namespace.svc:8000", "maxConcurrent": 8}
  },
  "retryAfterSeconds": 2,
  "maxBodyBytes": 16777216
}
```

| 필드 | 기본값 | 뜻 |
| --- | --- | --- |
| `models.<이름>` | (필수, `discovery`가 있으면 비워도 됨) | 요청의 `model`과 정확히 같은 이름. `unknown`은 지표 값과 겹쳐 쓸 수 없다 |
| `upstream` | (필수) | http(s) 주소. 사용자 정보와 쿼리는 넣을 수 없다 |
| `maxConcurrent` | (필수, `discovery`가 있으면 빼도 됨) | 동시 수 한도. 적으면 사람이 정한 값이 언제나 이긴다. `discovery`에서 빼면 기준선에서 정한다 |
| `discovery.namespace` | (켜면 필수) | ServedModel을 읽을 namespace. 차트는 비우면 `namespaces.llm` |
| `discovery.safetyFactor` | `0.8` | 한도 = `floor(maxConcurrency × safetyFactor)`, 최소 1. 0 초과 1 이하 |
| `discovery.defaultMaxConcurrent` | `4` | 쓸 수 있는 기준선이 없을 때의 한도 |
| `discovery.intervalSeconds` | `30` | ServedModel을 다시 읽는 주기 |
| `discovery.adaptive` | 없음(꺼짐). 차트 기본은 `observe` | 적응형 한도. 아래 "적응형 한도" |
| `retryAfterSeconds` | `2` | 429와 503의 `Retry-After` |
| `maxBodyBytes` | 16MiB | 요청 본문 상한 |

모르는 필드가 있으면 시작하지 않는다(오타가 기본값으로 묻히지 않게). `discovery`가 없으면 예전과 똑같이 동작하고, 모델 추가는 설정 변경과 재시작이다.

## 기준선으로 한도 정하기

`discovery`가 있으면 카운터가 `discovery.namespace`의 ServedModel을 주기적으로 읽어 모델과 한도를 정한다. 사람이 `status.baseline.maxConcurrency`를 보고 `maxConcurrent`를 옮겨 적는 단계가 없어진다. 기준선은 사람이 측정 annotation을 달아야 바뀌므로 30초 늦게 반영돼도 된다.

**읽는 방법.** 쿠버네티스 API에 `GET /apis/serving.kukie.io/v1alpha1/namespaces/<ns>/servedmodels`를 주기적으로 보낸다(Pod의 ServiceAccount 토큰, 매번 다시 읽음). watch·informer를 쓰지 않는 이유: client-go가 의존성 수십 개를 끌고 오고, 끊긴 watch를 다시 잇는 상태가 생긴다. 바뀌는 빈도(측정 한 번에 수 분)에 비해 30초 list는 API 서버에 거의 부담이 없고, 매번 전체를 다시 보므로 빠진 이벤트가 없다. 오븐 Go 타입은 import하지 않고 필요한 필드(`metadata.name`·`generation`, `spec.source.huggingface`, `spec.engine.args`, `status.phase`·`baseline`)만 읽는다.

**모델 이름과 upstream.** 이름은 vLLM이 받는 `model` 값이다. `spec.engine.args`에 `--served-model-name`이 있으면 그 값들, 없으면 `spec.source.huggingface`(오븐이 `--model`로 넘기는 값). upstream은 오븐이 만드는 Service `http://<metadata.name>.<ns>.svc:8000`이다. 한 ServedModel의 여러 이름은 자리 하나(한도 하나)를 같이 쓴다. `Stopped`이거나 지워지는 중인 ServedModel은 넣지 않는다. 두 ServedModel이 같은 이름을 내면 `metadata.name` 순으로 앞의 것을 쓰고 경고 로그를 남긴다. 자동으로 찾은 이름은 100개까지다(지표 줄 수 상한).

**한도.**

| 경우 | 한도 | `source` / `reason` |
| --- | --- | --- |
| 설정에 `maxConcurrent`가 있음 | 그 값. 기준선을 보지 않는다 | `config` / `configured` |
| 기준선 있음 | `floor(maxConcurrency × safetyFactor)`, 최소 1. 2026-11-04 L40S·gpt-oss-20b 실측 32면 25 | `baseline` / `measured` |
| `partial`(측정이 일찍 끝남) | 위와 같다. 값이 하한이라 그대로 써도 안전 쪽이다 | `baseline` / `partial` |
| 기준선 없음, `maxConcurrency` 0 | `defaultMaxConcurrent` | `default` / `no_baseline` |
| `baseline.generation` < `metadata.generation`(측정 뒤 스펙이 바뀜) | `defaultMaxConcurrent`와 옛 기준선 한도 중 작은 값 | `default` / `stale_baseline` |
| 설정 모델, upstream이 가리키는 ServedModel도 같은 이름의 ServedModel도 없음 | `defaultMaxConcurrent` | `default` / `no_servedmodel` |
| 설정 모델, 같은 이름의 ServedModel이 있지만 upstream이 실행 중인 ServedModel의 Service가 아님 | `defaultMaxConcurrent`(다른 서버에서 잰 값이라 쓰지 않는다) | `default` / `upstream_mismatch` |
| 설정 모델, 첫 list가 아직 성공 전 | `defaultMaxConcurrent` | `default` / `not_read_yet` |

**설정과 겹칠 때.** 사람이 적은 것이 이긴다. 설정의 모델 이름이 ServedModel과 같으면 설정의 upstream을 쓰고, `maxConcurrent`를 적었으면 그 값을 쓴다. 빼면 이름이 아니라 upstream이 가리키는 ServedModel(`<name>.<ns>`, `.svc`, `.svc.cluster.local`, 포트 8000)의 기준선을 쓴다. 설정 이름이 그 ServedModel의 served name이 아니어도 된다. upstream이 Service 꼴이 아니면 기본값이다. `discovery`가 있을 때 설정 모델의 upstream이 Service 꼴(`http://<a>.<b>[.svc[.cluster.local]]:8000`)이면, 자리 카운터는 upstream만 보고 ServedModel `<b>/<a>`의 것을 쓴다. list 결과가 바뀌어도 카운터가 새로 생기지 않으므로 처리 중인 요청이 계속 센다. 그 ServedModel의 다른 이름과도 자리(한도) 하나를 같이 쓰고, 지표와 `/v1/model-status`도 모든 이름에 같은 한도를 보인다. 이때 어느 이름에든 `maxConcurrent`를 적으면 그 값(여럿이면 가장 작은 값)이 서버 전체의 한도다. upstream이 다르면 따로 센다.

**실행 중 한도 바꾸기.** 자리는 버퍼 채널 대신 한도와 사용 수를 담은 작은 카운터다. 한도를 낮추면 이미 자리를 잡은 요청은 끝까지 가고, 사용 수가 새 한도 아래로 내려갈 때까지 새 요청은 429다. 높이면 바로 더 받는다. ServedModel이 사라지면 새 요청은 404(`model_not_found`)이고 처리 중인 요청은 끝까지 간다. 그 요청이 끝나기 전에 다시 나타나면 같은 자리 카운터를 이어 써서 한도를 넘지 않는다.

**읽기 실패.** API 오류, 권한 없음(403), CRD 없음(404)이면 직전의 모델과 한도를 그대로 쓰고 경고 로그와 `kukie_counter_discovery_errors_total`을 올린다. 첫 list가 실패해도 카운터는 뜨고(설정 모델만으로) 주기마다 다시 시도한다. 클러스터 밖에서 `discovery`를 켜면(`KUBERNETES_SERVICE_HOST` 없음) 시작하지 않는다.

**보는 곳.** 지표 `kukie_counter_limit{model}`(지금 한도), `kukie_counter_limit_source{model,source,reason}`(값 1), `/v1/model-status`의 `limits` 배열(`model`, `limit`, `source`, `reason`. 15초 준비 확인 캐시와 달리 매번 지금 값), 한도가 바뀔 때의 `model limit` 로그.

## 적응형 한도 (DURO-212)

설계: Linear 문서 "[설계] 카운터 적응형 동시 수 한도"(DURO-212). 기준선 한도는 조합(GPU·모델·설정)마다 미리 재야 하고 `× 0.8`에 근거가 없다. 2026-11-10 실측(L40S·gpt-oss-20b)에서 실제 한계는 24였는데 한도는 12였다. 그래서 카운터가 지나가는 **실제 요청의 꼬리 지연**을 보고 한도를 스스로 정한다.

**모드.**

| `mode` | 판단 | 실제 한도 | 브레이크 |
| --- | --- | --- | --- |
| `off`(블록 없음과 같음) | 안 함 | 위 "기준선으로 한도 정하기" | 없음 |
| `observe`(차트 기본) | 지표·로그로만 | 위 "기준선으로 한도 정하기" | 계산해 지표·로그로만 |
| `on` | 함 | **판단한 한도**(`source` `adaptive`) | 건다(429) |

`observe`로 먼저 낸다. 카운터가 잰 값이 맞는지 본 뒤 `on`으로 바꾼다(2026-11-11 kukie-gpu에서 `vllm bench`와 맞는 것을 확인했다, history). 되돌리기는 `mode`만 바꾸면 된다(설정이 바뀌어 Pod가 다시 뜬다).

**재는 것.** 스트리밍 요청(`"stream": true`이고 응답이 `text/event-stream`인 2xx)만 잰다.

| 값 | 어떻게 |
| --- | --- |
| TTFT | 전달 시작 → 생성된 글이 든 첫 조각. 역할만 든 첫 조각(`"content":""`)은 치지 않는다. 글이 든 조각은 `content`, `reasoning_content`, `reasoning`, `arguments`, `text` 문자열이 비지 않은 것 |
| ITL | 글이 든 **읽기와 읽기 사이**. 한 번에 조각 여럿이 와도 간격 하나다 |
| 처리 중 프리필 | 전달했지만 첫 토큰을 아직 못 받은 스트리밍 요청 수 |

- **클라이언트에 쓰는 시간은 뺀다.** 프록시는 모델에서 읽고 클라이언트에 쓰고 다시 읽는다. 클라이언트가 느리게 받으면 다음 읽기가 늦어지는데, 이 시간은 모델이 느린 것이 아니다. 그래서 모델을 기다린 시간(읽기 안의 시간)만 더한다. 일부러 느리게 받는 키가 모두의 한도를 내리지 못한다.
- 입력이 긴 요청은 TTFT 표본에서 뺀다(`ttftMaxPromptTokens`, 기본 4096). 길이는 `usage.prompt_tokens`, 없으면(대부분의 클라이언트는 `include_usage`를 보내지 않는다) 본문 4바이트를 1토큰으로 어림한다.
- 비스트리밍 요청은 첫 토큰을 알 수 없어 재지 않는다. 비스트리밍만 오는 모델은 판단이 `too_few_samples`·`hold`에 머문다.

**판단.** 서버마다 `intervalSeconds`(5초)마다, 최근 `windowSeconds`(30초)에 끝난 요청으로:

| 조건 | 판단(`reason`) |
| --- | --- |
| ITL·TTFT 표본이 둘 다 부족(ITL은 요청 10개·간격 200개 미만, TTFT는 요청 20개 미만) | 그대로(`too_few_samples`) |
| ITL(요청별 p95의 **중앙값**) > `slaItlMs` | `floor(한도 × 0.7)`, 최소 1(`decrease_itl`) |
| TTFT p95 > `slaTtftMs` | 같음(`decrease_ttft`) |
| ITL 표본 부족, 또는 어느 쪽이든 SLA의 80~100% | 그대로(`hold`). 깜빡임 방지 여유 폭. TTFT만으로는 내릴 수만 있고 올리지 않는다 |
| 줄인 뒤 3번(15초) | 그대로(`cooling`) |
| 창 안에서 처리 중 수가 한도의 90%에 못 미침 | 그대로(`not_full`). 손님이 적을 때 근거 없이 오르지 않게. 최고 처리 중 수도 주기마다 시각과 함께 적어 표본처럼 `windowSeconds` 안의 것만 본다 |
| `maxLimit`(256) | 그대로(`at_max`) |
| 그 밖(둘 다 SLA의 80% 안, 한도까지 찼음) | 한도 + 1(`increase`) |

- **TTFT는 요청 20개부터 본다.** TTFT는 요청마다 하나라 10개면 p95가 가장 느린 하나다. 2026-11-11 실측에서 모델이 쉬고 난 뒤 첫 요청(1.5초) 하나가 p95를 1.5초로 만들어 16 → 11로 내리는 판단이 나왔다(그때 p95는 0.18초). 20개면 SLA를 넘은 요청이 둘 이상이어야 p95가 넘는다.
- **바꾼 뒤 시작한 요청만 본다.** 요청은 자리를 잡을 때의 "한도 세대"를 갖고, 판단은 지금 세대의 요청만 쓴다. 줄이기 전에 시작해 늦게 끝난 느린 요청이 또 줄여 1까지 떨어지는 일이 없다. 대신 바꾼 뒤에는 새 요청 10개가 끝날 때까지 기다린다.
- **ITL은 요청별 p95의 중앙값이다.** 간격을 한 데 모으면 긴 응답 하나나 멈춘 요청 하나가 p95를 좌우한다.
- **시작값.** 기준선이 쓸 만하면(`baseline` / `measured`·`partial`) 그 `maxConcurrency`(× `safetyFactor` 없이). 아니면 지금 한도(`defaultMaxConcurrent` 등). `maxLimit`을 넘지 않는다. 사람이 `maxConcurrent`를 적은 서버는 판단하지 않는다.
- **`on`에서는 조절기가 한도의 주인이다.** 30초 갱신은 시작값과 사람이 적은 `maxConcurrent`만 넣고, 기준선 한도(× `safetyFactor`)를 다시 쓰지 않는다. `kukie_counter_limit`·`/v1/model-status`는 조절기의 한도를 바로 보인다. `source`는 `adaptive`, `reason`은 한도가 마지막으로 움직인 이유(`start`, `increase`, `decrease_itl`, `decrease_ttft`)다. 줄이면 처리 중인 요청은 끝까지 가고 새 요청만 막는다.
- **30초 갱신은 배운 값을 지우지 않는다.** 시작값의 근거(`source`/`reason`/값)가 바뀔 때만(다시 측정, 스펙 변경, 사람이 적음) 시작값으로 돌아간다. 카운터가 다시 뜨면 시작값부터다.
- 기대: 위 실측 조합이면 16에서 출발해 24 근처까지 오르고 28 근처에서 내려온다. 단 `observe`에서는 실제 한도(12)가 손님을 막아 "한도까지 찼음"이 그 위로 채워지지 않는다. 2026-11-11 실측에서 판단은 14에서 `not_full`로 멈췄다. 오르는 것은 `on`으로 시험한다.

**브레이크.** 조절기는 몇 초 단위로 움직이고, 동시 도착 몰림은 1~2초 안에 온다. 그래서 `brakeIntervalSeconds`(2초)마다 모델 서버의 vLLM `/metrics`를 직접 읽는다.

| 브레이크 | 조건 | `on`에서 |
| --- | --- | --- |
| `waiting` | `vllm:num_requests_waiting`(엔진 합)이 `brakeWaitingScrapes`(2)번 연속 0보다 큼 | 새 요청 429(`kukie_counter_rejected_total{reason="brake"}`) |
| `kv` | `vllm:kv_cache_usage_perc`(옛 이름 `gpu_cache_usage_perc`, 가장 높은 엔진)가 `brakeKvRatio`(0.9) 이상 | 같음 |

- **한도는 바꾸지 않는다.** 실측에서 대기는 SLA가 멀쩡한 동시 12·20에서도 몰림 순간에 생겼다. 이걸로 한도를 내리면 근거 없이 내려간다. 다음 읽기에서 조건이 풀리면 바로 푼다.
- 대기는 한 번이 아니라 연속으로 볼 때만 건다. 한 번은 손님이 같이 들어온 순간일 수 있다. KV는 차는 순간 이미 들어온 요청까지 밀려나므로(preemption) 한 번에 건다.
- **읽기 실패는 브레이크를 걸지 않는다**(걸려 있으면 푼다). 지표를 못 읽어서 모델이 막히지 않게 한다. 실패는 `kukie_counter_upstream_metrics_errors_total`로 센다.
- 오븐이 만든 Service(`http://<name>.<ns>.svc:8000`)만 읽는다. 다른 upstream의 `/metrics`는 vLLM 것이라는 보장이 없다.
- 사람이 `maxConcurrent`를 적은 서버에도 브레이크는 건다. 한도와 따로 노는 보호다.
- `observe`에서는 같은 계산을 `kukie_counter_brake_active`와 `brake` 로그(`enforced: false`)로만 낸다.

**설정.**

```json
"discovery": {
  "namespace": "llm",
  "adaptive": {"mode": "observe", "slaTtftMs": 1000, "slaItlMs": 50, "intervalSeconds": 5,
               "windowSeconds": 30, "maxLimit": 256, "ttftMaxPromptTokens": 4096,
               "brakeIntervalSeconds": 2, "brakeWaitingScrapes": 2, "brakeKvRatio": 0.9}
}
```

블록이 없거나 `mode`가 `off`면 판단하지 않는다(TTFT·ITL 지표는 그래도 낸다). 차트 values 파일에는 따옴표 없는 `mode: off`·`mode: on`도 된다(YAML 1.1이 false·true로 읽은 것을 차트가 되돌린다). 0인 값은 기본값이다. SLA는 오븐 측정 기본값과 같다. 모델별 SLA는 아직 없다.

**보는 곳.** 지표 `kukie_counter_adaptive_limit{model,mode}`, `kukie_counter_adaptive_decisions_total{model,reason}`, `kukie_counter_window_itl_p95_seconds`·`kukie_counter_window_ttft_p95_seconds`(판단에 쓴 값, 표본이 부족하면 -1), 판단한 한도가 바뀔 때의 `adaptive limit decided` 로그(`enforced`는 `on`일 때 true), 브레이크: `kukie_counter_brake_active{model,reason}`, `kukie_counter_upstream_waiting`·`kukie_counter_upstream_kv_ratio`, 브레이크가 바뀔 때의 `brake` 로그.

## 키 만들기와 폐기

```sh
counter genkey --name <이름>
```

첫 줄이 키(`kk-` + 32바이트 난수 base64url)이고, 다음 줄이 키 파일의 `keys` 목록에 넣을 항목이다. 파일을 쓰지 않으며 키는 이 출력에만 있다. 사용자에게 전달한 뒤 터미널 기록에 남기지 않는다. 키 파일 전체는 이런 모양이다.

```json
{"keys":[{"id":"key-3f9a…","name":"이름","sha256":"…","disabled":false}]}
```

- **추가**: `keys`에 줄을 더해 Secret을 고친다. **폐기**: `disabled`를 `true`로 바꾸거나 줄을 지운다. 지표에서 시도를 계속 보려면 `disabled`가 낫다. 재시작은 필요 없고, Secret이 Pod에 반영되는 시간(kubelet 동기화 1분 안팎)과 다시 읽는 주기를 더한 만큼 걸린다.
- 파일을 못 읽거나 깨졌으면(JSON 오류, 모르는 필드, 같은 `id`·해시, `keys` 없음) 이전 값을 그대로 쓰고 오류 로그를 남기며 `kukie_counter_keys_reload_errors_total`을 올린다. 즉 깨진 파일을 올리면 **폐기도 반영되지 않는다.** 이 지표에 알림을 건다.
- `{"keys":[]}`는 유효하며 모든 키를 막는다.
- `id`는 지표 라벨과 로그에 그대로 쓰인다. 영문, 숫자, `.`, `_`, `-`로 64자까지이고 `unknown`은 쓸 수 없다. 이름 같은 개인정보를 넣지 않는다. `name`은 파일에만 있고 어디에도 내보내지 않는다.

## API

허용 경로만 처리한다. 그 밖의 경로와 메서드는 전부 404다. vLLM의 `/metrics`, `/health`, tokenizer 등은 밖으로 나가지 않는다.

| 경로 | 키 | 처리 |
| --- | --- | --- |
| `POST /v1/chat/completions`, `POST /v1/completions` | 필요 | 모델로 전달. 스트리밍 그대로 |
| `GET /v1/models` | 필요 | 설정과 찾은 ServedModel에서 답한다 |
| `GET /v1/model-status` | 필요 | 모델마다 `/v1/models`로 준비 확인(15초 캐시)한 `readyModels`와 지금 한도 `limits` |
| `GET /healthz` | 없음 | 프로세스 생존만 본다. 모델 상태는 보지 않는다 |

| 경우 | 상태 | `error.code` |
| --- | --- | --- |
| 키 없음·틀림·폐기 | 401 | `invalid_api_key` |
| 본문이 JSON 객체가 아님, `model` 없음, 본문 읽기 실패·시간 초과 | 400 | `invalid_request` |
| 허용되지 않은 경로 | 404 | `not_found` |
| 설정에도 ServedModel에도 없는 모델 | 404 | `model_not_found` |
| 본문이 상한 초과 | 413 | `request_too_large` |
| 동시 수 한도 초과 | 429, `Retry-After` | `rate_limit_exceeded` |
| 모델이 꺼졌거나 연결 실패, 응답 헤더 시간 초과 | 503, `Retry-After` | `model_unavailable` |

모델이 직접 돌려준 4xx·5xx는 그대로 전달한다.

## 처리 순서

경로 → 키 → 본문을 상한까지 읽어 `model`만 꺼냄 → 모델 찾기 → 자리 얻기(줄 세우지 않음, 없으면 바로 429) → 전달 → 응답 본문이 끝나거나 클라이언트가 끊으면 자리 반납.

- **자리는 헤더가 아니라 본문이 끝날 때 돌아온다.** 스트림이 도는 동안 자리를 잡고 있다.
- **본문은 바꾸지 않는다.** 받은 바이트 그대로 보낸다(`stream_options`를 넣지 않는다).
- **모델로 넘기지 않는 것**: `Authorization`, `Cookie`, hop-by-hop 헤더, `Upgrade`, `Expect`, `Accept-Encoding`(응답을 압축하지 않게), `Forwarded`, `X-Forwarded-*`, `X-Real-Ip`, 쿼리 문자열.
- **클라이언트가 끊으면** 모델로 가는 요청도 취소되어 vLLM이 생성을 멈춘다.
- **응답이 나가기 전의 모델 오류**는 503이다. **응답이 나간 뒤**에 모델이 끊으면 상태를 바꿀 수 없으므로 클라이언트 연결을 끊어 잘린 것을 알리고, `kukie_counter_upstream_broken_total`을 올린다. `requests_total`의 `code`는 이미 보낸 200으로 남는다.

## 시간 제한

| 대상 | 제한 |
| --- | --- |
| 요청 헤더 읽기 | 10초 |
| 요청 본문 읽기 | 60초(요청마다 걸고 다 읽으면 푼다) |
| 응답 쓰기, 스트림 전체 | 없음. 스트림을 끊지 않기 위해서다 |
| 모델 연결 | 5초 |
| 모델 응답 헤더 | `--upstream-response-header-timeout` |
| 헤더 뒤 멈춘 모델 | 없음. 클라이언트가 끊거나 모델이 끊을 때까지 자리를 잡는다 |
| 빈 연결 유지 | 660초. 앞의 ALB 유휴 제한(600초)보다 길어야 한다. 짧으면 ALB가 방금 닫힌 연결을 다시 써서 502가 난다 |

서버의 `WriteTimeout`은 긴 스트림을 끊으므로 쓰지 않는다. 본문 읽기 제한은 서버 전체 값(`ReadTimeout`) 대신 요청마다 걸어 응답에 닿지 않게 한다.

앞에 프록시나 인그레스를 둘 때는 그쪽의 응답 버퍼링을 끄고(ingress-nginx는 `proxy-buffering: "off"`) 읽기 시간을 가장 긴 생성보다 길게 둔다. 버퍼링이 켜져 있으면 카운터가 바로 내보내도 클라이언트의 첫 바이트가 늦어진다.

## 공개 입구

`counter.ingress`를 켜면 AWS Load Balancer Controller가 ALB로 받는다. 기본은 꺼져 있다. 컨트롤러, 인증서, DNS는 이 차트에 없다. 우리 클러스터 절차는 kukie-gpu-infra `runbooks/cluster.md`의 '공개 입구'에 있다.

```yaml
# counter-values.yaml에 더한다
counter:
  ingress:
    enabled: true
    host: api.gpu.durokukie.com
    certificateArn: <terraform output public_ingress.certificate_arn>
    groupName: kukie-gpu-public   # 기본값. 포털과 같아야 ALB 하나를 같이 쓴다
```

| annotation | 값 | 이유 |
| --- | --- | --- |
| `target-type` | `ip` | 파드로 바로 보낸다. 파드가 VPC IP를 받아야 한다(VPC CNI) |
| `listen-ports`, `ssl-redirect` | 80·443, `443` | HTTPS만 받는다 |
| `ssl-policy` | `ELBSecurityPolicy-TLS13-1-2-Res-PQ-2025-09` | 2026-11-05 서울 리전의 FIPS 아닌 정책 중 최신. TLS 1.3과 TLS 1.2의 ECDHE GCM만 받는다 |
| `load-balancer-attributes` | `idle_timeout.timeout_seconds=600` | 긴 응답과 스트리밍. 토큰이 600초 멈추면 끊긴다 |
| `target-group-attributes` | `deregistration_delay.timeout_seconds=30` | `--shutdown-delay` 5초 + `--shutdown-timeout` 25초. grace 35초 안이다 |
| `healthcheck-path` | `/healthz` | 키 없이 답하는 경로 |

- **지표 포트는 나가지 않는다.** Ingress는 Service의 `http`만 가리키고 Service에 9090이 없다. 시험이 이것을 본다.
- **설정 검사.** 켜면 `host`, `certificateArn`, `groupName` 모양과 `counter.enabled`를 보고, 틀리면 설치 시점에 실패한다. 오류에 값을 적지 않는다.
- **포털과 같이 고친다.** `scheme`, `listen-ports`, `ssl-policy`, `load-balancer-attributes`는 ALB 단위라 kukie-gpu-web 차트와 다르면 컨트롤러가 그룹 전체를 거부한다.
- **카운터는 1개다.** 배포 때 끊기지 않으려면 namespace에 `elbv2.k8s.aws/pod-readiness-gate-inject=enabled` 라벨이 있어야 새 파드가 ALB에서 건강해진 뒤 옛 파드가 내려간다. 파드를 만들 때 적용되므로 라벨 뒤에 한 번 다시 띄운다.
- **아직 재지 않았다.** `--shutdown-delay` 5초 안에 ALB가 새 요청을 멈추는지는 실제 ALB에서 보지 못했다. 배포 중 502가 나오면 이 값을 늘리고 grace와 `deregistration_delay`를 같이 올린다.

## 지표

`:9090/metrics`. 라벨에는 설정의 모델 이름, ServedModel에서 찾은 이름(검사한 것, 100개까지), 키 파일의 `id`, 고정된 값만 쓴다. 요청의 `model`이나 경로를 그대로 쓰지 않는다.

| 지표 | 라벨 | 뜻 |
| --- | --- | --- |
| `kukie_counter_in_flight` | `model` | 자리를 잡은 요청 수 |
| `kukie_counter_limit` | `model` | 지금 한도. 모델이 사라지면 줄도 사라진다 |
| `kukie_counter_limit_source` | `model`, `source`, `reason` | 늘 1. 한도가 어디서 왔는지(`config`, `baseline`, `default`)와 이유(위 "기준선으로 한도 정하기" 표) |
| `kukie_counter_requests_total` | `model`, `key`, `code` | 건강 확인을 뺀 모든 요청. 키를 모르면 `key="unknown"`, 모델을 모르면 `model="unknown"`(`/v1/models`도 여기). `code="499"`는 응답을 보내기 전에 클라이언트가 떠난 경우, `500`은 응답 전 패닉 |
| `kukie_counter_rejected_total` | `model`, `reason` | `limit`(429), `unavailable`(503) |
| `kukie_counter_first_byte_seconds` | `model` | 전달 시작부터 응답 본문 첫 바이트까지. 2xx만 |
| `kukie_counter_request_seconds` | `model` | 전달 시작부터 응답 끝까지. 모델이 답한 요청만 |
| `kukie_counter_request_body_bytes` | `model` | 설정에 있는 모델을 부른 요청의 본문 크기 |
| `kukie_counter_tokens_total` | `model`, `key`, `type` | 응답의 `usage`에서 센 토큰. `type`은 `prompt`, `completion` |
| `kukie_counter_usage_missing_total` | `model` | `usage` 없이 끝난 2xx 응답 |
| `kukie_counter_upstream_broken_total` | `model` | 응답이 나간 뒤 모델이 끊은 수 |
| `kukie_counter_keys_reload_errors_total` | | 키 파일 다시 읽기 실패 |
| `kukie_counter_discovery_errors_total` | | ServedModel list 실패. 직전 한도를 그대로 쓴다 |
| `kukie_counter_discovery_last_success_timestamp_seconds` | | 마지막으로 list에 성공한 시각. 0은 아직 없음 또는 꺼짐 |
| `kukie_counter_ttft_seconds` | `model` | 스트리밍 요청: 전달 시작부터 글이 든 첫 조각까지, 클라이언트에 쓴 시간 뺌 |
| `kukie_counter_itl_seconds` | `model` | 스트리밍 요청: 글이 든 읽기 사이에 모델을 기다린 시간 |
| `kukie_counter_prefill_in_flight` | `model` | 첫 토큰을 아직 못 받은 스트리밍 요청 수 |
| `kukie_counter_adaptive_limit` | `model`, `mode` | 적응형 판단 한도. `observe`에서는 쓰지 않는다. 사람이 `maxConcurrent`를 적은 모델은 없다 |
| `kukie_counter_adaptive_decisions_total` | `model`, `reason` | 판단 수. 모델마다 주기당 1 |
| `kukie_counter_window_itl_p95_seconds`, `kukie_counter_window_ttft_p95_seconds` | `model` | 판단에 쓴 창의 값. -1은 표본 부족 |
| `kukie_counter_upstream_waiting`, `kukie_counter_upstream_kv_ratio` | `model` | 브레이크가 마지막으로 읽은 vLLM 대기 수와 KV 사용률(0~1) |
| `kukie_counter_upstream_metrics_errors_total` | `model` | vLLM `/metrics` 읽기 실패. 실패는 브레이크를 걸지 않는다 |
| `kukie_counter_brake_active` | `model`, `reason` | 걸려 있는(`observe`면 걸었을) 브레이크에 1. `reason`은 `waiting`, `kv` |

Go 런타임과 프로세스 지표도 함께 나온다.

- **`first_byte`는 첫 토큰이 아니라 첫 바이트다.** 스트림의 첫 조각은 토큰 없는 역할 조각일 수 있어 실제 첫 토큰보다 이를 수 있다. 첫 토큰은 `kukie_counter_ttft_seconds`(스트리밍만)를 본다. 일반 응답은 생성이 끝나야 첫 바이트가 나오므로 사실상 전체 시간이다. 스트림과 일반 요청을 나누지 않으니 섞어 읽을 때 주의한다.
- **토큰 수는 클라이언트가 고른다.** 스트림은 클라이언트가 `stream_options: {"include_usage": true}`를 보낼 때만 vLLM이 `usage`를 준다. 카운터는 요청을 고치지 않으므로, 이 옵션이 없는 스트림은 토큰을 세지 못하고 `usage_missing`으로 센다. 끝까지 받기 전에 끊긴 스트림도 마찬가지다. 따라서 `tokens_total`은 실제 사용량의 하한이며 과금 근거로 쓰지 않는다.
- 줄마다 누적값이 오는 스트림(`continuous_usage_stats`)은 마지막 값만 센다.

## 내용을 저장하지 않는다

요청과 응답 본문을 로그, 지표, 파일에 남기지 않는다. 키, 해시, `Authorization` 값도 남기지 않는다.

- 접근 로그(JSON, stdout)는 시각, 키 `id`, 모델, 상태, 시간, 본문 크기만 적는다. 경로도 적지 않는다.
- `usage`를 찾으려고 응답을 지나가며 읽지만 보관하지 않고, 쓰기 경로를 막지 않는다. 메모리 상한은 스트림 한 줄 64KiB(넘는 줄은 건너뛴다), 일반 응답은 뒤쪽 128KiB다. `usage`가 뒤쪽 범위에 없는 아주 큰 응답(예: 큰 `prompt_logprobs`)은 `usage_missing`으로 센다.

## 후속 (아직 안 함)

화면, 키별 한도, 토큰 한도, 비상 중단. 적응형 한도 `on`을 kukie-gpu에서 동시 40으로 시험(24 근처로 가는지), 모델별 SLA, 비스트리밍 요청의 TPOT 어림, 배운 한도를 재시작 뒤에도 남기기(DURO-212). `discovery`를 실제 클러스터(`kukie-gpu`)에서 켜서 25로 정해지는지 보기. 끝난 조건의 나머지(`kukie-gpu`에서 OpenAI SDK로 스트리밍과 429 확인)는 설치 다음에 한다.
