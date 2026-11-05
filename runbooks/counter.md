# kukie-counter: 초대제 API 입구 (DURO-190)

`counter/`(`kukie-counter`)는 OpenAI 호환 모델 서버(vLLM) 앞에 서는 Go 게이트웨이다. 비유하면 빵집 카운터다. 키를 확인하고 주방이 차면 돌려보낸다. 쿠버네티스 API를 읽지 않고, 필요한 것은 설정 파일과 키 파일뿐이다.

**지금 상태**: 코드뿐이다. 차트, 릴리스, 공개 주소, TLS는 아직 없다. 가짜 vLLM으로만 시험했고 실제 vLLM과 OpenAI SDK 확인은 차트와 함께 한다.

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
| `--shutdown-timeout` | `25s` | SIGTERM 뒤 처리 중인 요청을 기다리는 시간. 지나면 끊는다. Pod의 `terminationGracePeriodSeconds`보다 짧게 둔다 |
| `--upstream-response-header-timeout` | `10m` | 모델이 응답 헤더를 줄 때까지 기다리는 시간. 넘으면 503이다. `0`은 제한 없음. 일반(비스트림) 응답은 생성이 끝나야 헤더가 오므로 가장 긴 생성보다 길게 둔다 |

설정이나 키 파일을 처음부터 읽지 못하면 시작하지 않는다.

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
| `models.<이름>` | (필수) | 요청의 `model`과 정확히 같은 이름. `unknown`은 지표 값과 겹쳐 쓸 수 없다 |
| `upstream` | (필수) | http(s) 주소. 사용자 정보와 쿼리는 넣을 수 없다 |
| `maxConcurrent` | (필수) | 동시 수 한도. 사람이 정한다. 오븐 측정의 `status.baseline.maxConcurrency`를 참고한다 |
| `retryAfterSeconds` | `2` | 429와 503의 `Retry-After` |
| `maxBodyBytes` | 16MiB | 요청 본문 상한 |

모르는 필드가 있으면 시작하지 않는다(오타가 기본값으로 묻히지 않게). 모델 추가는 설정 변경과 재시작이다.

## 키 만들기와 폐기

```sh
counter genkey --name <이름>
```

첫 줄이 키(`kk-` + 32바이트 난수 base64url)이고, 다음 줄이 키 파일에 넣을 JSON 한 줄이다. 파일을 쓰지 않으며 키는 이 출력에만 있다. 사용자에게 전달한 뒤 터미널 기록에 남기지 않는다.

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
| `GET /v1/models` | 필요 | 설정에서 답한다 |
| `GET /healthz` | 없음 | 프로세스 생존만 본다. 모델 상태는 보지 않는다 |

| 경우 | 상태 | `error.code` |
| --- | --- | --- |
| 키 없음·틀림·폐기 | 401 | `invalid_api_key` |
| 본문이 JSON 객체가 아님, `model` 없음, 본문 읽기 실패·시간 초과 | 400 | `invalid_request` |
| 허용되지 않은 경로 | 404 | `not_found` |
| 설정에 없는 모델 | 404 | `model_not_found` |
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

서버의 `WriteTimeout`은 긴 스트림을 끊으므로 쓰지 않는다. 본문 읽기 제한은 서버 전체 값(`ReadTimeout`) 대신 요청마다 걸어 응답에 닿지 않게 한다.

앞에 프록시나 인그레스를 둘 때는 그쪽의 응답 버퍼링을 끄고(ingress-nginx는 `proxy-buffering: "off"`) 읽기 시간을 가장 긴 생성보다 길게 둔다. 버퍼링이 켜져 있으면 카운터가 바로 내보내도 클라이언트의 첫 바이트가 늦어진다.

## 지표

`:9090/metrics`. 라벨에는 설정의 모델 이름, 키 파일의 `id`, 고정된 값만 쓴다. 요청의 `model`이나 경로를 그대로 쓰지 않는다.

| 지표 | 라벨 | 뜻 |
| --- | --- | --- |
| `kukie_counter_in_flight` | `model` | 자리를 잡은 요청 수 |
| `kukie_counter_limit` | `model` | `maxConcurrent` |
| `kukie_counter_requests_total` | `model`, `key`, `code` | 건강 확인을 뺀 모든 요청. 키를 모르면 `key="unknown"`, 모델을 모르면 `model="unknown"`(`/v1/models`도 여기). `code="499"`는 응답을 보내기 전에 클라이언트가 떠난 경우, `500`은 응답 전 패닉 |
| `kukie_counter_rejected_total` | `model`, `reason` | `limit`(429), `unavailable`(503) |
| `kukie_counter_first_byte_seconds` | `model` | 전달 시작부터 응답 본문 첫 바이트까지. 2xx만 |
| `kukie_counter_request_seconds` | `model` | 전달 시작부터 응답 끝까지. 모델이 답한 요청만 |
| `kukie_counter_request_body_bytes` | `model` | 설정에 있는 모델을 부른 요청의 본문 크기 |
| `kukie_counter_tokens_total` | `model`, `key`, `type` | 응답의 `usage`에서 센 토큰. `type`은 `prompt`, `completion` |
| `kukie_counter_usage_missing_total` | `model` | `usage` 없이 끝난 2xx 응답 |
| `kukie_counter_upstream_broken_total` | `model` | 응답이 나간 뒤 모델이 끊은 수 |
| `kukie_counter_keys_reload_errors_total` | | 키 파일 다시 읽기 실패 |

Go 런타임과 프로세스 지표도 함께 나온다.

- **첫 토큰이 아니라 첫 바이트다.** 스트림의 첫 조각은 토큰 없는 역할 조각일 수 있어 실제 첫 토큰보다 이를 수 있다. 일반 응답은 생성이 끝나야 첫 바이트가 나오므로 사실상 전체 시간이다. 스트림과 일반 요청을 나누지 않으니 섞어 읽을 때 주의한다.
- **토큰 수는 클라이언트가 고른다.** 스트림은 클라이언트가 `stream_options: {"include_usage": true}`를 보낼 때만 vLLM이 `usage`를 준다. 카운터는 요청을 고치지 않으므로, 이 옵션이 없는 스트림은 토큰을 세지 못하고 `usage_missing`으로 센다. 끝까지 받기 전에 끊긴 스트림도 마찬가지다. 따라서 `tokens_total`은 실제 사용량의 하한이며 과금 근거로 쓰지 않는다.
- 줄마다 누적값이 오는 스트림(`continuous_usage_stats`)은 마지막 값만 센다.

## 내용을 저장하지 않는다

요청과 응답 본문을 로그, 지표, 파일에 남기지 않는다. 키, 해시, `Authorization` 값도 남기지 않는다.

- 접근 로그(JSON, stdout)는 시각, 키 `id`, 모델, 상태, 시간, 본문 크기만 적는다. 경로도 적지 않는다.
- `usage`를 찾으려고 응답을 지나가며 읽지만 보관하지 않고, 쓰기 경로를 막지 않는다. 메모리 상한은 스트림 한 줄 64KiB(넘는 줄은 건너뛴다), 일반 응답은 뒤쪽 128KiB다. `usage`가 뒤쪽 범위에 없는 아주 큰 응답(예: 큰 `prompt_logprobs`)은 `usage_missing`으로 센다.

## 후속 (아직 안 함)

차트와 릴리스, 공개 주소와 TLS, 화면, 키별 한도, 토큰 한도, 자동 조절, ServedModel 자동 발견, 비상 중단. 끝난 조건의 나머지(`kukie-gpu`에서 OpenAI SDK로 스트리밍과 429 확인)는 차트 다음에 한다.
