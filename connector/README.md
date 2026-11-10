# kukie-connector

고객 클러스터 안에서 Kukie 게이트웨이로 TLS WebSocket을 먼저 열고, 기존 카운터로 요청을 전달하는 독립 Go 모듈이다. `--credential-secret`을 주면 자격을 스스로 갱신하고 자기 Secret 하나만 `get`·`update`한다. 주지 않으면 Kubernetes API를 부르지 않고 자동 갱신도 하지 않는다. 오븐·카운터 코드는 복제하지 않는다.

`go test -race -timeout 60s ./...`, `go vet ./...`, `go build .`로 검사한다. 운영 절차는 [connector runbook](../runbooks/connector.md)에 있다. 아직 Helm 자동 설치를 하지 않았다.
