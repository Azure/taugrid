// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export interface EvidenceIdentity {
  run_id?: string;
  scope_id?: string;
  scope_type?: string;
  artifact_id?: string;
  config_hash?: string;
  event_id?: string;
  observation_id?: string;
}

export interface EvidenceArtifact extends EvidenceIdentity {
  artifact_id: string;
  type?: string;
  name?: string;
  uri?: string;
  preview?: string;
  external_ref?: string;
  content_type?: string;
  digest?: string;
  size_bytes?: string;
  created_at?: string;
  caption?: string;
  description?: string;
  tag?: string;
  summary_tag?: string;
  media_tag?: string;
  step?: string | number;
  global_step?: string | number;
  training_step?: string | number;
  wall_time?: string;
  table?: { columns?: string[]; rows?: Record<string, unknown>[]; caption?: string; step?: string };
}

export interface EvidenceConfig extends EvidenceIdentity {
  config_hash: string;
  format?: string;
  uri?: string;
  normalized_json?: string;
  indexed_fields?: string;
}

export interface EvidenceEvent extends EvidenceIdentity {
  event_id: string;
  time?: string;
  type?: string;
  source?: string;
  severity?: string;
  message?: string;
  payload?: string;
}

export interface EvidenceObservation extends EvidenceIdentity {
  observation_id: string;
  author?: string;
  source?: string;
  type?: string;
  text?: string;
  evidence?: string;
  created_at?: string;
}

export interface EvidenceCollections {
  artifacts?: EvidenceArtifact[];
  configs?: EvidenceConfig[];
  events?: EvidenceEvent[];
  observations?: EvidenceObservation[];
}

export interface EvidenceRun extends EvidenceCollections {
  run_id: string;
  run_group_id?: string;
  state?: string;
  lifecycle_state?: string;
  code_sha?: string;
  image_digest?: string;
  systems?: { name: string; value?: string; collection_state?: string }[];
}

export interface EvidenceRuntimeDiff {
  field: string;
  values: { run_group_id: string; value: string }[];
}

export interface EvidenceSnapshot extends EvidenceCollections {
  payload_mode?: string;
  store_path?: string;
  runs?: EvidenceRun[];
  warnings?: string[];
  metric_options?: { name: string; goal?: string }[];
  compare?: { metric_name?: string; runtime_diffs?: EvidenceRuntimeDiff[] };
  chart?: {
    metric_name?: string;
    has_data?: boolean;
    series?: {
      run_id: string;
      run_group_id?: string;
      values?: { step: number; value: number | null }[];
      decimated?: boolean;
    }[];
  };
}

export type EvidenceSection = 'media' | 'errors' | 'labels' | 'repro' | 'evidence';
export type MediaKind = 'report' | 'table' | 'image' | 'video' | 'audio' | 'text' | 'artifact';
