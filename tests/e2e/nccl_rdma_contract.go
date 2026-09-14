// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/Azure/taugrid/tests/e2e/internal/ncclimage"
	corev1 "k8s.io/api/core/v1"
)

const (
	NCCLRDMAContractProducerVersion = "rdma-validation.v1"
	NCCLRDMAContractParserVersion   = "torchrun-rdma-probe.v1"
)

type NCCLRDMAValidationInput struct {
	ValidationID string
	RunID        string
	Attempt      int
	WorkspaceID  string
	Cluster      string
	Namespace    string
	ProjectID    string
	ExperimentID string
	RunGroupID   string

	ExpectedSite     string
	ExpectedPool     string
	ExpectedGPUModel string
	SourceRevision   string

	CreatedAt   time.Time
	StartedAt   time.Time
	AdmittedAt  time.Time
	CompletedAt time.Time
	ObservedAt  time.Time
	StaleAfter  time.Duration

	Parsed            NCCLRDMAResult
	Pods              []corev1.Pod
	Nodes             map[string]*corev1.Node
	SanitizedManifest []byte
	Cleanup           rdmavalidation.Cleanup
	Errors            []rdmavalidation.ValidationError
}

func NewNCCLRDMAValidationSkeleton(input NCCLRDMAValidationInput) rdmavalidation.Result {
	trustVerified := ncclimage.QualifiedSupplyChainEvidence().SignatureTrustVerified
	observed := input.ObservedAt.UTC()
	if observed.IsZero() {
		observed = input.CreatedAt.UTC()
	}
	staleSeconds := int64(input.StaleAfter / time.Second)
	if staleSeconds <= 0 {
		staleSeconds = 86400
	}
	return rdmavalidation.Result{
		Schema: rdmavalidation.SchemaVersion, Kind: rdmavalidation.Kind,
		ValidationID: input.ValidationID, RunID: input.RunID, Attempt: input.Attempt,
		WorkspaceID: input.WorkspaceID, Cluster: input.Cluster, Namespace: input.Namespace,
		ProjectID: input.ProjectID, ExperimentID: input.ExperimentID, RunGroupID: input.RunGroupID,
		CreatedAt: input.CreatedAt.UTC(), StartedAt: input.StartedAt.UTC(),
		AdmittedAt: input.AdmittedAt.UTC(), CompletedAt: input.CompletedAt.UTC(),
		ObservedAt: observed, StaleAfterSeconds: staleSeconds,
		ValidUntil: observed.Add(time.Duration(staleSeconds) * time.Second),
		Source:     rdmavalidation.Source{Repository: "Azure/taugrid", Revision: input.SourceRevision},
		Image: rdmavalidation.ImageProvenance{
			Repository: ncclimage.Repository, IndexDigest: ncclimage.IndexDigest,
			PlatformDigest: ncclimage.LinuxAMD64Digest, ConfigDigest: ncclimage.LinuxAMD64Config,
			SBOMManifestDigest: ncclimage.SBOMManifestDigest, SBOMLayerDigest: ncclimage.SBOMLayerDigest,
			VEXManifestDigest: ncclimage.VEXManifestDigest, VEXLayerDigest: ncclimage.VEXLayerDigest,
			SignatureManifestDigest: ncclimage.SignatureManifestDigest,
			SignatureLayerDigest:    ncclimage.SignatureLayerDigest,
			SignatureTrustVerified:  &trustVerified,
		},
		Requested: rdmavalidation.Requested{
			Topology: rdmavalidation.RequestedTopology{
				NodeCount: 2, PodCount: 2, RankCount: 2, DistinctHostname: true,
				Site: input.ExpectedSite, Pool: input.ExpectedPool, GPUModel: input.ExpectedGPUModel,
			},
			Resources: rdmavalidation.RequestedResources{
				CPURequestMilli: 4000, CPULimitMilli: 8000,
				MemoryRequestBytes: 16 << 30, MemoryLimitBytes: 32 << 30,
				GPUResource: "nvidia.com/gpu", GPUCount: 1,
				RDMAResource: "rdma/rdma_shared_device_a", RDMACount: 1,
				SharedMemoryBytes: 16 << 30,
			},
			Parameters: rdmavalidation.BenchmarkParameters{
				WorldSize: 2, ProcessesPerPod: 1, Elements: 16_777_216,
				Warmup: 5, Iterations: 20, Operation: "all_reduce", DataType: "float32",
			},
		},
		Cleanup: input.Cleanup, Errors: append([]rdmavalidation.ValidationError(nil), input.Errors...),
		Producer: rdmavalidation.ComponentVersion{
			Name: "taugrid-e2e", Version: NCCLRDMAContractProducerVersion,
		},
		Parser: rdmavalidation.ComponentVersion{
			Name: "torchrun-rdma-parser", Version: NCCLRDMAContractParserVersion,
		},
	}
}

func BuildNCCLRDMAValidationResult(input NCCLRDMAValidationInput) (rdmavalidation.Result, error) {
	result := NewNCCLRDMAValidationSkeleton(input)
	if len(input.Pods) != 2 || len(input.Nodes) != 2 {
		return result, fmt.Errorf("contract conversion requires exactly two pods and two nodes")
	}

	pods := append([]corev1.Pod(nil), input.Pods...)
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Labels["batch.kubernetes.io/job-completion-index"] <
			pods[j].Labels["batch.kubernetes.io/job-completion-index"]
	})
	site, pool := "", ""
	placementMatches := true
	distinctNodes := pods[0].Spec.NodeName != pods[1].Spec.NodeName
	result.Pods = make([]rdmavalidation.PodResult, 0, 2)
	result.Ranks = make([]rdmavalidation.RankResult, 0, 2)
	result.RankExits = make([]rdmavalidation.RankExit, 0, 2)
	result.Actual.Nodes = make([]rdmavalidation.NodeResult, 0, 2)
	samples := make([]rdmavalidation.BandwidthMeasurement, 0, 2)
	interfaces := make([]string, 0, 2)
	jobExitCode := 0
	for rank, pod := range pods {
		node := input.Nodes[pod.Spec.NodeName]
		if node == nil {
			return result, fmt.Errorf("pod %s references missing node %s", pod.Name, pod.Spec.NodeName)
		}
		nodeSite := node.Labels["topology.kubernetes.io/region"]
		nodePool := node.Labels["kubernetes.azure.com/agentpool"]
		if rank == 0 {
			site, pool = nodeSite, nodePool
		} else if nodeSite != site || nodePool != pool {
			placementMatches = false
		}
		runtime, ok := input.Parsed.Runtime[rank]
		if !ok {
			return result, fmt.Errorf("missing runtime receipt for rank %d", rank)
		}
		measurement, ok := input.Parsed.Measurements[rank]
		if !ok {
			return result, fmt.Errorf("missing measurement receipt for rank %d", rank)
		}
		exitCode, err := podExitCode(pod)
		if err != nil {
			return result, err
		}
		if jobExitCode == 0 && exitCode != 0 {
			jobExitCode = exitCode
		}
		if runtime.Node != pod.Spec.NodeName || runtime.GPUModel != input.ExpectedGPUModel {
			placementMatches = false
		}
		result.Actual.Nodes = append(result.Actual.Nodes, rdmavalidation.NodeResult{
			Name: pod.Spec.NodeName, UID: string(node.UID), GPUModel: runtime.GPUModel,
			GPUUUID: runtime.GPUUUID, RDMADevice: runtime.RDMADevice,
			RDMAInterface: runtime.RDMAInterface, RDMALinkState: runtime.RDMALinkState,
		})
		result.Pods = append(result.Pods, rdmavalidation.PodResult{
			Name: pod.Name, UID: string(pod.UID), NodeName: pod.Spec.NodeName, Rank: rank,
			ExitCode: contractIntPointer(exitCode),
		})
		memlock := input.Parsed.Memlock[rank]
		result.Ranks = append(result.Ranks, rdmavalidation.RankResult{
			Rank: rank, PodUID: string(pod.UID), NodeName: pod.Spec.NodeName, NodeUID: string(node.UID),
			PeerAuthVerified: contractBoolPointer(input.Parsed.PeerAuthVerified),
			MemlockSoftBytes: contractInt64Pointer(memlock.Soft), MemlockHardBytes: contractInt64Pointer(memlock.Hard),
		})
		result.RankExits = append(result.RankExits, rdmavalidation.RankExit{
			Rank: rank, ExitCode: contractIntPointer(exitCode),
		})
		samples = append(samples, rdmavalidation.BandwidthMeasurement{
			Rank: rank, ElapsedSeconds: measurement.ElapsedSeconds,
			AlgBWGbps: measurement.AlgBW, BusBWGbps: measurement.BusBW,
		})
		interfaces = append(interfaces, runtime.RDMAInterface)
	}
	result.Actual.Site, result.Actual.Pool = site, pool
	placementMatches = placementMatches && distinctNodes &&
		site == input.ExpectedSite && pool == input.ExpectedPool &&
		input.Parsed.Nodes == [2]string{pods[0].Spec.NodeName, pods[1].Spec.NodeName}
	result.Placement = rdmavalidation.Placement{
		MatchesRequest: contractBoolPointer(placementMatches), DistinctNodes: contractBoolPointer(distinctNodes),
	}
	result.NCCL = rdmavalidation.NCCL{Version: input.Parsed.NCCLVersion, Operation: "all_reduce"}
	result.Transport = rdmavalidation.Transport{
		Backend: input.Parsed.Backend, NCCLNet: "IB", Interfaces: interfaces,
		SocketFallbackObserved: contractBoolPointer(false), PositiveIBEvidence: contractBoolPointer(len(input.Parsed.IBEvidence) > 0),
		Evidence:    append([]string(nil), input.Parsed.IBEvidence...),
		Environment: cloneStrings(input.Parsed.Runtime[0].Environment),
	}
	result.Measurements = rdmavalidation.NewMeasurements(samples, input.Parsed.MaxRankTime)
	result.Correctness = rdmavalidation.Correctness{
		MaxError: contractFloat64Pointer(input.Parsed.MaxError), ErrorCount: contractIntPointer(0),
	}
	result.JobExitCode = contractIntPointer(jobExitCode)

	evidence, err := ncclRDMAEvidence(input, result)
	if err != nil {
		return result, err
	}
	result.Evidence = evidence
	if err := rdmavalidation.Finalize(&result); err != nil {
		return result, err
	}
	return result, nil
}

func ncclRDMAEvidence(
	input NCCLRDMAValidationInput,
	result rdmavalidation.Result,
) ([]rdmavalidation.EvidenceRef, error) {
	capturedAt := input.ObservedAt.UTC()
	values := map[string]interface{}{
		"sanitized-manifest": json.RawMessage(input.SanitizedManifest),
		"sanitized-logs": struct {
			Runtime      map[int]NCCLRDMARuntime     `json:"runtime"`
			Measurements map[int]NCCLRDMAMeasurement `json:"measurements"`
			Memlock      map[int]NCCLRDMAMemlock     `json:"memlock"`
			IBEvidence   []string                    `json:"ib_evidence"`
		}{input.Parsed.Runtime, input.Parsed.Measurements, input.Parsed.Memlock, input.Parsed.IBEvidence},
		"placement": struct {
			Actual    rdmavalidation.Actual      `json:"actual"`
			Placement rdmavalidation.Placement   `json:"placement"`
			Pods      []rdmavalidation.PodResult `json:"pods"`
		}{result.Actual, result.Placement, result.Pods},
		"image-receipt": result.Image,
		"cleanup":       result.Cleanup,
	}
	refs := make([]rdmavalidation.EvidenceRef, 0, len(values))
	for _, name := range []string{"sanitized-manifest", "sanitized-logs", "placement", "image-receipt", "cleanup"} {
		raw, err := json.MarshalIndent(values[name], "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal %s evidence: %w", name, err)
		}
		raw = append(raw, '\n')
		ref, err := rdmavalidation.NewEvidenceRef(name, "", raw, capturedAt)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func podExitCode(pod corev1.Pod) (int, error) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "probe" && status.State.Terminated != nil {
			return int(status.State.Terminated.ExitCode), nil
		}
	}
	return 0, fmt.Errorf("pod %s has no terminated probe container status", pod.Name)
}

func cloneStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func contractIntPointer(value int) *int             { return &value }
func contractInt64Pointer(value int64) *int64       { return &value }
func contractBoolPointer(value bool) *bool          { return &value }
func contractFloat64Pointer(value float64) *float64 { return &value }
