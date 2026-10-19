{{- define "registry.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "registry.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "registry.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{ include "registry.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "registry.s3" -}}
{{- eq (dig "storage" "driver" "filesystem" .Values.config) "s3" -}}
{{- end -}}

{{- define "registry.serviceAccount" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "registry.fullname" .) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{- define "registry.s3Secret" -}}
{{- .Values.s3.existingSecret | default (include "registry.fullname" .) -}}
{{- end -}}

{{- define "registry.config" -}}
{{- $cfg := deepCopy .Values.config -}}
{{- $server := get $cfg "server" | default (dict) -}}
{{- if and .Values.ingress.enabled (not (get $server "public_url")) -}}
{{- $_ := set $server "public_url" (printf "https://%s" .Values.ingress.host) -}}
{{- $_ := set $cfg "server" $server -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}
