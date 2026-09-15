// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { Cluster, Nodes, NodeUtil, RDMAValidationDetail, RDMAValidationPage, RDMAValidationSummary } from '../types';

const fixtureObservedAt = '2026-09-14T20:03:00Z';
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

export const passedValidation: RDMAValidationDetail = {
  validationId: 'nccl-rdma-0123456789abcdef0123456789abcdef',
  runId: 'nccl-rdma-20260914-200000',
  runAttempt: 1,
  schemaVersion: 'rdma-validation.v1',
  kind: 'tau.rdma_validation',
  state: 'passed',
  historicalStatus: 'pass',
  freshness: 'fresh',
  reasonCode: 'validation_passed',
  reason: 'Verified two-GPU inter-node NCCL all-reduce used InfiniBand without socket fallback.',
  workspaceId: 'research',
  cluster: 'research-west',
  namespace: 'tau-system',
  createdAt: '2026-09-14T20:00:00Z',
  startedAt: '2026-09-14T20:01:00Z',
  admittedAt: '2026-09-14T20:01:20Z',
  completedAt: '2026-09-14T20:03:00Z',
  observedAt: '2026-09-14T20:03:05Z',
  validUntil: '2026-09-15T20:03:05Z',
  staleAfterSeconds: 86400,
  ageSeconds: 365,
  durationSeconds: 120,
  source: {
    repository: 'Azure/taugrid',
    revision: '0123456789abcdef0123456789abcdef01234567',
    imageRepository: 'nvcr.io/nvidia/pytorch',
    imageIndexDigest: 'sha256:' + '1'.repeat(64),
    imagePlatformDigest: 'sha256:' + '2'.repeat(64),
    imageConfigDigest: 'sha256:' + '3'.repeat(64),
    sbomManifestDigest: 'sha256:' + '4'.repeat(64),
    sbomLayerDigest: 'sha256:' + '5'.repeat(64),
    vexManifestDigest: 'sha256:' + '6'.repeat(64),
    vexLayerDigest: 'sha256:' + '7'.repeat(64),
    signatureManifestDigest: 'sha256:' + '8'.repeat(64),
    signatureLayerDigest: 'sha256:' + '9'.repeat(64),
    signatureTrustVerified: false,
  },
  requested: {
    nodeCount: 2,
    podCount: 2,
    rankCount: 2,
    ranksPerNode: 1,
    gpusPerRank: 1,
    distinctHostname: true,
    site: 'eastus2',
    pool: 'h200',
    gpuModel: 'NVIDIA H200',
    gpuResource: 'nvidia.com/gpu',
    rdmaResource: 'rdma/rdma_shared_device_a',
    rdmaPerPod: 1,
    messageSizesBytes: [67108864],
    warmupIterations: 5,
    iterations: 20,
  },
  actual: {
    site: 'eastus2',
    pool: 'h200',
    nodes: [
      {
        name: 'h200-node-a',
        uid: 'node-a-uid',
        site: 'eastus2',
        pool: 'h200',
        gpuModel: 'NVIDIA H200',
        gpuUuids: ['GPU-aaaaaaaa'],
        rdmaDevices: [{
          resourceName: 'rdma/rdma_shared_device_a',
          device: 'mlx5_0',
          interface: 'ib0',
          state: 'ACTIVE',
        }],
      },
      {
        name: 'h200-node-b',
        uid: 'node-b-uid',
        site: 'eastus2',
        pool: 'h200',
        gpuModel: 'NVIDIA H200',
        gpuUuids: ['GPU-bbbbbbbb'],
        rdmaDevices: [{
          resourceName: 'rdma/rdma_shared_device_a',
          device: 'mlx5_1',
          interface: 'ib1',
          state: 'ACTIVE',
        }],
      },
    ],
  },
  placement: { distinctNodes: true, matchesRequest: true },
  pods: [
    { name: 'nccl-rdma-0', uid: 'pod-0-uid', node: 'h200-node-a', exitCode: 0 },
    { name: 'nccl-rdma-1', uid: 'pod-1-uid', node: 'h200-node-b', exitCode: 0 },
  ],
  ranks: [
    { rank: 0, podUid: 'pod-0-uid', node: 'h200-node-a', nodeUid: 'node-a-uid', peerAuthenticated: true, exitCode: 0 },
    { rank: 1, podUid: 'pod-1-uid', node: 'h200-node-b', nodeUid: 'node-b-uid', peerAuthenticated: true, exitCode: 0 },
  ],
  collective: { library: 'nccl', version: '2.28.8', operation: 'all_reduce' },
  transport: {
    backend: 'nccl',
    ncclNet: 'IB',
    interfaces: ['mlx5_0', 'mlx5_1'],
    rdmaDevices: ['mlx5_0', 'mlx5_1'],
    socketFallbackDetected: false,
    ibPositiveEvidence: true,
    evidence: ['NCCL INFO NET/IB : Using mlx5_0', 'NCCL INFO NET/IB : Using mlx5_1'],
    environment: { NCCL_DEBUG: 'INFO', NCCL_IB_DISABLE: '0' },
  },
  parameters: {
    worldSize: 2,
    processesPerPod: 1,
    elements: 16777216,
    dataType: 'float32',
    operation: 'all_reduce',
    messageSizesBytes: [67108864],
    warmupIterations: 5,
    iterations: 20,
  },
  measurements: [
    { rank: 0, messageSizeBytes: 67108864, iterations: 20, elapsedSeconds: 0.18, algbwGbps: 13.5, busbwGbps: 13.5 },
    { rank: 1, messageSizeBytes: 67108864, iterations: 20, elapsedSeconds: 0.2, algbwGbps: 12.5, busbwGbps: 12.5 },
  ],
  summary: {
    algbwGbps: { min: 12.5, max: 13.5, mean: 13, median: 13 },
    busbwGbps: { min: 12.5, max: 13.5, mean: 13, median: 13 },
  },
  correctness: { passed: true, maxError: 0, errorCount: 0 },
  jobExitCode: 0,
  rankExitCodes: [{ rank: 0, code: 0 }, { rank: 1, code: 0 }],
  cleanup: {
    status: 'complete',
    ownedResources: ['batch/v1 Job tau-system/nccl-rdma (job-uid)'],
    remainingResources: [],
  },
  artifactVerification: {
    state: 'verified',
    contentType: 'application/vnd.tau.rdma-validation.v1+json',
    uri: 'rdma-validation/nccl-rdma-0123456789abcdef0123456789abcdef.json',
    sha256: 'sha256:' + 'a'.repeat(64),
    sizeBytes: 4096,
    verifiedAt: '2026-09-14T20:03:07Z',
  },
  evidence: [{
    name: 'sanitized-logs',
    uri: 'evidence/nccl-rdma-0.log',
    sha256: 'sha256:' + 'b'.repeat(64),
    sizeBytes: 1024,
    capturedAt: '2026-09-14T20:03:01Z',
  }],
  producer: { name: 'taugrid-e2e', version: '0123456789abcdef0123456789abcdef01234567' },
  parser: { name: 'nccl-rdma', version: 'v1' },
};

export const latestSummary: RDMAValidationSummary = {
  latest: passedValidation,
  total: 2,
  generatedAt: '2026-09-14T20:04:00Z',
};

export const fleetNodes: Nodes = {
  totalNodes: 3,
  readyNodes: 3,
  gpuNodes: 3,
  totalGPUs: 3,
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
      gpuProduct: 'NVIDIA H200',
      ready: true,
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
      gpuProduct: 'NVIDIA H200',
      ready: true,
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
      gpuProduct: 'NVIDIA A100',
      ready: true,
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
      instance: 'h200-node-a', gpu: '0', modelName: 'NVIDIA H200',
      utilizationPct: 41, temperatureCelsius: 58, powerWatts: 410,
      memoryUsedMB: 32768, memoryFreeMB: 111104,
      correctableRemappedRows: 0, uncorrectableRemappedRows: 0, rowRemapFailure: 0,
      healthy: true,
    },
    {
      instance: 'h200-node-b', gpu: '0', modelName: 'NVIDIA H200',
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
      instance: 'h200-node-a', cpuCores: 40, cpuUtilPct: 63, memUsedPct: 72,
      memTotalBytes: 337893654528, memAvailBytes: 94610259968,
      cpuCoverage: { samples: 20, observedCores: 40, usableCores: 40, observedSeconds: 560, windowCoveragePct: 93, counterResets: 0 },
    },
    {
      instance: 'h200-node-b', cpuCores: 40, cpuUtilPct: 48, memUsedPct: 68,
      memTotalBytes: 337893654528, memAvailBytes: 108125798400,
      cpuCoverage: { samples: 20, observedCores: 40, usableCores: 40, observedSeconds: 560, windowCoveragePct: 93, counterResets: 0 },
    },
    {
      instance: 'a100-node-c', cpuCores: 40, cpuUtilPct: null, memUsedPct: null,
      memTotalBytes: null, memAvailBytes: null,
      cpuCoverage: { samples: 0, observedCores: 0, usableCores: 0, observedSeconds: 0, windowCoveragePct: 0, counterResets: 0 },
    },
  ],
};

export const firstHistoryPage: RDMAValidationPage = {
  validations: [passedValidation],
  nextCursor: 'page-two',
  truncated: true,
  generatedAt: '2026-09-14T20:04:00Z',
};

export const secondHistoryPage: RDMAValidationPage = {
  validations: [{
    ...passedValidation,
    validationId: 'nccl-rdma-fedcba9876543210fedcba9876543210',
    state: 'failed',
    historicalStatus: 'fail',
    reasonCode: 'socket_fallback_observed',
    reason: 'NCCL socket fallback was observed.',
    transport: { ...passedValidation.transport, backend: 'socket', socketFallbackDetected: true },
  }],
  nextCursor: null,
  truncated: false,
  generatedAt: '2026-09-14T20:04:00Z',
};
