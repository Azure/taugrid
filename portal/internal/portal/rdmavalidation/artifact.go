// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corevalidation "github.com/Azure/taugrid/core/rdmavalidation"
)

const maxArtifactBytes = 8 << 20

func DecodeArtifact(raw []byte, metadata ArtifactMetadata, now time.Time) (Detail, error) {
	if len(raw) == 0 || len(raw) > maxArtifactBytes {
		return Detail{}, ErrMalformedArtifact
	}
	if metadata.ContentType != "" && metadata.ContentType != corevalidation.ArtifactContentType {
		return Detail{}, fmt.Errorf("%w: content type %q", ErrArtifactIntegrity, metadata.ContentType)
	}
	if metadata.SizeBytes > 0 && int64(len(raw)) != metadata.SizeBytes {
		return Detail{}, fmt.Errorf("%w: size mismatch", ErrArtifactIntegrity)
	}
	if metadata.SHA256 == "" || digest(raw) != metadata.SHA256 {
		return Detail{}, fmt.Errorf("%w: digest mismatch", ErrArtifactIntegrity)
	}

	var envelope struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Detail{}, fmt.Errorf("%w: %v", ErrMalformedArtifact, err)
	}
	if envelope.Schema != corevalidation.SchemaVersion {
		return Detail{}, fmt.Errorf("%w: %q", ErrUnsupportedSchema, envelope.Schema)
	}

	var result corevalidation.Result
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Detail{}, fmt.Errorf("%w: %v", ErrMalformedArtifact, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Detail{}, err
	}
	if err := corevalidation.Validate(result); err != nil {
		return Detail{}, fmt.Errorf("%w: %v", ErrMalformedArtifact, err)
	}
	canonical, err := corevalidation.MarshalCanonical(result)
	if err != nil {
		return Detail{}, fmt.Errorf("%w: %v", ErrMalformedArtifact, err)
	}
	if !bytes.Equal(raw, canonical) {
		return Detail{}, fmt.Errorf("%w: artifact is not canonical JSON", ErrArtifactIntegrity)
	}
	if metadata.ValidationID != "" && metadata.ValidationID != result.ValidationID {
		return Detail{}, fmt.Errorf("%w: validation ID", ErrScopeMismatch)
	}
	if metadata.RunID != "" && metadata.RunID != result.RunID {
		return Detail{}, fmt.Errorf("%w: run ID", ErrScopeMismatch)
	}
	if metadata.WorkspaceID != "" && metadata.WorkspaceID != result.WorkspaceID {
		return Detail{}, fmt.Errorf("%w: workspace", ErrScopeMismatch)
	}
	if metadata.SizeBytes == 0 {
		metadata.SizeBytes = int64(len(raw))
	}
	return mapResult(result, metadata, now.UTC()), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("%w: trailing JSON value", ErrMalformedArtifact)
	}
	return fmt.Errorf("%w: %v", ErrMalformedArtifact, err)
}

func mapResult(result corevalidation.Result, metadata ArtifactMetadata, now time.Time) Detail {
	historical := string(result.Status)
	state, freshness, age := DeriveState(
		now, result.ObservedAt, result.ValidUntil, historical, false, true,
	)
	messageSize := messageSizeBytes(result.Requested.Parameters.Elements, result.Requested.Parameters.DataType)
	messageSizes := []int64(nil)
	if messageSize != nil {
		messageSizes = []int64{*messageSize}
	}
	rankExits := make(map[int]*int, len(result.RankExits))
	for _, exit := range result.RankExits {
		rankExits[exit.Rank] = exit.ExitCode
	}
	nodes := make([]Node, 0, len(result.Actual.Nodes))
	rdmaDevices := make([]string, 0, len(result.Actual.Nodes))
	for _, node := range result.Actual.Nodes {
		devices := []RDMADevice(nil)
		if node.RDMADevice != "" || node.RDMAInterface != "" || node.RDMALinkState != "" {
			devices = []RDMADevice{{
				ResourceName: result.Requested.Resources.RDMAResource,
				Device:       node.RDMADevice, Interface: node.RDMAInterface, State: node.RDMALinkState,
			}}
		}
		if node.RDMADevice != "" {
			rdmaDevices = append(rdmaDevices, node.RDMADevice)
		}
		gpus := []string(nil)
		if node.GPUUUID != "" {
			gpus = []string{node.GPUUUID}
		}
		nodeSite, nodePool := node.Site, node.Pool
		if result.Actual.SiteMode == "" {
			nodeSite, nodePool = result.Actual.Site, result.Actual.Pool
		}
		nodes = append(nodes, Node{
			Name: node.Name, UID: node.UID,
			Site: nodeSite, SiteSourceKey: node.SiteSourceKey, SiteLabelConflict: node.SiteLabelConflict,
			Region: node.Region, Pool: nodePool,
			GPUModel: node.GPUModel, GPUUUIDs: gpus, RDMADevices: devices,
		})
	}
	pods := make([]Pod, 0, len(result.Pods))
	for _, pod := range result.Pods {
		pods = append(pods, Pod{
			Name: pod.Name, UID: pod.UID, Node: pod.NodeName, ExitCode: pod.ExitCode,
		})
	}
	ranks := make([]Rank, 0, len(result.Ranks))
	for _, rank := range result.Ranks {
		ranks = append(ranks, Rank{
			Rank: intPointer(rank.Rank), PodUID: rank.PodUID, Node: rank.NodeName, NodeUID: rank.NodeUID,
			PeerAuthenticated: rank.PeerAuthVerified, MemlockSoftBytes: rank.MemlockSoftBytes,
			MemlockHardBytes: rank.MemlockHardBytes, ExitCode: rankExits[rank.Rank],
		})
	}
	measurements := make([]Measurement, 0, len(result.Measurements.Samples))
	for _, sample := range result.Measurements.Samples {
		measurements = append(measurements, Measurement{
			Rank: intPointer(sample.Rank), MessageSizeBytes: messageSize,
			Iterations:     intPointer(result.Requested.Parameters.Iterations),
			ElapsedSeconds: floatPointer(sample.ElapsedSeconds),
			AlgBWGbps:      floatPointer(sample.AlgBWGbps), BusBWGbps: floatPointer(sample.BusBWGbps),
		})
	}
	errorsView := make([]ResultError, 0, len(result.Errors))
	for _, observed := range result.Errors {
		errorsView = append(errorsView, ResultError{
			Stage: observed.Field, Code: string(observed.Code), Message: observed.Message,
		})
	}
	exits := make([]RankExit, 0, len(result.RankExits))
	for _, exit := range result.RankExits {
		exits = append(exits, RankExit{Rank: intPointer(exit.Rank), Code: exit.ExitCode})
	}
	evidence := make([]Evidence, 0, len(result.Evidence))
	for _, item := range result.Evidence {
		evidence = append(evidence, Evidence{
			Name: item.Name, URI: item.URI, SHA256: item.SHA256,
			SizeBytes: int64Pointer(item.SizeBytes), CapturedAt: formatTime(item.CapturedAt),
		})
	}
	duration := durationSeconds(result.StartedAt, result.CompletedAt)
	detail := Detail{
		Validation: Validation{
			ValidationID: result.ValidationID, RunID: result.RunID, RunAttempt: intPointer(result.Attempt),
			State: state, HistoricalStatus: &historical, Freshness: freshness,
			ReasonCode: string(result.Reason), Reason: reasonText(result.Reason),
			WorkspaceID: result.WorkspaceID, Cluster: result.Cluster, Namespace: result.Namespace,
			Project: result.ProjectID, ExperimentID: result.ExperimentID, RunGroupID: result.RunGroupID,
			CreatedAt: formatTime(result.CreatedAt), StartedAt: formatTime(result.StartedAt),
			AdmittedAt: formatTime(result.AdmittedAt), CompletedAt: formatTime(result.CompletedAt),
			ObservedAt: formatTime(result.ObservedAt), ValidUntil: formatTime(result.ValidUntil),
			StaleAfterSeconds: int64Pointer(result.StaleAfterSeconds), AgeSeconds: age,
			Requested: mapRequested(result, messageSizes), Actual: &Actual{
				Site: result.Actual.Site, SiteProvider: result.Actual.SiteProvider,
				SiteMode: SiteTopologyMode(result.Actual.SiteMode), Region: result.Actual.Region,
				Pool: result.Actual.Pool, Nodes: nodes,
			},
			Placement: &Placement{
				DistinctNodes: result.Placement.DistinctNodes, MatchesRequest: result.Placement.MatchesRequest,
			},
			Source: &Source{
				Repository: result.Source.Repository, Revision: result.Source.Revision,
				ImageRepository: result.Image.Repository, ImageIndexDigest: result.Image.IndexDigest,
				ImagePlatformDigest: result.Image.PlatformDigest, ImageConfigDigest: result.Image.ConfigDigest,
				SBOMManifestDigest: result.Image.SBOMManifestDigest, SBOMLayerDigest: result.Image.SBOMLayerDigest,
				VEXManifestDigest: result.Image.VEXManifestDigest, VEXLayerDigest: result.Image.VEXLayerDigest,
				SignatureManifestDigest: result.Image.SignatureManifestDigest,
				SignatureLayerDigest:    result.Image.SignatureLayerDigest,
				SignatureTrustVerified:  result.Image.SignatureTrustVerified,
			},
			Collective: &Collective{
				Library: result.Transport.Backend, Version: result.NCCL.Version, Operation: result.NCCL.Operation,
			},
			Transport: &Transport{
				Backend: result.Transport.Backend, NCCLNet: result.Transport.NCCLNet,
				Interfaces: result.Transport.Interfaces, RDMADevices: rdmaDevices,
				SocketFallbackDetected: result.Transport.SocketFallbackObserved,
				IBPositiveEvidence:     result.Transport.PositiveIBEvidence,
				Evidence:               result.Transport.Evidence, Environment: result.Transport.Environment,
			},
			Parameters: &Parameters{
				WorldSize:       intPointer(result.Requested.Parameters.WorldSize),
				ProcessesPerPod: intPointer(result.Requested.Parameters.ProcessesPerPod),
				Elements:        int64Pointer(result.Requested.Parameters.Elements),
				DataType:        result.Requested.Parameters.DataType, Operation: result.Requested.Parameters.Operation,
				MessageSizesBytes: messageSizes,
				WarmupIterations:  intPointer(result.Requested.Parameters.Warmup),
				Iterations:        intPointer(result.Requested.Parameters.Iterations),
			},
			Summary: &BandwidthSummary{
				AlgBWGbps: mapDistribution(result.Measurements.AlgBWGbps),
				BusBWGbps: mapDistribution(result.Measurements.BusBWGbps),
			},
			Correctness: mapCorrectness(result.Correctness), DurationSeconds: duration,
			Cleanup: &Cleanup{
				Status: string(result.Cleanup.State), StartedAt: formatTime(result.Cleanup.StartedAt),
				CompletedAt:        formatTime(result.Cleanup.CompletedAt),
				OwnedResources:     resourceNames(result.Cleanup.OwnedResources),
				RemainingResources: resourceNames(result.Cleanup.RemainingResources),
				Reason:             cleanupReason(result),
			},
			ArtifactVerification: &ArtifactVerification{
				State: "verified", ContentType: corevalidation.ArtifactContentType,
				URI: metadata.URI, SHA256: metadata.SHA256, SizeBytes: int64Pointer(metadata.SizeBytes),
				VerifiedAt: formatTime(now),
			},
		},
		SchemaVersion: result.Schema, Kind: result.Kind, Pods: pods, Ranks: ranks,
		Measurements: measurements, Errors: errorsView, JobExitCode: result.JobExitCode,
		RankExitCodes: exits, Evidence: evidence,
		Producer: &Component{Name: result.Producer.Name, Version: result.Producer.Version},
		Parser:   &Component{Name: result.Parser.Name, Version: result.Parser.Version},
	}
	return detail
}

func mapRequested(result corevalidation.Result, messageSizes []int64) *Requested {
	topology := result.Requested.Topology
	parameters := result.Requested.Parameters
	resources := result.Requested.Resources
	requested := &Requested{
		NodeCount: intPointer(topology.NodeCount), PodCount: intPointer(topology.PodCount),
		RankCount: intPointer(topology.RankCount), DistinctHostname: boolPointer(topology.DistinctHostname),
		Site: topology.Site, SiteProvider: topology.SiteProvider,
		SiteMode: SiteTopologyMode(topology.SiteMode), Region: topology.Region,
		Pool: topology.Pool, GPUModel: topology.GPUModel,
		GPUResource: resources.GPUResource, RDMAResource: resources.RDMAResource,
		RDMAPerPod: intPointer(int(resources.RDMACount)), MessageSizesBytes: messageSizes,
		WarmupIterations: intPointer(parameters.Warmup), Iterations: intPointer(parameters.Iterations),
	}
	if topology.NodeCount > 0 && topology.RankCount%topology.NodeCount == 0 {
		requested.RanksPerNode = intPointer(topology.RankCount / topology.NodeCount)
	}
	if parameters.ProcessesPerPod > 0 && resources.GPUCount%int64(parameters.ProcessesPerPod) == 0 {
		requested.GPUsPerRank = intPointer(int(resources.GPUCount / int64(parameters.ProcessesPerPod)))
	}
	return requested
}

func mapDistribution(stats corevalidation.SummaryStats) *Distribution {
	if stats.Count == 0 && stats.Min == nil && stats.Max == nil && stats.Mean == nil && stats.Median == nil {
		return nil
	}
	return &Distribution{Min: stats.Min, Max: stats.Max, Mean: stats.Mean, Median: stats.Median}
}

func mapCorrectness(correctness corevalidation.Correctness) *Correctness {
	var passed *bool
	if correctness.MaxError != nil && correctness.ErrorCount != nil {
		value := *correctness.MaxError == 0 && *correctness.ErrorCount == 0
		passed = &value
	}
	var count *int64
	if correctness.ErrorCount != nil {
		value := int64(*correctness.ErrorCount)
		count = &value
	}
	return &Correctness{Passed: passed, MaxError: correctness.MaxError, ErrorCount: count}
}

func messageSizeBytes(elements int64, dataType string) *int64 {
	if elements <= 0 {
		return nil
	}
	var width int64
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "float64", "fp64", "double", "int64", "uint64":
		width = 8
	case "float32", "fp32", "float", "int32", "uint32":
		width = 4
	case "float16", "fp16", "half", "bfloat16", "bf16", "int16", "uint16":
		width = 2
	case "int8", "uint8", "bool":
		width = 1
	default:
		return nil
	}
	if elements > int64(^uint64(0)>>1)/width {
		return nil
	}
	value := elements * width
	return &value
}

func resourceNames(resources []corevalidation.ResourceRef) []string {
	names := make([]string, 0, len(resources))
	for _, resource := range resources {
		value := resource.APIVersion + " " + resource.Kind + " " + resource.Namespace + "/" + resource.Name
		if resource.UID != "" {
			value += " (" + resource.UID + ")"
		}
		names = append(names, strings.TrimSpace(value))
	}
	return names
}

func cleanupReason(result corevalidation.Result) string {
	if result.Cleanup.State == corevalidation.CleanupIncomplete || len(result.Cleanup.RemainingResources) > 0 {
		return reasonText(corevalidation.ReasonCleanupIncomplete)
	}
	if result.Cleanup.State == corevalidation.CleanupUnknown {
		return "Cleanup completion is unknown."
	}
	return ""
}

func reasonText(reason corevalidation.ReasonCode) string {
	switch reason {
	case corevalidation.ReasonValidationPassed:
		return "Verified two-GPU inter-node NCCL all-reduce used InfiniBand without socket fallback."
	case corevalidation.ReasonSocketFallbackObserved:
		return "NCCL socket fallback was observed."
	case corevalidation.ReasonIBTransportNotProven:
		return "Positive NCCL NET/IB transport evidence was not proven."
	case corevalidation.ReasonTransportFailure:
		return "The requested NCCL InfiniBand transport failed."
	case corevalidation.ReasonPeerAuthenticationFailed:
		return "Rank peer authentication failed."
	case corevalidation.ReasonPlacementMismatch:
		return "Actual placement did not match the requested topology."
	case corevalidation.ReasonTopologyMismatch:
		return "The observed validation topology was invalid."
	case corevalidation.ReasonTopologyEvidenceIncomplete:
		return "Unbounded site topology evidence was incomplete or conflicting."
	case corevalidation.ReasonCorrectnessError:
		return "The collective reported a correctness error."
	case corevalidation.ReasonNonzeroExit:
		return "The Job or one or more ranks exited nonzero."
	case corevalidation.ReasonCleanupIncomplete:
		return "Cleanup did not remove every owned resource."
	case corevalidation.ReasonMissingRequiredEvidence:
		return "Required validation evidence is missing."
	case corevalidation.ReasonInvalidMeasurement:
		return "Bandwidth measurements were invalid."
	case corevalidation.ReasonEvidenceIntegrityMissing:
		return "Required evidence hashes are missing or unverifiable."
	case corevalidation.ReasonParserRejected:
		return "The evidence parser rejected incomplete or malformed input."
	case corevalidation.ReasonRuntimeError:
		return "The validation encountered a runtime error."
	default:
		return "The validation result is unknown."
	}
}

func durationSeconds(start, end time.Time) *float64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil
	}
	value := end.Sub(start).Seconds()
	return &value
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func intPointer(value int) *int           { return &value }
func int64Pointer(value int64) *int64     { return &value }
func floatPointer(value float64) *float64 { return &value }
func boolPointer(value bool) *bool        { return &value }
