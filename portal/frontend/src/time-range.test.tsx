// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';
import { TimeRangeControls, useHistoricalRange } from './time-range';

afterEach(cleanup);

function RangeProbe() {
  const range = useHistoricalRange('24h');
  return <><output data-testid="api">{range.api}</output><output data-testid="navigation">{range.navigation}</output></>;
}

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

describe('historical range URL parsing', () => {
  it('preserves the untouched bound and edited instant through timezone toggles', () => {
    const end = '2026-09-16T10:01:30.900000009Z';
    render(<MemoryRouter initialEntries={['/portal/runs?' + new URLSearchParams({ start: '2026-09-16T10:00:30.100000001Z', end, tz: 'utc' })]}>
      <TimeRangeControls defaultWindow="24h"/><LocationProbe/>
    </MemoryRouter>);
    fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-16T10:00:45.123' } });
    fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'local' } });
    fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'utc' } });
    expect(screen.getByLabelText('Start')).toHaveValue('2026-09-16T10:00:45.123');
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    const location = new URL(screen.getByTestId('location').textContent || '', 'http://localhost');
    expect(location.searchParams.get('start')).toBe('2026-09-16T10:00:45.123Z');
    expect(location.searchParams.get('end')).toBe(end);
  });

  it.runIf(Intl.DateTimeFormat().resolvedOptions().timeZone === 'America/New_York')('rejects a local DST gap and retains the earlier fold occurrence', () => {
    render(<MemoryRouter initialEntries={['/portal/runs?window=1h']}>
      <TimeRangeControls defaultWindow="24h"/><LocationProbe/>
    </MemoryRouter>);
    fireEvent.change(screen.getByLabelText('Range'), { target: { value: 'custom' } });
    fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-03-08T02:30' } });
    fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-03-08T04:00' } });
    fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'utc' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Enter valid start and end');
    expect(screen.getByTestId('location')).toHaveTextContent('window=1h');
    fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'local' } });
    fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-11-01T01:30' } });
    fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-11-01T02:30' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    const location = new URL(screen.getByTestId('location').textContent || '', 'http://localhost');
    expect(location.searchParams.get('start')).toBe('2026-11-01T05:30:00.000Z');
    expect(location.searchParams.get('end')).toBe('2026-11-01T07:30:00.000Z');
  });

  it.each([
    ['2026-09-01T00:00:00.000000001Z', '2026-09-01T00:00:00.000000002Z', true],
    ['2026-09-01T00:00:00.000000001Z', '2026-10-01T00:00:00.000000001Z', true],
    ['2026-09-01T00:00:00.000000001Z', '2026-10-01T00:00:00.000000002Z', false],
    ['2026-09-01T08:00:00.000000001+08:00', '2026-09-01T00:00:00.000000001Z', false],
  ])('compares precise custom instants %s to %s', (start, end, valid) => {
    render(<MemoryRouter initialEntries={['/portal/runs?' + new URLSearchParams({ start, end, tz: 'utc' })]}>
      <TimeRangeControls defaultWindow="24h"/><RangeProbe/>
    </MemoryRouter>);
    expect(screen.queryByRole('alert') === null).toBe(valid);
    if (valid) {
      const before = screen.getByTestId('api').textContent;
      fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.getByTestId('api').textContent).toBe(before);
    }
  });

  it.each([false, true])('preserves exact untouched bounds when applying (switch timezone=%s)', switchTimezone => {
    const start = '2026-09-16T10:00:30.100000001+00:00';
    const end = '2026-09-16T10:00:30.900000009Z';
    const params = new URLSearchParams({ start, end, tz: 'utc', workspace: 'research' });
    render(<MemoryRouter initialEntries={['/portal/runs?' + params]}>
      <TimeRangeControls defaultWindow="24h"/><RangeProbe/><LocationProbe/>
    </MemoryRouter>);
    const before = screen.getByTestId('api').textContent;
    if (switchTimezone) fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'local' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByTestId('api').textContent).toBe(before);
    const location = new URL(screen.getByTestId('location').textContent || '', 'http://localhost');
    expect(location.searchParams.get('start')).toBe(start);
    expect(location.searchParams.get('end')).toBe(end);
    expect(location.searchParams.get('workspace')).toBe('research');
    expect(location.searchParams.get('tz')).toBe(switchTimezone ? 'local' : 'utc');
  });

  it('rejects normalized calendar dates and non-RFC3339 clock times', () => {
    for (const [start, end] of [
      ['2026-02-30T00:00:00Z', '2026-02-30T00:30:00Z'],
      ['2026-09-16T24:00:00Z', '2026-09-17T00:30:00Z'],
    ]) {
      const view = render(<MemoryRouter initialEntries={['/portal/cost?start=' + start + '&end=' + end]}>
        <TimeRangeControls defaultWindow="24h"/>
      </MemoryRouter>);
      expect(screen.getByRole('alert')).toHaveTextContent('valid RFC3339');
      view.unmount();
    }
  });

  it('enforces the shared 30-day limit for URL and custom selections', () => {
    for (const [search, valid] of [
      ['window=60m', true], ['window=3600s', true], ['window=720h', true],
      ['window=0s', false], ['window=-1h', false], ['window=720h1s', false],
      ['start=2026-09-01T00:00:00Z&end=2026-10-01T00:00:00Z', true],
      ['start=2026-09-01T00:00:00Z&end=2026-10-01T00:00:01Z', false],
      ['start=bad&end=also-bad', false],
      ['start=2026-09-16T01:00:00Z&end=2026-09-16T00:00:00Z', false],
      ['start=2026-09-16T00:00:00Z&end=2026-09-16T00:00:00Z', false],
    ] as const) {
      const view = render(<MemoryRouter initialEntries={['/portal/cost?' + search]}>
        <TimeRangeControls defaultWindow="24h"/>
      </MemoryRouter>);
      expect(screen.queryByRole('alert') !== null, search).toBe(!valid);
      if (search === 'window=60m') expect(screen.getByLabelText('Range')).toHaveValue('60m');
      view.unmount();
    }
    render(<MemoryRouter initialEntries={['/portal/cost?window=24h']}>
      <TimeRangeControls defaultWindow="24h"/><LocationProbe/>
    </MemoryRouter>);
    fireEvent.change(screen.getByLabelText('Range'), { target: { value: 'custom' } });
    fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'utc' } });
    fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-01T00:00' } });
    fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-10-01T00:01' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.getByRole('alert')).toHaveTextContent('cannot exceed 30 days');
    expect(screen.getByTestId('location')).toHaveTextContent('window=24h');
    fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-10-01T00:00' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByTestId('location')).toHaveTextContent('end=2026-10-01T00%3A00%3A00.000Z');
  });

  it('uses the default only when the URL has no temporal parameters', () => {
    render(<MemoryRouter initialEntries={['/portal/cost?workspace=research']}><RangeProbe/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=24h');
  });

  it('preserves invalid and mixed values so the API can reject them', () => {
    const { unmount } = render(<MemoryRouter initialEntries={['/portal/cost?window=bad']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=bad');
    expect(screen.getByRole('alert')).toHaveTextContent('Unsupported historical window: bad.');

    unmount();
    render(<MemoryRouter initialEntries={['/portal/cost?window=1h&start=2026-09-16T00%3A00%3A00Z&end=2026-09-16T01%3A00%3A00Z']}>
      <RangeProbe/><TimeRangeControls defaultWindow="24h"/>
    </MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=1h&start=2026-09-16T00%3A00%3A00Z&end=2026-09-16T01%3A00%3A00Z');
    expect(screen.getByRole('alert')).toHaveTextContent('not both');
  });

  it('preserves repeated parameters for strict server validation', () => {
    render(<MemoryRouter initialEntries={['/portal/cost?window=1h&window=24h']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=1h&window=24h');
    expect(screen.getByRole('alert')).toHaveTextContent('must not be repeated');
  });

  it('distinguishes valid non-preset durations from empty invalid values', () => {
    const { unmount } = render(<MemoryRouter initialEntries={['/portal/cost?window=2h30m']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=2h30m');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    unmount();
    render(<MemoryRouter initialEntries={['/portal/cost?window=']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=');
    expect(screen.getByRole('alert')).toHaveTextContent('Unsupported historical window');
  });

  it('preserves the display timezone for detail navigation but not API requests', () => {
    render(<MemoryRouter initialEntries={['/portal/runs?start=2026-09-16T00%3A00%3A00Z&end=2026-09-17T09%3A00%3A00Z&tz=utc']}><RangeProbe/></MemoryRouter>);
    expect(screen.getByTestId('api')).not.toHaveTextContent('tz=');
    expect(screen.getByTestId('navigation')).toHaveTextContent('tz=utc');
  });

  it('applies preset and custom ranges without dropping other URL filters', () => {
    const { unmount } = render(<MemoryRouter initialEntries={['/portal/cost?workspace=research&view=utilization&window=1h']}>
      <TimeRangeControls defaultWindow="24h"/><LocationProbe/>
    </MemoryRouter>);
    fireEvent.change(screen.getByLabelText('Range'), { target: { value: '168h' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.getByTestId('location')).toHaveTextContent('/portal/cost?workspace=research&view=utilization&window=168h');

    unmount();
    render(<MemoryRouter initialEntries={['/portal/cost?workspace=research&view=utilization&window=1h']}>
      <TimeRangeControls defaultWindow="24h"/><LocationProbe/>
    </MemoryRouter>);
    fireEvent.change(screen.getByLabelText('Range'), { target: { value: 'custom' } });
    fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'utc' } });
    fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-16T00:00' } });
    fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-09-17T09:00' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    const location = screen.getByTestId('location').textContent || '';
    expect(location).toContain('workspace=research');
    expect(location).toContain('view=utilization');
    expect(location).toContain('start=2026-09-16T00%3A00%3A00.000Z');
    expect(location).toContain('end=2026-09-17T09%3A00%3A00.000Z');
    expect(location).toContain('tz=utc');
  });
});
