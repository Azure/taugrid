variable "subscription_id" {
  description = "Azure subscription ID."
  type        = string
}

variable "tenant_id" {
  description = "Managed Entra tenant ID. Null uses the AzureRM caller's tenant."
  type        = string
  default     = null
  nullable    = true
}

variable "aks_admin_group_object_ids" {
  description = "Optional Entra groups granted AKS cluster-admin access. Managed Entra remains enabled when this list is empty."
  type        = list(string)
  default     = []
}

variable "azure_rbac_enabled" {
  description = "Use Azure RBAC for Kubernetes authorization. Keep false when TauWorkspace RoleBindings enforce workspace access."
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

variable "gpu_stack_mode" {
  description = "GPU software stack. self_managed installs the device plugin and DCGM exporter. aks_managed_preview uses the AKS Managed GPU Experience preview."
  type        = string
  default     = "self_managed"

  validation {
    condition     = contains(["self_managed", "aks_managed_preview"], var.gpu_stack_mode)
    error_message = "gpu_stack_mode must be self_managed or aks_managed_preview."
  }
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
  description = "Enable cluster autoscaling for the GPU node pool. This cannot be combined with normalize_gpu_mig=true."
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
  description = "Normalize MIG mode on the initial GPU pool before TauGrid installation. Keep true for the default A100 SKU. It cannot be combined with GPU autoscaling because newly scaled nodes are not normalized."
  type        = bool
  default     = true
}

variable "taugrid_version" {
  description = "Published TauGrid chart version passed to tau cluster install."
  type        = string
  default     = "0.3.1"
}

variable "install_taugrid" {
  description = "Install or upgrade TauGrid with tau cluster install after AKS and the selected GPU stack are ready. Requires tau, helm, and kubectl on PATH."
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

variable "enable_lifecycle_recorder" {
  description = "Enable the TauGrid lifecycle recorder and Portal run history. Requires enable_adx=true; Terraform bootstraps the configured workload namespace."
  type        = bool
  default     = false

  validation {
    condition     = !var.enable_lifecycle_recorder || var.enable_adx
    error_message = "enable_lifecycle_recorder requires enable_adx=true."
  }
}

variable "workspace_namespace" {
  description = "Target workload namespace shown by Portal and observed by the lifecycle recorder."
  type        = string
  default     = "taugrid-default"

  validation {
    condition = (
      can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.workspace_namespace)) &&
      length(var.workspace_namespace) <= 63 &&
      !contains(["default", "tau-platform", "tau-system"], var.workspace_namespace) &&
      !startswith(var.workspace_namespace, "kube-")
    )
    error_message = "workspace_namespace must be a lowercase Kubernetes namespace name of at most 63 characters and cannot be default, a Tau system namespace, or a kube-* namespace."
  }
}

variable "bootstrap_workspace" {
  description = "Optional single Entra-backed TauWorkspace to apply after TauGrid installation. Its workload namespace is workspace_namespace and its LocalQueue is jobqueue."
  type = object({
    name                  = string
    entra_group_object_id = string
  })
  default  = null
  nullable = true

  validation {
    condition = var.bootstrap_workspace == null || try(
      can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.bootstrap_workspace.name)) &&
      length(var.bootstrap_workspace.name) <= 63 &&
      can(regex("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$", var.bootstrap_workspace.entra_group_object_id)),
      false,
    )
    error_message = "bootstrap_workspace.name must be a lowercase Kubernetes name of at most 63 characters, and entra_group_object_id must be an Entra group object ID UUID."
  }
}

variable "adx_cluster_name" {
  description = "Globally unique Azure Data Explorer cluster name when enable_adx is true. Use 4 to 22 lowercase letters or digits."
  type        = string
  default     = "guweterraformadx"

  validation {
    condition     = can(regex("^[a-z0-9]{4,22}$", var.adx_cluster_name))
    error_message = "adx_cluster_name must contain 4 to 22 lowercase letters or digits."
  }
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
  description = "Pinned upstream NVIDIA DCGM exporter Helm chart version used when gpu_stack_mode is self_managed."
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
