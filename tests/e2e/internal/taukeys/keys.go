// Package taukeys is the e2e module's small, source-checked view of the Tau key
// contract. The e2e module does not depend on core/workloadmeta,
// so workflows/tau_label_contract_test.go verifies these declarations against
// the CLI source instead.
package taukeys

const (
	APIGroup = "tau.azure.com"

	LabelManagedBy = "tau.azure.com/managed-by"
	LabelWorkspace = "tau.azure.com/workspace"
	// LabelMigrationRole marks a PVC's role in a workspace data migration.
	LabelMigrationRole = "tau.azure.com/migration-role"
	LabelRunID         = "tau.azure.com/run-id"
	LabelWorkloadKind  = "tau.azure.com/workload-kind"

	// AnnotationWorkspaceID is an annotation, not a label: it records the
	// TauWorkspace UID a workload was admitted against.
	AnnotationWorkspaceID = "tau.azure.com/workspace-id"

	// Render-time GPU contract keys, mirrored from workloadmeta.
	LabelGPUCount     = "tau.azure.com/gpu-count"
	LabelGPUPlacement = "tau.azure.com/gpu-placement"

	// LabelSchedulerTest marks pods created by the e2e packing/affinity suite.
	LabelSchedulerTest = "tau.azure.com/scheduler-test"

	// Payload integrity annotations stamped on rendered workloads.
	AnnotationPayloadDigest = "tau.azure.com/payload-digest"
	AnnotationPayloadSHA256 = "tau.azure.com/payload-sha256"
	AnnotationClusterName   = "tau.azure.com/cluster-name"

	// MultiKueue activation keys. These are owned by the activation tooling in
	// applications/tau-queues/activation, not by the CLI, so they are declared
	// here rather than cross-checked against workloadmeta.
	LabelActivationRun              = "tau.azure.com/activation-run"
	LabelActivationScope            = "tau.azure.com/activation-scope"
	AnnotationWorkerAPIServer       = "tau.azure.com/worker-api-server"
	AnnotationCredentialTransaction = "tau.azure.com/credential-transaction"
)
