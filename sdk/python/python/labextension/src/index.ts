// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { ILayoutRestorer, JupyterFrontEnd, JupyterFrontEndPlugin } from '@jupyterlab/application';
import { Dialog, ICommandPalette, ReactWidget, showDialog, ToolbarButton, WidgetTracker } from '@jupyterlab/apputils';
import { ILauncher } from '@jupyterlab/launcher';
import { INotebookTracker, NotebookPanel } from '@jupyterlab/notebook';
import { RunTarget } from './model';
import { SubmissionConfirmation, SubmissionReview } from './submitview';
import { About, NotebookHandle, RunDetail, RunLogs, RunsSidebar, runWidgetId, SurfaceWidget } from './widget';

const plugin: JupyterFrontEndPlugin<void> = {
  id: 'taugrid:plugin',
  description: 'TauGrid runs and notebook submission',
  autoStart: true,
  optional: [ILayoutRestorer, ILauncher, ICommandPalette, INotebookTracker],
  activate: (app: JupyterFrontEnd, restorer: ILayoutRestorer | null, launcher: ILauncher | null, palette: ICommandPalette | null, notebooks: INotebookTracker | null): void => {
    const tracker = new WidgetTracker<SurfaceWidget>({ namespace: 'taugrid-runs' });
    const runTabs = new Map<string, SurfaceWidget>();
    const identities = new WeakMap<SurfaceWidget, RunTarget & { surface: string }>();
    const reviews = new WeakMap<NotebookPanel, SurfaceWidget>();
    const about = (): void => { void showDialog({ title: 'About TauGrid', body: ReactWidget.create(React.createElement(About)), buttons: [Dialog.okButton({ label: 'Close' })] }); };
    const openRun = async (target: RunTarget, surface = 'detail'): Promise<void> => {
      const id = runWidgetId(target, surface);
      let widget = runTabs.get(id);
      if (!widget || widget.isDisposed) {
        widget = new SurfaceWidget(surface === 'logs' ? React.createElement(RunLogs, { target }) : React.createElement(RunDetail, { target, onOpenLogs: identity => { void openRun(identity, 'logs'); } }));
        widget.id = id;
        widget.title.label = (surface === 'logs' ? 'Logs: ' : (target.kind || 'RayJob') + ': ') + target.name;
        widget.title.caption = target.namespace + '/' + target.kind + '/' + target.name;
        widget.title.closable = true;
        runTabs.set(id, widget);
        identities.set(widget, { ...target, surface });
        widget.disposed.connect(() => { runTabs.delete(id); });
      }
      if (!widget.isAttached) app.shell.add(widget, 'main');
      if (!tracker.has(widget)) await tracker.add(widget);
      if (!widget.isDisposed) app.shell.activateById(widget.id);
    };
    const sidebar = new SurfaceWidget(React.createElement(RunsSidebar, { onOpenRun: target => { void openRun(target); }, onAbout: about }));
    sidebar.id = 'taugrid-runs';
    sidebar.title.label = 'TauGrid';
    sidebar.title.caption = 'TauGrid runs';
    sidebar.title.closable = false;
    app.shell.add(sidebar, 'left', { rank: 700 });
    restorer?.add(sidebar, 'taugrid-runs');
    app.commands.addCommand('taugrid:open', {
      label: 'TauGrid: Open runs',
      execute: () => { app.shell.activateById(sidebar.id); }
    });
    app.commands.addCommand('taugrid:open-run', {
      label: 'TauGrid: Open run details',
      execute: args => {
        if (typeof args.namespace !== 'string' || !args.namespace.trim() || typeof args.name !== 'string' || !args.name.trim()) return;
        if (args.kind !== undefined && args.kind !== 'Job' && args.kind !== 'RayJob') return;
        return openRun({ namespace: args.namespace.trim(), name: args.name.trim(), kind: args.kind || 'RayJob' }, args.surface === 'logs' ? 'logs' : 'detail');
      }
    });
    if (restorer) void restorer.restore(tracker, {
      command: 'taugrid:open-run',
      args: widget => ({ ...identities.get(widget)! }),
      name: widget => widget.id
    });
    const openReview = (panel: NotebookPanel): void => {
      if (panel.isDisposed) return;
      let widget = reviews.get(panel);
      if (!widget || widget.isDisposed) {
        const path = panel.context.path;
        const notebook: NotebookHandle = {
          name: path,
          toJSON: () => {
            if (panel.isDisposed) throw new Error('The source notebook was closed. Reopen it and start a new submission review.');
            if (panel.context.path !== path) throw new Error('The source notebook was renamed. Close this review and submit again from the notebook toolbar.');
            return JSON.stringify(panel.context.model.toJSON());
          }
        };
        widget = new SurfaceWidget(React.createElement(SubmissionReview, {
          notebook,
          onAbout: about,
          onOpenRun: target => { void openRun(target); },
          onConfirm: async plan => {
          const result = await showDialog({ title: 'Submit notebook', body: ReactWidget.create(React.createElement(SubmissionConfirmation, { plan })), buttons: [Dialog.cancelButton(), Dialog.okButton({ label: 'Submit notebook' })] });
            return result.button.accept;
          }
        }));
        widget.id = 'taugrid-review-' + panel.id;
        widget.title.label = 'Submit: ' + path.split('/').pop();
        widget.title.caption = 'Review submission for ' + path;
        widget.title.closable = true;
        reviews.set(panel, widget);
      }
      if (!widget.isAttached) app.shell.add(widget, 'main');
      app.shell.activateById(widget.id);
    };
    const activeNotebook = (): NotebookPanel | null => {
      const panel = notebooks?.currentWidget;
      return panel && !panel.isDisposed && app.shell.currentWidget === panel ? panel : null;
    };
    app.commands.addCommand('taugrid:submit-notebook', {
      label: 'TauGrid: Submit current notebook',
      isEnabled: () => !!activeNotebook(),
      execute: () => { const panel = activeNotebook(); if (panel) openReview(panel); }
    });
    app.commands.addCommand('taugrid:about', { label: 'TauGrid: About', execute: about });
    const toolbarPanels = new WeakSet<NotebookPanel>();
    const addToolbar = (panel: NotebookPanel): void => {
      if (toolbarPanels.has(panel)) return;
      toolbarPanels.add(panel);
      panel.toolbar.addItem('taugrid-submit', new ToolbarButton({ label: 'Submit notebook', tooltip: 'Choose a TauGrid destination and review before submitting', onClick: () => openReview(panel) }));
    };
    notebooks?.forEach(addToolbar);
    notebooks?.widgetAdded.connect((sender, panel) => addToolbar(panel));
    app.shell.currentChanged?.connect(() => app.commands.notifyCommandChanged('taugrid:submit-notebook'));
    notebooks?.currentChanged.connect(() => app.commands.notifyCommandChanged('taugrid:submit-notebook'));
    for (const command of ['taugrid:open', 'taugrid:submit-notebook', 'taugrid:about']) palette?.addItem({ command, category: 'TauGrid' });
    launcher?.add({ command: 'taugrid:open', category: 'TauGrid', rank: 12 });
  }
};

export default plugin;
