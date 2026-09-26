# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Jupyter Server extension: the TauGrid REST API for the JupyterLab panel.

The frontend never talks to Kubernetes directly. It calls these endpoints, which
reuse the Python SDK (payload, renderer, submit orchestration, status reader)
with the server process identity.

Submission is behind a release gate. When the gate is off, POST /submit answers
409 and creates nothing; POST /preview still resolves a full plan offline so the
UI can show exactly what would be created. The gate is off by default and is
turned on by an operator (TAUGRID_SUBMISSION_ENABLED=1) once a certified
notebook runtime image record exists.
"""

from __future__ import annotations

import asyncio
import hmac
import json
import os
from typing import Any, Dict, Mapping

from jupyter_server.base.handlers import APIHandler
from jupyter_server.utils import url_path_join
from tornado import web

from tau._notebook_pkg import MAX_INPUT_BYTES
from tau.jupyter.notebook_files import (
    MAX_CANDIDATES,
    FileSelectionError,
    list_candidates,
    notebook_directory,
    read_selected,
    server_root,
)
from tau.jupyter.runs import list_namespaces
from tau.jupyter.submit import SubmitError, SubmitPlan, build_plan, submit_plan
from tau.widgets.kube import ClusterClient, load_client
from tau.widgets.status import RunStatus
from tau.jupyter import runs

_TRUTHY = {"1", "true", "yes", "on"}


def _submission_enabled() -> bool:
    """Operator-controlled release gate; off unless explicitly turned on."""
    return os.environ.get("TAUGRID_SUBMISSION_ENABLED", "").strip().lower() in _TRUTHY


#: Evaluated at import so /capabilities and /submit agree for the process lifetime.
SUBMISSION_ENABLED = _submission_enabled()


def _status_dict(status: RunStatus) -> Dict[str, Any]:
    return {
        "existing": status.existing,
        "name": status.name,
        "namespace": status.namespace,
        "state": status.state,
        "displayState": status.display_state,
        "rayClusterName": status.ray_cluster_name,
        "jobId": status.job_id,
        "deploymentStatus": status.deployment_status,
        "queue": status.queue,
        "admitted": status.admitted,
        "message": status.message,
        "readyPods": status.ready_pods,
        "totalPods": status.total_pods,
        "terminal": status.terminal,
        "kind": getattr(status, "kind", "RayJob"),
        "uid": getattr(status, "uid", None),
        "phases": getattr(status, "phases", []),
        "output": getattr(status, "output", {}),
        "pods": [
            {
                "name": pod.name,
                "phase": pod.phase,
                "node": pod.node,
                "ready": pod.ready,
                "restarts": pod.restarts,
                "rayNodeType": pod.ray_node_type,
                "containers": getattr(status, "containers", {}).get(pod.name, []),
                "role": getattr(status, "pod_roles", {}).get(pod.name, pod.ray_node_type),
                "uid": getattr(status, "pod_uids", {}).get(pod.name),
            }
            for pod in status.pods
        ],
        "diagnostics": [
            {
                "code": d.code,
                "severity": d.severity,
                "message": d.message,
                "suggestion": d.suggestion,
            }
            for d in status.diagnostics
        ],
    }


def _call_with_client(handler, reader, **kwargs):
    client = handler.client()
    try:
        return reader(client=client, **kwargs)
    finally:
        close = getattr(client, "close", None)
        if close:
            close()


class _ClientMixin:
    def client(self) -> ClusterClient:
        return load_client(retries=0)

    async def read(self, reader, **kwargs):
        try:
            return await asyncio.to_thread(_call_with_client, self, reader, **kwargs)
        except runs.ReadError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc

    def target(self):
        return {"namespace": self.get_argument("namespace", "default"), "name": self.get_argument("name", ""), "kind": self.get_argument("kind", "RayJob")}


class _SubmitMixin(_ClientMixin):
    """Shared request parsing and plan building for preview and submit."""

    def json_body(self) -> Mapping[str, Any]:
        raw = self.request.body or b"{}"
        if len(raw) > MAX_INPUT_BYTES * 8:
            raise web.HTTPError(413, "submission request exceeds the input budget")
        try:
            body = json.loads(raw.decode("utf-8"))
        except Exception as exc:
            raise web.HTTPError(400, f"invalid JSON body: {exc}") from exc
        if not isinstance(body, dict):
            raise web.HTTPError(400, "request body must be a JSON object")
        for key in ("namespace", "name", "profile", "queue"):
            if key in body and body[key] is not None and not isinstance(body[key], str):
                raise web.HTTPError(400, f"{key} must be a string")
        pip = body.get("pip")
        if pip is not None and (not isinstance(pip, list) or not all(isinstance(item, str) for item in pip)):
            raise web.HTTPError(400, "pip must be an array of strings")
        for key in ("env", "env_secret"):
            value = body.get(key)
            if value is not None and (not isinstance(value, dict) or not all(isinstance(item, str) for item in value.values())):
                raise web.HTTPError(400, f"{key} must map strings to strings")
        files = body.get("files")
        if files is not None and (not isinstance(files, list) or not all(isinstance(item, str) for item in files)):
            raise web.HTTPError(400, "files must be an array of file names")
        if len(files or []) > MAX_CANDIDATES:
            raise web.HTTPError(413, f"at most {MAX_CANDIDATES} files may ship with a notebook")
        if body.get("path") is not None and not isinstance(body["path"], str):
            raise web.HTTPError(400, "path must be a string")
        return body

    def selected_files(self, body: Mapping[str, Any]) -> Dict[str, bytes]:
        """Read the files the review asked to ship, from the notebook's directory."""
        names = body.get("files") or []
        if not names:
            return {}
        path = body.get("path")
        if not isinstance(path, str) or not path.strip():
            raise web.HTTPError(400, "path is required to select files to ship with the notebook")
        root = server_root(self.settings.get("server_root_dir"))
        try:
            return read_selected(notebook_directory(root, path), names)
        except FileSelectionError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc

    def build(self, body: Mapping[str, Any]) -> SubmitPlan:
        notebook = body.get("notebook")
        if not isinstance(notebook, str) or not notebook.strip():
            raise web.HTTPError(
                400, "notebook is required: send the saved .ipynb content as a UTF-8 string"
            )
        extra_files = self.selected_files(body)
        try:
            return _call_with_client(
                self, build_plan,
                notebook_bytes=notebook.encode("utf-8"),
                namespace=str(body.get("namespace") or "default"),
                name=body.get("name"),
                profile=body.get("profile"),
                queue=body.get("queue"),
                pip=body.get("pip"),
                extra_files=extra_files,
                env=body.get("env"),
                env_secret=body.get("env_secret"),
            )
        except SubmitError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc
        except web.HTTPError:
            raise
        except Exception as exc:
            raise web.HTTPError(500, f"could not build the submission plan: {exc}") from exc


class CapabilitiesHandler(APIHandler, _ClientMixin):
    @web.authenticated
    def get(self) -> None:
        self.finish(
            {
                "submissionEnabled": SUBMISSION_ENABLED,
                "submissionImplemented": True,
                "portalUrl": runs.portal_url(),
            }
        )


class DestinationsHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        from tau.jupyter.destinations import list_destinations

        self.set_header("Cache-Control", "no-store")
        self.finish(await self.read(list_destinations, namespace=self.get_argument("namespace", None)))


class StatusHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        from tau.jupyter.metrics import collector

        include_metrics = self.get_argument("includeMetrics", "false")
        if include_metrics not in ("true", "false"):
            raise web.HTTPError(400, "includeMetrics must be true or false")

        def read(client, **target):
            status = runs.read_run(client, **target)
            result = _status_dict(status)
            if include_metrics == "true":
                result["metrics"] = collector.collect(client, status, force=status.terminal)
            return result

        self.finish(await self.read(read, **self.target()))


class RunsHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        namespace = self.get_argument("namespace", "default")
        self.finish(await self.read(runs.list_runs, namespace=namespace, queue=self.get_argument("queue", "")))


class FilesHandler(APIHandler, _ClientMixin):
    @web.authenticated
    def get(self) -> None:
        path = self.get_argument("path", "")
        if not path:
            raise web.HTTPError(400, "path is required")
        root = server_root(self.settings.get("server_root_dir"))
        try:
            self.finish(list_candidates(root, path))
        except FileSelectionError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc


class NamespacesHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        self.finish(await self.read(lambda client: list_namespaces(client)))


class LogsHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        try:
            tail = int(self.get_argument("tail", "200"))
        except ValueError as exc:
            raise web.HTTPError(400, "tail must be an integer") from exc
        options = {}
        for key in ("previous", "timestamps"):
            value = self.get_argument(key, "false")
            if value not in ("true", "false"):
                raise web.HTTPError(400, f"{key} must be true or false")
            options[key] = value == "true"
        self.set_header("Cache-Control", "no-store")
        self.finish(await self.read(runs.read_logs, **self.target(), pod=self.get_argument("pod", ""), container=self.get_argument("container", ""), tail=tail, **options))


class PreviewHandler(APIHandler, _SubmitMixin):
    @web.authenticated
    async def post(self) -> None:
        body = self.json_body()
        plan = await asyncio.to_thread(self.build, body)
        self.finish(
            {
                "submittable": True,
                "submissionEnabled": SUBMISSION_ENABLED,
                "plan": plan.summary(),
            }
        )


class SubmitHandler(APIHandler, _SubmitMixin):
    @web.authenticated
    async def post(self) -> None:
        if not SUBMISSION_ENABLED:
            raise web.HTTPError(
                409,
                "TauGrid notebook submission is disabled on this server: an operator must set "
                "TAUGRID_SUBMISSION_ENABLED=1 after a certified notebook runtime image record "
                "exists. POST /taugrid/api/preview resolves the plan without creating anything.",
            )
        body = self.json_body()
        if body.get("confirm") is not True:
            raise web.HTTPError(
                400, "confirmation required: resend with confirm=true after reviewing the plan"
            )
        digest = body.get("planDigest")
        if not isinstance(digest, str) or len(digest) != 64:
            raise web.HTTPError(400, "planDigest from preview is required")
        plan = await asyncio.to_thread(self.build, body)
        if not hmac.compare_digest(digest, plan.summary()["planDigest"]):
            raise web.HTTPError(409, "Submission plan changed; review the notebook and plan again")
        try:
            result = await asyncio.to_thread(_call_with_client, self, submit_plan, plan=plan)
        except SubmitError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc
        self.finish(
            {
                "submitted": True,
                **result.as_dict(),
                "plan": plan.summary(),
            }
        )


def _handlers(base: str) -> list:
    return [
        (url_path_join(base, "capabilities"), CapabilitiesHandler),
        (url_path_join(base, "status"), StatusHandler),
        (url_path_join(base, "runs"), RunsHandler),
        (url_path_join(base, "namespaces"), NamespacesHandler),
        (url_path_join(base, "destinations"), DestinationsHandler),
        (url_path_join(base, "files"), FilesHandler),
        (url_path_join(base, "logs"), LogsHandler),
        (url_path_join(base, "preview"), PreviewHandler),
        (url_path_join(base, "submit"), SubmitHandler),
    ]


def _load_jupyter_server_extension(server_app: Any) -> None:
    """Register the TauGrid handlers under /taugrid/api."""
    web_app = server_app.web_app
    host_pattern = ".*$"
    base = url_path_join(web_app.settings["base_url"], "taugrid", "api")
    web_app.add_handlers(host_pattern, _handlers(base))
    server_app.log.info(
        "TauGrid Jupyter server extension loaded at %s (submission enabled: %s)",
        base,
        SUBMISSION_ENABLED,
    )


__all__ = ["_load_jupyter_server_extension", "SUBMISSION_ENABLED"]
