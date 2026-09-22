// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useEffect, useMemo, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';

type Timezone = 'local' | 'utc';
type BoundDraft = { kind: 'instant'; value: string } | { kind: 'invalid'; text: string };
const hourNanoseconds = 3_600_000_000_000n;

const presets = [
  ['15m', 'Last 15 minutes'],
  ['1h', 'Last hour'],
  ['24h', 'Last 24 hours'],
  ['168h', 'Last 7 days'],
  ['720h', 'Last 30 days'],
] as const;

function inputValue(date: Date, timezone: Timezone) {
  if (timezone === 'utc') return date.toISOString().slice(0, 23);
  const offset = date.getTimezoneOffset() * 60_000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 23);
}

function editDraft(value: string, timezone: Timezone): BoundDraft {
  const parsed = new Date(value + (timezone === 'utc' ? 'Z' : ''));
  const wallClock = new Date(value + 'Z');
  if (!Number.isFinite(parsed.getTime()) || !Number.isFinite(wallClock.getTime())
    || inputValue(parsed, timezone) !== wallClock.toISOString().slice(0, 23)) {
    return { kind: 'invalid', text: value };
  }
  return { kind: 'instant', value: parsed.toISOString() };
}

function initialDraft(value: string, fallback: Date): BoundDraft {
  if (!value) return { kind: 'instant', value: fallback.toISOString() };
  return validTimestamp(value) ? { kind: 'instant', value } : { kind: 'invalid', text: value };
}

function draftInput(draft: BoundDraft, timezone: Timezone) {
  return draft.kind === 'instant' ? inputValue(new Date(draft.value), timezone) : draft.text;
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

function timestampNanoseconds(value: string): bigint | null {
  if (!validTimestamp(value)) return null;
  const fraction = /\.(\d+)/.exec(value)?.[1] || '';
  const wholeSeconds = Date.parse(value.replace(/\.\d+/, ''));
  return BigInt(wholeSeconds) * 1_000_000n + BigInt(fraction.slice(0, 9).padEnd(9, '0'));
}

function customRangeError(start: string, end: string) {
  const startInstant = timestampNanoseconds(start);
  const endInstant = timestampNanoseconds(end);
  if (startInstant === null || endInstant === null) return 'Enter valid RFC3339 start and end timestamps.';
  const elapsed = endInstant - startInstant;
  if (elapsed <= 0n) return 'End must be after start.';
  if (elapsed > 30n * 24n * hourNanoseconds) return 'The selected range cannot exceed 30 days.';
  return '';
}

function isUTCHour(value: string) {
  const instant = timestampNanoseconds(value);
  return instant !== null && instant % hourNanoseconds === 0n;
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
    invalid = customRangeError(start, end);
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

export function TimeRangeControls({ defaultWindow, customRangePolicy = 'any' }: { defaultWindow: string; customRangePolicy?: 'any' | 'utc-hour' }) {
  const location = useLocation();
  const navigate = useNavigate();
  const active = useHistoricalRange(defaultWindow);
  const selectableWindow = !active.invalid && !active.custom ? active.window : defaultWindow;
  const [mode, setMode] = useState(active.custom && !active.invalid ? 'custom' : selectableWindow);
  const [timezone, setTimezone] = useState<Timezone>(active.timezone);
  const [start, setStart] = useState(() => initialDraft(active.start, new Date(Date.now() - 60 * 60 * 1000)));
  const [end, setEnd] = useState(() => initialDraft(active.end, new Date()));
  const [error, setError] = useState('');

  useEffect(() => {
    setMode(active.custom && !active.invalid ? 'custom' : selectableWindow);
    setError('');
  }, [active.custom, active.invalid, selectableWindow]);
  useEffect(() => { setTimezone(active.timezone); }, [active.timezone]);
  useEffect(() => {
    if (active.start) setStart(initialDraft(active.start, new Date()));
    if (active.end) setEnd(initialDraft(active.end, new Date()));
    setError('');
  }, [active.start, active.end]);

  const apply = () => {
    const next = new URLSearchParams(location.search);
    next.delete('window');
    next.delete('start');
    next.delete('end');
    next.delete('tz');
    if (mode === 'custom') {
      if (start.kind !== 'instant' || end.kind !== 'instant') {
        setError('Enter valid start and end timestamps.');
        return;
      }
      const invalid = customRangeError(start.value, end.value);
      if (invalid) {
        setError(invalid);
        return;
      }
      if (customRangePolicy === 'utc-hour' && (!isUTCHour(start.value) || !isUTCHour(end.value))) {
        setError('Cost ranges must start and end on whole UTC hours.');
        return;
      }
      next.set('start', start.value);
      next.set('end', end.value);
      next.set('tz', timezone);
    } else {
      next.set('window', mode);
    }
    setError('');
    navigate(location.pathname + '?' + next + location.hash);
  };

  return <section className="time-range" aria-label="Historical time range">
    <div className="time-range-fields">
      <label>Range<select value={mode} onChange={event => {
        const nextMode = event.target.value;
        if (nextMode === 'custom' && !active.custom && customRangePolicy === 'utc-hour') {
          const hourMilliseconds = 60 * 60 * 1000;
          const end = Math.floor(Date.now() / hourMilliseconds) * hourMilliseconds;
          setStart(initialDraft('', new Date(end - hourMilliseconds)));
          setEnd(initialDraft('', new Date(end)));
        }
        setMode(nextMode);
      }}>
        {presets.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
        {!presets.some(([value]) => value === selectableWindow) && <option value={selectableWindow}>{selectableWindow}</option>}
        <option value="custom">Custom range</option>
      </select></label>
      {mode === 'custom' && <>
        <label>Start<input type="datetime-local" step="0.001" title={start.kind === 'instant' ? start.value : undefined} value={draftInput(start, timezone)} onChange={event => setStart(editDraft(event.target.value, timezone))}/></label>
        <label>End<input type="datetime-local" step="0.001" title={end.kind === 'instant' ? end.value : undefined} value={draftInput(end, timezone)} onChange={event => setEnd(editDraft(event.target.value, timezone))}/></label>
        <label>Timezone<select value={timezone} onChange={event => setTimezone(event.target.value === 'utc' ? 'utc' : 'local')}>
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
