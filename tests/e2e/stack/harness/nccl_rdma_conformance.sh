#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
E2E_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly E2E_ROOT
readonly MPIJOB_NAME="e2e-nccl-rdma-2x8xh200"
readonly DIAGNOSTIC_SELECTOR="e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x8xh200"
readonly INVOCATION_LABEL="e2e.taugrid.azure.com/invocation"
readonly RDMA_RESOURCE="rdma/rdma_shared_device_a"
readonly CONFIRMATION="apply-fixed-nccl-rdma-mpijob"
readonly MPIJOB_FIXTURE="${E2E_ROOT}/stack/fixtures/nccl-rdma-mpijob-2x8xh200.yaml"
readonly CLEANUP_OVERALL_SECONDS=180
readonly CLEANUP_REQUEST_TIMEOUT_SECONDS=10

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
  local object_uid="$3"
  local remaining=$((deadline - SECONDS))
  ((remaining > 1)) || return 124

  python3 - "$remaining" "$E2E_ROOT" "$NCCL_RDMA_KUBECONFIG" "$NCCL_RDMA_KUBE_CONTEXT" "$namespace" "$object_uid" <<'PY'
import os
import signal
import subprocess
import sys

timeout, cwd, kubeconfig, context, namespace, uid = sys.argv[1:]
timeout_seconds = int(timeout)
operation_timeout = timeout_seconds - 1
command = [
    "go", "run", "./cmd/nccl-rdma-owned-delete",
    "--kubeconfig", kubeconfig,
    "--context", context,
    "--namespace", namespace,
    "--uid", uid,
    "--timeout", f"{operation_timeout}s",
]
process = subprocess.Popen(command, cwd=cwd, start_new_session=True)
try:
    raise SystemExit(process.wait(timeout=operation_timeout))
except subprocess.TimeoutExpired:
    os.killpg(process.pid, signal.SIGKILL)
    process.wait()
    raise SystemExit("owned-delete exceeded the overall cleanup deadline")
PY
}

require_digest_image() {
  local image
  image="$(require_env NCCL_RDMA_E2E_IMAGE)"
  [[ "$image" =~ ^mcr\.microsoft\.com/aks/ai-runtime/nccl-tests@sha256:[a-f0-9]{64}$ ]] \
    || fail "NCCL_RDMA_E2E_IMAGE must be mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256:<64 lowercase hex>"
}

ensure_invocation_marker() {
  if [[ -z "${NCCL_RDMA_INVOCATION:-}" ]]; then
    NCCL_RDMA_INVOCATION="nccl-rdma-$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
    export NCCL_RDMA_INVOCATION
  fi
  [[ "$NCCL_RDMA_INVOCATION" =~ ^nccl-rdma-[a-f0-9]{32}$ ]] \
    || fail "NCCL_RDMA_INVOCATION must be nccl-rdma- followed by 32 lowercase hex characters"
}

render_mpijob() {
  python3 - "$MPIJOB_FIXTURE" <<'PY'
import os
import pathlib
import sys

text = pathlib.Path(sys.argv[1]).read_text()
replacements = {
    "{{STACK_NAMESPACE}}": os.environ["E2E_STACK_NAMESPACE"],
    "{{STACK_LARGE_GPU_QUEUE}}": os.environ["E2E_STACK_LARGE_GPU_QUEUE"],
    "{{NCCL_RDMA_IMAGE}}": os.environ["NCCL_RDMA_E2E_IMAGE"],
    "{{NCCL_RDMA_INVOCATION}}": os.environ["NCCL_RDMA_INVOCATION"],
    "{{GPU_NODE_SELECTOR_KEY}}": os.environ["GPU_NODE_SELECTOR_KEY"],
    "{{GPU_NODE_SELECTOR_VALUE}}": os.environ["GPU_NODE_SELECTOR_VALUE"],
}
for placeholder, value in replacements.items():
    text = text.replace(placeholder, value)
if "{{" in text or "}}" in text:
    raise SystemExit("unresolved placeholder remains in MPIJob fixture")
sys.stdout.write(text)
PY
}

render_admission_probe() {
  render_mpijob | python3 -c '
import sys
text = sys.stdin.read()
needle = "  runPolicy:\n    suspend: true\n"
if text.count(needle) != 1:
    raise SystemExit("persisted MPIJob fixture must contain exactly one spec.runPolicy.suspend=true")
sys.stdout.write(text.replace(needle, "  runPolicy:\n", 1))
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
  local namespace enforce approval
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  kube get namespace "$namespace" >/dev/null
  enforce="$(kube get namespace "$namespace" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')"
  approval="$(kube get namespace "$namespace" -o jsonpath='{.metadata.annotations.tau\.azure\.com/nccl-rdma-diagnostic-approved}')"
  [[ "$enforce" == "privileged" ]] \
    || fail "namespace $namespace lacks the pre-existing approved Pod Security accommodation (enforce=privileged); this harness will not modify namespace labels"
  [[ "$approval" == "true" ]] \
    || fail "namespace $namespace lacks annotation tau.azure.com/nccl-rdma-diagnostic-approved=true"
}

validate_api_and_queue() {
  local namespace queue controller_selector config_reference config_map config_key
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  queue="$(require_env E2E_STACK_LARGE_GPU_QUEUE)"

  kube get --raw /apis/kubeflow.org/v2beta1 >/dev/null \
    || fail "kubeflow.org/v2beta1 is not served"
  kube get customresourcedefinition mpijobs.kubeflow.org -o json |
    python3 -c '
import json, sys
doc = json.load(sys.stdin)
served = any(v.get("name") == "v2beta1" and v.get("served") for v in doc["spec"]["versions"])
raise SystemExit(0 if served else "MPIJob CRD does not serve v2beta1")
'

  kube get localqueue.kueue.x-k8s.io "$queue" -n "$namespace" -o json |
    python3 -c '
import json, sys
doc = json.load(sys.stdin)
conditions = doc.get("status", {}).get("conditions", [])
active = any(c.get("type") == "Active" and c.get("status") == "True" for c in conditions)
raise SystemExit(0 if active else "LocalQueue is not Active=True")
'

  controller_selector="$(kube get deployment kueue-controller-manager -n kueue-system -o json |
    python3 -c '
import json, sys
doc = json.load(sys.stdin)
labels = doc.get("spec", {}).get("selector", {}).get("matchLabels", {})
if not labels:
    raise SystemExit("Kueue controller Deployment has no matchLabels selector")
print(",".join(f"{key}={value}" for key, value in sorted(labels.items())))
')"

  config_reference="$(kube get pods -n kueue-system -l "$controller_selector" -o json |
    python3 -c '
import json, pathlib, sys
doc = json.load(sys.stdin)
references = set()
ready_pods = 0
for pod in doc.get("items", []):
    if pod.get("status", {}).get("phase") != "Running":
        continue
    conditions = pod.get("status", {}).get("conditions", [])
    if not any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions):
        continue
    ready_pods += 1
    spec = pod.get("spec", {})
    pod_name = pod.get("metadata", {}).get("name", "<unknown>")
    volumes = {v["name"]: v for v in spec.get("volumes", [])}
    pod_reference = None
    for container in spec.get("containers", []):
        args = container.get("args", [])
        config_path = None
        for index, arg in enumerate(args):
            if arg.startswith("--config="):
                config_path = arg.split("=", 1)[1]
                break
            if arg == "--config" and index + 1 < len(args):
                config_path = args[index + 1]
                break
        if not config_path:
            continue
        for mount in sorted(container.get("volumeMounts", []), key=lambda item: len(item["mountPath"]), reverse=True):
            mount_path = mount["mountPath"].rstrip("/")
            if config_path != mount_path and not config_path.startswith(mount_path + "/"):
                continue
            volume = volumes.get(mount["name"], {})
            config_map = volume.get("configMap", {})
            name = config_map.get("name")
            if not name:
                raise SystemExit(f"running Kueue controller config {config_path} is not mounted from a ConfigMap")
            if mount.get("subPath"):
                key = mount["subPath"]
            else:
                relative = pathlib.PurePosixPath(config_path).relative_to(pathlib.PurePosixPath(mount_path))
                key = str(relative)
                for item in config_map.get("items", []):
                    if item.get("path") == key:
                        key = item["key"]
                        break
            pod_reference = (name, key)
            break
        if pod_reference:
            break
    if not pod_reference:
        raise SystemExit(f"Ready Kueue controller pod {pod_name} does not expose its --config ConfigMap reference")
    references.add(pod_reference)
if ready_pods == 0:
    raise SystemExit("Kueue controller has no Ready pod")
if not references:
    raise SystemExit("no Ready Kueue controller pod exposes its --config ConfigMap reference")
if len(references) != 1:
    raise SystemExit(f"Ready Kueue controller pods use inconsistent config references: {sorted(references)}")
name, key = references.pop()
print(f"{name}\t{key}")
')"
  IFS=$'\t' read -r config_map config_key <<<"$config_reference"
  [[ -n "$config_map" && -n "$config_key" ]] \
    || fail "could not resolve the ConfigMap key mounted by the running Kueue controller"

  kube get configmap "$config_map" -n kueue-system -o json |
    python3 -c '
import json, sys
doc = json.load(sys.stdin)
key = sys.argv[1]
value = doc.get("data", {}).get(key)
if value is None:
    raise SystemExit(f"running Kueue controller ConfigMap lacks referenced key {key!r}")
if "kubeflow.org/mpijob" not in value:
    raise SystemExit("running Kueue controller config does not advertise kubeflow.org/mpijob integration")
' "$config_key"
}

validate_server_side_admission() {
  local response
  response="$(render_admission_probe | kube create --dry-run=server -f - -o json)" \
    || fail "server-side MPIJob admission dry-run failed"
  python3 -c '
import json, os, sys
doc = json.load(sys.stdin)
if doc.get("metadata", {}).get("name") != "e2e-nccl-rdma-2x8xh200":
    raise SystemExit("admission dry-run returned an unexpected object")
labels = doc.get("metadata", {}).get("labels", {})
if labels.get("e2e.taugrid.azure.com/invocation") != os.environ["NCCL_RDMA_INVOCATION"]:
    raise SystemExit("admission dry-run lost the unique invocation marker")
if doc.get("spec", {}).get("runPolicy", {}).get("suspend") is not True:
    raise SystemExit("Kueue admission dry-run did not set spec.runPolicy.suspend=true")
' <<<"$response"
}

validate_fixed_object_absent() {
  local namespace object pods
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  object="$(kube get mpijob.kubeflow.org "$MPIJOB_NAME" -n "$namespace" --ignore-not-found -o name)" \
    || fail "cannot verify that fixed MPIJob $namespace/$MPIJOB_NAME is absent"
  if [[ -n "$object" ]]; then
    fail "fixed MPIJob $namespace/$MPIJOB_NAME already exists; refusing to replace or adopt it"
  fi
  pods="$(kube get pods -n "$namespace" -l "$DIAGNOSTIC_SELECTOR" -o name)" \
    || fail "cannot check for stale diagnostic pods in $namespace"
  if [[ -n "$pods" ]]; then
    fail "stale diagnostic pods exist in $namespace; refusing to mutate until an operator investigates"
  fi
}

validate_capacity() {
  local kubeconfig context selector
  kubeconfig="$(require_env NCCL_RDMA_KUBECONFIG)"
  context="$(require_env NCCL_RDMA_KUBE_CONTEXT)"
  selector="$(require_env NCCL_RDMA_H200_SELECTOR)"

  python3 - "$kubeconfig" "$context" "$selector" "$RDMA_RESOURCE" <<'PY'
import json
import subprocess
import sys

kubeconfig, context, selector, rdma_resource = sys.argv[1:]

def kubectl(*args):
    command = ["kubectl", "--kubeconfig", kubeconfig, "--context", context, *args, "-o", "json"]
    return json.loads(subprocess.check_output(command, text=True))

nodes = kubectl("get", "nodes", "-l", selector).get("items", [])
ready = []
for node in nodes:
    conditions = node.get("status", {}).get("conditions", [])
    is_ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions)
    if is_ready and not node.get("spec", {}).get("unschedulable", False):
        ready.append(node)

if len(ready) != 2:
    raise SystemExit(f"selector {selector!r} must resolve to exactly two Ready schedulable H200 nodes; got {len(ready)}")

pods = kubectl("get", "pods", "--all-namespaces").get("items", [])

def quantity(value):
    text = str(value or "0")
    return int(text) if text.isdigit() else 0

def pod_request(pod, resource):
    containers = pod.get("spec", {}).get("containers", [])
    init_containers = pod.get("spec", {}).get("initContainers", [])
    regular = sum(quantity(c.get("resources", {}).get("requests", {}).get(resource)) for c in containers)
    init_max = max(
        [quantity(c.get("resources", {}).get("requests", {}).get(resource)) for c in init_containers],
        default=0,
    )
    return max(regular, init_max)

for node in ready:
    name = node["metadata"]["name"]
    allocatable = node.get("status", {}).get("allocatable", {})
    gpu_total = quantity(allocatable.get("nvidia.com/gpu"))
    rdma_total = quantity(allocatable.get(rdma_resource))
    if gpu_total < 8 or rdma_total < 1:
        raise SystemExit(
            f"node {name} needs at least 8 GPUs and 1 {rdma_resource}; "
            f"allocatable is gpu={gpu_total}, rdma={rdma_total}"
        )

    consumers = []
    gpu_used = 0
    rdma_used = 0
    for pod in pods:
        if pod.get("spec", {}).get("nodeName") != name:
            continue
        if pod.get("status", {}).get("phase") in {"Succeeded", "Failed"}:
            continue
        gpu = pod_request(pod, "nvidia.com/gpu")
        rdma = pod_request(pod, rdma_resource)
        gpu_used += gpu
        rdma_used += rdma
        if gpu or rdma:
            consumers.append(f'{pod["metadata"]["namespace"]}/{pod["metadata"]["name"]}(gpu={gpu},rdma={rdma})')

    if consumers:
        raise SystemExit(f"node {name} has unrelated GPU/RDMA consumers; refusing to run: {', '.join(consumers)}")
    if gpu_total - gpu_used < 8 or rdma_total - rdma_used < 1:
        raise SystemExit(f"node {name} lacks 8 free GPUs and one free {rdma_resource}")

print("Capacity preflight passed for: " + ", ".join(node["metadata"]["name"] for node in ready))
PY
}

preflight() {
  require_env E2E_STACK_NAMESPACE >/dev/null
  require_env E2E_STACK_LARGE_GPU_QUEUE >/dev/null
  require_env GPU_NODE_SELECTOR_KEY >/dev/null
  require_env GPU_NODE_SELECTOR_VALUE >/dev/null
  [[ "$(require_env NCCL_RDMA_H200_SELECTOR)" == "$(require_env GPU_NODE_SELECTOR_KEY)=$(require_env GPU_NODE_SELECTOR_VALUE)" ]] \
    || fail "NCCL_RDMA_H200_SELECTOR must exactly match GPU_NODE_SELECTOR_KEY=GPU_NODE_SELECTOR_VALUE"
  require_digest_image
  ensure_invocation_marker
  validate_access
  validate_namespace_accommodation
  validate_api_and_queue
  validate_fixed_object_absent
  validate_server_side_admission
  validate_capacity
  echo "Read-only NCCL/RDMA preflight passed, including server-side admission dry-run. No cluster resources were persisted."
}

diagnostics() {
  local namespace
  local deadline=$((SECONDS + 60))
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  echo "=== MPIJob ===" >&2
  cleanup_kube "$deadline" get mpijob.kubeflow.org "$MPIJOB_NAME" -n "$namespace" -o yaml >&2 || true
  echo "=== Kueue Workloads ===" >&2
  cleanup_kube "$deadline" get workloads.kueue.x-k8s.io -n "$namespace" -o wide >&2 || true
  echo "=== Diagnostic pods ===" >&2
  cleanup_kube "$deadline" get pods -n "$namespace" -l "$DIAGNOSTIC_SELECTOR" -o wide >&2 || true
  echo "=== Namespace events ===" >&2
  cleanup_kube "$deadline" get events -n "$namespace" --sort-by=.lastTimestamp >&2 || true
}

cleanup_owned_invocation() {
  local namespace object ownership object_marker object_uid current_uid="" pods
  local deadline=$((SECONDS + CLEANUP_OVERALL_SECONDS))
  namespace="$(require_env E2E_STACK_NAMESPACE)"
  object="$(cleanup_kube "$deadline" get mpijob.kubeflow.org "$MPIJOB_NAME" -n "$namespace" --ignore-not-found -o json)" \
    || fail "cannot determine cleanup ownership for MPIJob $namespace/$MPIJOB_NAME"
  if [[ -n "$object" ]]; then
    ownership="$(python3 -c '
import json, sys
doc = json.load(sys.stdin)
print(doc.get("metadata", {}).get("labels", {}).get("e2e.taugrid.azure.com/invocation", ""))
print(doc.get("metadata", {}).get("uid", ""))
' <<<"$object")"
    object_marker="$(sed -n '1p' <<<"$ownership")"
    object_uid="$(sed -n '2p' <<<"$ownership")"
    if [[ "$object_marker" != "$NCCL_RDMA_INVOCATION" || -z "$object_uid" ]]; then
      fail "MPIJob $namespace/$MPIJOB_NAME is not owned by invocation $NCCL_RDMA_INVOCATION; refusing cleanup"
    fi

    run_owned_delete_bounded "$deadline" "$namespace" "$object_uid" \
      || fail "UID-precondition deletion failed for MPIJob $namespace/$MPIJOB_NAME UID $object_uid"

    while ((SECONDS < deadline)); do
      object="$(cleanup_kube "$deadline" get mpijob.kubeflow.org "$MPIJOB_NAME" -n "$namespace" --ignore-not-found -o json)" \
        || fail "cannot verify cleanup for MPIJob $namespace/$MPIJOB_NAME"
      if [[ -z "$object" ]]; then
        break
      fi
      current_uid="$(python3 -c 'import json, sys; print(json.load(sys.stdin).get("metadata", {}).get("uid", ""))' <<<"$object")"
      if [[ "$current_uid" != "$object_uid" ]]; then
        break
      fi
      sleep 1
    done
    [[ "$current_uid" != "$object_uid" || -z "$object" ]] \
      || fail "overall cleanup deadline expired for owned MPIJob $namespace/$MPIJOB_NAME UID $object_uid"
  fi

  while ((SECONDS < deadline)); do
    pods="$(cleanup_kube "$deadline" get pods -n "$namespace" -l "$DIAGNOSTIC_SELECTOR,$INVOCATION_LABEL=$NCCL_RDMA_INVOCATION" -o name)" \
      || fail "cannot verify pod cleanup for invocation $NCCL_RDMA_INVOCATION"
    [[ -z "$pods" ]] && return 0
    sleep 1
  done
  fail "overall cleanup deadline expired waiting for pods owned by invocation $NCCL_RDMA_INVOCATION"
}

cleanup_on_exit() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "${NCCL_RDMA_INVOCATION:-}" ]] && ! cleanup_owned_invocation; then
    status=1
  fi
  exit "$status"
}

run_diagnostic() {
  [[ "${NCCL_RDMA_CONFIRM:-}" == "$CONFIRMATION" ]] \
    || fail "set NCCL_RDMA_CONFIRM=$CONFIRMATION to authorize creating only $MPIJOB_NAME"
  ensure_invocation_marker
  preflight
  trap cleanup_on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  export AI_RUNTIME_E2E=1
  export E2E_GPU=1
  export E2E_NCCL_RDMA=1
  export E2E_STACK_USE_ARGOCD_QUEUE=1
  export E2E_STACK_QUEUE="$E2E_STACK_LARGE_GPU_QUEUE"
  export KUBECONFIG="$NCCL_RDMA_KUBECONFIG"
  export AI_RUNTIME_E2E_KUBE_CONTEXT="$NCCL_RDMA_KUBE_CONTEXT"

  if ! (
    cd "$E2E_ROOT"
    go test -count=1 -v -timeout 15m -run '^TestNCCLRDMA2x8H200$' ./stack/
  ); then
    diagnostics
    fail "NCCL/RDMA diagnostic failed; no retry was attempted"
  fi
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
