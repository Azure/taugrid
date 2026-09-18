// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';
import { TimeRangeControls, useHistoricalRange } from './time-range';

afterEach(cleanup);

function RangeProbe() {
  const range = useHistoricalRange('24h');
  return <output data-testid="api">{range.api}</output>;
}

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

describe('historical range URL parsing', () => {
  it('uses the default only when the URL has no temporal parameters', () => {
    render(<MemoryRouter initialEntries={['/portal/fleet?workspace=research']}><RangeProbe/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=24h');
  });

  it('preserves invalid and mixed values so the API can reject them', () => {
    const { unmount } = render(<MemoryRouter initialEntries={['/portal/fleet?window=bad']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=bad');
    expect(screen.getByRole('alert')).toHaveTextContent('Unsupported historical window: bad.');

    unmount();
    render(<MemoryRouter initialEntries={['/portal/fleet?window=1h&start=2026-09-16T00%3A00%3A00Z&end=2026-09-16T01%3A00%3A00Z']}>
      <RangeProbe/><TimeRangeControls defaultWindow="24h"/>
    </MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=1h&start=2026-09-16T00%3A00%3A00Z&end=2026-09-16T01%3A00%3A00Z');
    expect(screen.getByRole('alert')).toHaveTextContent('not both');
  });

  it('preserves repeated parameters for strict server validation', () => {
    render(<MemoryRouter initialEntries={['/portal/fleet?window=1h&window=24h']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=1h&window=24h');
    expect(screen.getByRole('alert')).toHaveTextContent('must not be repeated');
  });

  it('distinguishes valid non-preset durations from empty invalid values', () => {
    const { unmount } = render(<MemoryRouter initialEntries={['/portal/fleet?window=2h30m']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=2h30m');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    unmount();
    render(<MemoryRouter initialEntries={['/portal/fleet?window=']}><RangeProbe/><TimeRangeControls defaultWindow="24h"/></MemoryRouter>);
    expect(screen.getByTestId('api')).toHaveTextContent('window=');
    expect(screen.getByRole('alert')).toHaveTextContent('Unsupported historical window');
  });

  it('applies preset and custom ranges without dropping other URL filters', () => {
    const { unmount } = render(<MemoryRouter initialEntries={['/portal/fleet?workspace=research&view=utilization&window=1h']}>
      <TimeRangeControls defaultWindow="24h"/><LocationProbe/>
    </MemoryRouter>);
    fireEvent.change(screen.getByLabelText('Range'), { target: { value: '168h' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.getByTestId('location')).toHaveTextContent('/portal/fleet?workspace=research&view=utilization&window=168h');

    unmount();
    render(<MemoryRouter initialEntries={['/portal/fleet?workspace=research&view=utilization&window=1h']}>
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
