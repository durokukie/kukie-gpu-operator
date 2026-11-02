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
