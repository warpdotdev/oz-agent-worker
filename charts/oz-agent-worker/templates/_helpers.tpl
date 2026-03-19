{{- define "oz-agent-worker.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "oz-agent-worker.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "oz-agent-worker.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "oz-agent-worker.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "oz-agent-worker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "oz-agent-worker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "oz-agent-worker.labels" -}}
helm.sh/chart: {{ include "oz-agent-worker.chart" . }}
{{ include "oz-agent-worker.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "oz-agent-worker.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "oz-agent-worker.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "oz-agent-worker.apiKeySecretName" -}}
{{- if .Values.warp.apiKeySecret.create -}}
{{- default (printf "%s-api-key" (include "oz-agent-worker.fullname" .)) .Values.warp.apiKeySecret.name -}}
{{- else -}}
{{- required "warp.apiKeySecret.name is required when warp.apiKeySecret.create=false" .Values.warp.apiKeySecret.name -}}
{{- end -}}
{{- end -}}
