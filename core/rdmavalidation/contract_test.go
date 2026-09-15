// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvaluatePassAndGolden(t *testing.T) {
	result := validPassResult(t)
	assertStatus(t, &result, StatusPass, ReasonValidationPassed)
	assertGolden(t, "pass.golden.json", result)
}

func TestEvaluateSocketFallbackFailAndGolden(t *testing.T) {
	result := validPassResult(t)
	result.Transport.SocketFallbackObserved = boolPointer(true)
	assertStatus(t, &result, StatusFail, ReasonSocketFallbackObserved)
	assertGolden(t, "socket-fail.golden.json", result)
}

func TestEvaluateMissingEvidenceUnknownAndGolden(t *testing.T) {
	result := validPassResult(t)
	result.Actual.Nodes[0].GPUUUID = ""
	result.Actual.Nodes[1].RDMAInterface = ""
	for index := range result.Evidence {
		if result.Evidence[index].Name == "sanitized-manifest" {
			result.Evidence[index].SHA256 = ""
		}
	}
	assertStatus(t, &result, StatusUnknown, ReasonEvidenceIntegrityMissing)
	assertGolden(t, "missing-unknown.golden.json", result)
}

func TestValidateRejectsContractDrift(t *testing.T) {
	tests := map[string]func(*Result){
		"version":      func(result *Result) { result.Schema = "rdma-validation.v2" },
		"kind":         func(result *Result) { result.Kind = "tau.other" },
		"status enum":  func(result *Result) { result.Status = "maybe" },
		"cleanup enum": func(result *Result) { result.Cleanup.State = "done" },
		"reason enum":  func(result *Result) { result.Reason = "other" },
		"site mode enum": func(result *Result) {
			result.Requested.Topology.SiteMode = "other"
		},
		"site provider": func(result *Result) {
			enableUnboundedSiteTopology(result)
			result.Actual.SiteProvider = "other"
		},
		"site source key": func(result *Result) {
			enableUnboundedSiteTopology(result)
			result.Actual.Nodes[0].SiteSourceKey = "example.com/unbounded-cloud.io/site"
		},
		"non UTC": func(result *Result) {
			result.ObservedAt = result.ObservedAt.In(time.FixedZone("local", 0))
		},
		"invalid ID":       func(result *Result) { result.ValidationID = "Invalid/ID" },
		"invalid digest":   func(result *Result) { result.Image.PlatformDigest = "sha256:ABC" },
		"invalid revision": func(result *Result) { result.Source.Revision = "main" },
		"freshness window": func(result *Result) {
			result.StaleAfterSeconds = 3600
			result.ValidUntil = result.ObservedAt.Add(time.Hour)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			result := validPassResult(t)
			mutate(&result)
			if err := Validate(result); err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
		})
	}
}

func TestEvaluateTopologyAndRequiredEvidence(t *testing.T) {
	t.Run("duplicate topology fails", func(t *testing.T) {
		result := validPassResult(t)
		result.Actual.Nodes[1].UID = result.Actual.Nodes[0].UID
		assertStatus(t, &result, StatusFail, ReasonTopologyMismatch)
	})
	t.Run("pod node mismatch fails", func(t *testing.T) {
		result := validPassResult(t)
		result.Pods[1].NodeName = "other-node"
		assertStatus(t, &result, StatusFail, ReasonTopologyMismatch)
	})
	t.Run("rank identity must match corresponding pod", func(t *testing.T) {
		result := validPassResult(t)
		result.Ranks[1] = result.Ranks[0]
		result.Ranks[1].Rank = 1
		assertStatus(t, &result, StatusFail, ReasonTopologyMismatch)
	})
	t.Run("missing parameters is unknown", func(t *testing.T) {
		result := validPassResult(t)
		result.Requested.Parameters.Iterations = 0
		assertStatus(t, &result, StatusUnknown, ReasonMissingRequiredEvidence)
	})
	t.Run("missing exits is unknown", func(t *testing.T) {
		result := validPassResult(t)
		result.RankExits = nil
		assertStatus(t, &result, StatusUnknown, ReasonMissingRequiredEvidence)
	})
	t.Run("known nonzero pod exit fails", func(t *testing.T) {
		result := validPassResult(t)
		nonzero := 17
		result.Pods[1].ExitCode = &nonzero
		assertStatus(t, &result, StatusFail, ReasonNonzeroExit)
	})
	t.Run("cleanup incomplete fails", func(t *testing.T) {
		result := validPassResult(t)
		result.Cleanup.State = CleanupIncomplete
		result.Cleanup.RemainingResources = []ResourceRef{{
			APIVersion: "batch/v1", Kind: "Job", Namespace: "taugrid-rdma-diagnostic",
			Name: "e2e-nccl-rdma-2x1xh200", UID: "job-uid",
		}}
		assertStatus(t, &result, StatusFail, ReasonCleanupIncomplete)
	})
	t.Run("complete Unbounded topology passes", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		assertStatus(t, &result, StatusPass, ReasonValidationPassed)
	})
	t.Run("not applicable Unbounded topology passes", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Requested.Topology.Site = ""
		result.Requested.Topology.SiteMode = SiteTopologyNotApplicable
		result.Actual.Site = ""
		result.Actual.SiteMode = SiteTopologyNotApplicable
		for index := range result.Actual.Nodes {
			result.Actual.Nodes[index].Site = ""
			result.Actual.Nodes[index].SiteSourceKey = ""
		}
		assertStatus(t, &result, StatusPass, ReasonValidationPassed)
	})
	t.Run("requested Unbounded site without labels is unknown", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Actual.Site = ""
		result.Actual.SiteMode = SiteTopologyIncomplete
		for index := range result.Actual.Nodes {
			result.Actual.Nodes[index].Site = ""
			result.Actual.Nodes[index].SiteSourceKey = ""
		}
		assertStatus(t, &result, StatusUnknown, ReasonTopologyEvidenceIncomplete)
	})
	t.Run("requested site cannot be declared not applicable", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Requested.Topology.SiteMode = SiteTopologyNotApplicable
		result.Actual.Site = ""
		result.Actual.SiteMode = SiteTopologyNotApplicable
		for index := range result.Actual.Nodes {
			result.Actual.Nodes[index].Site = ""
			result.Actual.Nodes[index].SiteSourceKey = ""
		}
		assertStatus(t, &result, StatusUnknown, ReasonTopologyEvidenceIncomplete)
	})
	t.Run("partial Unbounded topology is unknown", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Actual.Site = ""
		result.Actual.SiteMode = SiteTopologyIncomplete
		result.Actual.Nodes[1].Site = ""
		result.Actual.Nodes[1].SiteSourceKey = ""
		assertStatus(t, &result, StatusUnknown, ReasonTopologyEvidenceIncomplete)
	})
	t.Run("conflicting Unbounded labels are unknown", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Actual.SiteMode = SiteTopologyIncomplete
		result.Actual.Nodes[0].SiteLabelConflict = true
		assertStatus(t, &result, StatusUnknown, ReasonTopologyEvidenceIncomplete)
	})
	t.Run("complete aggregate with node disagreement is unknown", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Actual.Nodes[1].Site = "eastus2"
		assertStatus(t, &result, StatusUnknown, ReasonTopologyEvidenceIncomplete)
	})
	t.Run("not applicable topology cannot contain node evidence", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Requested.Topology.Site = ""
		result.Requested.Topology.SiteMode = SiteTopologyNotApplicable
		result.Actual.Site = ""
		result.Actual.SiteMode = SiteTopologyNotApplicable
		assertStatus(t, &result, StatusUnknown, ReasonTopologyEvidenceIncomplete)
	})
	t.Run("complete Unbounded site mismatch fails", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Actual.Site = "eastus2"
		for index := range result.Actual.Nodes {
			result.Actual.Nodes[index].Site = "eastus2"
		}
		assertStatus(t, &result, StatusFail, ReasonPlacementMismatch)
	})
	t.Run("one Unbounded site may span unconstrained regions", func(t *testing.T) {
		result := validPassResult(t)
		enableUnboundedSiteTopology(&result)
		result.Requested.Topology.Region = ""
		result.Actual.Region = ""
		result.Actual.Nodes[0].Region = "eastus2euap"
		result.Actual.Nodes[1].Region = "westus3"
		assertStatus(t, &result, StatusPass, ReasonValidationPassed)
	})
}

func TestObservedParserReasonsPreserveFailVersusUnknown(t *testing.T) {
	for _, reason := range []ReasonCode{
		ReasonSocketFallbackObserved,
		ReasonIBTransportNotProven,
		ReasonTransportFailure,
		ReasonRuntimeError,
		ReasonPlacementMismatch,
		ReasonCorrectnessError,
		ReasonInvalidMeasurement,
	} {
		t.Run(string(reason), func(t *testing.T) {
			result := validPassResult(t)
			result.Errors = []ValidationError{{Code: reason, Field: "parser", Message: "proven violation"}}
			assertStatus(t, &result, StatusFail, reason)
		})
	}
	result := validPassResult(t)
	result.Errors = []ValidationError{{
		Code: ReasonParserRejected, Field: "parser", Message: "evidence was incomplete",
	}}
	assertStatus(t, &result, StatusUnknown, ReasonParserRejected)
}

func TestEvaluateRejectsInvalidMeasurements(t *testing.T) {
	for name, value := range map[string]float64{
		"zero": 0, "negative": -1, "nan": math.NaN(), "positive infinity": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			result := validPassResult(t)
			result.Measurements.Samples[0].AlgBWGbps = value
			result.Measurements.AlgBWGbps = CalculateSummary([]float64{
				result.Measurements.Samples[0].AlgBWGbps,
				result.Measurements.Samples[1].AlgBWGbps,
			})
			evaluation := Evaluate(result)
			if evaluation.Status != StatusFail || evaluation.Reason != ReasonInvalidMeasurement {
				t.Fatalf("Evaluate() = %s/%s, want fail/%s", evaluation.Status, evaluation.Reason, ReasonInvalidMeasurement)
			}
		})
	}
}

func TestCanonicalHashIsDeterministic(t *testing.T) {
	left := validPassResult(t)
	right := validPassResult(t)
	right.Actual.Nodes[0], right.Actual.Nodes[1] = right.Actual.Nodes[1], right.Actual.Nodes[0]
	right.Pods[0], right.Pods[1] = right.Pods[1], right.Pods[0]
	right.Transport.Interfaces[0], right.Transport.Interfaces[1] =
		right.Transport.Interfaces[1], right.Transport.Interfaces[0]
	right.Evidence[0], right.Evidence[4] = right.Evidence[4], right.Evidence[0]
	leftHash, err := Hash(left)
	if err != nil {
		t.Fatal(err)
	}

	rightHash, err := Hash(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftHash != rightHash {
		t.Fatalf("deterministic hash mismatch: %s != %s", leftHash, rightHash)
	}
}

func TestFinalizeCanonicalizesBeforeDerivingIndexedErrors(t *testing.T) {
	result := validPassResult(t)
	result.Actual.Nodes[0], result.Actual.Nodes[1] = result.Actual.Nodes[1], result.Actual.Nodes[0]
	result.Actual.Nodes[0].GPUUUID = ""

	if err := Finalize(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUnknown {
		t.Fatalf("status = %s, want %s", result.Status, StatusUnknown)
	}
	if result.Actual.Nodes[0].Name != "h200-a" || result.Actual.Nodes[1].Name != "h200-b" {
		t.Fatalf("nodes were not canonicalized before evaluation: %+v", result.Actual.Nodes)
	}
	if _, err := MarshalCanonical(result); err != nil {
		t.Fatalf("MarshalCanonical() rejected finalized incomplete result: %v", err)
	}
}

func TestValidateRejectsDuplicateOrUnexpectedEvidence(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		result := validPassResult(t)
		result.Evidence = append(result.Evidence, result.Evidence[0])
		if err := Finalize(&result); err == nil {
			t.Fatal("Finalize() accepted duplicate evidence names")
		}
	})
	t.Run("unexpected", func(t *testing.T) {
		result := validPassResult(t)
		result.Evidence[0].Name = "raw-secret-dump"
		if err := Finalize(&result); err == nil {
			t.Fatal("Finalize() accepted an unexpected evidence name")
		}
	})
}

func TestCanonicalJSONUsesUTCRFC3339Nano(t *testing.T) {
	result := validPassResult(t)
	raw, err := MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"created_at": "2026-09-14T20:00:00.123456789Z"`) {
		t.Fatalf("canonical JSON timestamp is not UTC RFC3339Nano:\n%s", raw)
	}
}

func TestWriteArtifactIsAtomicAndImmutable(t *testing.T) {
	result := validPassResult(t)
	path := filepath.Join(t.TempDir(), "rdma-validation", result.ValidationID+".json")
	info, err := WriteArtifact(path, result)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes != int64(len(raw)) || info.SHA256 != digestBytes(raw) {
		t.Fatalf("artifact info = %+v, bytes = %d/%s", info, len(raw), digestBytes(raw))
	}
	if _, err := WriteArtifact(path, result); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("second WriteArtifact() error = %v, want immutable replacement refusal", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary artifact files remain: %v", matches)
	}
}

func TestWriteArtifactRequiresCompletedCleanup(t *testing.T) {
	for name, mutate := range map[string]func(*Result){
		"missing start": func(result *Result) {
			result.Cleanup.StartedAt = time.Time{}
		},
		"missing completion": func(result *Result) {
			result.Cleanup.CompletedAt = time.Time{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := validPassResult(t)
			mutate(&result)
			if _, err := WriteArtifact(filepath.Join(t.TempDir(), "result.json"), result); err == nil ||
				!strings.Contains(err.Error(), "before cleanup completes") {
				t.Fatalf("WriteArtifact() error = %v, want cleanup completion rejection", err)
			}
		})
	}
}

func TestLifecycleUsesExistingRunStatesAndRequiresFinalArtifact(t *testing.T) {
	result := validPassResult(t)
	pending := result
	pending.StartedAt = time.Time{}
	lifecycle, err := pending.Lifecycle(nil)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != RunStatePending || !lifecycle.CompletedAt.IsZero() {
		t.Fatalf("pending lifecycle = %+v", lifecycle)
	}

	lifecycle, err = result.Lifecycle(nil)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != RunStateRunning || !lifecycle.CompletedAt.IsZero() {
		t.Fatalf("running lifecycle = %+v", lifecycle)
	}

	raw, err := MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	link := ArtifactLink{
		URI:         "file:///immutable/rdma-validation/result.json",
		SHA256:      digestBytes(raw),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
	lifecycle, err = result.Lifecycle(&link)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != RunStateSucceeded || !lifecycle.CompletedAt.Equal(link.FinalizedAt) {
		t.Fatalf("succeeded lifecycle = %+v", lifecycle)
	}

	failed := result
	failed.Transport.SocketFallbackObserved = boolPointer(true)
	assertStatus(t, &failed, StatusFail, ReasonSocketFallbackObserved)
	failedLink := artifactLinkForResult(t, failed)
	lifecycle, err = failed.Lifecycle(&failedLink)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != RunStateFailed {
		t.Fatalf("failed lifecycle = %+v", lifecycle)
	}

	unknown := result
	unknown.Evidence[0].SHA256 = ""
	assertStatus(t, &unknown, StatusUnknown, ReasonEvidenceIntegrityMissing)
	unknownLink := artifactLinkForResult(t, unknown)
	lifecycle, err = unknown.Lifecycle(&unknownLink)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != RunStateFailed {
		t.Fatalf("unknown terminal lifecycle = %+v, want failed", lifecycle)
	}
}

func TestLifecycleRejectsArtifactFinalizedBeforeCleanup(t *testing.T) {
	result := validPassResult(t)
	link := artifactLinkForResult(t, result)
	link.FinalizedAt = result.Cleanup.CompletedAt.Add(-time.Nanosecond)
	if _, err := result.Lifecycle(&link); err == nil ||
		!strings.Contains(err.Error(), "precedes cleanup completion") {
		t.Fatalf("Lifecycle() error = %v, want ordering rejection", err)
	}
}

func TestLifecycleRejectsArtifactContentMismatch(t *testing.T) {
	result := validPassResult(t)
	link := artifactLinkForResult(t, result)
	link.SHA256 = "sha256:" + strings.Repeat("f", 64)
	if _, err := result.Lifecycle(&link); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Lifecycle() error = %v, want artifact content mismatch", err)
	}
}

func TestArtifactAndEvidenceRejectCredentialMaterial(t *testing.T) {
	result := validPassResult(t)
	result.Errors = []ValidationError{{
		Code: ReasonRuntimeError, Message: `{"token":"do-not-write"}`,
	}}
	result.Status = StatusUnknown
	result.Reason = ReasonRuntimeError
	if _, err := MarshalCanonical(result); err == nil {
		t.Fatal("MarshalCanonical() accepted token material")
	}
	if _, err := NewEvidenceRef("logs", "file://logs", []byte("kubeconfig: secret"), time.Now().UTC()); err == nil {
		t.Fatal("NewEvidenceRef() accepted kubeconfig material")
	}
	result = validPassResult(t)
	result.Transport.Environment["AWS_SECRET_ACCESS_KEY"] = "do-not-record"
	if err := Finalize(&result); err == nil {
		t.Fatal("Finalize() accepted a non-allowlisted environment variable")
	}
}

func TestSummaryMetricsAreBoundedAndOmitUnknownMeasurements(t *testing.T) {
	result := validPassResult(t)
	metrics := result.SummaryMetrics()
	if len(metrics) != 8 {
		t.Fatalf("SummaryMetrics() count = %d, want 8", len(metrics))
	}
	if metrics[0].Name != MetricStatus || metrics[0].Value != 1 {
		t.Fatalf("status metric = %+v", metrics[0])
	}
	for _, metric := range metrics {
		if len(metric.Tags) > 7 {
			t.Fatalf("metric %s has unbounded tags: %v", metric.Name, metric.Tags)
		}
		for key, want := range map[string]string{
			MetricValidationIDTag:     result.ValidationID,
			MetricSchemaTag:           SchemaVersion,
			MetricKindTag:             Kind,
			MetricValidationStatusTag: string(result.Status),
			MetricValidationReasonTag: string(result.Reason),
		} {
			if metric.Tags[key] != want {
				t.Fatalf("metric %s tag %s = %q, want %q", metric.Name, key, metric.Tags[key], want)
			}
		}
		for _, forbidden := range []string{"node", "gpu", "interface"} {
			if _, exists := metric.Tags[forbidden]; exists {
				t.Fatalf("metric %s contains high-cardinality tag %q", metric.Name, forbidden)
			}
		}
	}

	failed := validPassResult(t)
	failed.Transport.SocketFallbackObserved = boolPointer(true)
	assertStatus(t, &failed, StatusFail, ReasonSocketFallbackObserved)
	if metric := failed.SummaryMetrics()[0]; metric.Value != -1 {
		t.Fatalf("failed status metric = %v, want -1", metric.Value)
	}

	result.Measurements = Measurements{}
	result.Correctness = Correctness{}
	result.Transport.PositiveIBEvidence = nil
	result.Transport.SocketFallbackObserved = nil
	result.Cleanup.State = CleanupUnknown
	assertStatus(t, &result, StatusUnknown, ReasonMissingRequiredEvidence)
	metrics = result.SummaryMetrics()
	if metrics[0].Value != 0 {
		t.Fatalf("unknown status metric = %v, want 0", metrics[0].Value)
	}
	for _, metric := range metrics {
		switch metric.Name {
		case MetricAlgBWGbps, MetricBusBWGbps, MetricMaxError, MetricNetIBObserved,
			MetricSocketFallbackObserved, MetricCleanupComplete:
			t.Fatalf("unknown metric %s must be omitted", metric.Name)
		}
	}
}

func TestSummaryMetricsLinkDeterministicallyToArtifact(t *testing.T) {
	result := validPassResult(t)
	link := artifactLinkForResult(t, result)
	metrics, err := result.SummaryMetricsForArtifact(link)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metrics {
		if metric.ArtifactURI != link.URI || metric.ArtifactSHA256 != link.SHA256 {
			t.Fatalf("metric %s artifact link = %q/%q", metric.Name, metric.ArtifactURI, metric.ArtifactSHA256)
		}
	}
}

func artifactLinkForResult(t *testing.T, result Result) ArtifactLink {
	t.Helper()
	raw, err := MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	return ArtifactLink{
		URI:         "file:///immutable/rdma-validation/result.json",
		SHA256:      digestBytes(raw),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
}

func TestFreshnessDoesNotMutateHistoricalStatus(t *testing.T) {
	result := validPassResult(t)
	status := result.Status
	if !IsFresh(result, result.ValidUntil) {
		t.Fatal("result should remain fresh at valid_until")
	}
	if IsFresh(result, result.ValidUntil.Add(time.Nanosecond)) {
		t.Fatal("result should be stale after valid_until")
	}
	if result.Status != status {
		t.Fatalf("freshness mutated historical status from %s to %s", status, result.Status)
	}
}

func validPassResult(t *testing.T) Result {
	t.Helper()
	created := time.Date(2026, time.September, 14, 20, 0, 0, 123456789, time.UTC)
	admitted := created.Add(time.Second)
	started := admitted.Add(2 * time.Second)
	completed := admitted.Add(10 * time.Second)
	cleanupStarted := completed.Add(time.Second)
	cleanupCompleted := cleanupStarted.Add(2 * time.Second)
	observed := cleanupCompleted
	zero, errorCount := 0, 0
	memlock := int64(4_206_821_376)
	yes, no := true, false
	maxRankTime := 0.2
	samples := []BandwidthMeasurement{
		{Rank: 0, ElapsedSeconds: 0.18, AlgBWGbps: 13.5, BusBWGbps: 13.5},
		{Rank: 1, ElapsedSeconds: maxRankTime, AlgBWGbps: 12.5, BusBWGbps: 12.5},
	}

	digest := "sha256:" + strings.Repeat("a", 64)
	evidence := make([]EvidenceRef, 0, 5)
	for index, name := range []string{"sanitized-manifest", "sanitized-logs", "placement", "image-receipt", "cleanup"} {
		evidence = append(evidence, EvidenceRef{
			Name: name, URI: "file://evidence/" + name + ".json",
			SHA256:    "sha256:" + strings.Repeat(string(rune('b'+index)), 64),
			SizeBytes: int64(100 + index), CapturedAt: observed,
		})
	}
	result := Result{
		Schema: SchemaVersion, Kind: Kind,
		ValidationID: "nccl-rdma-0123456789abcdef0123456789abcdef",
		RunID:        "nccl-rdma-0123456789abcdef0123456789abcdef", Attempt: 1,
		WorkspaceID: "taugrid-rdma", Cluster: "h200-validation", Namespace: "taugrid-rdma-diagnostic",
		ProjectID: "taugrid", ExperimentID: "rdma-validation", RunGroupID: "manual",
		CreatedAt: created, StartedAt: started, AdmittedAt: admitted, CompletedAt: completed,
		ObservedAt: observed, StaleAfterSeconds: DefaultStaleAfterSeconds, ValidUntil: observed.Add(24 * time.Hour),
		Source: Source{Repository: "Azure/taugrid", Revision: strings.Repeat("1", 40)},
		Image: ImageProvenance{
			Repository: "nvcr.io/nvidia/pytorch", IndexDigest: digest, PlatformDigest: digest,
			ConfigDigest: digest, SBOMManifestDigest: digest, SBOMLayerDigest: digest,
			VEXManifestDigest: digest, VEXLayerDigest: digest, SignatureManifestDigest: digest,
			SignatureLayerDigest: digest, SignatureTrustVerified: &no,
		},
		Requested: Requested{
			Topology: RequestedTopology{
				NodeCount: 2, PodCount: 2, RankCount: 2, DistinctHostname: true,
				Site: "westus3", Pool: "h200pool", GPUModel: "NVIDIA H200",
			},
			Resources: RequestedResources{
				CPURequestMilli: 4000, CPULimitMilli: 8000,
				MemoryRequestBytes: 16 << 30, MemoryLimitBytes: 32 << 30,
				GPUResource: "nvidia.com/gpu", GPUCount: 1,
				RDMAResource: "rdma/rdma_shared_device_a", RDMACount: 1, SharedMemoryBytes: 16 << 30,
			},
			Parameters: BenchmarkParameters{
				WorldSize: 2, ProcessesPerPod: 1, Elements: 16_777_216,
				Warmup: 5, Iterations: 20, Operation: "all_reduce", DataType: "float32",
			},
		},
		Actual: Actual{
			Site: "westus3", Pool: "h200pool",
			Nodes: []NodeResult{
				{Name: "h200-a", UID: "node-uid-a", GPUModel: "NVIDIA H200", GPUUUID: "GPU-aaaaaaaa", RDMADevice: "mlx5_0", RDMAInterface: "eth1", RDMALinkState: "ACTIVE"},
				{Name: "h200-b", UID: "node-uid-b", GPUModel: "NVIDIA H200", GPUUUID: "GPU-bbbbbbbb", RDMADevice: "mlx5_1", RDMAInterface: "eth2", RDMALinkState: "ACTIVE"},
			},
		},
		Placement: Placement{MatchesRequest: &yes, DistinctNodes: &yes},
		Pods: []PodResult{
			{Name: "probe-0", UID: "pod-uid-0", NodeName: "h200-a", Rank: 0, ExitCode: &zero},
			{Name: "probe-1", UID: "pod-uid-1", NodeName: "h200-b", Rank: 1, ExitCode: &zero},
		},
		Ranks: []RankResult{
			{Rank: 0, PodUID: "pod-uid-0", NodeName: "h200-a", NodeUID: "node-uid-a", PeerAuthVerified: &yes, MemlockSoftBytes: &memlock, MemlockHardBytes: &memlock},
			{Rank: 1, PodUID: "pod-uid-1", NodeName: "h200-b", NodeUID: "node-uid-b", PeerAuthVerified: &yes, MemlockSoftBytes: &memlock, MemlockHardBytes: &memlock},
		},
		NCCL: NCCL{Version: "2.28.8", Operation: "all_reduce"},
		Transport: Transport{
			Backend: "nccl", NCCLNet: "IB", Interfaces: []string{"eth1", "eth2"},
			SocketFallbackObserved: &no, PositiveIBEvidence: &yes,
			Evidence: []string{"NCCL INFO NET/IB : Using mlx5_0", "NCCL INFO NET/IB : Using mlx5_1"},
			Environment: map[string]string{
				"NCCL_DEBUG": "INFO", "NCCL_DEBUG_SUBSYS": "INIT,NET", "NCCL_IB_DISABLE": "0",
				"TAUGRID_BACKEND": "nccl", "TAUGRID_ELEMENTS": "16777216",
				"TAUGRID_ITERATIONS": "20", "TAUGRID_LIVE_RDMA": "1", "TAUGRID_WARMUP": "5",
			},
		},
		Measurements: NewMeasurements(samples, maxRankTime),
		Correctness:  Correctness{MaxError: floatPointer(0), ErrorCount: &errorCount},
		JobExitCode:  &zero, RankExits: []RankExit{{Rank: 0, ExitCode: &zero}, {Rank: 1, ExitCode: &zero}},
		Cleanup: Cleanup{
			State: CleanupComplete, StartedAt: cleanupStarted, CompletedAt: cleanupCompleted,
			OwnedResources: []ResourceRef{
				{APIVersion: "v1", Kind: "ConfigMap", Namespace: "taugrid-rdma-diagnostic", Name: "probe", UID: "configmap-uid"},
				{APIVersion: "batch/v1", Kind: "Job", Namespace: "taugrid-rdma-diagnostic", Name: "job", UID: "job-uid"},
				{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Namespace: "taugrid-rdma-diagnostic", Name: "network", UID: "network-uid"},
				{APIVersion: "v1", Kind: "Secret", Namespace: "taugrid-rdma-diagnostic", Name: "auth", UID: "secret-uid"},
				{APIVersion: "v1", Kind: "Service", Namespace: "taugrid-rdma-diagnostic", Name: "rank0", UID: "service-uid"},
				{APIVersion: "v1", Kind: "ServiceAccount", Namespace: "taugrid-rdma-diagnostic", Name: "runner", UID: "serviceaccount-uid"},
			},
			RemainingResources: []ResourceRef{},
		},
		Evidence: evidence,
		Producer: ComponentVersion{Name: "taugrid-e2e", Version: "1.0.0"},
		Parser:   ComponentVersion{Name: "torchrun-rdma-parser", Version: "1.0.0"},
	}
	if err := Finalize(&result); err != nil {
		t.Fatalf("Finalize(valid result): %v", err)
	}
	return result
}

func enableUnboundedSiteTopology(result *Result) {
	result.Requested.Topology.SiteProvider = UnboundedSiteProvider
	result.Requested.Topology.SiteMode = SiteTopologyComplete
	result.Requested.Topology.Region = "westus3"
	result.Actual.SiteProvider = UnboundedSiteProvider
	result.Actual.SiteMode = SiteTopologyComplete
	result.Actual.Region = "westus3"
	for index := range result.Actual.Nodes {
		result.Actual.Nodes[index].Site = result.Actual.Site
		result.Actual.Nodes[index].SiteSourceKey = UnboundedSiteLabelKey
		result.Actual.Nodes[index].Region = result.Actual.Region
		result.Actual.Nodes[index].Pool = result.Actual.Pool
	}
}

func assertStatus(t *testing.T, result *Result, status Status, reason ReasonCode) {
	t.Helper()
	if err := Finalize(result); err != nil {
		t.Fatalf("Finalize(): %v", err)
	}
	if result.Status != status || result.Reason != reason {
		t.Fatalf("Finalize() = %s/%s, want %s/%s; errors=%v", result.Status, result.Reason, status, reason, result.Errors)
	}
}

func assertGolden(t *testing.T, name string, result Result) {
	t.Helper()
	raw, err := MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("%s does not match canonical result", name)
	}
}

func boolPointer(value bool) *bool        { return &value }
func floatPointer(value float64) *float64 { return &value }
