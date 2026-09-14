// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
)

var (
	sha256DigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var allowedEnvironmentKeys = map[string]bool{
	"NCCL_DEBUG":         true,
	"NCCL_DEBUG_SUBSYS":  true,
	"NCCL_IB_DISABLE":    true,
	"TAUGRID_BACKEND":    true,
	"TAUGRID_ELEMENTS":   true,
	"TAUGRID_ITERATIONS": true,
	"TAUGRID_LIVE_RDMA":  true,
	"TAUGRID_WARMUP":     true,
}

var validReasons = map[ReasonCode]bool{
	ReasonValidationPassed:         true,
	ReasonSocketFallbackObserved:   true,
	ReasonIBTransportNotProven:     true,
	ReasonTransportFailure:         true,
	ReasonPeerAuthenticationFailed: true,
	ReasonPlacementMismatch:        true,
	ReasonTopologyMismatch:         true,
	ReasonCorrectnessError:         true,
	ReasonNonzeroExit:              true,
	ReasonCleanupIncomplete:        true,
	ReasonMissingRequiredEvidence:  true,
	ReasonInvalidMeasurement:       true,
	ReasonEvidenceIntegrityMissing: true,
	ReasonParserRejected:           true,
	ReasonRuntimeError:             true,
}

var failureReasons = map[ReasonCode]bool{
	ReasonSocketFallbackObserved:   true,
	ReasonIBTransportNotProven:     true,
	ReasonTransportFailure:         true,
	ReasonPeerAuthenticationFailed: true,
	ReasonPlacementMismatch:        true,
	ReasonTopologyMismatch:         true,
	ReasonCorrectnessError:         true,
	ReasonNonzeroExit:              true,
	ReasonCleanupIncomplete:        true,
	ReasonInvalidMeasurement:       true,
	ReasonRuntimeError:             true,
}

func Finalize(result *Result) error {
	if result == nil {
		return fmt.Errorf("result is required")
	}
	if result.Schema == "" {
		result.Schema = SchemaVersion
	}
	if result.Kind == "" {
		result.Kind = Kind
	}
	if result.Cleanup.State == "" {
		result.Cleanup.State = CleanupUnknown
	}
	evaluation := Evaluate(*result)
	result.Status = evaluation.Status
	result.Reason = evaluation.Reason
	result.Errors = evaluation.Errors
	return Validate(*result)
}

func Evaluate(result Result) Evaluation {
	var failures []ValidationError
	var unknowns []ValidationError
	for _, observed := range result.Errors {
		if failureReasons[observed.Code] {
			failures = append(failures, observed)
		} else {
			unknowns = append(unknowns, observed)
		}
	}

	addFailure := func(code ReasonCode, field, message string) {
		failures = append(failures, ValidationError{Code: code, Field: field, Message: message})
	}
	addUnknown := func(code ReasonCode, field, message string) {
		unknowns = append(unknowns, ValidationError{Code: code, Field: field, Message: message})
	}

	if result.Transport.SocketFallbackObserved != nil && *result.Transport.SocketFallbackObserved {
		addFailure(ReasonSocketFallbackObserved, "transport.socket_fallback_observed", "NCCL selected socket transport")
	}
	if result.Transport.PositiveIBEvidence != nil && !*result.Transport.PositiveIBEvidence {
		addFailure(ReasonIBTransportNotProven, "transport.positive_ib_evidence", "NCCL did not report positive NET/IB evidence")
	}
	if result.Transport.Backend != "" && result.Transport.Backend != "nccl" {
		addFailure(ReasonTransportFailure, "transport.backend", "live validation backend was not nccl")
	}
	if result.Transport.NCCLNet != "" && result.Transport.NCCLNet != "IB" {
		addFailure(ReasonTransportFailure, "transport.nccl_net", "NCCL network transport was not IB")
	}
	if result.Placement.MatchesRequest != nil && !*result.Placement.MatchesRequest {
		addFailure(ReasonPlacementMismatch, "placement.matches_request", "actual placement did not match the requested topology")
	}
	if result.Placement.DistinctNodes != nil && !*result.Placement.DistinctNodes {
		addFailure(ReasonPlacementMismatch, "placement.distinct_nodes", "ranks did not run on distinct nodes")
	}
	if result.Requested.Topology.Site != "" && result.Actual.Site != "" &&
		result.Requested.Topology.Site != result.Actual.Site {
		addFailure(ReasonPlacementMismatch, "actual.site", "actual site did not match requested site")
	}
	if result.Requested.Topology.Pool != "" && result.Actual.Pool != "" &&
		result.Requested.Topology.Pool != result.Actual.Pool {
		addFailure(ReasonPlacementMismatch, "actual.pool", "actual pool did not match requested pool")
	}
	for index, node := range result.Actual.Nodes {
		if result.Requested.Topology.GPUModel != "" && node.GPUModel != "" &&
			result.Requested.Topology.GPUModel != node.GPUModel {
			addFailure(ReasonPlacementMismatch, fmt.Sprintf("actual.nodes[%d].gpu_model", index), "actual GPU model did not match requested model")
		}
		if node.RDMALinkState != "" && !strings.Contains(strings.ToUpper(node.RDMALinkState), "ACTIVE") {
			addFailure(ReasonTransportFailure, fmt.Sprintf("actual.nodes[%d].rdma_link_state", index), "RDMA link was not active")
		}
	}
	if len(result.Actual.Nodes) > 2 || len(result.Pods) > 2 || len(result.Ranks) > 2 || len(result.RankExits) > 2 {
		addFailure(ReasonTopologyMismatch, "actual", "validation contained more than two nodes, pods, or ranks")
	}
	if duplicateNodeOrRank(result) {
		addFailure(ReasonTopologyMismatch, "actual", "validation contained duplicate or mismatched node, pod, or rank identity")
	}
	for _, rank := range result.Ranks {
		if rank.PeerAuthVerified != nil && !*rank.PeerAuthVerified {
			addFailure(ReasonPeerAuthenticationFailed, fmt.Sprintf("ranks[%d].peer_auth_verified", rank.Rank), "rank peer authentication failed")
		}
	}
	if result.Correctness.MaxError != nil && *result.Correctness.MaxError != 0 {
		addFailure(ReasonCorrectnessError, "correctness.max_error", "collective correctness error was nonzero")
	}
	if result.Correctness.ErrorCount != nil && *result.Correctness.ErrorCount != 0 {
		addFailure(ReasonCorrectnessError, "correctness.error_count", "collective correctness error count was nonzero")
	}
	if result.JobExitCode != nil && *result.JobExitCode != 0 {
		addFailure(ReasonNonzeroExit, "job_exit_code", "Job exit code was nonzero")
	}
	for _, exit := range result.RankExits {
		if exit.ExitCode != nil && *exit.ExitCode != 0 {
			addFailure(ReasonNonzeroExit, fmt.Sprintf("rank_exits[%d]", exit.Rank), "rank exit code was nonzero")
		}
	}
	switch result.Cleanup.State {
	case CleanupIncomplete:
		addFailure(ReasonCleanupIncomplete, "cleanup.state", "owned resources remained after cleanup")
	case "", CleanupUnknown:
		addUnknown(ReasonMissingRequiredEvidence, "cleanup.state", "cleanup completion is unknown")
	}
	if len(result.Cleanup.RemainingResources) > 0 {
		addFailure(ReasonCleanupIncomplete, "cleanup.remaining_resources", "owned resources remained after cleanup")
	}
	if invalidOwnedResourceSet(result.Cleanup.OwnedResources) {
		addFailure(ReasonCleanupIncomplete, "cleanup.owned_resources", "cleanup ownership did not match the six successful-create resources")
	}
	if hasInvalidMeasurement(result.Measurements) {
		addFailure(ReasonInvalidMeasurement, "measurements", "bandwidth measurements must be positive and finite")
	}

	for _, field := range missingPassFields(result) {
		code := ReasonMissingRequiredEvidence
		if strings.HasPrefix(field, "evidence") {
			code = ReasonEvidenceIntegrityMissing
		}
		addUnknown(code, field, "required pass evidence is missing or unverifiable")
	}

	failures = normalizeErrors(failures)
	unknowns = normalizeErrors(unknowns)
	if len(failures) > 0 {
		return Evaluation{Status: StatusFail, Reason: failures[0].Code, Errors: append(failures, unknowns...)}
	}
	if len(unknowns) > 0 {
		return Evaluation{Status: StatusUnknown, Reason: unknowns[0].Code, Errors: unknowns}
	}
	return Evaluation{Status: StatusPass, Reason: ReasonValidationPassed, Errors: []ValidationError{}}
}

func Validate(result Result) error {
	if result.Schema != SchemaVersion {
		return fmt.Errorf("schema = %q, want %q", result.Schema, SchemaVersion)
	}
	if result.Kind != Kind {
		return fmt.Errorf("kind = %q, want %q", result.Kind, Kind)
	}
	if err := exptelemetry.ValidateID("validation_id", result.ValidationID); err != nil {
		return err
	}
	for kind, value := range map[string]string{
		"run_id": result.RunID, "workspace_id": result.WorkspaceID, "project_id": result.ProjectID,
		"experiment_id": result.ExperimentID, "run_group_id": result.RunGroupID,
		"cluster": result.Cluster, "namespace": result.Namespace,
	} {
		if value != "" {
			if err := exptelemetry.ValidateID(kind, value); err != nil {
				return err
			}
		}
	}
	if result.Attempt < 1 {
		return fmt.Errorf("attempt must be positive")
	}
	if result.Status != StatusPass && result.Status != StatusFail && result.Status != StatusUnknown {
		return fmt.Errorf("unknown status %q", result.Status)
	}
	if result.Cleanup.State != CleanupComplete && result.Cleanup.State != CleanupIncomplete && result.Cleanup.State != CleanupUnknown {
		return fmt.Errorf("unknown cleanup state %q", result.Cleanup.State)
	}
	if !validReasons[result.Reason] {
		return fmt.Errorf("unknown reason %q", result.Reason)
	}
	if result.CreatedAt.IsZero() || result.ObservedAt.IsZero() {
		return fmt.Errorf("created_at and observed_at are required")
	}
	for field, value := range map[string]time.Time{
		"created_at": result.CreatedAt, "started_at": result.StartedAt, "admitted_at": result.AdmittedAt,
		"completed_at": result.CompletedAt, "observed_at": result.ObservedAt, "valid_until": result.ValidUntil,
		"cleanup.started_at": result.Cleanup.StartedAt, "cleanup.completed_at": result.Cleanup.CompletedAt,
	} {
		if !value.IsZero() && !isUTC(value) {
			return fmt.Errorf("%s must be UTC", field)
		}
	}
	if result.StaleAfterSeconds != DefaultStaleAfterSeconds {
		return fmt.Errorf("stale_after_seconds must be %d (24 hours)", DefaultStaleAfterSeconds)
	}
	if result.ValidUntil.IsZero() || !result.ValidUntil.Equal(result.ObservedAt.Add(time.Duration(result.StaleAfterSeconds)*time.Second)) {
		return fmt.Errorf("valid_until must equal observed_at plus stale_after_seconds")
	}
	if err := validateTimeOrder(result); err != nil {
		return err
	}
	if result.Source.Revision != "" && !revisionRE.MatchString(result.Source.Revision) {
		return fmt.Errorf("source.revision must be a 40-character lowercase Git SHA")
	}
	for field, digest := range imageDigests(result.Image) {
		if digest != "" && !sha256DigestRE.MatchString(digest) {
			return fmt.Errorf("%s must be sha256 followed by 64 lowercase hexadecimal characters", field)
		}
	}
	if len(result.Transport.Environment) > len(allowedEnvironmentKeys) {
		return fmt.Errorf("transport.environment exceeds the fixed allowlist")
	}
	for key, value := range result.Transport.Environment {
		if !allowedEnvironmentKeys[key] {
			return fmt.Errorf("transport.environment key %q is not allowlisted", key)
		}
		if len(value) > 128 || containsSecretMaterial(value) {
			return fmt.Errorf("transport.environment value for %q is unsafe", key)
		}
	}
	for _, observed := range result.Errors {
		if !validReasons[observed.Code] || len(observed.Field) > 128 || len(observed.Message) > 512 ||
			containsSecretMaterial(observed.Field) || containsSecretMaterial(observed.Message) {
			return fmt.Errorf("invalid or unsafe validation error")
		}
	}
	allowedEvidence := map[string]bool{
		"sanitized-manifest": false,
		"sanitized-logs":     false,
		"placement":          false,
		"image-receipt":      false,
		"cleanup":            false,
	}
	for _, evidence := range result.Evidence {
		if evidence.Name == "" || len(evidence.Name) > 64 || evidence.SizeBytes < 0 {
			return fmt.Errorf("invalid evidence reference")
		}
		if _, allowed := allowedEvidence[evidence.Name]; !allowed {
			return fmt.Errorf("unexpected evidence name %q", evidence.Name)
		}
		if allowedEvidence[evidence.Name] {
			return fmt.Errorf("duplicate evidence name %q", evidence.Name)
		}
		allowedEvidence[evidence.Name] = true
		if evidence.SHA256 != "" && !sha256DigestRE.MatchString(evidence.SHA256) {
			return fmt.Errorf("evidence %q has an invalid SHA-256 digest", evidence.Name)
		}
		if !evidence.CapturedAt.IsZero() && !isUTC(evidence.CapturedAt) {
			return fmt.Errorf("evidence %q captured_at must be UTC", evidence.Name)
		}
		if containsSecretMaterial(evidence.URI) || strings.ContainsAny(evidence.URI, "?#") {
			return fmt.Errorf("evidence %q URI is unsafe", evidence.Name)
		}
	}
	if hasNonFiniteResult(result) {
		return fmt.Errorf("result contains a non-finite numeric value")
	}
	if err := validateStats(result.Measurements); err != nil {
		return err
	}

	evaluation := Evaluate(result)
	if result.Status != evaluation.Status || result.Reason != evaluation.Reason ||
		!reflect.DeepEqual(normalizeErrors(result.Errors), normalizeErrors(evaluation.Errors)) {
		return fmt.Errorf("status, reason, and errors do not match fail-closed evaluation")
	}
	return nil
}

func IsFresh(result Result, now time.Time) bool {
	return !result.ObservedAt.IsZero() && !result.ValidUntil.IsZero() &&
		!now.UTC().After(result.ValidUntil)
}

func duplicateNodeOrRank(result Result) bool {
	nodes := map[string]bool{}
	nodeUIDs := map[string]bool{}
	gpuUUIDs := map[string]bool{}
	for _, node := range result.Actual.Nodes {
		if node.Name != "" && nodes[node.Name] || node.UID != "" && nodeUIDs[node.UID] || node.GPUUUID != "" && gpuUUIDs[node.GPUUUID] {
			return true
		}

		nodes[node.Name] = node.Name != ""
		nodeUIDs[node.UID] = node.UID != ""
		gpuUUIDs[node.GPUUUID] = node.GPUUUID != ""
	}
	podUIDs := map[string]bool{}
	podNodes := map[string]string{}
	podRanks := map[int]bool{}
	for _, pod := range result.Pods {
		if pod.Rank < 0 || pod.Rank > 1 || podUIDs[pod.UID] || podRanks[pod.Rank] ||
			pod.NodeName != "" && !nodes[pod.NodeName] {
			return true
		}
		podUIDs[pod.UID] = true
		podNodes[pod.UID] = pod.NodeName
		podRanks[pod.Rank] = true
	}
	ranks := map[int]bool{}
	for _, rank := range result.Ranks {
		if rank.Rank < 0 || rank.Rank > 1 || ranks[rank.Rank] ||
			rank.PodUID != "" && !podUIDs[rank.PodUID] || rank.NodeUID != "" && !nodeUIDs[rank.NodeUID] ||
			rank.PodUID != "" && rank.NodeName != "" && podNodes[rank.PodUID] != rank.NodeName {
			return true
		}
		ranks[rank.Rank] = true
	}
	exits := map[int]bool{}
	for _, exit := range result.RankExits {
		if exit.Rank < 0 || exit.Rank > 1 || exits[exit.Rank] {
			return true
		}
		exits[exit.Rank] = true
	}
	sampleRanks := map[int]bool{}
	for _, sample := range result.Measurements.Samples {
		if sample.Rank < 0 || sample.Rank > 1 || sampleRanks[sample.Rank] {
			return true
		}
		sampleRanks[sample.Rank] = true
	}
	return false
}

func invalidOwnedResourceSet(resources []ResourceRef) bool {
	if len(resources) == 0 {
		return false
	}
	if len(resources) != 6 {
		return len(resources) > 6
	}
	expected := map[string]bool{
		"v1|ConfigMap":                       false,
		"batch/v1|Job":                       false,
		"networking.k8s.io/v1|NetworkPolicy": false,
		"v1|Secret":                          false,
		"v1|Service":                         false,
		"v1|ServiceAccount":                  false,
	}
	uids := map[string]bool{}
	for _, resource := range resources {
		key := resource.APIVersion + "|" + resource.Kind
		if _, ok := expected[key]; !ok || expected[key] || resource.UID == "" || uids[resource.UID] {
			return true
		}
		expected[key] = true
		uids[resource.UID] = true
	}
	return false
}

func missingPassFields(result Result) []string {
	var missing []string
	requireString := func(field, value string) {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, field)
		}
	}
	requireTime := func(field string, value time.Time) {
		if value.IsZero() {
			missing = append(missing, field)
		}
	}
	requireString("run_id", result.RunID)
	if result.Attempt < 1 {
		missing = append(missing, "attempt")
	}
	requireString("workspace_id", result.WorkspaceID)
	requireString("cluster", result.Cluster)
	requireString("namespace", result.Namespace)
	requireTime("started_at", result.StartedAt)
	requireTime("admitted_at", result.AdmittedAt)
	requireTime("completed_at", result.CompletedAt)
	requireString("source.repository", result.Source.Repository)
	requireString("source.revision", result.Source.Revision)
	requireString("image.repository", result.Image.Repository)
	for field, digest := range imageDigests(result.Image) {
		requireString(field, digest)
	}
	if result.Image.SignatureTrustVerified == nil {
		missing = append(missing, "image.signature_trust_verified")
	}
	if result.Requested.Topology.NodeCount != 2 || result.Requested.Topology.PodCount != 2 ||
		result.Requested.Topology.RankCount != 2 || !result.Requested.Topology.DistinctHostname {
		missing = append(missing, "requested.topology")
	}
	requireString("requested.topology.gpu_model", result.Requested.Topology.GPUModel)
	requireString("requested.topology.site", result.Requested.Topology.Site)
	requireString("requested.topology.pool", result.Requested.Topology.Pool)
	resources := result.Requested.Resources
	if resources.CPURequestMilli <= 0 || resources.CPULimitMilli < resources.CPURequestMilli ||
		resources.MemoryRequestBytes <= 0 || resources.MemoryLimitBytes < resources.MemoryRequestBytes ||
		resources.GPUCount != 1 || resources.RDMACount != 1 || resources.SharedMemoryBytes <= 0 {
		missing = append(missing, "requested.resources")
	}
	requireString("requested.resources.gpu_resource", resources.GPUResource)
	requireString("requested.resources.rdma_resource", resources.RDMAResource)
	parameters := result.Requested.Parameters
	if parameters.WorldSize != 2 || parameters.ProcessesPerPod != 1 || parameters.Elements <= 0 ||
		parameters.Warmup <= 0 || parameters.Iterations <= 0 {
		missing = append(missing, "requested.parameters")
	}
	requireString("requested.parameters.operation", parameters.Operation)
	requireString("requested.parameters.data_type", parameters.DataType)
	requireString("actual.site", result.Actual.Site)
	requireString("actual.pool", result.Actual.Pool)
	if len(result.Actual.Nodes) != 2 {
		missing = append(missing, "actual.nodes")
	}
	for index, node := range result.Actual.Nodes {
		prefix := fmt.Sprintf("actual.nodes[%d].", index)
		requireString(prefix+"name", node.Name)
		requireString(prefix+"uid", node.UID)
		requireString(prefix+"gpu_model", node.GPUModel)
		requireString(prefix+"gpu_uuid", node.GPUUUID)
		requireString(prefix+"rdma_device", node.RDMADevice)
		requireString(prefix+"rdma_interface", node.RDMAInterface)
		requireString(prefix+"rdma_link_state", node.RDMALinkState)
	}
	if result.Placement.MatchesRequest == nil || result.Placement.DistinctNodes == nil {
		missing = append(missing, "placement")
	}
	if len(result.Pods) != 2 {
		missing = append(missing, "pods")
	}
	for index, pod := range result.Pods {
		prefix := fmt.Sprintf("pods[%d].", index)
		requireString(prefix+"name", pod.Name)
		requireString(prefix+"uid", pod.UID)
		requireString(prefix+"node_name", pod.NodeName)
		if pod.ExitCode == nil {
			missing = append(missing, prefix+"exit_code")
		}
	}
	if len(result.Ranks) != 2 {
		missing = append(missing, "ranks")
	}
	for index, rank := range result.Ranks {
		prefix := fmt.Sprintf("ranks[%d].", index)
		requireString(prefix+"pod_uid", rank.PodUID)
		requireString(prefix+"node_name", rank.NodeName)
		requireString(prefix+"node_uid", rank.NodeUID)
		if rank.PeerAuthVerified == nil || rank.MemlockSoftBytes == nil || rank.MemlockHardBytes == nil {
			missing = append(missing, prefix+"runtime")
		}
	}
	requireString("nccl.version", result.NCCL.Version)
	requireString("nccl.operation", result.NCCL.Operation)
	requireString("transport.backend", result.Transport.Backend)
	requireString("transport.nccl_net", result.Transport.NCCLNet)
	if len(result.Transport.Interfaces) != 2 || result.Transport.SocketFallbackObserved == nil ||
		result.Transport.PositiveIBEvidence == nil || len(result.Transport.Evidence) == 0 ||
		len(result.Transport.Environment) == 0 {
		missing = append(missing, "transport")
	}
	if len(result.Measurements.Samples) != 2 || result.Measurements.AlgBWGbps.Count != 2 ||
		result.Measurements.BusBWGbps.Count != 2 || result.Measurements.MaxRankTimeSeconds == nil {
		missing = append(missing, "measurements")
	}
	if result.Correctness.MaxError == nil || result.Correctness.ErrorCount == nil {
		missing = append(missing, "correctness")
	}
	if result.JobExitCode == nil || len(result.RankExits) != 2 {
		missing = append(missing, "exits")
	}
	for index, exit := range result.RankExits {
		if exit.ExitCode == nil {
			missing = append(missing, fmt.Sprintf("rank_exits[%d].exit_code", index))
		}
	}
	if result.Cleanup.State != CleanupComplete || result.Cleanup.StartedAt.IsZero() ||
		result.Cleanup.CompletedAt.IsZero() || len(result.Cleanup.OwnedResources) != 6 ||
		len(result.Cleanup.RemainingResources) != 0 {
		missing = append(missing, "cleanup")
	}
	for index, resource := range result.Cleanup.OwnedResources {
		prefix := fmt.Sprintf("cleanup.owned_resources[%d].", index)
		requireString(prefix+"api_version", resource.APIVersion)
		requireString(prefix+"kind", resource.Kind)
		requireString(prefix+"namespace", resource.Namespace)
		requireString(prefix+"name", resource.Name)
		requireString(prefix+"uid", resource.UID)
	}
	requiredEvidence := map[string]bool{
		"sanitized-manifest": false, "sanitized-logs": false, "placement": false,
		"image-receipt": false, "cleanup": false,
	}
	for _, evidence := range result.Evidence {
		if _, ok := requiredEvidence[evidence.Name]; ok && evidence.URI != "" &&
			evidence.SHA256 != "" && evidence.SizeBytes > 0 && !evidence.CapturedAt.IsZero() {
			requiredEvidence[evidence.Name] = true
		}
	}
	for name, present := range requiredEvidence {
		if !present {
			missing = append(missing, "evidence."+name)
		}
	}
	requireString("producer.name", result.Producer.Name)
	requireString("producer.version", result.Producer.Version)
	requireString("parser.name", result.Parser.Name)
	requireString("parser.version", result.Parser.Version)
	slices.Sort(missing)
	return slices.Compact(missing)
}

func imageDigests(image ImageProvenance) map[string]string {
	return map[string]string{
		"image.index_digest":              image.IndexDigest,
		"image.platform_digest":           image.PlatformDigest,
		"image.config_digest":             image.ConfigDigest,
		"image.sbom_manifest_digest":      image.SBOMManifestDigest,
		"image.sbom_layer_digest":         image.SBOMLayerDigest,
		"image.vex_manifest_digest":       image.VEXManifestDigest,
		"image.vex_layer_digest":          image.VEXLayerDigest,
		"image.signature_manifest_digest": image.SignatureManifestDigest,
		"image.signature_layer_digest":    image.SignatureLayerDigest,
	}
}

func normalizeErrors(errors []ValidationError) []ValidationError {
	result := append([]ValidationError(nil), errors...)
	slices.SortFunc(result, func(left, right ValidationError) int {
		if value := strings.Compare(string(left.Code), string(right.Code)); value != 0 {
			return value
		}
		if value := strings.Compare(left.Field, right.Field); value != 0 {
			return value
		}
		return strings.Compare(left.Message, right.Message)
	})
	return slices.CompactFunc(result, func(left, right ValidationError) bool {
		return left == right
	})
}

func validateTimeOrder(result Result) error {
	times := []struct {
		name string
		at   time.Time
	}{
		{"created_at", result.CreatedAt},
		{"admitted_at", result.AdmittedAt},
		{"started_at", result.StartedAt},
		{"completed_at", result.CompletedAt},
		{"observed_at", result.ObservedAt},
	}
	var previous time.Time
	var previousName string
	for _, item := range times {
		if item.at.IsZero() {
			continue
		}
		if !previous.IsZero() && item.at.Before(previous) {
			return fmt.Errorf("%s must not be before %s", item.name, previousName)
		}
		previous, previousName = item.at, item.name
	}
	if !result.Cleanup.StartedAt.IsZero() && !result.CompletedAt.IsZero() && result.Cleanup.StartedAt.Before(result.CompletedAt) {
		return fmt.Errorf("cleanup.started_at must not be before completed_at")
	}
	if !result.Cleanup.CompletedAt.IsZero() && result.Cleanup.CompletedAt.Before(result.Cleanup.StartedAt) {
		return fmt.Errorf("cleanup.completed_at must not be before cleanup.started_at")
	}
	if !result.Cleanup.CompletedAt.IsZero() && result.ObservedAt.Before(result.Cleanup.CompletedAt) {
		return fmt.Errorf("observed_at must not be before cleanup.completed_at")
	}
	return nil
}

func validateStats(measurements Measurements) error {
	if len(measurements.Samples) == 0 {
		return nil
	}
	alg := make([]float64, 0, len(measurements.Samples))
	bus := make([]float64, 0, len(measurements.Samples))
	for _, sample := range measurements.Samples {
		alg = append(alg, sample.AlgBWGbps)
		bus = append(bus, sample.BusBWGbps)
	}
	if !reflect.DeepEqual(measurements.AlgBWGbps, CalculateSummary(alg)) {
		return fmt.Errorf("measurements.algbw_gbps does not match samples")
	}
	if !reflect.DeepEqual(measurements.BusBWGbps, CalculateSummary(bus)) {
		return fmt.Errorf("measurements.busbw_gbps does not match samples")
	}
	maxElapsed := measurements.Samples[0].ElapsedSeconds
	for _, sample := range measurements.Samples[1:] {
		maxElapsed = math.Max(maxElapsed, sample.ElapsedSeconds)
	}
	if measurements.MaxRankTimeSeconds == nil ||
		math.Abs(*measurements.MaxRankTimeSeconds-maxElapsed) > 1e-9 {
		return fmt.Errorf("measurements.max_rank_time_seconds does not match samples")
	}
	return nil
}

func hasInvalidMeasurement(measurements Measurements) bool {
	for _, sample := range measurements.Samples {
		if sample.ElapsedSeconds <= 0 || sample.AlgBWGbps <= 0 || sample.BusBWGbps <= 0 ||
			!finite(sample.ElapsedSeconds) || !finite(sample.AlgBWGbps) || !finite(sample.BusBWGbps) {
			return true
		}
	}
	if measurements.MaxRankTimeSeconds != nil &&
		(*measurements.MaxRankTimeSeconds <= 0 || !finite(*measurements.MaxRankTimeSeconds)) {
		return true
	}
	return false
}

func hasNonFiniteResult(result Result) bool {
	values := []*float64{
		result.Measurements.MaxRankTimeSeconds, result.Correctness.MaxError,
		result.Measurements.AlgBWGbps.Min, result.Measurements.AlgBWGbps.Max,
		result.Measurements.AlgBWGbps.Mean, result.Measurements.AlgBWGbps.Median,
		result.Measurements.BusBWGbps.Min, result.Measurements.BusBWGbps.Max,
		result.Measurements.BusBWGbps.Mean, result.Measurements.BusBWGbps.Median,
	}
	for _, value := range values {
		if value != nil && !finite(*value) {
			return true
		}
	}
	for _, sample := range result.Measurements.Samples {
		if !finite(sample.ElapsedSeconds) || !finite(sample.AlgBWGbps) || !finite(sample.BusBWGbps) {
			return true
		}
	}
	return false
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0 && value.Location() == time.UTC
}

func containsSecretMaterial(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"-----begin private key", `"token":`, `"auth_key":`, `"hmac_key":`, "hmac-key:", "kubeconfig",
		"client-key-data", "client-certificate-data",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
