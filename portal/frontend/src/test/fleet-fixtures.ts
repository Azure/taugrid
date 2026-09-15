// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { Cluster, Nodes, NodeUtil } from '../types';

const fixtureObservedAt = '2026-09-14T20:03:00Z';
const fixtureScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'tau-system',
  source: 'portal', authorizationMode: 'workspace', availability: 'available', managed: false,
} as const;
const gpuConditionTypes = [
  'GPUECCDoubleRetired', 'GPUECCDoubleVolatile', 'GPUNVLinkCRCFlitErrors',
  'GPUNVLinkCRCDataErrors', 'GPUNVLinkReplayErrors', 'GPUThermalViolation',
  'GPUPowerViolation', 'GPUECCSingleVolatileRate', 'GPUECCSingleRetired',
  'GPUPCIeReplayErrors',
];
const ibConditionTypes = ['IBLinkDown', 'IBSymbolError'];
const observedConditions = (fault?: string) => [...gpuConditionTypes, ...ibConditionTypes].map(type => ({
  type,
  status: type === fault ? 'True' : 'False',
  reason: type === fault ? `${type}ThresholdExceeded` : `${type}Ok`,
  lastHeartbeatTime: fixtureObservedAt,
  lastTransitionTime: '2026-09-11T02:51:06Z',
}));

export const fleetNodes: Nodes = {
  scope: fixtureScope,
  totalNodes: 3,
  readyNodes: 3,
  gpuNodes: 3,
  totalGPUs: 3,
  gpuAllocatable: 3,
  gpuSchedulable: 3,
  gpuAllocated: 2,
  gpuAvailable: 1,
  gpuAllocationKnown: true,
  totalCPUCores: 120,
  totalMemoryGiB: 944.1,
  rdmaAdvertisedGpuNodes: 2,
  skus: [
    { sku: 'Standard_ND96isr_H200_v5', nodes: 2, gpus: 2 },
    { sku: 'Standard_NC24ads_A100_v4', nodes: 1, gpus: 1 },
  ],
  nodes: [
    {
      name: 'h200-node-a',
      agentPool: 'h200',
      agentPoolLabel: 'kubernetes.azure.com/agentpool',
      sku: 'Standard_ND96isr_H200_v5',
      site: 'eastus2',
      siteLabel: 'net.unbounded-cloud.io/site',
      region: 'eastus2euap',
      regionLabel: 'topology.kubernetes.io/region',
      zone: 'eastus2euap-1',
      zoneLabel: 'topology.kubernetes.io/zone',
      cpuCores: 40,
      memoryGiB: 314.7,
      gpuCapacity: 1,
      gpuAllocatable: 1,
      gpuAllocated: 1,
      gpuAvailable: 0,
      gpuProduct: 'NVIDIA H200',
      ready: true,
      schedulable: true,
      rdmaResources: [{ name: 'rdma/rdma_shared_device_a', capacity: 1, allocatable: 1 }],
      operationalConditions: observedConditions(),
    },
    {
      name: 'h200-node-b',
      agentPool: 'h200',
      agentPoolLabel: 'kubernetes.azure.com/agentpool',
      sku: 'Standard_ND96isr_H200_v5',
      site: 'eastus2',
      siteLabel: 'net.unbounded-cloud.io/site',
      region: 'eastus2euap',
      regionLabel: 'topology.kubernetes.io/region',
      zone: 'eastus2euap-1',
      zoneLabel: 'topology.kubernetes.io/zone',
      cpuCores: 40,
      memoryGiB: 314.7,
      gpuCapacity: 1,
      gpuAllocatable: 1,
      gpuAllocated: 1,
      gpuAvailable: 0,
      gpuProduct: 'NVIDIA H200',
      ready: true,
      schedulable: true,
      rdmaResources: [{ name: 'rdma/rdma_shared_device_a', capacity: 1, allocatable: 1 }],
      operationalConditions: observedConditions('GPUNVLinkReplayErrors')
        .filter(condition => condition.type !== 'GPUECCSingleRetired')
        .reverse(),
    },
    {
      name: 'a100-node-c',
      agentPool: 'a100',
      agentPoolLabel: 'kubernetes.azure.com/agentpool',
      sku: 'Standard_NC24ads_A100_v4',
      site: 'cluster',
      siteLabel: 'unbounded-cloud.io/site',
      region: 'westus3',
      regionLabel: 'topology.kubernetes.io/region',
      cpuCores: 40,
      memoryGiB: 314.7,
      gpuCapacity: 1,
      gpuAllocatable: 1,
      gpuAllocated: 0,
      gpuAvailable: 1,
      gpuProduct: 'NVIDIA A100',
      ready: true,
      schedulable: true,
    },
  ],
};

export const fleetGPUHealth: Cluster = {
  window: '15m0s',
  totalGPUs: 2,
  errorGPUs: 1,
  telemetryAvailable: true,
  utilizationObservedGPUs: 2,
  healthObservedGPUs: 2,
  unknownHealthGPUs: 0,
  models: [{ modelName: 'NVIDIA H200', gpus: 2 }],
  gpus: [
    {
      cluster: 'research-west', instance: 'h200-node-a', gpu: '0', modelName: 'NVIDIA H200',
      utilizationPct: 41, temperatureCelsius: 58, powerWatts: 410,
      memoryUsedMB: 32768, memoryFreeMB: 111104,
      correctableRemappedRows: 0, uncorrectableRemappedRows: 0, rowRemapFailure: 0,
      healthy: true,
    },
    {
      cluster: 'research-west', instance: 'h200-node-b', gpu: '0', modelName: 'NVIDIA H200',
      utilizationPct: 37, temperatureCelsius: 61, powerWatts: 402,
      memoryUsedMB: 32768, memoryFreeMB: 111104,
      correctableRemappedRows: 0, uncorrectableRemappedRows: 1, rowRemapFailure: 0,
      healthy: false,
    },
  ],
};

export const fleetNodeUtil: NodeUtil = {
  window: '15m0s',
  queriedAt: '2026-09-14T20:03:30Z',
  availability: 'ready',
  nodes: [
    {
      cluster: 'research-west', instance: 'h200-node-a', cpuCores: 40, cpuUtilPct: 63, memUsedPct: 72,
      memTotalBytes: 337893654528, memAvailBytes: 94610259968,
      cpuCoverage: { samples: 20, observedCores: 40, usableCores: 40, observedSeconds: 560, windowCoveragePct: 93, counterResets: 0 },
    },
    {
      cluster: 'research-west', instance: 'h200-node-b', cpuCores: 40, cpuUtilPct: 48, memUsedPct: 68,
      memTotalBytes: 337893654528, memAvailBytes: 108125798400,
      cpuCoverage: { samples: 20, observedCores: 40, usableCores: 40, observedSeconds: 560, windowCoveragePct: 93, counterResets: 0 },
    },
    {
      cluster: 'research-west', instance: 'a100-node-c', cpuCores: 40, cpuUtilPct: null, memUsedPct: null,
      memTotalBytes: null, memAvailBytes: null,
      cpuCoverage: { samples: 0, observedCores: 0, usableCores: 0, observedSeconds: 0, windowCoveragePct: 0, counterResets: 0 },
    },
  ],
};
