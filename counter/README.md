# kukie-counter

OpenAI 호환 모델 서버(vLLM) 앞에 서는 Go 게이트웨이다. API 키를 확인하고, 모델별 동시 수 한도를 넘는 요청을 줄 세우지 않고 바로 돌려보내고, 지표를 남긴다. 요청과 응답 본문은 저장하지 않는다. 실행·키·설정·지표는 [runbooks/counter.md](../runbooks/counter.md)에 있다.

오븐과 의존성을 나누지 않는 독립 모듈이다.

```sh
go vet ./...
go test -race ./...           # GPU·클러스터 없이 가짜 vLLM으로 시험한다
go build -o counter .
docker build -t kukie-counter:dev .
```

| 파일 | 담당 |
| --- | --- |
| `main.go` | 하위 명령(`serve`, `genkey`), 서버 시작과 종료 |
| `config.go` | 설정 파일 읽기와 검사 |
| `keys.go` | 키 파일 읽기, 주기적 다시 읽기, 키 확인, `genkey` |
| `server.go` | 처리 순서, 자리 얻기와 반납, `httputil.ReverseProxy` 전달, 오류 응답, 접근 로그 |
| `usage.go` | 응답을 지나가며 `usage` 찾기(상한 있는 버퍼) |
| `metrics.go` | Prometheus 지표 |
