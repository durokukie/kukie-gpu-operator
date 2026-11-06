# kukie-connector

고객 클러스터 안에서 Kukie 게이트웨이로 TLS WebSocket을 먼저 열고, 기존 카운터로 요청을 전달하는 독립 Go 모듈이다. Kubernetes API를 읽지 않으며 별도 SA 권한을 요구하지 않는다. 오븐·카운터 코드는 복제하지 않는다.

`go test -race -timeout 60s ./...`, `go vet ./...`, `go build .`로 검사한다. 운영 절차는 [connector runbook](../runbooks/connector.md)에 있다. 아직 Helm 자동 설치·이미지 릴리스·운영 연결을 하지 않았다.
