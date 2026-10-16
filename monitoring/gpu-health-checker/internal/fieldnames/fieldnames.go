// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fieldnames

const (
	XIDErrors            = "XID_ERRORS"
	ECCDBEVol            = "ECC_DBE_VOL"
	ECCDBEAgg            = "ECC_DBE_AGG"
	ECCSBEVol            = "ECC_SBE_VOL"
	ECCSBEAgg            = "ECC_SBE_AGG"
	RowRemapPending      = "ROW_REMAP_PENDING"
	RowRemapFailure      = "ROW_REMAP_FAILURE"
	NVLinkCRCFlit        = "NVLINK_CRC_FLIT"
	NVLinkCRCData        = "NVLINK_CRC_DATA"
	NVLinkReplay         = "NVLINK_REPLAY"
	NVLinkRecovery       = "NVLINK_RECOVERY"
	GPUNVLinkErrors      = "GPU_NVLINK_ERRORS"
	ThermalViolation     = "THERMAL_VIOLATION"
	PowerViolation       = "POWER_VIOLATION"
	GPUTemp              = "GPU_TEMP"
	MemoryTemp           = "MEMORY_TEMP"
	PowerUsage           = "POWER_USAGE"
	PCIeReplay           = "PCIE_REPLAY"
	ClockThrottleReasons = "CLOCK_THROTTLE_REASONS"
	RetiredDBE           = "RETIRED_DBE"
	RetiredSBE           = "RETIRED_SBE"
	C2CLinkStatus        = "C2C_LINK_STATUS"
	CPUTemp              = "CPU_TEMP"
	NVSwitchTemp         = "NVSWITCH_TEMP"
)

const (
	NVLinkActiveLinks   = "NVLINK_ACTIVE_LINKS"
	NVLinkTotalLinks    = "NVLINK_TOTAL_LINKS"
	NVSwitchActiveLinks = "NVSWITCH_ACTIVE_LINKS"
	NVSwitchTotalLinks  = "NVSWITCH_TOTAL_LINKS"
)

func AllFields() []string {
	return []string{
		XIDErrors, ECCDBEVol, ECCDBEAgg, ECCSBEVol, ECCSBEAgg,
		RowRemapPending, RowRemapFailure,
		NVLinkCRCFlit, NVLinkCRCData, NVLinkReplay, NVLinkRecovery, GPUNVLinkErrors,
		ThermalViolation, PowerViolation, GPUTemp, MemoryTemp, PowerUsage,
		PCIeReplay, ClockThrottleReasons,
		RetiredDBE, RetiredSBE, C2CLinkStatus,
		CPUTemp, NVSwitchTemp,
		NVLinkActiveLinks, NVLinkTotalLinks,
		NVSwitchActiveLinks, NVSwitchTotalLinks,
	}
}
