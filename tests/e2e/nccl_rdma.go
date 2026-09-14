// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const (
	NCCLRDMAOutputFormat = "nccl-tests-2.16.0-table-v1"
	NCCLRDMAPassSentinel = "NCCL_RDMA_CONFORMANCE_PASS"
	ncclRDMATableFields  = 13
)

var (
	ncclRDMAIBUsingRE     = regexp.MustCompile(`NET/IB\s*:\s*Using`)
	ncclRDMASocketRE      = regexp.MustCompile(`NET/Socket\s*:\s*Using`)
	ncclRDMAOutOfBoundsRE = regexp.MustCompile(`(?m)^# Out of bounds values\s*:\s*([0-9]+)\s+(OK|FAILED)\s*$`)
	ncclRDMAFailureRE     = regexp.MustCompile(`(?i)No device found|Failed to open libibverbs|ibv_[[:alnum:]_]+.*(fail|error)|mlx5.*(fail|error)|verbs.*(fail|error)|unhandled system error`)
	ncclRDMATableHeaderRE = regexp.MustCompile(`(?m)^#\s+size\s+count\s+type\s+redop\s+root\s+time\s+algbw\s+busbw\s+#wrong\s+time\s+algbw\s+busbw\s+#wrong\s*$`)
)

// NCCLRDMAResult is the validated performance-table summary from nccl-tests.
type NCCLRDMAResult struct {
	DataRows int
	MaxAlgBW float64
	MaxBusBW float64
}

// ParseNCCLRDMAOutput validates the pinned nccl-tests output contract. Missing
// evidence, fallback transports, verbs/device failures, malformed rows, and
// duplicate/missing success sentinels all fail closed.
func ParseNCCLRDMAOutput(output string) (NCCLRDMAResult, error) {
	var result NCCLRDMAResult
	formatMarker := "# taugrid nccl-tests format: " + NCCLRDMAOutputFormat
	if countExactLine(output, formatMarker) != 1 {
		return result, fmt.Errorf("expected exactly one pinned output format marker %q", formatMarker)
	}
	if countExactLine(output, NCCLRDMAPassSentinel) != 1 {
		return result, fmt.Errorf("expected exactly one pass sentinel %q", NCCLRDMAPassSentinel)
	}
	if !ncclRDMATableHeaderRE.MatchString(output) {
		return result, fmt.Errorf("missing pinned nccl-tests bandwidth table header")
	}
	if !ncclRDMAIBUsingRE.MatchString(output) {
		return result, fmt.Errorf("missing positive NET/IB Using evidence")
	}
	if ncclRDMASocketRE.MatchString(output) {
		return result, fmt.Errorf("NCCL selected forbidden NET/Socket transport")
	}
	if failure := ncclRDMAFailureRE.FindString(output); failure != "" {
		return result, fmt.Errorf("NCCL/RDMA device or verbs failure: %s", failure)
	}

	outOfBounds := ncclRDMAOutOfBoundsRE.FindAllStringSubmatch(output, -1)
	if len(outOfBounds) != 1 || outOfBounds[0][1] != "0" || outOfBounds[0][2] != "OK" {
		return result, fmt.Errorf("expected exactly one zero/OK out-of-bounds summary")
	}

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != ncclRDMATableFields || !allDigits(fields[0]) {
			continue
		}
		pairs := [][2]int{{6, 7}, {10, 11}}
		rowPositive := false
		for _, pair := range pairs {
			algBW, algErr := strconv.ParseFloat(fields[pair[0]], 64)
			busBW, busErr := strconv.ParseFloat(fields[pair[1]], 64)
			if algErr != nil || busErr != nil {
				return result, fmt.Errorf("malformed nccl-tests bandwidth row %q", line)
			}
			if math.IsNaN(algBW) || math.IsInf(algBW, 0) || math.IsNaN(busBW) || math.IsInf(busBW, 0) {
				return result, fmt.Errorf("non-finite nccl-tests bandwidth row %q", line)
			}
			if algBW > 0 && busBW > 0 {
				rowPositive = true
				if algBW > result.MaxAlgBW {
					result.MaxAlgBW = algBW
				}
				if busBW > result.MaxBusBW {
					result.MaxBusBW = busBW
				}
			}
		}
		if rowPositive {
			result.DataRows++
		}
	}
	if result.DataRows == 0 {
		return result, fmt.Errorf("no nccl-tests row had positive algbw and busbw")
	}
	return result, nil
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

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
