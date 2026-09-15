#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
E2E_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly E2E_ROOT
readonly JOB_NAME="e2e-nccl-rdma-2x1xh200"
readonly DIAGNOSTIC_SELECTOR="e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200"
readonly RDMA_RESOURCE="rdma/rdma_shared_device_a"
readonly CONFIRMATION="create-fixed-nccl-rdma-indexed-job"
readonly JOB_FIXTURE="${E2E_ROOT}/stack/fixtures/nccl-rdma-indexed-job-2x1xh200.yaml"
readonly SECURITY_BOUNDARY_FIXTURE="${E2E_ROOT}/stack/fixtures/nccl-rdma-security-boundary.yaml"
readonly CLEANUP_OVERALL_SECONDS=180
readonly CLEANUP_REQUEST_TIMEOUT_SECONDS=10
readonly TEST_STOP_TIMEOUT_SECONDS=30
readonly IMAGE="nvcr.io/nvidia/pytorch@sha256:e14cf0da7ca0d878d0874eb81062b77df275491d4a8d030a2a7463a4e8b07f01"
readonly IMAGE_REPOSITORY="nvcr.io/nvidia/pytorch"
readonly IMAGE_INDEX_DIGEST="sha256:417cbf33f87b5378849df37983552cd1f8bc8b62fe1ceabe004de816a55dff21"
readonly IMAGE_LINUX_AMD64_DIGEST="sha256:e14cf0da7ca0d878d0874eb81062b77df275491d4a8d030a2a7463a4e8b07f01"
readonly IMAGE_CONFIG_DIGEST="sha256:06faed719d1bbd31d7053c96d0c94fce4f2b5f92fd95f07d60ec323c9744f118"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

require_env() {
  local name="$1"
  local value
  value="$(printenv "$name" || true)"
  [[ -n "$value" ]] || fail "$name is required"
  printf '%s' "$value"
}

kube() {
  kubectl \
    --kubeconfig "$(require_env NCCL_RDMA_KUBECONFIG)" \
    --context "$(require_env NCCL_RDMA_KUBE_CONTEXT)" \
    "$@"
}

cleanup_kube() {
  local deadline="$1"
  shift
  local remaining=$((deadline - SECONDS))
  ((remaining > 0)) || return 124
  local request_timeout="$CLEANUP_REQUEST_TIMEOUT_SECONDS"
  if ((remaining < request_timeout)); then
    request_timeout="$remaining"
  fi
  kubectl \
    --kubeconfig "$(require_env NCCL_RDMA_KUBECONFIG)" \
    --context "$(require_env NCCL_RDMA_KUBE_CONTEXT)" \
    --request-timeout="${request_timeout}s" \
    "$@"
}

run_owned_delete_bounded() {
  local deadline="$1"
  local namespace="$2"
  local group="$3"
  local version="$4"
  local resource="$5"
  local name="$6"
  local object_uid="$7"
  local remaining=$((deadline - SECONDS))
  ((remaining > 1)) || return 124

  python3 - "$remaining" "$E2E_ROOT" "$NCCL_RDMA_KUBECONFIG" "$NCCL_RDMA_KUBE_CONTEXT" \
    "$namespace" "$group" "$version" "$resource" "$name" "$object_uid" <<'PY'
import os
import signal
import subprocess
import sys

(
    timeout,
    cwd,
    kubeconfig,
    context,
    namespace,
    group,
    version,
    resource,
    name,
    uid,
) = sys.argv[1:]
timeout_seconds = int(timeout)
operation_timeout = timeout_seconds - 1
command = [
    "go", "run", "./cmd/nccl-rdma-owned-delete",
    "--kubeconfig", kubeconfig,
    "--context", context,
    "--group", group,
    "--version", version,
    "--resource", resource,
    "--name", name,
    "--uid", uid,
    "--timeout", f"{operation_timeout}s",
]
if namespace:
    command.extend(["--namespace", namespace])
process = subprocess.Popen(command, cwd=cwd, start_new_session=True)
try:
    raise SystemExit(process.wait(timeout=operation_timeout))
except subprocess.TimeoutExpired:
    os.killpg(process.pid, signal.SIGKILL)
    process.wait()
    raise SystemExit("owned-delete exceeded the overall cleanup deadline")
PY
}

decode_owned_uid_entry() {
  python3 - "$1" <<'PY'
import json
import sys

required = {"group", "version", "resource", "namespace", "name", "uid"}
try:
    entry = json.loads(sys.argv[1])
except json.JSONDecodeError as exc:
    raise SystemExit(f"invalid JSON: {exc}")
if not isinstance(entry, dict) or set(entry) != required:
    raise SystemExit("ledger entry must contain exactly group, version, resource, namespace, name, and uid")
for field in required:
    if not isinstance(entry[field], str):
        raise SystemExit(f"ledger field {field} must be a string")
for field in ("version", "resource", "name", "uid"):
    if not entry[field]:
        raise SystemExit(f"ledger field {field} is required")
for field, value in entry.items():
    if any(delimiter in value for delimiter in ("|", "\r", "\n")):
        raise SystemExit(f"ledger field {field} contains a forbidden delimiter")
print("|".join(entry[field] for field in ("group", "version", "resource", "namespace", "name", "uid")))
PY
}

cleanup_get_owned_json() {
  local deadline="$1"
  local namespace="$2"
  local type="$3"
  local name="$4"
  if [[ -n "$namespace" ]]; then
    cleanup_kube "$deadline" get "$type" "$name" -n "$namespace" --ignore-not-found -o json
  else
    cleanup_kube "$deadline" get "$type" "$name" --ignore-not-found -o json
  fi
}

require_qualified_image() {
  (
    cd "$E2E_ROOT"
    go run ./cmd/nccl-rdma-image-ref \
      --image "$IMAGE" \
      --repository "$IMAGE_REPOSITORY" \
      --index-digest "$IMAGE_INDEX_DIGEST" \
      --linux-amd64-digest "$IMAGE_LINUX_AMD64_DIGEST" \
      --config-digest "$IMAGE_CONFIG_DIGEST"
  ) || fail "NCCL/RDMA image reference does not match the qualified NVIDIA PyTorch linux/amd64 child"
}

ensure_invocation_marker() {
  if [[ -z "${NCCL_RDMA_INVOCATION:-}" ]]; then
    NCCL_RDMA_INVOCATION="nccl-rdma-$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
    export NCCL_RDMA_INVOCATION
  fi
  [[ "$NCCL_RDMA_INVOCATION" =~ ^nccl-rdma-[a-f0-9]{32}$ ]] \
    || fail "NCCL_RDMA_INVOCATION must be nccl-rdma- followed by 32 lowercase hex characters"
}

render_job() {
  python3 - "$JOB_FIXTURE" <<'PY'
import os
import pathlib
import sys

text = pathlib.Path(sys.argv[1]).read_text()
replacements = {
    "{{STACK_NAMESPACE}}": os.environ["E2E_STACK_NAMESPACE"],
    "{{STACK_LARGE_GPU_QUEUE}}": os.environ["E2E_STACK_LARGE_GPU_QUEUE"],
    "{{NCCL_RDMA_INVOCATION}}": os.environ["NCCL_RDMA_INVOCATION"],
    "{{GPU_NODE_SELECTOR_KEY}}": os.environ["GPU_NODE_SELECTOR_KEY"],
    "{{GPU_NODE_SELECTOR_VALUE}}": os.environ["GPU_NODE_SELECTOR_VALUE"],
}
for placeholder, value in replacements.items():
    text = text.replace(placeholder, value)
if "{{" in text or "}}" in text:
    raise SystemExit("unresolved placeholder remains in Job fixture")
sys.stdout.write(text)
PY
}

render_admission_probe() {
  render_job | python3 -c '
import sys
text = sys.stdin.read()
needle = "  suspend: true\n"
if text.count(needle) != 1:
    raise SystemExit("persisted Job fixture must contain exactly one spec.suspend=true")
sys.stdout.write(text.replace(needle, "", 1))
'
}

validate_access() {
  local kubeconfig context
  kubeconfig="$(require_env NCCL_RDMA_KUBECONFIG)"
  context="$(require_env NCCL_RDMA_KUBE_CONTEXT)"
  [[ -f "$kubeconfig" ]] || fail "NCCL_RDMA_KUBECONFIG does not exist: $kubeconfig"
  kubectl --kubeconfig "$kubeconfig" --context "$context" config view --raw --minify -o json |
    python3 -c '
import json, sys
doc = json.load(sys.stdin)
usable = len(doc.get("clusters", [])) == 1 and len(doc.get("users", [])) == 1
raise SystemExit(0 if usable else "context does not resolve one cluster and user")
' \
    || fail "cannot resolve explicit context $context from $kubeconfig"
  kube auth can-i get nodes >/dev/null 2>&1 || fail "explicit credential cannot read nodes"
  kube auth can-i get pods --all-namespaces >/dev/null 2>&1 || fail "explicit credential cannot read cluster pod capacity"
}

validate_namespace_accommodation() {
  local namespace
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  [[ "$namespace" == "taugrid-rdma-diagnostic" ]] \
    || fail "the approved security boundary is fixed to namespace taugrid-rdma-diagnostic"
  kube get namespace "$namespace" -o json | python3 -c '
import json, sys
doc = json.load(sys.stdin)
labels = doc.get("metadata", {}).get("labels", {})
annotations = doc.get("metadata", {}).get("annotations", {})
required_labels = {
    "pod-security.kubernetes.io/enforce": "restricted",
    "pod-security.kubernetes.io/enforce-version": "latest",
    "pod-security.kubernetes.io/warn": "restricted",
    "pod-security.kubernetes.io/warn-version": "latest",
    "pod-security.kubernetes.io/audit": "restricted",
    "pod-security.kubernetes.io/audit-version": "latest",
    "tau.azure.com/nccl-rdma-security-boundary": "v3",
}
required_annotations = {
    "tau.azure.com/nccl-rdma-diagnostic-approved": "true",
    "tau.azure.com/owner-role": "tau-platform-admins",
}
for key, value in required_labels.items():
    if labels.get(key) != value:
        raise SystemExit(f"namespace label {key} must equal {value}")
for key, value in required_annotations.items():
    if annotations.get(key) != value:
        raise SystemExit(f"namespace annotation {key} must equal {value}")
' || fail "namespace $namespace does not match the exact approved Restricted security boundary"
}

validate_api_and_queue() {
  local namespace queue cluster_queue
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  queue="$(require_env E2E_STACK_LARGE_GPU_QUEUE)"
  kube get --raw /apis/batch/v1 >/dev/null || fail "batch/v1 is not served"

  cluster_queue="$(kube get localqueue.kueue.x-k8s.io "$queue" -n "$namespace" -o json |
    python3 -c '
import json, sys
doc = json.load(sys.stdin)
conditions = doc.get("status", {}).get("conditions", [])
if not any(c.get("type") == "Active" and c.get("status") == "True" for c in conditions):
    raise SystemExit("LocalQueue is not Active=True")
status = doc.get("status", {})
for field in ("pendingWorkloads", "reservingWorkloads", "admittedWorkloads"):
    if int(status.get(field, 0) or 0) != 0:
        raise SystemExit(f"diagnostic LocalQueue is not idle: {field}={status.get(field)}")
cluster_queue = doc.get("spec", {}).get("clusterQueue", "")
if not cluster_queue:
    raise SystemExit("LocalQueue has no spec.clusterQueue")
print(cluster_queue)
')"
  NCCL_RDMA_CLUSTER_QUEUE="$cluster_queue"
  export NCCL_RDMA_CLUSTER_QUEUE

  kube get clusterqueue.kueue.x-k8s.io "$cluster_queue" -o json |
    python3 -c '
import json, sys
doc = json.load(sys.stdin)
spec = doc.get("spec", {})
status = doc.get("status", {})
for field in ("pendingWorkloads", "reservingWorkloads", "admittedWorkloads"):
    if int(status.get(field, 0) or 0) != 0:
        raise SystemExit(f"diagnostic ClusterQueue is not idle: {field}={status.get(field)}")
if spec.get("admissionChecks"):
    raise SystemExit("diagnostic ClusterQueue must not use admissionChecks")
if spec.get("admissionChecksStrategy"):
    raise SystemExit("diagnostic ClusterQueue must not use admissionChecksStrategy")
preemption = spec.get("preemption", {})
if preemption.get("withinClusterQueue") != "Never":
    raise SystemExit("diagnostic ClusterQueue must set preemption.withinClusterQueue=Never")
if preemption.get("reclaimWithinCohort") != "Never":
    raise SystemExit("diagnostic ClusterQueue must set preemption.reclaimWithinCohort=Never")
if preemption.get("borrowWithinCohort", {}).get("policy") != "Never":
    raise SystemExit("diagnostic ClusterQueue must set preemption.borrowWithinCohort.policy=Never")
'
}

validate_active_security_boundary() {
  local operator kueue_controller job_controller garbage_collector untrusted
  operator="$(require_env NCCL_RDMA_OPERATOR_USERNAME)"
  kueue_controller="$(require_env NCCL_RDMA_KUEUE_CONTROLLER_USERNAME)"
  job_controller="$(require_env NCCL_RDMA_JOB_CONTROLLER_USERNAME)"
  garbage_collector="$(require_env NCCL_RDMA_GARBAGE_COLLECTOR_USERNAME)"
  untrusted="$(require_env NCCL_RDMA_UNTRUSTED_USERNAME)"
  [[ "$operator" != APPROVED_* && "$kueue_controller" != APPROVED_* &&
    "$job_controller" != APPROVED_* && "$garbage_collector" != APPROVED_* &&
    "$untrusted" != APPROVED_* ]] \
    || fail "all admission identities must be exact; unresolved APPROVED_* values are forbidden"

  (
    cd "$E2E_ROOT"
    go run ./cmd/nccl-rdma-admission-check \
      --kubeconfig "$NCCL_RDMA_KUBECONFIG" \
      --context "$NCCL_RDMA_KUBE_CONTEXT" \
      --boundary "$SECURITY_BOUNDARY_FIXTURE" \
      --job "$JOB_FIXTURE" \
      --probe "$E2E_ROOT/stack/scripts/torchrun-rdma-probe.py" \
      --namespace "$E2E_STACK_NAMESPACE" \
      --queue "$E2E_STACK_LARGE_GPU_QUEUE" \
      --cluster-queue "$NCCL_RDMA_CLUSTER_QUEUE" \
      --selector-key "$GPU_NODE_SELECTOR_KEY" \
      --selector-value "$GPU_NODE_SELECTOR_VALUE" \
      --invocation "$NCCL_RDMA_INVOCATION" \
      --operator "$operator" \
      --kueue-controller "$kueue_controller" \
      --job-controller "$job_controller" \
      --garbage-collector "$garbage_collector" \
      --untrusted "$untrusted" \
      --timeout 45s
  ) || fail "active API-server-compiled NCCL/RDMA admission boundary or malicious dry-run probes failed"
}

validate_delete_access_boundary() {
  local namespace untrusted resource answer status
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  untrusted="$(require_env NCCL_RDMA_UNTRUSTED_USERNAME)"
  for resource in \
    jobs.batch \
    serviceaccounts \
    configmaps \
    secrets \
    services \
    networkpolicies.networking.k8s.io \
    pods; do
    if answer="$(kube auth can-i delete "$resource" -n "$namespace" --as="$untrusted" 2>&1)"; then
      [[ "$answer" == "yes" ]] \
        || fail "untrusted DELETE authorization query for $resource returned an unexpected successful response"
      fail "untrusted identity $untrusted can delete protected $resource resources"
    else
      status=$?
      [[ "$status" -eq 1 && "$answer" == "no" ]] \
        || fail "cannot evaluate untrusted DELETE authority for $resource: $answer"
    fi
    answer="$(kube auth can-i delete "$resource" -n "$namespace" 2>&1)" \
      || fail "cannot evaluate approved operator DELETE authority for $resource: $answer"
    [[ "$answer" == "yes" ]] \
      || fail "approved operator credential cannot delete UID-owned $resource resources during cleanup"
  done
}

validate_fixed_objects_absent() {
  local namespace type name existing pods workloads quota
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  while IFS='|' read -r type name; do
    existing="$(kube get "$type" "$name" -n "$namespace" --ignore-not-found -o name)" \
      || fail "cannot verify that fixed $type $namespace/$name is absent"
    [[ -z "$existing" ]] || fail "fixed $type $namespace/$name already exists; refusing to replace or adopt it"
  done <<'EOF'
jobs.batch|e2e-nccl-rdma-2x1xh200
serviceaccounts|nccl-rdma-runner
configmaps|nccl-rdma-probe
secrets|nccl-rdma-auth
services|e2e-nccl-rdma-rank0
networkpolicies.networking.k8s.io|nccl-rdma-isolation
EOF
  pods="$(kube get pods -n "$namespace" -l "$DIAGNOSTIC_SELECTOR" -o name)" \
    || fail "cannot check for stale diagnostic pods in $namespace"
  [[ -z "$pods" ]] || fail "stale diagnostic pods exist in $namespace; refusing mutation"
  workloads="$(kube get workloads.kueue.x-k8s.io -n "$namespace" -o name)" \
    || fail "cannot check for stale Kueue Workloads in $namespace"
  [[ -z "$workloads" ]] || fail "stale Kueue Workloads exist in $namespace; refusing mutation"
  quota="$(kube get resourcequota nccl-rdma-shape -n "$namespace" -o json)" \
    || fail "cannot read the diagnostic ResourceQuota"
  python3 -c '
import json
import sys

doc = json.load(sys.stdin)
used = doc.get("status", {}).get("used", {})
if int(used.get("count/workloads.kueue.x-k8s.io", "0")) != 0:
    raise SystemExit("diagnostic ResourceQuota still accounts for a Kueue Workload")
' <<<"$quota" || fail "diagnostic Workload quota is not fully free"
}

validate_capacity() {
  local kubeconfig context selector expected_site expected_region expected_pool expected_gpu_model
  kubeconfig="$(require_env NCCL_RDMA_KUBECONFIG)"
  context="$(require_env NCCL_RDMA_KUBE_CONTEXT)"
  selector="$(require_env NCCL_RDMA_H200_SELECTOR)"
  expected_site="${NCCL_RDMA_EXPECTED_SITE:-}"
  expected_region="${NCCL_RDMA_EXPECTED_REGION:-}"
  expected_pool="$(require_env NCCL_RDMA_EXPECTED_POOL)"
  expected_gpu_model="$(require_env NCCL_RDMA_EXPECTED_GPU_MODEL)"

  python3 - "$kubeconfig" "$context" "$selector" "$RDMA_RESOURCE" \
    "$expected_site" "$expected_region" "$expected_pool" "$expected_gpu_model" <<'PY'
import json
import subprocess
import sys
from decimal import Decimal, InvalidOperation

kubeconfig, context, selector, rdma_resource, expected_site, expected_region, expected_pool, expected_gpu_model = sys.argv[1:]
canonical_site_key = "unbounded-cloud.io/site"
legacy_site_key = "net.unbounded-cloud.io/site"
worker_requests = {
    "cpu": 4000,
    "memory": 16 * 1024**3,
    "nvidia.com/gpu": 1,
    rdma_resource: 1,
}

def kubectl(*args):
    command = ["kubectl", "--kubeconfig", kubeconfig, "--context", context, *args, "-o", "json"]
    return json.loads(subprocess.check_output(command, text=True))

nodes = kubectl("get", "nodes", "-l", selector).get("items") or []
ready = []
for node in nodes:
    conditions = node.get("status", {}).get("conditions", [])
    is_ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions)
    if is_ready and not node.get("spec", {}).get("unschedulable", False):
        ready.append(node)
if len(ready) != 2:
    raise SystemExit(f"selector {selector!r} must resolve to exactly two Ready schedulable H200 nodes; got {len(ready)}")
site_evidence = []
for node in ready:
    labels = node.get("metadata", {}).get("labels", {})
    name = node.get("metadata", {}).get("name", "")
    for taint in node.get("spec", {}).get("taints", []):
        key = taint.get("key", "")
        effect = taint.get("effect", "")
        if (key, effect) not in {
            ("nvidia.com/gpu", "NoSchedule"),
            ("sku", "NoSchedule"),
        }:
            raise SystemExit(
                f"node {name} has disallowed taint {key}={taint.get('value', '')}:{effect}"
            )
    if expected_region and labels.get("topology.kubernetes.io/region") != expected_region:
        raise SystemExit(f"node {name} is not in expected region {expected_region}")
    canonical_site = labels.get(canonical_site_key, "").strip()
    legacy_site = labels.get(legacy_site_key, "").strip()
    if canonical_site:
        site_evidence.append((name, canonical_site, canonical_site_key))
        if legacy_site and legacy_site != canonical_site:
            raise SystemExit(
                f"node {name} has conflicting Unbounded site labels: "
                f"{canonical_site_key}={canonical_site!r}, {legacy_site_key}={legacy_site!r}; "
                "canonical value selected but topology evidence is incomplete"
            )
    elif legacy_site:
        site_evidence.append((name, legacy_site, legacy_site_key))
    else:
        site_evidence.append((name, "", ""))
    if labels.get("kubernetes.azure.com/agentpool") != expected_pool:
        raise SystemExit(f"node {name} is not in expected pool {expected_pool}")
    product = labels.get("nvidia.com/gpu.product", "")
    if product and expected_gpu_model.lower().replace(" ", "") not in product.lower().replace("-", "").replace("_", ""):
        raise SystemExit(f"node {name} GPU product {product!r} does not match expected model {expected_gpu_model!r}")

present_sites = [item for item in site_evidence if item[1]]
if not present_sites:
    if expected_site:
        raise SystemExit(
            f"expected Unbounded site {expected_site!r}, but neither exact supported site label "
            "was present on either selected node"
        )
    print("Unbounded site topology is not applicable: neither exact supported site label is present")
elif len(present_sites) != len(site_evidence):
    rendered = ", ".join(
        f"{name}={value!r} via {source or 'none'}" for name, value, source in site_evidence
    )
    raise SystemExit(f"Unbounded site label evidence is partial across selected nodes: {rendered}")
elif len({item[1] for item in present_sites}) != 1:
    rendered = ", ".join(
        f"{name}={value!r} via {source}" for name, value, source in site_evidence
    )
    raise SystemExit(f"selected nodes resolve to different Unbounded sites: {rendered}")
elif not expected_site:
    raise SystemExit(
        "NCCL_RDMA_EXPECTED_SITE is required when exact Unbounded site labels are present"
    )
elif present_sites[0][1] != expected_site:
    raise SystemExit(
        f"selected nodes resolve to Unbounded site {present_sites[0][1]!r}, "
        f"not expected site {expected_site!r}"
    )
else:
    print(
        "Unbounded site topology resolved: "
        + ", ".join(f"{name}={value!r} via {source}" for name, value, source in site_evidence)
    )

pods = kubectl("get", "pods", "--all-namespaces").get("items") or []

def quantity(value, resource):
    text = str(value or "0")
    try:
        if resource == "cpu":
            return int(Decimal(text[:-1])) if text.endswith("m") else int(Decimal(text) * 1000)
        if resource == "memory":
            suffixes = {
                "Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, "Ti": 1024**4,
                "K": 1000, "M": 1000**2, "G": 1000**3, "T": 1000**4,
            }
            for suffix, multiplier in suffixes.items():
                if text.endswith(suffix):
                    return int(Decimal(text[:-len(suffix)]) * multiplier)
            return int(Decimal(text))
        return int(Decimal(text))
    except (InvalidOperation, ValueError):
        raise SystemExit(f"cannot parse Kubernetes quantity {text!r} for {resource}")

def container_request(container, resource):
    resources = container.get("resources", {})
    value = resources.get("requests", {}).get(resource)
    if value is None and resource in {"cpu", "memory"}:
        value = resources.get("limits", {}).get(resource)
    return quantity(value, resource)

def pod_request(pod, resource):
    containers = pod.get("spec", {}).get("containers", [])
    init_containers = pod.get("spec", {}).get("initContainers", [])
    regular = sum(container_request(c, resource) for c in containers)
    restartable_init = 0
    init_peak = 0
    for container in init_containers:
        request = container_request(container, resource)
        if container.get("restartPolicy") == "Always":
            restartable_init += request
            init_use = restartable_init
        else:
            init_use = restartable_init + request
        init_peak = max(init_peak, init_use)
    overhead = quantity(pod.get("spec", {}).get("overhead", {}).get(resource), resource)
    return max(regular + restartable_init, init_peak) + overhead

for pod in pods:
    if pod.get("status", {}).get("phase") in {"Succeeded", "Failed"}:
        continue
    if pod.get("spec", {}).get("nodeName"):
        continue
    gpu = pod_request(pod, "nvidia.com/gpu")
    rdma = pod_request(pod, rdma_resource)
    if gpu or rdma:
        raise SystemExit(
            f'unassigned GPU/RDMA pod {pod["metadata"]["namespace"]}/{pod["metadata"]["name"]} '
            f'could race diagnostic placement (gpu={gpu},rdma={rdma})'
        )

for node in ready:
    name = node["metadata"]["name"]
    allocatable = node.get("status", {}).get("allocatable", {})
    consumers = []
    used = {resource: 0 for resource in worker_requests}
    for pod in pods:
        if pod.get("spec", {}).get("nodeName") != name:
            continue
        if pod.get("status", {}).get("phase") in {"Succeeded", "Failed"}:
            continue
        requests = {resource: pod_request(pod, resource) for resource in worker_requests}
        for resource, request in requests.items():
            used[resource] += request
        if any(requests.values()):
            consumers.append(
                f'{pod["metadata"]["namespace"]}/{pod["metadata"]["name"]}'
                f'(cpu={requests["cpu"]}m,memory={requests["memory"]},'
                f'gpu={requests["nvidia.com/gpu"]},rdma={requests[rdma_resource]})'
            )
    if used[rdma_resource]:
        raise SystemExit(
            f"node {name} has unrelated RDMA consumers; refusing transport validation: "
            + ", ".join(consumers)
        )
    for resource, required in worker_requests.items():
        total = quantity(allocatable.get(resource), resource)
        free = total - used[resource]
        if free < required:
            raise SystemExit(
                f"node {name} lacks requested headroom for {resource}: "
                f"required={required}, allocatable={total}, used={used[resource]}, free={free}; "
                f"consumers={', '.join(consumers) or 'none'}"
            )

print(
    "Request-based capacity preflight passed for one 4-CPU/16Gi/1-GPU/1-RDMA "
    "pod on each of: " + ", ".join(node["metadata"]["name"] for node in ready)
)
PY
}

prepare_result_contract() {
  local launch_dir source_revision
  launch_dir="$PWD"
  require_env NCCL_RDMA_WORKSPACE_ID >/dev/null
  require_env NCCL_RDMA_CLUSTER >/dev/null
  require_env NCCL_RDMA_EXPECTED_POOL >/dev/null
  require_env NCCL_RDMA_EXPECTED_GPU_MODEL >/dev/null
  [[ -z "$(git -C "$E2E_ROOT/../.." status --porcelain)" ]] \
    || fail "the RDMA validation source tree must be clean so source_revision identifies the executed code"
  source_revision="$(git -C "$E2E_ROOT/../.." rev-parse HEAD)"
  [[ "$source_revision" =~ ^[a-f0-9]{40}$ ]] || fail "cannot resolve an exact lowercase Git source revision"
  NCCL_RDMA_SOURCE_REVISION="$source_revision"
  NCCL_RDMA_RUN_ID="${NCCL_RDMA_RUN_ID:-$NCCL_RDMA_INVOCATION}"
  NCCL_RDMA_RUN_ATTEMPT="${NCCL_RDMA_RUN_ATTEMPT:-1}"
  [[ "$NCCL_RDMA_RUN_ID" =~ ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$ ]] \
    || fail "NCCL_RDMA_RUN_ID must satisfy the shared lowercase identifier contract"
  [[ "$NCCL_RDMA_RUN_ATTEMPT" =~ ^[1-9][0-9]*$ ]] \
    || fail "NCCL_RDMA_RUN_ATTEMPT must be a positive integer"
  NCCL_RDMA_RESULT_PATH="${NCCL_RDMA_RESULT_PATH:-${launch_dir}/rdma-validation/${NCCL_RDMA_INVOCATION}.json}"
  if [[ "$NCCL_RDMA_RESULT_PATH" != /* ]]; then
    NCCL_RDMA_RESULT_PATH="${launch_dir}/${NCCL_RDMA_RESULT_PATH}"
  fi
  [[ ! -e "$NCCL_RDMA_RESULT_PATH" ]] \
    || fail "immutable RDMA validation artifact already exists: $NCCL_RDMA_RESULT_PATH"
  export NCCL_RDMA_SOURCE_REVISION NCCL_RDMA_RUN_ID NCCL_RDMA_RUN_ATTEMPT NCCL_RDMA_RESULT_PATH
}

preflight() {
  require_env E2E_STACK_NAMESPACE >/dev/null
  require_env E2E_STACK_LARGE_GPU_QUEUE >/dev/null
  require_env GPU_NODE_SELECTOR_KEY >/dev/null
  require_env GPU_NODE_SELECTOR_VALUE >/dev/null
  require_env NCCL_RDMA_EXPECTED_POOL >/dev/null
  require_env NCCL_RDMA_EXPECTED_GPU_MODEL >/dev/null
  [[ "$(require_env NCCL_RDMA_H200_SELECTOR)" == "$(require_env GPU_NODE_SELECTOR_KEY)=$(require_env GPU_NODE_SELECTOR_VALUE)" ]] \
    || fail "NCCL_RDMA_H200_SELECTOR must exactly match GPU_NODE_SELECTOR_KEY=GPU_NODE_SELECTOR_VALUE"
  require_qualified_image
  ensure_invocation_marker
  validate_access
  validate_namespace_accommodation
  validate_api_and_queue
  validate_fixed_objects_absent
  validate_active_security_boundary
  validate_delete_access_boundary
  validate_capacity
  echo "Read-only NCCL/RDMA preflight passed, including exact active VAP/binding checks and allow/deny server-side dry-runs. No resources were persisted."
}

diagnostics() {
  local namespace
  local deadline=$((SECONDS + 60))
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  echo "=== Indexed Job ===" >&2
  cleanup_kube "$deadline" get job.batch "$JOB_NAME" -n "$namespace" -o yaml >&2 || true
  echo "=== Kueue Workloads ===" >&2
  cleanup_kube "$deadline" get workloads.kueue.x-k8s.io -n "$namespace" -o wide >&2 || true
  echo "=== Diagnostic pods ===" >&2
  cleanup_kube "$deadline" get pods -n "$namespace" -l "$DIAGNOSTIC_SELECTOR" -o wide >&2 || true
  echo "=== Namespace events ===" >&2
  cleanup_kube "$deadline" get events -n "$namespace" --sort-by=.lastTimestamp >&2 || true
}

cleanup_owned_uids() {
  local entry parsed group version resource namespace type name object_uid object current_uid pods index cleanup_failed
  local job_entry="" job_parsed="" barrier_complete
  local -a entries=()
  local -a support_entries=()
  local -a remaining_entries=()
  local deadline=$((SECONDS + CLEANUP_OVERALL_SECONDS))
  cleanup_failed=0
  [[ -n "${NCCL_RDMA_OWNED_UID_FILE:-}" && -f "$NCCL_RDMA_OWNED_UID_FILE" ]] \
    || fail "successful-create UID ledger is unavailable; refusing name or label based cleanup"
  while IFS= read -r entry; do
    entries+=("$entry")
  done <"$NCCL_RDMA_OWNED_UID_FILE"
  for entry in "${entries[@]}"; do
    parsed="$(decode_owned_uid_entry "$entry")" \
      || fail "malformed successful-create UID ledger entry; refusing cleanup"
    IFS='|' read -r group version resource namespace name object_uid <<<"$parsed"
    if [[ "$group" == "batch" && "$resource" == "jobs" && "$name" == "$JOB_NAME" ]]; then
      [[ -z "$job_entry" ]] || fail "successful-create UID ledger contains duplicate diagnostic Job entries"
      job_entry="$entry"
      job_parsed="$parsed"
      continue
    fi
    support_entries+=("$entry")
  done

  if [[ -n "$job_entry" ]]; then
    IFS='|' read -r group version resource namespace name object_uid <<<"$job_parsed"
    type="$resource"
    [[ -z "$group" ]] || type="$resource.$group"
    run_owned_delete_bounded "$deadline" "$namespace" "$group" "$version" "$resource" "$name" "$object_uid" \
      || {
        echo "ERROR: UID-precondition deletion failed for $resource ${namespace:+$namespace/}$name UID $object_uid" >&2
        return 1
      }
    barrier_complete=0
    while ((SECONDS < deadline)); do
      object="$(cleanup_get_owned_json "$deadline" "$namespace" "$type" "$name")" \
        || {
          echo "ERROR: cannot verify diagnostic Job cleanup for $resource ${namespace:+$namespace/}$name" >&2
          break
        }
      if [[ -n "$object" ]]; then
        current_uid="$(python3 -c 'import json, sys; print(json.load(sys.stdin).get("metadata", {}).get("uid", ""))' <<<"$object")"
        if [[ "$current_uid" == "$object_uid" ]]; then
          sleep 1
          continue
        fi
      fi
      pods="$(cleanup_kube "$deadline" get pods -n "$(require_env E2E_STACK_NAMESPACE)" -l "$DIAGNOSTIC_SELECTOR" -o name)" \
        || {
          echo "ERROR: cannot verify diagnostic Pod drain before support-resource cleanup" >&2
          break
        }
      if [[ -z "$pods" ]]; then
        barrier_complete=1
        break
      fi
      sleep 1
    done
    if ((barrier_complete == 0)); then
      echo "ERROR: diagnostic Job or Pods remained; preserving NetworkPolicy and support resources" >&2
      return 1
    fi
  fi

  for ((index=${#support_entries[@]} - 1; index >= 0; index--)); do
    entry="${support_entries[index]}"
    parsed="$(decode_owned_uid_entry "$entry")" \
      || fail "malformed successful-create UID ledger entry; refusing cleanup"
    IFS='|' read -r group version resource namespace name object_uid <<<"$parsed"
    type="$resource"
    [[ -z "$group" ]] || type="$resource.$group"
    run_owned_delete_bounded "$deadline" "$namespace" "$group" "$version" "$resource" "$name" "$object_uid" \
      || {
        echo "ERROR: UID-precondition deletion failed for $resource ${namespace:+$namespace/}$name UID $object_uid" >&2
        cleanup_failed=1
      }
  done
  remaining_entries=("${support_entries[@]}")
  while ((SECONDS < deadline && ${#remaining_entries[@]} > 0)); do
    entries=("${remaining_entries[@]}")
    remaining_entries=()
    for entry in "${entries[@]}"; do
      parsed="$(decode_owned_uid_entry "$entry")" \
        || fail "malformed successful-create UID ledger entry during verification; refusing cleanup"
      IFS='|' read -r group version resource namespace name object_uid <<<"$parsed"
      type="$resource"
      [[ -z "$group" ]] || type="$resource.$group"
      object="$(cleanup_get_owned_json "$deadline" "$namespace" "$type" "$name")" \
        || {
          echo "ERROR: cannot verify cleanup for $resource ${namespace:+$namespace/}$name" >&2
          remaining_entries+=("$entry")
          cleanup_failed=1
          continue
        }
      [[ -z "$object" ]] && continue
      current_uid="$(python3 -c 'import json, sys; print(json.load(sys.stdin).get("metadata", {}).get("uid", ""))' <<<"$object")"
      [[ "$current_uid" != "$object_uid" ]] && continue
      remaining_entries+=("$entry")
    done
    ((${#remaining_entries[@]} == 0)) || sleep 1
  done
  for entry in "${remaining_entries[@]}"; do
    parsed="$(decode_owned_uid_entry "$entry")" \
      || fail "malformed successful-create UID ledger entry during timeout reporting"
    IFS='|' read -r group version resource namespace name object_uid <<<"$parsed"
    echo "ERROR: overall cleanup deadline expired for $resource ${namespace:+$namespace/}$name UID $object_uid" >&2
    cleanup_failed=1
  done
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  while ((SECONDS < deadline)); do
    pods="$(cleanup_kube "$deadline" get pods -n "$namespace" -l "$DIAGNOSTIC_SELECTOR" -o name)" \
      || {
        echo "ERROR: cannot verify final diagnostic pod cleanup in $namespace" >&2
        cleanup_failed=1
        break
      }
    if [[ -z "$pods" ]]; then
      ((cleanup_failed == 0))
      return
    fi
    sleep 1
  done
  [[ -z "${pods:-}" ]] || {
    echo "ERROR: overall cleanup deadline expired with diagnostic pods still present in $namespace" >&2
    cleanup_failed=1
  }
  ((cleanup_failed == 0))
}

NCCL_RDMA_TEST_PID=""

stop_managed_test() {
  local signal="$1"
  local status="$2"
  local deadline pid
  trap - INT TERM
  pid="${NCCL_RDMA_TEST_PID:-}"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill -s "$signal" -- "-$pid" 2>/dev/null || kill -s "$signal" "$pid" 2>/dev/null || true
    deadline=$((SECONDS + TEST_STOP_TIMEOUT_SECONDS))
    while kill -0 "$pid" 2>/dev/null && ((SECONDS < deadline)); do
      sleep 1
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
    fi
    wait "$pid" 2>/dev/null || true
  fi
  NCCL_RDMA_TEST_PID=""
  exit "$status"
}

cleanup_on_exit() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "${NCCL_RDMA_OWNED_UID_FILE:-}" ]] && ! cleanup_owned_uids; then
    status=1
  fi
  [[ -z "${NCCL_RDMA_OWNED_UID_FILE:-}" ]] || rm -f -- "$NCCL_RDMA_OWNED_UID_FILE"
  exit "$status"
}

run_diagnostic() {
  [[ "${NCCL_RDMA_CONFIRM:-}" == "$CONFIRMATION" ]] \
    || fail "set NCCL_RDMA_CONFIRM=$CONFIRMATION to authorize creating only the fixed diagnostic resources"
  ensure_invocation_marker
  preflight
  prepare_result_contract
  NCCL_RDMA_OWNED_UID_FILE="$(mktemp "${TMPDIR:-/tmp}/taugrid-nccl-rdma-owned.XXXXXX")"
  chmod 600 "$NCCL_RDMA_OWNED_UID_FILE"
  export NCCL_RDMA_OWNED_UID_FILE
  trap cleanup_on_exit EXIT
  trap 'stop_managed_test INT 130' INT
  trap 'stop_managed_test TERM 143' TERM

  export AI_RUNTIME_E2E=1
  export E2E_GPU=1
  export E2E_NCCL_RDMA=1
  export E2E_STACK_USE_ARGOCD_QUEUE=1
  export E2E_STACK_QUEUE="$E2E_STACK_LARGE_GPU_QUEUE"
  export KUBECONFIG="$NCCL_RDMA_KUBECONFIG"
  export AI_RUNTIME_E2E_KUBE_CONTEXT="$NCCL_RDMA_KUBE_CONTEXT"

  python3 - "$E2E_ROOT" <<'PY' &
import os
import sys

os.chdir(sys.argv[1])
os.setsid()
os.execvp(
    "go",
    [
        "go", "test", "-count=1", "-v", "-timeout", "15m",
        "-run", "^TestNCCLRDMA2x1H200$", "./stack/",
    ],
)
PY
  NCCL_RDMA_TEST_PID=$!
  if ! wait "$NCCL_RDMA_TEST_PID"; then
    NCCL_RDMA_TEST_PID=""
    diagnostics
    fail "NCCL/RDMA diagnostic failed; no retry was attempted"
  fi
  NCCL_RDMA_TEST_PID=""
  [[ -f "$NCCL_RDMA_RESULT_PATH" ]] \
    || fail "NCCL/RDMA test succeeded without writing the immutable validation artifact"
  python3 - "$NCCL_RDMA_RESULT_PATH" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    result = json.load(stream)
if result.get("schema") != "rdma-validation.v1" or result.get("kind") != "tau.rdma_validation":
    raise SystemExit("validation artifact schema or kind changed")
if result.get("status") != "pass" or result.get("cleanup", {}).get("state") != "complete":
    raise SystemExit("validation artifact is not a cleanup-complete pass")
PY
  echo "Immutable NCCL/RDMA validation artifact: $NCCL_RDMA_RESULT_PATH"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  case "${1:-preflight}" in
    preflight)
      preflight
      ;;
    run)
      run_diagnostic
      ;;
    *)
      fail "usage: $0 [preflight|run]"
      ;;
  esac
fi
