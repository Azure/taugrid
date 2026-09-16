---
title: Configure Portal
linkTitle: Set up the Portal
weight: 50
description: Verify the default TauGrid Portal and configure additional Kubernetes and Kusto-backed capabilities
url: "/docs/platform-admin-guide/enable-portal/"
aliases:
  - "/docs/tasks/platform/enable-portal/"
---

{{< maturity status="alpha" reviewed="2026-08-13" >}}

Portal is the unified, read-only browser entry point. `tau cluster install` enables its operator-facing Kubernetes path by default in the system release namespace (`tau-system` unless `--namespace` selects another namespace); it ships as part of the one TauGrid umbrella release rather than a separate `taugrid-core` Helm release.

## Understand the default boundary

The default distribution creates `deployment/tau-portal`, `service/tau-portal`, a dedicated ServiceAccount, and cluster-wide read-only Kubernetes RBAC. Portal remains ClusterIP-only and relies on network-level access control rather than application-level login, so `kubectl port-forward` is an operator diagnostic rather than a researcher endpoint.

| Capability | Default state | Additional requirement |
|---|---|---|
| Portal shell, Runs, run detail, Cluster Nodes, live Ray discovery | Available through the default Kubernetes client and RBAC | A matching live workload or Ray head Service; Events remain a separate RBAC capability |
| Jobs / Queue computed board | Disabled | `portal.jobs.scopeMode` plus reviewed workspace-directory or operator scopes; workload profiles are read-only from ready TauCluster status |
| Kueue (Live) | Disabled | KueueViz Deployments/Services and `portal.kueueviz.enabled=true` |
| Experiments, Cluster Health, Cost | Degraded | ADX/Kusto endpoint, database, and a query identity |
| Durable Ray history | Disabled | Lifecycle recorder, successful schema management, and `portal.runHistory.enabled=true` |
| Researcher browser access | Not installed | Opt-in `portal.entraAuth` or an external authenticated HTTPS proxy; DNS, certificate, identity assignments and reviewed network path |
| Services and Observability pages | Planned | No shipped backend yet |

The live Ray dashboard reflects present state only. Durable history remains available after KubeRay removes runtime Pods only when the lifecycle recorder and Kusto path are configured.

## Verify the default Portal

Install or upgrade TauGrid with the cluster's complete values file, then verify the Portal resource and health endpoint:

```bash
export TAU_SYSTEM_NAMESPACE=tau-system
tau cluster install --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" --values <platform-values.yaml>
kubectl --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" rollout status deployment/tau-portal --timeout=180s
kubectl --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" get service/tau-portal serviceaccount/tau-portal
kubectl --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" port-forward service/tau-portal 18080:80
# In another terminal:
curl -fsS http://127.0.0.1:18080/healthz
```

The installation validation should include `PASS Portal`, the rollout should complete, the Service should be `ClusterIP`, and the health request should succeed. A default install can open `/portal` and Kubernetes-backed run details, but the capability table above remains the acceptance boundary.

## Confirm release capability before enabling ADX boards

Use a published TauGrid release whose chart and images include native Kusto Portal support. Confirm the template contract for the exact release you use. The matching `taugrid-core` chart must accept `portal.runHistory.enabled` with `portal.kusto.endpoint` and no `portal.kusto.queryCommand`, and it must derive the `azure.workload.identity/use: "true"` Pod label from the Portal ServiceAccount's `azure.workload.identity/client-id` annotation.

Before changing a production release, render the exact published chart and review these conditions together with its pinned Portal image. If the release lacks them, use a newer published release instead of working around the gate with a nonexistent `queryCommand` or a manual Deployment patch.

## Keep one canonical TauGrid values file

The capability sections below describe keys in one desired-state document
rather than independent Helm overlays. `tau cluster install` resets Helm release values during its upgrade path, so a later invocation containing only a small Portal/Kusto fragment can reset other customized TauGrid components to chart defaults and cause Helm to remove resources that are no longer rendered. Keep the complete reviewed configuration for the cluster in `<platform-values.yaml>`, merge every enabled component into that file, and pass that full file on every upgrade. Portal has no separate namespace setting; it follows the TauGrid Helm release namespace.

Keep `taugrid-core` and the umbrella release mutually exclusive: installing
both would let each try to own Portal or recorder resources, creating
ambiguous Helm ownership.

## Scope the Kubernetes-backed boards

Create the target workspace first. The following is an intentionally non-standalone merge fragment for the canonical reviewed values file. The Portal enablement, ServiceAccount, and RBAC keys repeat distribution defaults so the desired state is explicit; preserve every existing top-level setting and every other enabled component in that file.

```text
# Merge into the existing <platform-values.yaml> as a partial fragment.
# Preserve components, baselineQueue, Kueue/KubeRay/controller settings, and
# every existing taugrid-core service configuration.
taugrid-core:
  portal:
    enabled: true
    cluster: <aks-name>
    workspace: <workspace-name>
    workloadNamespace: <workspace-namespace>
    serviceAccount:
      create: true
      name: tau-portal
    rbac:
      create: true
```

```bash
tau cluster install --context <context> --version <taugrid-release-version> \
  --namespace "$TAU_SYSTEM_NAMESPACE" \
  --values <platform-values.yaml>
kubectl -n "$TAU_SYSTEM_NAMESPACE" rollout status deploy/tau-portal --timeout=180s
```

`portal.jobs.scopeMode` is disabled by default. Configure workspace-directory or explicit operator scopes before enabling it, so the Jobs board exposes only the intended workspace scope. `workloadNamespace` scopes the legacy Runs and Ray views only; `portal.jobs.scopeMode` separately authorizes the computed Jobs board.

## Add Kusto-backed boards to the same Portal block

First [prepare ADX/Kusto](../prepare-adx-kusto/). Merge the following keys into the existing `taugrid-core.portal` map above, keeping them there instead of a second values file passed alone to `tau cluster install`. A bare endpoint uses Portal's native `DefaultAzureCredential` path; `portal.kusto.queryCommand` is only an explicit adapter override and must exist in the image.

```text
# Fields to merge into the portal object already shown above.
source: kusto
kusto:
  endpoint: https://<adx>.<region>.kusto.windows.net
  database: Metrics
serviceAccount:
  annotations:
    azure.workload.identity/client-id: <portal-query-identity-client-id>
```

Durable Ray history is optional. Follow [Enable lifecycle recorder](../enable-lifecycle-recorder/) after adx-mon has successfully reconciled its lifecycle schema command and the writer identity is ready. Then merge the following field into the existing `portal` map:

```text
runHistory:
  enabled: true
```

Do this only after the recorder is successfully writing rows. This is an additional Portal capability layered on top of its core definition.

After the rollout, verify both the workload-identity injection and the durable history API before exposing Portal through an ingress:

```bash
kubectl -n "$TAU_SYSTEM_NAMESPACE" get pod -l app=tau-portal \
  -o jsonpath='{.items[0].metadata.labels.azure\.workload\.identity/use}{"\n"}'
kubectl -n "$TAU_SYSTEM_NAMESPACE" get pod -l app=tau-portal \
  -o jsonpath='{.items[0].spec.containers[0].env[?(@.name=="AZURE_FEDERATED_TOKEN_FILE")].value}{"\n"}'

kubectl -n "$TAU_SYSTEM_NAMESPACE" port-forward svc/tau-portal 18080:80
# In another terminal:
curl -fsS 'http://127.0.0.1:18080/api/portal/runs?workspace=<workspace-name>'
```

The API must report `"historyState":"available"` after the lifecycle recorder has ingested records. `history-unavailable` normally signals an ADX identity, network, schema, or release-template problem rather than evidence that Portal is reading durable history.

Portal Service is intentionally `ClusterIP`. Production access needs a platform-owned authenticated HTTPS proxy, DNS, certificate, and reviewed network path; `kubectl port-forward` is an operator diagnostic only.

## Opt into chart-managed Entra browser login

The umbrella field **`taugrid-core.portal.entraAuth.enabled: true`** installs
single-host Portal login. It defaults to `false`; ordinary installs are unchanged.
For a standalone `taugrid-core` release the field is
`portal.entraAuth.enabled`. Do not install both releases.

```text
Browser --HTTPS--> Gateway --> oauth2-proxy --HTTP--> Portal ClusterIP
                                 |
                          Microsoft Entra ID
```

All HTTPS paths, including Portal APIs and existing Portal Ray views/proxies,
go through the authenticating reverse proxy. This is not `static://200` or an
Istio external-authorization service. The chart creates no direct route to
Portal or Ray head Services, mesh extension provider, AuthorizationPolicy,
NetworkPolicy, DNS record, issuer or controller.

### Define the access boundary first

This option grants **shared viewer access** to the existing Portal, not new
per-user or per-workspace authorization. Every admitted viewer sees the data
available through Portal's configured backend identity and read-only Kubernetes
RBAC, which can be cluster-wide. Keep that scope appropriate for **all** assigned
viewers. Backend ADX Workload Identity is separate from the browser app identity.

The integration rejects `portal.workspaceDirectory.enabled=true` and
`portal.workspaceDirectory.auth.enabled=true`. The existing workspace directory
trusts headers whenever enabled; this proxy does not implement a verified mapping
to `X-MS-CLIENT-PRINCIPAL-*` or custom workspace identity headers. Do not assume
the login feature enables workspace authorization. Use an independently reviewed
proxy/header contract if workspace-directory authorization is required.

ClusterIP alone does not prevent another Pod from bypassing authentication.
Review in-cluster ingress to both Portal and proxy Services with your platform's
actual networking enforcement; remove old direct Ingress/HTTPRoute/LoadBalancer
exposure before enabling this path. Restrict who can create routes in the release
namespace, as the Gateway accepts HTTPRoutes from that namespace. Review outbound
access to Entra discovery/token endpoints and the issuer's certificate service.

### Keep callback credentials out of logs

The pinned oauth2-proxy logs full request queries by default, including OAuth
callback codes and state. The chart sets `--request-logging=false`; ordinary
standard/error and authentication logs remain enabled. Before real sign-in,
configure the operator-owned Gateway and any upstream proxy to omit query
strings from access logs or disable access logging. Do not log request cookies,
authorization headers, or token-exchange bodies. This chart does not configure
Gateway controller logging.

Verify the logging policy with a synthetic callback query, never by deliberately
writing a real authorization code or token to logs.

### Prepare operator-owned prerequisites

Complete these gates before deploying the values. Do not use application or
tenant IDs from an unrelated environment.

1. Install Gateway API **v1** CRDs and a compatible Gateway controller. The
   default class is `istio`; set `entraAuth.gatewayClassName` to another
   preinstalled compatible class when appropriate. No mesh-wide changes are
   required. Verify that the controller supports HTTP/HTTPS listeners, TLS
   termination, routing to ClusterIP Services and the application's WebSockets.
2. Install Gateway API CRDs **before cert-manager starts**, and enable
   cert-manager's Gateway integration with `config.gatewayAPI.enabled=true`
   in its operator-owned installation values. If CRDs were installed after
   cert-manager started, restart its controller after enabling the integration.
   Create an operator-owned `Issuer` in the release namespace or `ClusterIssuer`.
   Confirm issuance policy for the selected hostname. DNS-01 is supported
   through the issuer. For HTTP-01, configure the issuer's Gateway solver with
   parent references to the generated Gateway's **`http`** listener:

   ```yaml
   # Fragment of the operator-owned issuer's spec.acme.solvers entry.
   http01:
     gatewayHTTPRoute:
       parentRefs:
         - name: tau-portal-entra
           namespace: tau-system
           kind: Gateway
           sectionName: http
   ```

   The chart creates an explicit Certificate, so Gateway-shim annotations are
   unnecessary. The HTTP listener serves only operator/solver-attached routes;
   the chart adds **no HTTP Portal route or HTTP-to-HTTPS redirect**. The solver
   route must be in the Certificate's namespace, as only same-namespace routes
   are allowed. Adjust the namespace and names above for your release.
3. Enable the cluster's OIDC issuer and AKS Workload Identity webhook. The
   webhook must inject `AZURE_FEDERATED_TOKEN_FILE` and the projected token
   volume into Pods labeled `azure.workload.identity/use: "true"`. The pinned
   proxy fails startup if the variable or token path is missing; absent webhook
   injection does not fall back to an unauthenticated mode.
4. Create a dedicated **single-tenant Entra app registration** for browser
   login, with a **Web** redirect URI exactly
   `https://portal.example.com/oauth2/callback`. Replace the reserved example
   hostname everywhere. No SPA/implicit grant, client secret or certificate
   credential is needed. Set `entraAuth.clientID` to this app's application
   (client) ID and `entraAuth.tenantID` to its tenant UUID, not `common`,
   `organizations`, an object ID or the backend ADX identity.
5. On that same app, create a federated identity credential with the cluster's
   exact OIDC issuer, audience `api://AzureADTokenExchange`, and subject
   `system:serviceaccount:tau-system:tau-portal-oauth2-proxy`. For custom naming,
   use `system:serviceaccount:<release-namespace>:<portal.resourceName>-oauth2-proxy`.
   The browser app authenticates using this projected Kubernetes token directly;
   do not substitute a separate managed identity or add a client secret.
6. On the corresponding enterprise application, set **Assignment required?**
   to **Yes**, then explicitly assign only the approved users/groups. The proxy
   requests only the standard `openid` sign-in scope; do not add profile, email,
   offline-access or Graph permissions for this flow. Confirm the tenant permits
   assigned users to sign in to the application, and validate the real browser
   flow before rollout. No Graph group-membership permission is needed for this
   shared-viewer mode; avoid group claims and unnecessary permissions.
   `--email-domain=*` does **not** grant all tenant users permission: the
   single-tenant issuer and enterprise-app assignments are the admission gates.
7. Create the cookie Secret **outside Helm** in the release namespace using an
   approved secret-management process. The `cookie-secret` value must be a
   URL-safe base64-encoded random **32-byte** key, not an Entra credential. Ordinary
   base64 containing `+` or `/` is not accepted by this pinned proxy. For example,
   this operator command pipes the generated key without putting it in shell
   arguments, values files or Helm history:

   ```bash
   python3 -c 'import base64,secrets,sys; sys.stdout.write(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())' |
     kubectl --context <context> -n "$TAU_SYSTEM_NAMESPACE" create secret generic portal-cookie \
       --from-file=cookie-secret=/dev/stdin
   ```

   Protect and back up the Secret through your platform's normal process.
   Rotating it invalidates sessions; restart the proxy Deployment after rotation
   because Secret-backed environment variables are read at Pod startup.
8. Plan the DNS record and Gateway reachability for the hostname. Configure DNS
   to the controller-assigned Gateway address once available; HTTP-01 issuance
   needs externally reachable port 80. HTTPS requires a trusted certificate.
   Do not open unrelated services or bypass tenant policy to complete setup.

### Merge and render the opt-in values

Use a chart build or release containing this option. Do not assume an older
published artifact with the same development version contains it. Merge the
following into the **complete canonical values file**, retaining every other
component, queue, backend identity and storage setting. Pass the complete file
on **every** upgrade: `tau cluster install` uses `--reset-values`.

```yaml
# Merge into <platform-values.yaml>; not a complete installation configuration.
taugrid-core:
  portal:
    enabled: true
    access:
      mode: authenticated-proxy
      externalURL: https://portal.example.com
    entraAuth:
      enabled: true
      tenantID: 11111111-1111-1111-1111-111111111111 # replace
      clientID: 22222222-2222-2222-2222-222222222222 # replace
      cookieSecret:
        name: portal-cookie
        key: cookie-secret
      gatewayClassName: istio
      issuerRef:
        name: portal-issuer
        kind: ClusterIssuer
        group: cert-manager.io
```

`externalURL` is the only hostname source: an HTTPS origin, optional trailing
`/`, with no port, path, credentials, query or fragment. Open `/portal` on that
origin after login. The callback path is fixed; wildcard/multiple Ray hostnames
are deliberately unsupported.

The stock oauth2-proxy v7.15.2 image must not be used: its missing-CSRF
callback diagnostics can write the complete HTTP request, including callback
query, cookies and authorization header, to the standard logger. Configure
`entraAuth.image` with an approved build containing the callback-log privacy
fix, pinned by `sha256:` digest with an empty tag. Prefer MCR;
`imagePullSecrets`,
`resources`, `replicaCount`, `nodeSelector`, `tolerations` and `affinity` are
also configurable. Arbitrary bypass flags, inline cookie secrets and client
secrets are not supported.

Render the exact chart with the complete file and review it before applying:

```bash
helm template taugrid <exact-chart-path-or-reference> --namespace "$TAU_SYSTEM_NAMESPACE" \
  --values <platform-values.yaml>
tau cluster install --context <context> --version <release-containing-this-feature> \
  --namespace "$TAU_SYSTEM_NAMESPACE" --values <platform-values.yaml>
```

With default names this adds:

- `serviceaccount`, `deployment` and `service` named `tau-portal-oauth2-proxy`;
- `certificate`, `gateway` and `httproute` named `tau-portal-entra`;
- certificate output Secret `tau-portal-entra-tls` managed by cert-manager.

Names derive from `portal.resourceName` (DNS label starting with a letter, at
most 50 characters). The proxy's upstream uses the actual
`portal.serviceName`, release namespace and **Service port**, not container
port. The backend Deployment, RBAC and ADX identity are unchanged.

### Acceptance and troubleshooting

Deployment readiness alone is **not** browser signoff. Verify:

1. Proxy rollout succeeds; both Services stay ClusterIP. Its ServiceAccount
   has the browser app client/tenant annotations, and the Pod has the injected
   projected token file variable/volume. Do not print the token or cookie key.
   No `AZURE_CLIENT_SECRET` or `OAUTH2_PROXY_CLIENT_SECRET` should be present.
2. Certificate is `Ready`, Gateway is `Accepted`/`Programmed`, and HTTPRoute is
   `Accepted` with `ResolvedRefs` on the **HTTPS** parent. Check DNS and TLS
   hostname/chain. For pending HTTP-01 issuance, inspect the cert-manager
   Challenge and solver route's `http` parent, rather than adding a Portal
   route on port 80.
3. In a fresh private browser, `https://portal.example.com/portal` and an API URL
   such as `/api/portal/runs` must challenge for identity, not return Portal data.
   The same applies to `/healthz`; no Portal health bypass is configured.
   Proxy-owned `/ping` and `/ready` report only proxy health, not Portal content;
   these are the Deployment's liveness/readiness endpoints.
4. An assigned viewer completes sign-in through the exact callback and sees
   the Portal shell and expected boards. An unassigned tenant user and an
   account from another tenant are denied. Spoofed workspace identity headers
   must not turn unauthenticated requests into Portal responses.
5. Verify `__Host-taugrid-portal` session cookies are Secure/HttpOnly, path `/`,
   SameSite=Lax, and have **no Domain attribute**. Minimal one-hour cookie
   sessions retain no access, refresh or ID tokens and do not use offline
   refresh. Assignment removal may not revoke an existing session instantly;
   account for the one-hour session lifetime in your access-removal procedures.
6. HTTP `/portal` must not serve Portal. From untrusted in-cluster workloads,
   verify your separately configured network policy prevents reaching Portal
   directly. Verify no old direct backend routes remain. Exercise a Portal Ray
   view through the same authenticated hostname; never expose a Ray head
   Service to work around proxy failures.

For Entra callback errors, check the registered Web URI, tenant/client UUIDs,
federated credential's exact issuer/subject/audience, token injection, assignment
and consent first. For a 502 after login, check the Portal Service/endpoints and
configured Service port. `/ready` checks oauth2-proxy's session store, not the
Entra setup or Portal backend, so it cannot prove these dependencies work.

To remove this exposure, set `entraAuth.enabled=false`, return `access.mode`
to `cluster-internal` and clear `externalURL` in the complete values, then
upgrade. Remove or update platform-owned DNS and identity assignments as
appropriate; cookie Secret and issuer remain operator-owned. Confirm the
Gateway/route/proxy are removed and no alternate public backend path remains.
