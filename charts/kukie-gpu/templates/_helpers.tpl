{{/*
이미지 참조 문자열. image.registry(전체 덮어쓰기) > 이미지별 registry > 없음(Docker Hub) 순으로 레지스트리를 고르고,
tag와 digest를 붙인다. 호출: include "kukie-gpu.image" (dict "root" $ "img" .Values.dcgm.image)
*/}}
{{- define "kukie-gpu.image" -}}
{{- $img := .img -}}
{{- $registry := default $img.registry .root.Values.image.registry -}}
{{- $ref := $img.repository -}}
{{- if $registry }}{{ $ref = printf "%s/%s" $registry $img.repository }}{{ end -}}
{{- if $img.tag }}{{ $ref = printf "%s:%s" $ref $img.tag }}{{ end -}}
{{- if $img.digest }}{{ $ref = printf "%s@%s" $ref $img.digest }}{{ end -}}
{{- $ref -}}
{{- end -}}

{{/* GPU 노드 선택 조건(nodeSelector·tolerations). Pod spec 안에서 include 한다. */}}
{{- define "kukie-gpu.gpuNodeScheduling" -}}
{{- with .Values.gpuNode.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.gpuNode.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/* RoleBinding을 만들 namespace 목록(중복 제거) */}}
{{- define "kukie-gpu.rbacNamespaces" -}}
{{- list .Values.namespaces.metrics .Values.namespaces.llm | uniq | toJson -}}
{{- end -}}

{{/* 이 차트가 만들 Namespace 목록: 쓰이는 것만, 설치 대상 namespace는 제외 */}}
{{- define "kukie-gpu.namespacesToCreate" -}}
{{- $names := list -}}
{{- if or .Values.dcgm.enabled .Values.rbac.enabled }}{{ $names = append $names .Values.namespaces.metrics }}{{ end -}}
{{- if or .Values.demoVllm.enabled .Values.rbac.enabled }}{{ $names = append $names .Values.namespaces.llm }}{{ end -}}
{{- $out := list -}}
{{- range $name := ($names | uniq) }}{{ if ne $name $.Release.Namespace }}{{ $out = append $out $name }}{{ end }}{{ end -}}
{{- $out | toJson -}}
{{- end -}}

{{/* kukie-oven 리소스의 공통 라벨 */}}
{{- define "kukie-gpu.ovenLabels" -}}
app.kubernetes.io/name: kukie-oven
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* gpuNode.nodeSelector를 --gpu-node-selector 형식(key=value,key=value)으로. 비어 있으면 빈 문자열. */}}
{{- define "kukie-gpu.ovenGpuNodeSelector" -}}
{{- $pairs := list -}}
{{- range $k, $v := .Values.gpuNode.nodeSelector }}{{ $pairs = append $pairs (printf "%s=%s" $k $v) }}{{ end -}}
{{- join "," $pairs -}}
{{- end -}}

{{/* kukie-counter 리소스의 공통 라벨 */}}
{{- define "kukie-gpu.counterLabels" -}}
app.kubernetes.io/name: kukie-counter
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
1 이상의 정수인지 본다. 카운터는 소수와 문자열을 읽지 못하고, 0은 말없이 기본값으로 바꾼다. 호출: dict "name" "값 이름" "value" 값
values 파일의 숫자는 float64, --set의 숫자는 int64로 온다.
*/}}
{{- define "kukie-gpu.counterCheckPositiveInt" -}}
{{- $isNumber := or (kindIs "int" .value) (kindIs "int64" .value) (kindIs "float64" .value) -}}
{{- if not $isNumber -}}
{{- fail (printf "%s는 1 이상의 정수여야 합니다. 지금 값: %v. 값이 비어 있다면 helm upgrade --reuse-values 때문일 수 있습니다. 이 옵션은 새 차트의 기본값을 가져오지 않으니 --reset-then-reuse-values를 쓰세요(runbooks/counter.md)." .name .value) -}}
{{- end -}}
{{- if or (lt (float64 .value) 1.0) (ne (floor .value) (float64 .value)) -}}
{{- fail (printf "%s는 1 이상의 정수여야 합니다. 지금 값: %v" .name .value) -}}
{{- end -}}
{{- end -}}

{{/*
카운터 값 검사. 카운터는 설정이 틀리면 뜨지 못하므로(모르는 필드도 거부한다) --wait 없는 설치에서도 바로 알게 설치 시점에 막는다.
upstream과 keysSecret은 잘못 넣은 인증정보일 수 있어 오류 메시지에 값을 적지 않는다.
*/}}
{{- define "kukie-gpu.counterValidate" -}}
{{- $c := .Values.counter -}}
{{- $known := list "enabled" "image" "imagePullSecrets" "keysSecret" "models" "retryAfterSeconds" "maxBodyBytes" "ingress" "nodeSelector" "tolerations" "resources" -}}
{{- range $key, $_ := $c -}}
{{- if eq $key "replicas" -}}
{{- fail "counter.replicas는 없습니다. 자리 수를 Pod끼리 나누지 않아 Pod가 N개면 한도가 N배가 되므로 1로 고정합니다." -}}
{{- else if not (has $key $known) -}}
{{- fail (printf "counter.%s는 없는 값입니다. 쓸 수 있는 값: %s. 키와 해시는 values가 아니라 Secret에 넣습니다(counter.keysSecret)." $key (join ", " $known)) -}}
{{- end -}}
{{- end -}}
{{- $image := $c.image | default dict -}}
{{- if not $image.repository -}}
{{- fail "counter.image.repository가 없습니다. helm upgrade --reuse-values는 새 차트의 기본값을 가져오지 않습니다. --reset-then-reuse-values를 쓰세요(runbooks/counter.md)." -}}
{{- end -}}
{{- if not (or $image.tag $image.digest) -}}
{{- fail "counter.enabled=true에는 counter.image.tag 또는 counter.image.digest가 필요합니다. GHCR에 올린 차트에는 digest가 들어 있습니다." -}}
{{- end -}}
{{- if not $c.keysSecret -}}
{{- fail "counter.enabled=true에는 counter.keysSecret(미리 만든 Secret 이름)이 필요합니다. 차트는 Secret을 만들지 않습니다(runbooks/counter.md)." -}}
{{- end -}}
{{- if not (and (kindIs "string" $c.keysSecret) (regexMatch `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` (toString $c.keysSecret))) -}}
{{- fail "counter.keysSecret는 Secret 이름이어야 합니다(영문 소문자, 숫자, -, .). 키나 해시를 넣는 자리가 아닙니다." -}}
{{- end -}}
{{- if not (and (kindIs "map" $c.models) $c.models) -}}
{{- fail "counter.enabled=true에는 counter.models에 모델이 하나 이상 필요합니다. 값 모양은 values.yaml의 예시를 보세요." -}}
{{- end -}}
{{- range $name, $model := $c.models -}}
{{- if or (eq $name "") (eq $name "unknown") -}}
{{- fail (printf "counter.models의 모델 이름 %q는 쓸 수 없습니다. unknown은 지표 값과 겹칩니다." $name) -}}
{{- end -}}
{{- if not (kindIs "map" $model) -}}
{{- fail (printf "counter.models[%q]에는 upstream과 maxConcurrent가 있어야 합니다." $name) -}}
{{- end -}}
{{- range $key, $_ := $model -}}
{{- if not (has $key (list "upstream" "maxConcurrent")) -}}
{{- fail (printf "counter.models[%q].%s는 없는 값입니다. upstream과 maxConcurrent만 쓸 수 있습니다." $name $key) -}}
{{- end -}}
{{- end -}}
{{- if not (and (kindIs "string" $model.upstream) (regexMatch `^https?://(\[[0-9a-fA-F:.]+\]|[^/?#@:\[\]\s]+)(:[0-9]+)?(/[^?#\s]*)?$` (toString $model.upstream))) -}}
{{- fail (printf "counter.models[%q].upstream은 http(s) 주소여야 하고 사용자 정보, 쿼리, 조각을 넣을 수 없습니다." $name) -}}
{{- end -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" (printf "counter.models[%q].maxConcurrent" $name) "value" $model.maxConcurrent) -}}
{{- end -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" "counter.retryAfterSeconds" "value" $c.retryAfterSeconds) -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" "counter.maxBodyBytes" "value" $c.maxBodyBytes) -}}
{{- end -}}

{{/* 카운터 설정 파일. 카운터가 모르는 필드를 거부하므로 검사를 지난 값만 그대로 내보낸다. */}}
{{- define "kukie-gpu.counterConfig" -}}
{{- pick .Values.counter "models" "retryAfterSeconds" "maxBodyBytes" | toPrettyJson -}}
{{- end -}}

{{/* 카운터 공개 입구 값 검사. 켰을 때만 주소와 인증서를 본다. 값이 잘못 들어간 인증정보일 수 있어 오류에 값을 적지 않는다. */}}
{{- define "kukie-gpu.counterIngressValidate" -}}
{{- $counter := .Values.counter | default dict -}}
{{- $ingress := $counter.ingress | default dict -}}
{{- if not (kindIs "map" $ingress) -}}
{{- fail "counter.ingress는 enabled, host, certificateArn, groupName을 담은 값이어야 합니다." -}}
{{- end -}}
{{- range $key, $_ := $ingress -}}
{{- if not (has $key (list "enabled" "host" "certificateArn" "groupName")) -}}
{{- fail (printf "counter.ingress.%s는 없는 값입니다. 쓸 수 있는 값: enabled, host, certificateArn, groupName." $key) -}}
{{- end -}}
{{- end -}}
{{- if and (hasKey $ingress "enabled") (not (kindIs "bool" $ingress.enabled)) -}}
{{- fail "counter.ingress.enabled는 true 또는 false여야 합니다." -}}
{{- end -}}
{{- if $ingress.enabled -}}
{{- if not $counter.enabled -}}
{{- fail "counter.ingress.enabled=true에는 counter.enabled=true가 필요합니다. 보낼 카운터가 없습니다." -}}
{{- end -}}
{{- if not (and (kindIs "string" $ingress.host) (regexMatch `^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$` (toString $ingress.host))) -}}
{{- fail "counter.ingress.host는 소문자 DNS 이름이어야 합니다. https://, 경로, 와일드카드를 넣을 수 없습니다. 예: api.gpu.example.com" -}}
{{- end -}}
{{- if not (and (kindIs "string" $ingress.certificateArn) (regexMatch `^arn:aws[a-z-]*:acm:[a-z0-9-]+:[0-9]{12}:certificate/[-a-zA-Z0-9]+$` (toString $ingress.certificateArn))) -}}
{{- fail "counter.ingress.certificateArn은 ACM 인증서 ARN이어야 합니다(같은 리전, 발급 완료). HTTPS만 받으므로 필수입니다." -}}
{{- end -}}
{{- if not (and (kindIs "string" $ingress.groupName) (le (len (toString $ingress.groupName)) 63) (regexMatch `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` (toString $ingress.groupName))) -}}
{{- fail "counter.ingress.groupName은 63자 이하의 소문자, 숫자, -, .이어야 합니다. 포털과 같아야 ALB 하나를 같이 씁니다." -}}
{{- end -}}
{{- end -}}
{{- end -}}
