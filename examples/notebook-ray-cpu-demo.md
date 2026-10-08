# CPU Ray demo for the TauGrid notebook plugin

Runs a CPU-only Ray job through the TauGrid JupyterLab plugin, so it needs no GPU
quota, and shows the plugin tracking it from admission to completion.

- Notebook: [notebook-ray-cpu-demo.ipynb](notebook-ray-cpu-demo.ipynb)
- Driver: `tools/run-cpu-ray-demo.py` (drives the same endpoints the panel uses)

## What the job does

Connects to the Ray cluster the RayJob starts, then runs eight `@ray.remote`
CPU tasks and prints the Ray version, node and CPU counts, GPU count, and the
result. It finishes with `CPU_DEMO_OK`.

## Prerequisites

1. **A notebook runtime image.** The platform's AI runtime image ships Ray but no
   notebook executor, and cluster pods cannot reach PyPI on a locked-down
   network, so build the executor in:

   ```bash
   python -m pip download --dest images/notebook-runtime/wheels \
       --platform manylinux2014_x86_64 --python-version 3.12 --implementation cp --abi cp312 \
       --only-binary=:all: "nbformat>=5.10" "nbconvert>=7.16" "ipykernel>=6.29" \
       "pexpect>4.6" "ptyprocess"
   docker build -t taugrid-notebook-runtime:local images/notebook-runtime
   ```

   The extra `pexpect`/`ptyprocess` downloads are required because pip
   evaluates `sys_platform` markers against the host, not the target platform.

2. **Point the server at it** and enable submission:

   ```bash
   TAUGRID_RUNTIME_IMAGE=taugrid-notebook-runtime:local \
   TAUGRID_SUBMISSION_ENABLED=1 \
   jupyter server --no-browser --port=8888 --ServerApp.token=<token>
   ```

3. **A CPU workload profile.** The shipped profiles all request GPUs, so add a
   CPU one to the TauCluster:

   ```bash
   kubectl patch cluster cluster --type=json -p '[{"op":"add","path":"/spec/workloadProfiles/-","value":{
     "name":"azure.research.cpu.small","description":"CPU-only Ray workers.",
     "mode":"fixed","workerCount":1,"gpusPerWorker":0,"defaultLocalQueue":"jobqueue",
     "executionTarget":"singleCluster","placement":"independent",
     "applicability":{"lanes":["training"],"teams":["research"],"namespaces":["tau-notebook-e2e"]},
     "priorities":{"podPriorityClassName":"taugrid-default","workloadPriorityClassName":"taugrid-default"}}}]'
   ```

4. **A LocalQueue in the run namespace**, and the namespace label the ClusterQueue
   selects on. Without both, the workload stays unadmitted:

   ```bash
   kubectl create namespace tau-notebook-e2e --dry-run=client -o yaml | kubectl apply -f -
   kubectl label ns tau-notebook-e2e tau.azure.com/workspace=tau-notebook-e2e --overwrite
   kubectl apply -f - <<'YAML'
   apiVersion: kueue.x-k8s.io/v1beta2
   kind: LocalQueue
   metadata:
     name: jobqueue
     namespace: tau-notebook-e2e
   spec:
     clusterQueue: jobqueue
   YAML
   ```

## Run it

```bash
python tools/run-cpu-ray-demo.py --token <token> --name cpu-demo --timeout 900
```

Expected: the plan resolves `azure.research.cpu.small` on queue `jobqueue`, then
`state=queued` -> `state=running` -> `state=complete` with `RESULT: SUCCEEDED`.

In the JupyterLab panel: open **TauGrid: Open runs**, set the namespace, press
**List runs**, and open the run. The detail tab shows Finished, Admitted, the
queue, and 2/2 pods; **Open logs** shows the bounded pod log snapshot.

## Notes

- The RayJob sets `ttlSecondsAfterFinished: 15`, so pods are removed 15 seconds
  after the job ends. Patch the RayJob to a larger TTL if you want to read the
  executed notebook or the Ray driver log afterwards:

  ```bash
  kubectl patch rayjob <name> -n <ns> --type=merge -p '{"spec":{"ttlSecondsAfterFinished":900}}'
  kubectl exec -n <ns> <head-pod> -c ray-head -- \
      cat /tmp/ray/session_latest/logs/job-driver-*.log
  kubectl exec -n <ns> <head-pod> -c ray-head -- python3 -c \
      "import json; nb=json.load(open('/data/analysis.executed.ipynb')); print(''.join(t for c in nb['cells'] for o in c.get('outputs',[]) for t in o.get('text',[])))"
  ```

- The entrypoint still runs a `pip install nbconvert ipykernel` preamble. With a
  runtime image that already contains them it is a no-op; on an image without
  them it fails, because pods have no PyPI access here.
