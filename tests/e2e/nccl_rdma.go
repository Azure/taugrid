// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/Azure/taugrid/tests/e2e/internal/ncclimage"
)

const (
	NCCLRDMAPassSentinel         = "TAUGRID_RDMA_PASS"
	NCCLRDMAControlPlaneSentinel = "TAUGRID_CONTROL_PLANE_PASS"
)

var (
	ncclRDMAIBUsingRE      = regexp.MustCompile(`NET/IB[^\n]*Using`)
	ncclRDMAIBDeviceRE     = regexp.MustCompile(`\[[0-9]+\]([A-Za-z0-9_.-]+):[0-9]+/IB`)
	ncclRDMASocketRE       = regexp.MustCompile(`NET/Socket[^\n]*Using`)
	ncclRDMASafeEvidenceRE = regexp.MustCompile(`^[A-Za-z0-9_./:;<> ,\[\]()=+-]+$`)
	ncclRDMAFailureRE      = regexp.MustCompile(
		`(?i)No device found|Failed to open libibverbs|ibv_[[:alnum:]_]+[^\n]*(fail|error)|mlx5[^\n]*(fail|error)|verbs[^\n]*(fail|error)|unhandled system error|NCCL WARN[^\n]*(IB|NET)[^\n]*(fail|error)`,
	)
	ncclRDMAPeerAuthRE    = regexp.MustCompile(`(?m)^TAUGRID_PEER_AUTH\s+rank=[0-9]+\s+peers=[0-9]+\s*$`)
	ncclRDMARankPassRE    = regexp.MustCompile(`(?m)^TAUGRID_RDMA_RANK_PASS\s+rank=[0-9]+\s*$`)
	ncclRDMAMemlockRE     = regexp.MustCompile(`(?m)^TAUGRID_MEMLOCK\s+(\{.*\})\s*$`)
	ncclRDMARuntimeRE     = regexp.MustCompile(`(?m)^TAUGRID_RDMA_RUNTIME\s+(\{.*\})\s*$`)
	ncclRDMAMeasurementRE = regexp.MustCompile(`(?m)^TAUGRID_RDMA_MEASUREMENT\s+(\{.*\})\s*$`)
	ncclRDMAPassRE        = regexp.MustCompile(`(?m)^TAUGRID_RDMA_PASS\s+(\{.*\})\s*$`)
)

type NCCLRDMAParseError struct {
	Reason  rdmavalidation.ReasonCode
	Message string
}

func (err *NCCLRDMAParseError) Error() string {
	return err.Message
}

type NCCLRDMAMemlock struct {
	Soft int64
	Hard int64
}

type NCCLRDMAResult struct {
	MaxAlgBW         float64
	MaxBusBW         float64
	MaxError         float64
	MaxRankTime      float64
	Backend          string
	NCCLVersion      string
	Nodes            [2]string
	Memlock          map[int]NCCLRDMAMemlock
	Runtime          map[int]NCCLRDMARuntime
	Measurements     map[int]NCCLRDMAMeasurement
	IBDevices        map[int][]string
	IBEvidence       []string
	PeerAuthVerified bool
}

type ncclRDMAPassReceipt struct {
	Backend             string   `json:"backend"`
	NCCLVersion         string   `json:"nccl_version"`
	WorldSize           *int     `json:"world_size"`
	Nodes               []string `json:"nodes"`
	Hosts               []string `json:"hosts"`
	PeerAuthVerified    *bool    `json:"peer_auth_verified"`
	PayloadBytes        *int64   `json:"payload_bytes"`
	Iterations          *int     `json:"iterations"`
	MaxElapsedSeconds   *float64 `json:"max_elapsed_seconds"`
	SecondsPerIteration *float64 `json:"seconds_per_iteration"`
	AlgBW               *float64 `json:"algbw_gbps"`
	BusBW               *float64 `json:"busbw_gbps"`
	MaxError            *float64 `json:"max_error"`
}

type NCCLRDMARuntime struct {
	Rank          int
	Node          string
	Host          string
	GPUModel      string
	GPUUUID       string
	RDMADevice    string
	RDMAInterface string
	RDMALinkState string
	NCCLVersion   string
	Environment   map[string]string
}

type NCCLRDMAMeasurement struct {
	Rank           int
	ElapsedSeconds float64
	AlgBW          float64
	BusBW          float64
}

func ParseNCCLRDMAOutput(output string) (NCCLRDMAResult, error) {
	result := NCCLRDMAResult{
		Memlock: map[int]NCCLRDMAMemlock{}, Runtime: map[int]NCCLRDMARuntime{},
		Measurements: map[int]NCCLRDMAMeasurement{}, IBDevices: map[int][]string{},
	}

	if strings.Contains(output, "TAUGRID_RDMA_FAIL") {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonRuntimeError, "probe emitted a failure sentinel")
	}
	if strings.Contains(output, NCCLRDMAControlPlaneSentinel) {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonTransportFailure, "offline Gloo control-plane sentinel cannot satisfy live validation")
	}
	if ncclRDMASocketRE.MatchString(output) {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonSocketFallbackObserved, "NCCL selected forbidden NET/Socket transport")
	}
	if failure := ncclRDMAFailureRE.FindString(output); failure != "" {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonTransportFailure, "NCCL/RDMA device or verbs failure: %s", failure)
	}
	sections, err := ncclRDMARankLogSections(output)
	if err != nil {
		return result, err
	}
	for rank, section := range sections {
		if !ncclRDMAIBUsingRE.MatchString(section) {
			return result, newNCCLRDMAParseError(
				rdmavalidation.ReasonIBTransportNotProven,
				"rank %d is missing positive NET/IB Using evidence",
				rank,
			)
		}
		rankEvidence := 0
		rankDevices := map[string]bool{}
		for _, line := range strings.Split(section, "\n") {
			if !ncclRDMAIBUsingRE.MatchString(line) {
				continue
			}
			line = strings.TrimSpace(line)
			if len(line) == 0 || len(line) > 512 || !ncclRDMASafeEvidenceRE.MatchString(line) {
				return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "NET/IB evidence line contains unapproved content")
			}
			result.IBEvidence = append(result.IBEvidence, fmt.Sprintf("rank=%d %s", rank, line))
			for _, match := range ncclRDMAIBDeviceRE.FindAllStringSubmatch(line, -1) {
				rankDevices[match[1]] = true
			}
			rankEvidence++
		}
		if rankEvidence == 0 || rankEvidence > 8 || len(rankDevices) == 0 {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonIBTransportNotProven, "rank %d has an invalid NET/IB evidence count", rank)
		}
		for device := range rankDevices {
			result.IBDevices[rank] = append(result.IBDevices[rank], device)
		}
		sort.Strings(result.IBDevices[rank])
	}
	sort.Strings(result.IBEvidence)
	if len(ncclRDMAPeerAuthRE.FindAllString(output, -1)) != 2 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly two authenticated peer receipts")
	}
	if len(ncclRDMARankPassRE.FindAllString(output, -1)) != 2 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly two rank pass receipts")
	}
	for rank := 0; rank < 2; rank++ {
		if countExactLine(output, fmt.Sprintf("TAUGRID_PEER_AUTH rank=%d peers=2", rank)) != 1 {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly one authenticated peer receipt for rank %d", rank)
		}
		if countExactLine(output, fmt.Sprintf("TAUGRID_RDMA_RANK_PASS rank=%d", rank)) != 1 {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly one pass receipt for rank %d", rank)
		}
	}

	memlockMatches := ncclRDMAMemlockRE.FindAllStringSubmatch(output, -1)
	if len(memlockMatches) != 2 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly two process-start memlock receipts")
	}
	for _, match := range memlockMatches {
		var receipt struct {
			Rank     *int   `json:"rank"`
			Soft     *int64 `json:"soft"`
			Hard     *int64 `json:"hard"`
			Infinity *int64 `json:"infinity"`
		}
		if err := decodeExactJSON(match[1], &receipt); err != nil {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "decode memlock receipt: %v", err)
		}
		if receipt.Rank == nil || receipt.Soft == nil || receipt.Hard == nil ||
			receipt.Infinity == nil || *receipt.Infinity != -1 {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "memlock receipt is missing rank, soft, hard, or the pinned infinity sentinel")
		}
		if *receipt.Rank < 0 || *receipt.Rank > 1 ||
			(*receipt.Soft != -1 && *receipt.Soft <= 0) ||
			(*receipt.Hard != -1 && *receipt.Hard <= 0) ||
			*receipt.Hard != -1 && *receipt.Soft != -1 && *receipt.Soft > *receipt.Hard {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "invalid memlock receipt for rank %d", *receipt.Rank)
		}
		if _, duplicate := result.Memlock[*receipt.Rank]; duplicate {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "duplicate memlock receipt for rank %d", *receipt.Rank)
		}
		result.Memlock[*receipt.Rank] = NCCLRDMAMemlock{Soft: *receipt.Soft, Hard: *receipt.Hard}
	}

	runtimeMatches := ncclRDMARuntimeRE.FindAllStringSubmatch(output, -1)
	if len(runtimeMatches) != 2 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly two runtime receipts")
	}
	var expectedEnvironment map[string]string
	for _, match := range runtimeMatches {
		var receipt struct {
			Rank          *int              `json:"rank"`
			Node          string            `json:"node"`
			Host          string            `json:"host"`
			GPUModel      string            `json:"gpu_model"`
			GPUUUID       string            `json:"gpu_uuid"`
			RDMADevice    string            `json:"rdma_device"`
			RDMAInterface string            `json:"rdma_interface"`
			RDMALinkState string            `json:"rdma_link_state"`
			NCCLVersion   string            `json:"nccl_version"`
			Environment   map[string]string `json:"environment"`
		}
		if err := decodeExactJSON(match[1], &receipt); err != nil {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "decode runtime receipt: %v", err)
		}
		if receipt.Rank == nil || *receipt.Rank < 0 || *receipt.Rank > 1 ||
			receipt.Node == "" || receipt.Host == "" || receipt.GPUModel == "" || receipt.GPUUUID == "" ||
			receipt.RDMADevice == "" || receipt.RDMAInterface == "" ||
			!strings.Contains(strings.ToUpper(receipt.RDMALinkState), "ACTIVE") ||
			receipt.NCCLVersion != ncclimage.QualifiedNCCL ||
			!equalStringMap(receipt.Environment, expectedNCCLRDMAEnvironment()) {
			reason := rdmavalidation.ReasonParserRejected
			if receipt.RDMALinkState != "" && !strings.Contains(strings.ToUpper(receipt.RDMALinkState), "ACTIVE") {
				reason = rdmavalidation.ReasonTransportFailure
			} else if receipt.NCCLVersion != "" && receipt.NCCLVersion != ncclimage.QualifiedNCCL ||
				len(receipt.Environment) > 0 && !equalStringMap(receipt.Environment, expectedNCCLRDMAEnvironment()) {
				reason = rdmavalidation.ReasonRuntimeError
			}
			return result, newNCCLRDMAParseError(reason, "runtime receipt is incomplete or invalid")
		}
		if _, duplicate := result.Runtime[*receipt.Rank]; duplicate {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonTopologyMismatch, "duplicate runtime receipt for rank %d", *receipt.Rank)
		}
		if !stringSliceContains(result.IBDevices[*receipt.Rank], receipt.RDMADevice) {
			return result, newNCCLRDMAParseError(
				rdmavalidation.ReasonTransportFailure,
				"rank %d runtime RDMA device %q was not named by NCCL NET/IB evidence",
				*receipt.Rank,
				receipt.RDMADevice,
			)
		}
		if expectedEnvironment == nil {
			expectedEnvironment = receipt.Environment
		} else if !equalStringMap(expectedEnvironment, receipt.Environment) {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonRuntimeError, "rank runtime environment receipts do not match")
		}
		result.Runtime[*receipt.Rank] = NCCLRDMARuntime{
			Rank: *receipt.Rank, Node: receipt.Node, Host: receipt.Host, GPUModel: receipt.GPUModel,
			GPUUUID: receipt.GPUUUID, RDMADevice: receipt.RDMADevice,
			RDMAInterface: receipt.RDMAInterface, RDMALinkState: receipt.RDMALinkState,
			NCCLVersion: receipt.NCCLVersion, Environment: receipt.Environment,
		}
	}

	measurementMatches := ncclRDMAMeasurementRE.FindAllStringSubmatch(output, -1)
	if len(measurementMatches) != 2 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly two bandwidth measurement receipts")
	}
	for _, match := range measurementMatches {
		var receipt struct {
			Rank           *int     `json:"rank"`
			ElapsedSeconds *float64 `json:"elapsed_seconds"`
			AlgBW          *float64 `json:"algbw_gbps"`
			BusBW          *float64 `json:"busbw_gbps"`
		}
		if err := decodeExactJSON(match[1], &receipt); err != nil {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "decode bandwidth measurement receipt: %v", err)
		}
		if receipt.Rank == nil || receipt.ElapsedSeconds == nil || receipt.AlgBW == nil || receipt.BusBW == nil ||
			*receipt.Rank < 0 || *receipt.Rank > 1 || !positiveFinite(*receipt.ElapsedSeconds) ||
			!positiveFinite(*receipt.AlgBW) || !positiveFinite(*receipt.BusBW) {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonInvalidMeasurement, "bandwidth measurement receipt is incomplete or invalid")
		}
		if _, duplicate := result.Measurements[*receipt.Rank]; duplicate {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonTopologyMismatch, "duplicate bandwidth measurement receipt for rank %d", *receipt.Rank)
		}
		result.Measurements[*receipt.Rank] = NCCLRDMAMeasurement{
			Rank: *receipt.Rank, ElapsedSeconds: *receipt.ElapsedSeconds,
			AlgBW: *receipt.AlgBW, BusBW: *receipt.BusBW,
		}
	}

	passMatches := ncclRDMAPassRE.FindAllStringSubmatch(output, -1)
	if len(passMatches) != 1 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "expected exactly one live pass sentinel")
	}
	var receipt ncclRDMAPassReceipt
	if err := decodeExactJSON(passMatches[0][1], &receipt); err != nil {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "decode live pass receipt: %v", err)
	}
	if receipt.Backend != "nccl" || receipt.NCCLVersion != ncclimage.QualifiedNCCL ||
		receipt.WorldSize == nil || *receipt.WorldSize != 2 ||
		receipt.PeerAuthVerified == nil || !*receipt.PeerAuthVerified {
		reason := rdmavalidation.ReasonTransportFailure
		if receipt.PeerAuthVerified != nil && !*receipt.PeerAuthVerified {
			reason = rdmavalidation.ReasonPeerAuthenticationFailed
		}
		return result, newNCCLRDMAParseError(reason, "live pass receipt does not prove NCCL world-size two with peer authentication")
	}
	if len(receipt.Nodes) != 2 || receipt.Nodes[0] == "" || receipt.Nodes[1] == "" || receipt.Nodes[0] == receipt.Nodes[1] {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonPlacementMismatch, "live pass receipt does not contain two distinct actual nodes")
	}
	if len(receipt.Hosts) != 2 || receipt.Hosts[0] == "" || receipt.Hosts[1] == "" || receipt.Hosts[0] == receipt.Hosts[1] {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonTopologyMismatch, "live pass receipt does not contain two distinct pod hosts")
	}
	if receipt.MaxError == nil {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "live pass receipt is missing max_error")
	}
	if *receipt.MaxError != 0 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonCorrectnessError, "collective correctness error is %v, want zero", *receipt.MaxError)
	}
	if receipt.AlgBW == nil || receipt.BusBW == nil ||
		!positiveFinite(*receipt.AlgBW) || !positiveFinite(*receipt.BusBW) ||
		receipt.MaxElapsedSeconds == nil || !positiveFinite(*receipt.MaxElapsedSeconds) ||
		receipt.SecondsPerIteration == nil || !positiveFinite(*receipt.SecondsPerIteration) ||
		receipt.PayloadBytes == nil || *receipt.PayloadBytes <= 0 ||
		receipt.Iterations == nil || *receipt.Iterations <= 0 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonInvalidMeasurement, "live pass receipt does not contain positive finite algbw/busbw")
	}
	for rank := 0; rank < 2; rank++ {
		runtime := result.Runtime[rank]
		measurement := result.Measurements[rank]
		if runtime.Node != receipt.Nodes[rank] || runtime.Host != receipt.Hosts[rank] ||
			runtime.NCCLVersion != receipt.NCCLVersion {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonTopologyMismatch, "runtime and pass receipts disagree for rank %d", rank)
		}
		if measurement.ElapsedSeconds > *receipt.MaxElapsedSeconds+1e-9 {
			return result, newNCCLRDMAParseError(rdmavalidation.ReasonInvalidMeasurement, "rank %d elapsed time exceeds the reported maximum rank time", rank)
		}
	}
	expectedSeconds := *receipt.MaxElapsedSeconds / float64(*receipt.Iterations)
	expectedAlgBW := float64(*receipt.PayloadBytes) / expectedSeconds / 1e9
	if math.Abs(expectedSeconds-*receipt.SecondsPerIteration) > 1e-9 ||
		math.Abs(expectedAlgBW-*receipt.AlgBW) > 1e-9 ||
		math.Abs(expectedAlgBW-*receipt.BusBW) > 1e-9 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonInvalidMeasurement, "pass receipt bandwidth does not match payload, iterations, and maximum rank time")
	}
	maxMeasuredTime := math.Max(
		result.Measurements[0].ElapsedSeconds,
		result.Measurements[1].ElapsedSeconds,
	)
	if math.Abs(maxMeasuredTime-*receipt.MaxElapsedSeconds) > 1e-9 {
		return result, newNCCLRDMAParseError(rdmavalidation.ReasonInvalidMeasurement, "per-rank measurements do not match the reported maximum rank time")
	}
	result.MaxAlgBW = *receipt.AlgBW
	result.MaxBusBW = *receipt.BusBW
	result.MaxError = *receipt.MaxError
	result.MaxRankTime = *receipt.MaxElapsedSeconds
	result.Backend = receipt.Backend
	result.NCCLVersion = receipt.NCCLVersion
	result.PeerAuthVerified = *receipt.PeerAuthVerified
	result.Nodes = [2]string{receipt.Nodes[0], receipt.Nodes[1]}
	return result, nil
}

func NCCLRDMAParseFailureReason(err error) rdmavalidation.ReasonCode {
	var parseError *NCCLRDMAParseError
	if errors.As(err, &parseError) {
		return parseError.Reason
	}
	return rdmavalidation.ReasonParserRejected
}

func newNCCLRDMAParseError(
	reason rdmavalidation.ReasonCode,
	format string,
	arguments ...interface{},
) error {
	return &NCCLRDMAParseError{Reason: reason, Message: fmt.Sprintf(format, arguments...)}
}

func ncclRDMARankLogSections(output string) ([2]string, error) {
	var sections [2]string
	var beginPositions [2]int
	var endPositions [2]int
	for rank := 0; rank < 2; rank++ {
		begin := fmt.Sprintf("TAUGRID_RANK_LOG_BEGIN rank=%d\n", rank)
		end := fmt.Sprintf("TAUGRID_RANK_LOG_END rank=%d", rank)
		if strings.Count(output, begin) != 1 || strings.Count(output, end) != 1 {
			return sections, newNCCLRDMAParseError(
				rdmavalidation.ReasonParserRejected,
				"expected exactly one bounded log section for rank %d",
				rank,
			)
		}
		beginPositions[rank] = strings.Index(output, begin)
		start := beginPositions[rank] + len(begin)
		remainder := output[start:]
		finish := strings.Index(remainder, end)
		if finish < 0 {
			return sections, newNCCLRDMAParseError(rdmavalidation.ReasonParserRejected, "rank %d log section is not terminated", rank)
		}
		endPositions[rank] = start + finish
		sections[rank] = remainder[:finish]
	}
	if !(beginPositions[0] < endPositions[0] &&
		endPositions[0] < beginPositions[1] &&
		beginPositions[1] < endPositions[1]) {
		return sections, newNCCLRDMAParseError(
			rdmavalidation.ReasonParserRejected,
			"rank log sections overlap or are out of order",
		)
	}
	return sections, nil
}

func decodeExactJSON(raw string, destination interface{}) error {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func expectedNCCLRDMAEnvironment() map[string]string {
	return map[string]string{
		"NCCL_DEBUG":         "INFO",
		"NCCL_DEBUG_SUBSYS":  "INIT,NET",
		"NCCL_IB_DISABLE":    "0",
		"TAUGRID_BACKEND":    "nccl",
		"TAUGRID_ELEMENTS":   "16777216",
		"TAUGRID_ITERATIONS": "20",
		"TAUGRID_LIVE_RDMA":  "1",
		"TAUGRID_WARMUP":     "5",
	}
}

func countExactLine(output, want string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == want {
			count++
		}
	}
	return count
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
