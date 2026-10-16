// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dcgm

import (
	"fmt"
	"log/slog"
	"math"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
)

type FieldID = dcgm.Short

var (
	FieldXIDErrors FieldID = dcgm.DCGM_FI_DEV_XID_ERRORS

	FieldECCDBEVolTotal  FieldID = dcgm.DCGM_FI_DEV_ECC_DBE_VOL_TOTAL
	FieldECCDBEAggTotal  FieldID = dcgm.DCGM_FI_DEV_ECC_DBE_AGG_TOTAL
	FieldECCSBEVolTotal  FieldID = dcgm.DCGM_FI_DEV_ECC_SBE_VOL_TOTAL
	FieldECCSBEAggTotal  FieldID = dcgm.DCGM_FI_DEV_ECC_SBE_AGG_TOTAL
	FieldRowRemapPending FieldID = dcgm.DCGM_FI_DEV_ROW_REMAP_PENDING
	FieldRowRemapFailure FieldID = dcgm.DCGM_FI_DEV_ROW_REMAP_FAILURE

	FieldNVLinkCRCFlit   FieldID = dcgm.DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL
	FieldNVLinkCRCData   FieldID = dcgm.DCGM_FI_DEV_NVLINK_CRC_DATA_ERROR_COUNT_TOTAL
	FieldNVLinkReplay    FieldID = dcgm.DCGM_FI_DEV_NVLINK_REPLAY_ERROR_COUNT_TOTAL
	FieldNVLinkRecovery  FieldID = dcgm.DCGM_FI_DEV_NVLINK_RECOVERY_ERROR_COUNT_TOTAL
	FieldGPUNVLinkErrors FieldID = dcgm.DCGM_FI_DEV_GPU_NVLINK_ERRORS

	FieldThermalViolation FieldID = dcgm.DCGM_FI_DEV_THERMAL_VIOLATION
	FieldPowerViolation   FieldID = dcgm.DCGM_FI_DEV_POWER_VIOLATION
	FieldGPUTemp          FieldID = dcgm.DCGM_FI_DEV_GPU_TEMP
	FieldMemoryTemp       FieldID = dcgm.DCGM_FI_DEV_MEMORY_TEMP
	FieldPowerUsage       FieldID = dcgm.DCGM_FI_DEV_POWER_USAGE

	FieldPCIeReplay FieldID = dcgm.DCGM_FI_DEV_PCIE_REPLAY_COUNTER

	FieldClockThrottleReasons FieldID = dcgm.DCGM_FI_DEV_CLOCKS_EVENT_REASONS

	FieldRetiredDBE FieldID = dcgm.DCGM_FI_DEV_RETIRED_DBE
	FieldRetiredSBE FieldID = dcgm.DCGM_FI_DEV_RETIRED_SBE

	FieldC2CLinkStatus FieldID = dcgm.DCGM_FI_DEV_C2C_LINK_STATUS

	FieldCPUTemp      FieldID = dcgm.DCGM_FI_DEV_CPU_TEMP_CURRENT
	FieldNVSwitchTemp FieldID = dcgm.DCGM_FI_DEV_NVSWITCH_TEMPERATURE_CURRENT

	FieldVBIOSVersion  FieldID = dcgm.DCGM_FI_DEV_VBIOS_VERSION
	FieldDriverVersion FieldID = dcgm.DCGM_FI_DRIVER_VERSION
)

func AllCounterFields() []FieldID {
	return []FieldID{
		FieldXIDErrors,
		FieldECCDBEVolTotal,
		FieldECCDBEAggTotal,
		FieldECCSBEVolTotal,
		FieldECCSBEAggTotal,
		FieldRowRemapPending,
		FieldRowRemapFailure,
		FieldNVLinkCRCFlit,
		FieldNVLinkCRCData,
		FieldNVLinkReplay,
		FieldNVLinkRecovery,
		FieldGPUNVLinkErrors,
		FieldThermalViolation,
		FieldPowerViolation,
		FieldGPUTemp,
		FieldMemoryTemp,
		FieldPowerUsage,
		FieldPCIeReplay,
		FieldClockThrottleReasons,
		FieldRetiredDBE,
		FieldRetiredSBE,
		FieldC2CLinkStatus,
		FieldCPUTemp,
		FieldNVSwitchTemp,
	}
}

type FieldGroupHandle struct {
	handle dcgm.FieldHandle
	name   string
}

func SetupFieldWatch(groupID dcgm.GroupHandle, updateFreqMicros int64, maxKeepAge float64, maxKeepSamples int32) (*FieldGroupHandle, error) {
	fields := AllCounterFields()
	fieldIDs := make([]dcgm.Short, len(fields))
	copy(fieldIDs, fields)

	fgName := "gpu_health_checker"
	fg, err := dcgm.FieldGroupCreate(fgName, fieldIDs)
	if err != nil {
		slog.Warn("full field group rejected, probing individually", "error", err)
		supported := probeFields(fieldIDs)
		if len(supported) == 0 {
			return nil, fmt.Errorf("no DCGM fields supported by runtime")
		}
		slog.Info("using supported field subset", "total", len(fieldIDs), "supported", len(supported))
		fg, err = dcgm.FieldGroupCreate(fgName, supported)
		if err != nil {
			return nil, fmt.Errorf("dcgm field group create (fallback): %w", err)
		}
	}

	if err := dcgm.WatchFieldsWithGroupEx(fg, groupID, updateFreqMicros, maxKeepAge, maxKeepSamples); err != nil {
		_ = dcgm.FieldGroupDestroy(fg)
		return nil, fmt.Errorf("dcgm watch fields: %w", err)
	}

	return &FieldGroupHandle{handle: fg, name: fgName}, nil
}

func probeFields(fields []dcgm.Short) []dcgm.Short {
	supported := make([]dcgm.Short, 0, len(fields))
	for _, fid := range fields {
		fg, err := dcgm.FieldGroupCreate("probe", []dcgm.Short{fid})
		if err != nil {
			slog.Warn("field not supported by DCGM runtime, skipping", "fieldID", fid)
			continue
		}
		_ = dcgm.FieldGroupDestroy(fg)
		supported = append(supported, fid)
	}
	return supported
}

func (fg *FieldGroupHandle) Destroy() error {
	return dcgm.FieldGroupDestroy(fg.handle)
}

func ReadFieldValues(gpuIndex uint, fields []FieldID) (map[FieldID]float64, error) {
	values := make(map[FieldID]float64, len(fields))
	fvs, err := dcgm.GetLatestValuesForFields(gpuIndex, fields)
	if err != nil {
		return nil, fmt.Errorf("dcgm read fields for gpu %d: %w", gpuIndex, err)
	}
	for i, fv := range fvs {
		if fv.Status != dcgm.DCGM_ST_OK {
			slog.Debug("skipping field with non-OK status",
				"gpu", gpuIndex, "field", fields[i], "status", fv.Status)
			continue
		}
		var v float64
		switch fv.FieldType {
		case dcgm.DCGM_FT_DOUBLE:
			v = fv.Float64()
		default:
			v = float64(fv.Int64())
		}
		if isBlankValue(v) || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		values[fields[i]] = v
	}
	return values, nil
}

func isBlankValue(v float64) bool {
	return v >= dcgm.DCGM_FT_FP64_BLANK ||
		v == float64(dcgm.DCGM_FT_INT64_BLANK) ||
		v == float64(dcgm.DCGM_FT_INT64_NOT_FOUND) ||
		v == float64(dcgm.DCGM_FT_INT64_NOT_SUPPORTED) ||
		v == float64(dcgm.DCGM_FT_INT64_NOT_PERMISSIONED) ||
		v == float64(dcgm.DCGM_FT_INT32_BLANK) ||
		v == float64(dcgm.DCGM_FT_INT32_NOT_FOUND) ||
		v == float64(dcgm.DCGM_FT_INT32_NOT_SUPPORTED) ||
		v == float64(dcgm.DCGM_FT_INT32_NOT_PERMISSIONED)
}
