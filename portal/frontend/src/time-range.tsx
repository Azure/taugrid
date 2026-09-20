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

function historicalWindowMilliseconds(value: string) {
  const input = value.startsWith('+') ? value.slice(1) : value;
  if (!input) return null;
  const units: Record<string, number> = {
    ns: 1e-6, us: 1e-3, 'µs': 1e-3, 'μs': 1e-3, ms: 1,
    s: 1000, m: 60_000, h: 3_600_000,
  };
  const segment = /(?:\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/gy;
  let total = 0;
  let offset = 0;
  for (let match = segment.exec(input); match; match = segment.exec(input)) {
    if (match.index !== offset) return null;
    total += Number.parseFloat(match[0]) * units[match[1]];
    offset = segment.lastIndex;
  }
  return offset === input.length && total > 0 && total <= 720 * 3_600_000 ? total : null;
}

function validTimestamp(value: string) {
  const timestamp = /^\d{4}-\d{2}-\d{2}T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/;
  if (!timestamp.test(value) || !Number.isFinite(Date.parse(value))) return false;
  const calendarDate = value.slice(0, 10);
  const parsed = new Date(calendarDate + 'T00:00:00Z');
  return Number.isFinite(parsed.getTime()) && parsed.toISOString().slice(0, 10) === calendarDate;
}

export function useHistoricalRange(defaultWindow: string) {
  const location = useLocation();
  const params = useMemo(() => new URLSearchParams(location.search), [location.search]);
  const windowValues = params.getAll('window');
  const startValues = params.getAll('start');
  const endValues = params.getAll('end');
  const hasWindow = windowValues.length > 0;
  const hasCustom = startValues.length > 0 || endValues.length > 0;
  const custom = hasCustom && !hasWindow;
  const requested = hasWindow ? windowValues[0] : defaultWindow;
  const validWindow = historicalWindowMilliseconds(requested) !== null;
  const window = requested;
  const start = params.get('start') || '';
  const end = params.get('end') || '';
  const timezone: Timezone = params.get('tz') === 'utc' ? 'utc' : 'local';
  const apiParams = new URLSearchParams();
  for (const key of ['window', 'start', 'end'] as const) {
    for (const value of params.getAll(key)) apiParams.append(key, value);
  }

  if (!hasWindow && !hasCustom) apiParams.set('window', defaultWindow);
  const api = apiParams.toString();
  const navigationParams = new URLSearchParams(api);
  if (params.has('tz')) navigationParams.set('tz', timezone);
  const navigation = navigationParams.toString();
  let invalid = '';
  if (windowValues.length > 1 || startValues.length > 1 || endValues.length > 1) {
    invalid = 'Historical range parameters must not be repeated.';
  } else if (hasWindow && hasCustom) {
    invalid = 'Use either a preset window or custom start and end timestamps, not both.';
  } else if (hasWindow && !validWindow) {
    invalid = `Unsupported historical window: ${requested}.`;
  } else if (hasCustom && (!start || !end)) {
    invalid = 'Custom historical ranges require both start and end timestamps.';
  } else if (hasCustom) {
    const elapsed = Date.parse(end) - Date.parse(start);
    if (!validTimestamp(start) || !validTimestamp(end)) {
      invalid = 'Enter valid RFC3339 start and end timestamps.';
    } else if (elapsed <= 0) {
      invalid = 'End must be after start.';
    } else if (elapsed > 30 * 24 * 60 * 60 * 1000) {
      invalid = 'The selected range cannot exceed 30 days.';
    }
  }
  const startLabel = timestampLabel(start, timezone);
  const endLabel = timestampLabel(end, timezone);
  const label = invalid ? 'Invalid historical range'
    : custom
    ? startLabel && endLabel ? `${startLabel} to ${endLabel}` : 'Incomplete or invalid custom range'
    : presets.find(([value]) => value === window)?.[1] || window;
  return { api, navigation, custom, window, start, end, timezone, label, invalid };
}

export function withHistoricalRange(url: string, api: string) {
  if (!api) return url;
  return url + (url.includes('?') ? '&' : '?') + api;
}

export function TimeRangeControls({ defaultWindow }: { defaultWindow: string }) {
  const location = useLocation();
  const navigate = useNavigate();
  const active = useHistoricalRange(defaultWindow);
  const selectableWindow = !active.invalid && !active.custom ? active.window : defaultWindow;
  const [mode, setMode] = useState(active.custom && !active.invalid ? 'custom' : selectableWindow);
  const [timezone, setTimezone] = useState<Timezone>(active.timezone);
  const [start, setStart] = useState(() => initialInput(active.start, new Date(Date.now() - 60 * 60 * 1000), active.timezone));
  const [end, setEnd] = useState(() => initialInput(active.end, new Date(), active.timezone));
  const [error, setError] = useState('');

  useEffect(() => {
    setMode(active.custom && !active.invalid ? 'custom' : selectableWindow);
    setTimezone(active.timezone);
    if (active.start && Number.isFinite(new Date(active.start).getTime())) setStart(inputValue(new Date(active.start), active.timezone));
    if (active.end && Number.isFinite(new Date(active.end).getTime())) setEnd(inputValue(new Date(active.end), active.timezone));
    setError('');
  }, [active.custom, active.end, active.invalid, active.start, active.timezone, active.window, selectableWindow]);

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
        {!presets.some(([value]) => value === selectableWindow) && <option value={selectableWindow}>{selectableWindow}</option>}
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
    {active.invalid && <p className="warn time-range-error" role="alert">{active.invalid} Select a valid range and apply it.</p>}
    {error && <p className="warn time-range-error" role="alert">{error}</p>}
  </section>;
}
