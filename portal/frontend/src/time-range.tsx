// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useEffect, useMemo, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';

type Timezone = 'local' | 'utc';

const presets = [
  ['15m', 'Last 15 minutes'],
  ['1h', 'Last hour'],
  ['24h', 'Last 24 hours'],
  ['168h', 'Last 7 days'],
  ['720h', 'Last 30 days'],
] as const;

function inputValue(date: Date, timezone: Timezone) {
  if (timezone === 'utc') return date.toISOString().slice(0, 16);
  const offset = date.getTimezoneOffset() * 60_000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 16);
}

function parseInput(value: string, timezone: Timezone) {
  return new Date(value + (timezone === 'utc' ? 'Z' : ''));
}

function initialInput(value: string, fallback: Date, timezone: Timezone) {
  const date = new Date(value);
  return inputValue(Number.isFinite(date.getTime()) ? date : fallback, timezone);
}

function timestampLabel(value: string, timezone: Timezone) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return '';
  const rendered = date.toLocaleString(undefined, timezone === 'utc' ? { timeZone: 'UTC' } : undefined);
  return rendered + (timezone === 'utc' ? ' UTC' : '');
}

export function useHistoricalRange(defaultWindow: string) {
  const location = useLocation();
  const params = useMemo(() => new URLSearchParams(location.search), [location.search]);
  const custom = params.has('start') || params.has('end');
  const requested = params.get('window') || defaultWindow;
  const window = presets.some(([value]) => value === requested) ? requested : defaultWindow;
  const start = params.get('start') || '';
  const end = params.get('end') || '';
  const timezone: Timezone = params.get('tz') === 'utc' ? 'utc' : 'local';
  const api = custom
    ? new URLSearchParams({ start, end }).toString()
    : new URLSearchParams({ window }).toString();
  const startLabel = timestampLabel(start, timezone);
  const endLabel = timestampLabel(end, timezone);
  const label = custom
    ? startLabel && endLabel ? `${startLabel} to ${endLabel}` : 'Incomplete or invalid custom range'
    : presets.find(([value]) => value === window)?.[1] || window;
  return { api, custom, window, start, end, timezone, label };
}

export function TimeRangeControls({ defaultWindow }: { defaultWindow: string }) {
  const location = useLocation();
  const navigate = useNavigate();
  const active = useHistoricalRange(defaultWindow);
  const [mode, setMode] = useState(active.custom ? 'custom' : active.window);
  const [timezone, setTimezone] = useState<Timezone>(active.timezone);
  const [start, setStart] = useState(() => initialInput(active.start, new Date(Date.now() - 60 * 60 * 1000), active.timezone));
  const [end, setEnd] = useState(() => initialInput(active.end, new Date(), active.timezone));
  const [error, setError] = useState('');

  useEffect(() => {
    setMode(active.custom ? 'custom' : active.window);
    setTimezone(active.timezone);
    if (active.start && Number.isFinite(new Date(active.start).getTime())) setStart(inputValue(new Date(active.start), active.timezone));
    if (active.end && Number.isFinite(new Date(active.end).getTime())) setEnd(inputValue(new Date(active.end), active.timezone));
    setError('');
  }, [active.custom, active.end, active.start, active.timezone, active.window]);

  const apply = () => {
    const next = new URLSearchParams(location.search);
    next.delete('window');
    next.delete('start');
    next.delete('end');
    next.delete('tz');
    if (mode === 'custom') {
      const parsedStart = parseInput(start, timezone);
      const parsedEnd = parseInput(end, timezone);
      if (!Number.isFinite(parsedStart.getTime()) || !Number.isFinite(parsedEnd.getTime())) {
        setError('Enter valid start and end timestamps.');
        return;
      }
      if (parsedEnd <= parsedStart) {
        setError('End must be after start.');
        return;
      }
      if (parsedEnd.getTime() - parsedStart.getTime() > 30 * 24 * 60 * 60 * 1000) {
        setError('The selected range cannot exceed 30 days.');
        return;
      }
      next.set('start', parsedStart.toISOString());
      next.set('end', parsedEnd.toISOString());
      next.set('tz', timezone);
    } else {
      next.set('window', mode);
    }
    setError('');
    navigate(location.pathname + '?' + next + location.hash);
  };

  return <section className="time-range" aria-label="Historical time range">
    <div className="time-range-fields">
      <label>Range<select value={mode} onChange={event => setMode(event.target.value)}>
        {presets.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
        <option value="custom">Custom range</option>
      </select></label>
      {mode === 'custom' && <>
        <label>Start<input type="datetime-local" value={start} onChange={event => setStart(event.target.value)}/></label>
        <label>End<input type="datetime-local" value={end} onChange={event => setEnd(event.target.value)}/></label>
        <label>Timezone<select value={timezone} onChange={event => setTimezone(event.target.value as Timezone)}>
          <option value="local">Browser local</option>
          <option value="utc">UTC</option>
        </select></label>
      </>}
      <button type="button" className="btn-primary" onClick={apply}>Apply</button>
    </div>
    <p className="time-range-active"><strong>Active window:</strong> <output>{active.label}</output>. Requested range and observed coverage are reported separately.</p>
    {error && <p className="warn time-range-error" role="alert">{error}</p>}
  </section>;
}
