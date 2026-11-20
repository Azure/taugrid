# TauGrid 278 demo: success path

A laptop demo of the [design](../../capacity-provisioning-admission.md) success path (design §5.2). It runs real Kueue
with a real ProvisioningRequest AdmissionCheck on a kind cluster. A fake autoscaler and
KWOK stand in for AKS Cluster Autoscaler and GPU nodes.

The point of the demo: **the Job stays suspended until both GPU nodes exist, then both
pods start together.** Quota alone is not enough.

## What is real and what is simulated

| Piece | In the demo | In production |
|---|---|---|
| Kueue v0.19.2: Workload, quota, AdmissionCheck, ProvisioningRequest, admission | **Real** | Same |
| `ProvisioningRequest` CRD (`autoscaling.x-k8s.io/v1`) | **Real**, from cluster-autoscaler 1.35.2 | Installed by AKS |
| Cluster Autoscaler | `tau-demo fake-ca`: books the whole gang atomically, then scales idle nodes down | AKS Cluster Autoscaler |
| GPU nodes and pods | KWOK fake nodes (8 GPU each) and simulated containers | AKS node pool |
| `tau run` / `status` | `tau-demo submit` / `status` | `tau` CLI |

The node pool `gpupool` has min 0, max 2 nodes, 8 GPUs each, and boots a node in 15s.
The ClusterQueue has 16 GPUs of quota. A 2 × 8 GPU job fits both quota and the pool.

This demo does not cover timeout, cancel, quota-only admission, or the deadline
controller. Those are in the design; they are not needed to show the success path.

## Run it

Prerequisites: Docker, [kind](https://kind.sigs.k8s.io/), kubectl and Go 1.22 or later.

```bash
cd docs/design/examples/capacity-provisioning-demo
./setup.sh                     # kind cluster, PR CRD, Kueue, KWOK, demo queue, builds bin/tau-demo
```

If Kueue's webhook times out during setup (kube-proxy's iptables mode fails on some VMs), run
`./teardown.sh && KUBE_PROXY_MODE=nftables ./setup.sh`.

Use two terminals:

```bash
# terminal 1: simulated AKS Cluster Autoscaler
./bin/tau-demo fake-ca
# terminal 2
./scenario.sh                  # about 1.5 min; press enter between steps
```

`AUTO=1` skips the pauses. `TAU_DEMO_NO_CLEAR=1` stops the screen clearing.
`./bin/tau-demo overview --watch` in a spare terminal shows quota, Workloads,
ProvisioningRequests and nodes live.

To finish, run `./teardown.sh`.

## What you should see

1. Preflight: profile `gpu-a100-8` is ProvisioningReady, and 2 workers fit the pool max of 2.
2. Kueue reserves 16 GPUs, creates `job-train-…-taugrid-cas-gpu-1`, and keeps the Job suspended.
3. The fake CA scales the pool from 0 to 2 in one step (~15s boot) and sets `Provisioned=True`.
4. Kueue admits. Both pods start at the same moment on the new nodes.
5. `status --watch` walks Submitted → Quota reserved → Capacity provisioning → Pod scheduling → Running.
6. After the 20s run, Job TTL deletes the Job, Kueue removes the Workload and ProvisioningRequest, and the pool scales back to 0.

## Files

| Path | What |
|---|---|
| `setup.sh`, `teardown.sh` | Create or delete the kind cluster `tau278` |
| `scenario.sh` | Narrated success path |
| `sample-output.txt` | A captured run of `scenario.sh` and the fake CA log, for readers who don't run it |
| `manifests/kueue-objects.yaml` | Namespace, ResourceFlavor, ProvisioningRequestConfig, AdmissionCheck, ClusterQueue, LocalQueue, node pool config |
| `tau-demo/` | Go source: `submit.go`, `status.go`, `fakeca.go`, `kube.go` |
