{{- define "bale-flux.fullname" -}}
{{- printf "%s-bale-flux" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bale-flux.selectorLabels" -}}
app.kubernetes.io/name: bale-flux
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "bale-flux.labels" -}}
{{ include "bale-flux.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | replace "+" "_" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
{{- end -}}

{{- define "bale-flux.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default (.Chart.AppVersion | replace "+" "_")) -}}
{{- end -}}
{{- end -}}
