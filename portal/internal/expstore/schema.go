package expstore

const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
  key TEXT PRIMARY KEY,
  command TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS run_groups (
  run_group_id TEXT PRIMARY KEY,
  project TEXT NOT NULL,
  name TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS experiments (
  experiment_id TEXT PRIMARY KEY,
  project TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT,
  source TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
  run_id TEXT PRIMARY KEY,
  project TEXT NOT NULL,
  experiment_id TEXT NOT NULL DEFAULT '',
  run_group_id TEXT NOT NULL,
  parent_run_id TEXT,
  state TEXT NOT NULL,
  owner TEXT,
  created_at TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  config_hash TEXT,
  code_sha TEXT,
  image_digest TEXT,
  tau_command TEXT,
  result_uri TEXT,
  index_version TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS run_context (
  run_id TEXT PRIMARY KEY,
  cluster TEXT,
  namespace TEXT,
  team TEXT,
  profile TEXT,
  lane TEXT,
  local_queue TEXT,
  cluster_queue TEXT,
  kueue_workload TEXT,
  pod_uid TEXT,
  ray_job TEXT,
  resource_claims TEXT,
  gpu_class TEXT,
  gpu_count INTEGER,
  node_names TEXT,
  mounts TEXT,
  queue_wait_seconds REAL,
  gpu_hours REAL,
  estimated_cost REAL,
  runtime TEXT,
  dependencies TEXT,
  log_uri TEXT
);

CREATE TABLE IF NOT EXISTS configs (
  config_hash TEXT NOT NULL,
  run_id TEXT NOT NULL,
  format TEXT NOT NULL,
  uri TEXT NOT NULL,
  normalized_json TEXT,
  indexed_fields TEXT,
  PRIMARY KEY (config_hash, run_id)
);

CREATE TABLE IF NOT EXISTS artifacts (
  artifact_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  type TEXT NOT NULL,
  uri TEXT NOT NULL,
  name TEXT NOT NULL,
  durable_ref TEXT,
  content_type TEXT,
  digest TEXT,
  size_bytes INTEGER,
  step INTEGER,
  tags TEXT,
  rank INTEGER,
  created_at TEXT NOT NULL,
  preview TEXT,
  external_ref TEXT,
  caption TEXT,
  direction TEXT,
  alias TEXT,
  source_artifact_id TEXT,
  source_run_id TEXT,
  source_dataset_name TEXT,
  source_dataset_version TEXT,
  source_dataset_digest TEXT
);

CREATE TABLE IF NOT EXISTS events (
  event_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  time TEXT NOT NULL,
  type TEXT NOT NULL,
  source TEXT NOT NULL,
  severity TEXT NOT NULL,
  message TEXT NOT NULL,
  payload TEXT
);

CREATE TABLE IF NOT EXISTS observations (
  observation_id TEXT PRIMARY KEY,
  idempotency_key TEXT,
  author TEXT NOT NULL,
  source TEXT NOT NULL,
  type TEXT NOT NULL,
  scope_type TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  text TEXT NOT NULL,
  evidence TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tags (
  scope_type TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  PRIMARY KEY (scope_type, scope_id, key)
);

CREATE TABLE IF NOT EXISTS metric_files (
  file_id TEXT PRIMARY KEY,
  path TEXT NOT NULL,
  format TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  schema_hash TEXT,
  project TEXT,
  run_group_id TEXT,
  run_id TEXT,
  row_count INTEGER,
  digest TEXT,
  min_step INTEGER,
  max_step INTEGER,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS metric_summaries (
  run_id TEXT NOT NULL,
  metric_name TEXT NOT NULL,
  project TEXT,
  run_group_id TEXT,
  count INTEGER NOT NULL,
  finite_count INTEGER NOT NULL,
  non_finite_count INTEGER NOT NULL,
  min_step INTEGER,
  max_step INTEGER,
  latest_step INTEGER,
  latest_wall_time INTEGER,
  latest_value REAL,
  min_value REAL,
  max_value REAL,
  updated_at TEXT NOT NULL,
  latest_file_id TEXT,
  PRIMARY KEY (run_id, metric_name)
);

CREATE TABLE IF NOT EXISTS metric_summary_files (
  file_id TEXT PRIMARY KEY,
  run_id TEXT,
  summarized_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_runs_group ON runs(run_group_id);
CREATE INDEX IF NOT EXISTS idx_runs_state ON runs(state);
CREATE INDEX IF NOT EXISTS idx_experiments_project ON experiments(project);
CREATE INDEX IF NOT EXISTS idx_metric_files_run ON metric_files(run_id);
CREATE INDEX IF NOT EXISTS idx_metric_summaries_metric ON metric_summaries(metric_name);
CREATE INDEX IF NOT EXISTS idx_metric_summaries_group ON metric_summaries(run_group_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_run ON artifacts(run_id);
CREATE INDEX IF NOT EXISTS idx_events_run_time ON events(run_id, time);
CREATE INDEX IF NOT EXISTS idx_observations_scope ON observations(scope_type, scope_id);
`

const MetricSchemaVersion = "tau.metrics.scalar.v2"

var MetricSchemaColumns = []string{
	"project",
	"run_group_id",
	"run_id",
	"metric_name",
	"step",
	"wall_time",
	"value",
	"unit",
	"source",
	"split",
	"tags",
}
