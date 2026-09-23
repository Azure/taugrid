// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { LossMetrics } from './model';

export function LossSection({ metrics, stale = false }: { metrics?: LossMetrics; stale?: boolean }): JSX.Element {
  const id = React.useId();
  const samples = metrics?.samples || [];
  const minimum = samples.length ? Math.min(...samples.map(sample => sample.value)) : 0;
  const maximum = samples.length ? Math.max(...samples.map(sample => sample.value)) : 0;
  const scale = Math.max(Math.abs(minimum), Math.abs(maximum), 1);
  const span = maximum / scale - minimum / scale;
  const first = samples[0]?.step || 0;
  const last = samples[samples.length - 1]?.step || first;
  const points = samples.map(sample => ({
    x: last === first ? 334 : 60 + 548 * ((sample.step - first) / (last - first)),
    y: span === 0 ? 126 : 222 - 192 * ((sample.value / scale - minimum / scale) / span),
    ...sample
  }));
  return <section className="taugrid-metrics" data-testid="taugrid-metrics" aria-label="Loss">
    <h3>Loss</h3>
    <p data-testid="taugrid-metrics-source">{metrics?.source ? Object.entries(metrics.source).map(([key, value]) => `${key}: ${value}`).join(' / ') : 'No loss source available.'}</p>
    <p data-testid="taugrid-metrics-freshness">{stale || metrics?.stale ? 'Stale evidence. ' : ''}{metrics?.checkedAt ? `Source checked ${metrics.checkedAt}. ` : 'Source not checked. '}{samples.length} samples{samples.length ? `; steps ${first} to ${last}` : ''}.</p>
    {metrics?.possiblyTruncated && <p data-testid="taugrid-metrics-truncation">Partial window, not complete training history. {metrics.truncationReasons.join(', ')}. Limit: {metrics.limitBytes} bytes / {metrics.maxPoints} points.</p>}
    {metrics?.coverage && <p>Source coverage: {JSON.stringify(metrics.coverage)}</p>}
    {metrics?.message && <p data-testid={metrics.state === 'error' ? 'taugrid-metrics-error' : undefined} role={metrics.state === 'error' ? 'alert' : undefined}>{metrics.message}</p>}
    {samples.length ? <>
      <svg className="taugrid-loss-curve" data-testid="taugrid-loss-curve" viewBox="0 0 640 280" role="img" aria-labelledby={`${id}-title ${id}-description`}>
        <title id={`${id}-title`}>Observed loss by step</title>
        <desc id={`${id}-description`}>{samples.length} observed samples, steps {first} through {last}; loss {minimum} to {maximum}. Straight lines connect observations, not interpolated samples. Exact values are in the sample table.</desc>
        <path d="M60 30 V222 H608" fill="none" stroke="currentColor" />
        {points.length > 1 && <polyline points={points.map(point => `${point.x},${point.y}`).join(' ')} fill="none" stroke="currentColor" strokeWidth="2" />}
        {points.map(point => <circle key={point.step} cx={point.x} cy={point.y} r="3" fill="currentColor" />)}
        <text x="334" y="268" textAnchor="middle">Step</text>
        <text x="16" y="126" transform="rotate(-90 16 126)" textAnchor="middle">Loss</text>
        <text x="60" y="242">{first}</text><text x="608" y="242" textAnchor="end">{last}</text>
      </svg>
      <p>Loss range: {minimum} to {maximum}.</p>
      <details data-testid="taugrid-loss-samples"><summary>Show observed loss samples ({samples.length})</summary>
        <div className="taugrid-loss-table"><table><caption>Observed loss samples</caption><thead><tr><th scope="col">Step</th><th scope="col">Loss</th></tr></thead><tbody>
          {samples.map(sample => <tr key={sample.step}><td>{sample.step}</td><td>{sample.value}</td></tr>)}
        </tbody></table></div>
      </details>
    </> : <p data-testid="taugrid-metrics-empty">No finite loss samples available. No curve is inferred.</p>}
  </section>;
}
