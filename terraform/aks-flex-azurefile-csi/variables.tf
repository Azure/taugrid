variable "kubeconfig_path" {
  description = "Path to the kubeconfig containing the existing AKS cluster."
  type        = string
  default     = "~/.kube/config"
}

variable "kube_context" {
  description = "Exact kubeconfig context for the AKS cluster with attached Flex nodes."
  type        = string

  validation {
    condition     = length(trimspace(var.kube_context)) > 0
    error_message = "kube_context must name an existing AKS kubeconfig context."
  }
}

variable "azurefile_csi_version" {
  description = "Pinned upstream Azure File CSI Driver Helm chart version."
  type        = string
  default     = "1.35.10"
}

variable "namespace" {
  description = "Namespace for the customer-managed Azure File CSI Driver."
  type        = string
  default     = "kube-system"
}

variable "release_name" {
  description = "Helm release name for the customer-managed driver."
  type        = string
  default     = "azurefile-csi-flex"
}

variable "driver_name" {
  description = "Distinct CSI driver identity used by Flex StorageClasses. Keep this different from the AKS-managed file.csi.azure.com driver."
  type        = string
  default     = "file.flex.csi.azure.com"

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9.-]*[a-z0-9]$", var.driver_name)) && var.driver_name != "file.csi.azure.com"
    error_message = "driver_name must be a valid distinct CSI name and must not be file.csi.azure.com."
  }
}

variable "eligible_node_selector" {
  description = "Optional labels shared by every Linux node that may mount this driver's volumes. Empty installs the node plugin on all Linux nodes."
  type        = map(string)
  default     = {}
}

variable "linux_tolerations" {
  description = "Tolerations required for the Azure File CSI node DaemonSet to run on every selected Flex node."
  type = list(object({
    key      = optional(string)
    operator = string
    value    = optional(string)
    effect   = optional(string)
  }))
  default = [
    {
      operator = "Exists"
    }
  ]
}

variable "create_storage_class" {
  description = "Create a StorageClass for the customer-managed Flex driver."
  type        = bool
  default     = false
}

variable "storage_class_name" {
  description = "Name of the optional Flex Azure File StorageClass."
  type        = string
  default     = "azurefile-flex"
}

variable "storage_account_name" {
  description = "Existing Azure Storage account used by the optional StorageClass."
  type        = string
  default     = ""
}

variable "storage_account_resource_group" {
  description = "Resource group containing the existing Azure Storage account."
  type        = string
  default     = ""
}

variable "storage_account_subscription_id" {
  description = "Subscription containing the existing Azure Storage account."
  type        = string
  default     = ""
}

variable "storage_secret_name" {
  description = "Name of a pre-created Secret in each PVC namespace containing azurestorageaccountname and azurestorageaccountkey."
  type        = string
  default     = "azure-storage-account"
}

variable "storage_sku_name" {
  description = "Azure Files storage SKU for the optional StorageClass."
  type        = string
  default     = "Premium_LRS"
}

variable "storage_mount_uid" {
  description = "UID presented as the owner of files on the SMB mount. TauGrid restricted workloads use 65532."
  type        = number
  default     = 65532

  validation {
    condition     = var.storage_mount_uid >= 0 && var.storage_mount_uid <= 4294967295 && floor(var.storage_mount_uid) == var.storage_mount_uid
    error_message = "storage_mount_uid must be an integer between 0 and 4294967295."
  }
}

variable "storage_mount_gid" {
  description = "GID presented as the group owner of files on the SMB mount. TauGrid restricted workloads use 65532."
  type        = number
  default     = 65532

  validation {
    condition     = var.storage_mount_gid >= 0 && var.storage_mount_gid <= 4294967295 && floor(var.storage_mount_gid) == var.storage_mount_gid
    error_message = "storage_mount_gid must be an integer between 0 and 4294967295."
  }
}

variable "reclaim_policy" {
  description = "Reclaim policy for the optional StorageClass."
  type        = string
  default     = "Retain"

  validation {
    condition     = contains(["Delete", "Retain"], var.reclaim_policy)
    error_message = "reclaim_policy must be Delete or Retain."
  }
}

variable "ownership_reviewed" {
  description = "Required acknowledgement that the managed-addon boundary, node targeting, storage ownership, and upgrade ownership were reviewed."
  type        = bool
  default     = false

  validation {
    condition     = var.ownership_reviewed
    error_message = "Set ownership_reviewed=true only after reviewing the managed-addon boundary and customer responsibilities in README.md."
  }
}
