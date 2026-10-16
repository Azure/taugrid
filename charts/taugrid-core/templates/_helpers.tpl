{{- define "taugrid-core.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
tau.azure.com/component: taugrid-core
{{- end }}

{{- define "taugrid-core.adxQueryConnection" -}}
{{- $adx := default dict (default dict .Values.global).adx -}}
{{- $connection := default dict $adx.queryConnection -}}
{{- $endpoint := trim (default "" $connection.endpoint) -}}
{{- $database := trim (default "" $connection.database) -}}
{{- $clientID := trim (default "" $connection.clientID) -}}
{{- if or $endpoint $database $clientID -}}
{{- if not (and $endpoint $database $clientID) -}}
{{- fail "global.adx.queryConnection requires endpoint, database, and clientID together (a separately provisioned ADX Viewer identity); leave all three empty to disable inheritance" -}}
{{- end -}}
{{- $url := urlParse $endpoint -}}
{{- if or (ne (get $url "scheme") "https") (not (get $url "hostname")) (get $url "userinfo") (get $url "query") (get $url "fragment") (not (has (get $url "path") (list "" "/"))) -}}
{{- fail "global.adx.queryConnection.endpoint must be an HTTPS ADX query endpoint without credentials, path, query, or fragment" -}}
{{- end -}}
{{- if not (regexMatch "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$" $clientID) -}}
{{- fail "global.adx.queryConnection.clientID must be the UUID client ID of a separately provisioned ADX Viewer identity" -}}
{{- end -}}
{{- dict "endpoint" $endpoint "database" $database "clientID" $clientID | toJson -}}
{{- else -}}
{}
{{- end -}}
{{- end }}

{{- define "taugrid-core.apiReachable" -}}
{{- if lookup "v1" "Namespace" "" "kube-system" -}}
reachable
{{- end -}}
{{- end }}

{{- define "taugrid-core.image" -}}
{{- $component := .component -}}
{{- $image := .image -}}
{{- $repository := default "" $image.repository -}}
{{- $tag := default "" $image.tag -}}
{{- $digest := default "" $image.digest -}}
{{- if not $repository -}}
{{- fail (printf "%s.image.repository is required when %s.enabled=true" $component $component) -}}
{{- end -}}
{{- if eq (empty $tag) (empty $digest) -}}
{{- fail (printf "%s.image must set exactly one of tag or digest when %s.enabled=true" $component $component) -}}
{{- end -}}
{{- if and $digest (not (regexMatch "^sha256:[a-f0-9]{64}$" $digest)) -}}
{{- fail (printf "%s.image.digest must be sha256:<64 lowercase hex characters>" $component) -}}
{{- end -}}
{{- if $digest -}}
{{- printf "%s@%s" $repository $digest -}}
{{- else -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end }}
