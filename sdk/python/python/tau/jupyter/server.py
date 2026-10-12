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

Two boundaries are load-bearing:

* **Authorization.** ``@web.authenticated`` establishes identity only; it never
  consults the Jupyter ``Authorizer``, and ``APIHandler.prepare()`` does not
  either. Every handler calls :meth:`_ClientMixin.authorize` explicitly, so a
  restrictive Authorizer can deny cluster reads and submission even though the
  caller is authenticated. When a handler acts on a namespace, the Authorizer
  resource carries that namespace (``taugrid:<namespace>``) instead of the single
  coarse ``taugrid`` string, so per-namespace policy is expressible.
* **Caller scope.** The team/lane a submission is authorized against comes from
  this server's own configuration (``TAUGRID_TEAM``/``TAUGRID_LANE``), never from
  the request body and never from the profile being authorized. A notebook
  author cannot set the server process environment from a request, so the scope
  is a trusted, server-side value, and the same value drives both destination
  discovery and submission. When it is unset, team/lane-scoped profiles are
  withheld and refused instead of being authorized by their own declaration.
  The request namespace is not trusted on its own either: the submit path
  verifies it against the namespaces this server can actually list before any
  profile may scope on it, so a namespace-scoped profile cannot be authorized by
  a namespace the caller invented.
"""

from __future__ import annotations

import asyncio
import hmac
import inspect
import json
import os
from typing import Any, Dict, Mapping, Optional

from jupyter_server.base.handlers import APIHandler
from jupyter_server.utils import url_path_join
from tornado import web

from tau._notebook_pkg import MAX_INPUT_BYTES
from tau._render import GPU_EXECUTION_LOCAL, RUNTIME_IMAGE_ENV
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

#: Operator-set caller identity for team/lane-scoped profiles. These are read
#: from the server process environment, which a request cannot influence.
TEAM_ENV = "TAUGRID_TEAM"
LANE_ENV = "TAUGRID_LANE"


def _submission_enabled() -> bool:
    """Operator-controlled release gate; off unless explicitly turned on."""
    return os.environ.get("TAUGRID_SUBMISSION_ENABLED", "").strip().lower() in _TRUTHY


def trusted_scope() -> Dict[str, Optional[str]]:
    """The team/lane this Jupyter server is deployed for, from its own config.

    The values are read from the server process environment, set by the operator
    who starts the server. A request body cannot set them, so they are not
    attacker-controlled, and both discovery and submission use this same
    function so a profile can never be authorized by data that arrived with the
    submission.

    ``None`` means the server is not configured for that dimension, and
    ``ready_profiles`` then withholds profiles that scope on it. That is
    deliberate: a stock install with no configured team has an empty picker for
    team-scoped capacity rather than one that hands out another team's quota.
    """
    return {
        "team": os.environ.get(TEAM_ENV, "").strip() or None,
        "lane": os.environ.get(LANE_ENV, "").strip() or None,
    }


#: Evaluated at import so /capabilities and /submit agree for the process lifetime.
SUBMISSION_ENABLED = _submission_enabled()


def runtime_image_configured() -> bool:
    """Whether the operator selected a notebook runtime image explicitly."""
    return bool(os.environ.get(RUNTIME_IMAGE_ENV, "").strip())


def submission_config_error() -> Optional[str]:
    """The startup refusal when the release gate is on without a runtime image.

    The renderer's built-in default is the ray base image, which ships ray but
    not the notebook executor (nbformat/nbconvert/ipykernel). Enabling submission
    with no ``TAUGRID_RUNTIME_IMAGE`` therefore guarantees every run exits 3 with
    "nbconvert not installed" and makes ``/preview`` advertise an image that
    cannot execute notebooks. Refusing at startup is louder than a failed run.
    """
    if not SUBMISSION_ENABLED or runtime_image_configured():
        return None
    return (
        f"TAUGRID_SUBMISSION_ENABLED=1 requires {RUNTIME_IMAGE_ENV}: the renderer's built-in "
        "default is the ray base image, which does not ship the notebook executor "
        "(nbformat/nbconvert). Set TAUGRID_RUNTIME_IMAGE to a certified runtime such as the image "
        "built from images/notebook-runtime/Dockerfile, or leave submission disabled."
    )


def _status_dict(status: RunStatus) -> Dict[str, Any]:
    # facts is the shared status object runs.read_run attached; the list and log
    # surfaces read the same fields, so reason/explanation cannot drift.
    facts = getattr(status, "facts", None)
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
        "reason": status.reason,
        "explanation": facts.detail if facts else None,
        "suggestion": facts.suggestion if facts else None,
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
    #: The coarse Jupyter Authorizer resource, used when no namespace is in play.
    AUTH_RESOURCE = "taugrid"

    def client(self) -> ClusterClient:
        return load_client(retries=0)

    @classmethod
    def auth_resource(cls, namespace: Optional[str] = None) -> str:
        """The Authorizer resource for one decision.

        A namespace the caller names is part of *what* is being authorized, so it
        is reported to the deployment's Authorizer instead of being hidden behind
        the single coarse ``taugrid`` string. Handlers with no namespace
        (capabilities, cluster-wide namespace discovery, server files) keep the
        coarse resource. This makes a per-namespace policy expressible; it does
        not authenticate a caller's namespace membership by itself, which only the
        deployment's Authorizer configuration can supply.
        """
        return f"{cls.AUTH_RESOURCE}:{namespace}" if namespace else cls.AUTH_RESOURCE

    async def authorize(self, action: str, *, namespace: Optional[str] = None) -> None:
        """Require an explicit Jupyter Authorizer decision for ``action``.

        ``@web.authenticated`` proves *who* the caller is; it does not prove the
        caller may do anything. ``APIHandler.prepare()`` does not consult the
        Authorizer either, so without this call an authenticated user denied
        execution could still submit workloads under the server's Kubernetes
        identity and read cluster state through these handlers.

        ``namespace`` names the namespace the decision covers, when there is one.
        It is a request value, so it is *not* trusted as authorization input on
        its own; the submit path additionally verifies it against the namespaces
        this server can list before a profile may scope on it.

        The check fails closed: a missing Authorizer or an anonymous user is a
        403 rather than an implicit allow, and an Authorizer that returns a
        falsey awaitable is refused the same way a falsey bool is.
        """
        authorizer = getattr(self, "authorizer", None)
        user = getattr(self, "current_user", None)
        if authorizer is None or not user:
            raise web.HTTPError(403, "TauGrid API access requires an authenticated Jupyter user")
        allowed = authorizer.is_authorized(self, user, action, self.auth_resource(namespace))
        if inspect.isawaitable(allowed):
            allowed = await allowed
        if not allowed:
            raise web.HTTPError(
                403, f"not authorized to {action} TauGrid notebook resources"
            )

    async def read(self, reader, **kwargs):
        try:
            return await asyncio.to_thread(_call_with_client, self, reader, **kwargs)
        except runs.ReadError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc

    def target(self):
        return {"namespace": self.get_argument("namespace", "default"), "name": self.get_argument("name", ""), "kind": self.get_argument("kind", "RayJob")}


def _build_checked_plan(client, *, namespace, **kwargs):
    """Build a plan only for a namespace this server can actually see.

    The namespace is an authorization input for a namespace-scoped profile, so
    the request value may not authorize itself: it must be one the server's own
    Kubernetes identity can list. A profile whose applicability names a namespace
    is therefore withheld unless that namespace is really reachable, and the
    check fails closed when the namespace list cannot be read. This is the same
    listing that drives destination discovery, so a namespace offered by the
    picker is exactly a namespace a submission may target.
    """
    visible = {
        row.get("name")
        for row in (runs.list_namespaces(client).get("namespaces") or [])
        if isinstance(row, Mapping) and row.get("name")
    }
    if namespace not in visible:
        reason = (
            "it is not in the namespaces this server can see"
            if visible
            else "the server could not list namespaces"
        )
        raise SubmitError(
            f"namespace {namespace!r} cannot be authorized: {reason}. A namespace-scoped profile "
            "is only usable in a namespace this server can read; refresh destinations or ask the "
            "operator for namespace access.",
            status=403,
        )
    return build_plan(client=client, namespace=namespace, **kwargs)


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
        for key in ("namespace", "name", "profile", "queue", "gpuExecution"):
            if key in body and body[key] is not None and not isinstance(body[key], str):
                raise web.HTTPError(400, f"{key} must be a string")
        # team/lane decide which reserved capacity a profile authorizes, so they
        # come from this server's configuration (trusted_scope), never from the
        # request. Accepting them here would let a notebook author name another
        # team's scope, which is exactly the boundary this refuses.
        for key in ("team", "lane"):
            if body.get(key) is not None:
                env_name = TEAM_ENV if key == "team" else LANE_ENV
                raise web.HTTPError(
                    400,
                    f"{key} cannot be set by a request: this server's {key} identity is "
                    f"operator configuration ({env_name})",
                )
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
        scope = trusted_scope()
        namespace = str(body.get("namespace") or "default")
        try:
            return _call_with_client(
                self, _build_checked_plan,
                namespace=namespace,
                notebook_bytes=notebook.encode("utf-8"),
                name=body.get("name"),
                profile=body.get("profile"),
                queue=body.get("queue"),
                pip=body.get("pip"),
                extra_files=extra_files,
                env=body.get("env"),
                env_secret=body.get("env_secret"),
                gpu_execution=body.get("gpuExecution") or GPU_EXECUTION_LOCAL,
                # The same trusted scope drives discovery (destinations) and
                # submission, so a profile cannot be found in one scope and
                # authorized in another.
                team=scope["team"],
                lane=scope["lane"],
            )
        except SubmitError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc
        except web.HTTPError:
            raise
        except Exception as exc:
            raise web.HTTPError(500, f"could not build the submission plan: {exc}") from exc


class CapabilitiesHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        await self.authorize("read")
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
        from tau.jupyter.destinations import list_destinations, resolve_destination_namespace

        requested = self.get_argument("namespace", None)
        # Listing namespaces is cluster-wide discovery, so the coarse ``taugrid``
        # grant covers it. When the request omits ``namespace``, discovery then
        # reads one resolved namespace's profiles and LocalQueues; that namespace
        # is not known until it is resolved, so it is authorized as
        # ``taugrid:<resolved>`` before those reads happen. Otherwise a request
        # that names no namespace would reach a namespace's queues holding only
        # the coarse grant.
        await self.authorize("read")
        resolved = await self.read(resolve_destination_namespace, namespace=requested)
        await self.authorize("read", namespace=resolved)
        scope = trusted_scope()
        self.set_header("Cache-Control", "no-store")
        self.finish(
            await self.read(
                list_destinations,
                namespace=resolved,
                team=scope["team"],
                lane=scope["lane"],
            )
        )


class StatusHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        from tau.jupyter.metrics import collector

        target = self.target()
        await self.authorize("read", namespace=target["namespace"])
        include_metrics = self.get_argument("includeMetrics", "false")
        if include_metrics not in ("true", "false"):
            raise web.HTTPError(400, "includeMetrics must be true or false")

        def read(client, **target):
            status = runs.read_run(client, **target)
            result = _status_dict(status)
            if include_metrics == "true":
                result["metrics"] = collector.collect(client, status, force=status.terminal)
            return result

        self.finish(await self.read(read, **target))


class RunsHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        namespace = self.get_argument("namespace", "default")
        await self.authorize("read", namespace=namespace)
        self.finish(await self.read(runs.list_runs, namespace=namespace, queue=self.get_argument("queue", "")))


class FilesHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        await self.authorize("read")
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
        await self.authorize("read")
        self.finish(await self.read(lambda client: list_namespaces(client)))


class LogsHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        target = self.target()
        await self.authorize("read", namespace=target["namespace"])
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
        self.finish(await self.read(runs.read_logs, **target, pod=self.get_argument("pod", ""), container=self.get_argument("container", ""), tail=tail, **options))


class PreviewHandler(APIHandler, _SubmitMixin):
    @web.authenticated
    async def post(self) -> None:
        # The request namespace is part of what is authorized, so it is read
        # before the decision. Parsing the bounded body is not cluster access.
        body = self.json_body()
        await self.authorize("read", namespace=str(body.get("namespace") or "default"))
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
        body = self.json_body()
        await self.authorize("execute", namespace=str(body.get("namespace") or "default"))
        if not SUBMISSION_ENABLED:
            raise web.HTTPError(
                409,
                "TauGrid notebook submission is disabled on this server: an operator must set "
                "TAUGRID_SUBMISSION_ENABLED=1 after a certified notebook runtime image record "
                "exists. POST /taugrid/api/preview resolves the plan without creating anything.",
            )
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
    problem = submission_config_error()
    if problem is not None:
        raise RuntimeError(problem)
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
