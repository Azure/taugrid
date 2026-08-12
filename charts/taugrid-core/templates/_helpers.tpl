{{/*
Common labels for all taugrid-core resources.
*/}}
{{- define "taugrid-core.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
tau.azure.com/component: taugrid-core
{{- end }}

{{/*
Whether `lookup` can see the cluster at all.

`lookup` returns empty both for an absent object and for every render that has
no API connection — `helm template`, `--dry-run=client`, ArgoCD manifest
generation. Probing an object that always exists separates the two, so a guard
can refuse a missing namespace at install time without failing every render.
*/}}
{{- define "taugrid-core.apiReachable" -}}
{{- if lookup "v1" "Namespace" "" "kube-system" -}}
reachable
{{- end -}}
{{- end }}

{{/*
Whether this release may render a Namespace object for a given name.

Helm refuses to apply over an object it does not own: a namespace that already
exists without this release's ownership metadata fails install with `exists and
cannot be imported into the current release: invalid ownership metadata`. The
three keys checked below are exactly what Helm's own ownership check requires,
so a rendered namespace is always one Helm will accept. Helm stamps them on
apply; this template cannot write the two annotations itself.

A namespace owned by anything else — including this chart under a former release
name — is skipped, and keeps whatever labels it has. README covers adopting one.

An absent object and an unreachable API both read as "render": absent is the
case this chart exists to handle, and render mode must keep emitting the full
manifest.

Call with: include "taugrid-core.mayOwnNamespace" (dict "name" $name "root" $)
Returns a non-empty string when the namespace should be rendered.
*/}}
{{- define "taugrid-core.mayOwnNamespace" -}}
{{- $live := lookup "v1" "Namespace" "" .name -}}
{{- $labels := default dict (default dict $live.metadata).labels -}}
{{- $annotations := default dict (default dict $live.metadata).annotations -}}
{{- if or (not $live) (and
      (eq (get $labels "app.kubernetes.io/managed-by") "Helm")
      (eq (get $annotations "meta.helm.sh/release-name") .root.Release.Name)
      (eq (get $annotations "meta.helm.sh/release-namespace") .root.Release.Namespace)) -}}
render
{{- end -}}
{{- end }}

{{/*
Render a first-party image from exactly one release tag or immutable digest.
Call with: include "taugrid-core.image" (dict "component" "portal" "image" .Values.portal.image)
*/}}
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
