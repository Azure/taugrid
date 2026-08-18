variable "subscription_id" {
  description = "Azure subscription ID."
  type        = string
}

variable "tenant_id" {
  description = "Microsoft Entra tenant ID for advanced managed Entra AKS authentication. Null uses the tenant of the AzureRM caller."
  type        = string
  default     = null
  nullable    = true
}

variable "aks_admin_group_object_ids" {
  description = "Optional advanced setting. Providing Entra group object IDs enables managed Entra AKS authentication and grants those groups cluster-admin access."
  type        = list(string)
  default     = []
}

variable "azure_rbac_enabled" {
  description = "Use Azure RBAC for Kubernetes authorization when managed Entra AKS authentication is enabled. Keep false when TauWorkspace Kubernetes RoleBindings enforce group access."
  type        = bool
  default     = false
}

variable "location" {
  description = "Azure region for the resource group and AKS cluster."
  type        = string
  default     = "swedencentral"
}

variable "resource_group_name" {
  description = "Resource group created for this environment."
  type        = string
  default     = "rg-taugrid-aks"
}

variable "cluster_name" {
  description = "AKS cluster name and DNS prefix."
  type        = string
  default     = "taugrid-aks"

  validation {
    condition     = can(regex("^[a-zA-Z][-a-zA-Z0-9]{0,53}$", var.cluster_name))
    error_message = "cluster_name must start with a letter, use only letters, digits, and hyphens, and be at most 54 characters."
  }
}

variable "kubernetes_version" {
  description = "AKS Kubernetes version. Leave null to use the AKS default version for the region."
  type        = string
  default     = null
  nullable    = true
}

variable "system_node_count" {
  description = "Number of system nodes for Kubernetes and TauGrid control-plane pods."
  type        = number
  default     = 2
}

variable "system_vm_size" {
  description = "VM size for the system node pool."
  type        = string
  default     = "Standard_D4ds_v6"
}

variable "gpu_node_pool_name" {
  description = "GPU user node pool name."
  type        = string
  default     = "gpu"
}

variable "gpu_vm_size" {
  description = "GPU VM SKU. Standard_NC24ads_A100_v4 provides one A100 80 GB GPU and is the default cost-conscious GPU option."
  type        = string
  default     = "Standard_NC24ads_A100_v4"
}

variable "gpu_node_count" {
  description = "Number of GPU nodes when gpu_auto_scaling_enabled is false."
  type        = number
  default     = 1
}

variable "gpu_auto_scaling_enabled" {
  description = "Enable cluster autoscaling for the GPU node pool."
  type        = bool
  default     = false
}

variable "gpu_min_count" {
  description = "Minimum GPU node count when gpu_auto_scaling_enabled is true."
  type        = number
  default     = 1

  validation {
    condition     = var.gpu_min_count >= 0
    error_message = "gpu_min_count must be non-negative."
  }
}

variable "gpu_max_count" {
  description = "Maximum GPU node count when gpu_auto_scaling_enabled is true."
  type        = number
  default     = 1

  validation {
    condition     = var.gpu_max_count >= var.gpu_min_count
    error_message = "gpu_max_count must be greater than or equal to gpu_min_count."
  }
}

variable "gpu_count_per_node" {
  description = "GPUs exposed by each GPU node. Must match gpu_vm_size."
  type        = number
  default     = 1
}

variable "gpu_monitoring_sku_name" {
  description = "gpu-monitoring profile name. The default matches Standard_NC24ads_A100_v4."
  type        = string
  default     = "a100-pcie-1g"
}

variable "gpu_flavor_name" {
  description = "Kueue ResourceFlavor name for the GPU node pool."
  type        = string
  default     = "taugrid-a100"
}

variable "gpu_class" {
  description = "Canonical TauGrid GPU class label applied to the GPU nodes and ResourceFlavor."
  type        = string
  default     = "a100-80gb"
}

variable "gpu_series" {
  description = "Canonical Kueue GPU series label applied to the GPU nodes and ResourceFlavor."
  type        = string
  default     = "nc24ads-a100-v4"
}

variable "normalize_gpu_mig" {
  description = "Normalize MIG mode on the GPU pool before TauGrid installation. Keep true for the default A100 SKU; set false only for GPU SKUs that do not support MIG."
  type        = bool
  default     = true
}

variable "taugrid_version" {
  description = "Published TauGrid chart version passed to tau cluster install."
  type        = string
  default     = "0.3.0"
}

variable "install_taugrid" {
  description = "Install or upgrade TauGrid with tau cluster install after AKS and the NVIDIA device plugin are ready. Requires tau, helm, and kubectl on PATH."
  type        = bool
  default     = true
}

variable "command_interpreter" {
  description = "Interpreter used for local installation commands. The commands use portable single quotes and &&, so PowerShell 7 (default) and Bash are supported. Linux and macOS users can set [bash, -c]."
  type        = list(string)
  default     = ["pwsh", "-NoProfile", "-NonInteractive", "-Command"]
}

variable "enable_adx" {
  description = "Create the optional Azure Data Explorer data plane and install adx-mon. This creates additional billable Azure resources."
  type        = bool
  default     = false
}

variable "enable_portal" {
  description = "Install Portal. Disabled by default because a secure researcher endpoint requires a platform-owned authenticated proxy that prevents direct backend access."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_portal || var.enable_adx
    error_message = "enable_portal requires enable_adx=true because this Terraform root configures Portal to use Kusto."
  }
}

variable "enable_lifecycle_recorder" {
  description = "Enable the TauGrid lifecycle recorder. Requires enable_adx=true and an existing workload namespace."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_lifecycle_recorder || var.enable_adx
    error_message = "enable_lifecycle_recorder requires enable_adx=true."
  }
}

variable "workspace_namespace" {
  description = "Existing TauWorkspace namespace shown by Portal and observed by the lifecycle recorder."
  type        = string
  default     = "taugrid-default"
}

variable "adx_cluster_name" {
  description = "Globally unique Azure Data Explorer cluster name when enable_adx is true."
  type        = string
  default     = "guweterraformadx"
}

variable "adx_sku_name" {
  description = "Azure Data Explorer SKU for an evaluation deployment."
  type        = string
  default     = "Dev(No SLA)_Standard_D11_v2"
}

variable "adx_sku_capacity" {
  description = "Number of Azure Data Explorer instances."
  type        = number
  default     = 1
}

variable "dcgm_exporter_chart_version" {
  description = "Pinned NVIDIA DCGM exporter Helm chart version used for GPU telemetry when ADX is enabled."
  type        = string
  default     = "4.8.3"
}

variable "tags" {
  description = "Tags applied to Azure resources."
  type        = map(string)
  default = {
    scenario = "taugrid-aks"
  }
}
