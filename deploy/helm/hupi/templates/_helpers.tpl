{{- define "hupi.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "hupi.fullname" -}}
{{- if eq .Release.Name .Chart.Name -}}
{{- .Chart.Name -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "hupi.labels" -}}
app.kubernetes.io/name: {{ include "hupi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "hupi.appSecretName" -}}
{{- if .Values.secrets.app.create -}}
{{ include "hupi.fullname" . }}-app
{{- else -}}
{{ .Values.secrets.app.existingSecretName }}
{{- end -}}
{{- end -}}

{{- define "hupi.adminDBSecretName" -}}
{{- if .Values.secrets.adminDB.create -}}
{{ include "hupi.fullname" . }}-admin-db
{{- else -}}
{{ .Values.secrets.adminDB.existingSecretName }}
{{- end -}}
{{- end -}}
