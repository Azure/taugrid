package exptelemetry

import (
	"fmt"
	"strings"
)

const (
	RemoteWriteMetricName        = "experiment_metrics"
	RemoteWriteDatabase          = "Metrics"
	RemoteWriteTable             = "ExperimentMetrics"
	RemoteWriteDashboardFunction = "ExperimentMetricsDashboardRows"

	ProjectionTable             = "TauExpMetrics"
	ProjectionMetricsSpoolFile  = ProjectionTable + ".jsonl"
	ProjectionDashboardFunction = "TauExpMetricsDashboardRows"

	RunLifecycleTable             = "TauExpRunLifecycle"
	RunLifecycleDashboardFunction = "TauExpRunLifecycleDashboardRows"

	RunStatusMetricName       = "tau/run_status"
	RunStatusStateTag         = "tau.status.state"
	RunStatusReasonTag        = "tau.status.reason"
	RunStatusMessageTag       = "tau.status.message"
	RunStatusArtifactURITag   = "tau.status.artifact_uri"
	RunStatusCheckpointURITag = "tau.status.checkpoint_uri"

	TauWorkspaceTag    = "tau_workspace"
	TauNamespaceTag    = "tau_namespace"
	TauClusterTag      = "tau_cluster"
	TauRetryAttemptTag = "tau_retry_attempt"
)

// ValidateID enforces the identifier contract shared by expstore and telemetry producers.
func ValidateID(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%s %q is invalid (use lowercase alphanumerics with internal '-', '_' or '.')", kind, value)
	}
	for i, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !valid {
			return fmt.Errorf("%s %q is invalid (use lowercase alphanumerics with internal '-', '_' or '.')", kind, value)
		}
		if (i == 0 || i == len(value)-1) && (r == '-' || r == '_' || r == '.') {
			return fmt.Errorf("%s %q is invalid (must start and end with an alphanumeric)", kind, value)
		}
	}
	return nil
}
