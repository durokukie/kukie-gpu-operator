# kukie-serving-operator

`ServedModel`(`serving.kukie.io/v1alpha1`) 하나를 받아 PVC → 다운로드 Job → vLLM Deployment → `/health` → status 기록을
돌리는 kubebuilder 프로젝트다. 설치·예시·범위는 [runbooks/operator.md](../runbooks/operator.md), 설계는 Linear
`[설계] ServedModel 오퍼레이터 · 실패 진단 · 어드미션 프록시` 1·2절.

```sh
make manifests generate   # api/ 를 고친 뒤 CRD·RBAC·deepcopy 재생성 (CI가 최신 여부를 검사한다)
make test                 # envtest (GPU·클러스터 없이 reconcile 흐름 검증)
make build                # bin/manager
```
