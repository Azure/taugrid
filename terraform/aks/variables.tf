variable "subscription_id" {
  description = "Azure subscription ID."
  type        = string
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
  description = "Number of GPU nodes."
  type        = number
  default     = 1
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
  description = "Interpreter used for the local TauGrid installation command. The default supports Windows PowerShell. Linux and macOS users can set [bash, -c]."
  type        = list(string)
  default     = ["pwsh", "-NoProfile", "-NonInteractive", "-Command"]
}

variable "tags" {
  description = "Tags applied to Azure resources."
  type        = map(string)
  default = {
    scenario = "taugrid-aks"
  }
}
