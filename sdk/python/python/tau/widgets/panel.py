# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""The ipywidgets panel for the notebook plugin.

TauGridPanel builds the widget; panel() is the convenience entry point a
platform-authored template cell calls. ipywidgets (and IPython) are imported
lazily so a plain import tau never requires them; on hosts without a widget
manager the panel degrades to a plain-HTML render() snapshot.

The panel supports two run flows:

* submit the current notebook as a Kueue-admitted RayJob, and
* load an existing RayJob from the cluster by name and check its status.

Both are read-only with respect to any run the panel did not create: load and
watch never mutate or delete cluster resources.
"""

from __future__ import annotations

import html
from pathlib import Path
from typing import Any, Optional

from tau._backend import KubernetesBackend
from tau._notebook_pkg import MAX_INPUT_BYTES, StagedPayload
from tau._notebook_submit import build_plan, submit_plan
from tau._render import Profile
from tau.widgets.embed import EmbedView, PortalProxy, experiment_path, iframe_html, ray_dashboard_path, run_view_path, same_origin
from tau.widgets.kube import ClusterClient
from tau.widgets.metrics import MetricSeries
from tau.widgets.render import diagnostics_html, loss_delta_text, status_header_html, svg_loss_chart
from tau.widgets.status import RunStatus, list_runs, read_run_status
from tau.widgets.watcher import DEFAULT_INTERVAL, StatusWatcher


class TauGridPanel:
    """ipywidgets layout shell; render() is UI-free and testable.

    client and opener are injected so the panel works and is testable without a
    cluster. The interactive widget tree is built lazily by build() only when a
    display is actually needed.
    """

    def __init__(
        self,
        *,
        namespace: str = "ray",
        run_name: str = "",
        portal_url: str = "https://portal.contoso.com",
        client: Optional[ClusterClient] = None,
    ) -> None:
        self.namespace = namespace
        self.run_name = run_name
        self.portal_url = portal_url.rstrip("/")
        self.client = client
        self.loss: MetricSeries = MetricSeries()
        self.staged: Optional[StagedPayload] = None
        self.manifest: Optional[Any] = None
        self.notebook_path: Optional[str] = None  # manual override wins (design 4.1)
        self.status: Optional[RunStatus] = None
        self.watcher: Optional[StatusWatcher] = None
        self.embed_view: Optional[EmbedView] = None
        self._proxy: Optional[PortalProxy] = None
        self._controls: Optional[Any] = None

    # -- notebook identity (design 4.1: manual override is authoritative) --
    def set_notebook_path(self, path: Optional[str]) -> None:
        """Set a manual notebook path override (visible and authoritative)."""
        self.notebook_path = path
        # A built tree must reflect the override immediately: the Submit button
        # enables once a notebook identity is known (S005).
        self._repaint_controls()

    # -- load an existing run from the cluster (read-only) --
    def load(self, name: str, namespace: Optional[str] = None, *, client: Optional[ClusterClient] = None) -> RunStatus:
        """Attach the panel to an existing RayJob and read its status.

        This is the "load job from the cluster" path: it performs a single
        read-only get and switches the panel into run view. It never creates,
        mutates, or deletes a resource.
        """
        if namespace:
            self.namespace = namespace
        if client is not None:
            self.client = client
        self.run_name = name
        self.status = self._read_status()
        self._repaint_controls()
        return self.status if self.status is not None else RunStatus(name=name, namespace=self.namespace)

    #: Alias that reads better at call sites that just want the current state.
    def check_status(self) -> Optional[RunStatus]:
        """Re-read the attached run's status and return the normalized result."""
        return self.refresh_status()

    def list_runs(self, *, label_selector: Optional[str] = None) -> list:
        """List RayJobs in the panel's namespace (for a run chooser)."""
        if self.client is None:
            return []
        return list_runs(self.client, namespace=self.namespace, label_selector=label_selector)

    # -- TensorBoard-style inline embed of the portal UI --
    def embed(
        self,
        *,
        target: str = "run",
        height: int = 640,
        portal_url: Optional[str] = None,
        page_origin: Optional[str] = None,
        workspace: Optional[str] = None,
    ) -> EmbedView:
        """Embed the TauGrid portal UI inline in the notebook cell.

        This legacy kernel-identity helper removes upstream frame restrictions
        through a separate loopback origin. It is not same-origin with Jupyter;
        use it only with a trusted local kernel and portal.

        target selects the portal view: "run" (run detail), "ray" (the run's Ray
        dashboard, needs a resolved ray_cluster_name), or "experiments" (the
        Stellar view for the run's job id). Returns an EmbedView whose HTML is a
        framed view plus a direct portal link fallback.
        """
        portal = (portal_url or self.portal_url).rstrip("/")
        path = self._embed_path(target, workspace=workspace)
        direct = portal + path

        # Direct frames require matching origins; otherwise use the opt-in proxy.
        use_proxy = not (page_origin and same_origin(page_origin, portal))
        if use_proxy:
            if self._proxy is None:
                self._proxy = PortalProxy(portal).start()
            url = self._proxy.url(path)
        else:
            url = direct

        view = EmbedView(
            url=url,
            path=path,
            portal_url=portal,
            html=iframe_html(url, height=height, link=direct),
            proxied=use_proxy,
        )
        self.embed_view = view
        return view

    def _embed_path(self, target: str, *, workspace: Optional[str] = None) -> str:
        if target == "run":
            if not self.run_name:
                raise ValueError("embed(target='run') needs a run name: call load() or pass run_name=")
            path = run_view_path(self.namespace, self.run_name)
        elif target == "ray":
            cluster = self.status.ray_cluster_name if self.status else None
            if not cluster:
                raise ValueError(
                    "embed(target='ray') needs a resolved Ray cluster: call load() first so the "
                    "RayJob status reports rayClusterName"
                )
            path = ray_dashboard_path(self.namespace, cluster)
        elif target == "experiments":
            run_id = self.status.job_id if self.status else None
            if not run_id:
                raise ValueError("embed(target='experiments') needs a job id: call load() first")
            path = experiment_path(run_id)
        else:
            raise ValueError(f"unknown embed target {target!r}; expected run, ray, or experiments")
        if workspace:
            joiner = "&" if "?" in path else "?"
            path = f"{path}{joiner}workspace={workspace}"
        return path

    def close(self) -> None:
        """Stop the embed proxy and the status watcher, if either is running."""
        self.stop_watching()
        if self._proxy is not None:
            self._proxy.stop()
            self._proxy = None

    # -- data (pure, offline-testable) --
    def refresh_status(self) -> Optional[RunStatus]:
        """Re-read status for the attached run; None when no client is set."""
        self.status = self._read_status()
        return self.status

    def _read_status(self) -> Optional[RunStatus]:
        if self.client is None:
            return None
        if not self.run_name:
            return None
        return read_run_status(self.client, namespace=self.namespace, name=self.run_name)

    def set_loss(self, series: MetricSeries) -> None:
        self.loss = series

    # -- watch (single-flight poll loop) --
    def watch(self, *, interval: float = DEFAULT_INTERVAL) -> Optional[StatusWatcher]:
        """Start polling the attached run's status until it is terminal.

        Returns the watcher, or None when there is no client/run to watch. The
        watcher is generation-tagged, so stop_watching() discards late replies.
        """
        if self.client is None or not self.run_name:
            return None
        self.stop_watching()
        self.watcher = StatusWatcher(self._read_status, on_update=self._on_watch_update, interval=interval)
        self.watcher.start()
        return self.watcher

    def stop_watching(self) -> None:
        """Stop polling. Never cancels or deletes the run."""
        if self.watcher is not None:
            self.watcher.stop()
            self.watcher = None

    def _on_watch_update(self, status: Optional[RunStatus]) -> None:
        if status is not None:
            self.status = status
        self._repaint_controls()

    # -- submit (design 5.2: the panel IS the submit path) --
    def submit(
        self,
        notebook: Optional[str] = None,
        *,
        notebook_bytes: Optional[bytes] = None,
        cluster: Optional[Any] = None,
        profile: Optional[Profile] = None,
        queue: Optional[str] = None,
        runtime_pip: Optional[list] = None,
        backend: Optional[KubernetesBackend] = None,
        staging_dir: Optional[Any] = None,
        env: Optional[dict] = None,
        env_secret: Optional[dict] = None,
        input_cap: int = MAX_INPUT_BYTES,
    ) -> Any:
        """Submit the current notebook as a Kueue-admitted RayJob.

        The full chain, each step injectable so tests run offline with fakes:

        1. package the notebook bytes into a self-contained staged payload
           (_notebook_pkg.package);
        2. resolve the worker profile and Kueue queue (_profile.resolve from the
           TauCluster CRD, or the explicit args);
        3. render the constrained ray.io/v1 RayJob (_render.render_rayjob):
           spec.suspend true, managedBy absent, queue label, control-only head,
           worker sizing from the profile;
        4. apply it through KubernetesBackend (no tau binary, no kubectl), then
           switch the panel into run view.

        Returns the SubmittedRun-shaped handle.
        """
        bytes_in = notebook_bytes
        if bytes_in is None:
            path = notebook or self.run_name or ""
            if not path:
                raise ValueError(
                    "no notebook to submit: resolve the current notebook from "
                    "the Jupyter session or pass notebook=/notebook_bytes="
                )
            with Path(path).open("rb") as source:
                bytes_in = source.read(input_cap + 1)

        plan = build_plan(
            client=self.client, notebook_bytes=bytes_in, namespace=self.namespace,
            name=self.run_name or "analysis", profile=profile, queue=queue, cluster=cluster,
            pip=runtime_pip, env=env, env_secret=env_secret, input_cap=input_cap,
            staging_dir=staging_dir or self._default_staging_dir(),
        )
        handle = submit_plan(client=self.client, plan=plan, backend=backend)
        self.run_name = plan.name
        self.staged = plan.staged
        self.manifest = plan.manifest
        self.status = RunStatus(
            existing=True,
            name=plan.name,
            namespace=self.namespace,
            state="queued",
            display_state="queued",
            queue=plan.queue,
        )
        self._repaint_controls()
        return handle

    def _default_staging_dir(self) -> Any:
        import tempfile

        return Path(tempfile.mkdtemp(prefix="tau-notebook-"))

    # -- output --
    def render(self) -> str:
        """A plain-HTML render of the panel (used for Save HTML export)."""
        parts = [
            "<div class=\"taugrid-panel\">",
            self._status_html(),
            f"<div class=\"tg-loss\">{loss_delta_text(self.loss)}</div>",
            svg_loss_chart(self.loss),
        ]
        if self.status is not None:
            parts.append(diagnostics_html(self.status))
        parts.append("</div>")
        return "".join(parts)

    def _repr_html_(self) -> str:
        """The notebook display convention: Jupyter renders this as real HTML.

        Returning the panel itself from %taugrid (rather than render()'s plain
        string) makes JupyterLab present the panel as text/html, so the browser
        renders the SVG and class hooks as DOM nodes.
        """
        return self.render()

    # -- interactive widget tree (the button in the notebook) --
    def build(self) -> Any:
        """Build the interactive panel: Submit/Load/Refresh buttons, status, chart.

        Requires ipywidgets (the tau[widgets] extra). The tree is constructible
        without a frontend, so its wiring is unit-testable offline; the widget
        manager only enters the picture on display.
        """
        import ipywidgets as widgets  # lazy: only when a real button is wanted

        if self.client is not None and self.run_name:
            self.refresh_status()

        submit_button = widgets.Button(
            description="Submit notebook",
            button_style="primary",
            tooltip="Package this notebook and submit it as a Kueue-admitted RayJob",
            disabled=self._submit_disabled(),
        )
        run_input = widgets.Text(
            value=self.run_name,
            description="Run name",
            placeholder="existing RayJob name",
        )
        load_button = widgets.Button(description="Load run", tooltip="Load an existing RayJob and check its status")
        refresh_button = widgets.Button(description="Refresh", tooltip="Re-read run status")
        status = widgets.HTML(self._status_html())
        chart = widgets.HTML(self.render())

        submit_button.on_click(self._on_submit_clicked)
        load_button.on_click(self._on_load_clicked)
        refresh_button.on_click(self._on_refresh_clicked)

        self._controls = {
            "submit": submit_button,
            "load": load_button,
            "run_input": run_input,
            "refresh": refresh_button,
            "status": status,
            "chart": chart,
        }
        return widgets.VBox(
            [
                status,
                widgets.HBox([submit_button, refresh_button]),
                widgets.HBox([run_input, load_button]),
                chart,
            ],
            layout=widgets.Layout(border="1px solid #e2e8f0", padding="8px"),
        )

    def _submit_disabled(self) -> bool:
        """The Submit button is disabled until a notebook path is known (S005)."""
        return not bool(self.notebook_path)

    def _status_html(self) -> str:
        """One-line state header: run identity plus run state (design 6.3)."""
        if self.status is not None and self.status.existing:
            return status_header_html(self.status)
        if not self.notebook_path:
            return "<div class=\"tg-status\">notebook path not resolved</div>"
        if not self.run_name:
            return f"<div class=\"tg-status\">not submitted - {html.escape(str(self.notebook_path))}</div>"
        state = self.status.display_state if self.status is not None else "unknown"
        return f"<div class=\"tg-status\">{html.escape(self.run_name)}: {html.escape(str(state))} - {html.escape(str(self.notebook_path))}</div>"

    def _repaint_controls(self) -> None:
        if self._controls is None:
            return
        self._controls["submit"].disabled = self._submit_disabled()
        self._controls["status"].value = self._status_html()
        self._controls["chart"].value = self.render()

    def _on_submit_clicked(self, _change: Any = None) -> Any:
        """The Submit button's action: run the full chain, then repaint status.

        The button's on_click registers this handler; tests call it directly to
        exercise the same path a user click drives.
        """
        if self._submit_disabled():
            raise ValueError(
                "no notebook to submit: resolve it from the Jupyter session or "
                "set a manual path with panel.set_notebook_path(...)"
            )
        handle = self.submit(notebook=self.notebook_path)
        # Re-submission is allowed, so the button re-enables; the status line
        # switches into run view (design 6.3).
        self._repaint_controls()
        return handle

    def _on_load_clicked(self, _change: Any = None) -> Optional[RunStatus]:
        """The Load button's action: attach to the named run and read status."""
        name = ""
        if self._controls is not None:
            name = str(self._controls["run_input"].value or "").strip()
        name = name or self.run_name
        if not name:
            raise ValueError("enter a RayJob name to load")
        return self.load(name)

    def _on_refresh_clicked(self, _change: Any = None) -> None:
        """The Refresh button's action: re-read status and repaint."""
        self.refresh_status()
        self._repaint_controls()

    def _ipython_display_(self) -> Any:
        """Prefer the interactive button tree; degrade to static HTML.

        IPython calls this for %taugrid; when a widget manager is present the
        real Submit button renders, and without one the static _repr_html_
        snapshot is used instead (design: degrade, don't fail).
        """
        try:
            tree = self.build()
        except ImportError as exc:
            raise NotImplementedError("ipywidgets not installed") from exc

        from IPython.display import display  # type: ignore[import-not-found]

        display(tree)
        return None


def panel(**kwargs: Any) -> Any:
    """Build and return a TauGridPanel.

    This is the one function a platform-authored template cell calls
    (import tau.widgets as tg; tg.panel()).
    """
    return TauGridPanel(**kwargs)


__all__ = ["TauGridPanel", "panel"]
