package runhistory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

type kustoWriter struct {
	endpoint   string
	database   string
	table      string
	timeout    time.Duration
	credential azcore.TokenCredential
}

func NewKustoWriter(config WriterConfig) (Writer, error) {
	if config.Endpoint == "" {
		return nil, fmt.Errorf("Kusto endpoint is required")
	}
	if config.Database == "" {
		return nil, fmt.Errorf("Kusto database is required")
	}
	if config.Table == "" {
		return nil, fmt.Errorf("Kusto table is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = 2 * time.Minute
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	return &kustoWriter{endpoint: config.Endpoint, database: config.Database, table: config.Table, timeout: config.Timeout, credential: credential}, nil
}

func (w *kustoWriter) Write(ctx context.Context, records []Record) error {
	data, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("marshal lifecycle records: %w", err)
	}
	ingestor, err := azkustoingest.NewManaged(
		azkustodata.NewConnectionStringBuilder(w.endpoint).WithTokenCredential(w.credential),
		azkustoingest.WithDefaultDatabase(w.database),
		azkustoingest.WithDefaultTable(w.table),
	)
	if err != nil {
		return fmt.Errorf("create managed Kusto ingestor: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	tag := ingestByTag(records)
	result, err := ingestor.FromReader(writeCtx, bytes.NewReader(data),
		azkustoingest.Database(w.database),
		azkustoingest.Table(w.table),
		azkustoingest.IngestionMapping(lifecycleMapping, azkustoingest.MultiJSON),
		azkustoingest.FlushImmediately(),
		azkustoingest.ReportResultToTable(),
		azkustoingest.Tags([]string{"ingest-by:" + tag}),
		azkustoingest.IfNotExists(tag),
	)
	if err != nil {
		_ = ingestor.Close()
		return fmt.Errorf("start managed ingestion: %w", err)
	}
	if err := <-result.Wait(writeCtx, azkustoingest.WithImmediateFirst()); err != nil {
		_ = ingestor.Close()
		return fmt.Errorf("wait for managed ingestion acknowledgement: %w", err)
	}
	if err := ingestor.Close(); err != nil {
		return fmt.Errorf("close managed Kusto ingestor: %w", err)
	}
	return nil
}

func ingestByTag(records []Record) string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ObservationID)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return "tau-lifecycle-" + hex.EncodeToString(sum[:])
}

// lifecycleMapping is inline and explicit so the recorder never relies on a
// cluster-side default mapping that could silently drift.
var lifecycleMapping = []map[string]string{
	{"column": "observed_at", "path": "$.observed_at"},
	{"column": "observation_id", "path": "$.observation_id"},
	{"column": "durable_id", "path": "$.durable_id"},
	{"column": "run_id", "path": "$.run_id"},
	{"column": "workspace_id", "path": "$.workspace_id"},
	{"column": "result_scope", "path": "$.result_scope"},
	{"column": "project", "path": "$.project"},
	{"column": "run_group_id", "path": "$.run_group_id"},
	{"column": "tags", "path": "$.tags"},
	{"column": "owning_resource_kind", "path": "$.owning_resource_kind"},
	{"column": "owning_resource_name", "path": "$.owning_resource_name"},
	{"column": "namespace", "path": "$.namespace"},
	{"column": "cluster", "path": "$.cluster"},
	{"column": "resource_uid", "path": "$.resource_uid"},
	{"column": "resource_version", "path": "$.resource_version"},
	{"column": "generation", "path": "$.generation"},
	{"column": "submit_time", "path": "$.submit_time"},
	{"column": "created_time", "path": "$.created_time"},
	{"column": "kueue_admitted_time", "path": "$.kueue_admitted_time"},
	{"column": "pod_start_time", "path": "$.pod_start_time"},
	{"column": "completion_time", "path": "$.completion_time"},
	{"column": "state", "path": "$.state"},
	{"column": "reason", "path": "$.reason"},
	{"column": "message", "path": "$.message"},
	{"column": "local_queue", "path": "$.local_queue"},
	{"column": "cluster_queue", "path": "$.cluster_queue"},
	{"column": "workload_kind", "path": "$.workload_kind"},
	{"column": "image", "path": "$.image"},
	{"column": "image_digest", "path": "$.image_digest"},
	{"column": "config_hash", "path": "$.config_hash"},
	{"column": "code_sha", "path": "$.code_sha"},
	{"column": "tau_command", "path": "$.tau_command"},
	{"column": "result_path", "path": "$.result_path"},
	{"column": "result_pvc", "path": "$.result_pvc"},
	{"column": "artifact_uri", "path": "$.artifact_uri"},
	{"column": "checkpoint_uri", "path": "$.checkpoint_uri"},
	{"column": "controller_version", "path": "$.controller_version"},
	{"column": "experiment_tracking", "path": "$.experiment_tracking"},
	{"column": "experiment_source", "path": "$.experiment_source"},
}
