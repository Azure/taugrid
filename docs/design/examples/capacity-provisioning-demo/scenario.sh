#!/usr/bin/env bash
# Success path for TauGrid 278: a 2-worker GPU job is admitted only after the
# autoscaler books both nodes. Needs the fake autoscaler in another terminal:
#   ./bin/tau-demo fake-ca
#
# Usage: ./scenario.sh
#   AUTO=1              don't pause between steps
#   TAU_DEMO_NO_CLEAR=1 don't clear the screen on refresh (for logs)
set -euo pipefail
cd "$(dirname "$0")"

TAU=./bin/tau-demo
AUTO=${AUTO:-0}

bold=$'\033[1m'; dim=$'\033[2m'; reset=$'\033[0m'

say() { printf '\n%s== %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s   %s%s\n' "$dim" "$*" "$reset"; }
pause() {
  if [[ "$AUTO" == "1" ]]; then return; fi
  read -r -p "   [enter] " _ </dev/tty || true
}
run() {
  printf '%s$ %s%s\n' "$bold" "$*" "$reset"
  "$@"
}

if ! pgrep -f "tau-demo fake-ca" >/dev/null; then
  echo "start the fake autoscaler first:"
  echo "  ./bin/tau-demo fake-ca"
  exit 1
fi

say "Success: capacity-gated admission (profile gpu-a100-8)"
note "ClusterQueue gpu-a100 has the ProvisioningRequest AdmissionCheck on flavor a100-scale."
note "Pool gpupool: min 0, max 2 nodes x 8 GPU. We ask for 2 workers x 8 GPU."
pause
run $TAU submit --name train --workers 2 --run 20s
note "Kueue reserves quota, creates a ProvisioningRequest, and keeps the Job suspended until"
note "the autoscaler books the whole gang (atomic scale-up). Then both pods start together."
pause
run $TAU status train --watch
run $TAU overview
note "After the run, Job TTL removes it and the autoscaler scales the empty nodes down."
pause

note "waiting for the cluster to drain (no Jobs, Workloads, ProvisioningRequests or pool nodes)"
for i in $(seq 1 90); do
  jobs=$(kubectl get jobs -n team-a --no-headers 2>/dev/null | wc -l)
  wls=$(kubectl get workloads.kueue.x-k8s.io -n team-a --no-headers 2>/dev/null | wc -l)
  prs=$(kubectl get provisioningrequests.autoscaling.x-k8s.io -n team-a --no-headers 2>/dev/null | wc -l)
  nodes=$(kubectl get nodes -l agentpool=gpupool --no-headers 2>/dev/null | wc -l)
  if ((jobs + wls + prs + nodes == 0)); then
    note "quiet: pool back at 0, quota released"
    $TAU overview
    exit 0
  fi
  sleep 2
done
note "still not quiet after 3 minutes; check the fake-ca logs"
exit 1
