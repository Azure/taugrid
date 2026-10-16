// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { describe, expect, it } from 'vitest';
import { buildFleetGroups, selectInitialFleetNodes } from './InfiniBand';
import { largeFleetFixture } from './test/fleet-fixtures';

describe('Fleet large-fleet performance', () => {
  it('groups 2,048 nodes with linear append work and deterministic ordering', () => {
    const fixture = largeFleetFixture(2_048);
    const operations = { nodeVisits: 0, bucketAppends: 0 };

    const groups = buildFleetGroups(fixture.nodes.nodes, operations);

    expect(groups).toHaveLength(8);
    expect(groups.map(group => group.site)).toEqual([
      'site-0', 'site-1', 'site-2', 'site-3', 'site-4', 'site-5', 'site-6', 'site-7',
    ]);
    expect(groups.every(group => group.pools.length === 4)).toBe(true);
    expect(groups[0].pools[0].nodes[0].node.name).toBe('gpu-node-0000');
    expect(operations).toEqual({ nodeVisits: 2_048, bucketAppends: 4_096 });

    const legacySiteCopies = 8 * (256 * 257 / 2);
    const legacyPoolCopies = 32 * (64 * 65 / 2);
    expect(legacySiteCopies + legacyPoolCopies).toBe(329_728);
    expect((legacySiteCopies + legacyPoolCopies) / operations.bucketAppends).toBeGreaterThan(80);
  });

  it('bounds initial node disclosure independently of total fleet size', () => {
    const fixture = largeFleetFixture(2_048);
    const groups = buildFleetGroups(fixture.nodes.nodes);
    const visible = selectInitialFleetNodes(groups[7], 'gpu-node-2047');

    expect(visible).not.toBeNull();
    expect(visible).toHaveLength(49);
    expect([...visible!].some(entry => entry.node.name === 'gpu-node-2047')).toBe(true);
    for (const pool of groups[7].pools) {
      expect(visible!.filter(entry => pool.nodes.includes(entry))).toHaveLength(
        pool.nodes.some(entry => entry.node.name === 'gpu-node-2047') ? 13 : 12,
      );
    }
  });
});
