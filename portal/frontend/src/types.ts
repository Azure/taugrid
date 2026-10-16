// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Wire contracts from portalapi, internal/portal boards, and core/runs.
export interface WorkspaceScope {
  workspace: string; name: string; cluster: string; namespace: string;
  team?: string; localQueue?: string; resultScope?: string; source: string;
  authorizationMode: string; availability: string; managed: boolean;
  experimentsUrl?: string; experimentsNative?: { state: string; apiBasePath?: string; reason?: string }; portalEndpoint?: string;
}
export interface Directory { workspaces: WorkspaceScope[]; selected?: WorkspaceScope; managed: boolean }
export interface Scoped { scope?: WorkspaceScope }
export interface Profiles {
  available: boolean; error?: string; tauClusterGeneration?: number; profileSetHash?: string;
  readyProfiles: { name: string; executionTarget: string; placement: string; defaultLocalQueue: string }[];
}
export interface Tracking { experimentPath?: string; experimentTracking?: string; runId?: string; project?: string; experiment?: string }
export interface Run extends Tracking { name: string; namespace?: string; kind: string; status: string; age: string; resourceUid?: string }
export interface Runs extends Scoped { namespace?: string; total: number; historyState?: string; historyDiagnostic?: string; runs: Run[] }
export interface Overview extends Scoped {
  workloadProfiles?: Profiles; activeUnavailable?: string; runningUnavailable?: string;
  pending: {
    name: string; namespace: string; queue: string; clusterQueue?: string; gpuRequested?: number;
    reason?: string; message?: string; admissionPriorityClass?: string;
    admissionPriorityClassKind?: string; admissionPriority?: number; podPriorityClasses?: string[];
  }[];
  active: Run[];
  running: (Tracking & {
    name: string; job?: string; namespace: string; resourceUid?: string; queue?: string; clusterQueue?: string;
    admissionPriorityClass?: string; admissionPriorityClassKind?: string;
    admissionPriority?: number; podPriorityClasses?: string[];
  })[];
  waiting?: (Tracking & {
    name: string; job?: string; namespace: string; resourceUid?: string; queue?: string; clusterQueue?: string;
    pendingReason?: string; pendingMessage?: string; admissionPriorityClass?: string;
    admissionPriorityClassKind?: string; admissionPriority?: number; podPriorityClasses?: string[];
  })[];
  cards: {
    fleet?: {
      readyNodes: number; totalNodes: number; gpuNodes: number; totalGPUs: number;
      totalCPUCores: number; totalMemoryGiB: number; topSKU?: string;
    };
    health?: { totalGPUs: number; errorGPUs: number };
    queue?: {
      admitted: number; pending: number; gpuUsed: number; gpuHeadroom: number;
      queues?: { namespace: string; queue: string; clusterQueue?: string; admitted: number; pending: number }[];
    };
    cost?: { totalGPUHours: number; window: string; idleGPUs: number };
    ray?: { clusters: number };
    fleetUnavailable?: string; healthUnavailable?: string; queueUnavailable?: string; costUnavailable?: string; rayUnavailable?: string;
  };
}
export interface GPU {
  cluster?: string; instance: string; gpu: string; modelName?: string; namespace?: string; pod?: string;
  utilizationPct: number | null; temperatureCelsius: number | null; powerWatts: number | null;
  memoryUsedMB: number | null; memoryFreeMB: number | null; correctableRemappedRows: number | null;
  uncorrectableRemappedRows: number | null; rowRemapFailure: number | null; healthy: boolean | null;
}
export interface Cluster extends Scoped {
  window: string; totalGPUs: number; errorGPUs: number; telemetryAvailable: boolean;
  utilizationObservedGPUs: number; healthObservedGPUs: number; unknownHealthGPUs: number;
  models: { modelName: string; gpus: number }[]; gpus: GPU[];
}
export interface Nodes extends Scoped {
  totalNodes: number; readyNodes: number; gpuNodes: number; totalGPUs: number;
  gpuAllocatable: number; gpuSchedulable: number; gpuAllocated: number; gpuAvailable: number; gpuAllocationKnown: boolean; gpuAllocationError?: string;
  totalCPUCores: number; totalMemoryGiB: number; rdmaAdvertisedGpuNodes?: number;
  skus: { sku: string; nodes: number; gpus: number }[];
  nodes: { name: string; agentPool?: string; sku?: string; site?: string; region?: string; zone?: string;
    cpuCores: number; memoryGiB: number; gpuCapacity: number; gpuAllocatable?: number;
    cpuUtilPct?: number; memUsedPct?: number; metricsObservedAt?: string; metricsWindow?: string;
    gpuAllocated?: number; gpuAvailable?: number; gpuProduct?: string; ready: boolean; schedulable?: boolean;
    agentPoolLabel?: string; siteLabel?: string; siteLabelConflict?: boolean; regionLabel?: string; zoneLabel?: string;
    rdmaResources?: { name: string; capacity: number; allocatable: number }[];
    operationalConditions?: {
      type: string; category: 'gpu' | 'infiniband'; status: string; reason?: string; message?: string;
      lastHeartbeatTime?: string; lastTransitionTime?: string;
    }[] }[];
  daemonSets?: { namespace: string; name: string; ready: number; desired: number; available: number; healthy: boolean }[];
  daemonSetsError?: string;
  nodeMetricsError?: string;
}
export interface NodeUtil extends Scoped {
  window: string; queriedAt: string; availability: 'ready' | 'empty';
  nodes: {
    cluster?: string; instance: string; cpuCores: number; cpuUtilPct: number | null;
    memUsedPct: number | null; memTotalBytes: number | null; memAvailBytes: number | null;
    memTotalSampleAt?: string; memAvailSampleAt?: string;
    cpuCoverage: {
      samples: number; observedCores: number; usableCores: number; observedSeconds: number;
      windowCoveragePct: number; counterResets: number; firstSampleAt?: string; lastSampleAt?: string;
    };
  }[];
}
export interface Jobs extends Scoped {
  namespace?: string; workloadProfiles?: Profiles; hints?: string[];
  groups: { namespace: string; team: string; lane: string; gpuClass: string; queue: string;
    pending: number; admitted: number; gpuUsed: number; gpuNominal: number; gpuHeadroom: number; queueFound: boolean; quotaFound: boolean }[];
}
export interface CostCoverage {
  observedSamples: number; gpuHoursSamples: number; costSamples: number; utilizationSamples: number;
}
export interface Cost extends Scoped {
  window: string; totalGPUHours: number; totalEstimatedCostUSD: number;
  costAvailable: boolean; gpuHoursAvailable: boolean; costCoverage: CostCoverage; idleAvailable: boolean;
  idleCoverage: { observedGPUs: number; measuredGPUs: number; eligibleGPUs: number; observedSamples: number; validSamples: number };
  workspaces: {
    workspace: string; namespace: string; gpuHours: number; estimatedCostUSD: number; peakGPUs: number; avgUtilPct: number | null;
    costAvailable: boolean; gpuHoursAvailable: boolean; coverage: CostCoverage;
  }[];
  idleGPUs: { instance: string; gpu: string; modelName?: string; namespace?: string; pod?: string; avgUtilPct: number; samples: number }[];
}
export interface Ray extends Scoped {
  total: number; namespace?: string; historyState?: string; historyDiagnostic?: string; history?: Run[];
  clusters: { name: string; namespace: string; service: string; type?: string; proxyPath?: string; available: boolean }[];
}
export interface Lifecycle {
  state?: string; effectiveState?: string; reason?: string; message?: string;
  completionTime?: string; artifactUri?: string; checkpointUri?: string;
}
export interface HistoryEvent extends Lifecycle {
  observedAt: string; name?: string; runId?: string; durableId?: string; resourceUid?: string;
  namespace?: string; cluster?: string; queue?: string; image?: string; command?: string; resultPvc?: string; resultPath?: string;
}
export interface RayHistory extends Scoped { events: HistoryEvent[] }
export interface SourceDiagnostic {
  state: 'ready' | 'empty' | 'unavailable' | 'not_configured'; message?: string;
  stale?: boolean; lastSuccessAt?: number;
}
export interface JobDetail extends Scoped {
  name: string; namespace: string; kind: string; resourceUid?: string; objectState: 'live' | 'deleted'; status: string; runId?: string;
  stages: { object: string; admission: string; scheduling: string; application: string; tracking: string };
  object: { age: string; created?: string; jobDeploymentStatus?: string; rayClusterName?: string; jobId?: string; executionTarget?: string; reason?: string; message?: string };
  resourceRelease?: { computeState: string; quotaState: string; message: string; activePods: number; nodes?: string[] };
  links: { stellarPath?: string; rayDashboardPath?: string; rayDashboardReachable: boolean };
  workloads?: { name: string; queue?: string; clusterQueue?: string; admitted: boolean; finished: boolean; pendingReason?: string; pendingMessage?: string }[];
  pods?: { name: string; phase: string; node?: string; nodePath?: string; restarts: number; startedAt?: string; containers?: {
    name: string; ready: boolean; restarts: number; state?: string; reason?: string; message?: string; previousAvailable?: boolean;
  }[] }[];
  events?: { type: string; reason: string; message: string; count: number; last?: string }[];
  lifecycle?: Lifecycle;
  history?: HistoryEvent[];
  telemetry?: {
    start: string; end: string; gpuCount: number; sampleCount: number; utilizationSampleCount: number;
    averageUtilizationPct?: number; coverage: string; gpus: {
      instance: string; pod: string; gpu: string; modelName?: string; samples: number; utilizationSamples: number;
      firstSample?: string; lastSample?: string; averageUtilizationPct?: number; peakUtilizationPct?: number;
      maxTemperatureCelsius?: number; maxPowerWatts?: number; maxMemoryUsedMB?: number;
      maxCorrectableRemappedRows?: number; maxUncorrectableRemappedRows?: number; maxRowRemapFailure?: number;
    }[];
  };
  diagnostics: { workloads: SourceDiagnostic; pods: SourceDiagnostic; events: SourceDiagnostic; tracking: SourceDiagnostic; telemetry: SourceDiagnostic };
}
export interface WorkloadLogSnapshot extends Scoped {
  pod: string; container: string; previous: boolean; content: string;
  tailLines: number; limitBytes: number; truncated: boolean; redactionApplied: boolean;
}
