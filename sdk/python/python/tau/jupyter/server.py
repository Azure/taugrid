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
  coarse ``taugrid`` string, so per-namespace policy is expressible. The same is
  true of the companion files a notebook ships: every surface that lists or reads
  them authorizes ``taugrid:files:<directory>`` through :func:`files_resource`, so
  a per-directory policy is not expressible on one surface and bypassable on
  another, and the directory that is authorized is the one directory the reader
  consumes.
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
from pathlib import Path
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

#: Marker for a deliberately cluster-wide authorization decision. Distinct from
#: an explicit ``namespace=None`` so a caller that failed to resolve a namespace
#: cannot silently fall back to the coarse ``taugrid`` grant.
_COARSE = object()

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


def files_resource(root: Path, directory: Path) -> str:
    """The Authorizer namespace for one companion directory, for every surface.

    The directory that is read is part of *what* is being authorized, so it is
    named the same way a workload namespace is: ``files`` at the server root and
    ``files:<relative-directory>`` below it. ``FilesHandler`` lists those flat
    files and the preview/submit POST paths read them, so both must publish the
    one resource this function returns; deriving it in a single place is what
    stops a per-directory policy from being expressible on one surface and
    bypassable on the other. ``notebook_directory`` has already confined
    ``directory`` to ``root``, so it is safe to make it relative.
    """
    relative = directory.relative_to(root).as_posix()
    return "files" if relative == "." else f"files:{relative}"


class _ClientMixin:
    #: The coarse Jupyter Authorizer resource, used when no namespace is in play.
    AUTH_RESOURCE = "taugrid"

    def client(self) -> ClusterClient:
        return load_client(retries=0)

    @classmethod
    def auth_resource(cls, namespace: str) -> str:
        """The Authorizer resource for one namespaced decision.

        A namespace the caller names is part of *what* is being authorized, so it
        is reported to the deployment's Authorizer as ``taugrid:<namespace>``
        instead of being hidden behind the single coarse ``taugrid`` string. This
        makes a per-namespace policy expressible; it does not authenticate a
        caller's namespace membership by itself, which only the deployment's
        Authorizer configuration can supply.

        A missing namespace is *not* mapped to the coarse resource here. The
        coarse grant is a distinct, deliberately selected decision, so a caller
        that resolved no namespace must not silently fall back to it;
        ``authorize`` chooses the coarse resource explicitly and refuses an
        unresolved namespace.
        """
        if not namespace:
            raise ValueError("a namespaced authorization resource requires a namespace")
        return f"{cls.AUTH_RESOURCE}:{namespace}"

    async def authorize(self, action: str, *, namespace: object = _COARSE) -> None:
        """Require an explicit Jupyter Authorizer decision for ``action``.

        ``@web.authenticated`` proves *who* the caller is; it does not prove the
        caller may do anything. ``APIHandler.prepare()`` does not consult the
        Authorizer either, so without this call an authenticated user denied
        execution could still submit workloads under the server's Kubernetes
        identity and read cluster state through these handlers.

        Omit ``namespace`` for a deliberately cluster-wide decision
        (capabilities, namespace discovery, server files); the coarse ``taugrid``
        resource is authorized. Pass the namespace the operation acts on for a
        namespaced decision. ``namespace`` is a request value, so it is *not*
        trusted as authorization input on its own; the submit path additionally
        verifies it against the namespaces this server can list before a profile
        may scope on it.

        Passing ``namespace=None`` is a failure rather than a downgrade: ``None``
        means no namespace was resolved, so the coarse grant must not authorize
        the operation. That is what keeps a read or submit from reaching a
        namespace the request never established.

        The check fails closed: a missing Authorizer or an anonymous user is a
        403 rather than an implicit allow, and an Authorizer that returns a
        falsey awaitable is refused the same way a falsey bool is.
        """
        authorizer = getattr(self, "authorizer", None)
        user = getattr(self, "current_user", None)
        if authorizer is None or not user:
            raise web.HTTPError(403, "TauGrid API access requires an authenticated Jupyter user")
        if namespace is _COARSE:
            resource = self.AUTH_RESOURCE
        elif not namespace:
            raise web.HTTPError(
                403,
                f"not authorized to {action} TauGrid notebook resources: no namespace was resolved",
            )
        else:
            resource = self.auth_resource(str(namespace))
        allowed = authorizer.is_authorized(self, user, action, resource)
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

    async def authorize_companion_files(self, body: Mapping[str, Any]) -> Optional[Path]:
        """Resolve and authorize the companion directory this request will read.

        ``FilesHandler`` names the notebook directory ``files:<directory>``
        before listing it; the preview and submit POST paths read the same files,
        so they must make the same decision. Without it, a caller allowed to
        preview or submit into a namespace but denied a directory could name that
        directory's files in the request and have them packaged into the run
        payload.

        The request path is resolved exactly once, here, and the resolved
        directory is returned so the read consumes the very value that was
        authorized; re-resolving the path in the reader is what would let the two
        diverge. ``None`` means the request ships no companion files, so there is
        no directory to authorize.
        """
        if not (body.get("files") or []):
            return None
        path = body.get("path")
        if not isinstance(path, str) or not path.strip():
            raise web.HTTPError(400, "path is required to select files to ship with the notebook")
        root = server_root(self.settings.get("server_root_dir"))
        try:
            directory = notebook_directory(root, path)
        except FileSelectionError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc
        await self.authorize("read", namespace=files_resource(root, directory))
        return directory

    def selected_files(
        self, body: Mapping[str, Any], directory: Optional[Path] = None
    ) -> Dict[str, bytes]:
        """Read the selected files from the directory the handler authorized.

        ``directory`` is required whenever the request names files: re-resolving
        the request path here would be the second resolution that lets the
        authorized value and the read value diverge, so a missing directory fails
        closed instead of reading an unauthorized one.
        """
        names = body.get("files") or []
        if not names:
            return {}
        if directory is None:
            raise web.HTTPError(
                500, "companion directory was not authorized before reading notebook files"
            )
        try:
            return read_selected(directory, names)
        except FileSelectionError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc

    def build(
        self, body: Mapping[str, Any], files_directory: Optional[Path] = None
    ) -> SubmitPlan:
        notebook = body.get("notebook")
        if not isinstance(notebook, str) or not notebook.strip():
            raise web.HTTPError(
                400, "notebook is required: send the saved .ipynb content as a UTF-8 string"
            )
        extra_files = self.selected_files(body, files_directory)
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
        from tau.jupyter.destinations import list_destinations, resolve_destinations

        requested = self.get_argument("namespace", None)
        # Listing namespaces is cluster-wide discovery, so the coarse ``taugrid``
        # grant covers it. The namespace whose profiles and LocalQueues are read
        # is resolved exactly once, and the same discovery snapshot is handed to
        # the catalog, so the value authorized here and the value read there
        # cannot come from two independent cluster calls. A request that names no
        # namespace would otherwise reach a namespace's queues holding only the
        # coarse grant; a namespace discovery did not find is never read; and a
        # discovery that legitimately finds none leaves the catalog empty rather
        # than resolving to a namespace that was never authorized.
        await self.authorize("read")
        resolved, discovery = await self.read(resolve_destinations, namespace=requested)
        if resolved is not None:
            await self.authorize("read", namespace=resolved)
        scope = trusted_scope()
        self.set_header("Cache-Control", "no-store")
        self.finish(
            await self.read(
                list_destinations,
                namespace=resolved,
                discovery=discovery,
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
            directory = notebook_directory(root, path)
        except FileSelectionError as exc:
            raise web.HTTPError(exc.status, str(exc)) from exc
        # The directory that is read is part of *what* is being authorized, so it
        # is named in the Authorizer resource the same way a namespace is, via
        # ``files_resource``: ``taugrid:files:<relative-directory>``
        # (``taugrid:files`` at the server root). Without this, the constant
        # ``taugrid`` granted every directory under the root and per-directory
        # policy could not be expressed, so a request could name any directory
        # while holding only the coarse grant. The preview and submit POST paths
        # read these same files and publish the same resource through the same
        # helper, so one directory policy covers both surfaces.
        await self.authorize("read", namespace=files_resource(root, directory))
        try:
            # ``directory`` is the value just authorized, resolved exactly once by
            # ``notebook_directory``. Passing it (rather than ``root``/``path``)
            # keeps the listed directory the authorized one; a second resolution
            # inside ``list_candidates`` is what would let the two diverge.
            self.finish(list_candidates(directory, path))
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
        # The companion directory is also part of what is authorized: the plan
        # embeds the selected files, so a caller denied the directory must be
        # refused before the read, exactly as ``FilesHandler`` refuses the
        # listing. Resolving once and handing the value to ``build`` keeps the
        # authorized directory and the read directory the same value.
        files_directory = await self.authorize_companion_files(body)
        plan = await asyncio.to_thread(self.build, body, files_directory)
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
        # The submitted run payload embeds the selected files, so this path needs
        # the same companion-directory grant preview does; it is checked before
        # the release gate so a denial is never masked by configuration state,
        # and before ``build`` so no file is read without it.
        files_directory = await self.authorize_companion_files(body)
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
        plan = await asyncio.to_thread(self.build, body, files_directory)
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
