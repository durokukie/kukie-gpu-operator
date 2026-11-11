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
{{- if or .Values.demoVllm.enabled .Values.rbac.enabled .Values.oven.enabled }}{{ $names = append $names .Values.namespaces.llm }}{{ end -}}
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
{{- $known := list "enabled" "image" "imagePullSecrets" "keysSecret" "models" "discovery" "retryAfterSeconds" "maxBodyBytes" "ingress" "nodeSelector" "tolerations" "resources" -}}
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
{{- $generated := eq (include "kukie-gpu.connectorGeneratesCounterKey" .) "true" -}}
{{- if and $generated $c.keysSecret -}}
{{- fail "connector.counterKeySecret을 비우면 차트가 카운터 키 Secret을 만듭니다. counter.keysSecret도 비우거나, 카운터 키를 직접 관리한다면 connector.counterKeySecret도 직접 주세요(runbooks/connector.md)." -}}
{{- end -}}
{{- if and (not $generated) (not $c.keysSecret) -}}
{{- fail "counter.enabled=true에는 counter.keysSecret(미리 만든 Secret 이름)이 필요합니다. 커넥터를 켜고 connector.counterKeySecret을 비우면 차트가 만듭니다(runbooks/counter.md)." -}}
{{- end -}}
{{- if and (not $generated) (not (and (kindIs "string" $c.keysSecret) (regexMatch `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` (toString $c.keysSecret)))) -}}
{{- fail "counter.keysSecret는 Secret 이름이어야 합니다(영문 소문자, 숫자, -, .). 키나 해시를 넣는 자리가 아닙니다." -}}
{{- end -}}
{{- $discovery := include "kukie-gpu.counterDiscoveryOn" . | eq "true" -}}
{{- if not (kindIs "map" ($c.models | default dict)) -}}
{{- fail "counter.models는 모델 이름을 키로 둔 값이어야 합니다. 값 모양은 values.yaml의 예시를 보세요." -}}
{{- end -}}
{{- if and (not $discovery) (not $c.models) -}}
{{- fail "counter.enabled=true에는 counter.models에 모델이 하나 이상 필요합니다(counter.discovery.enabled=true면 비워도 됩니다). 값 모양은 values.yaml의 예시를 보세요." -}}
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
{{- /* discovery를 켜면 maxConcurrent를 빼도 된다. 빼면 ServedModel 기준선에서 정하고, 적으면 그 값이 이긴다. */ -}}
{{- if or (not $discovery) (hasKey $model "maxConcurrent") -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" (printf "counter.models[%q].maxConcurrent" $name) "value" $model.maxConcurrent) -}}
{{- end -}}
{{- end -}}
{{- include "kukie-gpu.counterDiscoveryValidate" . -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" "counter.retryAfterSeconds" "value" $c.retryAfterSeconds) -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" "counter.maxBodyBytes" "value" $c.maxBodyBytes) -}}
{{- end -}}

{{/* counter.discovery.enabled가 true면 "true". 이전 릴리스 값처럼 블록이 없으면 꺼진 것이다. */}}
{{- define "kukie-gpu.counterDiscoveryOn" -}}
{{- $d := (.Values.counter | default dict).discovery | default dict -}}
{{- and (kindIs "map" $d) (eq ($d.enabled | toString) "true") -}}
{{- end -}}

{{/* ServedModel을 읽을 namespace. 비우면 오븐 모델이 사는 namespaces.llm이다. */}}
{{- define "kukie-gpu.counterDiscoveryNamespace" -}}
{{- (.Values.counter.discovery | default dict).namespace | default .Values.namespaces.llm -}}
{{- end -}}

{{/* 기준선으로 한도를 정하는 값 검사. 켰을 때만 본다. */}}
{{- define "kukie-gpu.counterDiscoveryValidate" -}}
{{- $d := .Values.counter.discovery | default dict -}}
{{- if not (kindIs "map" $d) -}}
{{- fail "counter.discovery는 enabled, namespace, safetyFactor, defaultMaxConcurrent, intervalSeconds, adaptive를 담은 값이어야 합니다." -}}
{{- end -}}
{{- range $key, $_ := $d -}}
{{- if not (has $key (list "enabled" "namespace" "safetyFactor" "defaultMaxConcurrent" "intervalSeconds" "adaptive")) -}}
{{- fail (printf "counter.discovery.%s는 없는 값입니다. 쓸 수 있는 값: enabled, namespace, safetyFactor, defaultMaxConcurrent, intervalSeconds, adaptive." $key) -}}
{{- end -}}
{{- end -}}
{{- if and (hasKey $d "enabled") (not (kindIs "bool" $d.enabled)) -}}
{{- fail "counter.discovery.enabled는 true 또는 false여야 합니다." -}}
{{- end -}}
{{- if $d.enabled -}}
{{- if not (regexMatch `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$` (include "kukie-gpu.counterDiscoveryNamespace" .)) -}}
{{- fail "counter.discovery.namespace(비우면 namespaces.llm)는 namespace 이름이어야 합니다." -}}
{{- end -}}
{{- $f := $d.safetyFactor -}}
{{- if not (and (or (kindIs "float64" $f) (kindIs "int64" $f) (kindIs "int" $f)) (gt (float64 $f) 0.0) (le (float64 $f) 1.0)) -}}
{{- fail (printf "counter.discovery.safetyFactor는 0보다 크고 1 이하인 숫자여야 합니다. 지금 값: %v" $f) -}}
{{- end -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" "counter.discovery.defaultMaxConcurrent" "value" $d.defaultMaxConcurrent) -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" "counter.discovery.intervalSeconds" "value" $d.intervalSeconds) -}}
{{- include "kukie-gpu.counterAdaptiveValidate" . -}}
{{- end -}}
{{- end -}}

{{/*
적응형 한도 mode를 글자로. Helm은 values를 YAML 1.1로 읽어 따옴표 없는 off·on을 false·true로 바꾼다.
values 파일에 mode: off라고 적은 사람의 뜻대로 받는다.
*/}}
{{- define "kukie-gpu.counterAdaptiveMode" -}}
{{- $m := .Values.counter.discovery.adaptive.mode -}}
{{- if kindIs "bool" $m -}}
{{- ternary "on" "off" $m -}}
{{- else -}}
{{- toString $m -}}
{{- end -}}
{{- end -}}

{{/* 적응형 한도 값 검사(DURO-212). 이전 릴리스 값처럼 블록이 없으면 꺼진 것이다. */}}
{{- define "kukie-gpu.counterAdaptiveValidate" -}}
{{- $a := .Values.counter.discovery.adaptive -}}
{{- $keys := list "mode" "slaTtftMs" "slaItlMs" "intervalSeconds" "windowSeconds" "maxLimit" "ttftMaxPromptTokens" "brakeIntervalSeconds" "brakeWaitingScrapes" "brakeKvRatio" -}}
{{- if not (kindIs "invalid" $a) -}}
{{- if not (kindIs "map" $a) -}}
{{- fail (printf "counter.discovery.adaptive는 %s를 담은 값이어야 합니다." (join ", " $keys)) -}}
{{- end -}}
{{- range $key, $_ := $a -}}
{{- if not (has $key $keys) -}}
{{- fail (printf "counter.discovery.adaptive.%s는 없는 값입니다. 쓸 수 있는 값: %s." $key (join ", " $keys)) -}}
{{- end -}}
{{- end -}}
{{- $mode := include "kukie-gpu.counterAdaptiveMode" . -}}
{{- if not (has $mode (list "off" "observe" "on")) -}}
{{- fail (printf "counter.discovery.adaptive.mode는 off, observe, on 중 하나여야 합니다. 지금 값: %v" $a.mode) -}}
{{- end -}}
{{- range $key := list "slaTtftMs" "slaItlMs" -}}
{{- $v := index $a $key -}}
{{- if not (and (or (kindIs "float64" $v) (kindIs "int64" $v) (kindIs "int" $v)) (gt (float64 $v) 0.0)) -}}
{{- fail (printf "counter.discovery.adaptive.%s는 0보다 큰 숫자(밀리초)여야 합니다. 지금 값: %v" $key $v) -}}
{{- end -}}
{{- end -}}
{{- range $key := list "intervalSeconds" "windowSeconds" "maxLimit" "ttftMaxPromptTokens" "brakeIntervalSeconds" "brakeWaitingScrapes" -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" (printf "counter.discovery.adaptive.%s" $key) "value" (index $a $key)) -}}
{{- end -}}
{{- $r := $a.brakeKvRatio -}}
{{- if not (and (or (kindIs "float64" $r) (kindIs "int64" $r) (kindIs "int" $r)) (gt (float64 $r) 0.0) (le (float64 $r) 1.0)) -}}
{{- fail (printf "counter.discovery.adaptive.brakeKvRatio는 0보다 크고 1 이하인 숫자여야 합니다. 지금 값: %v" $r) -}}
{{- end -}}
{{- if lt (float64 $a.windowSeconds) (float64 $a.intervalSeconds) -}}
{{- fail "counter.discovery.adaptive.windowSeconds는 intervalSeconds 이상이어야 합니다." -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* 카운터 설정 파일. 카운터가 모르는 필드를 거부하므로 검사를 지난 값만 그대로 내보낸다. */}}
{{- define "kukie-gpu.counterConfig" -}}
{{- $config := pick .Values.counter "models" "retryAfterSeconds" "maxBodyBytes" -}}
{{- if not $config.models -}}
{{- $_ := set $config "models" dict -}}
{{- end -}}
{{- if eq (include "kukie-gpu.counterDiscoveryOn" .) "true" -}}
{{- $d := .Values.counter.discovery -}}
{{- $discovery := dict "namespace" (include "kukie-gpu.counterDiscoveryNamespace" .) "safetyFactor" $d.safetyFactor "defaultMaxConcurrent" $d.defaultMaxConcurrent "intervalSeconds" $d.intervalSeconds -}}
{{- if $d.adaptive -}}
{{- $adaptive := pick $d.adaptive "slaTtftMs" "slaItlMs" "intervalSeconds" "windowSeconds" "maxLimit" "ttftMaxPromptTokens" "brakeIntervalSeconds" "brakeWaitingScrapes" "brakeKvRatio" -}}
{{- $_ := set $adaptive "mode" (include "kukie-gpu.counterAdaptiveMode" .) -}}
{{- $_ := set $discovery "adaptive" $adaptive -}}
{{- end -}}
{{- $_ := set $config "discovery" $discovery -}}
{{- end -}}
{{- $config | toPrettyJson -}}
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

{{/* 오븐이 ServedModel을 처리하는 namespace 목록(JSON). oven.namespaces를 비우면 namespaces.llm 하나. 이름 형식을 여기서 검사한다. */}}
{{- define "kukie-gpu.ovenNamespaces" -}}
{{- $names := .Values.oven.namespaces | default (list .Values.namespaces.llm) -}}
{{- if not (kindIs "slice" $names) }}{{ fail (printf "oven.namespaces는 목록이어야 합니다. 예: --set 'oven.namespaces={llm,team-a}'. 지금 값: %v" $names) }}{{ end -}}
{{- $out := list -}}
{{- range $name := $names }}
{{- if not (and (kindIs "string" $name) (regexMatch `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$` $name)) }}
{{- fail (printf "oven.namespaces의 %v는 namespace 이름이 아닙니다(소문자·숫자·'-', 63자 이하)." $name) }}
{{- end }}
{{- $out = append $out $name -}}
{{- end -}}
{{- $out | uniq | toJson -}}
{{- end -}}

{{/* files/oven/role.yaml(controller-gen 생성물)에서 kind가 같은 문서의 rules. role.yaml에는 ClusterRole과 Role이 하나씩 있다. */}}
{{- define "kukie-gpu.ovenRules" -}}
{{- $kind := .kind -}}
{{- $found := false -}}
{{- range $doc := regexSplit "(?m)^---[ \t]*$" (.root.Files.Get "files/oven/role.yaml") -1 }}
{{- $obj := fromYaml $doc -}}
{{- if eq (get $obj "kind") $kind }}{{ $found = true }}{{ toYaml $obj.rules }}{{ end -}}
{{- end -}}
{{- if not $found }}{{ fail (printf "files/oven/role.yaml에 %s가 없습니다. oven/에서 make manifests를 다시 돌리세요." $kind) }}{{ end -}}
{{- end -}}

{{/* 카운터가 읽을 키 Secret 이름. 커넥터가 카운터 키를 만들면 차트가 만든 Secret이다. */}}
{{- define "kukie-gpu.counterKeysSecretName" -}}
{{- if eq (include "kukie-gpu.connectorGeneratesCounterKey" .) "true" -}}
kukie-counter-keys
{{- else -}}
{{- .Values.counter.keysSecret -}}
{{- end -}}
{{- end -}}

{{/* 커넥터를 켜고 connector.counterKeySecret을 비웠으면 차트가 카운터 키를 만든다. */}}
{{- define "kukie-gpu.connectorGeneratesCounterKey" -}}
{{- $c := .Values.connector | default dict -}}
{{- and $c.enabled (not $c.counterKeySecret) | toString -}}
{{- end -}}

{{/* kukie-connector 리소스의 공통 라벨 */}}
{{- define "kukie-gpu.connectorLabels" -}}
app.kubernetes.io/name: kukie-connector
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
커넥터 값 검사. 자격과 키는 values에 두지 않으므로 오류 메시지에 값을 적어도 되는 것(주소, ID)만 적는다.
*/}}
{{- define "kukie-gpu.connectorValidate" -}}
{{- $c := .Values.connector -}}
{{- $known := list "enabled" "image" "imagePullSecrets" "gateway" "clusterId" "credentialSecret" "counterKeySecret" "workers" "nodeSelector" "tolerations" "resources" -}}
{{- range $key, $_ := $c -}}
{{- if not (has $key $known) -}}
{{- fail (printf "connector.%s는 없는 값입니다. 쓸 수 있는 값: %s. 자격과 키는 values가 아니라 Secret에 넣습니다." $key (join ", " $known)) -}}
{{- end -}}
{{- end -}}
{{- if not (.Values.counter | default dict).enabled -}}
{{- fail "connector.enabled=true에는 counter.enabled=true가 필요합니다. 커넥터는 같은 namespace의 kukie-counter로 요청을 넘깁니다." -}}
{{- end -}}
{{- $image := $c.image | default dict -}}
{{- if not $image.repository -}}
{{- fail "connector.image.repository가 없습니다. helm upgrade --reuse-values는 새 차트의 기본값을 가져오지 않습니다. --reset-then-reuse-values를 쓰세요." -}}
{{- end -}}
{{- if not (or $image.tag $image.digest) -}}
{{- fail "connector.enabled=true에는 connector.image.tag 또는 connector.image.digest가 필요합니다. GHCR에 올린 차트에는 digest가 들어 있습니다." -}}
{{- end -}}
{{- if not (regexMatch `^https://[^/\s]+/connector/v1/connect$` (toString $c.gateway)) -}}
{{- fail (printf "connector.gateway는 https://<호스트>/connector/v1/connect 꼴이어야 합니다. 지금 값: %q" (toString $c.gateway)) -}}
{{- end -}}
{{- if not (regexMatch `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$` (toString $c.clusterId)) -}}
{{- fail (printf "connector.clusterId는 Kukie에 등록된 클러스터 ID(UUID)여야 합니다. 지금 값: %q" (toString $c.clusterId)) -}}
{{- end -}}
{{- range $name := list "credentialSecret" "counterKeySecret" -}}
{{- $v := index $c $name -}}
{{- if and (or $v (eq $name "credentialSecret")) (not (and (kindIs "string" $v) (regexMatch `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` (toString $v)))) -}}
{{- fail (printf "connector.%s는 Secret 이름이어야 합니다(영문 소문자, 숫자, -, .). 자격이나 키를 넣는 자리가 아닙니다." $name) -}}
{{- end -}}
{{- end -}}
{{- if and $c.counterKeySecret (eq $c.counterKeySecret $c.credentialSecret) -}}
{{- fail "connector.counterKeySecret과 connector.credentialSecret은 달라야 합니다. 커넥터가 자격 Secret을 통째로 고쳐 씁니다." -}}
{{- end -}}
{{- include "kukie-gpu.counterCheckPositiveInt" (dict "name" "connector.workers" "value" $c.workers) -}}
{{- if gt (int $c.workers) 8 -}}
{{- fail (printf "connector.workers는 1~8이어야 합니다. 지금 값: %v" $c.workers) -}}
{{- end -}}
{{- end -}}
